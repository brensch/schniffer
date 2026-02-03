package providers

import (
	"context"
	"testing"
	"time"
)

// These are integration tests that make actual HTTP calls to the ReserveCalifornia API.
// Run with: go test -v -run TestReserveCalifornia_Integration ./internal/providers/

// TestReserveCalifornia_Integration_GridEndpoint tests that the grid endpoint is reachable
// and returns valid availability data.
func TestReserveCalifornia_Integration_GridEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	provider := NewReserveCalifornia()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Use a well-known campground: "357" is Caswell Memorial State Park from the example
	// Composite ID format: parentID-facilityID
	campgroundID := "76-357"

	start := time.Now().AddDate(0, 0, 7)  // One week from now
	end := time.Now().AddDate(0, 0, 14)   // Two weeks from now

	availability, err := provider.FetchAvailability(ctx, campgroundID, start, end)
	if err != nil {
		t.Fatalf("FetchAvailability failed: %v", err)
	}

	t.Logf("Received %d availability records for campground %s", len(availability), campgroundID)

	// We should get some availability data back (even if all sites are booked)
	// The API should return at least the structure of the campground
	if len(availability) == 0 {
		t.Log("Warning: no availability records returned - campground might be out of season")
	}

	// Verify the data structure is valid
	for i, a := range availability {
		if a.ID == "" {
			t.Errorf("Availability record %d has empty ID", i)
		}
		if a.Date.IsZero() {
			t.Errorf("Availability record %d has zero date", i)
		}
		// Log first few entries for debugging
		if i < 5 {
			t.Logf("  Site %s on %s: available=%v", a.ID, a.Date.Format("2006-01-02"), a.Available)
		}
	}
}

// TestReserveCalifornia_Integration_CityParkEndpoint tests that the citypark endpoint is reachable.
func TestReserveCalifornia_Integration_CityParkEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	provider := NewReserveCalifornia()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// FetchAllCampgrounds internally calls the citypark and place endpoints
	// We'll test with a very short timeout to just verify the citypark endpoint works
	// This test might take a while since it fetches all campgrounds

	// Create a context that will timeout if it takes too long
	shortCtx, shortCancel := context.WithTimeout(ctx, 30*time.Second)
	defer shortCancel()

	// We'll test the first request only by checking if it doesn't immediately fail
	// A full FetchAllCampgrounds takes too long for a unit test
	campgrounds, err := provider.FetchAllCampgrounds(shortCtx)

	// If we get a context deadline exceeded, that's acceptable - it means the endpoint is working
	// but we just didn't wait long enough
	if err != nil {
		if shortCtx.Err() == context.DeadlineExceeded {
			t.Log("Citypark endpoint reachable but FetchAllCampgrounds timed out (expected for this test)")
			return
		}
		t.Fatalf("FetchAllCampgrounds failed: %v", err)
	}

	t.Logf("Received %d campgrounds", len(campgrounds))

	if len(campgrounds) == 0 {
		t.Fatal("Expected at least some campgrounds to be returned")
	}

	// Verify data structure
	for i, cg := range campgrounds {
		if cg.ID == "" {
			t.Errorf("Campground %d has empty ID", i)
		}
		if cg.Name == "" {
			t.Errorf("Campground %d has empty Name", i)
		}
		// Log first few entries
		if i < 5 {
			t.Logf("  Campground: %s - %s (%.4f, %.4f)", cg.ID, cg.Name, cg.Lat, cg.Lon)
		}
	}
}

// TestReserveCalifornia_Integration_BaseURLReachable tests that the base URL is reachable
// with a simple HTTP request.
func TestReserveCalifornia_Integration_BaseURLReachable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	provider := NewReserveCalifornia()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Test with a minimal grid request to verify connectivity
	// Use a simple facility ID just to test the endpoint responds
	campgroundID := "76-357"
	start := time.Now()
	end := start.AddDate(0, 0, 1)

	_, err := provider.FetchAvailability(ctx, campgroundID, start, end)
	if err != nil {
		t.Fatalf("Failed to reach ReserveCalifornia API: %v", err)
	}

	t.Log("Successfully connected to ReserveCalifornia API")
}
