// booker-stability holds a prewarmed tab on a campsite page and fires a
// HoldFast every -interval, rotating through fresh (site, date) slots.
//
// Goal: confirm steady-state behaviour over long windows — does the SPA
// stay healthy, the JWT survive, the grecaptcha bindings stay callable, and
// the on-demand wall time stay flat? If anything degrades, the run prints a
// recovery (re-prewarm) and continues.
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
		campground = flag.String("campground", "10085599", "test campground ID")
		monthStart = flag.String("month", "2026-07-01", "month YYYY-MM-01")
		nights     = flag.Int("nights", 1, "nights per hold")
		interval   = flag.Duration("interval", 3*time.Minute, "delay between holds")
		runFor     = flag.Duration("for", 60*time.Minute, "total wall-clock budget")
		profileDir = flag.String("profile", "", "Chrome profile dir")
		chromePath = flag.String("chrome", "", "Chrome binary")
		debugDir   = flag.String("debug-dir", "/tmp/booker-stability", "debug dump dir")
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

	month, err := time.Parse("2006-01-02", *monthStart)
	if err != nil {
		fatal("bad -month: %v", err)
	}
	slots, err := fetchAvailableSlots(context.Background(), *campground, month)
	if err != nil {
		fatal("fetch availability: %v", err)
	}
	if len(slots) < 5 {
		fatal("not enough slots (%d)", len(slots))
	}
	rand.Shuffle(len(slots), func(i, j int) { slots[i], slots[j] = slots[j], slots[i] })
	slog.Info("availability fetched", "slots", len(slots))

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

	tab, err := sess.NewTab()
	if err != nil {
		fatal("new tab: %v", err)
	}
	defer tab.Close()

	var records []record

	deadline := time.Now().Add(*runFor)
	// Group dates by siteID so when we rotate the date we stay on a
	// (site, date) tuple rec.gov has actually marked Available.
	bySite := map[string][]time.Time{}
	for _, s := range slots {
		bySite[s.siteID] = append(bySite[s.siteID], s.date)
	}
	pickSiteWithDates := func(min int) (string, []time.Time) {
		for sid, ds := range bySite {
			if len(ds) >= min {
				return sid, ds
			}
		}
		return "", nil
	}
	curSite, curSiteDates := pickSiteWithDates(5)
	if curSite == "" {
		fatal("no site has at least 5 available dates")
	}
	delete(bySite, curSite)
	rand.Shuffle(len(curSiteDates), func(i, j int) { curSiteDates[i], curSiteDates[j] = curSiteDates[j], curSiteDates[i] })
	nextDate := func() time.Time {
		if len(curSiteDates) == 0 {
			// Rotate to a fresh site if we exhaust the current site's dates.
			ns, nd := pickSiteWithDates(1)
			if ns == "" {
				return time.Time{}
			}
			curSite, curSiteDates = ns, nd
			delete(bySite, ns)
			rand.Shuffle(len(curSiteDates), func(i, j int) { curSiteDates[i], curSiteDates[j] = curSiteDates[j], curSiteDates[i] })
		}
		d := curSiteDates[0]
		curSiteDates = curSiteDates[1:]
		return d
	}
	_ = slots // not used after this point
	pctx, pcancel := context.WithTimeout(tab.Ctx(), 60*time.Second)
	if _, err := booker.PrewarmCampsite(pctx, curSite); err != nil {
		pcancel()
		fatal("initial prewarm: %v", err)
	}
	pcancel()
	slog.Info("initial prewarm done", "site", curSite)

	iter := 0
	recoverNext := false
	for time.Now().Before(deadline) {
		iter++
		date := nextDate()
		if date.IsZero() {
			slog.Warn("ran out of (site,date) tuples; stopping early")
			break
		}
		depart := date.AddDate(0, 0, *nights)
		hctx, hcancel := context.WithTimeout(tab.Ctx(), 60*time.Second)
		start := time.Now()
		res, err := booker.HoldFast(hctx, curSite, *campground, date, depart)
		wall := time.Since(start)
		hcancel()
		rec := record{
			Iter: iter, At: time.Now(), Site: curSite, Date: date.Format("2006-01-02"),
			Prewarmed: true, WallMs: wall.Milliseconds(),
			Recovered: recoverNext,
		}
		if res != nil {
			rec.PostMs = res.Timings.RecGovPost.Milliseconds()
			rec.TokenMs = res.Timings.RecaptchaToken.Milliseconds()
		}
		recoverNext = false
		// "modification already in cart" and "popular site already reserved"
		// are *not* infrastructure failures — recaptcha minted, the POST
		// reached rec.gov, and the server replied within wall time. They
		// mean the test target accumulated state (e.g., from a previous
		// run). Count the latency, mark the row, but don't trigger recovery.
		if err != nil && (strings.Contains(err.Error(), "modification already in cart") || strings.Contains(err.Error(), "popular site")) {
			rec.Err = err.Error()
			slog.Info("iter ok (server rejected w/ benign 400)", "iter", iter, "wall", wall, "err", err)
			records = append(records, rec)
			select { case <-time.After(*interval): }
			continue
		}
		if err != nil {
			rec.Err = err.Error()
			slog.Warn("iter failed", "iter", iter, "err", err, "wall", wall)
			// Recover: rotate to a fresh site so we keep going.
			ns, nd := pickSiteWithDates(1)
			if ns == "" {
				slog.Error("no fresh site to recover onto; stopping")
				break
			}
			curSite, curSiteDates = ns, nd
			delete(bySite, ns)
			rand.Shuffle(len(curSiteDates), func(i, j int) { curSiteDates[i], curSiteDates[j] = curSiteDates[j], curSiteDates[i] })
			pctx, pcancel := context.WithTimeout(tab.Ctx(), 60*time.Second)
			if _, perr := booker.PrewarmCampsite(pctx, curSite); perr != nil {
				slog.Error("recovery prewarm failed; reopening tab", "err", perr)
				tab.Close()
				tab, err = sess.NewTab()
				if err != nil {
					slog.Error("re-open tab failed", "err", err)
					break
				}
				pctx2, pcancel2 := context.WithTimeout(tab.Ctx(), 60*time.Second)
				if _, perr := booker.PrewarmCampsite(pctx2, curSite); perr != nil {
					slog.Error("post-reopen prewarm failed", "err", perr)
					pcancel2()
					break
				}
				pcancel2()
			}
			pcancel()
			recoverNext = true
		} else {
			slog.Info("iter ok", "iter", iter, "wall", wall, "site", curSite, "date", rec.Date)
		}
		records = append(records, rec)
		// Pace.
		select {
		case <-time.After(*interval):
		}
	}

	out := filepath.Join(*debugDir, fmt.Sprintf("stability-%d.json", time.Now().Unix()))
	if f, err := os.Create(out); err == nil {
		defer f.Close()
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		_ = enc.Encode(records)
	}
	summarize(records)
	fmt.Println("raw:", out)
}

