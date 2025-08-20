package bot

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// handleListCommand prints, for each active schniff owned by the user:
// - number of checks in the last 24 hours (for that campground)
// - number of notifications in the last 24 hours (for that request)
// - latest per-date availability counts within the schniff date range
func (b *Bot) handleListCommand(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	uid := getUserID(i)
	reqs, err := b.store.ListUserActiveRequests(context.Background(), uid)
	if err != nil {
		respond(s, i, "error: "+err.Error())
		return
	}
	// Filter to user and keep stable order by created_at via ID ascending
	type item struct {
		id                int64
		provider          string
		campgroundID      string
		checkin, checkout time.Time
		created           time.Time
	}
	items := make([]item, 0)
	for _, r := range reqs {
		if r.UserID != uid || !r.Active {
			continue
		}
		items = append(items, item{id: r.ID, provider: r.Provider, campgroundID: r.CampgroundID, checkin: r.Checkin, checkout: r.Checkout, created: r.CreatedAt})
	}
	if len(items) == 0 {
		respond(s, i, "no active schniffs")
		return
	}
	sort.Slice(items, func(a, b int) bool { return items[a].id < items[b].id })

	// We'll defer initial ack for longer responses (ephemeral)
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: 1 << 6},
	})

	// Build embeds (one per schniff) to ensure links render and to stay within limits
	weekday := func(t time.Time) string { return t.Format("Mon") }
	embeds := make([]*discordgo.MessageEmbed, 0, len(items))
	for _, it := range items {
		// display name with link
		name := b.formatCampgroundWithLink(context.Background(), it.provider, it.campgroundID, it.campgroundID)

		nights := int(it.checkout.Sub(it.checkin).Hours() / 24)
		// total checks for this campground since the request was created
		totalChecks, err := b.store.CountLookupsSinceTime(context.Background(), it.provider, it.campgroundID, it.created)
		if err != nil {
			b.logger.Warn("count request checks failed", "err", err)
			totalChecks = 0
		}
		// Build description in the required format but inside an embed
		desc := strings.Builder{}
		desc.WriteString(name + "\n")
		desc.WriteString(fmt.Sprintf("%s (%s) -> %s (%s) (%d nights)\n", it.checkin.Format("2006-01-02"), weekday(it.checkin), it.checkout.Format("2006-01-02"), weekday(it.checkout), nights))
		desc.WriteString(fmt.Sprintf("total api calls: %d\n", totalChecks))

		embeds = append(embeds, &discordgo.MessageEmbed{
			Description: desc.String(),
			Timestamp:   time.Now().Format(time.RFC3339),
		})
		// Send in batches of up to 10 embeds per message to fit Discord limits
		if len(embeds) == 10 {
			_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Embeds: embeds, Flags: 1 << 6})
			if err != nil {
				b.logger.Warn("state followup send failed", "err", err)
			}
			embeds = nil
		}
	}
	if len(embeds) > 0 {
		_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Embeds: embeds, Flags: 1 << 6})
		if err != nil {
			b.logger.Warn("state followup send failed", "err", err)
		}
	}
}
