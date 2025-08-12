package manager

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/providers"
)

// fake provider implementing minimal interfaces for tests
type fakeProv struct {
	name    string
	buckets []providers.DateRange
	// map key: date string YYYY-MM-DD -> list of campsite IDs available
	data map[string][]providers.Campsite
}

func (f *fakeProv) Name() string                   { return f.name }
func (f *fakeProv) CampsiteURL(_, _ string) string { return "" }
func (f *fakeProv) FetchAllCampgrounds(ctx context.Context) ([]providers.CampgroundInfo, error) {
	return nil, nil
}
func (f *fakeProv) PlanBuckets(dates []time.Time) []providers.DateRange {
	return f.buckets
}
func (f *fakeProv) FetchAvailability(ctx context.Context, campgroundID string, start, end time.Time) ([]providers.Campsite, error) {
	// return all entries whose date falls within [start..end]
	out := []providers.Campsite{}
	for d, arr := range f.data {
		t, _ := time.Parse("2006-01-02", d)
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		if (t.Equal(start) || t.After(start)) && (t.Equal(end) || t.Before(end)) {
			out = append(out, arr...)
		}
	}
	return out, nil
}

func Test_normalizeDay(t *testing.T) {
	ts := time.Date(2025, 1, 2, 12, 34, 56, 123, time.FixedZone("X", 7*3600))
	got := normalizeDay(ts)
	want := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func Test_generateNights(t *testing.T) {
	start := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC)
	got := generateNights(start, end)
	want := []time.Time{
		time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 4, 0, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func Test_datesFromSet(t *testing.T) {
	set := map[time.Time]struct{}{
		time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC): {},
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC): {},
		time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC): {},
	}
	got := datesFromSet(set)
	want := []time.Time{
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func Test_collectDatesByPC(t *testing.T) {
	reqs := []db.SchniffRequest{
		{Provider: "p", CampgroundID: "cg", Checkin: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC), Checkout: time.Date(2025, 1, 4, 0, 0, 0, 0, time.UTC)},
		{Provider: "p", CampgroundID: "cg", Checkin: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC), Checkout: time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)},
	}
	datesBy, reqsBy := collectDatesByPC(reqs)
	key := pc{prov: "p", cg: "cg"}
	if len(reqsBy[key]) != 2 {
		t.Fatalf("reqs grouping failed: %+v", reqsBy[key])
	}
	got := datesFromSet(datesBy[key])
	want := []time.Time{
		time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 4, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func Test_pollOnceResult_MinimalCalls(t *testing.T) {
	// Setup DB
	s := func(t *testing.T) *db.Store {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "test.duckdb")
		st, err := db.Open(path)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		t.Cleanup(func() { _ = st.Close(); _ = os.Remove(path) })
		return st
	}(t)
	ctx := context.Background()
	// two overlapping requests for the same provider/campground/month
	must := func(err error) {
		if err != nil {
			t.Fatalf("%v", err)
		}
	}
	_, err := s.AddRequest(ctx, db.SchniffRequest{UserID: "u1", Provider: "fake", CampgroundID: "cg", Checkin: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC), Checkout: time.Date(2025, 1, 4, 0, 0, 0, 0, time.UTC)})
	must(err)
	_, err = s.AddRequest(ctx, db.SchniffRequest{UserID: "u2", Provider: "fake", CampgroundID: "cg", Checkin: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC), Checkout: time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)})
	must(err)

	// Fake provider that buckets entire month Jan 2025 into a single call
	fp := &fakeProv{
		name: "fake",
		buckets: []providers.DateRange{{
			Start: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			End:   time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC),
		}},
		data: map[string][]providers.Campsite{
			"2025-01-03": {{ID: "s1", Date: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC), Available: true}},
		},
	}
	reg := providers.NewRegistry()
	reg.Register("fake", fp)
	mgr := NewManager(s, reg)

	res := mgr.pollOnceResult(ctx)
	if len(res.Calls) != 1 {
		t.Fatalf("expected 1 upstream call, got %d: %+v", len(res.Calls), res.Calls)
	}
	c := res.Calls[0]
	if c.Provider != "fake" || c.CampgroundID != "cg" || !c.Success {
		t.Fatalf("unexpected call: %+v", c)
	}
	if c.Start != fp.buckets[0].Start || c.End != fp.buckets[0].End {
		t.Fatalf("unexpected bucket window: got %v..%v", c.Start, c.End)
	}
}
