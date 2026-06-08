package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

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
	embeds := make([]*discordgo.MessageEmbed, 0, len(groups))
	for _, g := range groups {
		lines := make([]string, 0, len(g.Campgrounds))
		for _, cg := range g.Campgrounds {
			lines = append(lines, "▸ "+b.formatCampgroundWithLink(ctx, cg.Provider, cg.CampgroundID, cg.CampgroundID))
		}
		embeds = append(embeds, &discordgo.MessageEmbed{
			Author:      &discordgo.MessageEmbedAuthor{Name: "Group: " + g.Name},
			Description: strings.Join(lines, "\n"),
			Color:       colorFromName(g.Name),
			Footer:      &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("%d site%s", len(g.Campgrounds), plural(len(g.Campgrounds)))},
		})
	}

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
