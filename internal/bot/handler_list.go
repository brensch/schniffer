package bot

import (
	"context"
	"fmt"

	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleListCommand(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	reqs, err := b.store.ListActiveRequests(context.Background())
	if err != nil {
		respond(s, i, "error: "+err.Error())
		return
	}
	var out string
	for _, r := range reqs {
		if r.UserID == getUserID(i) {
			nights := int(r.Checkout.Sub(r.Checkin).Hours() / 24)
			out += fmt.Sprintf("%d: %s %s %s→%s (%d nights)\n", r.ID, r.Provider, r.CampgroundID, r.Checkin.Format("2006-01-02"), r.Checkout.Format("2006-01-02"), nights)
		}
	}
	if out == "" {
		out = "no active schniffs"
	}
	respond(s, i, out)
}