func summarize(records []record) {
	var oks, benign400s, infraErrs int
	var walls []int64
	for _, r := range records {
		switch {
		case r.Err == "":
			oks++
			walls = append(walls, r.WallMs)
		case strings.Contains(r.Err, "modification already in cart"), strings.Contains(r.Err, "popular site"):
			benign400s++
			walls = append(walls, r.WallMs) // still a real round trip
		default:
			infraErrs++
		}
	}
	errs := infraErrs
	fmt.Println()
	fmt.Println("=== stability summary ===")
	fmt.Printf("iters: %d  ok: %d  benign400: %d  infra_err: %d\n", len(records), oks, benign400s, errs)
	if len(walls) > 0 {
		var sum int64
		for _, w := range walls {
			sum += w
		}
		fmt.Printf("wall (ok only) mean=%dms first=%dms last=%dms\n", sum/int64(len(walls)), walls[0], walls[len(walls)-1])
		// drift check: median of first quartile vs last quartile
		q := len(walls) / 4
		if q > 0 {
			var early, late int64
			for i := 0; i < q; i++ {
				early += walls[i]
				late += walls[len(walls)-1-i]
			}
			fmt.Printf("wall first-quartile-mean=%dms  last-quartile-mean=%dms  (drift = %+dms)\n",
				early/int64(q), late/int64(q), late/int64(q)-early/int64(q))
		}
	}
	for _, r := range records {
		if r.Err != "" {
			fmt.Printf("  err iter=%d at=%s: %s\n", r.Iter, r.At.Format("15:04:05"), r.Err)
		}
	}
}

type record struct {
	Iter      int       `json:"iter"`
	At        time.Time `json:"at"`
	Site      string    `json:"site"`
	Date      string    `json:"date"`
	Prewarmed bool      `json:"prewarmed"`
	WallMs    int64     `json:"wall_ms"`
	PostMs    int64     `json:"post_ms"`
	TokenMs   int64     `json:"token_ms"`
	Err       string    `json:"err,omitempty"`
	Recovered bool      `json:"recovered,omitempty"`
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
	req.Header.Set("User-Agent", "Mozilla/5.0 booker-stability")
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
