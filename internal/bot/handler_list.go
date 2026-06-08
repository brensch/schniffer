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

	// We'll defer initial ack for longer responses (ephemeral)
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: 1 << 6},
	})

	// Build a compact, mobile-friendly list: two lines per schniff, many per embed.
	weekday := func(t time.Time) string { return t.Format("Mon") }
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

	for _, it := range items {
		name := b.formatCampgroundWithLink(context.Background(), it.provider, it.campgroundID, it.campgroundID)
		nights := int(it.checkout.Sub(it.checkin).Hours() / 24)
		totalChecks, err := b.store.CountLookupsSinceTime(context.Background(), it.provider, it.campgroundID, it.created)
		if err != nil {
			b.logger.Warn("count request checks failed", "err", err)
			totalChecks = 0
		}

		block := strings.Builder{}
		block.WriteString(fmt.Sprintf("**%s** · `#%d`\n", name, it.id))
		// Compact details line, separator-delimited so it wraps cleanly on mobile.
		bits := []string{
			fmt.Sprintf("%s %s → %s %s",
				it.checkin.Format("2006-01-02"), weekday(it.checkin),
				it.checkout.Format("2006-01-02"), weekday(it.checkout)),
			fmt.Sprintf("%dn", nights),
		}
		if it.minimumNights.Valid {
			bits = append(bits, fmt.Sprintf("min %d", it.minimumNights.Int64))
		}
		if it.strategy.Valid && it.strategy.String != "" {
			bits = append(bits, it.strategy.String)
		}
		bits = append(bits, fmt.Sprintf("%d checks", totalChecks))
		block.WriteString(strings.Join(bits, " · "))
		block.WriteString("\n\n")

		if desc.Len()+block.Len() > maxDesc {
			flush()
		}
		desc.WriteString(block.String())
	}
	flush()

	for start := 0; start < len(embeds); start += 10 {
		end := start + 10
		if end > len(embeds) {
			end = len(embeds)
		}
		_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Embeds: embeds[start:end], Flags: 1 << 6})
		if err != nil {
			b.logger.Warn("state followup send failed", "err", err)
		}
	}
}
