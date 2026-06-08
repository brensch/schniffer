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

// recentHoldWindow is how far back we look in the bookings table to decide
// whether a campsite flipping back to "available" is one of our own holds
// expiring (vs. a genuine new opening). rec.gov cart holds last 15 minutes;
// the extra few cover polling jitter.
const recentHoldWindow = 20 * time.Minute

// ProcessNotificationsWithBatches handles the state-change-based notification
// system. Requests are grouped into (provider, campground) buckets so we can
// arbitrate between users who want the same site before any holds fire.
func (m *Manager) ProcessNotificationsWithBatches(ctx context.Context, requests []db.SchniffRequest) error {
	m.logger.Info("processing notifications", slog.Int("request_count", len(requests)))

	stateChanges, err := m.store.GetUnnotifiedStateChanges(ctx, requests)
	if err != nil {
		m.logger.Warn("get unnotified state changes failed", slog.Any("err", err))
		return err
	}
	m.logger.Info("found unnotified state changes", slog.Int("count", len(stateChanges)))
	if len(stateChanges) == 0 {
		return nil
	}

	changesByRequest := groupStateChangesByRequest(stateChanges)
	reqIndex := indexRequestsByID(requests)

	type bucketKey struct{ provider, campground string }
	bucketReqs := map[bucketKey][]int64{}
	for reqID := range changesByRequest {
		r, ok := reqIndex[reqID]
		if !ok {
			m.logger.Warn("request not found for state changes", slog.Int64("requestID", reqID))
			continue
		}
		k := bucketKey{r.Provider, r.CampgroundID}
		bucketReqs[k] = append(bucketReqs[k], reqID)
	}

	batchID := uuid.New().String()
	now := time.Now()

	var nmu sync.Mutex
	var notificationsToRecord []db.Notification

	var bwg sync.WaitGroup
	for k, ids := range bucketReqs {
		bwg.Add(1)
		go func(k bucketKey, ids []int64) {
			defer bwg.Done()
			notes := m.processCampgroundBucket(ctx, k.provider, k.campground, ids, reqIndex, changesByRequest, batchID, now)
			if len(notes) == 0 {
				return
			}
			nmu.Lock()
			notificationsToRecord = append(notificationsToRecord, notes...)
			nmu.Unlock()
		}(k, ids)
	}
	bwg.Wait()

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

// preparedRequest carries the per-request data we need across arbitration
// and dispatch within a single bucket goroutine.
type preparedRequest struct {
	req          db.SchniffRequest
	changes      []db.StateChangeForRequest
	allAvailable []db.AvailabilityItem
	// risingEdges is the per-schniff rising-edge set: campsite-day pairs
	// that are currently available and that we haven't already shown this
	// user (or that came back after going away). This is what the embed
	// lists. On the schniff's first fire it'll cover all matching sites; on
	// later fires it narrows down to the truly new ones.
	risingEdges []db.AvailabilityItem
	candidates  []booker.Candidate
}

// bookOutcome travels back from the per-winner hold goroutines to the
// bucket coordinator, which uses .success to decide which displaced losers
// get DM'd.
type bookOutcome struct {
	winnerReqID  int64
	winnerUserID string
	pick         booker.Pick
	success      bool
}

// processCampgroundBucket arbitrates between all requests pointing at the
// same campground, dispatches the resulting DMs / channel pings / holds,
// then (after holds finish) sends "older schniff won" summary DMs to losers.
// Returns the notification rows to insert for the whole bucket.
func (m *Manager) processCampgroundBucket(
	ctx context.Context,
	provider, campgroundID string,
	reqIDs []int64,
	reqIndex map[int64]db.SchniffRequest,
	changesByRequest map[int64][]db.StateChangeForRequest,
	batchID string,
	now time.Time,
) []db.Notification {
	recentHolds, err := m.store.RecentHoldOwnersForCampground(ctx, provider, campgroundID, now.Add(-recentHoldWindow))
	if err != nil {
		m.logger.Warn("recent-hold lookup failed",
			slog.String("provider", provider),
			slog.String("campground", campgroundID),
			slog.Any("err", err))
	}
	recentHeldSet := make(map[string]struct{}, len(recentHolds))
	expiringHoldByCampsite := make(map[string]db.RecentHoldOwner, len(recentHolds))
	for _, h := range recentHolds {
		recentHeldSet[h.CampsiteID] = struct{}{}
		expiringHoldByCampsite[h.CampsiteID] = h
	}

	// skipNotifs collects mark-as-processed rows for requests that don't
	// make it into prep (no newly-available changes, or strategy not met).
	// Without these, GetUnnotifiedStateChanges keeps returning the same
	// state changes every poll AND the rising-edge query gets a stale view
	// (e.g. an unavailable transition we never recorded looks like the
	// site is "still notified as available").
	skipNotifs := make([]db.Notification, 0)
	markProcessed := func(req db.SchniffRequest, changes []db.StateChangeForRequest) {
		for _, c := range changes {
			state := "available"
			if !c.NewAvailable {
				state = "unavailable"
			}
			cc := c
			skipNotifs = append(skipNotifs, db.Notification{
				RequestID:     req.ID,
				UserID:        req.UserID,
				Provider:      cc.Provider,
				CampgroundID:  cc.CampgroundID,
				CampsiteID:    cc.CampsiteID,
				Date:          cc.Date,
				State:         state,
				StateChangeID: &cc.ID,
				SentAt:        now,
			})
		}
	}

	prep := make([]preparedRequest, 0, len(reqIDs))
	for _, id := range reqIDs {
		req, ok := reqIndex[id]
		if !ok {
			continue
		}
		changes := changesByRequest[id]
		newlyAvail, _ := separateChanges(changes)
		if len(newlyAvail) == 0 {
			m.logger.Info("no newly-available changes; skipping embed",
				slog.Int64("requestID", req.ID),
				slog.String("userID", req.UserID))
			markProcessed(req, changes)
			continue
		}

		allAvailable, qerr := m.store.GetCurrentlyAvailableCampsites(ctx, req.Provider, req.CampgroundID, req.Checkin, req.Checkout)
		if qerr != nil {
			m.logger.Warn("get currently available campsites failed", slog.Any("err", qerr))
		}
		if !requestConditionsMet(req, allAvailable) {
			m.logger.Info("schniff conditions not yet met; skipping embed",
				slog.Int64("requestID", req.ID),
				slog.String("userID", req.UserID),
				slog.Bool("hasMinNights", req.MinimumNights.Valid),
				slog.Bool("hasStrategy", req.Strategy.Valid))
			markProcessed(req, changes)
			continue
		}

		clipped := clipToWindow(newlyAvail, req.Checkin, req.Checkout)
		dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
		details, _ := m.store.GetCampsiteDetailsBatch(dctx, req.Provider, req.CampgroundID, uniqueCampsiteIDs(clipped))
		dcancel()
		candidates := buildCandidates(clipped, details)

		risingEdges, rerr := m.store.GetRisingEdgesForRequest(ctx, req)
		if rerr != nil {
			m.logger.Warn("rising-edge lookup failed; falling back to this-batch changes",
				slog.Int64("requestID", req.ID),
				slog.Any("err", rerr))
			risingEdges = clipped
		}

		prep = append(prep, preparedRequest{
			req:          req,
			changes:      changes,
			allAvailable: allAvailable,
			risingEdges:  risingEdges,
			candidates:   candidates,
		})
	}

	if len(prep) == 0 {
		return skipNotifs
	}

	inputs := make([]ArbInput, len(prep))
	for i, p := range prep {
		inputs[i] = ArbInput{Request: p.req, Candidates: p.candidates}
	}
	results := Arbitrate(inputs, expiringHoldByCampsite)

	outcomeChan := make(chan bookOutcome, len(results))
	expectedBooks := 0

	notifs := make([]db.Notification, 0)
	for i, r := range results {
		p := prep[i]

		for _, c := range p.changes {
			state := "available"
			if !c.NewAvailable {
				state = "unavailable"
			}
			cc := c
			notifs = append(notifs, db.Notification{
				RequestID:     p.req.ID,
				UserID:        p.req.UserID,
				Provider:      cc.Provider,
				CampgroundID:  cc.CampgroundID,
				CampsiteID:    cc.CampsiteID,
				Date:          cc.Date,
				State:         state,
				StateChangeID: &cc.ID,
				SentAt:        now,
			})
		}

		// Losers: no DM, no embed, no channel, no book. Displacement DM is
		// sent later after winners' holds resolve.
		if !r.Won && !r.ExpiringOwner {
			continue
		}

		channel, cerr := m.notifier.UserChannelCreate(p.req.UserID)
		if cerr != nil {
			m.logger.Warn("user channel create failed",
				slog.String("userID", p.req.UserID),
				slog.Any("err", cerr))
			continue
		}

		// Send the normal "site available" embed to both winners and
		// expiring-owners. The follow-up + book decision differs below.
		embed := m.sendAvailabilityEmbed(ctx, p)
		if embed != nil {
			if _, err := m.notifier.ChannelMessageSendEmbed(channel.ID, embed); err != nil {
				m.logger.Error("failed to send notification embed",
					slog.String("userID", p.req.UserID),
					slog.Any("err", err))
			}
		}

		// Record that we just told the user about each rising-edge site, so
		// next fire's rising-edge query knows not to re-show them. Items
		// that came from a state change in this batch already have a row
		// inserted above with state_change_id set; skip those to avoid dupes.
		seen := make(map[string]struct{}, len(p.changes))
		for _, c := range p.changes {
			if c.NewAvailable {
				seen[c.CampsiteID+"|"+c.Date.UTC().Format("2006-01-02")] = struct{}{}
			}
		}
		for _, it := range p.risingEdges {
			k := it.CampsiteID + "|" + it.Date.UTC().Format("2006-01-02")
			if _, dup := seen[k]; dup {
				continue
			}
			notifs = append(notifs, db.Notification{
				RequestID:    p.req.ID,
				UserID:       p.req.UserID,
				Provider:     p.req.Provider,
				CampgroundID: p.req.CampgroundID,
				CampsiteID:   it.CampsiteID,
				Date:         it.Date,
				State:        "available",
				SentAt:       now,
			})
		}

		if r.ExpiringOwner {
			followup := fmt.Sprintf(
				"🛒 i put site **%s** in your basket before and you didn't get it. this notification is that basket expiring. to avoid an infinite loop i'm not doing it this time.",
				r.ExpiringOwnerPick.CampsiteID,
			)
			if _, err := m.notifier.ChannelMessageSend(channel.ID, followup); err != nil {
				m.logger.Warn("send basket-expired followup failed", slog.Any("err", err))
			}
		}

		// Auto-book + channel ping only when the user actually won a site in
		// arbitration. An ExpiringOwner who didn't also win something else
		// skips both. Channel is suppressed when the won site is one we
		// recently held (i.e. the "new" availability is a cycle, not news).
		if r.Won {
			if _, recyc := recentHeldSet[r.Pick.CampsiteID]; !recyc {
				m.maybeChannelBroadcast(p.req.UserID)
			}
			if m.pool != nil && m.pool.HasUser(p.req.UserID) {
				expectedBooks++
				go m.attemptHoldAndReport(ctx, p.req, r.Pick, batchID, channel.ID, outcomeChan)
			}
		}
	}

	outcomesByReq := make(map[int64]bookOutcome, expectedBooks)
	for i := 0; i < expectedBooks; i++ {
		select {
		case <-ctx.Done():
			i = expectedBooks
		case o := <-outcomeChan:
			outcomesByReq[o.winnerReqID] = o
		}
	}

	for i, r := range results {
		if r.Won || len(r.DisplacedBy) == 0 {
			continue
		}
		successful := r.DisplacedBy[:0:0]
		for _, d := range r.DisplacedBy {
			if o, ok := outcomesByReq[d.WinnerReqID]; ok && o.success {
				successful = append(successful, d)
			}
		}
		if len(successful) == 0 {
			continue
		}
		m.sendDisplacementDM(ctx, prep[i].req.UserID, successful)
	}

	return append(notifs, skipNotifs...)
}

// sendAvailabilityEmbed builds the embed for a single request's currently
// available campsites. Returns the embed (caller sends), or nil if there's
// nothing to show. Extracted so winners and expiring-owners share the same
// embed format.
func (m *Manager) sendAvailabilityEmbed(ctx context.Context, p preparedRequest) *discordgo.MessageEmbed {
	// Embed lists only the rising-edge items for this schniff: first fire
	// gets the full matching inventory, subsequent fires only show what's
	// newly available since we last told them. Falls back to allAvailable
	// if the rising-edge query came back empty (defensive — shouldn't
	// happen since we got here via a state change).
	source := p.risingEdges
	if len(source) == 0 {
		source = p.allAvailable
	}
	byCampsite := groupAvailabilityByCampsite(source)
	campsiteIDs := collectMapKeys(byCampsite)
	detailsMap, derr := m.store.GetCampsiteDetailsBatch(ctx, p.req.Provider, p.req.CampgroundID, campsiteIDs)
	if derr != nil {
		m.logger.Warn("GetCampsiteDetailsBatch failed; using basic details", slog.Any("err", derr))
		detailsMap = map[string]db.CampsiteDetails{}
	}
	stats := buildCampsiteStats(byCampsite, p.req.Checkin, p.req.Checkout, detailsMap)
	campground, _, gerr := m.store.GetCampgroundByID(ctx, p.req.Provider, p.req.CampgroundID)
	if gerr != nil {
		m.logger.Warn("GetCampgroundByID failed; proceeding with minimal info", slog.Any("err", gerr))
	}
	campgroundURL := m.CampgroundURL(p.req.Provider, p.req.CampgroundID)
	provider, _ := m.reg.Get(p.req.Provider)

	embeds := BuildNotificationEmbeds(
		p.req.Checkin, p.req.Checkout, p.req.UserID,
		campground.Name, campgroundURL, campground.ID,
		stats,
		provider,
	)
	for _, e := range embeds {
		if e != nil {
			return e
		}
	}
	return nil
}

// maybeChannelBroadcast posts the silly broadcast to the public summary
// channel; isolated so the bucket coordinator can suppress it when the
// triggering availability is one of our own recent holds expiring.
func (m *Manager) maybeChannelBroadcast(userID string) {
	if m.summaryChannelID == "" {
		return
	}
	if _, err := m.notifier.ChannelMessageSend(m.summaryChannelID, nonsense.RandomSillyBroadcast(userID)); err != nil {
		m.logger.Warn("send channel broadcast failed", slog.Any("err", err))
	}
}

// attemptHoldAndReport runs one HoldCampsite call synchronously and reports
// the result back to the bucket coordinator via outcomeChan. It also sends
// the user-facing "attempting" + result DMs directly to channelID, and
// records the booking row to the DB — same side effects as the old
// startBookingAttempt, just structured to return its outcome upward.
func (m *Manager) attemptHoldAndReport(
	ctx context.Context,
	req db.SchniffRequest,
	pick booker.Pick,
	batchID string,
	channelID string,
	outcomeChan chan<- bookOutcome,
) {
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
			slog.Any("err", err))
	} else {
		oid := ""
		if res != nil {
			oid = res.OrderID
		}
		m.logger.Info("auto-booking held",
			slog.String("user", req.UserID),
			slog.String("campsite", pick.CampsiteID),
			slog.String("order_id", oid))
	}

	result := formatBookingOutcome(pick, res, err)
	if _, derr := m.notifier.ChannelMessageSend(channelID, result); derr != nil {
		m.logger.Warn("send booking result message failed", slog.Any("err", derr))
	}

	outcomeChan <- bookOutcome{
		winnerReqID:  req.ID,
		winnerUserID: req.UserID,
		pick:         pick,
		success:      err == nil,
	}
}

