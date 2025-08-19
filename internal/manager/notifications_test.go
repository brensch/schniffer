package manager_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/brensch/schniffer/internal/bot"
	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/manager"
	"github.com/brensch/schniffer/internal/providers"
	"github.com/bwmarrin/discordgo"
)

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

func TestBuildNotificationEmbed_Suite(t *testing.T) {

	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_TOKEN env var not set")
	}

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("error creating Discord session: %v", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentDirectMessages | discordgo.IntentsGuildMembers

	err = dg.Open()
	if err != nil {
		t.Error(err)
	}
	defer dg.Close()

	guildID := os.Getenv("GUILD_ID")
	if guildID == "" {
		t.Skip("GUILD_ID env var not set, skipping test")
	}

	channelID, err := bot.GuildIDToChannelID(dg, guildID)
	if err != nil {
		t.Error(err)
	}

	t.Log("got channel id", channelID)

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
		newlyAvailable []db.AvailabilityItem
		newlyBooked    []db.AvailabilityItem
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
			newlyAvailable: []db.AvailabilityItem{},
			newlyBooked:    []db.AvailabilityItem{},
			provider:       provider,
		},
		{
			name:           "One campsite, newly available",
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
			newlyAvailable: []db.AvailabilityItem{{CampsiteID: "cs1", Date: checkin}},
			newlyBooked:    []db.AvailabilityItem{},
			provider:       provider,
		},
		{
			name:           "Multiple campsites, mixed changes",
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
			newlyAvailable: []db.AvailabilityItem{{CampsiteID: "cs2", Date: checkin.AddDate(0, 0, 2)}},
			newlyBooked:    []db.AvailabilityItem{{CampsiteID: "cs3", Date: checkin.AddDate(0, 0, 2)}},
			provider:       provider,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fmt.Println("yoooooooooo")
			embed := manager.BuildNotificationEmbed(
				tc.checkin, tc.checkout, tc.userID,
				tc.campgroundName, tc.campgroundURL, tc.campgroundID,
				tc.campsiteStats,
				tc.newlyAvailable, tc.newlyBooked,
				tc.provider,
			)
			if embed == nil {
				t.Errorf("embed should not be nil")
			}
			if embed.Description == "" {
				t.Errorf("embed description should not be empty")
			}
			if len(embed.Fields) == 0 {
				t.Logf("embed has no fields (may be expected for no campsites)")
			}

			_, err = dg.ChannelMessageSendEmbed(channelID, embed)
			if err != nil {
				t.Error(err)
			}
		})
	}
}
