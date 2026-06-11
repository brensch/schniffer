package manager

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/providers"
)

// fakeProvider records call timings and sleeps for a fixed duration per
// FetchAvailability invocation so we can prove parallelism by wall time.
type fakeProvider struct {
	name      string
	latency   time.Duration
	inFlight  atomic.Int32
	peak      atomic.Int32
	callCount atomic.Int32
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) FetchAvailability(ctx context.Context, campgroundID string, start, end time.Time) ([]providers.CampsiteAvailability, error) {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	f.callCount.Add(1)
	select {
	case <-time.After(f.latency):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}
func (f *fakeProvider) FetchAllCampgrounds(context.Context) ([]providers.CampgroundInfo, error) {
	return nil, nil
}
func (f *fakeProvider) FetchCampsites(context.Context, string) ([]providers.CampsiteInfo, error) {
	return nil, nil
}
func (f *fakeProvider) CampsiteURL(string, string) string { return "" }
func (f *fakeProvider) CampgroundURL(string) string       { return "" }
func (f *fakeProvider) PlanBuckets(d []time.Time) []providers.DateRange {
	if len(d) == 0 {
		return nil
	}
	return []providers.DateRange{{Start: d[0], End: d[len(d)-1]}}
}

func newTestStore(t *testing.T) *db.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestPollProviderCampgroundsRunConcurrently asserts that PollProvider fans
// out FetchAvailability across many campgrounds concurrently rather than
// running them serially. With 20 campgrounds each "taking" 100ms, a serial
// implementation would take 2s; the parallel implementation should finish
// in well under 500ms.
func TestPollProviderCampgroundsRunConcurrently(t *testing.T) {
	store := newTestStore(t)
	const fakeLatency = 100 * time.Millisecond
	const numCampgrounds = 20

	prov := &fakeProvider{name: "fake", latency: fakeLatency}
	reg := providers.NewRegistry()
	reg.Register(prov.Name(), prov)

	m := &Manager{
		store:  store,
		reg:    reg,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx := context.Background()
	checkin := time.Now().Add(24 * time.Hour).UTC().Truncate(24 * time.Hour)
	checkout := checkin.Add(48 * time.Hour)
	for i := range numCampgrounds {
		_, err := store.DB.ExecContext(ctx, `
			INSERT INTO schniff_requests(user_id, provider, campground_id, checkin, checkout, active)
			VALUES (?, ?, ?, ?, ?, 1)
		`, "u1", prov.Name(), itoa(i), checkin, checkout)
		if err != nil {
			t.Fatalf("insert request: %v", err)
		}
	}

	start := time.Now()
	if err := m.PollProvider(ctx, prov.Name()); err != nil {
		t.Fatalf("PollProvider: %v", err)
	}
	elapsed := time.Since(start)

	calls := prov.callCount.Load()
	peak := prov.peak.Load()

	if calls != int32(numCampgrounds) {
		t.Errorf("expected %d FetchAvailability calls, got %d", numCampgrounds, calls)
	}
	if peak < 2 {
		t.Errorf("expected peak concurrency > 1, got %d (loop is serial)", peak)
	}
	// A serial impl would take numCampgrounds * fakeLatency = 2s. Allow generous slack.
	maxAllowed := 5 * fakeLatency
	if elapsed > maxAllowed {
		t.Errorf("PollProvider took %v with %d concurrent fakes at %v each; expected < %v",
			elapsed, numCampgrounds, fakeLatency, maxAllowed)
	}
	t.Logf("PollProvider elapsed=%v peak_concurrency=%d calls=%d", elapsed, peak, calls)
}

// flakyProvider returns an error for the first call only; later calls succeed.
type flakyProvider struct {
	fakeProvider
	failOnce atomic.Bool
}

func (f *flakyProvider) FetchAvailability(ctx context.Context, campgroundID string, start, end time.Time) ([]providers.CampsiteAvailability, error) {
	if f.failOnce.CompareAndSwap(false, true) {
		return nil, errFakeUpstream
	}
	return f.fakeProvider.FetchAvailability(ctx, campgroundID, start, end)
}

var errFakeUpstream = errSentinel("fake upstream broke")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// TestPollProviderContinuesPastErrors asserts that a single failed
// FetchAvailability doesn't abort the rest of the cycle's campgrounds.
func TestPollProviderContinuesPastErrors(t *testing.T) {
	store := newTestStore(t)
	const numCampgrounds = 10

	prov := &flakyProvider{fakeProvider: fakeProvider{name: "flaky", latency: 20 * time.Millisecond}}
	reg := providers.NewRegistry()
	reg.Register(prov.Name(), prov)

	m := &Manager{
		store:  store,
		reg:    reg,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx := context.Background()
	checkin := time.Now().Add(24 * time.Hour).UTC().Truncate(24 * time.Hour)
	checkout := checkin.Add(48 * time.Hour)
	for i := range numCampgrounds {
		_, err := store.DB.ExecContext(ctx, `
			INSERT INTO schniff_requests(user_id, provider, campground_id, checkin, checkout, active)
			VALUES (?, ?, ?, ?, ?, 1)
		`, "u1", prov.Name(), itoa(i), checkin, checkout)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	if err := m.PollProvider(ctx, prov.Name()); err != nil {
		t.Fatalf("PollProvider should not return error for single failure: %v", err)
	}
	// 10 campgrounds, 1 failure expected, 9 successes — but the failed one's
	// goroutine still attempted, so callCount on the embedded fakeProvider
	// counts only successful calls (flaky bypasses the embedded path on failure).
	if got := prov.callCount.Load(); got != numCampgrounds-1 {
		t.Errorf("expected %d successful fetches, got %d", numCampgrounds-1, got)
	}
}

// itoa is a tiny no-strconv helper for test ids — keeps the file dep-free.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [16]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
