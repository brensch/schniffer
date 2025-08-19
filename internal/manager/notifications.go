package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/nonsense"
	"github.com/brensch/schniffer/internal/providers"
	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
)

// ------- Public API on Manager -------

// ProcessNotificationsWithBatches handles the state-change-based notification system.
// DB access, logging, and notifier usage live here (methods on Manager).
func (m *Manager) ProcessNotificationsWithBatches(ctx context.Context, requests []db.SchniffRequest) error {
	m.logger.Info("processing notifications", slog.Int("request_count", len(requests)))

	// Get unnotified state changes for all requests
	stateChanges, err := m.store.GetUnnotifiedStateChanges(ctx, requests)
	if err != nil {
		m.logger.Warn("get unnotified state changes failed", slog.Any("err", err))
		return err
	}
	m.logger.Info("found unnotified state changes", slog.Int("count", len(stateChanges)))
	if len(stateChanges) == 0 {
		return nil
	}

	// Group changes per request (pure helper)
	changesByRequest := groupStateChangesByRequest(stateChanges)
	m.logger.Info("grouped state changes by request", slog.Int("requests", len(changesByRequest)))

	// Batch ID for recording notifications
	batchID := uuid.New().String()
	var notificationsToRecord []db.Notification
	now := time.Now()

	// Process each request independently
	reqIndex := indexRequestsByID(requests)
	for requestID, changes := range changesByRequest {
		req, ok := reqIndex[requestID]
		if !ok {
			m.logger.Warn("request not found for state changes", slog.Int64("requestID", requestID))
			continue
		}

		m.logger.Info("processing request",
			slog.Int64("requestID", requestID),
			slog.String("provider", req.Provider),
			slog.String("campgroundID", req.CampgroundID),
			slog.Int("changes", len(changes)),
		)

		err := m.sendStateChangeNotification(ctx, req)
		if err != nil {
			m.logger.Warn("send state change notification failed",
				slog.String("userID", req.UserID),
				slog.Any("err", err))
		}

		// Record outgoing notifications for each change
		for _, c := range changes {
			state := "available"
			if !c.NewAvailable {
				state = "unavailable"
			}
			notificationsToRecord = append(notificationsToRecord, db.Notification{
				RequestID:     req.ID,
				UserID:        req.UserID,
				Provider:      c.Provider,
				CampgroundID:  c.CampgroundID,
				CampsiteID:    c.CampsiteID,
				Date:          c.Date,
				State:         state,
				StateChangeID: &c.ID,
				SentAt:        now,
			})
		}
	}

	// Record all notifications (single DB call)
	if len(notificationsToRecord) > 0 {
		if err := m.store.InsertNotificationsBatch(ctx, notificationsToRecord, batchID); err != nil {
			m.logger.Warn("record notification batch failed", slog.Any("err", err))
		} else {
			m.logger.Info("recorded state change notification batch",
				slog.String("batchID", batchID),
				slog.Int("count", len(notificationsToRecord)))
		}
	}

	return nil
}

