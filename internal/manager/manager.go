package manager

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/providers"
)

type Manager struct {
	store            *db.Store
	reg              *providers.Registry
	mu               sync.Mutex
	notifier         Notifier
	summaryChannelID string
	logger           *slog.Logger
}

func NewManager(store *db.Store, reg *providers.Registry) *Manager {
	return &Manager{store: store, reg: reg, logger: slog.Default()}
}

// Run polls every 5 seconds for active requests and performs deduped provider lookups
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.PollOnce(ctx)
		}
	}
}

// PollResult is returned by pollOnce for testing/inspection.
type PollResult struct {
	Calls []struct {
		Provider     string
		CampgroundID string
		Start        time.Time
		End          time.Time
		Success      bool
		Error        string
	}
	States int // number of campsite states observed this run (before persist)
}

func (m *Manager) PollOnce(ctx context.Context) {
	_ = m.PollOnceResult(ctx)
}

// normalizeDay returns t truncated to 00:00:00 UTC.
func normalizeDay(t time.Time) time.Time {
	tt := t.UTC()
	return time.Date(tt.Year(), tt.Month(), tt.Day(), 0, 0, 0, 0, time.UTC)
}

// generateNights returns the UTC days in [checkin, checkout) at day granularity.
func generateNights(checkin, checkout time.Time) []time.Time {
	if !checkin.Before(checkout) {
		return nil
	}
	out := []time.Time{}
	for d := normalizeDay(checkin); d.Before(normalizeDay(checkout)); d = d.AddDate(0, 0, 1) {
		out = append(out, d)
	}
	return out
}

// datesFromSet converts a set of dates to a sorted slice (ascending) of UTC days.
func datesFromSet(set map[time.Time]struct{}) []time.Time {
	if len(set) == 0 {
		return nil
	}
	out := make([]time.Time, 0, len(set))
	for d := range set {
		out = append(out, normalizeDay(d))
	}
	// simple insertion sort (small N typical); could use sort.Slice
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j].Before(out[j-1]) {
			out[j], out[j-1] = out[j-1], out[j]
			j--
		}
	}
	return out
}

type pc struct{ prov, cg string }

// collectDatesByPC groups requests by provider+campground and accumulates unique UTC days.
func collectDatesByPC(reqs []db.SchniffRequest) (map[pc]map[time.Time]struct{}, map[pc][]db.SchniffRequest) {
	datesBy := map[pc]map[time.Time]struct{}{}
	reqsBy := map[pc][]db.SchniffRequest{}
	for _, r := range reqs {
		if !r.Checkin.Before(r.Checkout) {
			continue
		}
		key := pc{prov: r.Provider, cg: r.CampgroundID}
		if _, ok := datesBy[key]; !ok {
			datesBy[key] = map[time.Time]struct{}{}
		}
		for _, d := range generateNights(r.Checkin, r.Checkout) {
			datesBy[key][d] = struct{}{}
		}
		reqsBy[key] = append(reqsBy[key], r)
	}
	return datesBy, reqsBy
}

