package db_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brensch/schniffer/internal/db"
)

func newTestStore(t *testing.T) *db.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.duckdb")
	s, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(); _ = os.Remove(path) })
	return s
}

func TestRequestsCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC)
	id, err := s.AddRequest(ctx, db.SchniffRequest{UserID: "u1", Provider: "recreation_gov", CampgroundID: "cg1", Checkin: start, Checkout: end})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if id == 0 {
		t.Fatalf("got id 0")
	}
	reqs, err := s.ListActiveRequests(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("want 1 req, got %d", len(reqs))
	}
	if reqs[0].ID != id || !reqs[0].Active {
		t.Fatalf("unexpected req: %+v", reqs[0])
	}
	if err := s.DeactivateRequest(ctx, id, "u1"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	reqs, err = s.ListActiveRequests(ctx)
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	if len(reqs) != 0 {
		t.Fatalf("want 0 after deactivate, got %d", len(reqs))
	}
}

func TestStateAndNotifications(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	states := []db.CampsiteState{
		{Provider: "recreation_gov", CampgroundID: "cg1", CampsiteID: "s1", Date: now, Available: false, CheckedAt: now},
	}
	if err := s.UpsertCampsiteStateBatch(ctx, states); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	avail, ok, err := s.GetLastState(ctx, "recreation_gov", "cg1", "s1", now)
	if err != nil {
		t.Fatalf("get last: %v", err)
	}
	if !ok || avail {
		t.Fatalf("unexpected state ok=%v avail=%v", ok, avail)
	}
	// record notification
	if err := s.RecordNotification(ctx, db.Notification{RequestID: 1, UserID: "u1", Provider: "recreation_gov", CampgroundID: "cg1", CampsiteID: "s1", Date: now, State: "unavailable", SentAt: now}); err != nil {
		t.Fatalf("record note: %v", err)
	}
}

func TestLookupLog(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.RecordLookup(ctx, db.LookupLog{Provider: "recreation_gov", CampgroundID: "cg1", Month: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), CheckedAt: time.Now().UTC(), Success: true}); err != nil {
		t.Fatalf("lookup: %v", err)
	}
}

func TestMetadata(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertCampground(ctx, "recreation_gov", "cg1", "Test Campground", "Parent Area", "p1", 1.23, 4.56); err != nil {
		t.Fatalf("upsert cg: %v", err)
	}
	if err := s.UpsertCampsiteMeta(ctx, "recreation_gov", "cg1", "s1", "Site 1"); err != nil {
		t.Fatalf("upsert site: %v", err)
	}
	cgs, err := s.ListCampgrounds(ctx, "Test")
	if err != nil {
		t.Fatalf("list cg: %v", err)
	}
	if len(cgs) == 0 {
		t.Fatalf("expected campgrounds")
	}
	cg, ok, err := s.GetCampgroundByID(ctx, "recreation_gov", "cg1")
	if err != nil || !ok || cg.Name == "" {
		t.Fatalf("get by id: %v ok=%v cg=%+v", err, ok, cg)
	}
}
