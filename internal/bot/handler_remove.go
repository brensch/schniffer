package bot

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleRemoveCommand(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	uid := getUserID(i)
	opts := optMap(sub.Options)
	opt, ok := opts["ids"]
	if ok && opt != nil {
		id := int64(opt.IntValue())
		err := b.store.DeactivateRequest(context.Background(), id, uid)
		if err != nil {
			respond(s, i, "error: "+err.Error())
			return
		}
		respond(s, i, "removed")
		return
	}

	// No ID provided, remove all schniffs
	reqs, err := b.store.ListActiveRequests(context.Background())
	if err != nil {
		b.logger.Warn("list active reqs failed", "err", err)
		respond(s, i, "failed to get schniffs to remove")
		return
	}

	for _, r := range reqs {
		err = b.store.DeactivateRequest(context.Background(), r.ID, r.UserID)
		if err != nil {
			respond(s, i, "error: "+err.Error())
			return
		}
		slog.Info("Removed schniff for user", "id", r.ID, "user_id", r.UserID)
	}
	respond(s, i, "removed all schniffs")
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
			name = cg.Name
		}
		label := r.Checkin.Format("2006-01-02") + "→" + r.Checkout.Format("2006-01-02")
		display := sanitizeGenericText(label + " • " + name)
		value := sanitizeChoiceValue(strconv.FormatInt(r.ID, 10))
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: display, Value: value})
		if len(choices) >= 25 {
			break
		}
	}
	if len(choices) == 0 {
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: "No active schniffs", Value: "0"})
	}
	return choices
}
