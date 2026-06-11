package db

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brensch/schniffer/internal/metrics"
	dto "github.com/prometheus/client_model/go"
)

// TestNormalizeQueryLabelBounded checks the regex-strip defence against
// dynamic identifier suffixes. A regression here would put us right back
// in the OOM scenario where every upsert spawned a fresh `query` label
// value.
func TestNormalizeQueryLabelBounded(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"uuid_temp_table_create", "CREATE TEMP TABLE temp_new_states_0000043692b7402281033f346b135c2f (a INT);", "create:temp_new_states"},
		{"uuid_temp_table_insert", "INSERT INTO temp_new_states_abcdef0123456789 (a) VALUES (?);", "insert:temp_new_states"},
		{"uuid_temp_table_select", "SELECT * FROM temp_new_states_deadbeefdeadbeef AS ns;", "select:temp_new_states"},
		{"plain_table", "SELECT * FROM campsite_availability WHERE x=1", "select:campsite_availability"},
		{"empty", "", "unknown"},
		{"upsert", "INSERT INTO campsite_availability VALUES (?)", "insert:campsite_availability"},
		{"hex_short_kept", "SELECT 1 FROM t_abc", "select:t_abc"}, // <8 hex chars: not stripped
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeQueryLabel(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeQueryLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > 64 {
				t.Fatalf("label %q exceeds 64-char cap", got)
			}
		})
	}
}

// TestUpsertCampsiteAvailabilityBoundedLabels is the regression test for
// the OOM incident on 2026-06-11. The previous implementation used a
// UUID-suffixed TEMP table per call which leaked unbounded `query` label
// values into schniffer_db_query_duration_seconds. After 1d 21h of
// production traffic the metric grew to 212k unique label values and
// took down the host. This test fakes ~500 calls and asserts the label
// set never grows beyond what the codebase's distinct SQL produces.
func TestUpsertCampsiteAvailabilityBoundedLabels(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "card.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Reset histogram so we start clean (other tests in this package
	// may have touched it).
	metrics.DBQueryDuration.Reset()

	ctx := context.Background()
	now := time.Now()
	const calls = 500
	for i := 0; i < calls; i++ {
		states := []CampsiteAvailability{
			{
				Provider:     "recreation_gov",
				CampgroundID: fmt.Sprintf("cg-%d", i%17),
				CampsiteID:   fmt.Sprintf("cs-%d", i),
				Date:         now.AddDate(0, 0, i%30),
				Available:    i%2 == 0,
				LastChecked:  now,
			},
		}
		if err := store.UpsertCampsiteAvailabilityBatch(ctx, states); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	labels := collectDBQueryLabels(t)
	// 500 calls × ~5 distinct SQL statements per call (drop, create,
	// insert, state_changes, upsert) leaves us with at most a handful
	// of label values. A regression would push this into the hundreds.
	const cap = 15
	if len(labels) > cap {
		t.Fatalf("db_query label cardinality = %d (>%d) after %d calls; values: %v", len(labels), cap, calls, labels)
	}
	// And none of those labels should contain a UUID-shaped suffix.
	for l := range labels {
		if strings.Contains(l, "temp_new_states_") && len(l) > len("insert:temp_new_states_") {
			t.Fatalf("label %q still embeds dynamic suffix", l)
		}
	}
	t.Logf("after %d calls, db_query label values: %v", calls, labels)
}

// collectDBQueryLabels reads schniffer_db_query_duration_seconds out of
// the registry and returns the set of `query` label values currently
// recorded.
func collectDBQueryLabels(t *testing.T) map[string]struct{} {
	t.Helper()
	mfs, err := metrics.Reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]struct{}{}
	for _, mf := range mfs {
		if mf.GetName() != "schniffer_db_query_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "query" {
					out[lp.GetValue()] = struct{}{}
				}
			}
		}
	}
	return out
}

// silence unused-import if metric type ever changes.
var _ = (*dto.MetricFamily)(nil)
