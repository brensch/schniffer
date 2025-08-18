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
	"golang.org/x/time/rate"
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

// Helper types for streaming sync

type RecGovSearchResult struct {
	EntityID      string  `json:"entity_id"`
	EntityType    string  `json:"entity_type"`
	Name          string  `json:"name"`
	Latitude      string  `json:"latitude"`
	Longitude     string  `json:"longitude"`
	ParentName    string  `json:"parent_name"`
	Reservable    bool    `json:"reservable"`
	AverageRating float64 `json:"average_rating"`
	Activities    []struct {
		ActivityName        string `json:"activity_name"`
		ActivityDescription string `json:"activity_description"`
	} `json:"activities"`
	CampsiteEquipmentName []string `json:"campsite_equipment_name"`
	Description           string   `json:"description"`
	PreviewImageURL       string   `json:"preview_image_url"`
	PriceRange            struct {
		AmountMax float64 `json:"amount_max"`
		AmountMin float64 `json:"amount_min"`
		PerUnit   string  `json:"per_unit"`
	} `json:"price_range"`
}

type RecGovSearchResponse struct {
	Results []RecGovSearchResult `json:"results"`
	Size    int                  `json:"size"`
}

// Run polls providers at dynamic intervals based on their rate limit status
func (m *Manager) Run(ctx context.Context) {
	m.logger.Info("Starting manager")

	// Start a goroutine for each provider
	for _, providerName := range m.reg.GetProviderNames() {
		go m.runProviderLoop(ctx, providerName)
	}

	// Wait for context cancellation
	<-ctx.Done()
}

const fastestPoll = 10 * time.Second
const pollIncrement = 10 * time.Second

func (m *Manager) runProviderLoop(ctx context.Context, providerName string) {
	interval := fastestPoll

	m.logger.Info("Starting provider loop", "provider", providerName, "interval", interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			result := m.PollOnceResultForProvider(ctx, providerName)
			// Check if any calls had 429 errors
			has429 := false
			for _, call := range result.Calls {
				if strings.Contains(call.Error, "429") {
					has429 = true
					break
				}
			}
			if has429 {
				// Double the interval on 429 errors
				interval += pollIncrement
				m.logger.Warn("Rate limited, increasing interval", "provider", providerName, "new_interval", interval)

				// Send Discord notification
				m.mu.Lock()
				notifier := m.notifier
				channelID := m.summaryChannelID
				m.mu.Unlock()
				if notifier != nil && channelID != "" {
					msg := fmt.Sprintf("⚠️🐽🛑 %s rate limit detected while schniffing. Increased polling interval to %v", providerName, interval)
					if err := notifier.NotifySummary(channelID, msg); err != nil {
						m.logger.Warn("failed to send rate limit notification", slog.Any("err", err))
					}
				}
			} else {
				interval = fastestPoll // Reset to fastest poll on success
			}
		}
	}
}

