package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/bwmarrin/discordgo"
)

// warmTabRegisterTimeout caps how long the synchronous slot-claim is
// allowed to take. The slot publish itself is ~microseconds; we only
// time out if ensureAlive has to relaunch a dead session, which is rare.
const warmTabRegisterTimeout = 10 * time.Second

// kickWarmTabOpen synchronously claims a warm-tab slot for the new
// schniff and kicks the actual chromedp nav in the background. Returns
// once the slot is published — guarantees that any booking goroutine
// arriving after this point will find the slot and queue on its mutex
// instead of falling back to main_session.
//
// Skipped if the user has no warm pool session (not auto-book linked
// yet). Errors from the synchronous claim are logged; the 30s
// reconcile loop will retry. Errors from the background nav are
// logged inside the Pool.
func (b *Bot) kickWarmTabOpen(userID string, requestID int64, campgroundID string) {
	if b.pool == nil || !b.pool.HasUser(userID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), warmTabRegisterTimeout)
	defer cancel()
	if err := b.pool.OpenWarmTabForRequestAsync(ctx, userID, requestID, campgroundID); err != nil {
		b.logger.Warn("warm-tab slot claim failed; reconcile loop will retry",
			slog.String("user_id", userID),
			slog.Int64("request_id", requestID),
			slog.String("campground_id", campgroundID),
			slog.Any("err", err))
	}
}

func (b *Bot) handleAddCommand(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	opts := optMap(sub.Options)
	campgroundResponse, ok := opts["campground"]
	if !ok || campgroundResponse == nil {
		respond(s, i, "campground selection is required")
		return
	}

	checkinResponse, ok := opts["checkin"]
	if !ok || checkinResponse == nil {
		respond(s, i, "check-in date is required")
		return
	}

	checkoutResponse, ok := opts["checkout"]
	if !ok || checkoutResponse == nil {
		respond(s, i, "check-out date is required")
		return
	}

	campgroundIDAndProvider := campgroundResponse.StringValue()
	parts := strings.SplitN(campgroundIDAndProvider, "||", 3)
	if len(parts) != 3 {
		respond(s, i, "invalid campground selection")
		return
	}
	campgroundProvider := parts[0]
	campgroundID := parts[1]
	campgroundName := parts[2]
	start, end, err := parseDates(opts["checkin"].StringValue(), opts["checkout"].StringValue())
	if err != nil {
		respond(s, i, "invalid dates: "+err.Error())
		return
	}

	if !start.Before(end) {
		respond(s, i, "checkin must be before checkout")
		return
	}

	minN, strat, ferr := parseScheduleFilters(opts, start, end)
	if ferr != "" {
		respond(s, i, ferr)
		return
	}

	uid := getUserID(i)
	reqID, err := b.store.AddRequest(context.Background(), db.SchniffRequest{
		UserID:        uid,
		Provider:      campgroundProvider,
		CampgroundID:  campgroundID,
		Checkin:       start,
		Checkout:      end,
		MinimumNights: minN,
		Strategy:      strat,
	})
	if err != nil {
		respond(s, i, "error: "+err.Error())
		return
	}
	// Open the warm tab right now so the first booking on this schniff
	// hits the fast path (~1.4s) instead of waiting up to 30s for the
	// reconcile loop and paying the cold-start tax (~4s).
	b.kickWarmTabOpen(uid, reqID, campgroundID)

	// get the length of the stay
	stayDuration := end.Sub(start)
	formattedName := b.formatCampgroundWithLink(context.Background(), campgroundProvider, campgroundID, campgroundName)
	msg := fmt.Sprintf("Now schniffing: %s, dates %s to %s (%.0f nights)", formattedName, start.Format("2006-01-02"), end.Format("2006-01-02"), stayDuration.Hours()/24)
	if minN.Valid {
		msg += fmt.Sprintf(", minimum_nights=%d", minN.Int64)
	}
	if strat.Valid {
		msg += fmt.Sprintf(", strategy=%s", strat.String)
	}
	respond(s, i, msg)
}

func (b *Bot) autocompleteCampgrounds(i *discordgo.InteractionCreate, query string) []*discordgo.ApplicationCommandOptionChoice {
	ctx := context.Background()
	cgs, err := b.store.ListCampgrounds(ctx, query)
	if err != nil {
		b.logger.Warn("list campgrounds failed", "err", err)
		return nil
	}
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(cgs))
	for _, c := range cgs {
		display := sanitizeChoiceName(c.Name, c.Provider, c.Rating)
		value := strings.Join([]string{c.Provider, c.ID, c.Name}, "||")
		value = sanitizeChoiceValue(value)
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
			Name:  display,
			Value: value,
		})
		if len(choices) >= 25 { // Discord limit
			break
		}
	}
	return choices
}
