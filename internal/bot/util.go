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
