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

	uid := getUserID(i)
	_, err = b.store.AddRequest(context.Background(), db.SchniffRequest{UserID: uid, Provider: campgroundProvider, CampgroundID: campgroundID, Checkin: start, Checkout: end})
	if err != nil {
		respond(s, i, "error: "+err.Error())
		return
	}

	// get the length of the stay
	stayDuration := end.Sub(start)
	formattedName := b.formatCampgroundWithLink(context.Background(), campgroundProvider, campgroundID, campgroundName)
	respond(s, i, fmt.Sprintf("Now schniffing: %s, dates %s to %s (%.0f nights)", formattedName, start.Format("2006-01-02"), end.Format("2006-01-02"), stayDuration.Hours()/24))
}
