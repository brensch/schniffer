package booker

import (
	"sort"
	"time"
)

// Candidate is the input to the selection algorithm: one campsite that is
// newly available, the dates that opened up within the user's window, and
// its rating (0 if unknown).
type Candidate struct {
	CampsiteID string
	Dates      []time.Time // dates within user's window, any order
	Rating     float64
}

// Pick is the selection algorithm's output for a (user, campground) batch.
// CheckIn..CheckOut is the longest contiguous date span available on the
// chosen campsite, clipped to the user's window. Always book this — even a
// 1-night overlap is worth holding (user decides at checkout).
type Pick struct {
	CampsiteID string
	CheckIn    time.Time // night to arrive
	CheckOut   time.Time // morning to depart (= last night + 1 day)
	Nights     int
	Rating     float64
}

// SelectBestPartial picks the campsite + contiguous night stretch with the
// most nights, breaking ties by Rating (descending), then by CampsiteID for
// stability. Returns ok=false if there are no candidates with at least one
// date.
//
// The selection algorithm intentionally does NOT require the stretch to
// cover the entire user window — partial overlaps are still booked because
// the user can choose to ignore them at checkout.
func SelectBestPartial(candidates []Candidate) (Pick, bool) {
	type scored struct {
		Pick
	}
	var best Pick
	var have bool
	for _, c := range candidates {
		if len(c.Dates) == 0 {
			continue
		}
		in, out, nights := longestContiguous(c.Dates)
		if nights == 0 {
			continue
		}
		cand := Pick{
			CampsiteID: c.CampsiteID,
			CheckIn:    in,
			CheckOut:   out,
			Nights:     nights,
			Rating:     c.Rating,
		}
		if !have || better(cand, best) {
			best = cand
			have = true
		}
	}
	return best, have
}

// better returns true if a is preferable to b: more nights wins; ties broken
// by higher rating, then by lower CampsiteID lexically.
func better(a, b Pick) bool {
	if a.Nights != b.Nights {
		return a.Nights > b.Nights
	}
	if a.Rating != b.Rating {
		return a.Rating > b.Rating
	}
	return a.CampsiteID < b.CampsiteID
}

// longestContiguous returns the start date (inclusive), end date (exclusive
// = last_date + 1 day), and night count for the longest run of consecutive
// dates in the input. Dates are deduplicated and sorted internally.
func longestContiguous(dates []time.Time) (time.Time, time.Time, int) {
	if len(dates) == 0 {
		return time.Time{}, time.Time{}, 0
	}
	uniq := make([]time.Time, 0, len(dates))
	seen := map[string]bool{}
	for _, d := range dates {
		d = d.UTC().Truncate(24 * time.Hour)
		k := d.Format("2006-01-02")
		if seen[k] {
			continue
		}
		seen[k] = true
		uniq = append(uniq, d)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].Before(uniq[j]) })

	var bestStart, bestEnd time.Time
	bestLen := 0
	runStart := uniq[0]
	runLen := 1
	for i := 1; i <= len(uniq); i++ {
		if i < len(uniq) && uniq[i].Sub(uniq[i-1]) == 24*time.Hour {
			runLen++
			continue
		}
		if runLen > bestLen {
			bestLen = runLen
			bestStart = runStart
			bestEnd = uniq[i-1].AddDate(0, 0, 1)
		}
		if i < len(uniq) {
			runStart = uniq[i]
			runLen = 1
		}
	}
	return bestStart, bestEnd, bestLen
}
