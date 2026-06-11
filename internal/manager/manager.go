package manager

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/brensch/schniffer/internal/booker"
	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/metrics"
	"github.com/brensch/schniffer/internal/providers"
	"github.com/bwmarrin/discordgo"
	"github.com/robfig/cron/v3"
)

// dbWriteRequest represents a database write operation to be serialized
type dbWriteRequest struct {
	operation func() error
	result    chan error
}

type Manager struct {
	store            *db.Store
	reg              *providers.Registry
	mu               sync.Mutex
	notifier         *discordgo.Session
	summaryChannelID string
	logger           *slog.Logger
	dbWriteChan      chan dbWriteRequest

	// Optional auto-booking. Nil when SCHNIFFER_ENC_KEY is unset or pool
	// isn't wired; notification path falls back to plain DM in that case.
	pool *booker.Pool
}

// SetAutoBooking wires the browser pool used during notification dispatch.
// Call once after NewManager.
func (m *Manager) SetAutoBooking(pool *booker.Pool) {
	m.pool = pool
}

func NewManager(store *db.Store, reg *providers.Registry, notifier *discordgo.Session, summaryChannelID string) *Manager {
	m := &Manager{
		store:            store,
		reg:              reg,
		notifier:         notifier,
		summaryChannelID: summaryChannelID,
		logger:           slog.Default(),
		dbWriteChan:      make(chan dbWriteRequest, 100), // Buffer to prevent blocking
	}
	// Start database writer goroutine
	go m.dbWriter()
	return m
}

func (m *Manager) GetSummaryChannel() string {
	return m.summaryChannelID
}

// dbWriter processes database write operations sequentially to avoid lock contention
func (m *Manager) dbWriter() {
	for req := range m.dbWriteChan {
		req.result <- req.operation()
		close(req.result)
	}
}

// executeDBOperation queues a database operation for sequential execution
func (m *Manager) executeDBOperation(operation func() error) error {
	result := make(chan error, 1)
	req := dbWriteRequest{
		operation: operation,
		result:    result,
	}

	// This will block and wait if the channel is full,
	// guaranteeing sequential execution.
	m.dbWriteChan <- req
	return <-result
}

// Run polls providers at dynamic intervals based on their rate limit status
func (m *Manager) Run(ctx context.Context) {
	m.logger.Info("Starting manager")

	// Start the ad-hoc scrape processor
	m.StartAdhocScrapeProcessor(ctx)

	// Expire deactivation runs on its own cadence (used to run every poll cycle).
	go m.runExpiryReaper(ctx)

	// Warm-tab reconcile loop: keep one Chrome tab open per active
	// auto-book schniff, parked on a recaptcha-loaded campsite page so
	// hold POSTs skip the ~1.1s Nav + recaptcha load.
	if m.pool != nil {
		go m.runWarmTabReconciler(ctx)
	}

	// Start a goroutine for each provider
	for _, providerName := range m.reg.GetProviderNames() {
		go m.runProviderLoop(ctx, providerName)
	}

	// Wait for context cancellation
	<-ctx.Done()
}

const expiryReaperInterval = 5 * time.Minute

// runExpiryReaper periodically deactivates schniff requests whose checkout date
// has passed, and DMs the owning users. Decoupled from the hot poll loop so the
// scan isn't wasted work every 10s.
func (m *Manager) runExpiryReaper(ctx context.Context) {
	t := time.NewTicker(expiryReaperInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			deactivated, err := m.store.DeactivateExpiredRequests(ctx)
			if err != nil {
				m.logger.Warn("expiry reaper: deactivate failed", slog.Any("err", err))
				continue
			}
			if len(deactivated) > 0 {
				m.logger.Info("expiry reaper deactivated requests", slog.Int("count", len(deactivated)))
				m.notifyUsersOfDeactivatedRequests(ctx, deactivated)
			}
		}
	}
}

