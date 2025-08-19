package bot

import (
	"context"
	"log/slog"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/nonsense"
	"github.com/brensch/schniffer/internal/providers"
	"github.com/bwmarrin/discordgo"
)

type Bot struct {
	session          *discordgo.Session
	broadcastChannel string

	store    *db.Store
	registry *providers.Registry
	logger   *slog.Logger
	useGuild bool // use guild commands (default) vs global commands (production)
}

func New(store *db.Store, discordSession *discordgo.Session, registry *providers.Registry, broadcastChannel string, useGuild bool) *Bot {
	return &Bot{
		store:            store,
		session:          discordSession,
		broadcastChannel: broadcastChannel,
		logger:           slog.Default(),
		registry:         registry,
		useGuild:         useGuild,
	}
}

func (b *Bot) Run(ctx context.Context) error {
	b.session.AddHandler(b.onReady)
	b.session.AddHandler(b.onInteraction)
	b.session.AddHandler(b.onGuildMemberAdd)
	b.session.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentDirectMessages | discordgo.IntentsGuildMembers

	<-ctx.Done()
	return nil
}

// resolveChannelID takes a channel ID or guild ID and returns the actual channel ID to send to
func (b *Bot) resolveChannelID(channelID string) string {
	// Try to get it as a guild first
	guild, err := b.session.Guild(channelID)
	if err == nil {
		// This is a guild ID, find the first text channel
		channels, err := b.session.GuildChannels(guild.ID)
		if err == nil {
			for _, channel := range channels {
				if channel.Type == discordgo.ChannelTypeGuildText {
					return channel.ID
				}
			}
		}
	}
	// If not a guild or no text channels found, assume it's already a channel ID
	return channelID
}

func (b *Bot) Notify(channelID string, msg string) error {
	resolvedChannelID := b.resolveChannelID(channelID)
	_, err := b.session.ChannelMessageSend(resolvedChannelID, msg)
	return err
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	b.logger.Info("bot ready", slog.String("user", s.State.User.Username))
	b.registerCommands()

	// Send startup message to the broadcast channel
	if b.broadcastChannel != "" {
		resolvedChannelID := b.resolveChannelID(b.broadcastChannel)
		err := b.Notify(resolvedChannelID, "scniffbot online and ready to schniff")
		if err != nil {
			b.logger.Error("failed to send startup message", slog.Any("err", err))
		}
	}
}

func (b *Bot) onGuildMemberAdd(s *discordgo.Session, m *discordgo.GuildMemberAdd) {
	b.logger.Info("new member joined", slog.String("user", m.User.Username), slog.String("id", m.User.ID))

	// Send a DM to the new user with instructions
	dmChannel, err := s.UserChannelCreate(m.User.ID)
	if err != nil {
		b.logger.Error("failed to create DM channel", slog.Any("err", err))
		return
	} else {
		// Add detailed instructions on how to use the bot
		instructions := `

**Hello schniffist**

Congratulations! Being a schniffist is an honour.

**How to schniff**

👃 Add a schniff

⏰ Wait

🔍 I find you a campsite

📨 I send you a message, you click the link to the freed website, and then book it

Send all your commands directly to me privately (ie not in the schniffer channel).
Type /schniff to see the commands available. You can figure it out from there.

**Why can you find campsites that are free when they're all booked right now?**
People make plans, those plans change. They cancel their booking. They normally do it on sunday night for some reason. I don't know why, i'm not a human and don't do human stuff, i'm a schniffer.
`

		_, err = s.ChannelMessageSend(dmChannel.ID, instructions)
		if err != nil {
			b.logger.Error("failed to send DM to new user", slog.Any("err", err))
		} else {
			b.logger.Info("sent welcome DM to new user", slog.String("user", m.User.Username))
		}
	}

	if b.broadcastChannel == "" {
		return
	}

	// Send a brief public notification to the broadcast channel
	resolvedChannelID := b.resolveChannelID(b.broadcastChannel)

	// Generate public welcome message
	welcomeMessage := nonsense.RandomSillyGreeting(m.User.ID)

	// Create an embed with "⚠️ New schniffist alert 🐽" title
	embed := &discordgo.MessageEmbed{
		Title:       "⚠️ New schniffist alert 🐽",
		Description: welcomeMessage,
		Color:       0x5865F2, // Discord blurple color
	}

	_, err = s.ChannelMessageSendEmbed(resolvedChannelID, embed)
	if err != nil {
		b.logger.Error("failed to send public welcome message", slog.Any("err", err))
	}

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
				{Name: "group", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Add a schniff for all campgrounds in a group", Options: []*discordgo.ApplicationCommandOption{
					{Name: "group", Type: discordgo.ApplicationCommandOptionString, Required: true, Description: "Select group", Autocomplete: true},
					{Name: "checkin", Type: discordgo.ApplicationCommandOptionString, Required: true, Description: "Check-in (YYYY-MM-DD)"},
					{Name: "checkout", Type: discordgo.ApplicationCommandOptionString, Required: true, Description: "Check-out (YYYY-MM-DD)"},
				}},
				{Name: "creategroup", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Open web interface to create a new campground group"},
				{Name: "remove", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Remove a schniff. Blank id removes all.", Options: []*discordgo.ApplicationCommandOption{
					{Name: "ids", Type: discordgo.ApplicationCommandOptionInteger, Required: false, Description: "Request ID to remove", Autocomplete: true},
				}},
				{Name: "state", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Show current state for your schniffs"},
				{Name: "summary", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Get detailed schniffer summary"},
				{Name: "nonsense", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Broadcast a silly greeting to the channel"},
			},
		},
	}
	appID := b.session.State.Application.ID
	guildID := ""
	if b.useGuild {
		guildID = b.broadcastChannel
		b.logger.Info("registering commands for guild", slog.String("guild", guildID))
	} else {
		b.logger.Info("registering commands globally")
	}
	for _, c := range cmds {
		_, err := b.session.ApplicationCommandCreate(appID, guildID, c)
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
	case "group":
		choices = b.autocompleteGroups(i, focused.StringValue())
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
	case "group":
		b.handleGroupCommand(s, i, sub)
	case "creategroup":
		b.handleCreateGroupCommand(s, i, sub)
	case "remove":
		b.handleRemoveCommand(s, i, sub)
	case "state":
		b.handleStateCommand(s, i, sub)
	case "summary":
		b.handleSummaryCommand(s, i, sub)
	case "nonsense":
		b.handleNonsenseCommand(s, i, sub)
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
