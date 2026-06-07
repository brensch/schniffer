package booker

import (
	"testing"
	"time"
)

func d(y int, m time.Month, day int) time.Time {
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
}

func TestSelectBestPartial_LongestWins(t *testing.T) {
	cands := []Candidate{
		{CampsiteID: "A", Dates: []time.Time{d(2026, 9, 1), d(2026, 9, 2)}, Rating: 5.0},
		{CampsiteID: "B", Dates: []time.Time{d(2026, 9, 1), d(2026, 9, 2), d(2026, 9, 3)}, Rating: 3.0},
	}
	pick, ok := SelectBestPartial(cands)
	if !ok || pick.CampsiteID != "B" {
		t.Fatalf("expected B, got %+v ok=%v", pick, ok)
	}
	if pick.Nights != 3 {
		t.Fatalf("want 3 nights, got %d", pick.Nights)
	}
	if !pick.CheckIn.Equal(d(2026, 9, 1)) || !pick.CheckOut.Equal(d(2026, 9, 4)) {
		t.Fatalf("bad checkin/checkout: %+v", pick)
	}
}

func TestSelectBestPartial_TieBreakByRating(t *testing.T) {
	cands := []Candidate{
		{CampsiteID: "A", Dates: []time.Time{d(2026, 9, 1), d(2026, 9, 2)}, Rating: 2.0},
		{CampsiteID: "B", Dates: []time.Time{d(2026, 9, 1), d(2026, 9, 2)}, Rating: 4.5},
	}
	pick, _ := SelectBestPartial(cands)
	if pick.CampsiteID != "B" {
		t.Fatalf("expected B (higher rating), got %s", pick.CampsiteID)
	}
}

func TestSelectBestPartial_NonContiguous(t *testing.T) {
	// gap means longest run is 2, not 3
	cands := []Candidate{
		{CampsiteID: "A", Dates: []time.Time{d(2026, 9, 1), d(2026, 9, 2), d(2026, 9, 5)}},
	}
	pick, ok := SelectBestPartial(cands)
	if !ok {
		t.Fatal("expected a pick")
	}
	if pick.Nights != 2 {
		t.Fatalf("want 2 nights, got %d", pick.Nights)
	}
	if !pick.CheckIn.Equal(d(2026, 9, 1)) {
		t.Fatalf("want checkin 09-01, got %v", pick.CheckIn)
	}
}

func TestSelectBestPartial_Empty(t *testing.T) {
	_, ok := SelectBestPartial(nil)
	if ok {
		t.Fatal("expected no pick")
	}
	_, ok = SelectBestPartial([]Candidate{{CampsiteID: "A"}})
	if ok {
		t.Fatal("expected no pick for empty dates")
	}
}

func TestSelectBestPartial_SingleNightOK(t *testing.T) {
	cands := []Candidate{{CampsiteID: "A", Dates: []time.Time{d(2026, 9, 1)}}}
	pick, ok := SelectBestPartial(cands)
	if !ok || pick.Nights != 1 {
		t.Fatalf("expected 1-night pick, got %+v ok=%v", pick, ok)
	}
}
