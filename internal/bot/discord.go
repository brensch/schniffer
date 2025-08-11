package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/manager"
	"github.com/bwmarrin/discordgo"
)

type Bot struct {
	s      *discordgo.Session
	g      string
	token  string
	store  *db.Store
	mgr    *manager.Manager
	logger *slog.Logger
}

func New(token, guildID string, store *db.Store, mgr *manager.Manager) *Bot {
	return &Bot{g: guildID, store: store, mgr: mgr, token: token, logger: slog.Default()}
}

func (b *Bot) Run(ctx context.Context) error {
	s, err := discordgo.New("Bot " + b.token)
	if err != nil {
		return err
	}
	b.s = s
	b.mgr.SetNotifier(b)
	s.AddHandler(b.onReady)
	s.AddHandler(b.onInteraction)
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentDirectMessages
	if err := s.Open(); err != nil {
		return err
	}
	defer s.Close()
	<-ctx.Done()
	return nil
}

// Notifier implementation
func (b *Bot) NotifyAvailability(userID string, msg string) error {
	channel, err := b.s.UserChannelCreate(userID)
	if err != nil {
		return err
	}
	_, err = b.s.ChannelMessageSend(channel.ID, msg)
	return err
}

func (b *Bot) NotifySummary(channelID string, msg string) error {
	_, err := b.s.ChannelMessageSend(channelID, msg)
	return err
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	b.logger.Info("bot ready", slog.String("user", s.State.User.Username))
	b.registerCommands()
}

