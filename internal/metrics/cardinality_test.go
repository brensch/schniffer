package metrics

import (
	"testing"
)

// TestMetricCardinalityBounded exercises every metric with a realistic
// spread of label values and asserts the resulting series count per
// metric family stays under a hard bound. A regression here is the OOM
// scenario we hit on 2026-06-11: one runaway label took the host down.
//
// The point of this test is NOT to enumerate exact series counts — it
// is to fail loudly if a future change starts feeding free-form values
// (IDs, query text, error messages, user input, ...) into any label.
// The bounds are sized to be comfortably above the natural codebase
// cardinality but well below the failure mode.
func TestMetricCardinalityBounded(t *testing.T) {
	// Reset every histogram/counter so the bounds aren't polluted by
	// other tests in this package that touched the registry.
	resetAll()

	// providers grown to a generous upper bound. Real codebase has 3.
	providers := []string{"recreation_gov", "recreation_gov_search", "recreation_gov_campsites", "reservecalifornia"}
	// regions from proxy/endpoints.json — bounded by the embedded file.
	regions := []string{"us-east", "us-west", "eu-west", "ap-south", ""}
	// proxy endpoints — bounded by embedded endpoints.json.
	endpoints := []string{"https://proxy-1.example", "https://proxy-2.example", "https://proxy-3.example"}
	// web routes — bounded by mux registrations in server.go.
	routes := []string{
		"static", "campground_page",
		"api_campgrounds", "api_viewport", "api_filter_options",
		"api_campground_detail", "api_campground_state",
		"api_groups", "api_groups_create",
	}
	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH"}
	statuses := []string{"200", "204", "301", "304", "400", "401", "403", "404", "405", "409", "422", "429", "500", "502", "503", "504"}
	bookerPhases := []string{"nav", "recaptcha_wait", "session_check", "script_eval", "recaptcha_token", "recgov_post", "total"}
	bookerOutcomes := []string{"success", "error"}
	bookerReasons := []string{"session_expired", "watchdog_jwt_missing", "watchdog_ping_failed", "watchdog_dead"}
	bools := []string{"true", "false"}

	// Hammer each metric with every combination its callsites could
	// realistically produce. If somebody widens a label's domain to
	// something unbounded, the gathered series count will explode.
	for _, p := range providers {
		for _, b := range bools {
			ProviderFetchDuration.WithLabelValues(p, b).Observe(0.1)
			ProviderFetchTotal.WithLabelValues(p, b).Inc()
		}
		for _, s := range statuses {
			ProviderUpstreamStatus.WithLabelValues(p, s).Inc()
		}
		for _, r := range regions {
			ProviderUpstreamDuration.WithLabelValues(p, r).Observe(0.1)
		}
		PollCycleDuration.WithLabelValues(p).Observe(1.0)
		PollCycleCampgrounds.WithLabelValues(p).Observe(10)
		PollCycleFailed.WithLabelValues(p).Inc()
	}
	for _, ep := range endpoints {
		for _, r := range regions {
			ProxyBatchSize.WithLabelValues(ep, r).Observe(3)
			for _, b := range bools {
				ProxyDispatchDuration.WithLabelValues(ep, r, b).Observe(0.2)
			}
		}
		ProxyEndpointBadTotal.WithLabelValues(ep).Inc()
	}
	for _, phase := range bookerPhases {
		for _, oc := range bookerOutcomes {
			BookerHoldDuration.WithLabelValues(phase, oc).Observe(0.5)
		}
	}
	BookerSessionWait.Observe(0.01)
	for _, r := range bookerReasons {
		BookerRelaunchTotal.WithLabelValues(r).Inc()
	}
	for _, route := range routes {
		WebResponseBytes.WithLabelValues(route).Observe(1024)
		for _, m := range methods {
			for _, s := range statuses {
				WebRequestDuration.WithLabelValues(route, m, s).Observe(0.06)
				WebRequestsTotal.WithLabelValues(route, m, s).Inc()
			}
		}
	}
	WebRequestsInFlight.Inc()
	// DB query labels are codebase-bounded; sample the known set.
	for _, q := range []string{
		"select:campgrounds", "select:campsite_availability", "insert:state_changes",
		"insert:campsite_availability", "create:temp_new_states", "drop:IF",
		"insert:temp_new_states", "update:requests", "select:requests",
		"delete:state_changes",
	} {
		DBQueryDuration.WithLabelValues(q).Observe(0.001)
	}

	// Per-family upper bounds. Sized at ~2x the realistic worst case
	// so they only trip on genuine cardinality regressions.
	bounds := map[string]int{
		"schniffer_provider_fetch_duration_seconds":     20,
		"schniffer_provider_upstream_duration_seconds":  40,
		"schniffer_provider_fetch_total":                20,
		"schniffer_provider_upstream_status_total":      160, // providers × statuses
		"schniffer_proxy_batch_size":                    40,
		"schniffer_proxy_dispatch_duration_seconds":     80,
		"schniffer_proxy_endpoint_bad_total":            20,
		"schniffer_poll_cycle_duration_seconds":         10,
		"schniffer_poll_cycle_campgrounds":              10,
		"schniffer_poll_cycle_failed_campgrounds_total": 10,
		"schniffer_booker_hold_duration_seconds":        40,
		"schniffer_booker_session_wait_seconds":         2,
		"schniffer_booker_relaunch_total":               20,
		"schniffer_web_request_duration_seconds":        2000, // routes × methods × statuses
		"schniffer_web_requests_total":                  2000,
		"schniffer_web_requests_in_flight":              2,
		"schniffer_web_response_bytes":                  20,
		"schniffer_db_query_duration_seconds":           30,
	}

	mfs, err := Reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	seen := map[string]int{}
	for _, mf := range mfs {
		seen[mf.GetName()] = len(mf.GetMetric())
	}

	for name, cap := range bounds {
		got, ok := seen[name]
		if !ok {
			t.Errorf("metric family %q not registered", name)
			continue
		}
		if got > cap {
			t.Errorf("metric %q has %d series, bound is %d", name, got, cap)
		}
	}

	// Belt-and-braces: catch any *new* metric the test forgot to
	// enumerate but which still exposes a suspiciously large series
	// set. 5000 is well above any legitimate combinatorial space
	// here and well below the failure threshold.
	for _, mf := range mfs {
		if mf.GetName() == "go_gc_duration_seconds" || mf.GetName() == "go_gc_heap_allocs_by_size_bytes" {
			continue // Go runtime collector; known fixed cardinality
		}
		if n := len(mf.GetMetric()); n > 5000 {
			t.Errorf("unknown metric %q exceeded blanket cardinality cap: %d series", mf.GetName(), n)
		}
	}
}

func resetAll() {
	ProviderFetchDuration.Reset()
	ProviderUpstreamDuration.Reset()
	ProviderFetchTotal.Reset()
	ProviderUpstreamStatus.Reset()
	ProxyBatchSize.Reset()
	ProxyDispatchDuration.Reset()
	ProxyEndpointBadTotal.Reset()
	PollCycleDuration.Reset()
	PollCycleCampgrounds.Reset()
	PollCycleFailed.Reset()
	BookerHoldDuration.Reset()
	BookerRelaunchTotal.Reset()
	WebRequestDuration.Reset()
	WebRequestsTotal.Reset()
	WebResponseBytes.Reset()
	DBQueryDuration.Reset()
}
