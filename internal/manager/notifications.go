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

		// Use the changes we already have. Only send if there is at least ONE newly-available change.
		sent, sendErr := m.sendStateChangeNotification(ctx, req, changes)
		if sendErr != nil {
			m.logger.Warn("send state change notification failed",
				slog.String("userID", req.UserID),
				slog.Any("err", sendErr))
		}

		// Post a fun summary broadcast only when we actually sent a DM about availability.
		if sent {
			_, _ = m.notifier.ChannelMessageSend(m.summaryChannelID, nonsense.RandomSillyBroadcast(req.UserID))
		}

		// Record outgoing notifications for each change (both available and unavailable).
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
// It returns 'sent = true' iff a DM was sent (i.e., there was at least one newly-available change).
func (m *Manager) sendStateChangeNotification(
	ctx context.Context,
	req db.SchniffRequest,
	changes []db.StateChangeForRequest,
) (sent bool, err error) {
	// Decide based on the provided changes (avoid any redundant lookups).
	newlyAvail, _ := separateChanges(changes)
	if len(newlyAvail) == 0 {
		m.logger.Info("no newly-available changes; skipping DM",
			slog.Int64("requestID", req.ID),
			slog.String("userID", req.UserID),
			slog.String("campgroundID", req.CampgroundID))
		return false, nil
	}

	// Create DM channel only if we plan to send something.
	channel, err := m.notifier.UserChannelCreate(req.UserID)
	if err != nil {
		return false, err
	}

	// Build the current availability context for the requested date range.
	// This gives users a "what can I book right now" snapshot, not just the changed dates.
	allAvailable, qerr := m.store.GetCurrentlyAvailableCampsites(ctx, req.Provider, req.CampgroundID, req.Checkin, req.Checkout)
	if qerr != nil {
		m.logger.Warn("get currently available campsites failed", slog.Any("err", qerr))
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

	// Build stats (pure).
	stats := buildCampsiteStats(byCampsite, req.Checkin, req.Checkout, detailsMap)

	// Get campground presentation info
	campground, _, gerr := m.store.GetCampgroundByID(ctx, req.Provider, req.CampgroundID)
	if gerr != nil {
		m.logger.Warn("GetCampgroundByID failed; proceeding with minimal info", slog.Any("err", gerr))
	}
	campgroundURL := m.CampgroundURL(req.Provider, req.CampgroundID)

	// missing the provider is irrelevant, checked in
	provider, _ := m.reg.Get(req.Provider)

	// Build a single embed showing only the top 3 campsites with up to 20 dates each.
	embeds := BuildNotificationEmbeds(
		req.Checkin, req.Checkout, req.UserID,
		campground.Name, campgroundURL, campground.ID,
		stats,
		provider,
	)

	actuallySent := false
	for _, e := range embeds {
		if e == nil {
			continue
		}
		if _, sendErr := m.notifier.ChannelMessageSendEmbed(channel.ID, e); sendErr != nil {
			m.logger.Error("failed to send notification embed", slog.Any("err", sendErr), slog.String("userID", req.UserID))
			err = sendErr // track the last error
		} else {
			actuallySent = true
		}
	}

	return actuallySent, err
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

// BuildNotificationEmbeds creates a single embed that lists ONLY the top 3 campsites by days available.
// Each campsite shows at most 20 dates. No chunking or secondary embeds.
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
		slog.Warn("no campsite stats available for notification", "req", userID, "campground", campgroundID)
		return nil
	}

	const dateFmtISO = "Monday 2006-01-02"

	// Sort by days available (desc), then by campsiteID for stability.
	sort.Slice(campsiteStats, func(i, j int) bool {
		if campsiteStats[i].DaysAvailable != campsiteStats[j].DaysAvailable {
			return campsiteStats[i].DaysAvailable > campsiteStats[j].DaysAvailable
		}
		return campsiteStats[i].CampsiteID < campsiteStats[j].CampsiteID
	})

	// Keep only top 3.
	if len(campsiteStats) > 3 {
		campsiteStats = campsiteStats[:3]
	}

	title := nonsense.RandomSillyHeader()
	title = fmt.Sprintf("%s\n%s", title, campgroundName)

	desc := fmt.Sprintf("[%s ➡️ %s](%s)",
		checkin.Format(dateFmtISO), checkout.Format(dateFmtISO),
		campgroundURL,
	)

	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: desc,
		Color:       0x00ff00, // green
		Fields:      []*discordgo.MessageEmbedField{},
	}

	for _, s := range campsiteStats {
		var b strings.Builder

		// Optional meta line.
		if s.Details.Type != "" {
			b.WriteString(fmt.Sprintf("📍 %s ", s.Details.Type))
		}
		if len(s.Details.Equipment) > 0 {
			b.WriteString(fmt.Sprintf("🛖 %s\n", strings.Join(s.Details.Equipment, ", ")))
		}

		// Availability summary w/ link if provider present.
		if provider != nil {
			url := provider.CampsiteURL(campgroundID, s.CampsiteID)
			b.WriteString(fmt.Sprintf("[%d of %d days available](%s)\n", s.DaysAvailable, s.TotalDays, url))
		} else {
			b.WriteString(fmt.Sprintf("%d of %d days available\n", s.DaysAvailable, s.TotalDays))
		}

		// Up to 20 dates.
		maxDates := 20
		limit := len(s.Dates)
		if limit > maxDates {
			limit = maxDates
		}
		for i := 0; i < limit; i++ {
			b.WriteString(s.Dates[i].Format(dateFmtISO))
			b.WriteByte('\n')
		}
		// If there are more dates beyond 20, note it.
		if len(s.Dates) > maxDates {
			b.WriteString(fmt.Sprintf("…and %d more\n", len(s.Dates)-maxDates))
		}

		displayName := s.Details.Name
		if displayName == "" {
			displayName = fmt.Sprintf("Campsite %s", s.CampsiteID)
		}

		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   displayName,
			Value:  b.String(),
			Inline: false,
		})
	}

	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name: "Important Information",
		Value: strings.Join([]string{
			"🔗 Links go to booking pages",
			"🏃‍♂️ Campsites at Yosemite book out in 2 minutes",
			"⚠️ Opening links in mobile app goes to your last open page",
			"\nWith 💖 from 🐽",
		}, "\n"),
		Inline: false,
	})

	return []*discordgo.MessageEmbed{embed}
}