// pollOnceResult performs one poll cycle and returns a summary for tests.
func (m *Manager) PollOnceResult(ctx context.Context) PollResult {
	// First, deactivate any expired requests
	expiredCount, err := m.store.DeactivateExpiredRequests(ctx)
	if err != nil {
		m.logger.Warn("failed to deactivate expired requests", slog.Any("err", err))
	} else if expiredCount > 0 {
		m.logger.Info("deactivated expired requests", slog.Int64("count", expiredCount))
	}

	requests, err := m.store.ListActiveRequests(ctx)
	if err != nil {
		m.logger.Error("list requests failed", slog.Any("err", err))
		return PollResult{}
	}
	// dedupe by provider+campground, then provider decides how to bucket dates
	datesByPC, reqsByPC := collectDatesByPC(requests)
	var result PollResult
	for k, datesSet := range datesByPC {
		prov, ok := m.reg.Get(k.prov)
		if !ok {
			continue
		}
		// to sorted slice
		dates := datesFromSet(datesSet)
		// provider decides minimal set of requests
		buckets := prov.PlanBuckets(dates)
		// collect all states for this provider+campground across buckets to enable bundled notifications
		collectedStates := make([]providers.Campsite, 0, 128)
		for _, b := range buckets {
			states, err := prov.FetchAvailability(ctx, k.cg, b.Start, b.End)
			call := struct {
				Provider     string
				CampgroundID string
				Start        time.Time
				End          time.Time
				Success      bool
				Error        string
			}{Provider: k.prov, CampgroundID: k.cg, Start: b.Start, End: b.End, Success: err == nil}
			if err != nil {
				call.Error = err.Error()
				if err2 := m.store.RecordLookup(ctx, db.LookupLog{Provider: k.prov, CampgroundID: k.cg, Month: b.Start, StartDate: b.Start, EndDate: b.End, CheckedAt: time.Now(), Success: false, Err: err.Error()}); err2 != nil {
					m.logger.Warn("record lookup failed", slog.Any("err", err2))
				}
				m.logger.Warn("fetch availability failed", slog.String("provider", k.prov), slog.String("campground", k.cg), slog.Time("start", b.Start), slog.Time("end", b.End), slog.Any("err", err))
				result.Calls = append(result.Calls, call)
				continue
			}
			if err2 := m.store.RecordLookup(ctx, db.LookupLog{Provider: k.prov, CampgroundID: k.cg, Month: b.Start, StartDate: b.Start, EndDate: b.End, CheckedAt: time.Now(), Success: true}); err2 != nil {
				m.logger.Warn("record lookup failed", slog.Any("err", err2))
			}
			result.Calls = append(result.Calls, call)
			result.States += len(states)
			if len(states) == 0 {
				m.logger.Info("no states returned", slog.String("provider", k.prov), slog.String("campground", k.cg), slog.Time("start", b.Start), slog.Time("end", b.End))
			}
			// collect for later bundled change detection and notification
			collectedStates = append(collectedStates, states...)
			// persist states after detecting changes
			batch := make([]db.CampsiteState, 0, len(states))
			now := time.Now()
			for _, s := range states {
				batch = append(batch, db.CampsiteState{Provider: k.prov, CampgroundID: k.cg, CampsiteID: s.ID, Date: s.Date, Available: s.Available, CheckedAt: now})
			}
			if err := m.store.UpsertCampsiteStateBatch(ctx, batch); err != nil {
				m.logger.Error("upsert states failed", slog.Any("err", err))
			} else {
				m.logger.Info("persisted campsite states", slog.String("provider", k.prov), slog.String("campground", k.cg), slog.Int("count", len(batch)))
			}
		}
		// change detection and bundled notification across all buckets for this provider+campground
		if len(collectedStates) > 0 {
			reqs := reqsByPC[k]
			m.detectChangesAndNotify(ctx, reqs, collectedStates, k.prov, k.cg)
		}
	}
	return result
}

// Notifier must be provided by bot; here we define an interface to call back.

type Notifier interface {
	NotifyAvailability(userID string, msg string) error
	// NotifyAvailabilityEmbed sends an embed with availability items; implementations may ignore extra fields.
	NotifyAvailabilityEmbed(userID string, provider string, campgroundID string, items []db.AvailabilityItem) error
	NotifySummary(channelID string, msg string) error
	// ResolveUsernames converts user IDs to usernames
	ResolveUsernames(userIDs []string) []string
}

type SummaryData struct {
	Stats                 db.DetailedSummaryStats
	NotificationUsernames []string
	ActiveUsernames       []string
	TrackedCampgrounds    []string
}

func (m *Manager) SetNotifier(n Notifier) { // optional injection later
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifier = n
}

var _ = (*Manager).SetNotifier // avoid unused warning when bot not yet wired

type notifyKey struct {
	prov, cg, site string
	date           time.Time
}

func (m *Manager) detectChangesAndNotify(ctx context.Context, reqs []db.SchniffRequest, states []providers.Campsite, prov, cg string) {
	m.mu.Lock()
	n := m.notifier
	m.mu.Unlock()
	if n == nil {
		return
	}
	// Convert provider states to db-friendly incoming states
	incoming := make([]db.IncomingCampsiteState, 0, len(states))
	for _, s := range states {
		incoming = append(incoming, db.IncomingCampsiteState{CampsiteID: s.ID, Date: s.Date, Available: s.Available})
	}
	// Ask DB to reconcile notifications and return per-user newly opened availability items
	newlyOpenByUser, err := m.store.ReconcileNotifications(ctx, prov, cg, reqs, incoming)
	if err != nil {
		m.logger.Warn("reconcile notifications failed", slog.Any("err", err))
		return
	}
	if len(newlyOpenByUser) == 0 {
		return
	}
	// Bundle notifications per user (embed preferred, limit to first 10)
	for userID, items := range newlyOpenByUser {
		if len(items) == 0 {
			continue
		}
		// cap to first 10
		if len(items) > 10 {
			items = items[:10]
		}
		// Try embed path first
		if err := n.NotifyAvailabilityEmbed(userID, prov, cg, items); err != nil {
			// fallback to plain text
			b := strings.Builder{}
			b.WriteString("Available: ")
			b.WriteString(prov)
			b.WriteString(" ")
			b.WriteString(cg)
			for _, it := range items {
				b.WriteString("\n- ")
				b.WriteString(it.Date.Format("2006-01-02"))
				b.WriteString(" site ")
				b.WriteString(it.CampsiteID)
				if url := m.CampsiteURL(prov, cg, it.CampsiteID); url != "" {
					b.WriteString(" ")
					b.WriteString(url)
				}
			}
			if err2 := n.NotifyAvailability(userID, b.String()); err2 != nil {
				m.logger.Warn("notify availability failed", slog.Any("err", err2))
			}
		}
	}
}

