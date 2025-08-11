package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/manager"
	"github.com/bwmarrin/discordgo"
)

type Bot struct {
	s     *discordgo.Session
	g     string
	token string
	store *db.Store
	mgr   *manager.Manager
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
					{Name: "start_date", Type: discordgo.ApplicationCommandOptionString, Required: true, Description: "YYYY-MM-DD"},
					{Name: "end_date", Type: discordgo.ApplicationCommandOptionString, Required: true, Description: "YYYY-MM-DD"},
				}},
				{Name: "list", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "List your active schniffs"},
				{Name: "remove", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Remove a schniff", Options: []*discordgo.ApplicationCommandOption{
					{Name: "id", Type: discordgo.ApplicationCommandOptionInteger, Required: true, Description: "Request ID"},
				}},
				{Name: "stats", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Show today stats"},
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
			if focused != nil && focused.Name == "campground" {
				query := focused.StringValue()
				choices := b.autocompleteCampgrounds(i, query)
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionApplicationCommandAutocompleteResult,
					Data: &discordgo.InteractionResponseData{Choices: choices},
				})
				return
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
		start, end, err := parseDates(opts["start_date"].StringValue(), opts["end_date"].StringValue())
		if err != nil {
			respond(s, i, "invalid dates: "+err.Error())
			return
		}
		id, err := b.store.AddRequest(context.Background(), db.SchniffRequest{UserID: i.Member.User.ID, Provider: provider, CampgroundID: campID, StartDate: start, EndDate: end})
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
			if r.UserID == i.Member.User.ID {
				out += fmt.Sprintf("%d: %s %s %s..%s\n", r.ID, r.Provider, r.CampgroundID, r.StartDate.Format("2006-01-02"), r.EndDate.Format("2006-01-02"))
			}
		}
		if out == "" {
			out = "no active schniffs"
		}
		respond(s, i, out)
	case "remove":
		opts := optMap(sub.Options)
		id := int64(opts["id"].IntValue())
		if err := b.store.DeactivateRequest(context.Background(), id, i.Member.User.ID); err != nil {
			respond(s, i, "error: "+err.Error())
			return
		}
		respond(s, i, "removed")
	case "stats":
		row := b.store.DB.QueryRow(`
			SELECT coalesce((SELECT count(*) FROM schniff_requests WHERE active=true),0),
			coalesce((SELECT count(*) FROM lookup_log WHERE date(checked_at)=current_date),0),
			coalesce((SELECT count(*) FROM notifications WHERE date(sent_at)=current_date),0)
		`)
		var active, lookups, notes int64
		_ = row.Scan(&active, &lookups, &notes)
		respond(s, i, fmt.Sprintf("active requests: %d\nlookups today: %d\nnotifications today: %d", active, lookups, notes))
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
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content, Flags: 1 << 6},
	})
}

func (b *Bot) autocompleteCampgrounds(i *discordgo.InteractionCreate, query string) []*discordgo.ApplicationCommandOptionChoice {
	ctx := context.Background()
	cgs, err := b.store.ListCampgrounds(ctx, query)
	if err != nil {
		return nil
	}
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(cgs))
	for _, c := range cgs {
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
			Name:  c.Name,
			Value: c.Provider + "|" + c.CampgroundID,
		})
	}
	return choices
}
