package manager

import (
	"sort"

	"github.com/brensch/schniffer/internal/booker"
	"github.com/brensch/schniffer/internal/db"
)

// ArbInput is the per-request input to arbitration: a schniff request and
// its already-filtered candidate pool (newly-available campsites with the
// dates that fall in the user's window, after minimum_nights / strategy
// gating).
type ArbInput struct {
	Request    db.SchniffRequest
	Candidates []booker.Candidate
}

// DisplacementRecord describes one site that this request had eligible but
// lost to an older (lower request_id) competitor.
type DisplacementRecord struct {
	CampsiteID   string
	WinnerUserID string
	WinnerReqID  int64
	WinnerNights int
}

// ArbResult is the per-request output of arbitration.
type ArbResult struct {
	Request db.SchniffRequest
	// Won: this request was assigned a campsite via arbitration. Auto-book
	// attempts go out for these.
	Won  bool
	Pick booker.Pick

	// ExpiringOwner: this request's user is the most-recent holder of a
	// campsite in their candidate pool, within the recent-hold window. They
	// are excluded from auto-booking that site (would be an infinite loop)
	// and get a special "basket expired" follow-up DM instead. If the same
	// user could have won other sites via arbitration, Won + Pick still
	// reflect that.
	ExpiringOwner       bool
	ExpiringOwnerPick   booker.Pick // the site they had held, for the follow-up DM
	ExpiringOwnerHadCap bool        // there was at least one date overlap

	// DisplacedBy: campsites this request's user could have used but lost to
	// other users. Populated only when Won == false and at least one of the
	// candidates was awarded to a different user. Used for the "older
	// schniff won" summary DM, sent after winner holds succeed.
	DisplacedBy []DisplacementRecord
}

// pickForRequest applies the right selector for a request: full_weekend
// schniffs book only the first Fri+Sat pair, everything else takes the
// longest contiguous run.
func pickForRequest(req db.SchniffRequest, candidates []booker.Candidate) (booker.Pick, bool) {
	if req.Strategy.Valid && req.Strategy.String == db.StrategyFullWeekend {
		return booker.SelectFirstWeekend(candidates)
	}
	return booker.SelectBestPartial(candidates)
}

// Arbitrate runs greedy global assignment over (request, campsite) pairs.
// At each step the highest-scoring still-eligible pair wins: more nights →
// higher rating → lower request_id. Once a campsite is taken or a request
// already has a win, both drop out of the pool.
//
// A pair where the request's user is the expiringOwners[campsite] (their
// own hold just expired on that site) is excluded — we never auto-book the
// same user the same site they just let go.
func Arbitrate(inputs []ArbInput, expiringOwners map[string]string) []ArbResult {
	type pairScore struct {
		reqIdx int
		pick   booker.Pick
	}
	var pairs []pairScore
	for i, in := range inputs {
		for _, c := range in.Candidates {
			if owner, ok := expiringOwners[c.CampsiteID]; ok && owner == in.Request.UserID {
				continue
			}
			pick, ok := pickForRequest(in.Request, []booker.Candidate{c})
			if !ok {
				continue
			}
			pairs = append(pairs, pairScore{reqIdx: i, pick: pick})
		}
	}

	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].pick.Nights != pairs[j].pick.Nights {
			return pairs[i].pick.Nights > pairs[j].pick.Nights
		}
		if pairs[i].pick.Rating != pairs[j].pick.Rating {
			return pairs[i].pick.Rating > pairs[j].pick.Rating
		}
		return inputs[pairs[i].reqIdx].Request.ID < inputs[pairs[j].reqIdx].Request.ID
	})

	wonByReq := map[int]booker.Pick{}
	wonBySite := map[string]int{}
	for _, p := range pairs {
		if _, taken := wonBySite[p.pick.CampsiteID]; taken {
			continue
		}
		if _, alreadyWon := wonByReq[p.reqIdx]; alreadyWon {
			continue
		}
		wonByReq[p.reqIdx] = p.pick
		wonBySite[p.pick.CampsiteID] = p.reqIdx
	}

	results := make([]ArbResult, len(inputs))
	for i, in := range inputs {
		r := ArbResult{Request: in.Request}
		if pick, ok := wonByReq[i]; ok {
			r.Won = true
			r.Pick = pick
		}

		// ExpiringOwner: did this user have one of their own recently-held
		// sites in their candidate pool?
		for _, c := range in.Candidates {
			if owner, ok := expiringOwners[c.CampsiteID]; ok && owner == in.Request.UserID {
				pick, hasDates := pickForRequest(in.Request, []booker.Candidate{c})
				r.ExpiringOwner = true
				r.ExpiringOwnerHadCap = hasDates
				if hasDates {
					r.ExpiringOwnerPick = pick
				} else {
					r.ExpiringOwnerPick = booker.Pick{CampsiteID: c.CampsiteID}
				}
				break
			}
		}

		// Displacement (strict): only report displacements for requests that
		// walked away empty-handed (no Won). A request that won a different
		// site doesn't get spammed about sites it didn't get.
		if !r.Won {
			for _, c := range in.Candidates {
				winnerIdx, taken := wonBySite[c.CampsiteID]
				if !taken {
					continue
				}
				if winnerIdx == i {
					continue
				}
				winnerReq := inputs[winnerIdx].Request
				if winnerReq.ID == in.Request.ID {
					continue
				}
				nights := 0
				if w, ok := wonByReq[winnerIdx]; ok && w.CampsiteID == c.CampsiteID {
					nights = w.Nights
				}
				r.DisplacedBy = append(r.DisplacedBy, DisplacementRecord{
					CampsiteID:   c.CampsiteID,
					WinnerUserID: winnerReq.UserID,
					WinnerReqID:  winnerReq.ID,
					WinnerNights: nights,
				})
			}
		}

		results[i] = r
	}
	return results
}
