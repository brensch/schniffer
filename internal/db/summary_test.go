package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func newSummaryTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE notifications (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			batch_id TEXT NOT NULL DEFAULT '',
			request_id INTEGER NOT NULL DEFAULT 0,
			user_id TEXT NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			campground_id TEXT NOT NULL DEFAULT '',
			campsite_id TEXT NOT NULL DEFAULT '',
			date DATE NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			state_change_id INTEGER,
			sent_at DATETIME NOT NULL
		);
		CREATE TABLE schniff_requests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			campground_id TEXT NOT NULL DEFAULT '',
			checkin DATE NOT NULL DEFAULT '',
			checkout DATE NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			active BOOLEAN DEFAULT TRUE
		);
	`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &Store{DB: db, ReadDB: db}
}

func TestGetUserNotificationCounts(t *testing.T) {
	store := newSummaryTestStore(t)
	ctx := context.Background()
	// alice: 3 available, 1 unavailable (should be filtered out)
	// bob: 1 available
	// carol: 0 (only unavailable)
	for _, r := range []struct {
		user, state string
		ago         string
	}{
		{"alice", "available", "-1 hour"},
		{"alice", "available", "-2 hour"},
		{"alice", "available", "-3 hour"},
		{"alice", "unavailable", "-1 hour"},
		{"bob", "available", "-4 hour"},
		{"carol", "unavailable", "-1 hour"},
		{"alice", "available", "-2 day"}, // outside 24h, ignored
	} {
		_, err := store.DB.ExecContext(ctx,
			`INSERT INTO notifications(user_id, state, sent_at) VALUES (?,?,datetime('now',?))`,
			r.user, r.state, r.ago)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.GetUserNotificationCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 users, got %d: %+v", len(got), got)
	}
	if got[0].UserID != "alice" || got[0].Count != 3 {
		t.Errorf("want alice=3 first, got %+v", got[0])
	}
	if got[1].UserID != "bob" || got[1].Count != 1 {
		t.Errorf("want bob=1 second, got %+v", got[1])
	}
}

func TestGetUserActiveRequestCounts(t *testing.T) {
	store := newSummaryTestStore(t)
	ctx := context.Background()
	for _, r := range []struct {
		user   string
		active int
	}{
		{"alice", 1},
		{"alice", 1},
		{"bob", 1},
		{"bob", 1},
		{"bob", 1},
		{"carol", 0}, // inactive — filtered out
	} {
		_, err := store.DB.ExecContext(ctx,
			`INSERT INTO schniff_requests(user_id, active) VALUES (?,?)`,
			r.user, r.active)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.GetUserActiveRequestCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 active users, got %d: %+v", len(got), got)
	}
	if got[0].UserID != "bob" || got[0].Count != 3 {
		t.Errorf("want bob=3 first, got %+v", got[0])
	}
	if got[1].UserID != "alice" || got[1].Count != 2 {
		t.Errorf("want alice=2 second, got %+v", got[1])
	}
}

func TestMakeSummaryEmbed_CombinedActiveAndFired(t *testing.T) {
	data := SummaryData{
		ActiveCounts: []UserCount{
			{UserID: "u1", Count: 4}, // hulio has 4 active
			{UserID: "u3", Count: 1}, // u3 has 1 active, no fires
		},
		NotificationCounts: []UserCount{
			{UserID: "u1", Count: 2}, // hulio got 2 fires
			{UserID: "u2", Count: 5}, // u2 got fires but no active (e.g. removed mid-window)
		},
		UserNames: map[string]string{
			"u1": "hulio",
			// u2, u3 intentionally absent — should fall back to <@id>
		},
	}
	embed := MakeSummaryEmbed(data)
	var got string
	for _, f := range embed.Fields {
		if f.Name == "👥 Schniffists" {
			got = f.Value
		}
	}
	if got == "" {
		t.Fatalf("Schniffists field missing")
	}
	// u2 has the most sites found → first row.
	if !strings.HasPrefix(got, "<@u2> — 0 active / 5 sites found") {
		t.Errorf("expected u2 first (most sites found); got %q", got)
	}
	if !strings.Contains(got, "hulio — 4 active / 2 sites found") {
		t.Errorf("expected hulio row; got %q", got)
	}
	if !strings.Contains(got, "<@u3> — 1 active / 0 sites found") {
		t.Errorf("expected u3 row (active-only); got %q", got)
	}
}

func TestMakeSummaryEmbed_SchniffsSentShowsUserDMs(t *testing.T) {
	// The "Schniffs Sent" field reflects the count of Discord messages
	// users actually received (UserDMs24h = distinct user×batch
	// state-changes), NOT the raw notifications-table row count
	// (Notifications24h). Verifies the embed wires the right field.
	data := SummaryData{
		Stats: DetailedSummaryStats{
			Notifications24h: 200, // many site×date rows
			UserDMs24h:       7,   // but only 7 actual DMs went out
		},
	}
	embed := MakeSummaryEmbed(data)
	var got string
	for _, f := range embed.Fields {
		if f.Name == "📬 Schniffs Sent" {
			got = f.Value
		}
	}
	if got != "7" {
		t.Errorf("Schniffs Sent should reflect UserDMs24h (7); got %q", got)
	}
}

func TestMakeSummaryEmbed_EmptyStates(t *testing.T) {
	embed := MakeSummaryEmbed(SummaryData{})
	var got string
	for _, f := range embed.Fields {
		if f.Name == "👥 Schniffists" {
			got = f.Value
		}
	}
	if !strings.Contains(got, "No schniffists yet") {
		t.Errorf("want empty placeholder, got %q", got)
	}
}
