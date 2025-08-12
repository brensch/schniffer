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
	cg := opts["campground"].StringValue()
	parts := strings.SplitN(cg, "|", 2)
	if len(parts) != 2 {
		respond(s, i, "invalid campground selection")
		return
	}
	provider := parts[0]
	campID := parts[1]
	start, end, err := parseDates(opts["checkin"].StringValue(), opts["checkout"].StringValue())
	if err != nil {
		respond(s, i, "invalid dates: "+err.Error())
		return
	}
	uid := getUserID(i)
	id, err := b.store.AddRequest(context.Background(), db.SchniffRequest{UserID: uid, Provider: provider, CampgroundID: campID, Checkin: start, Checkout: end})
	if err != nil {
		respond(s, i, "error: "+err.Error())
		return
	}
	respond(s, i, fmt.Sprintf("added schniff %d", id))
}
