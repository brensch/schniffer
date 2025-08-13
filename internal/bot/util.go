package bot

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

func parseDates(s1, s2 string) (time.Time, time.Time, error) {
	const layout = "2006-01-02"
	a, err := time.Parse(layout, s1)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	b, err := time.Parse(layout, s2)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return a, b, nil
}

func optMap(opts []*discordgo.ApplicationCommandInteractionDataOption) map[string]*discordgo.ApplicationCommandInteractionDataOption {
	m := map[string]*discordgo.ApplicationCommandInteractionDataOption{}
	for _, o := range opts {
		m[o.Name] = o
	}
	return m
}

func respond(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content, Flags: 1 << 6},
	})
}

// getUserID safely returns the user ID for both guild and DM interactions
func getUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// sanitizeChoiceName trims whitespace and ensures the string is between 1 and 100 characters (runes).
// If longer than 100, it truncates to 99 and appends an ellipsis so the final length is 100.
func sanitizeChoiceName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	if utf8.RuneCountInString(s) <= 100 {
		return s
	}
	runes := []rune(s)
	if len(runes) > 99 {
		runes = runes[:99]
	}
	return string(runes) + "…"
}

// bookingURL returns a provider-specific booking/search URL for a given campground and date.
// For providers where deep linking per campsite is not reliable, we link to the campground or search page.
func bookingURL(provider, campgroundID string, date time.Time) string {
	// standard YYYY-MM-DD
	d := date.UTC().Format("2006-01-02")
	switch provider {
	case "recreation_gov":
		// Recreation.gov campground-month availability page; date param uses first day shown.
		// Linking to campsite list filtered by date is tricky without campground slug; fall back to month view.
		// Example: https://www.recreation.gov/camping/campgrounds/<campgroundID>/availability?date=2025-08-12
		if campgroundID == "" {
			return ""
		}
		return "https://www.recreation.gov/camping/campgrounds/" + campgroundID + "/availability?date=" + d
	case "reservecalifornia":
		// ReserveCalifornia lacks stable deep links. Send to landing page.
		return "https://reservecalifornia.com/"
	default:
		return ""
	}
}