// sendDisplacementDM tells a user that an older schniff (lower request_id)
// beat them to one or more sites this tick, naming the winners so the user
// can reach out. Winner IDs are plain text — DMs from a bot can't ping
// other users anyway, and we want recipients to know the username.
func (m *Manager) sendDisplacementDM(ctx context.Context, userID string, displacements []DisplacementRecord) {
	channel, err := m.notifier.UserChannelCreate(userID)
	if err != nil {
		m.logger.Warn("displacement DM: user channel create failed",
			slog.String("userID", userID),
			slog.Any("err", err))
		return
	}

	var b strings.Builder
	b.WriteString("👀 Heads up — a site you were watching opened up, but an older schniff got there first:\n")
	for _, d := range displacements {
		name := m.lookupUsername(d.WinnerUserID)
		b.WriteString(fmt.Sprintf("• Site **%s** → **%s** created their schniff before you, so I booked it for them. It's in their basket now.\n",
			d.CampsiteID, name))
	}
	if _, err := m.notifier.ChannelMessageSend(channel.ID, b.String()); err != nil {
		m.logger.Warn("send displacement DM failed",
			slog.String("userID", userID),
			slog.Any("err", err))
	}
	_ = ctx
}

// lookupUsername returns the Discord username for userID, falling back to a
// raw ID reference if the lookup fails. Plain text only — DMs from a bot
// can't ping other users.
func (m *Manager) lookupUsername(userID string) string {
	if m.notifier == nil {
		return userID
	}
	u, err := m.notifier.User(userID)
	if err != nil || u == nil || u.Username == "" {
		return userID
	}
	return u.Username
}

func formatBookingOutcome(pick booker.Pick, res *booker.HoldResult, err error) string {
	if err != nil {
		if errors.Is(err, booker.ErrHumanVerification) {
			url := booker.CampsiteBookingURL(pick.CampsiteID, pick.CheckIn, pick.CheckOut)
			return fmt.Sprintf("⚠️ Recreation.gov requires a human verification step for site **%s**. [Open the site and finish manually](%s).",
				pick.CampsiteID, url)
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