func (b *Bot) registerCommands() {
	cmds := []*discordgo.ApplicationCommand{
		{
			Name:        "schniff",
			Description: "Manage campground monitors",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "add", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Add a schniff", Options: []*discordgo.ApplicationCommandOption{
					{Name: "campground", Type: discordgo.ApplicationCommandOptionString, Required: true, Description: "Select campground", Autocomplete: true},
					{Name: "checkin", Type: discordgo.ApplicationCommandOptionString, Required: true, Description: "Check-in (YYYY-MM-DD)"},
					{Name: "checkout", Type: discordgo.ApplicationCommandOptionString, Required: true, Description: "Check-out (YYYY-MM-DD)"},
				}},
				{Name: "list", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "List your active schniffs"},
				// Remove supports autocomplete of your active schniffs
				{Name: "remove", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Remove a schniff", Options: []*discordgo.ApplicationCommandOption{
					{Name: "ids", Type: discordgo.ApplicationCommandOptionInteger, Required: true, Description: "Request ID to remove", Autocomplete: true},
				}},
				{Name: "stats", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Show today stats"},
				{Name: "checks", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Show last 50 checks for your requests"},
			},
		},
	}
	appID := b.s.State.Application.ID
	for _, c := range cmds {
		_, err := b.s.ApplicationCommandCreate(appID, b.g, c)
		if err != nil {
			b.logger.Warn("command registration failed", slog.Any("err", err))
		}
	}
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Handle select menu for removal
	if i.Type == discordgo.InteractionMessageComponent {
		data := i.MessageComponentData()
		switch data.CustomID {
		case "remove_checks":
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
			// Update the message to confirm removal
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseUpdateMessage,
				Data: &discordgo.InteractionResponseData{Content: fmt.Sprintf("removed schniff %d", id), Components: []discordgo.MessageComponent{}, Flags: 1 << 6},
			})
			return
		}
	}
	// Handle autocomplete for campground
	if i.Type == discordgo.InteractionApplicationCommandAutocomplete {
		data := i.ApplicationCommandData()
		if data.Name == "schniff" && len(data.Options) > 0 {
			sub := data.Options[0]
			var focused *discordgo.ApplicationCommandInteractionDataOption
			for _, o := range sub.Options {
				if o.Focused {
					focused = o
					break
				}
			}
			if focused != nil {
				var choices []*discordgo.ApplicationCommandOptionChoice
				switch focused.Name {
				case "campground":
					query := focused.StringValue()
					choices = b.autocompleteCampgrounds(i, query)
				case "ids":
					choices = b.autocompleteRemoveIDs(i)
				}
				if choices != nil {
					if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionApplicationCommandAutocompleteResult,
						Data: &discordgo.InteractionResponseData{Choices: choices},
					}); err != nil {
						b.logger.Warn("autocomplete respond failed", slog.Any("err", err))
					}
					return
				}
			}
		}
	}
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	if i.ApplicationCommandData().Name != "schniff" {
		return
	}
	data := i.ApplicationCommandData()
	if len(data.Options) == 0 {
		return
	}
	sub := data.Options[0]
	switch sub.Name {
	case "add":
		opts := optMap(sub.Options)
		cg := opts["campground"].StringValue()
		parts := strings.SplitN(cg, "|", 2)
		if len(parts) != 2 {
			respond(s, i, "invalid campground selection")
			return
		}
		provider := parts[0]
		campID := parts[1]
		start, end, err := parseDates(opts["checkin"].StringValue(), opts["checkout"].StringValue())
		if err != nil {
			respond(s, i, "invalid dates: "+err.Error())
			return
		}
		uid := getUserID(i)
		id, err := b.store.AddRequest(context.Background(), db.SchniffRequest{UserID: uid, Provider: provider, CampgroundID: campID, Checkin: start, Checkout: end})
		if err != nil {
			respond(s, i, "error: "+err.Error())
			return
		}
		respond(s, i, fmt.Sprintf("added schniff %d", id))
	case "list":
		reqs, err := b.store.ListActiveRequests(context.Background())
		if err != nil {
			respond(s, i, "error: "+err.Error())
			return
		}
		var out string
		for _, r := range reqs {
			if r.UserID == getUserID(i) {
				nights := int(r.Checkout.Sub(r.Checkin).Hours() / 24)
				out += fmt.Sprintf("%d: %s %s %s→%s (%d nights)\n", r.ID, r.Provider, r.CampgroundID, r.Checkin.Format("2006-01-02"), r.Checkout.Format("2006-01-02"), nights)
			}
		}
		if out == "" {
			out = "no active schniffs"
		}
		respond(s, i, out)
	case "remove":
		uid := getUserID(i)
		opts := optMap(sub.Options)
		if opt, ok := opts["ids"]; ok && opt != nil {
			id := int64(opt.IntValue())
			if err := b.store.DeactivateRequest(context.Background(), id, uid); err != nil {
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
		// Filter and build options (max 25)
		options := []discordgo.SelectMenuOption{}
		count := 0
		for _, r := range reqs {
			if r.UserID != uid {
				continue
			}
			// Resolve campground display name if available
			name := r.CampgroundID
			if cg, ok, _ := b.store.GetCampgroundByID(context.Background(), r.Provider, r.CampgroundID); ok {
				if strings.TrimSpace(cg.ParentName) != "" {
					name = cg.ParentName + " - " + cg.Name
				} else {
					name = cg.Name
				}
			}
		nights := int(r.Checkout.Sub(r.Checkin).Hours() / 24)
		// Remove provider from label; show just dates and nights, name in description
		label := fmt.Sprintf("%s → %s • %d night(s)", r.Checkin.Format("2006-01-02"), r.Checkout.Format("2006-01-02"), nights)
		desc := name
		options = append(options, discordgo.SelectMenuOption{Label: label, Description: desc, Value: strconv.FormatInt(r.ID, 10)})
			count++
			if count >= 25 { // Discord limit
				break
			}
		}
		if len(options) == 0 {
			respond(s, i, "no active schniffs")
			return
		}
		// Build select menu
		selectMenu := discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.SelectMenu{
					CustomID:    "remove_checks",
					Placeholder: "Select a schniff to remove",
					Options:     options,
				},
			},
		}
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content:    "Pick a schniff to remove. You'll get a confirmation after selection.",
				Components: []discordgo.MessageComponent{selectMenu},
				Flags:      1 << 6, // ephemeral
			},
		}); err != nil {
			b.logger.Warn("remove respond failed", slog.Any("err", err))
		}
	case "stats":
		row := b.store.DB.QueryRow(`
			SELECT coalesce((SELECT count(*) FROM schniff_requests WHERE active=true),0),
			coalesce((SELECT count(*) FROM lookup_log WHERE date(checked_at)=current_date),0),
			coalesce((SELECT count(*) FROM notifications WHERE date(sent_at)=current_date),0)
		`)
		var active, lookups, notes int64
		_ = row.Scan(&active, &lookups, &notes)
		respond(s, i, fmt.Sprintf("active requests: %d\nlookups today: %d\nnotifications today: %d", active, lookups, notes))
	case "checks":
		// Build: last 50 lookup checks related to this user's requests
		userID := getUserID(i)
		// Fetch relevant provider/cg combos for user
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
		// Get last 200 lookup logs joined to requests to overfetch then de-dup to 50 checks
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
			if len(order) >= 50 { // we only need 50 distinct checks in order
				// keep collecting rows to add spans, but don't extend order more than 50
			}
		}
		rows.Close()
		if len(order) == 0 {
			respond(s, i, "no checks found yet")
			return
		}
		// Prepare output
		var chunks []string
		var bld strings.Builder
		dateFmt := "2006-01-02"
		// Ensure deterministic order by timestamp desc
		sort.SliceStable(order, func(i1, j1 int) bool { return order[i1].t.After(order[j1].t) })
		if len(order) > 50 {
			order = order[:50]
		}
		for _, k := range order {
			// Find a nearby state batch timestamp
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
				// fetch one example error for this timestamp
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
			line := header + "\n"
			bld.WriteString(line)
			// For each matching request span, compute counts by day at batchTS
			for _, sp := range grouped[k] {
				// Cap long ranges to first 10 days to avoid message overflows
				maxDays := 10
				dates := make([]time.Time, 0, maxDays)
				for d := sp.start; !d.After(sp.end) && len(dates) < maxDays; d = d.AddDate(0, 0, 1) {
					dates = append(dates, d)
				}
				// Query counts
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
							b.logger.Warn("scan counts failed", slog.Any("err", err))
							continue
						}
						counts[dt.Format(dateFmt)] = [2]int{total, free}
					}
					rows2.Close()
				}
				// Build per-day brief
				var parts []string
				for _, d := range dates {
					key := d.Format(dateFmt)
					c := counts[key]
					parts = append(parts, fmt.Sprintf("%s %d/%d", key, c[1], c[0]))
				}
				suffix := ""
				// If truncated range
				if sp.end.Sub(sp.start).Hours()/24.0+1 > float64(len(dates)) {
					suffix = " …"
				}
				bld.WriteString(fmt.Sprintf("  req %d (%s..%s): %s%s\n", sp.id, sp.start.Format(dateFmt), sp.end.Format(dateFmt), strings.Join(parts, ", "), suffix))
			}
			// Chunk if getting close to Discord's limit
			if bld.Len() > 1600 {
				chunks = append(chunks, bld.String())
				bld.Reset()
			}
		}
		if bld.Len() > 0 {
			chunks = append(chunks, bld.String())
		}
		// Send initial and follow-ups
		if len(chunks) == 0 {
			respond(s, i, "no data")
			return
		}
		// Defer then send larger content as followups to avoid 2000 char limit
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource}); err != nil {
			b.logger.Warn("checks defer failed", slog.Any("err", err))
		}
		// ephemeral is true by default with our respond helper; here we'll mark ephemeral true for followups too
		first := chunks[0]
		if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Content: first}); err != nil {
			b.logger.Warn("checks followup send failed", slog.Any("err", err))
		}
		for _, c := range chunks[1:] {
			if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Content: c}); err != nil {
				b.logger.Warn("checks followup send failed", slog.Any("err", err))
			}
		}
	}
}

