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

// warmTabOpenTimeout caps how long the immediate post-creation warm-tab
// open is allowed to take. 60s matches the chromedp Poll budget inside
// PrewarmCampground; if nav stalls longer than that we move on and let
// the 30s reconcile loop pick it up.
const warmTabOpenTimeout = 60 * time.Second

// kickWarmTabOpen fires Pool.EnsureWarmTabForRequest in the background as
// soon as a new schniff is created, so the user doesn't have to wait for
// the next reconcile-loop tick (up to 30s) before their tab is ready. If
// this fails, no problem — the reconcile loop will retry on its cadence.
// Skipped if the user has no warm pool session yet.
func (b *Bot) kickWarmTabOpen(userID string, requestID int64, campgroundID string) {
	if b.pool == nil || !b.pool.HasUser(userID) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), warmTabOpenTimeout)
		defer cancel()
		if err := b.pool.EnsureWarmTabForRequest(ctx, userID, requestID, campgroundID); err != nil {
			b.logger.Warn("immediate warm-tab open failed; reconcile loop will retry",
				slog.String("user_id", userID),
				slog.Int64("request_id", requestID),
				slog.String("campground_id", campgroundID),
				slog.Any("err", err))
		}
	}()
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
