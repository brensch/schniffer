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
		endpoints:         eps,
		rlCooldown:        5 * time.Minute,
		cooldown:          time.Minute,
		endpointBad:       map[string]time.Time{},
		endpointThrottled: map[string]time.Time{},
		stats:             map[string]*EndpointStat{},
		statsSince:        time.Now(),
	}
}

func TestPickSkipsHostThrottledEndpoint(t *testing.T) {
	p := newTestPool("a", "b")
	host := "recreation.gov"

	// Throttle "a" for recreation.gov only.
	p.throttle(Endpoint{URL: "a", Region: "region-a"}, host)

	// Every pick for that host must avoid "a" while the cooldown holds.
	for i := 0; i < 10; i++ {
		ep, ok := p.pick(map[string]bool{}, host)
		if !ok {
			t.Fatal("pick returned no endpoint")
		}
		if ep.URL == "a" {
			t.Fatalf("pick returned throttled endpoint 'a' for host %s", host)
		}
	}

	// A different host is unaffected — "a" should still be reachable.
	sawA := false
	for i := 0; i < 10; i++ {
		ep, _ := p.pick(map[string]bool{}, "reservecalifornia.com")
		if ep.URL == "a" {
			sawA = true
			break
		}
	}
	if !sawA {
		t.Fatal("endpoint 'a' should remain available for a different host")
	}
}

func TestPickFallsBackWhenAllThrottled(t *testing.T) {
	p := newTestPool("a", "b")
	host := "recreation.gov"
	p.throttle(Endpoint{URL: "a"}, host)
	p.throttle(Endpoint{URL: "b"}, host)

	// Both throttled: pick must still return something rather than fail,
	// so requests degrade instead of dropping.
	ep, ok := p.pick(map[string]bool{}, host)
	if !ok {
		t.Fatal("pick should fall back to a throttled endpoint, not fail")
	}
	if ep.URL != "a" && ep.URL != "b" {
		t.Fatalf("unexpected endpoint %q", ep.URL)
	}
}

func TestThrottleExpires(t *testing.T) {
	p := newTestPool("a", "b")
	host := "recreation.gov"
	// Set an already-expired throttle directly.
	p.endpointThrottled[badKey("a", host)] = time.Now().Add(-time.Second)

	sawA := false
	for i := 0; i < 10; i++ {
		ep, _ := p.pick(map[string]bool{}, host)
		if ep.URL == "a" {
			sawA = true
			break
		}
	}
	if !sawA {
		t.Fatal("expired throttle should let 'a' back into rotation")
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
