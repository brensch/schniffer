package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleGroupListCommand(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	uid := getUserID(i)
	groups, err := b.store.GetUserGroups(context.Background(), uid)
	if err != nil {
		respond(s, i, "error: "+err.Error())
		return
	}
	if len(groups) == 0 {
		respond(s, i, "you have no groups. Run `/schniff map` to create one.")
		return
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: 1 << 6},
	})

	ctx := context.Background()
	const maxDesc = 3800
	desc := strings.Builder{}
	embeds := make([]*discordgo.MessageEmbed, 0)
	flush := func() {
		if desc.Len() == 0 {
			return
		}
		embeds = append(embeds, &discordgo.MessageEmbed{
			Description: desc.String(),
			Timestamp:   time.Now().Format(time.RFC3339),
		})
		desc.Reset()
	}

	for _, g := range groups {
		block := strings.Builder{}
		block.WriteString(fmt.Sprintf("**%s** · `id %d` · %d site%s\n",
			sanitizeGenericText(g.Name), g.ID, len(g.Campgrounds), plural(len(g.Campgrounds))))
		for _, cg := range g.Campgrounds {
			name := b.formatCampgroundWithLink(ctx, cg.Provider, cg.CampgroundID, cg.CampgroundID)
			block.WriteString("• " + name + "\n")
		}
		block.WriteString("\n")

		if desc.Len()+block.Len() > maxDesc {
			flush()
		}
		desc.WriteString(block.String())
	}
	flush()

	// Send in batches of up to 10 embeds per message.
	for start := 0; start < len(embeds); start += 10 {
		end := start + 10
		if end > len(embeds) {
			end = len(embeds)
		}
		_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Embeds: embeds[start:end], Flags: 1 << 6})
		if err != nil {
			b.logger.Warn("group list followup send failed", "err", err)
		}
	}
}

func (b *Bot) handleGroupRemoveCommand(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	uid := getUserID(i)
	opts := optMap(sub.Options)
	groupResponse, ok := opts["group"]
	if !ok || groupResponse == nil {
		respond(s, i, "group selection is required")
		return
	}
	val := groupResponse.StringValue()
	if val == noGroupsFound {
		respond(s, i, "bro you're not meant to click that option.")
		return
	}
	parts := strings.SplitN(val, "||", 2)
	if len(parts) != 2 {
		respond(s, i, "invalid group selection")
		return
	}
	groupID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		respond(s, i, "invalid group ID")
		return
	}
	if err := b.store.DeleteGroup(context.Background(), groupID, uid); err != nil {
		respond(s, i, "error: "+err.Error())
		return
	}
	respond(s, i, fmt.Sprintf("removed group '%s'", parts[1]))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
