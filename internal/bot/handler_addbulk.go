package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/brensch/schniffer/internal/db"
	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleAddBulkCommand(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	opts := optMap(sub.Options)
	groupResponse, ok := opts["group"]
	if !ok || groupResponse == nil {
		respond(s, i, "group selection is required")
		return
	}

	if groupResponse.StringValue() == noGroupsFound {
		respond(s, i, "bro you're not meant to click that option.")
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

const noGroupsFound = "__no_groups__"

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

	if len(choices) == 0 {
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
			Name:  "No groups found. Run `/schniff map` to create a group.",
			Value: noGroupsFound,
		})
	}

	return choices
}
