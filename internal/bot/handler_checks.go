package bot

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleChecksCommand(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	userID := getUserID(i)
	combos := map[string]struct{}{}
	rows, err := b.store.DB.Query(`SELECT DISTINCT provider, campground_id FROM schniff_requests WHERE user_id=?`, userID)
	if err != nil {
		respond(s, i, "error: "+err.Error())
		return
	}
	for rows.Next() {
		var p, cg string
		_ = rows.Scan(&p, &cg)
		combos[p+"|"+cg] = struct{}{}
	}
	rows.Close()
	if len(combos) == 0 {
		respond(s, i, "no requests found")
		return
	}
	rows, err = b.store.DB.Query(`
        SELECT l.provider, l.campground_id, l.checked_at, l.success, coalesce(l.err, ''), r.id, coalesce(r.checkin, r.start_date) as checkin, coalesce(r.checkout, r.end_date) as checkout
        FROM lookup_log l
        JOIN schniff_requests r ON r.user_id=? AND r.provider=l.provider AND r.campground_id=l.campground_id
        ORDER BY l.checked_at DESC
        LIMIT 200
    `, userID)
	if err != nil {
		respond(s, i, "error: "+err.Error())
		return
	}
	type reqSpan struct {
		id         int64
		start, end time.Time
	}
	type checkKey struct {
		prov, cg string
		t        time.Time
		ok       bool
	}
	grouped := map[checkKey][]reqSpan{}
	order := []checkKey{}
	for rows.Next() {
		var prov, cg string
		var ts time.Time
		var success bool
		var errStr string
		var id int64
		var start, end time.Time
		if err := rows.Scan(&prov, &cg, &ts, &success, &errStr, &id, &start, &end); err != nil {
			continue
		}
		k := checkKey{prov: prov, cg: cg, t: ts, ok: success}
		if _, seen := grouped[k]; !seen {
			order = append(order, k)
		}
		grouped[k] = append(grouped[k], reqSpan{id: id, start: start, end: end})
	}
	rows.Close()
	if len(order) == 0 {
		respond(s, i, "no checks found yet")
		return
	}
	var chunks []string
	var bld strings.Builder
	dateFmt := "2006-01-02"
	sort.SliceStable(order, func(i1, j1 int) bool { return order[i1].t.After(order[j1].t) })
	if len(order) > 50 {
		order = order[:50]
	}
	for _, k := range order {
		upper := k.t.Add(5 * time.Minute)
		var batchTS time.Time
		err := b.store.DB.QueryRow(`
            SELECT coalesce(max(checked_at), ?)
            FROM campsite_state
            WHERE provider=? AND campground_id=? AND checked_at<=?
        `, k.t, k.prov, k.cg, upper).Scan(&batchTS)
		if err != nil {
			batchTS = k.t
		}
		name := k.cg
		if cg, ok, _ := b.store.GetCampgroundByID(context.Background(), k.prov, k.cg); ok {
			name = cg.Name
		}
		var errSnippet string
		if !k.ok {
			var es string
			_ = b.store.DB.QueryRow(`SELECT coalesce(err,'') FROM lookup_log WHERE provider=? AND campground_id=? AND checked_at=? LIMIT 1`, k.prov, k.cg, k.t).Scan(&es)
			if es != "" {
				if len(es) > 120 {
					es = es[:120] + "…"
				}
				errSnippet = " error: " + es
			}
		}
		status := "ok"
		if !k.ok {
			status = "fail"
		}
		header := fmt.Sprintf("%s %s %s (%s) [%s]%s", k.t.Format("2006-01-02 15:04"), k.prov, k.cg, name, status, errSnippet)
		bld.WriteString(header + "\n")
		for _, sp := range grouped[k] {
			maxDays := 10
			dates := make([]time.Time, 0, maxDays)
			for d := sp.start; !d.After(sp.end) && len(dates) < maxDays; d = d.AddDate(0, 0, 1) {
				dates = append(dates, d)
			}
			rows2, err := b.store.DB.Query(`
                SELECT date, count(DISTINCT campsite_id) AS total, sum(CASE WHEN available THEN 1 ELSE 0 END) AS free
                FROM campsite_state
                WHERE provider=? AND campground_id=? AND checked_at=? AND date BETWEEN ? AND ?
                GROUP BY date ORDER BY date
            `, k.prov, k.cg, batchTS, sp.start, sp.end)
			counts := map[string][2]int{}
			if err == nil {
				for rows2.Next() {
					var dt time.Time
					var total, free int
					if err := rows2.Scan(&dt, &total, &free); err != nil {
						continue
					}
					counts[dt.Format(dateFmt)] = [2]int{total, free}
				}
				rows2.Close()
			}
			var parts []string
			for _, d := range dates {
				key := d.Format(dateFmt)
				c := counts[key]
				parts = append(parts, fmt.Sprintf("%s %d/%d", key, c[1], c[0]))
			}
			suffix := ""
			if sp.end.Sub(sp.start).Hours()/24.0+1 > float64(len(dates)) {
				suffix = " …"
			}
			bld.WriteString(fmt.Sprintf("  req %d (%s..%s): %s%s\n", sp.id, sp.start.Format(dateFmt), sp.end.Format(dateFmt), strings.Join(parts, ", "), suffix))
		}
		if bld.Len() > 1600 {
			chunks = append(chunks, bld.String())
			bld.Reset()
		}
	}
	if bld.Len() > 0 {
		chunks = append(chunks, bld.String())
	}
	if len(chunks) == 0 {
		respond(s, i, "no data")
		return
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource}); err != nil {
		b.logger.Warn("checks defer failed", "err", err)
	}
	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Content: chunks[0]}); err != nil {
		b.logger.Warn("checks followup send failed", "err", err)
	}
	for _, c := range chunks[1:] {
		if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Content: c}); err != nil {
			b.logger.Warn("checks followup send failed", "err", err)
		}
	}
}
