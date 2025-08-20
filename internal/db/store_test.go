package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestDeactivateExpiredRequests(t *testing.T) {
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
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	dayAfterTomorrow := time.Now().AddDate(0, 0, 2).Format("2006-01-02")

	// Insert test data
	testCases := []struct {
		name                string
		checkin             string
		checkout            string
		shouldBeDeactivated bool
	}{
		{"Past checkin, past checkout", yesterday, yesterday, true},
		{"Past checkin, future checkout", yesterday, tomorrow, true},
		{"Future checkin, future checkout", tomorrow, dayAfterTomorrow, false},
		{"Future checkin, far future checkout", dayAfterTomorrow, dayAfterTomorrow, false},
		{"Future checkin, past checkout (edge case)", tomorrow, yesterday, true}, // checkout in past
	}

	for i, tc := range testCases {
		_, err = db.Exec(`
			INSERT INTO schniff_requests (user_id, provider, campground_id, checkin, checkout, active) 
			VALUES (?, ?, ?, ?, ?, true)
		`, "test_user", "test_provider", "test_campground", tc.checkin, tc.checkout)
		if err != nil {
			t.Fatalf("Failed to insert test case %d (%s): %v", i, tc.name, err)
		}
	}

	// Call the function
	deactivatedCount, err := store.DeactivateExpiredRequests(ctx)
	if err != nil {
		t.Fatalf("DeactivateExpiredRequests failed: %v", err)
	}

	// Count expected deactivations
	expectedDeactivated := 0
	for _, tc := range testCases {
		if tc.shouldBeDeactivated {
			expectedDeactivated++
		}
	}

	if deactivatedCount != int64(expectedDeactivated) {
		t.Errorf("Expected %d deactivated requests, got %d", expectedDeactivated, deactivatedCount)
	}

	// Verify the correct requests were deactivated
	rows, err := db.Query(`
		SELECT checkin, checkout, active 
		FROM schniff_requests 
		ORDER BY id
	`)
	if err != nil {
		t.Fatalf("Failed to query results: %v", err)
	}
	defer rows.Close()

	i := 0
	for rows.Next() {
		var checkin, checkout string
		var active bool
		if err := rows.Scan(&checkin, &checkout, &active); err != nil {
			t.Fatalf("Failed to scan row: %v", err)
		}

		tc := testCases[i]
		expectedActive := !tc.shouldBeDeactivated

		if active != expectedActive {
			t.Errorf("Test case %d (%s): expected active=%v, got active=%v",
				i, tc.name, expectedActive, active)
		}
		i++
	}
}