// sendStateChangeNotification fetches context data, builds the embed(s) via pure helpers, and sends them.
func (m *Manager) sendStateChangeNotification(
	ctx context.Context,
	req db.SchniffRequest,
) error {
	// Create DM channel
	channel, err := m.notifier.UserChannelCreate(req.UserID)
	if err != nil {
		return err
	}

	// Currently available items for the user's window
	allAvailable, err := m.store.GetCurrentlyAvailableCampsites(ctx, req.Provider, req.CampgroundID, req.Checkin, req.Checkout)
	if err != nil {
		m.logger.Warn("get currently available campsites failed", slog.Any("err", err))
		// We can still continue with only the change lists, but the experience is better with context.
	}

	// Group by campsite; collect IDs to enrich details
	byCampsite := groupAvailabilityByCampsite(allAvailable)
	campsiteIDs := collectMapKeys(byCampsite)

	// Try to fetch enhanced details in batch; if it fails, fall back to empty map
	detailsMap, derr := m.store.GetCampsiteDetailsBatch(ctx, req.Provider, req.CampgroundID, campsiteIDs)
	if derr != nil {
		m.logger.Warn("GetCampsiteDetailsBatch failed; using basic details", slog.Any("err", derr))
		detailsMap = map[string]db.CampsiteDetails{} // empty — pure helpers will handle defaults
	}

	// Build stats (pure). Do NOT cut/sort here — that happens in BuildNotificationEmbeds now.
	stats := buildCampsiteStats(byCampsite, req.Checkin, req.Checkout, detailsMap)

	// Get campground presentation info
	campground, _, err := m.store.GetCampgroundByID(ctx, req.Provider, req.CampgroundID)
	campgroundURL := m.CampgroundURL(req.Provider, req.CampgroundID)

	// missing the provider is irrelevant, checked in
	provider, _ := m.reg.Get(req.Provider)

	// Build embeds (pure). This handles sorting, field chunking, and splitting across embeds.
	embeds := BuildNotificationEmbeds(
		req.Checkin, req.Checkout, req.UserID,
		campground.Name, campgroundURL, campground.ID,
		stats,
		provider,
	)

	// Send all embeds
	var firstErr error
	for _, e := range embeds {
		if _, err := m.notifier.ChannelMessageSendEmbed(channel.ID, e); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ------- Data structures used by pure functions -------

// CampsiteStats holds statistics for a campsite's availability with enhanced details.
type CampsiteStats struct {
	CampsiteID    string
	DaysAvailable int
	TotalDays     int
	Dates         []time.Time
	Details       db.CampsiteDetails // Optional/enhanced details from DB
}

// ------- Pure helpers (easy to unit test) -------

// groupStateChangesByRequest groups state changes by RequestID.
func groupStateChangesByRequest(changes []db.StateChangeForRequest) map[int64][]db.StateChangeForRequest {
	out := make(map[int64][]db.StateChangeForRequest, len(changes))
	for _, c := range changes {
		out[c.RequestID] = append(out[c.RequestID], c)
	}
	return out
}

// indexRequestsByID makes a quick lookup map for SchniffRequest by ID.
func indexRequestsByID(requests []db.SchniffRequest) map[int64]db.SchniffRequest {
	idx := make(map[int64]db.SchniffRequest, len(requests))
	for _, r := range requests {
		idx[r.ID] = r
	}
	return idx
}

// separateChanges splits state changes into newly available and newly booked (unavailable) items.
func separateChanges(changes []db.StateChangeForRequest) (newlyAvailable []db.AvailabilityItem, newlyBooked []db.AvailabilityItem) {
	for _, c := range changes {
		item := db.AvailabilityItem{
			CampsiteID: c.CampsiteID,
			Date:       c.Date,
		}
		if c.NewAvailable {
			newlyAvailable = append(newlyAvailable, item)
		} else {
			newlyBooked = append(newlyBooked, item)
		}
	}
	return
}

// groupAvailabilityByCampsite groups raw availability items by campsite ID.
func groupAvailabilityByCampsite(items []db.AvailabilityItem) map[string][]time.Time {
	by := make(map[string][]time.Time)
	for _, it := range items {
		by[it.CampsiteID] = append(by[it.CampsiteID], it.Date)
	}
	// Ensure each slice of dates is sorted (deterministic output)
	for k := range by {
		sort.Slice(by[k], func(i, j int) bool { return by[k][i].Before(by[k][j]) })
	}
	return by
}

// collectMapKeys returns the keys of map[string]T as a slice of strings.
func collectMapKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// buildCampsiteStats converts grouped availability + optional details into per-campsite stats.
func buildCampsiteStats(
	byCampsite map[string][]time.Time,
	checkin, checkout time.Time,
	details map[string]db.CampsiteDetails,
) []CampsiteStats {
	totalDays := int(checkout.Sub(checkin).Hours() / 24)
	if totalDays < 0 {
		totalDays = 0
	}

	stats := make([]CampsiteStats, 0, len(byCampsite))
	for campsiteID, dates := range byCampsite {
		d := details[campsiteID] // zero-value ok if missing
		stats = append(stats, CampsiteStats{
			CampsiteID:    campsiteID,
			DaysAvailable: len(dates),
			TotalDays:     totalDays,
			Dates:         dates,
			Details:       d,
		})
	}
	return stats
}

// BuildNotificationEmbeds creates one or more embeds with fields for each campsite.
// Pure: does not hit DB; accepts all text inputs and precomputed stats.
// Differences from prior version:
//   - Removes cost/rating and "top N" summary.
//   - Removes "new/missed it" markers and change summary.
//   - Sorts by DaysAvailable (desc) internally.
//   - Does NOT truncate content; instead splits fields and embeds to respect Discord limits.
//   - Adds a divider between campsites.
func BuildNotificationEmbeds(
	checkin, checkout time.Time,
	userID string,
	campgroundName string,
	campgroundURL string,
	campgroundID string,
	campsiteStats []CampsiteStats,
	provider providers.Provider,
) []*discordgo.MessageEmbed {

	if len(campsiteStats) == 0 {
		return nil
	}
	// Discord embed constraints we care about
	const (
		maxFieldsPerEmbed  = 25
		maxFieldValueChars = 1024
		dateFmtISO         = "Monday 2006-01-02"
	)

	// Sort by days available (desc), then by campsiteID for stability.
	sort.Slice(campsiteStats, func(i, j int) bool {
		if campsiteStats[i].DaysAvailable != campsiteStats[j].DaysAvailable {
			return campsiteStats[i].DaysAvailable > campsiteStats[j].DaysAvailable
		}
		return campsiteStats[i].CampsiteID < campsiteStats[j].CampsiteID
	})

	// Format campground name (linked if URL provided)
	campgroundLine := campgroundName
	if strings.TrimSpace(campgroundURL) != "" {
		campgroundLine = fmt.Sprintf("[%s](%s)", campgroundName, campgroundURL)
	}

	// Helper to start a new embed with shared header
	newEmbed := func(part int) *discordgo.MessageEmbed {
		title := nonsense.RandomSillyHeader()
		if part > 1 {
			title = fmt.Sprintf("%s (cont. %d)", title, part)
		}
		title = fmt.Sprintf("%s\n%s", title, campgroundName)
		desc := fmt.Sprintf("%s\n%s ➡️ %s",
			campgroundLine,
			checkin.Format(dateFmtISO), checkout.Format(dateFmtISO),
		)
		return &discordgo.MessageEmbed{
			Title:       title,
			Description: desc,
			Color:       0x00ff00, // green
			Fields:      []*discordgo.MessageEmbedField{},
		}
	}

	embeds := []*discordgo.MessageEmbed{}
	current := newEmbed(1)
	part := 1
	fieldCount := 0

	// Helper: flush current embed if field capacity reached
	flushIfFull := func() {
		if fieldCount >= maxFieldsPerEmbed {
			embeds = append(embeds, current)
			part++
			current = newEmbed(part)
			fieldCount = 0
		}
	}

	// Helper: append a field safely
	appendField := func(name, value string) {
		current.Fields = append(current.Fields, &discordgo.MessageEmbedField{
			Name:   name,
			Value:  value,
			Inline: false,
		})
		fieldCount++
	}

	// Helper: chunk a long block into <= maxFieldValueChars preserving line breaks
	chunkByLimit := func(s string, limit int) []string {
		if len(s) <= limit {
			return []string{s}
		}
		lines := strings.Split(s, "\n")
		var out []string
		var b strings.Builder
		for _, line := range lines {
			// +1 for newline if needed
			add := len(line)
			if b.Len() > 0 {
				add++ // newline
			}
			if b.Len()+add > limit {
				out = append(out, b.String())
				b.Reset()
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
		}
		if b.Len() > 0 {
			out = append(out, b.String())
		}
		// Fallback split if any single line > limit
		var final []string
		for _, seg := range out {
			if len(seg) <= limit {
				final = append(final, seg)
				continue
			}
			// hard split by characters
			runes := []rune(seg)
			for i := 0; i < len(runes); i += limit {
				j := i + limit
				if j > len(runes) {
					j = len(runes)
				}
				final = append(final, string(runes[i:j]))
			}
		}
		return final
	}

	// Build fields for each campsite
	for _, s := range campsiteStats {
		// Build the main value (without cost/rating; keep optional name/type/equipment)
		var value strings.Builder

		if s.Details.Type != "" {
			value.WriteString(fmt.Sprintf("📍 %s ", s.Details.Type))
		}
		if len(s.Details.Equipment) > 0 {
			value.WriteString(fmt.Sprintf("🛖 %s\n", strings.Join(s.Details.Equipment, ", ")))
		}

		if provider != nil {
			url := provider.CampsiteURL(campgroundID, s.CampsiteID)
			value.WriteString(fmt.Sprintf("[%d of %d days available](%s)\n", s.DaysAvailable, s.TotalDays, url))
		} else {
			value.WriteString(fmt.Sprintf("%d of %d days available\n", s.DaysAvailable, s.TotalDays))
		}

		// Full, untruncated date list, no markers
		for _, d := range s.Dates {
			value.WriteString(fmt.Sprintf("%s\n", d.Format(dateFmtISO)))
		}

		// Chunk the value into <= 1024 and add as possibly multiple fields.
		chunks := chunkByLimit(value.String(), maxFieldValueChars)
		displayName := s.Details.Name
		if displayName == "" {
			displayName = fmt.Sprintf("Campsite %s", s.CampsiteID)
		}
		for i, chunk := range chunks {
			flushIfFull()
			name := fmt.Sprintf("%s", displayName)
			if len(chunks) > 1 {
				name = fmt.Sprintf("%s (part %d/%d)", displayName, i+1, len(chunks))
			}
			appendField(name, chunk)
		}
	}

	// Add a short "Remember" helper at the end of the last embed (kept concise to avoid crowding).
	flushIfFull()
	appendField("HURRY",
		strings.Join([]string{
			"🔗 Links go to booking pages",
			"🏃‍♂️ Campsites at Yosemite book out in 2 minutes",
			"📱 Opening links in mobile app goes to last open page - double check",
			"\n🐽💖",
		}, "\n"),
	)

	// Push the final embed
	if len(current.Fields) > 0 || current.Description != "" {
		embeds = append(embeds, current)
	}

	return embeds
}
