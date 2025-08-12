package providers

import (
	"testing"
	"time"
)

func TestReserveCaliforniaPlanBuckets(t *testing.T) {
	r := NewReserveCalifornia()
	d1 := time.Date(2025, 8, 12, 13, 0, 0, 0, time.UTC)
	d2 := time.Date(2025, 8, 15, 0, 0, 0, 0, time.UTC)
	b := r.PlanBuckets([]time.Time{d2, d1})
	if len(b) != 1 {
		t.Fatalf("expected one bucket, got %d", len(b))
	}
	if !b[0].Start.Equal(time.Date(2025, 8, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected start: %v", b[0].Start)
	}
	if !b[0].End.Equal(time.Date(2025, 8, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected end: %v", b[0].End)
	}
}
