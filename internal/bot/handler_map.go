package bot

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleLinkMapCommand(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	uid := getUserID(i)

	// Create the URL with the user's token and welcome parameter
	baseURL := "https://schniff.snek2.ddns.net"
	groupCreationURL := fmt.Sprintf("%s/?user=%s&welcome=true", baseURL, uid)

	// Create an embed with the link
	embed := &discordgo.MessageEmbed{
		Title:       "🗺️ View the Schniff Map 🐽",
		Description: "Schniffmap allows you to create groups of sites to monitor, or quickly see availability right now.",
		Color:       0xc47331, // Orange color matching the theme
		Footer: &discordgo.MessageEmbedFooter{
			Text: "This link is personalized for your account",
		},
	}

	// Create a button component with the URL
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label: "Open Schniffmap",
					Style: discordgo.LinkButton,
					URL:   groupCreationURL,
					Emoji: discordgo.ComponentEmoji{
						Name: "🔗",
					},
				},
			},
		},
	}

	// Send the response with embed and button
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: components,
			Flags:      discordgo.MessageFlagsEphemeral, // Only visible to the user who ran the command
		},
	})

	if err != nil {
		b.logger.Warn("failed to respond to creategroup command", "error", err)
	}
}
