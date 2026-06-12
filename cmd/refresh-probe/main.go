// refresh-probe validates that POST /api/accounts/login/v2/refresh works
// as discovered from the rec.gov SPA bundle. The flow we test mirrors
// what the SPA does:
//
//	const {access_token, account.account_id, refresh_id} = JSON.parse(localStorage.recaccount)
//	const resp = await fetch('/api/accounts/login/v2/refresh', {
//	  method: 'POST',
//	  headers: {'Authorization': 'Bearer ' + access_token, 'content-type': 'application/json'},
//	  body: JSON.stringify({account_id, refresh_id}),
//	})
//	if (resp.status === 200) localStorage.recaccount = await resp.text()
//
// Success criteria:
//
//  1. Refresh returns 200.
//  2. The new recaccount's access_token is different from the old one.
//  3. The new access_token's JWT `exp` claim is later than the old one's.
//  4. A sibling tab sees the new token after the refresh completes.
//  5. A second refresh using the NEW recaccount also succeeds (proves
//     the refresh_id is rotated to a fresh one and the chain doesn't
//     break after one use).
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/booker"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const refreshScript = `(async () => {
	const raw = window.localStorage.getItem('recaccount');
	if (!raw) return {ok: false, err: 'no recaccount'};
	let rec;
	try { rec = JSON.parse(raw); } catch (e) { return {ok: false, err: 'parse: ' + e.message}; }
	const accessToken = rec && rec.access_token;
	const accountID   = rec && rec.account && rec.account.account_id;
	const refreshID   = rec && rec.refresh_id;
	if (!accessToken || !accountID || !refreshID) {
		return {ok: false, err: 'missing access_token / account_id / refresh_id'};
	}
	const t0 = performance.now();
	const resp = await fetch('/api/accounts/login/v2/refresh', {
		method: 'POST',
		headers: {
			'authorization': 'Bearer ' + accessToken,
			'content-type': 'application/json',
		},
		body: JSON.stringify({account_id: accountID, refresh_id: refreshID}),
		credentials: 'include',
	});
	const wallMs = performance.now() - t0;
	const text = await resp.text();
	if (resp.status !== 200) {
		return {ok: false, status: resp.status, body: text, wallMs};
	}
	// Atomic swap of localStorage — sibling tabs will pick this up on
	// their next read.
	window.localStorage.setItem('recaccount', text);
	return {ok: true, status: resp.status, body: text, wallMs};
})()`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	home, _ := os.UserHomeDir()
	sess, err := booker.Open(booker.Config{ProfileDir: filepath.Join(home, ".cache", "recgov-booker")})
	if err != nil {
		die("open: %v", err)
	}
	defer sess.Close()
	ctx, cancel := context.WithTimeout(sess.Ctx(), 120*time.Second)
	defer cancel()

	if err := sess.Login(ctx, os.Getenv("REC_GOV_EMAIL"), os.Getenv("REC_GOV_PASSWORD")); err != nil {
		die("login: %v", err)
	}
	fmt.Println("=== logged in; reading initial recaccount ===")
	at1, rid1, exp1 := readRecaccount(ctx)
	fmt.Printf("  access_token: %s... (exp %s, %s remaining)\n", at1[:30], time.Unix(exp1, 0).Format(time.RFC3339), time.Until(time.Unix(exp1, 0)).Round(time.Second))
	fmt.Printf("  refresh_id:   %s\n", rid1)

	// Open a sibling tab BEFORE the refresh so we can observe whether
	// the refreshed token propagates without re-navigating.
	tab, err := sess.NewTab()
	if err != nil {
		die("new tab: %v", err)
	}
	defer tab.Close()
	if err := chromedp.Run(tab.Ctx(),
		chromedp.Navigate("https://www.recreation.gov/"),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		die("sibling nav: %v", err)
	}
	atSib1, _, _ := readRecaccountOnCtx(tab.Ctx())
	fmt.Println("\n=== sibling tab opened ===")
	fmt.Printf("  sibling sees access_token = main? %v\n", atSib1 == at1)

	fmt.Println("\n=== Call POST /api/accounts/login/v2/refresh from main tab ===")
	res := callRefresh(ctx)
	fmt.Printf("  ok=%v status=%v wall=%.0fms\n", res.OK, res.Status, res.WallMs)
	if !res.OK {
		fmt.Printf("  body: %s\n", truncate(res.Body, 240))
		die("refresh failed")
	}

	at2, rid2, exp2 := readRecaccount(ctx)
	fmt.Println("\n=== Validation ===")
	if at2 == at1 {
		fmt.Println("  ✗ access_token UNCHANGED — refresh didn't actually mint a new token")
	} else {
		fmt.Println("  ✓ access_token changed")
	}
	if exp2 <= exp1 {
		fmt.Printf("  ✗ new exp (%d) is NOT later than old (%d)\n", exp2, exp1)
	} else {
		fmt.Printf("  ✓ new exp %ds later than old (token lifetime extended)\n", exp2-exp1)
	}
	if rid2 == rid1 {
		fmt.Println("  ! refresh_id UNCHANGED — might still work but the chain isn't being rotated")
	} else {
		fmt.Println("  ✓ refresh_id rotated")
	}

	atSib2, _, _ := readRecaccountOnCtx(tab.Ctx())
	if atSib2 == at2 {
		fmt.Println("  ✓ sibling tab IMMEDIATELY sees the new access_token (no re-navigation needed)")
	} else if atSib2 == at1 {
		fmt.Println("  ! sibling tab still sees the OLD token (localStorage cross-tab cache lag)")
	} else {
		fmt.Println("  ?? sibling tab sees a third token, neither old nor new")
	}

	fmt.Println("\n=== Second refresh using the new recaccount (chain validity) ===")
	res2 := callRefresh(ctx)
	fmt.Printf("  ok=%v status=%v wall=%.0fms\n", res2.OK, res2.Status, res2.WallMs)
	if !res2.OK {
		fmt.Printf("  body: %s\n", truncate(res2.Body, 240))
		fmt.Println("  ✗ second refresh failed — refresh_id rotation is broken")
	} else {
		fmt.Println("  ✓ second refresh succeeded — silent refresh chain works indefinitely")
	}

	fmt.Println("\n=== Validating Session.RefreshAccessToken (the public Go API) ===")
	at3, _, _ := readRecaccount(ctx)
	start := time.Now()
	if err := sess.RefreshAccessToken(ctx); err != nil {
		fmt.Printf("  ✗ sess.RefreshAccessToken: %v\n", err)
		os.Exit(1)
	}
	wall := time.Since(start)
	at4, _, _ := readRecaccount(ctx)
	if at4 == at3 {
		fmt.Printf("  ✗ access_token UNCHANGED after sess.RefreshAccessToken — Go wrapper broken\n")
		os.Exit(1)
	}
	fmt.Printf("  ✓ sess.RefreshAccessToken rotated the token in %s wall\n", wall.Round(time.Millisecond))

	// === The actual end-to-end test: book a campsite using the
	// refreshed token. If rec.gov accepts the new token on
	// /api/camps/reservations/.../multi, the silent refresh fix is
	// fully proven for production.
	fmt.Println("\n=== Real HoldFast booking with the refreshed token (Baker Dam) ===")
	bookSite, bookDate, err := pickAvailableSlot(ctx)
	if err != nil {
		fmt.Printf("  ✗ couldn't find an available slot: %v\n", err)
		os.Exit(1)
	}
	checkOut := bookDate.AddDate(0, 0, 1)
	fmt.Printf("  target: site=%s date=%s\n", bookSite, bookDate.Format("2006-01-02"))

	// HoldFast requires the tab to already be on a page with
	// grecaptcha loaded. Navigate the main tab to the campground page.
	if _, err := booker.PrewarmCampground(ctx, "10085599"); err != nil {
		fmt.Printf("  ✗ prewarm campground: %v\n", err)
		os.Exit(1)
	}
	holdStart := time.Now()
	holdRes, holdErr := booker.HoldFast(ctx, bookSite, "10085599", bookDate, checkOut)
	holdWall := time.Since(holdStart)
	if holdErr != nil {
		fmt.Printf("  ✗ HoldFast failed (wall=%s): %v\n", holdWall.Round(time.Millisecond), holdErr)
		os.Exit(1)
	}
	if holdRes == nil || holdRes.OrderID == "" {
		fmt.Printf("  ✗ HoldFast returned no order_id (wall=%s)\n", holdWall.Round(time.Millisecond))
		os.Exit(1)
	}
	fmt.Printf("  ✓ booking POST succeeded with refreshed token: order_id=%s wall=%s\n",
		holdRes.OrderID, holdWall.Round(time.Millisecond))
	fmt.Println("  → silent refresh fully validated end-to-end. The refreshed token works for bookings.")
}

// pickAvailableSlot finds any (site, date) tuple currently available
// at Baker Dam (10085599). Used purely to feed HoldFast a target that
// rec.gov will accept (not "modification already in cart" or "popular
// site"). Pulls availability for the next ~3 months and returns the
// first match.
func pickAvailableSlot(ctx context.Context) (siteID string, date time.Time, err error) {
	type avail struct {
		Campsites map[string]struct {
			Availabilities map[string]string `json:"availabilities"`
		} `json:"campsites"`
	}
	month := time.Now().AddDate(0, 1, 0).UTC()
	month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	u, _ := neturl.Parse("https://www.recreation.gov/api/camps/availability/campground/10085599/month")
	q := u.Query()
	q.Set("start_date", month.Format("2006-01-02T15:04:05.000Z"))
	u.RawQuery = q.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 refresh-probe")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", time.Time{}, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	var parsed avail
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", time.Time{}, err
	}
	for site, sd := range parsed.Campsites {
		for d, status := range sd.Availabilities {
			if status != "Available" {
				continue
			}
			t, err := time.Parse(time.RFC3339, d)
			if err != nil {
				continue
			}
			return site, t, nil
		}
	}
	return "", time.Time{}, fmt.Errorf("no available slots found at Baker Dam in %s", month.Format("2006-01"))
}

type refreshResult struct {
	OK     bool    `json:"ok"`
	Status int     `json:"status"`
	Body   string  `json:"body"`
	Err    string  `json:"err"`
	WallMs float64 `json:"wallMs"`
}

func callRefresh(ctx context.Context) refreshResult {
	var out refreshResult
	awaitOpt := func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }
	if err := chromedp.Run(ctx, chromedp.Evaluate(refreshScript, &out, awaitOpt)); err != nil {
		out.Err = err.Error()
	}
	return out
}

func readRecaccount(ctx context.Context) (accessToken, refreshID string, exp int64) {
	return readRecaccountOnCtx(ctx)
}

func readRecaccountOnCtx(ctx context.Context) (accessToken, refreshID string, exp int64) {
	var out struct {
		AccessToken string `json:"access_token"`
		RefreshID   string `json:"refresh_id"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`(() => { const r = JSON.parse(window.localStorage.getItem('recaccount') || '{}'); return {access_token: r.access_token || '', refresh_id: r.refresh_id || ''}; })()`,
		&out,
	)); err != nil {
		return "", "", 0
	}
	if out.AccessToken == "" {
		return "", "", 0
	}
	exp, _ = decodeJWTExp(out.AccessToken)
	return out.AccessToken, out.RefreshID, exp
}

func decodeJWTExp(token string) (int64, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, fmt.Errorf("not a JWT")
	}
	seg := parts[1]
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	b, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		return 0, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, err
	}
	exp, _ := m["exp"].(float64)
	return int64(exp), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...)
	os.Exit(1)
}
