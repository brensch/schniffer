package manager

import (
	"testing"
	"time"

	"github.com/brensch/schniffer/internal/db"
)

// day is a tiny helper for readable UTC midnight dates.
func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// TestGenerateNights_Fencepost pins down the [checkin, checkout) per-night
// semantics: checkin is the day of arrival (you sleep that night), checkout
// is the day of departure (you do NOT sleep that night).
//
// The motivating bug report: a user with a Fri->Sun request worried they were
// matching a Thu->Fri (Thursday-night-only) campground availability. With
// these semantics, Fri->Sun generates nights {Fri, Sat} and the lone Thursday
// night cannot match.
func TestGenerateNights_Fencepost(t *testing.T) {
	thu := day(2025, 8, 14)
	fri := day(2025, 8, 15)
	sat := day(2025, 8, 16)
	sun := day(2025, 8, 17)

	cases := []struct {
		name     string
		checkin  time.Time
		checkout time.Time
		want     []time.Time
	}{
		{"single-night Thu->Fri", thu, fri, []time.Time{thu}},
		{"single-night Fri->Sat", fri, sat, []time.Time{fri}},
		{"two-night Fri->Sun (Fri+Sat)", fri, sun, []time.Time{fri, sat}},
		{"three-night Thu->Sun", thu, sun, []time.Time{thu, fri, sat}},
		{"zero range Fri->Fri yields no nights", fri, fri, nil},
		{"inverted Sun->Fri yields no nights", sun, fri, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := generateNights(tc.checkin, tc.checkout)
			if len(got) != len(tc.want) {
				t.Fatalf("nights len = %d, want %d (got=%v want=%v)", len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if !got[i].Equal(tc.want[i]) {
					t.Fatalf("night[%d] = %s, want %s", i, got[i].Format("2006-01-02"), tc.want[i].Format("2006-01-02"))
				}
			}
		})
	}
}

// TestGenerateNights_ThuFriDoesNotOverlapFriSun is the explicit case from the
// bug report: a Thursday-night-only campground stay must not be considered
// part of a Friday-arrival, Sunday-departure request.
func TestGenerateNights_ThuFriDoesNotOverlapFriSun(t *testing.T) {
	thu := day(2025, 8, 14)
	fri := day(2025, 8, 15)
	sun := day(2025, 8, 17)

	requested := generateNights(fri, sun) // {Fri, Sat}
	campground := generateNights(thu, fri) // {Thu}

	for _, want := range requested {
		for _, have := range campground {
			if want.Equal(have) {
				t.Fatalf("unexpected overlap on %s: Thu-only campground availability must not match Fri->Sun request",
					want.Format("2006-01-02"))
			}
		}
	}
}

// TestCollectDatesByPC_FencepostFiltering verifies that the per-(provider,
// campground) date set the polling loop builds from active requests respects
// per-night semantics and drops malformed requests where checkout <= checkin.
func TestCollectDatesByPC_FencepostFiltering(t *testing.T) {
	fri := day(2025, 8, 15)
	sat := day(2025, 8, 16)
	sun := day(2025, 8, 17)

	reqs := []db.SchniffRequest{
		// Fri->Sun => nights Fri, Sat
		{ID: 1, Provider: "p", CampgroundID: "cg", Checkin: fri, Checkout: sun, Active: true},
		// Same-day, must be ignored.
		{ID: 2, Provider: "p", CampgroundID: "cg", Checkin: fri, Checkout: fri, Active: true},
		// Inverted, must be ignored.
		{ID: 3, Provider: "p", CampgroundID: "cg", Checkin: sun, Checkout: fri, Active: true},
	}

	dates, byPC := collectDatesByPC(reqs)
	key := pc{prov: "p", cg: "cg"}

	set := dates[key]
	if len(set) != 2 {
		t.Fatalf("expected 2 unique nights, got %d (%v)", len(set), set)
	}
	if _, ok := set[fri]; !ok {
		t.Errorf("expected Fri in night set")
	}
	if _, ok := set[sat]; !ok {
		t.Errorf("expected Sat in night set")
	}
	if _, ok := set[sun]; ok {
		t.Errorf("checkout day (Sun) must not appear as a night")
	}

	// Only the valid request should be retained per-PC.
	if len(byPC[key]) != 1 || byPC[key][0].ID != 1 {
		t.Errorf("expected only request 1 retained, got %+v", byPC[key])
	}
}