// PollOnceResultForProvider performs one poll cycle for a specific provider and returns a summary
func (m *Manager) PollOnceResultForProvider(ctx context.Context, targetProvider string) PollResult {
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

	// Filter requests for the target provider
	var filteredRequests []db.SchniffRequest
	for _, req := range requests {
		if req.Provider == targetProvider {
			filteredRequests = append(filteredRequests, req)
		}
	}

	if len(filteredRequests) == 0 {
		return PollResult{} // No requests for this provider
	}

	// dedupe by provider+campground, then provider decides how to bucket dates
	datesByPC, _ := collectDatesByPC(filteredRequests)
	var result PollResult
	for k, datesSet := range datesByPC {
		if k.prov != targetProvider {
			continue // Should not happen due to filtering above, but safety check
		}

		prov, ok := m.reg.Get(k.prov)
		if !ok {
			continue
		}
		// to sorted slice
		dates := datesFromSet(datesSet)
		// provider decides minimal set of requests
		buckets := prov.PlanBuckets(dates)
		// collect all states for this provider+campground across buckets to enable bundled notifications
		collectedStates := make([]providers.CampsiteAvailability, 0, 128)
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
				if err2 := m.store.RecordLookup(ctx, db.LookupLog{Provider: k.prov, CampgroundID: k.cg, StartDate: b.Start, EndDate: b.End, CheckedAt: time.Now(), Success: false, ErrorMsg: err.Error(), CampsiteCount: 0}); err2 != nil {
					m.logger.Warn("record lookup failed", slog.Any("err", err2))
				}
				m.logger.Warn("fetch availability failed", slog.String("provider", k.prov), slog.String("campground", k.cg), slog.Time("start", b.Start), slog.Time("end", b.End), slog.Any("err", err))
				result.Calls = append(result.Calls, call)
				continue
			}
			if err2 := m.store.RecordLookup(ctx, db.LookupLog{Provider: k.prov, CampgroundID: k.cg, StartDate: b.Start, EndDate: b.End, CheckedAt: time.Now(), Success: true, CampsiteCount: len(states)}); err2 != nil {
				m.logger.Warn("record lookup failed", slog.Any("err", err2))
			}
			result.Calls = append(result.Calls, call)
			result.States += len(states)
			if len(states) == 0 {
				m.logger.Info("no states returned", slog.String("provider", k.prov), slog.String("campground", k.cg), slog.Time("start", b.Start), slog.Time("end", b.End))
			}
			// collect for later bundled change detection and notification
			collectedStates = append(collectedStates, states...)
		}

		// Process all collected states for this provider+campground at once
		if len(collectedStates) > 0 {
			// Convert to db format
			batch := make([]db.CampsiteAvailability, 0, len(collectedStates))
			now := time.Now()
			for _, s := range collectedStates {
				batch = append(batch, db.CampsiteAvailability{
					Provider:     k.prov,
					CampgroundID: k.cg,
					CampsiteID:   s.ID,
					Date:         s.Date,
					Available:    s.Available,
					LastChecked:  now,
				})
			}

			// Upsert states
			start := time.Now()
			err := m.store.UpsertCampsiteAvailabilityBatch(ctx, batch)
			if err != nil {
				m.logger.Error("upsert states failed", slog.Any("err", err))
			} else {
				m.logger.Info("persisted campsite states",
					slog.String("provider", k.prov),
					slog.String("campground", k.cg),
					slog.Int("count", len(batch)),
					slog.Duration("duration_ms", time.Since(start)),
				)
			}
		}
	}

	// After processing all states, check for notifications
	if len(filteredRequests) > 0 {
		err := m.ProcessNotificationsWithBatches(ctx, filteredRequests)
		if err != nil {
			m.logger.Warn("process notifications failed", slog.String("provider", targetProvider), slog.Any("err", err))
		}
	}

	return result
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
	datesByPC, _ := collectDatesByPC(requests)
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
		collectedStates := make([]providers.CampsiteAvailability, 0, 128)
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
				if err2 := m.store.RecordLookup(ctx, db.LookupLog{Provider: k.prov, CampgroundID: k.cg, StartDate: b.Start, EndDate: b.End, CheckedAt: time.Now(), Success: false, ErrorMsg: err.Error(), CampsiteCount: 0}); err2 != nil {
					m.logger.Warn("record lookup failed", slog.Any("err", err2))
				}
				m.logger.Warn("fetch availability failed", slog.String("provider", k.prov), slog.String("campground", k.cg), slog.Time("start", b.Start), slog.Time("end", b.End), slog.Any("err", err))
				result.Calls = append(result.Calls, call)
				continue
			}
			if err2 := m.store.RecordLookup(ctx, db.LookupLog{Provider: k.prov, CampgroundID: k.cg, StartDate: b.Start, EndDate: b.End, CheckedAt: time.Now(), Success: true, CampsiteCount: len(states)}); err2 != nil {
				m.logger.Warn("record lookup failed", slog.Any("err", err2))
			}
			result.Calls = append(result.Calls, call)
			result.States += len(states)
			if len(states) == 0 {
				m.logger.Info("no states returned", slog.String("provider", k.prov), slog.String("campground", k.cg), slog.Time("start", b.Start), slog.Time("end", b.End))
			}
			// collect for later bundled change detection and notification
			collectedStates = append(collectedStates, states...)
		}

		// Process all collected states for this provider+campground at once
		if len(collectedStates) > 0 {
			// Convert to db format
			batch := make([]db.CampsiteAvailability, 0, len(collectedStates))
			now := time.Now()
			for _, s := range collectedStates {
				batch = append(batch, db.CampsiteAvailability{
					Provider:     k.prov,
					CampgroundID: k.cg,
					CampsiteID:   s.ID,
					Date:         s.Date,
					Available:    s.Available,
					LastChecked:  now,
				})
			}

			// Upsert states
			start := time.Now()
			err := m.store.UpsertCampsiteAvailabilityBatch(ctx, batch)
			if err != nil {
				m.logger.Error("upsert states failed", slog.Any("err", err))
			} else {
				m.logger.Info("persisted campsite states",
					slog.String("provider", k.prov),
					slog.String("campground", k.cg),
					slog.Int("count", len(batch)),
					slog.Duration("duration_ms", time.Since(start)),
				)
			}
		}
	}

	// After processing all states, check for notifications
	if len(requests) > 0 {
		err := m.ProcessNotificationsWithBatches(ctx, requests)
		if err != nil {
			m.logger.Warn("process notifications failed", slog.Any("err", err))
		}
	}

	return result
}

