package db

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

type SummaryData struct {
	Stats              DetailedSummaryStats
	NotificationCounts []UserCount // users who received hits in last 24h, count desc
	ActiveCounts       []UserCount // users with active schniffs, count desc
	TrackedCampgrounds []string

	// Optional: maps user_id -> display name. When nil/missing the embed
	// falls back to <@user_id> mentions, which Discord renders as the
	// display name client-side.
	UserNames map[string]string
}

// GetDetailedSummary returns a formatted summary string with comprehensive statistics
func (s *Store) GetDetailedSummary(ctx context.Context) (string, error) {
	// Get detailed stats
	stats, err := s.GetDetailedSummaryStats(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get stats: %w", err)
	}

	// Get users with notifications + counts
	notifCounts, err := s.GetUserNotificationCounts(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get notification counts: %w", err)
	}

	// Get users with active requests + counts
	activeCounts, err := s.GetUserActiveRequestCounts(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get active request counts: %w", err)
	}

	// Get tracked campgrounds
	trackedCampgrounds, err := s.GetTrackedCampgrounds(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get tracked campgrounds: %w", err)
	}

	// Build the summary message
	var summary strings.Builder
	summary.WriteString("24 Hour Schniff roundup:\n")
	summary.WriteString("Schniffs sent\n")
	summary.WriteString(fmt.Sprintf("%d\n", stats.UserDMs24h))
	summary.WriteString("Checks made\n")
	summary.WriteString(fmt.Sprintf("%d\n", stats.Lookups24h))
	summary.WriteString("Active Schniffs\n")
	summary.WriteString(fmt.Sprintf("%d\n", stats.ActiveRequests))

	// Schniffists, combined view
	summary.WriteString("Schniffists\n")
	rows := mergeSchniffistRows(activeCounts, notifCounts)
	if len(rows) == 0 {
		summary.WriteString("No schniffists yet.\n")
	} else {
		for _, r := range rows {
			summary.WriteString(fmt.Sprintf("<@%s> — %d active / %d schniffs sent\n",
				r.UserID, r.Active, r.Sent))
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

// GetSummaryData returns structured summary data for creating embeds.
// UserNames is left empty; callers that want display names instead of
// mention rendering should populate it from Discord before passing to
// MakeSummaryEmbed.
func (s *Store) GetSummaryData(ctx context.Context) (SummaryData, error) {
	stats, err := s.GetDetailedSummaryStats(ctx)
	if err != nil {
		return SummaryData{}, fmt.Errorf("failed to get stats: %w", err)
	}
	notifyCounts, err := s.GetUserNotificationCounts(ctx)
	if err != nil {
		return SummaryData{}, fmt.Errorf("failed to get notification counts: %w", err)
	}
	activeCounts, err := s.GetUserActiveRequestCounts(ctx)
	if err != nil {
		return SummaryData{}, fmt.Errorf("failed to get active request counts: %w", err)
	}
	trackedCampgrounds, err := s.GetTrackedCampgrounds(ctx)
	if err != nil {
		return SummaryData{}, fmt.Errorf("failed to get tracked campgrounds: %w", err)
	}
	return SummaryData{
		Stats:              stats,
		NotificationCounts: notifyCounts,
		ActiveCounts:       activeCounts,
		TrackedCampgrounds: trackedCampgrounds,
	}, nil
}

// userLabel renders one user as either their cached display name or, as
// a fallback, an @-mention which Discord auto-resolves client-side.
func userLabel(userID string, names map[string]string) string {
	if name, ok := names[userID]; ok && name != "" {
		return name
	}
	return fmt.Sprintf("<@%s>", userID)
}

// mergeSchniffistRows combines active + sent-schniff counts per user
// into one row each, sorted by (sent desc, active desc, userID asc).
// Users who only appear in one of the two inputs are still included.
func mergeSchniffistRows(active, sent []UserCount) []schniffistRow {
	byID := map[string]*schniffistRow{}
	order := []string{}
	get := func(id string) *schniffistRow {
		if r, ok := byID[id]; ok {
			return r
		}
		r := &schniffistRow{UserID: id}
		byID[id] = r
		order = append(order, id)
		return r
	}
	for _, uc := range active {
		get(uc.UserID).Active = uc.Count
	}
	for _, uc := range sent {
		get(uc.UserID).Sent = uc.Count
	}
	rows := make([]schniffistRow, 0, len(order))
	for _, id := range order {
		rows = append(rows, *byID[id])
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Sent != rows[j].Sent {
			return rows[i].Sent > rows[j].Sent
		}
		if rows[i].Active != rows[j].Active {
			return rows[i].Active > rows[j].Active
		}
		return rows[i].UserID < rows[j].UserID
	})
	return rows
}

type schniffistRow struct {
	UserID string
	Active int64
	// Sent is the per-user count of schniffs delivered as DMs in the
	// window. Sorted on this descending so heaviest recipients land at
	// the top of the schniffists field.
	Sent int64
}

// formatSchniffistRows renders one line per user:
//
//	<name> — <active> active / <sent> schniffs sent
func formatSchniffistRows(rows []schniffistRow, names map[string]string) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s — %d active / %d schniffs sent",
			userLabel(r.UserID, names), r.Active, r.Sent)
	}
	return b.String()
}

func MakeSummaryEmbed(summaryData SummaryData) *discordgo.MessageEmbed {
	rows := mergeSchniffistRows(summaryData.ActiveCounts, summaryData.NotificationCounts)
	schniffists := formatSchniffistRows(rows, summaryData.UserNames)
	if schniffists == "" {
		schniffists = "*No schniffists yet.*"
	}

	embed := &discordgo.MessageEmbed{
		Title:     "🏕️ 24h Schniffer Roundup",
		Color:     0x5865F2,
		Timestamp: time.Now().Format(time.RFC3339),
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "📬 Schniffs Sent",
				Value:  fmt.Sprintf("%d", summaryData.Stats.UserDMs24h),
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
				Name:   "👥 Schniffists",
				Value:  schniffists,
				Inline: false,
			},
			{
				Name: "🏞️ Campgrounds Being Tracked",
				Value: func() string {
					if len(summaryData.TrackedCampgrounds) == 0 {
						return "*None*"
					}
					campgrounds := summaryData.TrackedCampgrounds
					if len(campgrounds) > 10 {
						campgrounds = campgrounds[:10]
					}
					value := strings.Join(campgrounds, "\n")
					if len(summaryData.TrackedCampgrounds) > 10 {
						value += fmt.Sprintf("\n*...and %d more*", len(summaryData.TrackedCampgrounds)-10)
					}
					return value
				}(),
				Inline: false,
			},
		},
	}
	return embed
}
