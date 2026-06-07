package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// fenceDay is a small helper for readable UTC midnight dates used by these tests.
func fenceDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// setupFenceDB creates the tables used by the date-range queries we exercise here.
func setupFenceDB(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	stmts := []string{
		`CREATE TABLE campsite_availability (
			provider TEXT, campground_id TEXT, campsite_id TEXT,
			date DATETIME, available INTEGER, last_checked DATETIME,
			PRIMARY KEY (provider, campground_id, campsite_id, date)
		)`,
		`CREATE TABLE state_changes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			provider TEXT, campground_id TEXT, campsite_id TEXT,
			date DATETIME, new_available INTEGER,
			changed_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE notifications (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			batch_id TEXT,
			request_id INTEGER,
			user_id TEXT, provider TEXT, campground_id TEXT, campsite_id TEXT,
			date DATETIME, state TEXT, state_change_id INTEGER,
			sent_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return &Store{DB: db}
}

// TestGetCurrentlyAvailableCampsites_Fencepost confirms the SQL range filter is
// [checkin, checkout) — i.e., checkin day IS included (the arrival night) and
// checkout day is NOT included (you've already left). The motivating bug case:
// a Fri->Sun request must not pick up a Thursday-night availability row, but
// must pick up the Friday and Saturday nights.
func TestGetCurrentlyAvailableCampsites_Fencepost(t *testing.T) {
	store := setupFenceDB(t)
	ctx := context.Background()

	wed := fenceDay(2025, 8, 13)
	thu := fenceDay(2025, 8, 14)
	fri := fenceDay(2025, 8, 15)
	sat := fenceDay(2025, 8, 16)
	sun := fenceDay(2025, 8, 17)
	mon := fenceDay(2025, 8, 18)

	// Seed one campsite as available across the whole week.
	for _, d := range []time.Time{wed, thu, fri, sat, sun, mon} {
		_, err := store.DB.ExecContext(ctx, `
			INSERT INTO campsite_availability(provider, campground_id, campsite_id, date, available, last_checked)
			VALUES ('p', 'cg', 's1', ?, 1, ?)
		`, d, time.Now())
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Fri->Sun request: nights are Fri and Sat only.
	got, err := store.GetCurrentlyAvailableCampsites(ctx, "p", "cg", fri, sun)
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	wantDates := map[string]bool{
		fri.Format("2006-01-02"): true,
		sat.Format("2006-01-02"): true,
	}
	bannedDates := map[string]bool{
		wed.Format("2006-01-02"): true,
		thu.Format("2006-01-02"): true, // the bug-report case
		sun.Format("2006-01-02"): true, // checkout day must be excluded
		mon.Format("2006-01-02"): true,
	}

	gotDates := map[string]bool{}
	for _, it := range got {
		k := it.Date.UTC().Format("2006-01-02")
		gotDates[k] = true
		if bannedDates[k] {
			t.Errorf("unexpected date in result: %s (Fri->Sun request must not return this)", k)
		}
	}
	for d := range wantDates {
		if !gotDates[d] {
			t.Errorf("missing expected date in result: %s", d)
		}
	}
}

// TestGetUnnotifiedStateChanges_Fencepost is the end-to-end check that motivated
// the bug report: a state-change row for a Thursday-night flip-to-available must
// NOT surface as a notifyable change for a Fri->Sun request.
func TestGetUnnotifiedStateChanges_Fencepost(t *testing.T) {
	store := setupFenceDB(t)
	ctx := context.Background()

	thu := fenceDay(2025, 8, 14)
	fri := fenceDay(2025, 8, 15)
	sat := fenceDay(2025, 8, 16)
	sun := fenceDay(2025, 8, 17)

	// Seed three state changes: Thu (out of range), Fri (in range), Sun (out of range, == checkout day).
	for _, d := range []time.Time{thu, fri, sun} {
		_, err := store.DB.ExecContext(ctx, `
			INSERT INTO state_changes(provider, campground_id, campsite_id, date, new_available)
			VALUES ('p', 'cg', 's1', ?, 1)
		`, d)
		if err != nil {
			t.Fatalf("seed state_change: %v", err)
		}
	}

	req := SchniffRequest{
		ID: 42, UserID: "u", Provider: "p", CampgroundID: "cg",
		Checkin: fri, Checkout: sun, Active: true,
	}

	changes, err := store.GetUnnotifiedStateChanges(ctx, []SchniffRequest{req})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	if len(changes) != 1 {
		t.Fatalf("expected exactly 1 in-range state change, got %d (%+v)", len(changes), changes)
	}
	gotDate := changes[0].Date.UTC().Format("2006-01-02")
	if gotDate != fri.Format("2006-01-02") {
		t.Errorf("expected Fri state change, got %s", gotDate)
	}

	// Saturday isn't even in the seeded set — sanity guard so this test breaks loudly
	// if the seed expands but the assertions aren't updated.
	_ = sat
}
