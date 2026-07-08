package proxypool

import (
	"strings"
	"testing"
	"time"
)

func TestFormatReportNoTraffic(t *testing.T) {
	now := time.Now()
	msg := FormatReport(nil, now.Add(-24*time.Hour), now)
	if !strings.Contains(msg, "No proxy traffic") {
		t.Fatalf("expected no-traffic message, got: %q", msg)
	}
}

func TestFormatReportCleanRun(t *testing.T) {
	now := time.Now()
	stats := []EndpointStat{
		{URL: "a", Region: "us-central1", Requests: 100},
		{URL: "b", Region: "us-west1", Requests: 80},
	}
	msg := FormatReport(stats, now.Add(-24*time.Hour), now)
	if !strings.Contains(msg, "No individual IP hit a 429 or 403") {
		t.Fatalf("expected clean-run message, got: %q", msg)
	}
	if !strings.Contains(msg, "0.0% throttled") {
		t.Fatalf("expected 0%% throttled summary, got: %q", msg)
	}
}

func TestFormatReportOffendersSortedAndCounted(t *testing.T) {
	now := time.Now()
	stats := []EndpointStat{
		{URL: "a", Region: "us-central1", Requests: 100, RateLimited: 2, Forbidden: 1, Cooldowns: 1},
		{URL: "b", Region: "asia-east1", Requests: 50, RateLimited: 40, Forbidden: 5, Cooldowns: 9},
		{URL: "c", Region: "europe-west1", Requests: 70},
	}
	msg := FormatReport(stats, now.Add(-24*time.Hour), now)

	// Totals: 220 reqs, 42 rate-limited, 6 blocked => 48/220 = 21.8%.
	if !strings.Contains(msg, "21.8% throttled") {
		t.Fatalf("expected 21.8%% overall, got: %q", msg)
	}
	// The worst offender (asia-east1, 45 hits) must be listed before the
	// lesser one (us-central1, 3 hits).
	iAsia := strings.Index(msg, "asia-east1")
	iCentral := strings.Index(msg, "us-central1")
	if iAsia < 0 || iCentral < 0 {
		t.Fatalf("both offenders should appear, got: %q", msg)
	}
	if iAsia > iCentral {
		t.Fatalf("asia-east1 should be listed before us-central1, got: %q", msg)
	}
	// The clean endpoint (europe-west1) is not an offender and shouldn't
	// appear in the per-IP table.
	if strings.Contains(msg, "europe-west1") {
		t.Fatalf("clean endpoint should be omitted from offender table, got: %q", msg)
	}
}

func TestHasRateLimiting(t *testing.T) {
	if HasRateLimiting([]EndpointStat{{Requests: 100}}) {
		t.Fatal("clean stats should report no rate limiting")
	}
	if !HasRateLimiting([]EndpointStat{{Requests: 100, Forbidden: 1}}) {
		t.Fatal("a 403 should count as rate limiting")
	}
	if !HasRateLimiting([]EndpointStat{{Requests: 100, RateLimited: 1}}) {
		t.Fatal("a 429 should count as rate limiting")
	}
}
