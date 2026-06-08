package bot

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// handleListCommand renders one embed per active schniff, with the campground
// name as a clickable author header and a sidebar color hashed from the
// parent-park name so campgrounds in the same park share a color.
func (b *Bot) handleListCommand(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	uid := getUserID(i)
	reqs, err := b.store.ListUserActiveRequests(context.Background(), uid)
	if err != nil {
		respond(s, i, "error: "+err.Error())
		return
	}
	type item struct {
		id                int64
		provider          string
		campgroundID      string
		checkin, checkout time.Time
		created           time.Time
		minimumNights     sql.NullInt64
		strategy          sql.NullString
	}
	items := make([]item, 0)
	for _, r := range reqs {
		if r.UserID != uid || !r.Active {
			continue
		}
		items = append(items, item{
			id:            r.ID,
			provider:      r.Provider,
			campgroundID:  r.CampgroundID,
			checkin:       r.Checkin,
			checkout:      r.Checkout,
			created:       r.CreatedAt,
			minimumNights: r.MinimumNights,
			strategy:      r.Strategy,
		})
	}
	if len(items) == 0 {
		respond(s, i, "no active schniffs")
		return
	}
	sort.Slice(items, func(a, b int) bool { return items[a].id < items[b].id })

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: 1 << 6},
	})

	ctx := context.Background()
	embeds := make([]*discordgo.MessageEmbed, 0, len(items))
	flushBatch := func() {
		if len(embeds) == 0 {
			return
		}
		_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Embeds: embeds, Flags: 1 << 6})
		if err != nil {
			b.logger.Warn("list followup send failed", "err", err)
		}
		embeds = embeds[:0]
	}

	for _, it := range items {
		name := it.campgroundID
		var url string
		if cg, ok, _ := b.store.GetCampgroundByID(ctx, it.provider, it.campgroundID); ok {
			name = cg.Name
		}
		if pi, ok := b.registry.Get(it.provider); ok {
			url = pi.CampgroundURL(it.campgroundID)
		}

		totalChecks, err := b.store.CountLookupsSinceTime(ctx, it.provider, it.campgroundID, it.created)
		if err != nil {
			b.logger.Warn("count request checks failed", "err", err)
			totalChecks = 0
		}
		nights := int(it.checkout.Sub(it.checkin).Hours() / 24)

		bits := []string{}
		if it.minimumNights.Valid {
			bits = append(bits, fmt.Sprintf("min %d", it.minimumNights.Int64))
		}
		if it.strategy.Valid && it.strategy.String != "" {
			bits = append(bits, it.strategy.String)
		}
		bits = append(bits, fmt.Sprintf("%d checks", totalChecks))

		desc := fmt.Sprintf(
			"**%s %s** → **%s %s**  ·  %d %s\n-# %s",
			it.checkin.Format("2006-01-02"), it.checkin.Format("Mon"),
			it.checkout.Format("2006-01-02"), it.checkout.Format("Mon"),
			nights, nightsWord(nights),
			strings.Join(bits, " · "),
		)

		embeds = append(embeds, &discordgo.MessageEmbed{
			Author:      &discordgo.MessageEmbedAuthor{Name: name, URL: url},
			Description: desc,
			Color:       colorFromName(parkOf(name)),
		})
		if len(embeds) == 10 {
			flushBatch()
		}
	}
	flushBatch()
}

func nightsWord(n int) string {
	if n == 1 {
		return "night"
	}
	return "nights"
}
