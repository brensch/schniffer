package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMetricsRegistration verifies every metric family is registered and
// scraping /metrics produces a parsable Prometheus exposition. Also touches
// each metric with a sample observation so we catch label-arity bugs at
// test time rather than under production load.
func TestMetricsRegistration(t *testing.T) {
	// Exercise every label tuple used by the rest of the codebase. If a
	// caller passes the wrong number of label values, WithLabelValues
	// panics — this would surface it.
	ProviderFetchDuration.WithLabelValues("recreation_gov", "true").Observe(0.123)
	ProviderUpstreamDuration.WithLabelValues("recreation_gov", "us-east").Observe(0.05)
	ProviderFetchTotal.WithLabelValues("recreation_gov", "true").Inc()
	ProxyBatchSize.WithLabelValues("http://example", "us-east").Observe(5)
	ProxyDispatchDuration.WithLabelValues("http://example", "us-east", "true").Observe(0.1)
	ProxyEndpointBadTotal.WithLabelValues("http://example").Inc()
	PollCycleDuration.WithLabelValues("recreation_gov").Observe(1.5)
	PollCycleCampgrounds.WithLabelValues("recreation_gov").Observe(12)
	PollCycleFailed.WithLabelValues("recreation_gov").Inc()
	for _, phase := range []string{"nav", "recaptcha_wait", "session_check", "script_eval", "recaptcha_token", "recgov_post", "total"} {
		BookerHoldDuration.WithLabelValues(phase, "success").Observe(0.5)
	}
	BookerSessionWait.Observe(0.01)
	BookerRelaunchTotal.WithLabelValues("session_expired").Inc()
	WebRequestDuration.WithLabelValues("api_viewport", "POST", "200").Observe(0.06)
	WebRequestsTotal.WithLabelValues("api_viewport", "POST", "200").Inc()
	WebRequestsInFlight.Inc()
	WebRequestsInFlight.Dec()
	WebResponseBytes.WithLabelValues("api_viewport").Observe(1024)
	DBQueryDuration.WithLabelValues("select:campgrounds").Observe(0.002)

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	expected := []string{
		"schniffer_provider_fetch_duration_seconds",
		"schniffer_provider_upstream_duration_seconds",
		"schniffer_provider_fetch_total",
		"schniffer_proxy_batch_size",
		"schniffer_proxy_dispatch_duration_seconds",
		"schniffer_proxy_endpoint_bad_total",
		"schniffer_poll_cycle_duration_seconds",
		"schniffer_poll_cycle_campgrounds",
		"schniffer_poll_cycle_failed_campgrounds_total",
		"schniffer_booker_hold_duration_seconds",
		"schniffer_booker_session_wait_seconds",
		"schniffer_booker_relaunch_total",
		"schniffer_web_request_duration_seconds",
		"schniffer_web_requests_total",
		"schniffer_web_requests_in_flight",
		"schniffer_web_response_bytes",
		"schniffer_db_query_duration_seconds",
		// Go + process collectors should be present.
		"go_goroutines",
		"process_cpu_seconds_total",
	}
	for _, name := range expected {
		if !strings.Contains(out, name) {
			t.Errorf("metric %q not present in /metrics output", name)
		}
	}
}

func TestBoolLabel(t *testing.T) {
	if BoolLabel(true) != "true" || BoolLabel(false) != "false" {
		t.Fatal("BoolLabel did not return canonical labels")
	}
}

func TestSeconds(t *testing.T) {
	start := time.Now().Add(-500 * time.Millisecond)
	got := Seconds(start)
	if got < 0.4 || got > 1.0 {
		t.Fatalf("Seconds returned implausible value: %v", got)
	}
}
