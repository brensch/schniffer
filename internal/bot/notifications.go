package bot

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/nonsense"
	"github.com/bwmarrin/discordgo"
)

// CampsiteStats holds statistics for a campsite's availability with enhanced details
type CampsiteStats struct {
	CampsiteID    string
	DaysAvailable int
	TotalDays     int
	Dates         []time.Time
	Details       db.CampsiteDetails // Enhanced details from database
}

// NotifyAvailabilityEmbed sends a beautifully formatted embed with campsite fields
func (b *Bot) NotifyAvailabilityEmbed(userID string, provider string, campgroundID string, req db.SchniffRequest, items []db.AvailabilityItem, newlyAvailable []db.AvailabilityItem, newlyBooked []db.AvailabilityItem) error {
	channel, err := b.s.UserChannelCreate(userID)
	if err != nil {
		return err
	}

	// Get campground info (we'll use the helper function in buildNotificationEmbed)
	// No need to get the name here since the helper function will handle it

	// Group items by campsite and calculate availability stats with enhanced details
	campsiteStats := b.calculateCampsiteStats(items, req.Checkin, req.Checkout, provider, campgroundID)

	// Sort campsites by days available (descending)
	sort.Slice(campsiteStats, func(i, j int) bool {
		return campsiteStats[i].DaysAvailable > campsiteStats[j].DaysAvailable
	})

	// Limit to top 5 campsites to prevent Discord size issues
	if len(campsiteStats) > 5 {
		campsiteStats = campsiteStats[:5]
	}

	// Build the single embed with fields for each campsite
	embed := b.buildNotificationEmbed(req.Checkin, req.Checkout, userID, campsiteStats, provider, campgroundID, newlyAvailable, newlyBooked)

	_, err = b.s.ChannelMessageSendEmbed(channel.ID, embed)
	return err
}

// calculateCampsiteStats groups availability items by campsite and calculates stats with enhanced details
func (b *Bot) calculateCampsiteStats(items []db.AvailabilityItem, checkin, checkout time.Time, provider, campgroundID string) []CampsiteStats {
	// Group by campsite
	byCampsite := make(map[string][]time.Time)
	for _, item := range items {
		byCampsite[item.CampsiteID] = append(byCampsite[item.CampsiteID], item.Date)
	}

	// Calculate total days in the requested range
	totalDays := int(checkout.Sub(checkin).Hours() / 24)

	// Get all campsite IDs for batch lookup
	var campsiteIDs []string
	for campsiteID := range byCampsite {
		campsiteIDs = append(campsiteIDs, campsiteID)
	}

	// Get enhanced details for all campsites in batch (with error handling)
	campsiteDetails, err := b.store.GetCampsiteDetailsBatch(context.Background(), provider, campgroundID, campsiteIDs)
	if err != nil {
		// Log error but continue with basic info
		campsiteDetails = make(map[string]db.CampsiteDetails)
		for _, id := range campsiteIDs {
			campsiteDetails[id] = db.CampsiteDetails{
				CampsiteID: id,
				Equipment:  []string{},
			}
		}
	}

	// Build stats for each campsite
	var stats []CampsiteStats
	for campsiteID, dates := range byCampsite {
		// Sort dates
		sort.Slice(dates, func(i, j int) bool {
			return dates[i].Before(dates[j])
		})

		// Get details, fallback to basic info if not found
		details, exists := campsiteDetails[campsiteID]
		if !exists {
			details = db.CampsiteDetails{
				CampsiteID: campsiteID,
				Equipment:  []string{},
			}
		}

		stats = append(stats, CampsiteStats{
			CampsiteID:    campsiteID,
			DaysAvailable: len(dates),
			TotalDays:     totalDays,
			Dates:         dates,
			Details:       details,
		})
	}

	return stats
}

