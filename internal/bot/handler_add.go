package bot

import (
	"context"
	"fmt"
	"strings"

	"github.com/brensch/schniffer/internal/db"
	"github.com/bwmarrin/discordgo"
)

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
	_, err = b.store.AddRequest(context.Background(), db.SchniffRequest{
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
