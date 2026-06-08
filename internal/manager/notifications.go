package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brensch/schniffer/internal/booker"
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

	// Process each request independently. DM sends are I/O-bound and slow
	// (~100-300ms each), so fan out with a bounded worker pool.
	reqIndex := indexRequestsByID(requests)
	const maxConcurrentDMs = 5
	sem := make(chan struct{}, maxConcurrentDMs)
	var nwg sync.WaitGroup
	var nmu sync.Mutex

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

		nwg.Add(1)
		sem <- struct{}{}
		go func(req db.SchniffRequest, changes []db.StateChangeForRequest) {
			defer nwg.Done()
			defer func() { <-sem }()

			sent, sendErr := m.sendStateChangeNotification(ctx, req, changes, batchID)
			if sendErr != nil {
				m.logger.Warn("send state change notification failed",
					slog.String("userID", req.UserID),
					slog.Any("err", sendErr))
			}
			if sent {
				_, _ = m.notifier.ChannelMessageSend(m.summaryChannelID, nonsense.RandomSillyBroadcast(req.UserID))
			}

			local := make([]db.Notification, 0, len(changes))
			for _, c := range changes {
				state := "available"
				if !c.NewAvailable {
					state = "unavailable"
				}
				local = append(local, db.Notification{
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
			nmu.Lock()
			notificationsToRecord = append(notificationsToRecord, local...)
			nmu.Unlock()
		}(req, changes)
	}
	nwg.Wait()

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
	batchID string,
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

	// Build the current availability context for the requested date range.
	// We need this up front because the schniff's optional minimum_nights /
	// strategy filters gate both the DM and the auto-booking call.
	allAvailable, qerr := m.store.GetCurrentlyAvailableCampsites(ctx, req.Provider, req.CampgroundID, req.Checkin, req.Checkout)
	if qerr != nil {
		m.logger.Warn("get currently available campsites failed", slog.Any("err", qerr))
	}

	if !requestConditionsMet(req, allAvailable) {
		m.logger.Info("schniff conditions not yet met; skipping notify+book",
			slog.Int64("requestID", req.ID),
			slog.String("userID", req.UserID),
			slog.String("campgroundID", req.CampgroundID),
			slog.Bool("hasMinNights", req.MinimumNights.Valid),
			slog.Bool("hasStrategy", req.Strategy.Valid),
		)
		return false, nil
	}

	// Create DM channel only if we plan to send something.
	channel, err := m.notifier.UserChannelCreate(req.UserID)
	if err != nil {
		return false, err
	}

	// HOT PATH: if the user has a warm browser session, kick off the booking
	// attempt NOW in parallel with embed building. The goroutine posts its
	// own "attempting" DM and result DM directly to channel.ID — we do not
	// block the hit notification on it, and we do not block the booking on
	// the hit notification.
	if m.pool != nil && m.pool.HasUser(req.UserID) {
		m.startBookingAttempt(ctx, req, newlyAvail, batchID, channel.ID)
	}

	byCampsite := groupAvailabilityByCampsite(allAvailable)
	campsiteIDs := collectMapKeys(byCampsite)
	detailsMap, derr := m.store.GetCampsiteDetailsBatch(ctx, req.Provider, req.CampgroundID, campsiteIDs)
	if derr != nil {
		m.logger.Warn("GetCampsiteDetailsBatch failed; using basic details", slog.Any("err", derr))
		detailsMap = map[string]db.CampsiteDetails{}
	}
	stats := buildCampsiteStats(byCampsite, req.Checkin, req.Checkout, detailsMap)

	campground, _, gerr := m.store.GetCampgroundByID(ctx, req.Provider, req.CampgroundID)
	if gerr != nil {
		m.logger.Warn("GetCampgroundByID failed; proceeding with minimal info", slog.Any("err", gerr))
	}
	campgroundURL := m.CampgroundURL(req.Provider, req.CampgroundID)
	provider, _ := m.reg.Get(req.Provider)

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
			err = sendErr
			continue
		}
		actuallySent = true
	}
	return actuallySent, err
}

