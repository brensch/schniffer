package bot

import (
	"log/slog"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/nonsense"
	"github.com/brensch/schniffer/internal/providers"
	"github.com/bwmarrin/discordgo"
)

type Bot struct {
	session          *discordgo.Session
	guildID          string
	broadcastChannel string

	store    *db.Store
	registry *providers.Registry
	logger   *slog.Logger
	useGuild bool // use guild commands (default) vs global commands (production)
}

func New(store *db.Store, discordSession *discordgo.Session, registry *providers.Registry, guildID string, useGuild bool) (*Bot, error) {
	broadcastChannel, err := GuildIDToChannelID(discordSession, guildID)
	if err != nil {
		slog.Error("failed to resolve broadcast channel", slog.Any("err", err))
		return nil, err
	}
	return &Bot{
		store:            store,
		session:          discordSession,
		guildID:          guildID,
		broadcastChannel: broadcastChannel,
		logger:           slog.Default(),
		registry:         registry,
		useGuild:         useGuild,
	}, nil
}

func (b *Bot) MountHandlers() error {
	b.session.AddHandler(b.onReady)
	b.session.AddHandler(b.onInteraction)
	b.session.AddHandler(b.onGuildMemberAdd)
	return nil
}

func GuildIDToChannelID(session *discordgo.Session, guildID string) (string, error) {
	channels, err := session.GuildChannels(guildID)
	if err != nil {
		return "", err
	}

	// Find the first text channel in the guild
	for _, channel := range channels {
		if channel.Type == discordgo.ChannelTypeGuildText {
			return channel.ID, nil
		}
	}
	return "", nil
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	b.logger.Info("bot ready", slog.String("user", s.State.User.Username))
	// Uncomment the next line if you want to clear all commands before registering new ones
	// b.clearAllCommands()
	b.registerCommands()
	b.session.ChannelMessageSend(b.broadcastChannel, nonsense.RandomLaunchMessage())
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

	// Generate public welcome message
	welcomeMessage := nonsense.RandomSillyGreeting(m.User.ID)

	// Create an embed with "⚠️ New schniffist alert 🐽" title
	embed := &discordgo.MessageEmbed{
		Title:       "⚠️ New schniffist alert 🐽",
		Description: welcomeMessage,
		Color:       0x5865F2, // Discord blurple color
	}

	_, err = s.ChannelMessageSendEmbed(b.broadcastChannel, embed)
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
				{Name: "add-bulk", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Add a schniff for all campgrounds in a group. Use `/schniff map` to make groups.", Options: []*discordgo.ApplicationCommandOption{
					{Name: "group", Type: discordgo.ApplicationCommandOptionString, Required: true, Description: "Select group", Autocomplete: true},
					{Name: "checkin", Type: discordgo.ApplicationCommandOptionString, Required: true, Description: "Check-in (YYYY-MM-DD)"},
					{Name: "checkout", Type: discordgo.ApplicationCommandOptionString, Required: true, Description: "Check-out (YYYY-MM-DD)"},
				}},
				{Name: "map", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Open map to create groups or quickly see availability at a site."},
				{Name: "remove", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Remove a schniff. Blank id removes all.", Options: []*discordgo.ApplicationCommandOption{
					{Name: "ids", Type: discordgo.ApplicationCommandOptionInteger, Required: false, Description: "Request ID to remove", Autocomplete: true},
				}},
				{Name: "list", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "List all your active schniffs"},
				{Name: "summary", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Get summary of schniff activity for all users"},
				// {Name: "nonsense", Type: discordgo.ApplicationCommandOptionSubCommand, Description: "Broadcast a silly greeting to the channel"},
			},
		},
	}
	appID := b.session.State.Application.ID
	guildID := ""
	if b.useGuild {
		guildID = b.guildID
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
	case "add-bulk":
		b.handleAddBulkCommand(s, i, sub)
	case "map":
		b.handleLinkMapCommand(s, i, sub)
	case "remove":
		b.handleRemoveCommand(s, i, sub)
	case "list":
		b.handleListCommand(s, i, sub)
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