// fastestPoll is the floor for the provider poll cadence. 5s keeps us
// safely under recreation.gov's per-IP throttle — at 2s we saw immediate
// 100% 429s when traffic was funnelled through a single CF Worker egress
// pool (see PR #32 revert). The ticker change still gives a tighter
// effective cadence than the old time.After loop at the same value.
const fastestPoll = 5 * time.Second

// pollBackoffStep is added to the interval each time a cycle returns an
// error; the next success resets to fastestPoll. Keeps backoff linear so
// recovery is quick after a transient.
const pollBackoffStep = 10 * time.Second

func (m *Manager) runProviderLoop(ctx context.Context, providerName string) {
	interval := fastestPoll
	m.logger.Info("Starting provider loop", "provider", providerName, "interval", interval)

	// Ticker, not time.After: cycles fire at a fixed cadence even when a
	// cycle runs longer than the interval. The old time.After paid
	// interval + cycle_duration between cycles; the ticker decouples the
	// two so a slow cycle doesn't push the next one out.
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err := m.PollProvider(ctx, providerName)
			if err != nil {
				interval += pollBackoffStep
				m.logger.Warn("Rate limited, increasing interval", "provider", providerName, "new_interval", interval)
				if interval > 60*time.Second {
					msg := fmt.Sprintf("⚠️🐽🛑 %s rate limit detected while schniffing. Increased polling interval to %v", providerName, interval)
					_, sendErr := m.notifier.ChannelMessageSend(m.summaryChannelID, msg)
					if sendErr != nil {
						m.logger.Warn("failed to send rate limit notification", slog.Any("err", sendErr))
					}
				}
				t.Reset(interval)
			} else if interval != fastestPoll {
				interval = fastestPoll
				t.Reset(interval)
			}
		}
	}
}

// maxConcurrentCampgrounds caps the fan-out per poll cycle. Each goroutine
// makes one or more HTTP fetches via the proxy pool, which coalesces them
// into batches; the cap exists to bound memory + DB writer pressure, not to
// throttle the upstream.
const maxConcurrentCampgrounds = 32

// PollProvider performs one poll cycle for a specific provider.
//
// Returns an error only if more than half of campground fetches failed in the
// same cycle (treated as a global upstream problem worth backing off for).
// Individual fetch errors are logged but do not abort the cycle.
func (m *Manager) PollProvider(ctx context.Context, targetProvider string) error {
	cycleStart := time.Now()
	defer func() {
		metrics.PollCycleDuration.
			WithLabelValues(targetProvider).
			Observe(time.Since(cycleStart).Seconds())
	}()
	requests, err := m.store.ListActiveRequests(ctx)
	if err != nil {
		m.logger.Error("list requests failed", slog.Any("err", err))
		return nil
	}

	var filteredRequests []db.SchniffRequest
	for _, req := range requests {
		if req.Provider == targetProvider {
			filteredRequests = append(filteredRequests, req)
		}
	}
	if len(filteredRequests) == 0 {
		return nil
	}

	datesByPC, _ := collectDatesByPC(filteredRequests)
	if len(datesByPC) == 0 {
		return nil
	}

	prov, ok := m.reg.Get(targetProvider)
	if !ok {
		return nil
	}

	type cgResult struct {
		lookups []db.LookupLog
		failed  bool
	}
	results := make(chan cgResult, len(datesByPC))
	sem := make(chan struct{}, maxConcurrentCampgrounds)
	var wg sync.WaitGroup

	for k, datesSet := range datesByPC {
		wg.Add(1)
		sem <- struct{}{}
		go func(k pc, datesSet map[time.Time]struct{}) {
			defer wg.Done()
			defer func() { <-sem }()
			results <- m.pollOneCampground(ctx, prov, k, datesSet)
		}(k, datesSet)
	}
	wg.Wait()
	close(results)

	var allLookups []db.LookupLog
	var totalCG, failedCG int
	for r := range results {
		allLookups = append(allLookups, r.lookups...)
		totalCG++
		if r.failed {
			failedCG++
		}
	}
	metrics.PollCycleCampgrounds.WithLabelValues(targetProvider).Observe(float64(totalCG))
	if failedCG > 0 {
		metrics.PollCycleFailed.WithLabelValues(targetProvider).Add(float64(failedCG))
	}

	// One batched insert at end of cycle instead of N writes through the writer.
	if len(allLookups) > 0 {
		if err := m.executeDBOperation(func() error {
			return m.store.RecordLookupBatch(ctx, allLookups)
		}); err != nil {
			m.logger.Warn("record lookup batch failed", slog.Any("err", err))
		}
	}

	if err := m.ProcessNotificationsWithBatches(ctx, filteredRequests); err != nil {
		m.logger.Warn("process notifications failed", slog.String("provider", targetProvider), slog.Any("err", err))
	}

	if totalCG > 0 && failedCG*2 > totalCG {
		return fmt.Errorf("most campground fetches failed (%d/%d)", failedCG, totalCG)
	}
	return nil
}

