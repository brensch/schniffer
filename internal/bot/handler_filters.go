package bot

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/bwmarrin/discordgo"
)

// strategyChoices returns the curated set of strategies surfaced in Discord.
// Keep in sync with db.ValidStrategies.
func strategyChoices() []*discordgo.ApplicationCommandOptionChoice {
	return []*discordgo.ApplicationCommandOptionChoice{
		{Name: "Full weekend (Fri + Sat nights free)", Value: db.StrategyFullWeekend},
	}
}

// autocompleteMinimumNights suggests night counts based on the user's
// currently-typed checkin/checkout. Top option is the full span, descending
// down to 1.
func autocompleteMinimumNights(opts []*discordgo.ApplicationCommandInteractionDataOption) []*discordgo.ApplicationCommandOptionChoice {
	var checkin, checkout string
	for _, o := range opts {
		switch o.Name {
		case "checkin":
			checkin = o.StringValue()
		case "checkout":
			checkout = o.StringValue()
		}
	}
	span := 0
	if checkin != "" && checkout != "" {
		start, end, err := parseDates(checkin, checkout)
		if err == nil && end.After(start) {
			span = int(end.Sub(start).Hours() / 24)
		}
	}
	if span <= 0 {
		// Fall back to a small static range so the suggestion list is never empty.
		span = 7
	}
	if span > 25 {
		span = 25 // Discord limit
	}
	out := make([]*discordgo.ApplicationCommandOptionChoice, 0, span)
	for n := span; n >= 1; n-- {
		label := fmt.Sprintf("%d night", n)
		if n != 1 {
			label += "s"
		}
		out = append(out, &discordgo.ApplicationCommandOptionChoice{
			Name:  label,
			Value: n,
		})
	}
	return out
}

// parseScheduleFilters extracts and validates the optional minimum_nights and
// strategy options against the requested window [start, end). Returns the
// stored DB values plus a user-facing error message when validation fails.
func parseScheduleFilters(opts map[string]*discordgo.ApplicationCommandInteractionDataOption, start, end time.Time) (sql.NullInt64, sql.NullString, string) {
	var minN sql.NullInt64
	var strat sql.NullString

	nights := int(end.Sub(start).Hours() / 24)

	if o, ok := opts["minimum_nights"]; ok && o != nil {
		v := o.IntValue()
		if v < 1 {
			return minN, strat, "minimum_nights must be at least 1"
		}
		if v > int64(nights) {
			return minN, strat, fmt.Sprintf("minimum_nights (%d) cannot exceed the %d night window", v, nights)
		}
		minN = sql.NullInt64{Int64: v, Valid: true}
	}

	if o, ok := opts["strategy"]; ok && o != nil {
		s := strings.TrimSpace(o.StringValue())
		if s != "" {
			if !db.IsValidStrategy(s) {
				return minN, strat, fmt.Sprintf("unknown strategy %q (valid: %s)", s, strings.Join(db.ValidStrategies, ", "))
			}
			strat = sql.NullString{String: s, Valid: true}
		}
	}
	return minN, strat, ""
}
