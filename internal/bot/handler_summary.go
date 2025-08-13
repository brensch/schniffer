package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleSummaryCommand(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	ctx := context.Background()

	// Get comprehensive summary data
	summaryData, err := b.mgr.GetSummaryData(ctx)
	if err != nil {
		respond(s, i, "Failed to get summary: "+err.Error())
		return
	}

	// Create embed
	embed := &discordgo.MessageEmbed{
		Title:     "🏕️ 24h Schniffer Roundup",
		Color:     0x5865F2, // Discord Blurple
		Timestamp: time.Now().Format(time.RFC3339),
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "🎯 Available Campsites Found",
				Value:  fmt.Sprintf("%d", summaryData.Stats.Notifications24h),
				Inline: true,
			},
			{
				Name:   "🔍 Checks Made",
				Value:  fmt.Sprintf("%d", summaryData.Stats.Lookups24h),
				Inline: true,
			},
			{
				Name:   "👃 Active Schniffs",
				Value:  fmt.Sprintf("%d", summaryData.Stats.ActiveRequests),
				Inline: true,
			},
			{
				Name: "🎉 Schniffists Who Got Schniffs",
				Value: func() string {
					if len(summaryData.NotificationUsernames) > 0 {
						return strings.Join(summaryData.NotificationUsernames, "\n")
					}
					return "*No bueno today.*"
				}(),
				Inline: false,
			},
			{
				Name: "👥 Schniffists With Active Schniffs",
				Value: func() string {
					if len(summaryData.ActiveUsernames) > 0 {
						return strings.Join(summaryData.ActiveUsernames, "\n")
					}
					return "*None*"
				}(),
				Inline: false,
			},
			{
				Name: "🏞️ Campgrounds Being Tracked",
				Value: func() string {
					if len(summaryData.TrackedCampgrounds) > 0 {
						// Limit to first 10 campgrounds to avoid hitting embed limits
						campgrounds := summaryData.TrackedCampgrounds
						if len(campgrounds) > 10 {
							campgrounds = campgrounds[:10]
						}
						value := strings.Join(campgrounds, "\n")
						if len(summaryData.TrackedCampgrounds) > 10 {
							value += fmt.Sprintf("\n*...and %d more*", len(summaryData.TrackedCampgrounds)-10)
						}
						return value
					}
					return "*None*"
				}(),
				Inline: false,
			},
		},
	}

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
