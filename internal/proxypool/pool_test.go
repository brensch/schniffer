package proxypool

import (
	"net/http"
	"testing"
	"time"
)

// newTestPool builds a Pool with the given endpoint URLs directly, skipping
// the embedded endpoints.json so tests control the rotation set.
func newTestPool(urls ...string) *Pool {
	eps := make([]Endpoint, len(urls))
	for i, u := range urls {
		eps[i] = Endpoint{URL: u, Provider: "test", Region: "region-" + u}
	}
	return &Pool{
		endpoints:      eps,
		cooldown:       time.Minute,
		endpointBad:    map[string]time.Time{},
		endpointHealth: map[string]*epHealth{},
		stats:          map[string]*EndpointStat{},
		statsSince:     time.Now(),
	}
}

func TestPickSkipsBackingOffEndpoint(t *testing.T) {
	p := newTestPool("a", "b")
	target := "recreation_gov"

	// Back "a" off for recreation_gov only.
	p.onFailure(Endpoint{URL: "a", Region: "region-a"}, target)

	// Every pick for that target must avoid "a" while the backoff holds.
	for i := 0; i < 10; i++ {
		ep, ok := p.pick(map[string]bool{}, target)
		if !ok {
			t.Fatal("pick returned no endpoint")
		}
		if ep.URL == "a" {
			t.Fatalf("pick returned backed-off endpoint 'a' for target %s", target)
		}
	}

	// A different target is unaffected — "a" should still be reachable.
	sawA := false
	for i := 0; i < 10; i++ {
		ep, _ := p.pick(map[string]bool{}, "reservecalifornia")
		if ep.URL == "a" {
			sawA = true
			break
		}
	}
	if !sawA {
		t.Fatal("endpoint 'a' should remain available for a different target")
	}
}

func TestBackoffEscalatesAndSuccessResets(t *testing.T) {
	p := newTestPool("a", "b")
	target := "reservecalifornia"
	ep := Endpoint{URL: "a", Region: "region-a"}

	p.onFailure(ep, target)
	if got := p.endpointHealth[badKey("a", target)].failLevel; got != 1 {
		t.Fatalf("first failure should be level 1, got %d", got)
	}
	p.onFailure(ep, target)
	h := p.endpointHealth[badKey("a", target)]
	if h.failLevel != 2 {
		t.Fatalf("second failure should escalate to level 2, got %d", h.failLevel)
	}
	if h.nextRetry.Sub(h.lastFail) != backoffFor(2) {
		t.Fatalf("backoff should match ladder level 2 (%v)", backoffFor(2))
	}
	// A success clears the backoff entirely.
	p.onSuccess(ep, target)
	if _, ok := p.endpointHealth[badKey("a", target)]; ok {
		t.Fatal("success should delete the backoff entry (healthy again)")
	}
}

func TestPickFallsBackWhenAllBackedOff(t *testing.T) {
	p := newTestPool("a", "b")
	target := "recreation_gov"
	p.onFailure(Endpoint{URL: "a"}, target)
	p.onFailure(Endpoint{URL: "b"}, target)

	// Both backed off: pick must still return something rather than fail.
	ep, ok := p.pick(map[string]bool{}, target)
	if !ok {
		t.Fatal("pick should fall back to a backed-off endpoint, not fail")
	}
	if ep.URL != "a" && ep.URL != "b" {
		t.Fatalf("unexpected endpoint %q", ep.URL)
	}
}

func TestBackoffExpires(t *testing.T) {
	p := newTestPool("a", "b")
	target := "recreation_gov"
	// Set an already-expired backoff directly.
	p.endpointHealth[badKey("a", target)] = &epHealth{failLevel: 1, nextRetry: time.Now().Add(-time.Second)}

	sawA := false
	for i := 0; i < 10; i++ {
		ep, _ := p.pick(map[string]bool{}, target)
		if ep.URL == "a" {
			sawA = true
			break
		}
	}
	if !sawA {
		t.Fatal("expired backoff should let 'a' back into rotation")
	}
}

func TestHealthByTarget(t *testing.T) {
	p := newTestPool("a", "b", "c")
	target := "reservecalifornia"
	// Give the target some traffic so it shows up, and block "a".
	p.recordResult(target, Endpoint{URL: "a", Region: "region-a"}, wireResp{Status: 403})
	p.recordResult(target, Endpoint{URL: "b", Region: "region-b"}, wireResp{Status: 200})
	for i := 0; i < len(backoffLadder); i++ {
		p.onFailure(Endpoint{URL: "a", Region: "region-a"}, target)
	}
	rows, ok := p.HealthByTarget()[target]
	if !ok || len(rows) != 3 {
		t.Fatalf("want 3 IP rows for target, got %d", len(rows))
	}
	// Worst-first: "a" is blocked and should be first.
	if rows[0].Region != "region-a" || rows[0].State != "blocked" {
		t.Fatalf("expected region-a blocked first, got %+v", rows[0])
	}
}

func TestDrainStatsResets(t *testing.T) {
	p := newTestPool("a")
	ep := Endpoint{URL: "a", Region: "region-a"}
	// 200 is clean; 429, 403, and a transport error all count as failed.
	p.recordResult("recreation_gov", ep, wireResp{Status: http.StatusOK})
	p.recordResult("recreation_gov", ep, wireResp{Status: http.StatusTooManyRequests})
	p.recordResult("recreation_gov", ep, wireResp{Status: http.StatusForbidden})
	p.recordResult("recreation_gov", ep, wireResp{Error: "dial timeout"})

	stats, _ := p.DrainStats()
	if len(stats) != 1 {
		t.Fatalf("want 1 endpoint stat, got %d", len(stats))
	}
	s := stats[0]
	if s.Target != "recreation_gov" || s.Requests != 4 || s.Failed != 3 {
		t.Fatalf("unexpected tally: %+v", s)
	}

	// Second drain is empty — the window reset.
	stats2, _ := p.DrainStats()
	if len(stats2) != 0 {
		t.Fatalf("want empty stats after drain, got %d", len(stats2))
	}
}

func TestRecordResultKeysByTarget(t *testing.T) {
	p := newTestPool("a")
	ep := Endpoint{URL: "a", Region: "region-a"}
	// Same endpoint, two targets → two distinct rows.
	p.recordResult("recreation_gov", ep, wireResp{Status: 200})
	p.recordResult("reservecalifornia", ep, wireResp{Status: 403})

	stats, _ := p.Snapshot()
	if len(stats) != 2 {
		t.Fatalf("want 2 rows (one per target), got %d: %+v", len(stats), stats)
	}
}
