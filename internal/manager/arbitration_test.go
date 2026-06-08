package manager

import (
	"testing"
	"time"

	"github.com/brensch/schniffer/internal/booker"
	"github.com/brensch/schniffer/internal/db"
)

func arbDay(d int) time.Time {
	return time.Date(2026, 7, d, 0, 0, 0, 0, time.UTC)
}

func mkReq(id int64, userID string) db.SchniffRequest {
	return db.SchniffRequest{
		ID:        id,
		UserID:    userID,
		Provider:  "recreation_gov",
		Checkin:   arbDay(1),
		Checkout:  arbDay(10),
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestArbitrate_OldestWinsSingleSite(t *testing.T) {
	a := mkReq(1, "alice")
	b := mkReq(2, "bob")
	cand := []booker.Candidate{{CampsiteID: "X", Dates: []time.Time{arbDay(1), arbDay(2)}}}
	res := Arbitrate([]ArbInput{{a, cand}, {b, cand}}, nil)
	if !res[0].Won {
		t.Fatal("alice should win — lower request_id")
	}
	if res[1].Won {
		t.Fatal("bob should not win")
	}
	if len(res[1].DisplacedBy) != 1 || res[1].DisplacedBy[0].WinnerUserID != "alice" {
		t.Fatalf("bob should be displaced by alice, got %+v", res[1].DisplacedBy)
	}
}

func TestArbitrate_LongerBookingWinsEvenIfNewer(t *testing.T) {
	a := mkReq(1, "alice")
	b := mkReq(2, "bob")
	// Same site X. Alice's pool gives 1 night, Bob's gives 5 nights.
	res := Arbitrate([]ArbInput{
		{a, []booker.Candidate{{CampsiteID: "X", Dates: []time.Time{arbDay(1)}}}},
		{b, []booker.Candidate{{CampsiteID: "X", Dates: []time.Time{arbDay(1), arbDay(2), arbDay(3), arbDay(4), arbDay(5)}}}},
	}, nil)
	if !res[1].Won {
		t.Fatal("bob should win — longer booking trumps request_id")
	}
	if res[0].Won {
		t.Fatal("alice should not win")
	}
	if len(res[0].DisplacedBy) != 1 || res[0].DisplacedBy[0].WinnerUserID != "bob" {
		t.Fatalf("alice should be displaced by bob")
	}
}

func TestArbitrate_DistributeOneEach(t *testing.T) {
	a := mkReq(1, "alice")
	b := mkReq(2, "bob")
	candAB := []booker.Candidate{
		{CampsiteID: "X", Dates: []time.Time{arbDay(1), arbDay(2)}},
		{CampsiteID: "Y", Dates: []time.Time{arbDay(1), arbDay(2)}},
	}
	res := Arbitrate([]ArbInput{{a, candAB}, {b, candAB}}, nil)
	if !res[0].Won || !res[1].Won {
		t.Fatalf("both should win different sites, got %+v", res)
	}
	if res[0].Pick.CampsiteID == res[1].Pick.CampsiteID {
		t.Fatalf("alice and bob got same site %s", res[0].Pick.CampsiteID)
	}
	if len(res[0].DisplacedBy) != 0 || len(res[1].DisplacedBy) != 0 {
		t.Fatalf("nobody should be displaced when each got a site")
	}
}

func TestArbitrate_ExpiringOwnerExcluded(t *testing.T) {
	a := mkReq(1, "alice")
	b := mkReq(2, "bob")
	cand := []booker.Candidate{{CampsiteID: "X", Dates: []time.Time{arbDay(1), arbDay(2)}}}
	// Alice's hold on X just expired — she can't auto-book it again.
	res := Arbitrate([]ArbInput{{a, cand}, {b, cand}}, map[string]db.RecentHoldOwner{"X": {CampsiteID: "X", UserID: "alice", Checkin: arbDay(1), Checkout: arbDay(3)}})
	if res[0].Won {
		t.Fatal("alice should not win — expiring owner excluded")
	}
	if !res[0].ExpiringOwner {
		t.Fatal("alice should be classified as expiring owner")
	}
	if !res[1].Won {
		t.Fatal("bob should win since alice is excluded")
	}
}

func TestArbitrate_ExpiringOwnerStillWinsNonOverlappingDates(t *testing.T) {
	// Alice's just-expired hold on X was for July 1-3. She has a separate
	// schniff for July 8-9 on the same site X — that's a different trip,
	// not a re-attempt of the dates she let go. She should win it.
	a := mkReq(1, "alice")
	cand := []booker.Candidate{{CampsiteID: "X", Dates: []time.Time{arbDay(8), arbDay(9)}}}
	holds := map[string]db.RecentHoldOwner{"X": {CampsiteID: "X", UserID: "alice", Checkin: arbDay(1), Checkout: arbDay(3)}}
	res := Arbitrate([]ArbInput{{a, cand}}, holds)
	if !res[0].Won {
		t.Fatal("alice should win X for non-overlapping dates")
	}
	if res[0].ExpiringOwner {
		t.Fatal("not flagged as expiring owner since dates do not overlap")
	}
}

func TestArbitrate_ExpiringOwnerStillWinsDifferentSite(t *testing.T) {
	a := mkReq(1, "alice")
	cand := []booker.Candidate{
		{CampsiteID: "X", Dates: []time.Time{arbDay(1), arbDay(2)}}, // alice held this
		{CampsiteID: "Y", Dates: []time.Time{arbDay(1), arbDay(2)}}, // alice can have this
	}
	res := Arbitrate([]ArbInput{{a, cand}}, map[string]db.RecentHoldOwner{"X": {CampsiteID: "X", UserID: "alice", Checkin: arbDay(1), Checkout: arbDay(3)}})
	if !res[0].Won {
		t.Fatal("alice should win site Y")
	}
	if res[0].Pick.CampsiteID != "Y" {
		t.Fatalf("alice should win Y not %s", res[0].Pick.CampsiteID)
	}
	if !res[0].ExpiringOwner {
		t.Fatal("alice still flagged as expiring owner (gets the basket-expired DM for X)")
	}
}

func TestArbitrate_StrictDisplacementOnlyForLosers(t *testing.T) {
	a := mkReq(1, "alice")
	b := mkReq(2, "bob")
	// 2 sites, alice wins X; bob loses X but wins Y. Bob should NOT see X
	// in his displacement list (he won something else).
	candA := []booker.Candidate{
		{CampsiteID: "X", Dates: []time.Time{arbDay(1), arbDay(2), arbDay(3), arbDay(4)}},
	}
	candB := []booker.Candidate{
		{CampsiteID: "X", Dates: []time.Time{arbDay(1), arbDay(2)}},
		{CampsiteID: "Y", Dates: []time.Time{arbDay(1), arbDay(2)}},
	}
	res := Arbitrate([]ArbInput{{a, candA}, {b, candB}}, nil)
	if !res[0].Won || res[0].Pick.CampsiteID != "X" {
		t.Fatalf("alice should win X with 4 nights, got %+v", res[0])
	}
	if !res[1].Won || res[1].Pick.CampsiteID != "Y" {
		t.Fatalf("bob should win Y (X taken), got %+v", res[1])
	}
	if len(res[1].DisplacedBy) != 0 {
		t.Fatalf("bob already won Y so should not be DM'd about losing X")
	}
}

func TestArbitrate_TieBrokenByRequestID(t *testing.T) {
	a := mkReq(5, "alice")
	b := mkReq(3, "bob") // bob has lower id despite later in slice
	cand := []booker.Candidate{{CampsiteID: "X", Dates: []time.Time{arbDay(1), arbDay(2)}}}
	res := Arbitrate([]ArbInput{{a, cand}, {b, cand}}, nil)
	if !res[1].Won {
		t.Fatal("bob should win on lower request_id tiebreak")
	}
}
