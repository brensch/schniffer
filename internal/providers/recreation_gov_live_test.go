package providers

import (
	"context"
	"os"
	"testing"
	"time"
)

// This test hits the real recreation.gov search API. It is skipped by default.
// Run with: RUN_LIVE_REC_GOV=1 go test -run Live -v ./internal/providers -count=1
func TestRecreationGov_FetchAllCampgrounds_Live(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in -short mode")
	}
	if os.Getenv("RUN_LIVE_REC_GOV") == "" {
		t.Skip("set RUN_LIVE_REC_GOV=1 to run live recreation.gov API test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	p := NewRecreationGov()

	start := time.Now()
	camps, err := p.FetchAllCampgrounds(ctx)
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("FetchAllCampgrounds live call failed: %v", err)
	}

	t.Logf("fetched %d campgrounds from recreation.gov in %s", len(camps), dur)

	// Make minimal, robust assertions that shouldn't be flaky.
	if len(camps) < 50 {
		t.Fatalf("expected at least 50 campgrounds, got %d", len(camps))
	}
	// Spot check first few entries have non-empty fields.
	maxCheck := 3
	if len(camps) < maxCheck {
		maxCheck = len(camps)
	}
	for i := 0; i < maxCheck; i++ {
		if camps[i].ID == "" || camps[i].Name == "" {
			t.Fatalf("campground %d has empty fields: %+v", i, camps[i])
		}
	}
}

// This test exercises the availability endpoint live to ensure the start_date encoding works.
// Provide RUN_LIVE_REC_GOV=1 and REC_GOV_CAMPGROUND_ID to run.
func TestRecreationGov_FetchAvailability_Live(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in -short mode")
	}
	if os.Getenv("RUN_LIVE_REC_GOV") == "" {
		t.Skip("set RUN_LIVE_REC_GOV=1 to run live recreation.gov API test")
	}
	cgID := "245489"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p := NewRecreationGov()
	start := time.Now().UTC().AddDate(0, 0, -3) // a few days back to be safe
	end := start.AddDate(0, 2, 0)

	_, err := p.FetchAvailability(ctx, cgID, start, end)
	if err != nil {
		t.Fatalf("FetchAvailability live call failed: %v", err)
	}
}
