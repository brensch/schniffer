// recaptcha-probe checks which rec.gov SPA routes have
// grecaptcha.enterprise.execute defined, and (when so) whether a HoldFast
// call from that page actually succeeds against the rec.gov POST. If the
// campground overview accepts the hold, we can park one warm tab per
// schniff there and never re-navigate per campsite.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/brensch/schniffer/internal/booker"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	home, _ := os.UserHomeDir()
	sess, err := booker.Open(booker.Config{ProfileDir: filepath.Join(home, ".cache", "recgov-booker")})
	if err != nil {
		die("open: %v", err)
	}
	defer sess.Close()
	ctx, cancel := context.WithTimeout(sess.Ctx(), 180*time.Second)
	defer cancel()
	if err := sess.Login(ctx, os.Getenv("REC_GOV_EMAIL"), os.Getenv("REC_GOV_PASSWORD")); err != nil {
		die("login: %v", err)
	}

	// Pick a fresh (siteID, date) for Baker Dam so we can try a real POST.
	month, _ := time.Parse("2006-01-02", "2026-09-01")
	slots, err := fetchAvailableSlots(ctx, "10085599", month)
	if err != nil || len(slots) == 0 {
		die("availability: %v", err)
	}
	rand.Shuffle(len(slots), func(i, j int) { slots[i], slots[j] = slots[j], slots[i] })
	target := slots[0]
	fmt.Printf("test target: siteID=%s date=%s\n\n", target.siteID, target.date.Format("2006-01-02"))

	cases := []struct{ label, url string }{
		{"campground overview", "https://www.recreation.gov/camping/campgrounds/10085599"},
		{"campsite page (control)", fmt.Sprintf("https://www.recreation.gov/camping/campsites/%s", target.siteID)},
		{"homepage", "https://www.recreation.gov/"},
	}
	for _, c := range cases {
		probeWithRealHold(ctx, c.label, c.url, target.siteID, "10085599", target.date)
	}
}

func probeWithRealHold(ctx context.Context, label, u, campsiteID, campgroundID string, date time.Time) {
	fmt.Printf("=== %s (%s) ===\n", label, u)
	navStart := time.Now()
	if err := chromedp.Run(ctx,
		chromedp.Navigate(u),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		fmt.Printf("  NAV FAILED: %v\n\n", err)
		return
	}
	fmt.Printf("  nav: %s\n", time.Since(navStart).Round(time.Millisecond))

	pStart := time.Now()
	if err := chromedp.Run(ctx, chromedp.Poll(
		`typeof grecaptcha !== 'undefined' && grecaptcha.enterprise && typeof grecaptcha.enterprise.execute === 'function'`,
		nil, chromedp.WithPollingTimeout(15*time.Second),
	)); err != nil {
		fmt.Printf("  recaptcha NEVER LOADED (%s)\n\n", time.Since(pStart).Round(time.Millisecond))
		return
	}
	fmt.Printf("  recaptcha ready: %s\n", time.Since(pStart).Round(time.Millisecond))

	// Run the booking script: mint token + POST. Adapted from booker.HoldFast.
	checkOut := date.AddDate(0, 0, 1)
	const iso = "2006-01-02T00:00:00.000Z"
	nightMap := map[string]map[string]string{
		date.UTC().Format(iso): {"campsite_id": campsiteID, "campsite_loop": "", "campsite_name": ""},
	}
	payload := map[string]any{
		"campsiteID":   campsiteID,
		"campgroundID": campgroundID,
		"checkIn":      date.UTC().Format(iso),
		"checkOut":     checkOut.UTC().Format(iso),
		"nightMap":     nightMap,
		"siteKey":      booker.RecaptchaSiteKey,
		"action":       booker.RecaptchaAction,
	}
	pb, _ := json.Marshal(payload)
	script := `(async (p) => {
		const _t0 = performance.now();
		const token = await grecaptcha.enterprise.execute(p.siteKey, {action: p.action});
		const _tokenMs = performance.now() - _t0;
		const recRaw = window.localStorage.getItem('recaccount');
		if (!recRaw) throw new Error('no recaccount in localStorage');
		const rec = JSON.parse(recRaw);
		const body = {
			reservations: [{
				account_id: rec.account.account_id,
				campsite_id: p.campsiteID,
				check_in: p.checkIn,
				check_out: p.checkOut,
				reservation_options: {night_map: p.nightMap, recommendation_referrer: 'campground-vnull:campsitePage'},
			}],
			gate_a: {value: token, description: p.action, success: true, terminal: 'east'},
		};
		const _postStart = performance.now();
		const r = await fetch('/api/camps/reservations/campgrounds/' + p.campgroundID + '/multi', {
			method: 'POST',
			headers: {'content-type': 'application/json', 'authorization': 'Bearer ' + rec.access_token},
			body: JSON.stringify(body),
			credentials: 'include',
		});
		const text = await r.text();
		const _postMs = performance.now() - _postStart;
		let parsed; try { parsed = JSON.parse(text); } catch (_) { parsed = {raw: text}; }
		return {status: r.status, _tokenMs, _postMs, ...parsed};
	})(` + string(pb) + `)`
	var out map[string]any
	holdStart := time.Now()
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
		fmt.Printf("  SCRIPT FAILED: %v\n\n", err)
		return
	}
	wall := time.Since(holdStart)
	fmt.Printf("  hold wall: %s  (token=%.0fms post=%.0fms)\n",
		wall.Round(time.Millisecond), out["_tokenMs"], out["_postMs"])
	status := out["status"]
	res, _ := json.Marshal(out)
	if oid := booker.ExtractOrderID(out); oid != "" {
		fmt.Printf("  STATUS=%v order_id=%s ✓ HELD\n", status, oid)
	} else {
		fmt.Printf("  STATUS=%v  body=%s\n", status, truncate(string(res), 240))
	}
	fmt.Println()
}

func truncate(s string, n int) string { if len(s) > n { return s[:n] + "…" }; return s }

type slotInfo struct{ siteID string; date time.Time }
func fetchAvailableSlots(ctx context.Context, cg string, month time.Time) ([]slotInfo, error) {
	base, _ := url.Parse(fmt.Sprintf("https://www.recreation.gov/api/camps/availability/campground/%s/month", cg))
	q := base.Query(); q.Set("start_date", month.UTC().Format("2006-01-02T15:04:05.000Z")); base.RawQuery = q.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req); if err != nil { return nil, err }; defer resp.Body.Close()
	if resp.StatusCode != 200 { body, _ := io.ReadAll(resp.Body); return nil, fmt.Errorf("%d %s", resp.StatusCode, body) }
	var parsed struct{ Campsites map[string]struct{ Availabilities map[string]string `json:"availabilities"` } `json:"campsites"` }
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil { return nil, err }
	var out []slotInfo
	for s, d := range parsed.Campsites { for dt, st := range d.Availabilities { if st == "Available" { t, _ := time.Parse(time.RFC3339, dt); out = append(out, slotInfo{s, t}) } } }
	return out, nil
}

func die(format string, args ...any) { fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...); os.Exit(1) }
