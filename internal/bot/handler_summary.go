package bot

import (
	"context"

	"github.com/brensch/schniffer/internal/db"
	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleSummaryCommand(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	ctx := context.Background()

	// Get comprehensive summary data
	summaryData, err := b.store.GetSummaryData(ctx)
	if err != nil {
		respond(s, i, "Failed to get summary: "+err.Error())
		return
	}

	// Create embed
	embed := db.MakeSummaryEmbed(summaryData)

	// Respond to the interaction with the embed
	err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
	if err != nil {
		respond(s, i, "Failed to send summary")
		return
	}
}
