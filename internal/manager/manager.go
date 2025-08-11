package manager

import (
	"context"
	"fmt"
	"log/slog"
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
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pollOnce(ctx)
		}
	}
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func (m *Manager) pollOnce(ctx context.Context) {
	requests, err := m.store.ListActiveRequests(ctx)
	if err != nil {
		m.logger.Error("list requests failed", slog.Any("err", err))
		return
	}
	// dedupe by provider+campground+month buckets across date ranges
	type key struct {
		prov, cg string
		month    time.Time
	}
	buckets := map[key][]db.SchniffRequest{}
	for _, r := range requests {
		// for each month between start and end, add bucket
		cur := monthStart(r.StartDate)
		end := monthStart(r.EndDate)
		for !cur.After(end) {
			k := key{r.Provider, r.CampgroundID, cur}
			buckets[k] = append(buckets[k], r)
			cur = cur.AddDate(0, 1, 0)
		}
	}
	for k, reqs := range buckets {
		prov, ok := m.reg.Get(k.prov)
		if !ok {
			continue
		}
		// The actual range needed across reqs within this month
		start := reqs[0].StartDate
		end := reqs[0].EndDate
		for _, r := range reqs[1:] {
			if r.StartDate.Before(start) {
				start = r.StartDate
			}
			if r.EndDate.After(end) {
				end = r.EndDate
			}
		}
		// clamp to month
		ms := monthStart(start)
		me := monthStart(end)
		start = ms
		end = me.AddDate(0, 1, -1)
		states, err := prov.FetchAvailability(ctx, k.cg, start, end)
		if err != nil {
			if err2 := m.store.RecordLookup(ctx, db.LookupLog{Provider: k.prov, CampgroundID: k.cg, Month: k.month, CheckedAt: time.Now(), Success: false, Err: err.Error()}); err2 != nil {
				m.logger.Warn("record lookup failed", slog.Any("err", err2))
			}
			continue
		}
		if err2 := m.store.RecordLookup(ctx, db.LookupLog{Provider: k.prov, CampgroundID: k.cg, Month: k.month, CheckedAt: time.Now(), Success: true}); err2 != nil {
			m.logger.Warn("record lookup failed", slog.Any("err", err2))
		}
		// change detection and notify
		m.detectChangesAndNotify(ctx, reqs, states, k.prov, k.cg)
		// persist states after detecting changes
		batch := make([]db.CampsiteState, 0, len(states))
		now := time.Now()
		for _, s := range states {
			batch = append(batch, db.CampsiteState{Provider: k.prov, CampgroundID: k.cg, CampsiteID: s.ID, Date: s.Date, Available: s.Available, CheckedAt: now})
		}
		if err := m.store.UpsertCampsiteStateBatch(ctx, batch); err != nil {
			m.logger.Error("upsert states failed", slog.Any("err", err))
		}
	}
}

// Notifier must be provided by bot; here we define an interface to call back.

type Notifier interface {
	NotifyAvailability(userID string, msg string) error
	NotifySummary(channelID string, msg string) error
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
	// Build quick lookup for requests by date
	perDate := map[string][]db.SchniffRequest{}
	for _, r := range reqs {
		// inclusive range by day
		for d := r.StartDate; !d.After(r.EndDate); d = d.AddDate(0, 0, 1) {
			key := d.Format("2006-01-02")
			perDate[key] = append(perDate[key], r)
		}
	}
	for _, s := range states {
		// get previous state
		prevAvail, prevExist, err := m.store.GetLastState(ctx, prov, cg, s.ID, s.Date)
		if err != nil {
			continue
		}
		if prevExist && prevAvail == s.Available {
			continue // no change
		}
		key := s.Date.Format("2006-01-02")
		for _, r := range perDate[key] {
			state := "unavailable"
			if s.Available {
				state = "available"
			}
			msg := "Campground " + cg + " site " + s.ID + " on " + key + " is " + state
			if err := n.NotifyAvailability(r.UserID, msg); err != nil {
				m.logger.Warn("notify availability failed", slog.Any("err", err))
			}
			if err := m.store.RecordNotification(ctx, db.Notification{RequestID: r.ID, UserID: r.UserID, Provider: prov, CampgroundID: cg, CampsiteID: s.ID, Date: s.Date, State: state, SentAt: time.Now()}); err != nil {
				m.logger.Warn("record notification failed", slog.Any("err", err))
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
	_, err := m.store.DB.ExecContext(ctx, `
		INSERT INTO daily_summary(date, total_requests, active_requests, lookups, notifications, created_at)
		SELECT current_date,
			(SELECT count(*) FROM schniff_requests),
			(SELECT count(*) FROM schniff_requests WHERE active=true),
			(SELECT count(*) FROM lookup_log WHERE date(checked_at)=current_date),
			(SELECT count(*) FROM notifications WHERE date(sent_at)=current_date),
			now()
	`)
	if err != nil {
		m.logger.Error("daily summary insert failed", slog.Any("err", err))
	}
	// post summary to channel if configured
	m.mu.Lock()
	n := m.notifier
	ch := m.summaryChannelID
	m.mu.Unlock()
	if n != nil && ch != "" {
		row := m.store.DB.QueryRowContext(ctx, `
			SELECT coalesce((SELECT count(*) FROM schniff_requests),0),
				   coalesce((SELECT count(*) FROM schniff_requests WHERE active=true),0),
				   coalesce((SELECT count(*) FROM lookup_log WHERE date(checked_at)=current_date),0),
				   coalesce((SELECT count(*) FROM notifications WHERE date(sent_at)=current_date),0)
		`)
		var total, active, lookups, notes int64
		if err := row.Scan(&total, &active, &lookups, &notes); err == nil {
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
		if err := m.store.UpsertCampground(ctx, providerName, cg.ID, cg.Name, cg.ParentName, cg.ParentID, cg.Lat, cg.Lon); err != nil {
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
