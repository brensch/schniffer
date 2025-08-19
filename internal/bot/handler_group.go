package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/brensch/schniffer/internal/db"
	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleGroupCommand(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	opts := optMap(sub.Options)
	groupResponse, ok := opts["group"]
	if !ok || groupResponse == nil {
		respond(s, i, "group selection is required")
		return
	}

	checkinResponse, ok := opts["checkin"]
	if !ok || checkinResponse == nil {
		respond(s, i, "check-in date is required")
		return
	}

	checkoutResponse, ok := opts["checkout"]
	if !ok || checkoutResponse == nil {
		respond(s, i, "check-out date is required")
		return
	}

	// Parse group ID from the selection
	groupIDAndName := groupResponse.StringValue()
	parts := strings.SplitN(groupIDAndName, "||", 2)
	if len(parts) != 2 {
		respond(s, i, "invalid group selection")
		return
	}

	groupIDStr := parts[0]
	groupName := parts[1]

	groupID, err := strconv.ParseInt(groupIDStr, 10, 64)
	if err != nil {
		respond(s, i, "invalid group ID")
		return
	}

	// Parse dates
	start, end, err := parseDates(opts["checkin"].StringValue(), opts["checkout"].StringValue())
	if err != nil {
		respond(s, i, "invalid dates: "+err.Error())
		return
	}

	if !start.Before(end) {
		respond(s, i, "checkin must be before checkout")
		return
	}

	uid := getUserID(i)

	// Get the group and verify ownership
	group, err := b.store.GetGroup(context.Background(), groupID, uid)
	if err != nil {
		respond(s, i, "error getting group: "+err.Error())
		return
	}

	// Create schniff requests for all campgrounds in the group
	var successCount int
	var errors []string

	for _, campgroundRef := range group.Campgrounds {
		_, err := b.store.AddRequest(context.Background(), db.SchniffRequest{
			UserID:       uid,
			Provider:     campgroundRef.Provider,
			CampgroundID: campgroundRef.CampgroundID,
			Checkin:      start,
			Checkout:     end,
		})

		if err != nil {
			errors = append(errors, fmt.Sprintf("Failed to add %s/%s: %s", campgroundRef.Provider, campgroundRef.CampgroundID, err.Error()))
		} else {
			successCount++
		}
	}

	// Calculate stay duration
	stayDuration := end.Sub(start)

	// Build response message
	responseMsg := fmt.Sprintf("Added schniffs for group '%s' (%d/%d campgrounds), dates %s to %s (%.0f nights)",
		groupName, successCount, len(group.Campgrounds),
		start.Format("2006-01-02"), end.Format("2006-01-02"),
		stayDuration.Hours()/24)

	if len(errors) > 0 {
		responseMsg += "\n\nErrors:\n" + strings.Join(errors, "\n")
	}

	respond(s, i, responseMsg)
}

func (b *Bot) handleCreateGroupCommand(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	uid := getUserID(i)

	// Create the URL with the user's token and welcome parameter
	baseURL := "https://schniff.snek2.ddns.net"
	groupCreationURL := fmt.Sprintf("%s/?user=%s&welcome=true", baseURL, uid)

	// Create an embed with the link
	embed := &discordgo.MessageEmbed{
		Title:       "🐽🐽🐽 Create Schniff Group",
		Description: "A schniffgroup allows you to schniff a group. Click the link below to create a group of campgrounds to monitor at once.",
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
					Label: "Open Schniffgroupomatic9000",
					Style: discordgo.LinkButton,
					URL:   groupCreationURL,
					Emoji: discordgo.ComponentEmoji{
						Name: "🗺️",
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

func (b *Bot) autocompleteGroups(i *discordgo.InteractionCreate, query string) []*discordgo.ApplicationCommandOptionChoice {
	uid := getUserID(i)

	groups, err := b.store.GetUserGroups(context.Background(), uid)
	if err != nil {
		b.logger.Warn("failed to get user groups for autocomplete", "error", err)
		return nil
	}

	var choices []*discordgo.ApplicationCommandOptionChoice
	query = strings.ToLower(query)

	for _, group := range groups {
		// Filter groups by query if provided
		if query != "" && !strings.Contains(strings.ToLower(group.Name), query) {
			continue
		}

		// Format: "groupID||groupName"
		value := fmt.Sprintf("%d||%s", group.ID, group.Name)
		name := fmt.Sprintf("%s (%d campgrounds)", group.Name, len(group.Campgrounds))

		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
			Name:  name,
			Value: value,
		})

		// Discord has a limit of 25 choices
		if len(choices) >= 25 {
			break
		}
	}

	return choices
}
