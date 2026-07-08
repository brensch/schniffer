package proxypool

import (
	"fmt"
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

func TestFormatReportGroupsByProvider(t *testing.T) {
	now := time.Now()
	stats := []EndpointStat{
		{Target: "recreation_gov", URL: "a", Region: "us-central1", Requests: 8100, Failed: 9},
		{Target: "recreation_gov", URL: "b", Region: "asia-east1", Requests: 512, Failed: 215},
		{Target: "reservecalifornia", URL: "a", Region: "us-west1", Requests: 120, Failed: 72},
	}
	msg := FormatReport(stats, now.Add(-3*time.Hour), now)

	// One section per provider.
	if !strings.Contains(msg, "**recreation_gov**") || !strings.Contains(msg, "**reservecalifornia**") {
		t.Fatalf("expected a section per provider, got:\n%s", msg)
	}
	// reservecalifornia (60% failed) is worse than recreation_gov (2.6%),
	// so it should be listed first.
	if strings.Index(msg, "reservecalifornia") > strings.Index(msg, "recreation_gov") {
		t.Fatalf("worse provider should be first, got:\n%s", msg)
	}
	// Both successes and failures are shown, without 403/429 jargon.
	if !strings.Contains(msg, "ok /") || !strings.Contains(msg, "failed") {
		t.Fatalf("expected ok/failed split, got:\n%s", msg)
	}
	if strings.Contains(msg, "blocked") || strings.Contains(msg, "429") || strings.Contains(msg, "403") {
		t.Fatalf("should not mention blocked/429/403, got:\n%s", msg)
	}
	// Thousands separators.
	if !strings.Contains(msg, "8,100") {
		t.Fatalf("expected comma-formatted counts, got:\n%s", msg)
	}
}

func TestFormatReportShowsSuccesses(t *testing.T) {
	now := time.Now()
	// One failing IP (throttled after one attempt) plus many healthy IPs —
	// the report must surface the successes, not just the 1/1 failure.
	stats := []EndpointStat{
		{Target: "reservecalifornia", URL: "a", Region: "us-west1", Requests: 1, Failed: 1},
		{Target: "reservecalifornia", URL: "b", Region: "asia-east1", Requests: 40, Failed: 0},
		{Target: "reservecalifornia", URL: "c", Region: "me-west1", Requests: 35, Failed: 0},
	}
	msg := FormatReport(stats, now.Add(-time.Hour), now)
	// Provider header shows the ok count.
	if !strings.Contains(msg, "75 ok / 1 failed") {
		t.Fatalf("expected explicit ok count, got:\n%s", msg)
	}
	// Healthy IPs are rolled up so successes are visible.
	if !strings.Contains(msg, "2 other IPs healthy — 75 requests, no failures") {
		t.Fatalf("expected healthy-IP rollup, got:\n%s", msg)
	}
	// The failing IP is shown as a count, not a scary 100%.
	if !strings.Contains(msg, "us-west1 — 1 failed of 1 request") {
		t.Fatalf("expected count-based failing line, got:\n%s", msg)
	}
	if strings.Contains(msg, "100%") {
		t.Fatalf("per-IP lines should not show a standalone rate, got:\n%s", msg)
	}
}

func TestFormatReportHealthyProvider(t *testing.T) {
	now := time.Now()
	stats := []EndpointStat{
		{Target: "recreation_gov", URL: "a", Region: "us-central1", Requests: 100, Failed: 0},
		{Target: "recreation_gov", URL: "b", Region: "us-west1", Requests: 60, Failed: 0},
	}
	msg := FormatReport(stats, now.Add(-time.Hour), now)
	if !strings.Contains(msg, "160 ok / 0 failed (0.0% of 160)") {
		t.Fatalf("expected clean summary, got:\n%s", msg)
	}
	if !strings.Contains(msg, "all IPs healthy") {
		t.Fatalf("expected all-healthy note, got:\n%s", msg)
	}
}

func TestFormatReportTruncatesManyFailingIPs(t *testing.T) {
	now := time.Now()
	var stats []EndpointStat
	for i := 0; i < 15; i++ {
		stats = append(stats, EndpointStat{
			Target:   "recreation_gov",
			URL:      fmt.Sprintf("u%d", i),
			Region:   fmt.Sprintf("region-%02d", i),
			Requests: 100,
			Failed:   50,
		})
	}
	msg := FormatReport(stats, now.Add(-time.Hour), now)
	// 15 failing IPs, cap 10 → note the remaining 5.
	if !strings.Contains(msg, "…and 5 more IPs with failures") {
		t.Fatalf("expected truncation note for 5 extra IPs, got:\n%s", msg)
	}
}

func TestHasFailures(t *testing.T) {
	if HasFailures([]EndpointStat{{Requests: 100}}) {
		t.Fatal("clean stats should report no failures")
	}
	if !HasFailures([]EndpointStat{{Requests: 100, Failed: 1}}) {
		t.Fatal("a failed request should be detected")
	}
}