// Daily summary routine
func (m *Manager) RunDailySummary(ctx context.Context) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.snapshotDaily(ctx)
		}
	}
}

func (m *Manager) snapshotDaily(ctx context.Context) {
	// aggregate and store
	if err := m.store.InsertDailySummarySnapshot(ctx); err != nil {
		m.logger.Error("daily summary insert failed", slog.Any("err", err))
	}
	// post summary to channel if configured
	m.mu.Lock()
	n := m.notifier
	ch := m.summaryChannelID
	m.mu.Unlock()
	if n != nil && ch != "" {
		total, active, lookups, notes, err := func() (int64, int64, int64, int64, error) {
			t, err := m.store.CountTotalRequests(ctx)
			if err != nil {
				return 0, 0, 0, 0, err
			}
			a, l, n, err := m.store.StatsToday(ctx)
			return t, a, l, n, err
		}()
		if err == nil {
			msg := "Daily summary (" + time.Now().Format("2006-01-02") + ")\n" +
				"Total requests: " + itoa(total) + "\n" +
				"Active requests: " + itoa(active) + "\n" +
				"Lookups today: " + itoa(lookups) + "\n" +
				"Notifications today: " + itoa(notes)
			if err := n.NotifySummary(ch, msg); err != nil {
				m.logger.Warn("notify summary failed", slog.Any("err", err))
			}
		}
	}
}

// minimal int64 to string without extra import
func itoa(i int64) string { return fmt.Sprintf("%d", i) }

// Configure summary channel id
func (m *Manager) SetSummaryChannel(id string) { m.mu.Lock(); m.summaryChannelID = id; m.mu.Unlock() }

// CampsiteURL exposes provider-specific campsite URLs for the bot to build embeds.
func (m *Manager) CampsiteURL(provider, campgroundID, campsiteID string) string {
	p, ok := m.reg.Get(provider)
	if !ok || p == nil {
		return ""
	}
	return p.CampsiteURL(campgroundID, campsiteID)
}

// SyncCampgrounds pulls all campgrounds from a provider and stores them in DB.
func (m *Manager) SyncCampgrounds(ctx context.Context, providerName string) (int, error) {
	prov, ok := m.reg.Get(providerName)
	if !ok {
		return 0, fmt.Errorf("unknown provider: %s", providerName)
	}
	// Check last successful sync within 24h
	if last, ok, err := m.store.GetLastSuccessfulSync(ctx, "campgrounds", providerName); err == nil && ok {
		if time.Since(last) < 24*time.Hour {
			m.logger.Info("skip campground sync; recently synced", slog.String("provider", providerName), slog.Time("last", last))
			return 0, nil
		}
	} else if err != nil {
		m.logger.Warn("get last sync failed", slog.Any("err", err))
	}
	started := time.Now()
	all, err := prov.FetchAllCampgrounds(ctx)
	if err != nil {
		// store failed sync
		if err2 := m.store.RecordSync(ctx, db.SyncLog{SyncType: "campgrounds", Provider: providerName, StartedAt: started, FinishedAt: time.Now(), Success: false, Err: err.Error(), Count: 0}); err2 != nil {
			m.logger.Warn("record sync failed", slog.Any("err", err2))
		}
		return 0, err
	}
	count := 0
	for _, cg := range all {
		if err := m.store.UpsertCampground(ctx, providerName, cg.ID, cg.Name, cg.Lat, cg.Lon); err != nil {
			// store failed sync
			if err2 := m.store.RecordSync(ctx, db.SyncLog{SyncType: "campgrounds", Provider: providerName, StartedAt: started, FinishedAt: time.Now(), Success: false, Err: err.Error(), Count: int64(count)}); err2 != nil {
				m.logger.Warn("record sync failed", slog.Any("err", err2))
			}
			return count, err
		}
		count++
	}
	if err := m.store.RecordSync(ctx, db.SyncLog{SyncType: "campgrounds", Provider: providerName, StartedAt: started, FinishedAt: time.Now(), Success: true, Count: int64(count)}); err != nil {
		m.logger.Warn("record sync failed", slog.Any("err", err))
	}
	return count, nil
}

