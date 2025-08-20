package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brensch/schniffer/internal/db"
)

func TestBuildDeactivationEmbed(t *testing.T) {
	// Create a mock manager with a nil store for testing
	// Since GetCampgroundByID will fail with nil store, we expect fallback to campground ID
	manager := &Manager{
		store: nil, // This will cause GetCampgroundByID to fail, triggering fallback behavior
	}

	// Create test requests
	requests := []db.SchniffRequest{
		{
			ID:           1,
			UserID:       "user123",
			Provider:     "recreation_gov",
			CampgroundID: "cg001",
			Checkin:      time.Date(2025, 8, 15, 0, 0, 0, 0, time.UTC),
			Checkout:     time.Date(2025, 8, 18, 0, 0, 0, 0, time.UTC),
		},
		{
			ID:           2,
			UserID:       "user123",
			Provider:     "reservecalifornia",
			CampgroundID: "cg002",
			Checkin:      time.Date(2025, 8, 17, 0, 0, 0, 0, time.UTC),
			Checkout:     time.Date(2025, 8, 20, 0, 0, 0, 0, time.UTC),
		},
	}

	ctx := context.Background()
	embed := manager.buildDeactivationEmbed(ctx, requests)

	// Verify embed structure
	if embed == nil {
		t.Fatal("Expected embed to be non-nil")
	}

	// Check title
	expectedTitle := "🛑 2 Schniff Requests Deactivated"
	if embed.Title != expectedTitle {
		t.Errorf("Expected title '%s', got '%s'", expectedTitle, embed.Title)
	}

	// Check color (should be red for warnings)
	expectedColor := 0xFF6B6B
	if embed.Color != expectedColor {
		t.Errorf("Expected color %d, got %d", expectedColor, embed.Color)
	}

	// Check that we have fields for each request plus call-to-action field
	expectedFieldCount := 3 // 2 requests + 1 call-to-action
	if len(embed.Fields) != expectedFieldCount {
		t.Errorf("Expected %d fields, got %d", expectedFieldCount, len(embed.Fields))
	}

	// Check footer
	if embed.Footer == nil {
		t.Error("Expected footer to be present")
	} else {
		expectedFooterText := "💡 Click the /schniff command above to create new requests in a server where I'm present."
		if embed.Footer.Text != expectedFooterText {
			t.Errorf("Expected footer text '%s', got '%s'", expectedFooterText, embed.Footer.Text)
		}
	}

	// Check timestamp format
	if embed.Timestamp == "" {
		t.Error("Expected timestamp to be present")
	}

	// Verify field content
	if len(embed.Fields) >= 1 {
		firstField := embed.Fields[0]
		if firstField.Name != "1. cg001" {
			t.Errorf("Expected first field name '1. cg001', got '%s'", firstField.Name)
		}

		if !contains(firstField.Value, "📅 **Dates:** Aug 15, 2025 - Aug 18, 2025") {
			t.Errorf("Expected date information in field value, got '%s'", firstField.Value)
		}

		if !contains(firstField.Value, "🏕️ **Provider:** recreation_gov") {
			t.Errorf("Expected provider information in field value, got '%s'", firstField.Value)
		}
	}

	// Verify that the slash command mention is included in description
	if !contains(embed.Description, "</schniff:0>") {
		t.Errorf("Expected slash command mention in description, got '%s'", embed.Description)
	}

	// Verify the call-to-action field exists
	if len(embed.Fields) >= 3 {
		ctaField := embed.Fields[2] // Last field should be call-to-action
		if ctaField.Name != "🚀 Create New Requests" {
			t.Errorf("Expected call-to-action field name '🚀 Create New Requests', got '%s'", ctaField.Name)
		}
		if !contains(ctaField.Value, "</schniff:0>") {
			t.Errorf("Expected slash command mention in call-to-action field, got '%s'", ctaField.Value)
		}
	}
}

func TestBuildDeactivationEmbed_SingleRequest(t *testing.T) {
	manager := &Manager{}

	// Create single test request
	requests := []db.SchniffRequest{
		{
			ID:           1,
			UserID:       "user123",
			Provider:     "recreation_gov",
			CampgroundID: "cg001",
			Checkin:      time.Date(2025, 8, 15, 0, 0, 0, 0, time.UTC),
			Checkout:     time.Date(2025, 8, 18, 0, 0, 0, 0, time.UTC),
		},
	}

	ctx := context.Background()
	embed := manager.buildDeactivationEmbed(ctx, requests)

	// Check title for single request
	expectedTitle := "🛑 Schniff Request Deactivated"
	if embed.Title != expectedTitle {
		t.Errorf("Expected title '%s', got '%s'", expectedTitle, embed.Title)
	}

	// Should have exactly two fields (1 request + 1 call-to-action)
	if len(embed.Fields) != 2 {
		t.Errorf("Expected 2 fields, got %d", len(embed.Fields))
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
