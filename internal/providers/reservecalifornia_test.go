package providers

import (
	"testing"
	"time"
)

func TestReserveCaliforniaPlanBuckets(t *testing.T) {
	r := NewReserveCalifornia()
	d1 := time.Date(2025, 8, 12, 13, 0, 0, 0, time.UTC)
	d2 := time.Date(2025, 8, 15, 0, 0, 0, 0, time.UTC)
	b := r.PlanBuckets([]time.Time{d2, d1})
	if len(b) != 1 {
		t.Fatalf("expected one bucket, got %d", len(b))
	}
	if !b[0].Start.Equal(time.Date(2025, 8, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected start: %v", b[0].Start)
	}
	if !b[0].End.Equal(time.Date(2025, 8, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected end: %v", b[0].End)
	}
}

// Guards the site route format. The old "/Web/#!park/x/y" hash links started
// 404ing when ReserveCalifornia moved to a React app served from /.
func TestReserveCaliforniaURLs(t *testing.T) {
	r := NewReserveCalifornia()

	const want = "https://www.reservecalifornia.com/park/713/787"
	if got := r.CampgroundURL("713-787"); got != want {
		t.Errorf("CampgroundURL = %q, want %q", got, want)
	}
	if got := r.CampsiteURL("713-787", "49638"); got != want {
		t.Errorf("CampsiteURL = %q, want %q", got, want)
	}

	checkin := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	const wantStay = "https://www.reservecalifornia.com/park/713/787?date=2026-09-04&night=3"
	if got := r.CampgroundURLForStay("713-787", checkin, 3); got != wantStay {
		t.Errorf("CampgroundURLForStay = %q, want %q", got, wantStay)
	}

	// Missing dates degrade to the plain campground link rather than emitting
	// query params the site would choke on.
	if got := r.CampgroundURLForStay("713-787", time.Time{}, 0); got != want {
		t.Errorf("CampgroundURLForStay(zero) = %q, want %q", got, want)
	}

	// Unexpected ID shapes fall back to the site root, not a broken path.
	const wantRoot = "https://www.reservecalifornia.com/"
	if got := r.CampgroundURL("713"); got != wantRoot {
		t.Errorf("CampgroundURL(bad id) = %q, want %q", got, wantRoot)
	}
	if got := r.CampgroundURLForStay("713", checkin, 3); got != wantRoot {
		t.Errorf("CampgroundURLForStay(bad id) = %q, want %q", got, wantRoot)
	}
}

// The notification embed reaches for this via a type assertion; if the method
// signature drifts the links silently lose their dates.
func TestReserveCaliforniaImplementsStayURLProvider(t *testing.T) {
	var _ StayURLProvider = NewReserveCalifornia()
}
