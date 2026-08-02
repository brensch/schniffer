package manager

import (
	"strings"
	"testing"
	"time"
)

func TestDiskTransition(t *testing.T) {
	cases := []struct {
		name      string
		prev      diskState
		freePct   float64
		sinceLast time.Duration
		wantState diskState
		wantMsg   string // required substring; "" means silence
	}{
		{"healthy stays silent", diskOK, 60, time.Hour, diskOK, ""},
		{"crossing warn alerts", diskOK, 14, time.Hour, diskWarn, "⚠️"},
		{"crossing crit alerts", diskWarn, 9, time.Minute, diskCrit, "🚨"},
		{"straight to crit alerts", diskOK, 5, time.Minute, diskCrit, "🚨"},
		{"warn holds quiet before reminder", diskWarn, 14, time.Hour, diskWarn, ""},
		{"warn re-alerts after a day", diskWarn, 14, 25 * time.Hour, diskWarn, "⚠️"},
		{"crit re-alerts after a day", diskCrit, 8, 25 * time.Hour, diskCrit, "🚨"},
		{"crit improving to warn is quiet", diskCrit, 12, time.Minute, diskWarn, ""},
		{"deadband holds warn quietly", diskWarn, 16, 25 * time.Hour, diskWarn, ""},
		{"recovery announces once", diskWarn, 20, time.Minute, diskOK, "✅"},
		{"recovered stays silent", diskOK, 20, time.Minute, diskOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := diskTransition(tc.prev, tc.freePct, 10, tc.sinceLast)
			if got != tc.wantState {
				t.Fatalf("state = %v, want %v", got, tc.wantState)
			}
			if tc.wantMsg == "" && msg != "" {
				t.Fatalf("expected silence, got %q", msg)
			}
			if tc.wantMsg != "" && !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("msg %q missing %q", msg, tc.wantMsg)
			}
		})
	}
}
