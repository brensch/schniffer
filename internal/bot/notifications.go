package bot

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/bwmarrin/discordgo"
)

// CampsiteStats holds statistics for a campsite's availability
type CampsiteStats struct {
	CampsiteID    string
	DaysAvailable int
	TotalDays     int
	Dates         []time.Time
}

// NotifyAvailabilityEmbed sends a beautifully formatted embed with campsite fields
func (b *Bot) NotifyAvailabilityEmbed(userID string, provider string, campgroundID string, req db.SchniffRequest, items []db.AvailabilityItem) error {
	channel, err := b.s.UserChannelCreate(userID)
	if err != nil {
		return err
	}

	// Get campground name
	campground, found, err := b.store.GetCampgroundByID(context.Background(), provider, campgroundID)
	if err != nil {
		return fmt.Errorf("failed to get campground: %w", err)
	}
	campgroundName := campgroundID // fallback to ID if name not found
	if found {
		campgroundName = campground.Name
	}

	// Group items by campsite and calculate availability stats
	campsiteStats := b.calculateCampsiteStats(items, req.Checkin, req.Checkout)

	// Sort campsites by days available (descending)
	sort.Slice(campsiteStats, func(i, j int) bool {
		return campsiteStats[i].DaysAvailable > campsiteStats[j].DaysAvailable
	})

	// Limit to top 5 campsites to prevent Discord size issues
	if len(campsiteStats) > 5 {
		campsiteStats = campsiteStats[:5]
	}

	// Build the single embed with fields for each campsite
	embed := b.buildNotificationEmbed(campgroundName, req.Checkin, req.Checkout, userID, campsiteStats, provider, campgroundID)

	_, err = b.s.ChannelMessageSendEmbed(channel.ID, embed)
	return err
}

// calculateCampsiteStats groups availability items by campsite and calculates stats
func (b *Bot) calculateCampsiteStats(items []db.AvailabilityItem, checkin, checkout time.Time) []CampsiteStats {
	// Group by campsite
	byCampsite := make(map[string][]time.Time)
	for _, item := range items {
		byCampsite[item.CampsiteID] = append(byCampsite[item.CampsiteID], item.Date)
	}

	// Calculate total days in the requested range
	totalDays := int(checkout.Sub(checkin).Hours() / 24)

	// Build stats for each campsite
	var stats []CampsiteStats
	for campsiteID, dates := range byCampsite {
		// Sort dates
		sort.Slice(dates, func(i, j int) bool {
			return dates[i].Before(dates[j])
		})

		stats = append(stats, CampsiteStats{
			CampsiteID:    campsiteID,
			DaysAvailable: len(dates),
			TotalDays:     totalDays,
			Dates:         dates,
		})
	}

	return stats
}

// buildNotificationEmbed creates a single embed with fields for each campsite
func (b *Bot) buildNotificationEmbed(campgroundName string, checkin, checkout time.Time, userID string, campsiteStats []CampsiteStats, provider, campgroundID string) *discordgo.MessageEmbed {
	// Main description with header info
	var description strings.Builder
	description.WriteString(campgroundName + "\n")
	description.WriteString(checkin.Format("2006-01-02") + " to " + checkout.Format("2006-01-02") + "\n")
	description.WriteString(fmt.Sprintf("<@%s>, I just schniffed some available campsites for you.\n\n", userID))

	// Summary stats
	if len(campsiteStats) == 1 {
		description.WriteString("Showing the top 1 campsite by days available.\n")
		description.WriteString("1 total campsite with availabilities.")
	} else {
		description.WriteString(fmt.Sprintf("Showing the top %d campsites by days available.\n", len(campsiteStats)))
		description.WriteString(fmt.Sprintf("%d total campsites with availabilities.", len(campsiteStats)))
	}

	embed := &discordgo.MessageEmbed{
		Title:       "Look what the schniffer dragged in!",
		Description: description.String(),
		Color:       0x00ff00, // Green color
		Timestamp:   time.Now().Format(time.RFC3339),
		Fields:      []*discordgo.MessageEmbedField{},
	}

	// Add a field for each campsite
	for _, stats := range campsiteStats {
		// Build field content
		var fieldValue strings.Builder

		// Make the availability count the clickable link
		availabilityText := fmt.Sprintf("%d of %d days available", stats.DaysAvailable, stats.TotalDays)
		if url := b.mgr.CampsiteURL(provider, campgroundID, stats.CampsiteID); url != "" {
			availabilityText = fmt.Sprintf("[%s](%s)", availabilityText, url)
		}
		fieldValue.WriteString(availabilityText + "\n")

		// List available dates (limit to prevent field size issues)
		maxDates := 10
		if len(stats.Dates) > maxDates {
			fieldValue.WriteString(fmt.Sprintf("First %d of %d dates:\n", maxDates, len(stats.Dates)))
		}

		for j, date := range stats.Dates {
			if j >= maxDates {
				fieldValue.WriteString(fmt.Sprintf("... and %d more", len(stats.Dates)-maxDates))
				break
			}
			dayName := date.Format("Monday")
			dateStr := date.Format("2006-01-02")
			fieldValue.WriteString(fmt.Sprintf("%s (%s)\n", dayName, dateStr))
		}

		// Create field name without link (headers can't be clickable)
		fieldName := fmt.Sprintf("Campsite %s", stats.CampsiteID)

		// Add the field to the embed
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   fieldName,
			Value:  fieldValue.String(),
			Inline: false, // Each campsite gets its own full-width section
		})
	}

	// Add Remember section as the final field
	rememberValue := "• Act fast to get these sites - typically gone within 5 minutes\n" +
		"• Links take you to booking pages\n" +
		"• Find the availability and click to book\n" +
		"• If no availability when you click, you were too slow\n" +
		"• I don't make mistakes (added 'no mistakes' to chatgpt prompt)\n" +
		"• Mobile app may open to last page despite link - double check"

	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name:   "Remember",
		Value:  rememberValue,
		Inline: false,
	})

	return embed
}
