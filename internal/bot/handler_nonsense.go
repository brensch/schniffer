package bot

import (
	"github.com/brensch/schniffer/internal/nonsense"
	"github.com/bwmarrin/discordgo"
)

// handleNonsenseCommand broadcasts a silly greeting to the channel
func (b *Bot) handleNonsenseCommand(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	uid := getUserID(i)

	// Generate a random silly greeting for this user
	greeting := nonsense.RandomSillyGreeting(uid)

	// Send the greeting as an embed
	embed := &discordgo.MessageEmbed{
		Description: greeting,
		Color:       0x5865F2, // Discord blurple color
	}

	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})

	if err != nil {
		b.logger.Error("failed to send nonsense greeting", "error", err)
	}
}