// pollOneCampground handles one (provider, campground) — plans buckets,
// fetches them in parallel, upserts the resulting state, and returns the
// lookup log entries that the caller will batch-insert.
func (m *Manager) pollOneCampground(ctx context.Context, prov providers.Provider, k pc, datesSet map[time.Time]struct{}) (out struct {
	lookups []db.LookupLog
	failed  bool
}) {
	dates := datesFromSet(datesSet)
	buckets := prov.PlanBuckets(dates)
	if len(buckets) == 0 {
		return
	}

	type bucketResult struct {
		bucket providers.DateRange
		states []providers.CampsiteAvailability
		err    error
	}
	bres := make([]bucketResult, len(buckets))
	var bwg sync.WaitGroup
	for i, b := range buckets {
		bwg.Add(1)
		go func(i int, b providers.DateRange) {
			defer bwg.Done()
			states, err := prov.FetchAvailability(ctx, k.cg, b.Start, b.End)
			bres[i] = bucketResult{bucket: b, states: states, err: err}
		}(i, b)
	}
	bwg.Wait()

	now := time.Now()
	var collected []providers.CampsiteAvailability
	bucketFails := 0
	for _, r := range bres {
		entry := db.LookupLog{
			Provider:     k.prov,
			CampgroundID: k.cg,
			StartDate:    r.bucket.Start,
			EndDate:      r.bucket.End,
			CheckedAt:    now,
		}
		if r.err != nil {
			m.logger.Warn("fetch availability failed",
				slog.String("provider", k.prov),
				slog.String("campground", k.cg),
				slog.Time("start", r.bucket.Start),
				slog.Time("end", r.bucket.End),
				slog.Any("err", r.err))
			entry.Success = false
			entry.ErrorMsg = r.err.Error()
			bucketFails++
		} else {
			entry.Success = true
			entry.CampsiteCount = len(r.states)
			collected = append(collected, r.states...)
		}
		out.lookups = append(out.lookups, entry)
	}
	if bucketFails == len(buckets) {
		out.failed = true
	}

	if len(collected) == 0 {
		return
	}

	batch := make([]db.CampsiteAvailability, 0, len(collected))
	for _, s := range collected {
		batch = append(batch, db.CampsiteAvailability{
			Provider:     k.prov,
			CampgroundID: k.cg,
			CampsiteID:   s.ID,
			Date:         s.Date,
			Available:    s.Available,
			LastChecked:  now,
		})
	}
	start := time.Now()
	if err := m.executeDBOperation(func() error {
		return m.store.UpsertCampsiteAvailabilityBatch(ctx, batch)
	}); err != nil {
		m.logger.Error("upsert states failed", slog.String("provider", k.prov), slog.String("campground", k.cg), slog.Any("err", err))
	} else {
		m.logger.Info("persisted campsite states",
			slog.String("provider", k.prov),
			slog.String("campground", k.cg),
			slog.Int("count", len(batch)),
			slog.Duration("duration_ms", time.Since(start)),
		)
	}
	return
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

// dailySummarySchedule defines when the nightly roundup fires. 21:00
// America/Los_Angeles means 9pm Pacific, automatically tracking PST/PDT.
// Timezone data is provided by `_ "time/tzdata"` imported in main, so this
// works in any base image including debian:bookworm-slim.
const dailySummarySchedule = "0 21 * * *"
const dailySummaryTimezone = "America/Los_Angeles"

// RunDailySummary schedules the nightly Discord roundup. Returns
// immediately; the scheduler keeps running until ctx is cancelled.
func (m *Manager) RunDailySummary(ctx context.Context) {
	loc, err := time.LoadLocation(dailySummaryTimezone)
	if err != nil {
		// With _ "time/tzdata" embedded this should never happen, but if
		// it does we degrade gracefully to UTC rather than silently
		// disabling the summary entirely.
		m.logger.Error("failed to load timezone for daily summary; falling back to UTC",
			slog.String("timezone", dailySummaryTimezone),
			slog.Any("err", err))
		loc = time.UTC
	}
	c := cron.New(cron.WithLocation(loc))
	if _, err := c.AddFunc(dailySummarySchedule, func() {
		m.fireDailySummary(ctx)
	}); err != nil {
		m.logger.Error("failed to register daily summary cron",
			slog.String("schedule", dailySummarySchedule),
			slog.Any("err", err))
		return
	}
	c.Start()
	now := time.Now().In(loc)
	next := c.Entries()[0].Next
	m.logger.Info("daily summary scheduled",
		slog.String("schedule", dailySummarySchedule),
		slog.String("timezone", loc.String()),
		slog.String("now", now.Format(time.RFC3339)),
		slog.String("next_fire", next.Format(time.RFC3339)),
		slog.Duration("until_next", next.Sub(now)),
	)
	<-ctx.Done()
	stopCtx := c.Stop()
	<-stopCtx.Done()
}

// fireDailySummary builds and sends one roundup. Surfaced as its own
// method so the failure modes log at WARN/ERROR — silent cron jobs are
// the worst kind.
func (m *Manager) fireDailySummary(ctx context.Context) {
	channelID := m.GetSummaryChannel()
	if channelID == "" {
		m.logger.Error("daily summary skipped: no summary channel configured")
		return
	}
	summary, err := m.store.GetSummaryData(ctx)
	if err != nil {
		m.logger.Error("daily summary: get data failed", slog.Any("err", err))
		return
	}
	summary.UserNames = m.resolveUserNames(summary)
	embed := db.MakeSummaryEmbed(summary)
	m.logger.Info("daily summary firing",
		slog.Int64("user_dms_24h", summary.Stats.UserDMs24h),
		slog.Int64("notifications_24h", summary.Stats.Notifications24h),
		slog.Int64("lookups_24h", summary.Stats.Lookups24h),
		slog.Int64("active_requests", summary.Stats.ActiveRequests),
		slog.Int("notification_users", len(summary.NotificationCounts)),
		slog.Int("active_users", len(summary.ActiveCounts)),
	)
	if _, err := m.notifier.ChannelMessageSendEmbed(channelID, embed); err != nil {
		m.logger.Error("daily summary: send failed", slog.Any("err", err))
	}
}

// resolveUserNames asks Discord for the display name of each user that
// appears in the summary. Falls back to an empty map; MakeSummaryEmbed
// then renders <@id> mentions which Discord resolves client-side.
func (m *Manager) resolveUserNames(s db.SummaryData) map[string]string {
	ids := make(map[string]struct{})
	for _, uc := range s.NotificationCounts {
		ids[uc.UserID] = struct{}{}
	}
	for _, uc := range s.ActiveCounts {
		ids[uc.UserID] = struct{}{}
	}
	if len(ids) == 0 || m.notifier == nil {
		return nil
	}
	names := make(map[string]string, len(ids))
	for id := range ids {
		u, err := m.notifier.User(id)
		if err != nil {
			m.logger.Debug("daily summary: user lookup failed (will fall back to mention)",
				slog.String("user_id", id), slog.Any("err", err))
			continue
		}
		if u == nil {
			continue
		}
		if u.Username != "" {
			names[id] = u.Username
		}
	}
	return names
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
	if m.reg == nil {
		return ""
	}
	p, ok := m.reg.Get(provider)
	if !ok || p == nil {
		return ""
	}
	return p.CampgroundURL(campgroundID)
}

// StartAdhocScrapeProcessor starts a background goroutine to process pending ad-hoc scrape requests
func (m *Manager) StartAdhocScrapeProcessor(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second) // Check every 30 seconds

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.processAdhocScrapes(ctx)
			}
		}
	}()
}

