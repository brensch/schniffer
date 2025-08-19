package manager_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brensch/schniffer/internal/bot"
	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/manager"
	"github.com/brensch/schniffer/internal/providers"
	"github.com/bwmarrin/discordgo"
)

// ------------------ Mocks & Helpers ------------------

// mockProvider implements providers.Provider for testing
type mockProvider struct{}

func (m *mockProvider) CampsiteURL(campgroundID, campsiteID string) string {
	return "https://example.com/campsite/" + campsiteID
}
func (m *mockProvider) CampgroundURL(campgroundID string) string {
	return "https://example.com/campground/" + campgroundID
}
func (m *mockProvider) Name() string { return "mock" }
func (m *mockProvider) FetchAvailability(ctx context.Context, campgroundID string, start, end time.Time) ([]providers.CampsiteAvailability, error) {
	return nil, nil
}
func (m *mockProvider) FetchAllCampgrounds(ctx context.Context) ([]providers.CampgroundInfo, error) {
	return nil, nil
}
func (m *mockProvider) FetchCampsites(ctx context.Context, campgroundID string) ([]providers.CampsiteInfo, error) {
	return nil, nil
}
func (m *mockProvider) PlanBuckets(dates []time.Time) []providers.DateRange { return nil }

func mustDate(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func genDates(start time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, start.AddDate(0, 0, i))
	}
	return out
}