// Notifier must be provided by bot; here we define an interface to call back.

type Notifier interface {
	// NotifyAvailabilityEmbed sends an embed with availability items; implementations may ignore extra fields.
	// newlyAvailable are items that just opened; newlyBooked are items that just became unavailable.
	NotifyAvailabilityEmbed(userID string, provider string, campgroundID string, req db.SchniffRequest, items []db.AvailabilityItem, newlyAvailable []db.AvailabilityItem, newlyBooked []db.AvailabilityItem) error
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

// Daily summary routine - runs at 10 PM San Francisco time every night
func (m *Manager) RunDailySummary(ctx context.Context) {
	// Load San Francisco timezone
	sfLocation, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		m.logger.Error("failed to load San Francisco timezone", slog.Any("err", err))
		return
	}

	// Calculate next 10 PM SF time
	nextRun := m.calculateNext10PMSF(sfLocation)
	m.logger.Info("next daily summary scheduled", slog.Time("time", nextRun))

	timer := time.NewTimer(time.Until(nextRun))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			m.snapshotDaily(ctx)
			// Calculate next 10 PM SF time (24 hours later, accounting for DST)
			nextRun = m.calculateNext10PMSF(sfLocation)
			m.logger.Info("next daily summary scheduled", slog.Time("time", nextRun))
			timer.Reset(time.Until(nextRun))
		}
	}
}

// calculateNext10PMSF returns the next 10 PM San Francisco time
func (m *Manager) calculateNext10PMSF(sfLocation *time.Location) time.Time {
	now := time.Now().In(sfLocation)

	// Get today at 10 PM SF time
	today10PM := time.Date(now.Year(), now.Month(), now.Day(), 22, 0, 0, 0, sfLocation)

	// If it's already past 10 PM today, schedule for tomorrow
	if now.After(today10PM) {
		return today10PM.AddDate(0, 0, 1)
	}

	return today10PM
}

func (m *Manager) snapshotDaily(ctx context.Context) {
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
			err := n.NotifySummary(ch, msg)
			if err != nil {
				m.logger.Warn("notify summary failed", slog.Any("err", err))
			}
		}
	}
}

// minimal int64 to string without extra import
func itoa(i int64) string { return fmt.Sprintf("%d", i) }

// Configure summary channel id
func (m *Manager) SetSummaryChannel(id string) { m.mu.Lock(); m.summaryChannelID = id; m.mu.Unlock() }

// GetSummaryChannel returns the configured summary channel ID
func (m *Manager) GetSummaryChannel() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.summaryChannelID
}

