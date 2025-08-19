package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/brensch/schniffer/internal/db"
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
			err := m.PollProvider(ctx, providerName)
			if err != nil {
				// Double the interval on errors
				interval += pollIncrement
				m.logger.Warn("Rate limited, increasing interval", "provider", providerName, "new_interval", interval)

				msg := fmt.Sprintf("⚠️🐽🛑 %s rate limit detected while schniffing. Increased polling interval to %v", providerName, interval)
				_, err = m.notifier.ChannelMessageSend(m.summaryChannelID, msg)
				if err != nil {
					m.logger.Warn("failed to send rate limit notification", slog.Any("err", err))
				}

			} else {
				interval = fastestPoll // Reset to fastest poll on success
			}
		}
	}
}

// PollProvider performs one poll cycle for a specific provider and returns a summary
func (m *Manager) PollProvider(ctx context.Context, targetProvider string) error {
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
		return nil
	}

	// Filter requests for the target provider
	var filteredRequests []db.SchniffRequest
	for _, req := range requests {
		if req.Provider == targetProvider {
			filteredRequests = append(filteredRequests, req)
		}
	}

	if len(filteredRequests) == 0 {
		return nil
	}

	// dedupe by provider+campground, then provider decides how to bucket dates
	datesByPC, _ := collectDatesByPC(filteredRequests)
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
		var collectedStates []providers.CampsiteAvailability
		for _, b := range buckets {
			states, err := prov.FetchAvailability(ctx, k.cg, b.Start, b.End)
			if err != nil {
				// return an error straight away at first sign of api failing
				return fmt.Errorf("failed to fetch availability: %w", err)
			}

			// record lookup if no error
			err = m.store.RecordLookup(ctx, db.LookupLog{
				Provider:      k.prov,
				CampgroundID:  k.cg,
				StartDate:     b.Start,
				EndDate:       b.End,
				CheckedAt:     time.Now(),
				Success:       true,
				CampsiteCount: len(states),
			})
			if err != nil {
				m.logger.Warn("record lookup failed", slog.Any("err", err))
			}

			if len(states) == 0 {
				m.logger.Info("no states returned", slog.String("provider", k.prov), slog.String("campground", k.cg), slog.Time("start", b.Start), slog.Time("end", b.End))
			}
			// collect for later bundled change detection and notification
			collectedStates = append(collectedStates, states...)
		}

		// Process all collected states for this provider+campground at once
		if len(collectedStates) == 0 {
			continue
		}

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
		err := m.executeDBOperation(func() error {
			return m.store.UpsertCampsiteAvailabilityBatch(ctx, batch)
		})
		if err != nil {
			// only http errors need to fail the function.
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

	// After processing all states, check for notifications
	if len(filteredRequests) > 0 {
		err := m.ProcessNotificationsWithBatches(ctx, filteredRequests)
		if err != nil {
			m.logger.Warn("process notifications failed", slog.String("provider", targetProvider), slog.Any("err", err))
		}
	}

	return nil
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

// Daily summary routine - runs at 10 PM San Francisco time every night
func (m *Manager) RunDailySummary(ctx context.Context) {
	// Load San Francisco timezone
	sfLocation, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		m.logger.Error("failed to load San Francisco timezone", slog.Any("err", err))
		return
	}

	// use go cron library to fire at 10pm every day.
	cron := cron.New(cron.WithLocation(sfLocation))
	cron.AddFunc("0 22 * * *", func() {
		summary, err := m.store.GetSummaryData(ctx)
		if err != nil {
			m.logger.Error("failed to get summary data", slog.Any("err", err))
			return
		}

		embed := db.MakeSummaryEmbed(summary)

		m.logger.Info("daily summary generated", slog.Any("summary", summary))
		m.notifier.ChannelMessageSendEmbed(m.GetSummaryChannel(), embed)
	})
	cron.Start()
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
