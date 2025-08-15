package bot

import (
	"context"
	"fmt"
	"strconv"

	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleRemoveCommand(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	uid := getUserID(i)
	opts := optMap(sub.Options)
	if opt, ok := opts["ids"]; ok && opt != nil {
		id := int64(opt.IntValue())
		err := b.store.DeactivateRequest(context.Background(), id, uid)
		if err != nil {
			respond(s, i, "error: "+err.Error())
			return
		}
		respond(s, i, "removed")
		return
	}
	// Interactive removal: present select of active schniffs for this user
	reqs, err := b.store.ListActiveRequests(context.Background())
	if err != nil {
		respond(s, i, "error: "+err.Error())
		return
	}
	options := []discordgo.SelectMenuOption{}
	count := 0
	for _, r := range reqs {
		if r.UserID != uid {
			continue
		}
		name := r.CampgroundID
		if cg, ok, _ := b.store.GetCampgroundByID(context.Background(), r.Provider, r.CampgroundID); ok {
			name = cg.Name
		}
		nights := int(r.Checkout.Sub(r.Checkin).Hours() / 24)
		label := fmt.Sprintf("%s → %s • %d night(s)", r.Checkin.Format("2006-01-02"), r.Checkout.Format("2006-01-02"), nights)
		desc := name
		options = append(options, discordgo.SelectMenuOption{Label: label, Description: desc, Value: strconv.FormatInt(r.ID, 10)})
		count++
		if count >= 25 {
			break
		}
	}
	if len(options) == 0 {
		respond(s, i, "no active schniffs")
		return
	}
	selectMenu := discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{
				CustomID:    "remove_checks",
				Placeholder: "Select a schniff to remove",
				Options:     options,
			},
		},
	}
	err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content:    "Pick a schniff to remove. You'll get a confirmation after selection.",
			Components: []discordgo.MessageComponent{selectMenu},
			Flags:      1 << 6,
		},
	})
	if err != nil {
		b.logger.Warn("remove respond failed", "err", err)
	}
}