// RunCampgroundSync runs periodic campground syncs in the background.
func (m *Manager) RunCampgroundSync(ctx context.Context, provider string, interval time.Duration) {
	doSync := func() {
		n, err := m.SyncCampgrounds(ctx, provider)
		if err != nil {
			m.logger.Error("campground sync failed", slog.String("provider", provider), slog.Any("err", err))
			return
		}
		m.logger.Info("campground sync completed", slog.String("provider", provider), slog.Int("count", n))
	}
	doSync()
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			doSync()
		}
	}
}

// GetDetailedSummary returns a formatted summary string with comprehensive statistics
func (m *Manager) GetDetailedSummary(ctx context.Context) (string, error) {
	// Get detailed stats
	stats, err := m.store.GetDetailedSummaryStats(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get stats: %w", err)
	}

	// Get users with notifications
	usersWithNotifications, err := m.store.GetUsersWithNotifications(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get users with notifications: %w", err)
	}

	// Get users with active requests
	usersWithActiveRequests, err := m.store.GetUsersWithActiveRequests(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get users with active requests: %w", err)
	}

	// Get tracked campgrounds
	trackedCampgrounds, err := m.store.GetTrackedCampgrounds(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get tracked campgrounds: %w", err)
	}

	// Resolve usernames if notifier is available
	var notificationUsernames, activeUsernames []string
	m.mu.Lock()
	n := m.notifier
	m.mu.Unlock()

	if n != nil {
		notificationUsernames = n.ResolveUsernames(usersWithNotifications)
		activeUsernames = n.ResolveUsernames(usersWithActiveRequests)
	} else {
		// Fallback to user IDs if no notifier
		notificationUsernames = usersWithNotifications
		activeUsernames = usersWithActiveRequests
	}

	// Build the summary message
	var summary strings.Builder
	summary.WriteString("24 Hour Schniff roundup:\n")
	summary.WriteString("Available campsites found\n")
	summary.WriteString(fmt.Sprintf("%d\n", stats.Notifications24h))
	summary.WriteString("Checks made\n")
	summary.WriteString(fmt.Sprintf("%d\n", stats.Lookups24h))
	summary.WriteString("Active Schniffs\n")
	summary.WriteString(fmt.Sprintf("%d\n", stats.ActiveRequests))

	// Schniffists who got schniffs
	summary.WriteString("Schniffists who got schniffs\n")
	if len(notificationUsernames) == 0 {
		summary.WriteString("No bueno today.\n")
	} else {
		summary.WriteString(strings.Join(notificationUsernames, "\n") + "\n")
	}

	// Schniffists with active schniffs
	summary.WriteString("Schniffists with active schniffs\n")
	if len(activeUsernames) == 0 {
		summary.WriteString("None\n")
	} else {
		summary.WriteString(strings.Join(activeUsernames, "\n") + "\n")
	}

	// Campgrounds being tracked
	summary.WriteString("Campgrounds being tracked\n")
	if len(trackedCampgrounds) == 0 {
		summary.WriteString("None\n")
	} else {
		summary.WriteString(strings.Join(trackedCampgrounds, "\n"))
	}

	return summary.String(), nil
}

// GetSummaryData returns structured summary data for creating embeds
func (m *Manager) GetSummaryData(ctx context.Context) (SummaryData, error) {
	// Get detailed stats
	stats, err := m.store.GetDetailedSummaryStats(ctx)
	if err != nil {
		return SummaryData{}, fmt.Errorf("failed to get stats: %w", err)
	}

	// Get users with notifications
	usersWithNotifications, err := m.store.GetUsersWithNotifications(ctx)
	if err != nil {
		return SummaryData{}, fmt.Errorf("failed to get users with notifications: %w", err)
	}

	// Get users with active requests
	usersWithActiveRequests, err := m.store.GetUsersWithActiveRequests(ctx)
	if err != nil {
		return SummaryData{}, fmt.Errorf("failed to get users with active requests: %w", err)
	}

	// Get tracked campgrounds
	trackedCampgrounds, err := m.store.GetTrackedCampgrounds(ctx)
	if err != nil {
		return SummaryData{}, fmt.Errorf("failed to get tracked campgrounds: %w", err)
	}

	// Resolve usernames if notifier is available
	var notificationUsernames, activeUsernames []string
	m.mu.Lock()
	n := m.notifier
	m.mu.Unlock()

	if n != nil {
		notificationUsernames = n.ResolveUsernames(usersWithNotifications)
		activeUsernames = n.ResolveUsernames(usersWithActiveRequests)
	} else {
		// Fallback to user IDs if no notifier
		notificationUsernames = usersWithNotifications
		activeUsernames = usersWithActiveRequests
	}

	return SummaryData{
		Stats:                 stats,
		NotificationUsernames: notificationUsernames,
		ActiveUsernames:       activeUsernames,
		TrackedCampgrounds:    trackedCampgrounds,
	}, nil
}