// processAdhocScrapes processes pending ad-hoc scrape requests
func (m *Manager) processAdhocScrapes(ctx context.Context) {
	pending, err := m.store.GetPendingAdhocScrapes(ctx)
	if err != nil {
		m.logger.Error("failed to get pending adhoc scrapes", slog.Any("error", err))
		return
	}

	if len(pending) == 0 {
		return
	}

	m.logger.Info("processing adhoc scrape requests", slog.Int("count", len(pending)))

	for _, req := range pending {
		err := m.processAdhocScrapeRequest(ctx, req)
		if err != nil {
			m.logger.Error("failed to process adhoc scrape request",
				slog.Int("request_id", req.ID),
				slog.String("provider", req.Provider),
				slog.String("campground_id", req.CampgroundID),
				slog.Any("error", err))

			// Mark as failed
			errorMsg := err.Error()
			m.store.UpdateAdhocScrapeStatus(ctx, req.ID, "failed", &errorMsg)
		}
	}
}

// processAdhocScrapeRequest processes a single ad-hoc scrape request
func (m *Manager) processAdhocScrapeRequest(ctx context.Context, req *db.AdhocScrapeRequest) error {
	m.logger.Info("processing adhoc scrape request",
		slog.Int("request_id", req.ID),
		slog.String("provider", req.Provider),
		slog.String("campground_id", req.CampgroundID))

	// Get the provider
	provider, ok := m.reg.Get(req.Provider)
	if !ok {
		return fmt.Errorf("provider %s not found", req.Provider)
	}

	// Calculate date range (next 60 days from now)
	startDate := time.Now()
	endDate := startDate.AddDate(0, 0, 60)

	// Execute the scrape using FetchAvailability
	results, err := provider.FetchAvailability(ctx, req.CampgroundID, startDate, endDate)
	if err != nil {
		return fmt.Errorf("failed to scrape availability: %w", err)
	}

	// Convert provider results to database format
	var availabilityStates []db.CampsiteAvailability
	now := time.Now()
	for _, result := range results {
		availabilityStates = append(availabilityStates, db.CampsiteAvailability{
			Provider:     req.Provider,
			CampgroundID: req.CampgroundID,
			CampsiteID:   result.ID,
			Date:         result.Date,
			Available:    result.Available,
			LastChecked:  now,
		})
	}

	// Store results in database using the serialized writer
	if len(availabilityStates) > 0 {
		err = m.executeDBOperation(func() error {
			return m.store.UpsertCampsiteAvailabilityBatch(ctx, availabilityStates)
		})
		if err != nil {
			return fmt.Errorf("failed to store availability results: %w", err)
		}
	}

	// Mark request as completed
	err = m.store.UpdateAdhocScrapeStatus(ctx, req.ID, "completed", nil)
	if err != nil {
		m.logger.Error("failed to mark adhoc scrape as completed",
			slog.Int("request_id", req.ID),
			slog.Any("error", err))
	}

	m.logger.Info("completed adhoc scrape request",
		slog.Int("request_id", req.ID),
		slog.String("provider", req.Provider),
		slog.String("campground_id", req.CampgroundID),
		slog.Int("results_count", len(results)))

	return nil
}

