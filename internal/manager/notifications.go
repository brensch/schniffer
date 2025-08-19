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

		// Only compute/send when there's something to notify about
		newlyAvail, newlyBooked := separateChanges(changes)
		if len(newlyAvail) == 0 && len(newlyBooked) == 0 {
			continue
		}

		err := m.sendStateChangeNotification(ctx, req, newlyAvail, newlyBooked)
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

// sendStateChangeNotification fetches context data, builds the embed via pure helpers, and sends it.
func (m *Manager) sendStateChangeNotification(
	ctx context.Context,
	req db.SchniffRequest,
	newlyAvailable []db.AvailabilityItem,
	newlyBooked []db.AvailabilityItem,
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

	// Build stats (pure)
	stats := buildCampsiteStats(byCampsite, req.Checkin, req.Checkout, detailsMap)
	sort.Slice(stats, func(i, j int) bool { return stats[i].DaysAvailable > stats[j].DaysAvailable })
	if len(stats) > 5 {
		stats = stats[:5]
	}

	// Get campground presentation info
	campground, _, err := m.store.GetCampgroundByID(ctx, req.Provider, req.CampgroundID)
	campgroundURL := m.CampgroundURL(req.Provider, req.CampgroundID)

	// missing the provider is irrelevant, checked in
	provider, _ := m.reg.Get(req.Provider)

	// Build embed (pure)
	embed := BuildNotificationEmbed(
		req.Checkin, req.Checkout, req.UserID,
		campground.Name, campgroundURL, campground.ID,
		stats,
		newlyAvailable, newlyBooked,
		provider,
	)

	// Send
	_, err = m.notifier.ChannelMessageSendEmbed(channel.ID, embed)
	return err
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

// BuildNotificationEmbed creates a single embed with fields for each campsite.
// Pure: does not hit DB; accepts all text inputs and precomputed stats.
func BuildNotificationEmbed(
	checkin, checkout time.Time,
	userID string,
	campgroundName string,
	campgroundURL string,
	campgroundID string,
	campsiteStats []CampsiteStats,
	newlyAvailable []db.AvailabilityItem,
	newlyBooked []db.AvailabilityItem,
	provider providers.Provider,
) *discordgo.MessageEmbed {
	// Format campground name (linked if URL provided)
	campgroundLine := campgroundName
	if strings.TrimSpace(campgroundURL) != "" {
		campgroundLine = fmt.Sprintf("[%s](%s)", campgroundName, campgroundURL)
	}

	// Main description with header info
	var desc strings.Builder
	desc.WriteString(campgroundLine + "\n")
	desc.WriteString(checkin.Format("2006-01-02") + " to " + checkout.Format("2006-01-02") + "\n")
	desc.WriteString(fmt.Sprintf("<@%s>, I just schniffed some available campsites for you.\n\n", userID))

	// Summary stats
	switch len(campsiteStats) {
	case 0:
		desc.WriteString("No campsites currently available.\n")
	case 1:
		desc.WriteString("Showing the top 1 campsite by days available.\n")
		desc.WriteString("1 total campsite with availabilities.")
	default:
		desc.WriteString(fmt.Sprintf("Showing the top %d campsites by days available.\n", len(campsiteStats)))
		desc.WriteString(fmt.Sprintf("%d total campsites with availabilities.", len(campsiteStats)))
	}

	// Changes since last notification
	if len(newlyAvailable) > 0 || len(newlyBooked) > 0 {
		desc.WriteString("\nChanges since last notification:\n")
		if len(newlyAvailable) > 0 {
			desc.WriteString(fmt.Sprintf("%d newly available\n", len(newlyAvailable)))
		}
		if len(newlyBooked) > 0 {
			desc.WriteString(fmt.Sprintf("%d newly booked\n", len(newlyBooked)))
		}
	}

	embed := &discordgo.MessageEmbed{
		Title:       nonsense.RandomSillyHeader(),
		Description: desc.String(),
		Color:       0x00ff00, // green
		Timestamp:   time.Now().Format(time.RFC3339),
		Fields:      []*discordgo.MessageEmbedField{},
	}

	// Build quick lookup sets for change markers
	newAvailSet := make(map[string]struct{}, len(newlyAvailable))
	for _, a := range newlyAvailable {
		newAvailSet[a.CampsiteID+"|"+a.Date.Format("2006-01-02")] = struct{}{}
	}
	newBookedSet := make(map[string]struct{}, len(newlyBooked))
	for _, b := range newlyBooked {
		newBookedSet[b.CampsiteID+"|"+b.Date.Format("2006-01-02")] = struct{}{}
	}

	// Add a field for each campsite
	for _, s := range campsiteStats {
		var fieldValue strings.Builder

		// Optional enhanced details
		if s.Details.Name != "" {
			fieldValue.WriteString(fmt.Sprintf("**%s**\n", s.Details.Name))
		}
		if s.Details.Type != "" {
			fieldValue.WriteString(fmt.Sprintf("Type: %s\n", s.Details.Type))
		}
		if s.Details.CostPerNight > 0 {
			fieldValue.WriteString(fmt.Sprintf("Cost: $%.2f/night\n", s.Details.CostPerNight))
		}
		if s.Details.Rating > 0 {
			fieldValue.WriteString(fmt.Sprintf("Rating: ⭐ %.1f\n", s.Details.Rating))
		}
		if len(s.Details.Equipment) > 0 {
			eq := s.Details.Equipment
			if len(eq) > 5 {
				fieldValue.WriteString(fmt.Sprintf("Equipment: %s, +%d more\n", strings.Join(eq[:5], ", "), len(eq)-5))
			} else {
				fieldValue.WriteString(fmt.Sprintf("Equipment: %s\n", strings.Join(eq, ", ")))
			}
		}
		if fieldValue.Len() > 0 {
			fieldValue.WriteString("\n")
		}

		if provider != nil {
			url := provider.CampsiteURL(campgroundID, s.CampsiteID)
			fieldValue.WriteString(fmt.Sprintf("[%d of %d days available](%s)\n", s.DaysAvailable, s.TotalDays, url))
		} else {
			fieldValue.WriteString(fmt.Sprintf("%d of %d days available\n", s.DaysAvailable, s.TotalDays))
		}

		// Summary line for availability (not linked here because we don't know URL patterns generically)

		// List available dates with change markers
		maxDates := 8
		total := len(s.Dates)
		if total > maxDates {
			fieldValue.WriteString(fmt.Sprintf("First %d of %d dates:\n", maxDates, total))
		}
		show := s.Dates
		if len(show) > maxDates {
			show = s.Dates[:maxDates]
		}
		for _, d := range show {
			key := s.CampsiteID + "|" + d.Format("2006-01-02")
			marker := ""
			if _, ok := newAvailSet[key]; ok {
				marker = " (new)"
			} else if _, ok := newBookedSet[key]; ok {
				marker = " (missed it!)"
			}
			fieldValue.WriteString(fmt.Sprintf("%s (%s)%s\n", d.Format("Monday"), d.Format("2006-01-02"), marker))
		}
		if total > maxDates {
			fieldValue.WriteString(fmt.Sprintf("... and %d more", total-maxDates))
		}

		fieldName := fmt.Sprintf("Campsite %s", s.CampsiteID)
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   fieldName,
			Value:  fieldValue.String(),
			Inline: false,
		})
	}

	// Remember section
	rememberValue := strings.Join([]string{
		"• Act fast to get these sites - typically gone within 5 minutes",
		"• Links take you to booking pages",
		"• Find the availability and click to book",
		"• If no availability when you click, you were too slow",
		"• I don't make mistakes (added 'no mistakes' to chatgpt prompt)",
		"• Mobile app may open to last page despite link - double check",
	}, "\n")

	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name:   "Remember",
		Value:  rememberValue,
		Inline: false,
	})

	return embed
}
