package bot

import (
	"context"
	"crypto/md5"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// colorFromName maps a name to a deterministic, bright-on-dark embed color.
// Same input always produces the same color; useful for grouping items
// (e.g. all campgrounds in one park share a color).
func colorFromName(s string) int {
	h := md5.Sum([]byte(s))
	hue := float64(uint16(h[0])|uint16(h[1])<<8) / 65535.0
	r, g, b := hslToRGB(hue, 0.55, 0.70)
	return r<<16 | g<<8 | b
}

// hslToRGB converts HSL (all in [0,1]) to 0-255 RGB ints, matching Python's
// colorsys.hls_to_rgb (note: Python's order is H, L, S).
func hslToRGB(h, l, s float64) (int, int, int) {
	if s == 0 {
		v := int(l * 255)
		return v, v, v
	}
	var m2 float64
	if l <= 0.5 {
		m2 = l * (1 + s)
	} else {
		m2 = l + s - l*s
	}
	m1 := 2*l - m2
	c := func(x float64) int {
		if x < 0 {
			x += 1
		}
		if x > 1 {
			x -= 1
		}
		switch {
		case x*6 < 1:
			return int((m1 + (m2-m1)*6*x) * 255)
		case x*2 < 1:
			return int(m2 * 255)
		case x*3 < 2:
			return int((m1 + (m2-m1)*(2.0/3.0-x)*6) * 255)
		default:
			return int(m1 * 255)
		}
	}
	return c(h + 1.0/3.0), c(h), c(h - 1.0/3.0)
}

// parkOf returns the parent-park prefix of a campground name
// ("Yosemite National Park: Upper Pines" -> "Yosemite National Park"),
// falling back to the whole name when there is no ": " separator.
func parkOf(name string) string {
	if i := strings.Index(name, ":"); i >= 0 {
		return strings.TrimSpace(name[:i])
	}
	return strings.TrimSpace(name)
}

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

const outputMaxLength = 100

func sanitizeGenericText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "-"
	}
	if utf8.RuneCountInString(text) > outputMaxLength {
		runes := []rune(text)
		text = string(runes[:outputMaxLength])
	}
	return text
}

// sanitizeChoiceName makes the choice name safe for Discord display.
// It truncates the name to as many characters are left out of 100 after the trailing info is added.
func sanitizeChoiceName(name, provider string, rating float64) string {
	trailer := fmt.Sprintf(" [%s] %.3f/5", provider, rating)
	nameMinusEnding := outputMaxLength - len(trailer)
	name = strings.TrimSpace(name)
	if name == "" {
		return "-"
	}
	if utf8.RuneCountInString(name) <= nameMinusEnding {
		return name + trailer
	}
	runes := []rune(name)
	if len(runes) > nameMinusEnding {
		runes = runes[:nameMinusEnding-3]
	}
	return string(runes) + "…" + trailer
}

// sanitizeChoiceValue ensures the choice value is at most 100 characters (bytes).
// Discord's API limit is 100 characters for choice values.
func sanitizeChoiceValue(s string) string {
	if len(s) <= outputMaxLength {
		return s
	}
	// Truncate to 100 bytes - this is safer than counting runes since
	// Discord's limit is likely in terms of bytes/characters, not Unicode runes
	return s[:outputMaxLength]
}

// formatCampgroundWithLink returns a formatted campground name with a link if available.
// If a campground URL can be generated, it creates a markdown link format.
func (b *Bot) formatCampgroundWithLink(ctx context.Context, provider, campgroundID, fallbackName string) string {
	// Try to get campground info for better name
	name := fallbackName
	cg, ok, err := b.store.GetCampgroundByID(ctx, provider, campgroundID)
	if err != nil {
		return name
	}
	if ok {
		name = cg.Name
	}

	providerInterface, ok := b.registry.Get(cg.Provider)
	if !ok {
		return name
	}

	// Get campground URL
	url := providerInterface.CampgroundURL(cg.ID)
	if url != "" {
		return fmt.Sprintf("[%s](%s)", name, url)
	}

	return name
}
