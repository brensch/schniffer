// booker-warmtab-test exercises the per-schniff warm-tab path end-to-end
// against real rec.gov so we can confirm:
//
//  1. Pool.EnsureWarmTabForRequest opens + navigates a dedicated tab on
//     the campground page.
//  2. Pool.HoldOnRequestTab hits the fast path (HoldFast) for one campsite,
//     and crucially ALSO hits the fast path for a DIFFERENT campsite in
//     the same campground — no re-navigation required because the tab is
//     parked at the campground level.
//  3. CloseRequestTab leaves the underlying session healthy for a
//     follow-up call that uses the main tab (HoldCampsite fallback).
//
// Picks two fresh (site, date) tuples from Baker Dam to avoid colliding
// with the user's accumulated cart holds. Output is the booking_phase log
// stream — grep on correlation_id to read one booking at a time.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/booker"
)

func main() {
	var (
		campground = flag.String("campground", "10085599", "test campground ID (Baker Dam default)")
		monthStart = flag.String("month", "2026-09-01", "month YYYY-MM-01")
		nights     = flag.Int("nights", 1, "nights per hold")
		profileDir = flag.String("profile", "", "Chrome profile dir (default ~/.cache/recgov-booker)")
		chromePath = flag.String("chrome", "", "Chrome binary")
	)
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	email := os.Getenv("REC_GOV_EMAIL")
	password := os.Getenv("REC_GOV_PASSWORD")
	if email == "" || password == "" {
		fatal("REC_GOV_EMAIL and REC_GOV_PASSWORD must be set")
	}
	if *profileDir == "" {
		home, _ := os.UserHomeDir()
		*profileDir = filepath.Join(home, ".cache", "recgov-booker")
	}

	month, err := time.Parse("2006-01-02", *monthStart)
	if err != nil {
		fatal("bad -month: %v", err)
	}
	slots, err := fetchAvailableSlots(context.Background(), *campground, month)
	if err != nil {
		fatal("availability: %v", err)
	}
	if len(slots) < 4 {
		fatal("need at least 4 slots; got %d", len(slots))
	}
	rand.Shuffle(len(slots), func(i, j int) { slots[i], slots[j] = slots[j], slots[i] })

	siteA := slots[0]
	var siteB slotInfo
	for _, s := range slots[1:] {
		if s.siteID != siteA.siteID {
			siteB = s
			break
		}
	}
	if siteB.siteID == "" {
		fatal("could not find two distinct sites with availability")
	}
	slog.Info("selected test sites", "site_a", siteA.siteID, "site_b", siteB.siteID)

	const fakeUserID = "warmtab-test"
	pool := booker.NewPool(booker.PoolConfig{
		BaseProfileDir: filepath.Dir(*profileDir),
		ChromePath:     *chromePath,
		LookupCredential: func(ctx context.Context, _ string) (string, string, error) {
			return email, password, nil
		},
		Logger: slog.Default(),
	})
	if err := pool.StartUser(context.Background(), fakeUserID); err != nil {
		fatal("pool warmup: %v", err)
	}
	defer pool.Close()
	slog.Info("pool warm")

	const reqID = int64(424242)

	// Step 1: ensure warm tab on the CAMPGROUND page (not a campsite).
	ectx, ecancel := context.WithTimeout(context.Background(), 60*time.Second)
	if err := pool.EnsureWarmTabForRequest(ectx, fakeUserID, reqID, *campground); err != nil {
		ecancel()
		fatal("ensure warm tab on campground: %v", err)
	}
	ecancel()
	slog.Info("=== step 1 ok: warm tab on campground page ===", "campground", *campground)

	// Step 2: HoldOnRequestTab for siteA → fast path.
	bctx, bcancel := context.WithTimeout(context.Background(), 60*time.Second)
	res, err := pool.HoldOnRequestTab(bctx, fakeUserID, reqID, siteA.siteID, *campground, siteA.date, siteA.date.AddDate(0, 0, *nights))
	bcancel()
	slog.Info("=== step 2 result (siteA on warm tab) ===",
		"path", pathOf(res), "wall", wallOf(res), "err", errString(err), "order", orderID(res))

	// Step 3: HoldOnRequestTab for siteB → ALSO fast path (no re-nav).
	// This is the critical assertion: the warm tab is on the campground
	// page, so it serves any campsite in the campground equally well.
	bctx2, bcancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	res2, err2 := pool.HoldOnRequestTab(bctx2, fakeUserID, reqID, siteB.siteID, *campground, siteB.date, siteB.date.AddDate(0, 0, *nights))
	bcancel2()
	slog.Info("=== step 3 result (siteB on SAME warm tab) ===",
		"path", pathOf(res2), "wall", wallOf(res2), "err", errString(err2), "order", orderID(res2))
	if pathOf(res2) != "warm_tab_fast" {
		slog.Error("REGRESSION: step 3 took the slow path despite warm tab being parked on the campground",
			"path", pathOf(res2))
	}

	// Step 4: close the warm tab.
	pool.CloseRequestTab(fakeUserID, reqID)
	slog.Info("=== step 4 ok: warm tab closed ===")

	// Step 5: a fallback HoldOnRequestTab without warm tab still works.
	bctx3, bcancel3 := context.WithTimeout(context.Background(), 90*time.Second)
	res3, err3 := pool.HoldOnRequestTab(bctx3, fakeUserID, reqID, siteA.siteID, *campground, siteA.date.AddDate(0, 0, 7), siteA.date.AddDate(0, 0, 7+*nights))
	bcancel3()
	slog.Info("=== step 5 result (fallback path; no warm tab) ===",
		"path", pathOf(res3), "wall", wallOf(res3), "err", errString(err3), "order", orderID(res3))
}

func pathOf(r *booker.HoldResult) string {
	if r == nil {
		return ""
	}
	return string(r.Path)
}

func wallOf(r *booker.HoldResult) time.Duration {
	if r == nil {
		return 0
	}
	return r.Timings.Total
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func orderID(r *booker.HoldResult) string {
	if r == nil {
		return ""
	}
	return r.OrderID
}

type slotInfo struct {
	siteID string
	date   time.Time
}

func fetchAvailableSlots(ctx context.Context, cg string, month time.Time) ([]slotInfo, error) {
	base, _ := url.Parse(fmt.Sprintf("https://www.recreation.gov/api/camps/availability/campground/%s/month", cg))
	q := base.Query()
	q.Set("start_date", month.UTC().Format("2006-01-02T15:04:05.000Z"))
	base.RawQuery = q.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 booker-warmtab-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	var parsed struct {
		Campsites map[string]struct {
			Availabilities map[string]string `json:"availabilities"`
		} `json:"campsites"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	var out []slotInfo
	for siteID, sd := range parsed.Campsites {
		for dateStr, status := range sd.Availabilities {
			if status != "Available" {
				continue
			}
			d, err := time.Parse(time.RFC3339, dateStr)
			if err != nil {
				continue
			}
			out = append(out, slotInfo{siteID: siteID, date: d})
		}
	}
	return out, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+strings.TrimRight(format, "\n")+"\n", args...)
	os.Exit(1)
}
