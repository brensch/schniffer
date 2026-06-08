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
// currently-typed checkin/checkout. The largest suggestion is the span between
// the two dates so users can't pick a value that's guaranteed to never fire.
// When the dates aren't both valid yet, returns a single hint instead of a
// misleading numeric range.
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
	if checkin == "" || checkout == "" {
		return []*discordgo.ApplicationCommandOptionChoice{{
			Name:  "Enter checkin and checkout first to see options",
			Value: 1,
		}}
	}
	start, end, err := parseDates(checkin, checkout)
	if err != nil {
		return []*discordgo.ApplicationCommandOptionChoice{{
			Name:  fmt.Sprintf("Use YYYY-MM-DD for checkin/checkout (got %q / %q)", checkin, checkout),
			Value: 1,
		}}
	}
	if !end.After(start) {
		return []*discordgo.ApplicationCommandOptionChoice{{
			Name:  fmt.Sprintf("checkout (%s) must be after checkin (%s)", checkout, checkin),
			Value: 1,
		}}
	}
	span := int(end.Sub(start).Hours() / 24)
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
	const layout = "2006-01-02"

	if o, ok := opts["minimum_nights"]; ok && o != nil {
		v := o.IntValue()
		if v < 1 {
			return minN, strat, fmt.Sprintf("minimum_nights must be at least 1 (got %d)", v)
		}
		if v > int64(nights) {
			return minN, strat, fmt.Sprintf(
				"minimum_nights=%d cannot exceed the %d-night window between checkin %s and checkout %s — pick a value between 1 and %d, or widen the date range",
				v, nights, start.Format(layout), end.Format(layout), nights,
			)
		}
		minN = sql.NullInt64{Int64: v, Valid: true}
	}

	if o, ok := opts["strategy"]; ok && o != nil {
		s := strings.TrimSpace(o.StringValue())
		if s != "" {
			if !db.IsValidStrategy(s) {
				return minN, strat, fmt.Sprintf(
					"unknown strategy %q — valid options: %s",
					s, strings.Join(db.ValidStrategies, ", "),
				)
			}
			if s == db.StrategyFullWeekend {
				if !windowContainsFriSat(start, end) {
					return minN, strat, fmt.Sprintf(
						"strategy=%s requires the window %s → %s to include both a Friday and the following Saturday night — none found in this range",
						s, start.Format(layout), end.Format(layout),
					)
				}
			}
			strat = sql.NullString{String: s, Valid: true}
		}
	}
	return minN, strat, ""
}

// windowContainsFriSat reports whether [start, end) covers at least one Fri+Sat
// night pair (i.e. a Friday night where the following Saturday night is also
// inside the window).
func windowContainsFriSat(start, end time.Time) bool {
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Friday {
			sat := d.AddDate(0, 0, 1)
			if sat.Before(end) {
				return true
			}
		}
	}
	return false
}
