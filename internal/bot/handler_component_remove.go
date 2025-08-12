package bot

import (
	"context"
	"fmt"
	"strconv"

	"github.com/bwmarrin/discordgo"
)

// handleRemoveChecksComponent processes the select menu to remove a schniff request.
func (b *Bot) handleRemoveChecksComponent(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.MessageComponentInteractionData) {
	userID := getUserID(i)
	if userID == "" {
		respond(s, i, "unable to resolve user")
		return
	}
	if len(data.Values) == 0 {
		respond(s, i, "no selection")
		return
	}
	vid := data.Values[0]
	id, err := strconv.ParseInt(vid, 10, 64)
	if err != nil {
		respond(s, i, "invalid selection")
		return
	}
	if err := b.store.DeactivateRequest(context.Background(), id, userID); err != nil {
		respond(s, i, "error: "+err.Error())
		return
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Content: fmt.Sprintf("removed schniff %d", id), Components: []discordgo.MessageComponent{}, Flags: 1 << 6},
	})
}