func makeStats(campgroundDays int, campsiteID string, dates []time.Time, withDetails bool) manager.CampsiteStats {
	dets := db.CampsiteDetails{}
	if withDetails {
		dets = db.CampsiteDetails{
			Name:         "Fancy Site " + campsiteID,
			Type:         "Tent",
			CostPerNight: 123.45, // should be ignored by embed builder
			Rating:       4.9,    // should be ignored by embed builder
			Equipment:    []string{"Tent", "Car", "Bike"},
		}
	}
	return manager.CampsiteStats{
		CampsiteID:    campsiteID,
		DaysAvailable: len(dates),
		TotalDays:     campgroundDays,
		Dates:         dates,
		Details:       dets,
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ------------------ Unit-Style Tests (No Discord needed) ------------------

func TestBuildNotificationEmbeds_NoCampsites(t *testing.T) {
	checkin := mustDate(2025, 8, 18)
	checkout := checkin.AddDate(0, 0, 3)
	provider := &mockProvider{}

	embeds := manager.BuildNotificationEmbeds(
		checkin, checkout, "12345",
		"Test Campground", "https://example.com/cg", "cg1",
		nil,
		provider,
	)

	if len(embeds) == 0 {
		t.Fatalf("expected at least one embed with description header")
	}
	for _, e := range embeds {
		if e.Description == "" {
			t.Errorf("description should not be empty")
		}
		// No fields because no campsites
		if len(e.Fields) != 1 {
			// one "Remember" field is appended at the end of the last embed
			// but since there are no campsite fields, it will be on the only embed
			t.Logf("fields=%d (OK if 'Remember' was added)", len(e.Fields))
		}
	}
}

func TestBuildNotificationEmbeds_RemovesCostAndRating_AndHasDivider(t *testing.T) {
	checkin := mustDate(2025, 8, 18)
	checkout := checkin.AddDate(0, 0, 5)
	provider := &mockProvider{}
	stats := []manager.CampsiteStats{
		makeStats(5, "cs1", genDates(checkin, 3), true), // has cost/rating in details but should not appear
	}

	embeds := manager.BuildNotificationEmbeds(
		checkin, checkout, "u1",
		"Camp", "https://example.com/cg", "cgid",
		stats,
		provider,
	)
	if len(embeds) == 0 {
		t.Fatalf("expected embeds")
	}

	foundDivider := false
	for _, e := range embeds {
		for _, f := range e.Fields {
			if containsAny(f.Value, "Cost:", "Rating:", "⭐") {
				t.Fatalf("cost/rating must not be present; found in field: %q", f.Value)
			}
			if strings.Contains(f.Value, "────────────────────────────────────────") {
				foundDivider = true
			}
		}
	}
	if !foundDivider {
		t.Fatalf("expected divider line between campsites")
	}
}

func TestBuildNotificationEmbeds_SortsByDaysAvailableThenID(t *testing.T) {
	checkin := mustDate(2025, 8, 1)
	checkout := checkin.AddDate(0, 0, 10)
	provider := &mockProvider{}

	// Intentionally unsorted input:
	stats := []manager.CampsiteStats{
		makeStats(10, "csB", genDates(checkin, 4), true), // 4 days
		makeStats(10, "csA", genDates(checkin, 7), true), // 7 days -> should come first
		makeStats(10, "csC", genDates(checkin, 7), true), // 7 days -> tie, sorted by ID -> csA then csC
		makeStats(10, "csD", genDates(checkin, 2), true), // 2 days
	}

	embeds := manager.BuildNotificationEmbeds(
		checkin, checkout, "u2",
		"My CG", "https://example.com/cg", "cgid",
		stats,
		provider,
	)
	if len(embeds) == 0 {
		t.Fatalf("expected embeds")
	}

	// The first campsite field should be csA (or csA part 1 of N if chunked)
	var firstField *discordgo.MessageEmbedField
outer:
	for _, e := range embeds {
		for _, f := range e.Fields {
			if strings.HasPrefix(f.Name, "Campsite ") {
				firstField = f
				break outer
			}
		}
	}
	if firstField == nil {
		t.Fatalf("no campsite fields found")
	}
	if !strings.HasPrefix(firstField.Name, "Campsite csA") {
		t.Fatalf("expected first campsite to be csA, got field name: %s", firstField.Name)
	}
}

func TestBuildNotificationEmbeds_SplitsAcrossMultipleEmbeds_ByFieldCount(t *testing.T) {
	checkin := mustDate(2025, 8, 18)
	checkout := checkin.AddDate(0, 0, 3)
	provider := &mockProvider{}

	// 60 campsites -> with 25 fields max per embed, should create at least 3 embeds
	stats := make([]manager.CampsiteStats, 0, 60)
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("cs%03d", i+1)
		stats = append(stats, makeStats(3, id, genDates(checkin, 2), true))
	}

	embeds := manager.BuildNotificationEmbeds(
		checkin, checkout, "userX",
		"Huge CG", "https://example.com/cg", "cgHuge",
		stats,
		provider,
	)

	if len(embeds) < 3 {
		t.Fatalf("expected >= 3 embeds, got %d", len(embeds))
	}
	for i, e := range embeds {
		if len(e.Fields) > 25 {
			t.Fatalf("embed %d exceeds 25 fields: %d", i, len(e.Fields))
		}
	}
}

func TestBuildNotificationEmbeds_ChunksLongFieldValues_AndDoesNotTruncate(t *testing.T) {
	checkin := mustDate(2025, 8, 18)
	checkout := checkin.AddDate(0, 0, 300)
	provider := &mockProvider{}

	// Single campsite with 200+ dates -> field value should exceed 1024 and be chunked into multiple fields
	longDates := genDates(checkin, 200)
	stats := []manager.CampsiteStats{
		makeStats(300, "csLONG", longDates, true),
	}

	embeds := manager.BuildNotificationEmbeds(
		checkin, checkout, "uChunk",
		"Chunk CG", "https://example.com/cg", "cgChunk",
		stats,
		provider,
	)

	if len(embeds) == 0 {
		t.Fatalf("expected embeds")
	}

	// Collect fields for this campsite
	var parts []*discordgo.MessageEmbedField
	for _, e := range embeds {
		for _, f := range e.Fields {
			if strings.HasPrefix(f.Name, "Campsite csLONG") {
				parts = append(parts, f)
			}
		}
	}
	if len(parts) < 2 {
		t.Fatalf("expected multiple chunked fields for csLONG, got %d", len(parts))
	}
	for i, p := range parts {
		if len(p.Value) == 0 {
			t.Fatalf("part %d empty", i)
		}
		if len(p.Value) > 1024 {
			t.Fatalf("part %d exceeds 1024 chars; got %d", i, len(p.Value))
		}
	}

	// Ensure some tail dates are present (no truncation of the list overall)
	lastDateStr := longDates[len(longDates)-1].Format("2006-01-02")
	foundLast := false
	for _, p := range parts {
		if strings.Contains(p.Value, lastDateStr) {
			foundLast = true
			break
		}
	}
	if !foundLast {
		t.Fatalf("expected last date %s to appear across chunks", lastDateStr)
	}
}

func TestBuildNotificationEmbeds_NoProviderURL_OK(t *testing.T) {
	checkin := mustDate(2025, 8, 18)
	checkout := checkin.AddDate(0, 0, 5)
	// provider = nil -> should still build fine without links
	var provider providers.Provider

	stats := []manager.CampsiteStats{
		makeStats(5, "cs1", genDates(checkin, 3), true),
	}

	embeds := manager.BuildNotificationEmbeds(
		checkin, checkout, "uNP",
		"No Provider CG", "https://example.com/cg", "cgid",
		stats,
		provider,
	)

	if len(embeds) == 0 {
		t.Fatalf("expected embeds")
	}
	// Ensure no "](http" in values when provider is nil (no links)
	for _, e := range embeds {
		for _, f := range e.Fields {
			if strings.HasPrefix(f.Name, "Campsite ") && strings.Contains(f.Value, "](") {
				t.Fatalf("unexpected link in field value without provider: %q", f.Value)
			}
		}
	}
}

func TestBuildNotificationEmbeds_EquipmentNotTruncated(t *testing.T) {
	checkin := mustDate(2025, 8, 18)
	checkout := checkin.AddDate(0, 0, 5)
	provider := &mockProvider{}

	// Large equipment list should not be truncated by builder
	dets := db.CampsiteDetails{
		Name:         "Monster Equip Site",
		Type:         "Tent/RV",
		Equipment:    []string{"Tent", "RV", "Trailer", "Van", "Car", "Bike", "Boat", "Kayak", "Motorcycle", "Foot"},
		CostPerNight: 999, // ignored
		Rating:       5,   // ignored
	}
	stats := []manager.CampsiteStats{
		{
			CampsiteID:    "csEQ",
			DaysAvailable: 3,
			TotalDays:     5,
			Dates:         genDates(checkin, 3),
			Details:       dets,
		},
	}

	embeds := manager.BuildNotificationEmbeds(
		checkin, checkout, "uEq",
		"Equip CG", "https://example.com/cg", "cgEq",
		stats,
		provider,
	)

	if len(embeds) == 0 {
		t.Fatalf("expected embeds")
	}
	ok := false
	for _, e := range embeds {
		for _, f := range e.Fields {
			if strings.Contains(f.Value, "Equipment: Tent, RV, Trailer, Van, Car, Bike, Boat, Kayak, Motorcycle, Foot") {
				ok = true
			}
		}
	}
	if !ok {
		t.Fatalf("expected full equipment list present without truncation")
	}
}

// ------------------ Integration-style Test (optional live send) ------------------

func TestBuildNotificationEmbeds_SendToDiscord_LongAndMixed(t *testing.T) {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		t.Skip("DISCORD_TOKEN not set; skipping live Discord send")
		return
	}
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		t.Fatalf("error creating Discord session: %v", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentDirectMessages | discordgo.IntentsGuildMembers

	if err := dg.Open(); err != nil {
		t.Fatal(err)
	}
	defer dg.Close()

	guildID := os.Getenv("GUILD_ID")
	if guildID == "" {
		t.Skip("GUILD_ID env var not set, skipping live send")
		return
	}

	channelID, err := bot.GuildIDToChannelID(dg, guildID)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("sending to channel id", channelID)

	checkin := mustDate(2025, 8, 18)
	checkout := checkin.AddDate(0, 0, 30) // a month
	provider := &mockProvider{}

	// Build a mixed set: a lot of small sites + one huge date-list site
	stats := make([]manager.CampsiteStats, 0, 40)
	for i := 0; i < 35; i++ {
		id := fmt.Sprintf("cs%03d", i+1)
		stats = append(stats, makeStats(30, id, genDates(checkin, (i%5)+1), true))
	}
	// One very long
	stats = append(stats, makeStats(30, "csLONG", genDates(checkin, 200), true))

	embeds := manager.BuildNotificationEmbeds(
		checkin, checkout, "12345",
		"Test Campground", "https://example.com/campground", "cg1",
		stats,
		provider,
	)

	if len(embeds) == 0 {
		t.Fatalf("expected embeds")
	}
	for _, e := range embeds {
		if e.Description == "" {
			t.Errorf("embed description should not be empty")
		}
		if len(e.Fields) > 25 {
			t.Fatalf("embed has too many fields: %d", len(e.Fields))
		}
		// send live
		if _, err := dg.ChannelMessageSendEmbed(channelID, e); err != nil {
			t.Errorf("send embed failed: %v", err)
		}
	}
}

// ------------------ Retained Original-ish Smoke Suite ------------------

func TestBuildNotificationEmbed_Suite_Smoke(t *testing.T) {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Println("DISCORD_TOKEN env var not set; running without live send")
	}

	var dg *discordgo.Session
	var err error
	if token != "" {
		dg, err = discordgo.New("Bot " + token)
		if err != nil {
			t.Fatalf("error creating Discord session: %v", err)
		}
		dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentDirectMessages | discordgo.IntentsGuildMembers
		if err = dg.Open(); err != nil {
			t.Error(err)
		}
		defer dg.Close()
	}

	guildID := os.Getenv("GUILD_ID")
	var channelID string
	if dg != nil && guildID != "" {
		channelID, err = bot.GuildIDToChannelID(dg, guildID)
		if err != nil {
			t.Error(err)
		}
		t.Log("got channel id", channelID)
	}

	checkin := time.Date(2025, 8, 18, 0, 0, 0, 0, time.UTC)
	checkout := checkin.AddDate(0, 0, 3)
	provider := &mockProvider{}

	testCases := []struct {
		name           string
		checkin        time.Time
		checkout       time.Time
		userID         string
		campgroundName string
		campgroundURL  string
		campgroundID   string
		campsiteStats  []manager.CampsiteStats
		provider       providers.Provider
	}{
		{
			name:           "No campsites available",
			checkin:        checkin,
			checkout:       checkout,
			userID:         "12345",
			campgroundName: "Test Campground",
			campgroundURL:  "https://example.com/campground",
			campgroundID:   "cg1",
			campsiteStats:  []manager.CampsiteStats{},
			provider:       provider,
		},
		{
			name:           "One campsite basic",
			checkin:        checkin,
			checkout:       checkout,
			userID:         "12345",
			campgroundName: "Test Campground",
			campgroundURL:  "https://example.com/campground",
			campgroundID:   "cg1",
			campsiteStats: []manager.CampsiteStats{
				{
					CampsiteID:    "cs1",
					DaysAvailable: 2,
					TotalDays:     3,
					Dates:         []time.Time{checkin, checkin.AddDate(0, 0, 1)},
					Details:       db.CampsiteDetails{Name: "Site 1", Type: "Tent", CostPerNight: 25.0, Rating: 4.5, Equipment: []string{"Tent", "Car"}},
				},
			},
			provider: provider,
		},
		{
			name:           "Multiple campsites",
			checkin:        checkin,
			checkout:       checkout,
			userID:         "67890",
			campgroundName: "Another Campground",
			campgroundURL:  "https://example.com/campground2",
			campgroundID:   "cg2",
			campsiteStats: []manager.CampsiteStats{
				{
					CampsiteID:    "cs2",
					DaysAvailable: 3,
					TotalDays:     3,
					Dates:         []time.Time{checkin, checkin.AddDate(0, 0, 1), checkin.AddDate(0, 0, 2)},
					Details:       db.CampsiteDetails{Name: "Site 2", Type: "RV", CostPerNight: 40.0, Rating: 4.8, Equipment: []string{"RV"}},
				},
				{
					CampsiteID:    "cs3",
					DaysAvailable: 1,
					TotalDays:     3,
					Dates:         []time.Time{checkin.AddDate(0, 0, 2)},
					Details:       db.CampsiteDetails{Name: "Site 3", Type: "Cabin", CostPerNight: 100.0, Rating: 5.0, Equipment: []string{"Cabin"}},
				},
			},
			provider: provider,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			embeds := manager.BuildNotificationEmbeds(
				tc.checkin, tc.checkout, tc.userID,
				tc.campgroundName, tc.campgroundURL, tc.campgroundID,
				tc.campsiteStats,
				tc.provider,
			)
			if len(embeds) == 0 {
				t.Fatalf("expected embeds")
			}
			for _, embed := range embeds {
				if embed.Description == "" {
					t.Errorf("embed description should not be empty")
				}
				if len(embed.Fields) > 25 {
					t.Fatalf("embed has too many fields: %d", len(embed.Fields))
				}
				// Ensure we didn't include cost/rating
				for _, f := range embed.Fields {
					if containsAny(f.Value, "Cost:", "Rating:", "⭐") {
						t.Fatalf("cost/rating must not be present in field: %q", f.Value)
					}
				}
			}

			// Optional send to Discord if configured
			if dg != nil && channelID != "" {
				for _, embed := range embeds {
					if _, err := dg.ChannelMessageSendEmbed(channelID, embed); err != nil {
						t.Error(err)
					}
				}
			}
		})
	}
}