// startBookingAttempt runs the selection + HoldCampsite call in a goroutine.
// It sends two DMs directly to channelID: an "attempting to hold X" message
// as soon as we have a pick, and a result message when the hold finishes.
// The caller does not block on Discord, and the booking does not block on
// Discord.
func (m *Manager) startBookingAttempt(
	ctx context.Context,
	req db.SchniffRequest,
	newlyAvail []db.AvailabilityItem,
	batchID string,
	channelID string,
) {
	go func() {
		// Defensive clip in case state changes overlap multiple windows.
		clipped := clipToWindow(newlyAvail, req.Checkin, req.Checkout)
		if len(clipped) == 0 {
			return
		}

		// Pull rating per campsite for the selection tiebreak; best-effort.
		dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
		ids := uniqueCampsiteIDs(clipped)
		details, _ := m.store.GetCampsiteDetailsBatch(dctx, req.Provider, req.CampgroundID, ids)
		dcancel()
		candidates := buildCandidates(clipped, details)

		pick, ok := booker.SelectBestPartial(candidates)
		if !ok {
			return
		}

		m.logger.Info("auto-booking attempt",
			slog.String("user", req.UserID),
			slog.String("provider", req.Provider),
			slog.String("campground", req.CampgroundID),
			slog.String("campsite", pick.CampsiteID),
			slog.String("checkin", pick.CheckIn.Format("2006-01-02")),
			slog.String("checkout", pick.CheckOut.Format("2006-01-02")),
			slog.Int("nights", pick.Nights),
			slog.Float64("rating", pick.Rating),
			slog.String("batch_id", batchID),
		)

		// Fire-and-forget "attempting" DM right now; nothing in the booking
		// path waits on it.
		go func() {
			msg := fmt.Sprintf("🛒 I'm attempting to hold site **%s** for you right now (%s → %s, %d night%s). I'll let you know how I go.",
				pick.CampsiteID,
				pick.CheckIn.Format("Mon Jan 2"),
				pick.CheckOut.Format("Mon Jan 2"),
				pick.Nights, pluralS(pick.Nights),
			)
			if _, err := m.notifier.ChannelMessageSend(channelID, msg); err != nil {
				m.logger.Warn("send attempting message failed", slog.Any("err", err))
			}
		}()

		bctx, bcancel := context.WithTimeout(ctx, 2*time.Minute)
		defer bcancel()
		res, err := m.pool.HoldCampsite(bctx, req.UserID, pick.CampsiteID, req.CampgroundID, pick.CheckIn, pick.CheckOut)

		booking := db.Booking{
			BatchID:      batchID,
			UserID:       req.UserID,
			Provider:     req.Provider,
			CampgroundID: req.CampgroundID,
			CampsiteID:   pick.CampsiteID,
			Checkin:      pick.CheckIn,
			Checkout:     pick.CheckOut,
		}
		if err != nil {
			emsg := err.Error()
			booking.Outcome = db.BookingOutcomeFailed
			booking.ErrorMsg = &emsg
		} else {
			booking.Outcome = db.BookingOutcomeHeld
			if res != nil && res.OrderID != "" {
				oid := res.OrderID
				booking.OrderID = &oid
			}
		}
		recCtx, recCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, rerr := m.store.InsertBooking(recCtx, booking); rerr != nil {
			m.logger.Warn("insert booking row failed", slog.Any("err", rerr))
		}
		recCancel()

		if err != nil {
			m.logger.Warn("auto-booking failed",
				slog.String("user", req.UserID),
				slog.String("campsite", pick.CampsiteID),
				slog.Any("err", err),
			)
		} else {
			oid := ""
			if res != nil {
				oid = res.OrderID
			}
			m.logger.Info("auto-booking held",
				slog.String("user", req.UserID),
				slog.String("campsite", pick.CampsiteID),
				slog.String("order_id", oid),
			)
		}

		result := formatBookingOutcome(pick, res, err)
		if _, derr := m.notifier.ChannelMessageSend(channelID, result); derr != nil {
			m.logger.Warn("send booking result message failed", slog.Any("err", derr))
		}
	}()
}

func formatBookingOutcome(pick booker.Pick, res *booker.HoldResult, err error) string {
	if err != nil {
		if errors.Is(err, booker.ErrHumanVerification) {
			url := booker.CampsiteBookingURL(pick.CampsiteID, pick.CheckIn, pick.CheckOut)
			return fmt.Sprintf("⚠️ Recreation.gov requires a human verification step for site **%s**. [Open the site and finish manually](%s).",
				pick.CampsiteID, url)
		}
		if errors.Is(err, booker.ErrSiteUnavailable) {
			return fmt.Sprintf("🤖 Bot-on-bot violence: someone else grabbed site **%s** before us.", pick.CampsiteID)
		}
		return fmt.Sprintf("❌ Couldn't hold site **%s**: %s", pick.CampsiteID, err.Error())
	}
	url := ""
	if res != nil && res.OrderID != "" {
		url = booker.OrderURL(res.OrderID)
	}
	line := fmt.Sprintf("✅ Held site **%s** for you (%s → %s, %d night%s).",
		pick.CampsiteID,
		pick.CheckIn.Format("Mon Jan 2"),
		pick.CheckOut.Format("Mon Jan 2"),
		pick.Nights, pluralS(pick.Nights),
	)
	if url != "" {
		line += " [Finish checkout](" + url + ")"
	}
	return line
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// clipToWindow drops items outside [checkin, checkout).
func clipToWindow(items []db.AvailabilityItem, checkin, checkout time.Time) []db.AvailabilityItem {
	in := normalizeDay(checkin)
	out := normalizeDay(checkout)
	keep := items[:0:0]
	for _, it := range items {
		d := normalizeDay(it.Date)
		if (d.Equal(in) || d.After(in)) && d.Before(out) {
			keep = append(keep, it)
		}
	}
	return keep
}

func uniqueCampsiteIDs(items []db.AvailabilityItem) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, it := range items {
		if seen[it.CampsiteID] {
			continue
		}
		seen[it.CampsiteID] = true
		out = append(out, it.CampsiteID)
	}
	return out
}

func buildCandidates(items []db.AvailabilityItem, details map[string]db.CampsiteDetails) []booker.Candidate {
	byID := map[string][]time.Time{}
	for _, it := range items {
		byID[it.CampsiteID] = append(byID[it.CampsiteID], it.Date)
	}
	out := make([]booker.Candidate, 0, len(byID))
	for id, dates := range byID {
		out = append(out, booker.Candidate{
			CampsiteID: id,
			Dates:      dates,
			Rating:     details[id].Rating,
		})
	}
	return out
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