// ProcessAdhocScrapeRequest exposes the internal method for immediate processing
func (m *Manager) ProcessAdhocScrapeRequest(ctx context.Context, req *db.AdhocScrapeRequest) error {
	return m.processAdhocScrapeRequest(ctx, req)
}

// notifyUsersOfDeactivatedRequests sends private messages to users about their deactivated requests
func (m *Manager) notifyUsersOfDeactivatedRequests(ctx context.Context, deactivatedRequests []db.SchniffRequest) {
	// Group requests by user to minimize messages
	requestsByUser := make(map[string][]db.SchniffRequest)
	for _, req := range deactivatedRequests {
		requestsByUser[req.UserID] = append(requestsByUser[req.UserID], req)
	}

	for userID, requests := range requestsByUser {
		// Create a private channel or send DM
		channel, err := m.notifier.UserChannelCreate(userID)
		if err != nil {
			m.logger.Warn("failed to create DM channel for user",
				slog.String("user_id", userID),
				slog.Any("err", err))
			continue
		}

		// Build embed content
		embed := m.buildDeactivationEmbed(ctx, requests)

		// Send the embed
		_, err = m.notifier.ChannelMessageSendEmbed(channel.ID, embed)
		if err != nil {
			m.logger.Warn("failed to send deactivation notification",
				slog.String("user_id", userID),
				slog.Any("err", err))
		} else {
			m.logger.Info("sent deactivation notification",
				slog.String("user_id", userID),
				slog.Int("requests_count", len(requests)))
		}
	}
} // buildDeactivationEmbed creates a Discord embed about deactivated requests
func (m *Manager) buildDeactivationEmbed(ctx context.Context, requests []db.SchniffRequest) *discordgo.MessageEmbed {
	var title string
	var description strings.Builder

	if len(requests) == 1 {
		title = "🛑 Schniff Request Deactivated"
		description.WriteString("Your camping request has been automatically deactivated because its start date has passed.")
	} else {
		title = fmt.Sprintf("🛑 %d Schniff Requests Deactivated", len(requests))
		description.WriteString("The following camping requests have been automatically deactivated because their start dates have passed.")
	}

	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: description.String(),
		Color:       0xFF6B6B, // Red color for warnings
		Timestamp:   time.Now().Format(time.RFC3339),
	}

	// Add fields for each deactivated request
	for i, req := range requests {
		// Get campground name
		campgroundName := req.CampgroundID // fallback to ID
		if m.store != nil {
			if campground, found, err := m.store.GetCampgroundByID(ctx, req.Provider, req.CampgroundID); err == nil && found {
				campgroundName = campground.Name
			}
		}

		checkinDate := req.Checkin.Format("Jan 2, 2006")
		checkoutDate := req.Checkout.Format("Jan 2, 2006")

		fieldName := fmt.Sprintf("%d. %s", i+1, campgroundName)

		var fieldValue strings.Builder
		fieldValue.WriteString(fmt.Sprintf("📅 **Dates:** %s - %s\n", checkinDate, checkoutDate))
		fieldValue.WriteString(fmt.Sprintf("🏕️ **Provider:** %s\n", req.Provider))

		// Add campground URL if available
		if campgroundURL := m.CampgroundURL(req.Provider, req.CampgroundID); campgroundURL != "" {
			fieldValue.WriteString(fmt.Sprintf("🔗 [View Campground](%s)", campgroundURL))
		}

		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   fieldName,
			Value:  fieldValue.String(),
			Inline: false,
		})
	}

	return embed
}
