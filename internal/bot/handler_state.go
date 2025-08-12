package bot

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// handleStateCommand prints, for each active schniff owned by the user:
// - number of checks in the last 24 hours (for that campground)
// - number of notifications in the last 24 hours (for that request)
// - latest per-date availability counts within the schniff date range
func (b *Bot) handleStateCommand(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	uid := getUserID(i)
	reqs, err := b.store.ListActiveRequests(context.Background())
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

	// Prepare output with chunking under Discord 2000 chars (use ~1600 buffer)
	var chunks []string
	var bld strings.Builder
	dateFmt := "2006-01-02"

	// We'll defer initial ack for longer responses
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource})

	for _, it := range items {
		// Campground display name
		name := it.campgroundID
		if cg, ok, _ := b.store.GetCampgroundByID(context.Background(), it.provider, it.campgroundID); ok {
			if strings.TrimSpace(cg.ParentName) != "" {
				name = cg.ParentName + " - " + cg.Name
			} else {
				name = cg.Name
			}
		}
		header := fmt.Sprintf("schniff %s: %s -> %s", name, it.checkin.Format(dateFmt), it.checkout.Format(dateFmt))

		// Checks in last 24h for provider+campground
		var checks24 int64
		_ = b.store.DB.QueryRow(`
            SELECT coalesce(count(*),0)
            FROM lookup_log
            WHERE provider=? AND campground_id=? AND checked_at >= current_timestamp - INTERVAL 1 DAY
        `, it.provider, it.campgroundID).Scan(&checks24)

		// Notifications in last 24h for this request id
		var notes24 int64
		_ = b.store.DB.QueryRow(`
            SELECT coalesce(count(*),0)
            FROM notifications
            WHERE request_id=? AND sent_at >= current_timestamp - INTERVAL 1 DAY
        `, it.id).Scan(&notes24)

		// Latest batch timestamp for campsite_state
		var latest time.Time
		_ = b.store.DB.QueryRow(`
            SELECT coalesce(max(checked_at), current_timestamp) FROM campsite_state WHERE provider=? AND campground_id=?
        `, it.provider, it.campgroundID).Scan(&latest)

		// Dates to display: inclusive of checkout to match existing UX example
		// Limit to at most 14 days to keep message size reasonable
		maxDays := 14
		dates := make([]time.Time, 0, maxDays)
		for d := it.checkin; !d.After(it.checkout) && len(dates) < maxDays; d = d.AddDate(0, 0, 1) {
			dates = append(dates, d)
		}
		// Aggregate latest availability per date in range
		counts := map[string][2]int{}
		if !latest.IsZero() {
			rows, err := b.store.DB.Query(`
                SELECT date, count(DISTINCT campsite_id) AS total, sum(CASE WHEN available THEN 1 ELSE 0 END) AS free
                FROM campsite_state
                WHERE provider=? AND campground_id=? AND checked_at=? AND date BETWEEN ? AND ?
                GROUP BY date ORDER BY date
            `, it.provider, it.campgroundID, latest, it.checkin, it.checkout)
			if err == nil {
				for rows.Next() {
					var dt time.Time
					var total, free int
					if err := rows.Scan(&dt, &total, &free); err == nil {
						counts[dt.Format(dateFmt)] = [2]int{total, free}
					}
				}
				rows.Close()
			}
		}

		// Build section
		bld.WriteString(header + "\n")
		bld.WriteString(fmt.Sprintf("%d checks in last 24 hours\n", checks24))
		bld.WriteString(fmt.Sprintf("%d notifications in the last 24 hours\n", notes24))
		bld.WriteString("Latest state:\n")
		for _, d := range dates {
			key := d.Format(dateFmt)
			c := counts[key]
			bld.WriteString(fmt.Sprintf("%s: %d/%d\n", key, c[1], c[0]))
		}
		if len(dates) == maxDays && it.checkout.After(dates[len(dates)-1]) {
			bld.WriteString("…\n")
		}
		// Spacer between schniffs
		bld.WriteString("\n")

		if bld.Len() > 1600 {
			chunks = append(chunks, bld.String())
			bld.Reset()
		}
	}

	if bld.Len() > 0 {
		chunks = append(chunks, bld.String())
	}
	if len(chunks) == 0 {
		chunks = []string{"no data"}
	}
	// Send first chunk as follow-up, then the rest
	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Content: chunks[0]}); err != nil {
		b.logger.Warn("state followup send failed", "err", err)
	}
	for _, c := range chunks[1:] {
		if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Content: c}); err != nil {
			b.logger.Warn("state followup send failed", "err", err)
		}
	}
}
