package db

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestDeactivateExpiredRequests_ReturnsCorrectRequests(t *testing.T) {
	// Create in-memory database for testing
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	store := &Store{DB: db}

	// Create the table
	_, err = db.Exec(`
		CREATE TABLE schniff_requests (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id     TEXT NOT NULL,
			provider    TEXT NOT NULL,
			campground_id TEXT NOT NULL,
			checkin     DATE NOT NULL,
			checkout    DATE NOT NULL,
			created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
			active      BOOLEAN DEFAULT TRUE
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}

	ctx := context.Background()

	// Insert test requests using SQLite's date functions
	testRequests := []struct {
		userID       string
		provider     string
		campgroundID string
		checkinDays  string // relative to now, e.g., "-1 day" for yesterday
		checkoutDays string
		shouldExpire bool
	}{
		{"user1", "provider1", "cg1", "-2 day", "-1 day", true},  // expired: checkin and checkout in past
		{"user2", "provider2", "cg2", "-1 day", "+1 day", true},  // expired: checkin in past
		{"user3", "provider1", "cg3", "+1 day", "+2 day", false}, // future: both in future
	}

	for _, req := range testRequests {
		_, err = db.Exec(`
			INSERT INTO schniff_requests (user_id, provider, campground_id, checkin, checkout, active) 
			VALUES (?, ?, ?, date('now', ?), date('now', ?), true)
		`, req.userID, req.provider, req.campgroundID, req.checkinDays, req.checkoutDays)
		if err != nil {
			t.Fatalf("Failed to insert test request: %v", err)
		}
	}

	// Call the function
	deactivatedRequests, err := store.DeactivateExpiredRequests(ctx)
	if err != nil {
		t.Fatalf("DeactivateExpiredRequests failed: %v", err)
	}

	// Should have 2 deactivated requests
	expectedCount := 2
	if len(deactivatedRequests) != expectedCount {
		t.Errorf("Expected %d deactivated requests, got %d", expectedCount, len(deactivatedRequests))
	}

	// Verify the returned requests have the correct data
	expectedUsers := map[string]bool{"user1": true, "user2": true}
	for _, req := range deactivatedRequests {
		if !expectedUsers[req.UserID] {
			t.Errorf("Unexpected user ID in deactivated requests: %s", req.UserID)
		}
		// The returned requests should have Active=true (their state before deactivation)
		if !req.Active {
			t.Errorf("Returned request should have Active=true (original state), got Active=%v", req.Active)
		}
		if req.Provider == "" || req.CampgroundID == "" {
			t.Errorf("Deactivated request missing required fields: Provider=%s, CampgroundID=%s", req.Provider, req.CampgroundID)
		}
	}

	// Verify that the remaining request is still active
	var remainingActive bool
	err = db.QueryRow("SELECT active FROM schniff_requests WHERE user_id = 'user3'").Scan(&remainingActive)
	if err != nil {
		t.Fatalf("Failed to check remaining request: %v", err)
	}
	if !remainingActive {
		t.Error("Future request should still be active")
	}
}
