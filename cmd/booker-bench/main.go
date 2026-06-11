// booker-bench measures different chromedp booking strategies against
// rec.gov so we can identify where wall-clock time is spent and how much
// of it is pre-payable via warm tabs.
//
// Scenarios:
//
//	cold:   one tab, full Session.HoldCampsite each iteration (current prod).
//	warm1:  one tab, PrewarmCampsite once, then HoldFast each iteration.
//	warmN:  N tabs, each prewarmed on a different campsite, HoldFast per tab.
//
// Each iteration consumes a fresh (site, date) pair so the 15-min auto-expiring
// holds never collide. By default the test target is Baker Dam (10085599) — 18
// sites, all open July 2026 (probed live before running).
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
	"sort"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/booker"
)

func main() {
	var (
		campground = flag.String("campground", "10085599", "test campground ID (default Baker Dam)")
		monthStart = flag.String("month", "2026-07-01", "month to pull availability from (YYYY-MM-01)")
		nights     = flag.Int("nights", 1, "nights per hold")
		iter       = flag.Int("iter", 8, "iterations per scenario")
		tabs       = flag.Int("tabs", 3, "tabs for warmN scenario")
		scenarios  = flag.String("scenarios", "cold,warm1,warmN", "comma-separated subset of {cold,warm1,warmN}")
		profileDir = flag.String("profile", "", "Chrome user-data-dir (default ~/.cache/recgov-booker)")
		chromePath = flag.String("chrome", "", "chrome binary (default autodetect)")
		debugDir   = flag.String("debug-dir", "/tmp/booker-bench", "debug dump dir")
		pause      = flag.Duration("pause", 1*time.Second, "pause between iterations")
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
	_ = os.MkdirAll(*debugDir, 0o755)

	// Step 1: pull available (site, date) tuples.
	month, err := time.Parse("2006-01-02", *monthStart)
	if err != nil {
		fatal("bad -month: %v", err)
	}
	pool, err := fetchAvailableSlots(context.Background(), *campground, month)
	if err != nil {
		fatal("fetch availability: %v", err)
	}
	if len(pool) == 0 {
		fatal("no available (site,date) slots for cg %s in %s", *campground, *monthStart)
	}
	slog.Info("availability fetched", "slots", len(pool))
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	// Step 2: open Chrome + log in.
	sess, err := booker.Open(booker.Config{ProfileDir: *profileDir, ChromePath: *chromePath})
	if err != nil {
		fatal("open chrome: %v", err)
	}
	defer sess.Close()
	loginCtx, loginCancel := context.WithTimeout(sess.Ctx(), 90*time.Second)
	if err := sess.Login(loginCtx, email, password); err != nil {
		loginCancel()
		fatal("login: %v", err)
	}
	loginCancel()
	slog.Info("logged in")

	// Step 3: run scenarios.
	want := map[string]bool{}
	for _, s := range strings.Split(*scenarios, ",") {
		want[strings.TrimSpace(s)] = true
	}
	results := map[string][]iterRecord{}
	slot := func() (string, time.Time) {
		if len(pool) == 0 {
			fatal("ran out of slots")
		}
		s := pool[0]
		pool = pool[1:]
		return s.siteID, s.date
	}

	if want["cold"] {
		slog.Info("scenario cold start")
		recs := runCold(sess, *campground, *iter, *nights, *pause, slot)
		results["cold"] = recs
	}
	if want["warm1"] {
		slog.Info("scenario warm1 start")
		recs, err := runWarm1(sess, *campground, *iter, *nights, *pause, slot)
		if err != nil {
			slog.Error("warm1 failed", "err", err)
		}
		results["warm1"] = recs
	}
	if want["warmN"] {
		slog.Info("scenario warmN start", "tabs", *tabs)
		recs, err := runWarmN(sess, *campground, *tabs, *iter, *nights, *pause, slot)
		if err != nil {
			slog.Error("warmN failed", "err", err)
		}
		results["warmN"] = recs
	}

	// Step 4: report.
	report(results, *debugDir)
}

type iterRecord struct {
	Scenario string             `json:"scenario"`
	SiteID   string             `json:"site_id"`
	Date     string             `json:"date"`
	Wall     time.Duration      `json:"wall_ns"` // wall clock for the on-demand portion only (no prewarm)
	Timings  booker.HoldTimings `json:"timings"`
	OrderID  string             `json:"order_id,omitempty"`
	Err      string             `json:"err,omitempty"`
	Raw      map[string]any     `json:"-"`
}

func runCold(sess *booker.Session, cg string, n, nights int, pause time.Duration, slot func() (string, time.Time)) []iterRecord {
	recs := make([]iterRecord, 0, n)
	for i := 0; i < n; i++ {
		siteID, date := slot()
		depart := date.AddDate(0, 0, nights)
		ctx, cancel := context.WithTimeout(sess.Ctx(), 90*time.Second)
		start := time.Now()
		res, err := sess.HoldCampsite(ctx, siteID, cg, date, depart)
		wall := time.Since(start)
		cancel()
		rec := iterRecord{Scenario: "cold", SiteID: siteID, Date: date.Format("2006-01-02"), Wall: wall}
		if res != nil {
			rec.Timings = res.Timings
			rec.OrderID = res.OrderID
		}
		if err != nil {
			rec.Err = err.Error()
		}
		slog.Info("cold iter", "i", i, "site", siteID, "date", rec.Date, "wall", wall, "err", rec.Err)
		recs = append(recs, rec)
		time.Sleep(pause)
	}
	return recs
}

func runWarm1(sess *booker.Session, cg string, n, nights int, pause time.Duration, slot func() (string, time.Time)) ([]iterRecord, error) {
	// Pre-warm on the FIRST site we'll book. Subsequent iterations will
	// re-prewarm on each new site, but we measure only HoldFast wall.
	recs := make([]iterRecord, 0, n)
	tab, err := sess.NewTab()
	if err != nil {
		return nil, err
	}
	defer tab.Close()
	for i := 0; i < n; i++ {
		siteID, date := slot()
		depart := date.AddDate(0, 0, nights)
		// Pre-warm (out-of-band, not counted in Wall).
		pctx, pcancel := context.WithTimeout(tab.Ctx(), 60*time.Second)
		pt, perr := booker.PrewarmCampsite(pctx, siteID)
		pcancel()
		if perr != nil {
			slog.Warn("warm1 prewarm failed", "err", perr)
			recs = append(recs, iterRecord{Scenario: "warm1", SiteID: siteID, Date: date.Format("2006-01-02"), Err: perr.Error()})
			continue
		}
		// Now measure the on-demand HoldFast.
		hctx, hcancel := context.WithTimeout(tab.Ctx(), 60*time.Second)
		start := time.Now()
		res, err := booker.HoldFast(hctx, siteID, cg, date, depart)
		wall := time.Since(start)
		hcancel()
		rec := iterRecord{Scenario: "warm1", SiteID: siteID, Date: date.Format("2006-01-02"), Wall: wall}
		if res != nil {
			rec.Timings = res.Timings
			rec.Timings.Nav = pt.Nav
			rec.Timings.RecaptchaWait = pt.RecaptchaWait
			rec.OrderID = res.OrderID
		}
		if err != nil {
			rec.Err = err.Error()
		}
		slog.Info("warm1 iter", "i", i, "site", siteID, "date", rec.Date, "wall", wall, "prewarm_nav", pt.Nav, "err", rec.Err)
		recs = append(recs, rec)
		time.Sleep(pause)
	}
	return recs, nil
}

func runWarmN(sess *booker.Session, cg string, tabs, n, nights int, pause time.Duration, slot func() (string, time.Time)) ([]iterRecord, error) {
	// Open N tabs, each prewarmed on a distinct site. Reuse slots round-robin.
	type tabState struct {
		tab    *booker.Tab
		siteID string
		date   time.Time
	}
	states := make([]*tabState, 0, tabs)
	for i := 0; i < tabs; i++ {
		t, err := sess.NewTab()
		if err != nil {
			return nil, fmt.Errorf("open tab %d: %w", i, err)
		}
		siteID, date := slot()
		pctx, pcancel := context.WithTimeout(t.Ctx(), 60*time.Second)
		if _, err := booker.PrewarmCampsite(pctx, siteID); err != nil {
			pcancel()
			t.Close()
			return nil, fmt.Errorf("prewarm tab %d: %w", i, err)
		}
		pcancel()
		states = append(states, &tabState{tab: t, siteID: siteID, date: date})
		slog.Info("warmN tab ready", "tab", i, "site", siteID)
	}
	defer func() {
		for _, s := range states {
			s.tab.Close()
		}
	}()

	recs := make([]iterRecord, 0, n)
	for i := 0; i < n; i++ {
		st := states[i%len(states)]
		// Use the slot this tab was already prewarmed for the first time it
		// is hit; afterwards prewarm a fresh slot on this tab (which counts
		// nav+recaptcha against future iterations, but those aren't part of
		// Wall — they happen out-of-band).
		siteID, date := st.siteID, st.date
		depart := date.AddDate(0, 0, nights)
		hctx, hcancel := context.WithTimeout(st.tab.Ctx(), 60*time.Second)
		start := time.Now()
		res, err := booker.HoldFast(hctx, siteID, cg, date, depart)
		wall := time.Since(start)
		hcancel()
		rec := iterRecord{Scenario: "warmN", SiteID: siteID, Date: date.Format("2006-01-02"), Wall: wall}
		if res != nil {
			rec.Timings = res.Timings
			rec.OrderID = res.OrderID
		}
		if err != nil {
			rec.Err = err.Error()
		}
		slog.Info("warmN iter", "i", i, "tab", i%len(states), "site", siteID, "wall", wall, "err", rec.Err)
		recs = append(recs, rec)
		// Re-prewarm this tab for its next turn on a fresh slot.
		newSite, newDate := slot()
		pctx, pcancel := context.WithTimeout(st.tab.Ctx(), 60*time.Second)
		if _, perr := booker.PrewarmCampsite(pctx, newSite); perr != nil {
			slog.Warn("warmN re-prewarm failed", "tab", i%len(states), "err", perr)
		} else {
			st.siteID, st.date = newSite, newDate
		}
		pcancel()
		time.Sleep(pause)
	}
	return recs, nil
}

func report(results map[string][]iterRecord, debugDir string) {
	fmt.Println()
	fmt.Println("=== booker-bench results ===")
	names := []string{"cold", "warm1", "warmN"}
	for _, name := range names {
		recs, ok := results[name]
		if !ok || len(recs) == 0 {
			continue
		}
		var walls, navs, recs2, sess2, scripts, tokens, posts []time.Duration
		var ok2, errs int
		for _, r := range recs {
			walls = append(walls, r.Wall)
			navs = append(navs, r.Timings.Nav)
			recs2 = append(recs2, r.Timings.RecaptchaWait)
			sess2 = append(sess2, r.Timings.SessionCheck)
			scripts = append(scripts, r.Timings.ScriptEval)
			tokens = append(tokens, r.Timings.RecaptchaToken)
			posts = append(posts, r.Timings.RecGovPost)
			if r.Err == "" {
				ok2++
			} else {
				errs++
			}
		}
		fmt.Printf("\n[%s]  n=%d  ok=%d  err=%d\n", name, len(recs), ok2, errs)
		printRow("wall (on-demand)", walls)
		printRow("  nav            ", navs)
		printRow("  recaptcha wait ", recs2)
		printRow("  session check  ", sess2)
		printRow("  script eval    ", scripts)
		printRow("    recap token  ", tokens)
		printRow("    recgov post  ", posts)
	}
	// Dump raw records too.
	out := filepath.Join(debugDir, fmt.Sprintf("bench-%d.json", time.Now().Unix()))
	f, err := os.Create(out)
	if err == nil {
		defer f.Close()
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
		fmt.Printf("\nRaw records: %s\n", out)
	}
}

func printRow(label string, ds []time.Duration) {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	if len(ds) == 0 {
		return
	}
	p := func(q float64) time.Duration {
		if len(ds) == 0 {
			return 0
		}
		i := int(float64(len(ds)-1) * q)
		return ds[i]
	}
	fmt.Printf("  %s  p50=%-8s p95=%-8s max=%-8s\n", label, fmtDur(p(0.5)), fmtDur(p(0.95)), fmtDur(p(1.0)))
}

func fmtDur(d time.Duration) string {
	if d == 0 {
		return "—"
	}
	return d.Round(time.Millisecond).String()
}

// availability helpers

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
	req.Header.Set("User-Agent", "Mozilla/5.0 booker-bench")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("availability status %d: %s", resp.StatusCode, string(body))
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