// CampsiteURL exposes provider-specific campsite URLs for the bot to build embeds.
func (m *Manager) CampsiteURL(provider, campgroundID, campsiteID string) string {
	p, ok := m.reg.Get(provider)
	if !ok || p == nil {
		return ""
	}
	return p.CampsiteURL(campgroundID, campsiteID)
}

// CampgroundURL exposes provider-specific campground URLs for the bot to build embeds.
func (m *Manager) CampgroundURL(provider, campgroundID string) string {
	p, ok := m.reg.Get(provider)
	if !ok || p == nil {
		return ""
	}
	return p.CampgroundURL(campgroundID)
}

// SyncCampgrounds pulls all campgrounds from a provider and stores them in DB.
func (m *Manager) SyncCampgrounds(ctx context.Context, providerName string) (int, error) {
	prov, ok := m.reg.Get(providerName)
	if !ok {
		return 0, fmt.Errorf("unknown provider: %s", providerName)
	}
	// Check last successful sync within 24h
	if last, ok, err := m.store.GetLastSuccessfulMetadataSync(ctx, "campgrounds", providerName); err == nil && ok {
		if time.Since(last) < 24*time.Hour {
			m.logger.Info("skip campground sync; recently synced", slog.String("provider", providerName), slog.Time("last", last))
			return 0, nil
		}
	} else if err != nil {
		m.logger.Warn("get last sync failed", slog.Any("err", err))
	}
	started := time.Now()

	// Use the provider's FetchAllCampgrounds method directly - it now handles all the amenities extraction
	all, err := prov.FetchAllCampgrounds(ctx)
	if err != nil {
		// store failed sync
		if err2 := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{SyncType: "campgrounds", Provider: providerName, CampgroundID: nil, StartedAt: started, FinishedAt: time.Now(), Success: false, ErrorMsg: err.Error(), Count: 0}); err2 != nil {
			m.logger.Warn("record sync failed", slog.Any("err", err2))
		}
		return 0, err
	}
	count := 0
	for _, cg := range all {
		err := m.store.UpsertCampground(ctx, providerName, cg.ID, cg.Name, cg.Lat, cg.Lon, cg.Rating, cg.Amenities, cg.ImageURL, cg.PriceMin, cg.PriceMax, cg.PriceUnit)
		if err != nil {
			// store failed sync
			if err2 := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{SyncType: "campgrounds", Provider: providerName, CampgroundID: nil, StartedAt: started, FinishedAt: time.Now(), Success: false, ErrorMsg: err.Error(), Count: count}); err2 != nil {
				m.logger.Warn("record sync failed", slog.Any("err", err2))
			}
			return count, err
		}
		count++
	}
	err = m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{SyncType: "campgrounds", Provider: providerName, CampgroundID: nil, StartedAt: started, FinishedAt: time.Now(), Success: true, Count: count})
	if err != nil {
		m.logger.Warn("record sync failed", slog.Any("err", err))
	}
	return count, nil
}