func parseDates(s1, s2 string) (time.Time, time.Time, error) {
	const layout = "2006-01-02"
	a, err := time.Parse(layout, s1)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	b, err := time.Parse(layout, s2)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return a, b, nil
}

func optMap(opts []*discordgo.ApplicationCommandInteractionDataOption) map[string]*discordgo.ApplicationCommandInteractionDataOption {
	m := map[string]*discordgo.ApplicationCommandInteractionDataOption{}
	for _, o := range opts {
		m[o.Name] = o
	}
	return m
}

func respond(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content, Flags: 1 << 6},
	}); err != nil {
		// last resort
	}
}

// getUserID safely returns the user ID for both guild and DM interactions
func getUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

func (b *Bot) autocompleteCampgrounds(i *discordgo.InteractionCreate, query string) []*discordgo.ApplicationCommandOptionChoice {
	ctx := context.Background()
	cgs, err := b.store.ListCampgrounds(ctx, query)
	if err != nil {
		b.logger.Warn("list campgrounds failed", slog.Any("err", err))
		return nil
	}
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(cgs))
	for _, c := range cgs {
		display := c.Name
		if strings.TrimSpace(c.ParentName) != "" {
			display = c.ParentName + " - " + c.Name
		}
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
			Name:  display,
			Value: c.Provider + "|" + c.CampgroundID,
		})
	}
	return choices
}

// autocompleteRemoveIDs suggests the caller's active schniffs as choices.
func (b *Bot) autocompleteRemoveIDs(i *discordgo.InteractionCreate) []*discordgo.ApplicationCommandOptionChoice {
	uid := getUserID(i)
	reqs, err := b.store.ListActiveRequests(context.Background())
	if err != nil {
		b.logger.Warn("list active reqs failed", slog.Any("err", err))
		return nil
	}
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, 25)
	for _, r := range reqs {
		if r.UserID != uid {
			continue
		}
		name := r.CampgroundID
		if cg, ok, _ := b.store.GetCampgroundByID(context.Background(), r.Provider, r.CampgroundID); ok {
			if strings.TrimSpace(cg.ParentName) != "" {
				name = cg.ParentName + " - " + cg.Name
			} else {
				name = cg.Name
			}
		}
	nights := int(r.Checkout.Sub(r.Checkin).Hours() / 24)
	// Remove ID and provider from display; show dates, nights, and name
	label := fmt.Sprintf("%s→%s • %d night(s)", r.Checkin.Format("2006-01-02"), r.Checkout.Format("2006-01-02"), nights)
	val := r.ID
	display := label + " • " + name
		// Discord requires Name length 1..100
		if len(display) > 100 {
			display = display[:97] + "…"
		}
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: display, Value: val})
		if len(choices) >= 25 {
			break
		}
	}
	if len(choices) == 0 {
		// Discord requires at least one item for an autocomplete response; provide a hint.
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: "No active schniffs", Value: 0})
	}
	return choices
}
