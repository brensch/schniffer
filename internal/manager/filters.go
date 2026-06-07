package manager

import (
	"sort"
	"time"

	"github.com/brensch/schniffer/internal/db"
)

// requestConditionsMet reports whether the currently-available campsites
// satisfy the optional filters attached to the schniff request
// (minimum_nights, strategy). A request with neither filter set always
// passes.
//
// allAvailable should be the currently-available campsite/date pairs inside
// the request's [Checkin, Checkout) window.
func requestConditionsMet(req db.SchniffRequest, allAvailable []db.AvailabilityItem) bool {
	hasMin := req.MinimumNights.Valid && req.MinimumNights.Int64 > 0
	hasStrat := req.Strategy.Valid && req.Strategy.String != ""
	if !hasMin && !hasStrat {
		return true
	}

	// Group dates by campsite, normalized to day boundaries.
	byCampsite := map[string][]time.Time{}
	for _, it := range allAvailable {
		d := normalizeDay(it.Date)
		if d.Before(normalizeDay(req.Checkin)) || !d.Before(normalizeDay(req.Checkout)) {
			continue
		}
		byCampsite[it.CampsiteID] = append(byCampsite[it.CampsiteID], d)
	}

	for _, dates := range byCampsite {
		sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
		if hasMin && !hasContiguousRun(dates, int(req.MinimumNights.Int64)) {
			continue
		}
		if hasStrat && !campsiteMeetsStrategy(dates, req.Strategy.String) {
			continue
		}
		return true
	}
	return false
}

// hasContiguousRun reports whether dates (sorted ascending, deduped or not)
// contains a run of at least n consecutive days.
func hasContiguousRun(dates []time.Time, n int) bool {
	if n <= 0 {
		return true
	}
	if len(dates) < n {
		return false
	}
	run := 1
	for i := 1; i < len(dates); i++ {
		switch {
		case dates[i].Equal(dates[i-1]):
			// duplicate; ignore
		case dates[i].Equal(dates[i-1].AddDate(0, 0, 1)):
			run++
			if run >= n {
				return true
			}
		default:
			run = 1
		}
	}
	return run >= n
}

// campsiteMeetsStrategy applies the named strategy filter to a sorted set of
// available nights for a single campsite. Unknown strategies fail closed.
func campsiteMeetsStrategy(dates []time.Time, strategy string) bool {
	switch strategy {
	case db.StrategyFullWeekend:
		// Need a Friday night (Fri) immediately followed by Saturday night
		// (Sat) both available on the same campsite.
		set := map[time.Time]bool{}
		for _, d := range dates {
			set[d] = true
		}
		for d := range set {
			if d.Weekday() == time.Friday && set[d.AddDate(0, 0, 1)] {
				return true
			}
		}
		return false
	default:
		return false
	}
}