// buildNotificationEmbed creates a single embed with fields for each campsite
func (b *Bot) buildNotificationEmbed(checkin, checkout time.Time, userID string, campsiteStats []CampsiteStats, provider, campgroundID string, newlyAvailable []db.AvailabilityItem, newlyBooked []db.AvailabilityItem) *discordgo.MessageEmbed {
	// Format campground name with link
	campgroundNameWithLink := b.formatCampgroundWithLink(context.Background(), provider, campgroundID, campgroundID)

	// Main description with header info
	var description strings.Builder
	description.WriteString(campgroundNameWithLink + "\n")
	description.WriteString(checkin.Format("2006-01-02") + " to " + checkout.Format("2006-01-02") + "\n")
	description.WriteString(fmt.Sprintf("<@%s>, I just schniffed some available campsites for you.\n\n", userID))

	// Summary stats
	if len(campsiteStats) == 1 {
		description.WriteString("Showing the top 1 campsite by days available.\n")
		description.WriteString("1 total campsite with availabilities.")
	} else if len(campsiteStats) == 0 {
		description.WriteString("No campsites currently available.\n")
	} else {
		description.WriteString(fmt.Sprintf("Showing the top %d campsites by days available.\n", len(campsiteStats)))
		description.WriteString(fmt.Sprintf("%d total campsites with availabilities.", len(campsiteStats)))
	}

	// If there are state changes, add a short summary line
	if len(newlyAvailable) > 0 || len(newlyBooked) > 0 {
		description.WriteString("\nChanges since last notification:\n")
		if len(newlyAvailable) > 0 {
			description.WriteString(fmt.Sprintf("%d newly available\n", len(newlyAvailable)))
		}
		if len(newlyBooked) > 0 {
			description.WriteString(fmt.Sprintf("%d newly booked\n", len(newlyBooked)))
		}
	}

	embed := &discordgo.MessageEmbed{
		Title:       nonsense.RandomSillyHeader(),
		Description: description.String(),
		Color:       0x00ff00, // Green color
		Timestamp:   time.Now().Format(time.RFC3339),
		Fields:      []*discordgo.MessageEmbedField{},
	}

	// Add a field for each campsite
	for _, stats := range campsiteStats {
		// Build field content
		var fieldValue strings.Builder

		// Add campsite details first if available
		if stats.Details.Name != "" {
			fieldValue.WriteString(fmt.Sprintf("**%s**\n", stats.Details.Name))
		}
		if stats.Details.Type != "" {
			fieldValue.WriteString(fmt.Sprintf("Type: %s\n", stats.Details.Type))
		}
		if stats.Details.CostPerNight > 0 {
			fieldValue.WriteString(fmt.Sprintf("Cost: $%.2f/night\n", stats.Details.CostPerNight))
		}
		if stats.Details.Rating > 0 {
			fieldValue.WriteString(fmt.Sprintf("Rating: ⭐ %.1f\n", stats.Details.Rating))
		}
		if len(stats.Details.Equipment) > 0 {
			// Limit equipment list to prevent field overflow
			equipment := stats.Details.Equipment
			if len(equipment) > 5 {
				equipment = equipment[:5]
				fieldValue.WriteString(fmt.Sprintf("Equipment: %s, +%d more\n", strings.Join(equipment, ", "), len(stats.Details.Equipment)-5))
			} else {
				fieldValue.WriteString(fmt.Sprintf("Equipment: %s\n", strings.Join(equipment, ", ")))
			}
		}
		fieldValue.WriteString("\n")

		// Make the availability count the clickable link
		availabilityText := fmt.Sprintf("%d of %d days available", stats.DaysAvailable, stats.TotalDays)
		if url := b.mgr.CampsiteURL(provider, campgroundID, stats.CampsiteID); url != "" {
			availabilityText = fmt.Sprintf("[%s](%s)", availabilityText, url)
		}
		fieldValue.WriteString(availabilityText + "\n")

		// List available dates (limit to prevent field size issues)
		maxDates := 8 // Reduced from 10 to account for additional info
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

			// Check if this date/campsite has a state change marker
			marker := ""
			for _, newAvail := range newlyAvailable {
				if newAvail.CampsiteID == stats.CampsiteID && newAvail.Date.Format("2006-01-02") == dateStr {
					marker = " (new)"
					break
				}
			}
			for _, newBooked := range newlyBooked {
				if newBooked.CampsiteID == stats.CampsiteID && newBooked.Date.Format("2006-01-02") == dateStr {
					marker = " (missed it!)"
					break
				}
			}

			fieldValue.WriteString(fmt.Sprintf("%s (%s)%s\n", dayName, dateStr, marker))
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
