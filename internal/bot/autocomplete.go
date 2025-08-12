package bot

import (
	"context"
	"strings"

	"github.com/bwmarrin/discordgo"
)

func (b *Bot) autocompleteCampgrounds(i *discordgo.InteractionCreate, query string) []*discordgo.ApplicationCommandOptionChoice {
	ctx := context.Background()
	cgs, err := b.store.ListCampgrounds(ctx, query)
	if err != nil {
		b.logger.Warn("list campgrounds failed", "err", err)
		return nil
	}
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(cgs))
	for _, c := range cgs {
		display := c.Name
		if strings.TrimSpace(c.ParentName) != "" {
			display = c.ParentName + " - " + c.Name
		}
		display = sanitizeChoiceName(display)
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
			Name:  display,
			Value: c.Provider + "|" + c.CampgroundID,
		})
		if len(choices) >= 25 { // Discord limit
			break
		}
	}
	return choices
}

// autocompleteRemoveIDs suggests the caller's active schniffs as choices.
func (b *Bot) autocompleteRemoveIDs(i *discordgo.InteractionCreate) []*discordgo.ApplicationCommandOptionChoice {
	uid := getUserID(i)
	reqs, err := b.store.ListActiveRequests(context.Background())
	if err != nil {
		b.logger.Warn("list active reqs failed", "err", err)
		return nil
	}
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, 25)
	for _, r := range reqs {
		if r.UserID != uid {
			continue
		}
		name := r.CampgroundID
		if cg, ok, _ := b.store.GetCampgroundByID(context.Background(), r.Provider, r.CampgroundID); ok {
			if strings.TrimSpace(cg.ParentName) != "" {
				name = cg.ParentName + " - " + cg.Name
			} else {
				name = cg.Name
			}
		}
		label := r.Checkin.Format("2006-01-02") + "→" + r.Checkout.Format("2006-01-02")
		display := sanitizeChoiceName(label + " • " + name)
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: display, Value: r.ID})
		if len(choices) >= 25 {
			break
		}
	}
	if len(choices) == 0 {
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: "No active schniffs", Value: 0})
	}
	return choices
}
