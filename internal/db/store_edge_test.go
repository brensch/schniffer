package db

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestDeactivateExpiredRequests_EdgeCases(t *testing.T) {
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

	// Test case: today's date for checkin and tomorrow for checkout
	// Use SQLite's date functions to avoid timezone issues

	// Insert a request with today's checkin date but tomorrow's checkout
	_, err = db.Exec(`
		INSERT INTO schniff_requests (user_id, provider, campground_id, checkin, checkout, active) 
		VALUES ('user1', 'provider1', 'cg1', date('now'), date('now', '+1 day'), true)
	`)
	if err != nil {
		t.Fatalf("Failed to insert test data: %v", err)
	}

	// Call the function
	deactivatedCount, err := store.DeactivateExpiredRequests(ctx)
	if err != nil {
		t.Fatalf("DeactivateExpiredRequests failed: %v", err)
	}

	// Today's checkin should NOT be deactivated (since it's not before today)
	if deactivatedCount != 0 {
		t.Errorf("Expected 0 deactivated requests for today's checkin, got %d", deactivatedCount)
	}

	// Verify the request is still active
	var active bool
	err = db.QueryRow("SELECT active FROM schniff_requests WHERE id = 1").Scan(&active)
	if err != nil {
		t.Fatalf("Failed to query request status: %v", err)
	}

	if !active {
		t.Error("Request with today's checkin date should still be active")
	}
}
