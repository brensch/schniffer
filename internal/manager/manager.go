package manager

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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
}

type RecGovSearchResponse struct {
	Results []RecGovSearchResult `json:"results"`
	Size    int                  `json:"size"`
}

// Run polls every 5 seconds for active requests and performs deduped provider lookups.
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
					Type:         s.Type,
					CostPerNight: s.CostPerNight,
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
		if err2 := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{SyncType: "campgrounds", Provider: providerName, StartedAt: started, FinishedAt: time.Now(), Success: false, ErrorMsg: err.Error(), Count: 0}); err2 != nil {
			m.logger.Warn("record sync failed", slog.Any("err", err2))
		}
		return 0, err
	}
	count := 0
	for _, cg := range all {
		err := m.store.UpsertCampground(ctx, providerName, cg.ID, cg.Name, cg.Lat, cg.Lon, cg.Rating, cg.Amenities)
		if err != nil {
			// store failed sync
			if err2 := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{SyncType: "campgrounds", Provider: providerName, StartedAt: started, FinishedAt: time.Now(), Success: false, ErrorMsg: err.Error(), Count: count}); err2 != nil {
				m.logger.Warn("record sync failed", slog.Any("err", err2))
			}
			return count, err
		}
		count++
	}
	err = m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{SyncType: "campgrounds", Provider: providerName, StartedAt: started, FinishedAt: time.Now(), Success: true, Count: count})
	if err != nil {
		m.logger.Warn("record sync failed", slog.Any("err", err))
	}
	return count, nil
}

// syncCampgroundsStreaming implements streaming sync for recreation.gov provider
func (m *Manager) syncCampgroundsStreaming(ctx context.Context, prov providers.Provider, providerName string, started time.Time) (int, error) {
	m.logger.Info("starting streaming sync for recreation.gov")

	totalCount := 0
	page := 0
	const pageSize = 50

	for {
		// Fetch page
		results, hasMore, err := m.fetchRecreationGovPage(ctx, page, pageSize)
		if err != nil {
			m.logger.Error("failed to fetch page", slog.Int("page", page), slog.Any("err", err))
			// Record failed sync
			if err2 := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{
				SyncType: "campgrounds", Provider: providerName, StartedAt: started,
				FinishedAt: time.Now(), Success: false, ErrorMsg: err.Error(), Count: totalCount,
			}); err2 != nil {
				m.logger.Warn("record sync failed", slog.Any("err", err2))
			}
			return totalCount, err
		}

		// Convert to campground info
		campgrounds := make([]providers.CampgroundInfo, 0, len(results))
		for _, result := range results {
			campground, err := m.convertRecGovResult(result)
			if err != nil {
				m.logger.Warn("failed to convert result", slog.Any("err", err), slog.String("id", result.EntityID))
				continue
			}
			campgrounds = append(campgrounds, campground)
		}

		// Fetch detailed info and save to database
		for _, campground := range campgrounds {
			// Get detailed info
			detailed, err := m.fetchRecGovCampgroundDetails(ctx, campground.ID)
			if err != nil {
				m.logger.Warn("failed to fetch details", slog.String("id", campground.ID), slog.Any("err", err))
				// Save basic info without details
			} else {
				// Merge detailed info
				campground.Rating = detailed.Rating
				campground.Amenities = detailed.Amenities
			}

			// Save to database
			if err := m.store.UpsertCampground(ctx, providerName, campground.ID, campground.Name, campground.Lat, campground.Lon, campground.Rating, campground.Amenities); err != nil {
				m.logger.Warn("failed to save campground", slog.String("id", campground.ID), slog.Any("err", err))
				continue
			}
			totalCount++

			if totalCount%10 == 0 {
				m.logger.Info("synced campgrounds", slog.Int("count", totalCount))
			}
		}

		page++
		if !hasMore {
			break
		}
	}

	// Record successful sync
	if err := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{
		SyncType: "campgrounds", Provider: providerName, StartedAt: started,
		FinishedAt: time.Now(), Success: true, ErrorMsg: "", Count: totalCount,
	}); err != nil {
		m.logger.Warn("record sync failed", slog.Any("err", err))
	}

	m.logger.Info("completed streaming sync", slog.Int("total", totalCount))
	return totalCount, nil
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

// Helper methods for streaming sync

// fetchRecreationGovPage fetches a single page of campgrounds from recreation.gov API
func (m *Manager) fetchRecreationGovPage(ctx context.Context, page int, pageSize int) ([]RecGovSearchResult, bool, error) {
	url := fmt.Sprintf("https://www.recreation.gov/api/search?fq=entity_type%%3Acampground&size=%d&start=%d", 
		pageSize, page*pageSize)
	
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, false, err
	}
	
	// Add Chrome headers to avoid 403 errors
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != 200 {
		return nil, false, fmt.Errorf("API returned status %d", resp.StatusCode)
	}
	
	// Handle gzip response
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, false, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	
	var searchResp RecGovSearchResponse
	if err := json.NewDecoder(reader).Decode(&searchResp); err != nil {
		return nil, false, err
	}
	
	hasMore := len(searchResp.Results) == pageSize
	return searchResp.Results, hasMore, nil
}// convertRecGovResult converts a recreation.gov search result to CampgroundInfo
func (m *Manager) convertRecGovResult(result RecGovSearchResult) (providers.CampgroundInfo, error) {
	var lat, lon float64
	if result.Latitude != "" {
		if v, err := strconv.ParseFloat(result.Latitude, 64); err == nil {
			lat = v
		}
	}
	if result.Longitude != "" {
		if v, err := strconv.ParseFloat(result.Longitude, 64); err == nil {
			lon = v
		}
	}

	// Create final name with parent info if available
	name := result.Name
	if result.ParentName != "" {
		name = result.ParentName + ": " + result.Name
	}

	// Build amenities map from activities and equipment
	amenities := make(map[string]string)
	for _, activity := range result.Activities {
		amenities[activity.ActivityName] = activity.ActivityDescription
	}
	for _, equipment := range result.CampsiteEquipmentName {
		amenities["Equipment: "+equipment] = ""
	}
	if result.Description != "" {
		amenities["Description"] = result.Description
	}

	return providers.CampgroundInfo{
		ID:        result.EntityID,
		Name:      name,
		Lat:       lat,
		Lon:       lon,
		Rating:    result.AverageRating,
		Amenities: amenities,
	}, nil
}

// fetchRecGovCampgroundDetails fetches detailed information for a campground
// This is now unnecessary since we get all data from the search API
func (m *Manager) fetchRecGovCampgroundDetails(ctx context.Context, facilityID string) (providers.CampgroundInfo, error) {
	// This method is no longer needed since the search API provides all the data we need
	// But we keep it for backward compatibility in case it's called
	return providers.CampgroundInfo{
		Rating:    0.0,
		Amenities: make(map[string]string),
	}, nil
}