// SyncCampsites pulls all campsite metadata from a provider and stores them in DB.
func (m *Manager) SyncCampsites(ctx context.Context, providerName string) (int, error) {
	prov, ok := m.reg.Get(providerName)
	if !ok {
		return 0, fmt.Errorf("unknown provider: %s", providerName)
	}

	// Check last successful sync within 24h
	if last, ok, err := m.store.GetLastSuccessfulMetadataSync(ctx, "campsites", providerName); err == nil && ok {
		if time.Since(last) < 24*time.Hour {
			m.logger.Info("skip campsite sync; recently synced", slog.String("provider", providerName), slog.Time("last", last))
			return 0, nil
		}
	} else if err != nil {
		m.logger.Warn("get last campsite sync failed", slog.Any("err", err))
	}

	started := time.Now()

	// Get all campgrounds from the database to sync campsites for
	campgrounds, err := m.store.GetCampgroundsByProvider(ctx, providerName)
	if err != nil {
		m.logger.Warn("failed to get campgrounds", slog.String("provider", providerName), slog.Any("err", err))
		return 0, fmt.Errorf("failed to get campgrounds: %w", err)
	}

	if len(campgrounds) == 0 {
		m.logger.Info("no campgrounds found for provider", slog.String("provider", providerName))
		return 0, nil
	}

	// Create rate limiter: 1 request every 2 seconds with burst of 5
	rateLimiter := rate.NewLimiter(rate.Every(2*time.Second), 5)

	count := 0
	processed := 0
	skipped := 0
	totalCampgrounds := len(campgrounds)

	m.logger.Info("starting campsite sync",
		slog.String("provider", providerName),
		slog.Int("total_campgrounds", totalCampgrounds),
		slog.String("estimated_duration", fmt.Sprintf("~%.1f hours", float64(totalCampgrounds)*2.1/3600))) // 2s rate limit + 0.1s delay

	for i, campground := range campgrounds {
		if ctx.Err() != nil {
			m.logger.Warn("context canceled", slog.String("provider", providerName), slog.String("campground", campground.ID))
			return processed, ctx.Err()
		}
		processed++

		// Log progress every 100 campgrounds
		if processed%100 == 0 {
			m.logger.Info("campsite sync progress",
				slog.String("provider", providerName),
				slog.Int("processed", processed),
				slog.Int("total", totalCampgrounds),
				slog.Int("successful", count),
				slog.Int("skipped", skipped),
				slog.Float64("percent", float64(processed)/float64(totalCampgrounds)*100))
		}

		// Check if this specific campground was recently synced (within 1 hour)
		if last, ok, err := m.store.GetLastSuccessfulCampgroundSync(ctx, "campsites", providerName, campground.ID); err == nil && ok {
			if time.Since(last) < 1*time.Hour {
				skipped++
				continue
			}
		} else if err != nil {
			m.logger.Warn("get last campground sync failed", slog.Any("err", err))
		}

		// Rate limit the request
		if err := rateLimiter.Wait(ctx); err != nil {
			m.logger.Warn("rate limiter context canceled", slog.Any("err", err))
			return processed, err
		}

		// Add a small delay between requests to be extra respectful to the API
		if i > 0 {
			select {
			case <-ctx.Done():
				return processed, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}

		// Fetch campsite metadata for this campground
		campsiteInfos, err := prov.FetchCampsites(ctx, campground.ID)
		if err != nil {
			m.logger.Warn("failed to fetch campsite metadata",
				slog.String("provider", providerName),
				slog.String("campground", campground.ID),
				slog.Any("err", err))

			// If it's a rate limit error (429), send Discord notification and add extra delay
			if strings.Contains(err.Error(), "429") {
				m.logger.Info("rate limit detected, adding extra delay",
					slog.String("provider", providerName),
					slog.String("campground", campground.ID))

				// Send Discord notification
				m.mu.Lock()
				notifier := m.notifier
				channelID := m.summaryChannelID
				m.mu.Unlock()
				if notifier != nil && channelID != "" {
					msg := fmt.Sprintf("⚠️ %s rate limit hit while syncing campsite metadata for campground %s. Slowing down requests.", providerName, campground.ID)
					if err := notifier.NotifySummary(channelID, msg); err != nil {
						m.logger.Warn("failed to send rate limit notification", slog.Any("err", err))
					}
				}

				select {
				case <-ctx.Done():
					return processed, ctx.Err()
				case <-time.After(5 * time.Minute):
				}
			}

			// Record failed sync for this campground
			campgroundIDPtr := &campground.ID
			if err2 := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{
				SyncType:     "campsites",
				Provider:     providerName,
				CampgroundID: campgroundIDPtr,
				StartedAt:    time.Now(),
				FinishedAt:   time.Now(),
				Success:      false,
				ErrorMsg:     err.Error(),
				Count:        0,
			}); err2 != nil {
				m.logger.Warn("record campground sync failed", slog.Any("err", err2))
			}
			continue
		}

		// Store each campsite metadata
		err = m.store.UpsertCampsiteMetadataBatch(ctx, providerName, campground.ID, campsiteInfos)
		if err != nil {
			m.logger.Warn("failed to store campsite metadata",
				slog.String("provider", providerName),
				slog.String("campground", campground.ID),
				slog.Any("err", err))

			// Record failed sync for this campground
			campgroundIDPtr := &campground.ID
			if err2 := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{
				SyncType:     "campsites",
				Provider:     providerName,
				CampgroundID: campgroundIDPtr,
				StartedAt:    time.Now(),
				FinishedAt:   time.Now(),
				Success:      false,
				ErrorMsg:     err.Error(),
				Count:        0,
			}); err2 != nil {
				m.logger.Warn("record campground sync failed", slog.Any("err", err2))
			}
			continue
		}

		// Extract unique campsite types and equipment from the fetched data
		campsiteTypesSet := make(map[string]struct{})
		equipmentSet := make(map[string]struct{})

		for _, campsite := range campsiteInfos {
			if campsite.Type != "" {
				campsiteTypesSet[campsite.Type] = struct{}{}
			}
			for _, eq := range campsite.Equipment {
				if eq != "" {
					equipmentSet[eq] = struct{}{}
				}
			}
		}

		// Convert sets to slices
		var campsiteTypes []string
		for t := range campsiteTypesSet {
			campsiteTypes = append(campsiteTypes, t)
		}

		var equipment []string
		for e := range equipmentSet {
			equipment = append(equipment, e)
		}

		// get max min price from campsites
		var minPrice, maxPrice float64
		for _, campsite := range campsiteInfos {
			if campsite.CostPerNight < minPrice || minPrice == 0 {
				minPrice = campsite.CostPerNight
			}
			if campsite.CostPerNight > maxPrice {
				maxPrice = campsite.CostPerNight
			}
		}

		// Update campground with aggregated campsite types and equipment
		err = m.store.UpdateCampgroundBasedOnCampsites(ctx, providerName, campground.ID, campsiteTypes, equipment, minPrice, maxPrice)
		if err != nil {
			m.logger.Warn("failed to update campground with campsite data",
				slog.String("provider", providerName),
				slog.String("campground", campground.ID),
				slog.Any("err", err))
			// Don't skip - this is not critical
		}

		// Record successful sync for this campground
		campgroundIDPtr := &campground.ID
		if err := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{
			SyncType:     "campsites",
			Provider:     providerName,
			CampgroundID: campgroundIDPtr,
			StartedAt:    time.Now(),
			FinishedAt:   time.Now(),
			Success:      true,
			Count:        len(campsiteInfos),
		}); err != nil {
			m.logger.Warn("record campground sync failed", slog.Any("err", err))
		}

		count++
	}

	// Record overall sync completion
	err = m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{
		SyncType:     "campsites",
		Provider:     providerName,
		CampgroundID: nil, // NULL for provider-level sync
		StartedAt:    started,
		FinishedAt:   time.Now(),
		Success:      true,
		Count:        count,
	})
	if err != nil {
		m.logger.Warn("record campsite sync failed", slog.Any("err", err))
	}

	m.logger.Info("campsite sync completed",
		slog.String("provider", providerName),
		slog.Int("campgrounds_processed", processed),
		slog.Int("campsites_synced", count))

	return count, nil
}

// RunCampgroundSync runs periodic campground syncs in the background.
func (m *Manager) RunCampgroundSync(ctx context.Context, provider string, interval time.Duration) {
	doSync := func() {
		// First sync campgrounds
		n, err := m.SyncCampgrounds(ctx, provider)
		if err != nil {
			m.logger.Error("campground sync failed", slog.String("provider", provider), slog.Any("err", err))
			return
		}
		m.logger.Info("campground sync completed", slog.String("provider", provider), slog.Int("count", n))

		// Then sync campsites
		campsiteCount, err := m.SyncCampsites(ctx, provider)
		if err != nil {
			m.logger.Error("campsite sync failed", slog.String("provider", provider), slog.Any("err", err))
			return
		}
		m.logger.Info("campsite sync completed", slog.String("provider", provider), slog.Int("count", campsiteCount))
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
