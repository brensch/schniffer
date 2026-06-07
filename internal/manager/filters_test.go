package manager

import (
	"database/sql"
	"testing"
	"time"

	"github.com/brensch/schniffer/internal/db"
)

func parseDay(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestRequestConditionsMet_NoFilters(t *testing.T) {
	req := db.SchniffRequest{Checkin: parseDay("2026-05-12"), Checkout: parseDay("2026-05-15")}
	if !requestConditionsMet(req, nil) {
		t.Fatal("no filters should always pass even with empty availability")
	}
}

func TestRequestConditionsMet_MinimumNights(t *testing.T) {
	req := db.SchniffRequest{
		Checkin:       parseDay("2026-05-12"),
		Checkout:      parseDay("2026-05-15"),
		MinimumNights: sql.NullInt64{Int64: 3, Valid: true},
	}
	// One campsite with only 2 consecutive nights → fails.
	avail := []db.AvailabilityItem{
		{CampsiteID: "A", Date: parseDay("2026-05-12")},
		{CampsiteID: "A", Date: parseDay("2026-05-13")},
	}
	if requestConditionsMet(req, avail) {
		t.Fatal("2 consecutive nights should not satisfy min_nights=3")
	}
	// Same campsite gets the 3rd consecutive night → passes.
	avail = append(avail, db.AvailabilityItem{CampsiteID: "A", Date: parseDay("2026-05-14")})
	if !requestConditionsMet(req, avail) {
		t.Fatal("3 consecutive nights should satisfy min_nights=3")
	}
	// Split across two campsites → still fails (must be a single campsite).
	avail = []db.AvailabilityItem{
		{CampsiteID: "A", Date: parseDay("2026-05-12")},
		{CampsiteID: "A", Date: parseDay("2026-05-13")},
		{CampsiteID: "B", Date: parseDay("2026-05-14")},
	}
	if requestConditionsMet(req, avail) {
		t.Fatal("availability split across campsites should not satisfy min_nights=3")
	}
}

func TestRequestConditionsMet_FullWeekend(t *testing.T) {
	// 2026-05-15 is a Friday.
	req := db.SchniffRequest{
		Checkin:  parseDay("2026-05-15"),
		Checkout: parseDay("2026-05-18"),
		Strategy: sql.NullString{String: db.StrategyFullWeekend, Valid: true},
	}
	// Only Saturday → fails.
	avail := []db.AvailabilityItem{{CampsiteID: "A", Date: parseDay("2026-05-16")}}
	if requestConditionsMet(req, avail) {
		t.Fatal("Saturday only should not satisfy full_weekend")
	}
	// Fri + Sat on same campsite → passes.
	avail = []db.AvailabilityItem{
		{CampsiteID: "A", Date: parseDay("2026-05-15")},
		{CampsiteID: "A", Date: parseDay("2026-05-16")},
	}
	if !requestConditionsMet(req, avail) {
		t.Fatal("Fri+Sat should satisfy full_weekend")
	}
	// Fri and Sat split across campsites → fails (per-campsite check).
	avail = []db.AvailabilityItem{
		{CampsiteID: "A", Date: parseDay("2026-05-15")},
		{CampsiteID: "B", Date: parseDay("2026-05-16")},
	}
	if requestConditionsMet(req, avail) {
		t.Fatal("Fri+Sat split across campsites should not satisfy full_weekend")
	}
}
