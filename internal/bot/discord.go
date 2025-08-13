package bot

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/manager"
	"github.com/bwmarrin/discordgo"
)

type Bot struct {
	s        *discordgo.Session
	g        string
	token    string
	store    *db.Store
	mgr      *manager.Manager
	logger   *slog.Logger
	useGuild bool // use guild commands (default) vs global commands (production)
}

func New(token, guildID string, store *db.Store, mgr *manager.Manager, useGuild bool) *Bot {
	return &Bot{g: guildID, store: store, mgr: mgr, token: token, logger: slog.Default(), useGuild: useGuild}
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

// ResolveUsernames converts user IDs to usernames, falling back to user ID if resolution fails
func (b *Bot) ResolveUsernames(userIDs []string) []string {
	usernames := make([]string, len(userIDs))
	for i, userID := range userIDs {
		if user, err := b.s.User(userID); err == nil {
			usernames[i] = user.Username
		} else {
			// Fallback to user ID if we can't resolve the username
			usernames[i] = userID
		}
	}
	return usernames
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

// NotifyAvailabilityEmbed sends an embed with first 10 items, each as a line with date, site, and link.
func (b *Bot) NotifyAvailabilityEmbed(userID string, provider string, campgroundID string, items []db.AvailabilityItem) error {
	channel, err := b.s.UserChannelCreate(userID)
	if err != nil {
		return err
	}
	// Sort by date ascending and cap 10
	if len(items) > 1 {
		sort.Slice(items, func(i, j int) bool { return items[i].Date.Before(items[j].Date) })
	}
	if len(items) > 10 {
		items = items[:10]
	}
	weekday := func(t time.Time) string { return t.Format("Mon") }
	desc := strings.Builder{}
	for _, it := range items {
		// Format: - 2025-08-12 (Tue) site [12345](link)
		dateStr := it.Date.Format("2006-01-02") + " (" + weekday(it.Date) + ")"
		url := b.mgr.CampsiteURL(provider, campgroundID, it.CampsiteID)
		siteLabel := it.CampsiteID
		if url != "" {
			siteLabel = "[" + siteLabel + "](" + url + ")"
		}
		desc.WriteString("- ")
		desc.WriteString(dateStr)
		desc.WriteString(" site ")
		desc.WriteString(siteLabel)
		desc.WriteString("\n")
	}
	// Title should be the campground name if we have it
	title := "Available"
	if cg, ok, _ := b.store.GetCampgroundByID(context.Background(), provider, campgroundID); ok {
		title = "Available: " + cg.Name
	} else {
		title = "Available: " + campgroundID
	}
	embed := &discordgo.MessageEmbed{Title: title, Description: desc.String(), Timestamp: time.Now().Format(time.RFC3339)}
	_, err = b.s.ChannelMessageSendEmbed(channel.ID, embed)
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
				{Name: "remove", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Remove a schniff", Options: []*discordgo.ApplicationCommandOption{
					{Name: "ids", Type: discordgo.ApplicationCommandOptionInteger, Required: true, Description: "Request ID to remove", Autocomplete: true},
				}},
				{Name: "state", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Show current state for your schniffs"},
				{Name: "summary", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Get detailed schniffer summary"},
			},
		},
	}
	appID := b.s.State.Application.ID
	guildID := ""
	if b.useGuild {
		guildID = b.g
		b.logger.Info("registering commands for guild", slog.String("guild", b.g))
	} else {
		b.logger.Info("registering commands globally")
	}
	for _, c := range cmds {
		_, err := b.s.ApplicationCommandCreate(appID, guildID, c)
		if err != nil {
			b.logger.Warn("command registration failed", slog.Any("err", err))
		}
	}
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		b.handleMessageComponent(s, i)
		return
	case discordgo.InteractionApplicationCommandAutocomplete:
		b.handleAutocomplete(s, i)
		return
	case discordgo.InteractionApplicationCommand:
		b.handleApplicationCommand(s, i)
		return
	default:
		return
	}
}

// handleMessageComponent routes UI component interactions (kept flat with early returns)
func (b *Bot) handleMessageComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	if data.CustomID == "remove_checks" {
		b.handleRemoveChecksComponent(s, i, data)
		return
	}
}

// handleAutocomplete serves autocomplete choices with minimal nesting
func (b *Bot) handleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	if data.Name != "schniff" || len(data.Options) == 0 {
		return
	}
	sub := data.Options[0]
	focused := findFocusedOption(sub.Options)
	if focused == nil {
		return
	}
	var choices []*discordgo.ApplicationCommandOptionChoice
	switch focused.Name {
	case "campground":
		choices = b.autocompleteCampgrounds(i, focused.StringValue())
	case "ids":
		choices = b.autocompleteRemoveIDs(i)
	}
	if choices == nil {
		return
	}
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{
			Choices: choices,
		},
	})
	if err != nil {
		b.logger.Warn("autocomplete respond failed", slog.Any("err", err))
	}
}

// handleApplicationCommand dispatches schniff subcommands without nested conditionals
func (b *Bot) handleApplicationCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
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
		b.handleAddCommand(s, i, sub)
	case "remove":
		b.handleRemoveCommand(s, i, sub)
	case "state":
		b.handleStateCommand(s, i, sub)
	case "summary":
		b.handleSummaryCommand(s, i, sub)
	}
}

// findFocusedOption returns the focused option (if any) from a list
func findFocusedOption(opts []*discordgo.ApplicationCommandInteractionDataOption) *discordgo.ApplicationCommandInteractionDataOption {
	for _, o := range opts {
		if o.Focused {
			return o
		}
	}
	return nil
}
