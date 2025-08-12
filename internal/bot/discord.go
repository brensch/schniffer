package bot

import (
	"context"
	"log/slog"

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
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	}); err != nil {
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
	case "list":
		b.handleListCommand(s, i, sub)
	case "remove":
		b.handleRemoveCommand(s, i, sub)
	case "stats":
		b.handleStatsCommand(s, i, sub)
	case "checks":
		b.handleChecksCommand(s, i, sub)
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
