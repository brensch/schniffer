package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

type SummaryData struct {
	Stats                 DetailedSummaryStats
	NotificationUsernames []string
	ActiveUsernames       []string
	TrackedCampgrounds    []string
}

// GetDetailedSummary returns a formatted summary string with comprehensive statistics
func (s *Store) GetDetailedSummary(ctx context.Context) (string, error) {
	// Get detailed stats
	stats, err := s.GetDetailedSummaryStats(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get stats: %w", err)
	}

	// Get users with notifications
	usersWithNotifications, err := s.GetUsersWithNotifications(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get users with notifications: %w", err)
	}

	// Get users with active requests
	usersWithActiveRequests, err := s.GetUsersWithActiveRequests(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get users with active requests: %w", err)
	}

	// Get tracked campgrounds
	trackedCampgrounds, err := s.GetTrackedCampgrounds(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get tracked campgrounds: %w", err)
	}

	// Build the summary message
	var summary strings.Builder
	summary.WriteString("24 Hour Schniff roundup:\n")
	summary.WriteString("Available campsites found\n")
	summary.WriteString(fmt.Sprintf("%d\n", stats.Notifications24h))
	summary.WriteString("Checks made\n")
	summary.WriteString(fmt.Sprintf("%d\n", stats.Lookups24h))
	summary.WriteString("Active Schniffs\n")
	summary.WriteString(fmt.Sprintf("%d\n", stats.ActiveRequests))

	// Schniffists who got schniffs
	summary.WriteString("Schniffists who got schniffs\n")
	if len(usersWithNotifications) == 0 {
		summary.WriteString("No bueno today.\n")
	} else {
		for _, username := range usersWithNotifications {
			summary.WriteString(fmt.Sprintf("<@%s> ", username))
		}
	}

	// Schniffists with active schniffs
	summary.WriteString("Schniffists with active schniffs\n")
	if len(usersWithActiveRequests) == 0 {
		summary.WriteString("None\n")
	} else {
		for _, username := range usersWithActiveRequests {
			summary.WriteString(fmt.Sprintf("<@%s> ", username))
		}
	}

	// Campgrounds being tracked
	summary.WriteString("Campgrounds being tracked\n")
	if len(trackedCampgrounds) == 0 {
		summary.WriteString("None\n")
	} else {
		summary.WriteString(strings.Join(trackedCampgrounds, "\n"))
	}

	return summary.String(), nil
}

// GetSummaryData returns structured summary data for creating embeds
func (s *Store) GetSummaryData(ctx context.Context) (SummaryData, error) {
	// Get detailed stats
	stats, err := s.GetDetailedSummaryStats(ctx)
	if err != nil {
		return SummaryData{}, fmt.Errorf("failed to get stats: %w", err)
	}

	// Get users with notifications
	usersWithNotifications, err := s.GetUsersWithNotifications(ctx)
	if err != nil {
		return SummaryData{}, fmt.Errorf("failed to get users with notifications: %w", err)
	}

	// Get users with active requests
	usersWithActiveRequests, err := s.GetUsersWithActiveRequests(ctx)
	if err != nil {
		return SummaryData{}, fmt.Errorf("failed to get users with active requests: %w", err)
	}

	// Get tracked campgrounds
	trackedCampgrounds, err := s.GetTrackedCampgrounds(ctx)
	if err != nil {
		return SummaryData{}, fmt.Errorf("failed to get tracked campgrounds: %w", err)
	}

	return SummaryData{
		Stats:                 stats,
		NotificationUsernames: usersWithNotifications,
		ActiveUsernames:       usersWithActiveRequests,
		TrackedCampgrounds:    trackedCampgrounds,
	}, nil
}

func MakeSummaryEmbed(summaryData SummaryData) *discordgo.MessageEmbed {

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

	return embed
}
