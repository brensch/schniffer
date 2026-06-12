// Package booker drives a real Chrome session (chromedp) against
// recreation.gov so the reCAPTCHA Enterprise check on the add-to-cart
// endpoint passes transparently.
//
// A Session owns one Chrome instance with a persistent user-data-dir; once
// Login succeeds, the JWT + r1s anti-bot cookie sit in that profile and
// subsequent HoldCampsite calls reuse them with no extra round trips.
package booker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const (
	SiteOrigin       = "https://www.recreation.gov"
	RecaptchaSiteKey = "6LdBIvUZAAAAAM02b8GWJew_1LffQJo9rNB5yVTU"
	RecaptchaAction  = "campsiteListBooking"
)

// ErrBadCredentials is returned by Login when the submitted email/password is
// rejected (or MFA/captcha intervenes). Callers use this to disable the user's
// stored credential and notify them.
var ErrBadCredentials = errors.New("bad credentials")

// ErrHumanVerification is returned when rec.gov rejects the automated v3
// reCAPTCHA score and requires its visible human-verification challenge.
var ErrHumanVerification = errors.New("human verification required")

// ErrNotLoggedIn is returned by HoldCampsite when the session's JWT
// ('recaccount' in localStorage) has been cleared by rec.gov — typically a
// silent server-side session expiry. Chrome is still alive, but a fresh
// Login is required before the booking script will succeed. The pool catches
// this, relaunches, and retries once.
var ErrNotLoggedIn = errors.New("not logged in")

// ErrAuthExpired is returned when the recaccount IS present in localStorage
// but rec.gov rejected its access_token with HTTP 401 on the booking POST.
// Different from ErrNotLoggedIn (where the JWT was cleared entirely): the
// token still exists, it's just past its TTL. Recoverable by clearing the
// stale token + re-running Login on the same Chrome instance — no full
// browser relaunch needed. The Pool catches this, re-logins in place, and
// retries on the warm tab.
var ErrAuthExpired = errors.New("auth token expired")

type Config struct {
	ProfileDir string
	ChromePath string // empty = autodetect
}

type Session struct {
	cfg        Config
	allocCtx   context.Context
	allocClose context.CancelFunc
	ctx        context.Context
	ctxClose   context.CancelFunc
}

// Open launches Chrome under the given profile dir. The session stays alive
// until Close is called — callers should keep it warm for the lifetime of the
// process.
func Open(cfg Config) (*Session, error) {
	if cfg.ProfileDir == "" {
		return nil, errors.New("ProfileDir required")
	}
	if err := os.MkdirAll(cfg.ProfileDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir profile: %w", err)
	}
	// Hostname-keyed singleton locks survive container restarts and block
	// Chrome from reusing the profile; clear them on launch.
	for _, name := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket"} {
		_ = os.Remove(filepath.Join(cfg.ProfileDir, name))
	}

	opts := []chromedp.ExecAllocatorOption{
		chromedp.UserDataDir(cfg.ProfileDir),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.WindowSize(1280, 900),
		// Route Chrome's stderr through slog so crashes show up in
		// `docker logs` alongside everything else. Plain os.Stderr drowns
		// in dbus noise.
		chromedp.CombinedOutput(newChromeStderr(slog.Default().With("component", "chrome"), cfg.ProfileDir)),
	}
	chromePath := cfg.ChromePath
	if chromePath == "" {
		chromePath = AutodetectChrome()
	}
	if chromePath != "" {
		opts = append(opts, chromedp.ExecPath(chromePath))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancelCtx := chromedp.NewContext(allocCtx, chromedp.WithLogf(log.Printf))
	// Trigger browser start now so failures surface here, not on first action.
	if err := chromedp.Run(ctx); err != nil {
		cancelCtx()
		cancelAlloc()
		return nil, fmt.Errorf("start chrome: %w", err)
	}
	return &Session{
		cfg:        cfg,
		allocCtx:   allocCtx,
		allocClose: cancelAlloc,
		ctx:        ctx,
		ctxClose:   cancelCtx,
	}, nil
}

func (s *Session) Close() {
	if s.ctxClose != nil {
		s.ctxClose()
	}
	if s.allocClose != nil {
		s.allocClose()
	}
}

// Ctx returns the underlying chromedp context. Useful for callers that want
// to bound a single operation with WithTimeout without killing the session.
func (s *Session) Ctx() context.Context { return s.ctx }

// IsLoggedIn returns true if the SPA already has a JWT stashed in
// localStorage (set by a prior Login on this profile).
func (s *Session) IsLoggedIn(ctx context.Context) (bool, error) {
	if err := chromedp.Run(ctx,
		chromedp.Navigate(SiteOrigin+"/"),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return false, err
	}
	var ok bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`!!(window.localStorage.getItem('recaccount'))`, &ok,
	)); err != nil {
		return false, err
	}
	return ok, nil
}

// Login submits the rec.gov login form. Returns ErrBadCredentials if no JWT
// appears within the timeout (rec.gov does not return a clean error code —
// failed login leaves the form mounted with an inline error).
func (s *Session) Login(ctx context.Context, email, password string) error {
	loggedIn, err := s.IsLoggedIn(ctx)
	if err != nil {
		return err
	}
	if loggedIn {
		return nil
	}
	if err := chromedp.Run(ctx,
		chromedp.Navigate(SiteOrigin+"/log-in"),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("nav login: %w", err)
	}
	emailSel, passSel, submitSel, err := findLoginFields(ctx)
	if err != nil {
		return fmt.Errorf("locate fields: %w", err)
	}
	fill := func(sel, val string) chromedp.Action {
		js := fmt.Sprintf(`(() => {
			const el = document.querySelector(%q);
			if (!el) return false;
			const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
			setter.call(el, %q);
			el.dispatchEvent(new Event('input', {bubbles: true}));
			el.dispatchEvent(new Event('change', {bubbles: true}));
			return true;
		})()`, sel, val)
		var ok bool
		return chromedp.Evaluate(js, &ok)
	}
	if err := chromedp.Run(ctx,
		fill(emailSel, email),
		fill(passSel, password),
		chromedp.WaitEnabled(submitSel, chromedp.ByQuery),
		chromedp.Click(submitSel, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("submit: %w", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var ok bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(
			`!!(window.localStorage.getItem('recaccount'))`, &ok,
		)); err != nil {
			return fmt.Errorf("poll session: %w", err)
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return ErrBadCredentials
}

// Refresh navigates to the homepage to keep the session warm and the JWT
// fresh. Cheap; safe to call periodically.
func (s *Session) Refresh(ctx context.Context) error {
	return chromedp.Run(ctx,
		chromedp.Navigate(SiteOrigin+"/"),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
}

// ErrRefreshFailed is returned by RefreshAccessToken when the silent
// refresh endpoint returned non-200. Caller should fall back to the full
// form-fill Login. Common causes: refresh_id no longer valid (rec.gov
// rotated keys), session past max-refresh-chain age.
var ErrRefreshFailed = errors.New("token refresh failed")

// RefreshAccessToken calls rec.gov's POST /api/accounts/login/v2/refresh
// from this session's main tab, swapping the new recaccount into
// localStorage atomically. Returns nil on success; the new access_token
// is now in localStorage and visible to all tabs on the same origin
// (eventually — Chrome's cross-tab localStorage cache is lazy, but the
// old token is still valid until its original exp, so warm tabs serving
// stale cache succeed during the refresh window).
//
// Reverse-engineered from sarsaparilla-*.js: the SPA's refresh function
// POSTs {account_id, refresh_id} with the current access_token as
// Bearer auth, and on 200 sets the response body as the new recaccount.
// We replicate that exactly so behaviour matches what rec.gov expects
// for a normal browser session.
//
// Returns ErrRefreshFailed on non-200 so callers can fall back to the
// full Login path. ~100ms wall in practice (vs ~2-3s for Login).
func (s *Session) RefreshAccessToken(ctx context.Context) error {
	const script = `(async () => {
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
		const resp = await fetch('/api/accounts/login/v2/refresh', {
			method: 'POST',
			headers: {
				'authorization': 'Bearer ' + accessToken,
				'content-type': 'application/json',
			},
			body: JSON.stringify({account_id: accountID, refresh_id: refreshID}),
			credentials: 'include',
		});
		const text = await resp.text();
		if (resp.status !== 200) return {ok: false, status: resp.status, body: text};
		// Atomic swap. The OLD token remains valid until its original
		// exp, so any warm tab serving a stale localStorage cache during
		// this window's tail still succeeds with the old token.
		window.localStorage.setItem('recaccount', text);
		return {ok: true, status: resp.status};
	})()`
	var out struct {
		OK     bool   `json:"ok"`
		Status int    `json:"status"`
		Body   string `json:"body"`
		Err    string `json:"err"`
	}
	awaitOpt := func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &out, awaitOpt)); err != nil {
		return fmt.Errorf("eval refresh: %w", err)
	}
	if !out.OK {
		detail := out.Err
		if detail == "" {
			detail = fmt.Sprintf("status=%d body=%s", out.Status, truncateBody(out.Body, 200))
		}
		return fmt.Errorf("%w: %s", ErrRefreshFailed, detail)
	}
	return nil
}

// truncateBody bounds an error string so a giant HTML response body
// doesn't make the log line unreadable.
func truncateBody(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

type HoldResult struct {
	OrderID string
	Raw     map[string]any
	// Timings record per-phase wall-clock so the caller can feed
	// Prometheus histograms without re-instrumenting the chromedp internals.
	// Each duration is zero if the corresponding phase never started (e.g.
	// nav failed before recaptcha wait).
	Timings HoldTimings
	// Path names which Pool execution branch produced this result. Set by
	// Pool.HoldOnRequestTab so the manager can tell the user (and the log
	// stream) whether the fast warm-tab path won the race or we had to
	// fall back to a full nav.
	Path HoldPath
}

// HoldPath enumerates which Pool branch ran the hold. The manager surfaces
// this in the booking result DM and in the auto-booking log line so an
// operator can tell at a glance whether the warm-tab plumbing actually
// kicked in for this attempt.
type HoldPath string

const (
	// HoldPathWarmTabFast: dedicated warm tab was parked on the schniff's
	// campground page; HoldFast skipped Nav + recaptcha wait entirely.
	// Floor case (~1.15s = recaptcha mint + rec.gov POST).
	HoldPathWarmTabFast HoldPath = "warm_tab_fast"
	// HoldPathMainSession: no warm tab was registered for this schniff
	// (e.g. the reconcile loop hasn't ticked yet, the user has no active
	// auto-book schniffs, or — rare — the schniff's campground changed
	// mid-flight). Fell through to the all-in-one HoldCampsite on the
	// main session.
	HoldPathMainSession HoldPath = "main_session"
	// HoldPathRelaunchRetry: the warm tab's session JWT silently
	// expired; Pool relaunched (re-login) and replayed the hold against
	// the fresh session. Warm tabs are rebuilt by the reconcile loop on
	// its next tick.
	HoldPathRelaunchRetry HoldPath = "relaunch_retry"
)

// Human returns a short tag suitable for the user-facing booking DM.
func (h HoldPath) Human() string {
	switch h {
	case HoldPathWarmTabFast:
		return "⚡ warm tab (fast path)"
	case HoldPathMainSession:
		return "🐌 cold start (no warm tab)"
	case HoldPathRelaunchRetry:
		return "♻️ relaunch + retry"
	case "":
		return ""
	}
	return string(h)
}

// HoldTimings carries fine-grained latency breakdown for one HoldCampsite call.
// In-page durations (RecaptchaToken, RecGovPost) come from performance.now()
// markers inside the injected script — they isolate the recaptcha enterprise
// token mint and the actual booking POST from script-eval overhead, which
// otherwise all collapse into ScriptEval.
type HoldTimings struct {
	Nav            time.Duration // chromedp.Navigate + WaitReady
	RecaptchaWait  time.Duration // Poll waiting for grecaptcha.enterprise.execute to be defined
	SessionCheck   time.Duration // Read recaccount from localStorage
	ScriptEval     time.Duration // Full Evaluate(script) round trip (includes RecaptchaToken+RecGovPost)
	RecaptchaToken time.Duration // grecaptcha.enterprise.execute alone (from in-page perf.now)
	RecGovPost     time.Duration // fetch(...) + body read alone (from in-page perf.now)
	Total          time.Duration // Nav + RecaptchaWait + SessionCheck + ScriptEval
}

// HoldCampsite navigates to the campsite page, mints a fresh reCAPTCHA
// Enterprise token in-page, and POSTs to the multi-reservation endpoint.
func (s *Session) HoldCampsite(ctx context.Context, campsiteID, campgroundID string, checkIn, checkOut time.Time) (*HoldResult, error) {
	const iso = "2006-01-02T00:00:00.000Z"
	if !checkIn.Before(checkOut) {
		return nil, errors.New("check-in must be before check-out")
	}
	t := &HoldTimings{}
	totalStart := time.Now()
	// finalize: must run before each return that builds a HoldResult, so
	// the embedded Timings.Total is meaningful (value copy at return time).
	finalize := func() {
		t.Total = time.Since(totalStart)
		logPhase(ctx, "hold_total", totalStart, t.Total, nil)
	}

	navStart := time.Now()
	navErr := chromedp.Run(ctx,
		chromedp.Navigate(fmt.Sprintf("%s/camping/campsites/%s", SiteOrigin, campsiteID)),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
	t.Nav = time.Since(navStart)
	logPhase(ctx, "nav", navStart, t.Nav, navErr)
	if navErr != nil {
		finalize()
		return &HoldResult{Timings: *t}, fmt.Errorf("nav: %w", navErr)
	}

	recaptchaStart := time.Now()
	recErr := chromedp.Run(ctx, chromedp.Poll(
		`typeof grecaptcha !== 'undefined' && grecaptcha.enterprise && typeof grecaptcha.enterprise.execute === 'function'`,
		nil, chromedp.WithPollingTimeout(30*time.Second),
	))
	t.RecaptchaWait = time.Since(recaptchaStart)
	logPhase(ctx, "recaptcha_wait", recaptchaStart, t.RecaptchaWait, recErr)
	if recErr != nil {
		finalize()
		return &HoldResult{Timings: *t}, fmt.Errorf("wait recaptcha: %w", recErr)
	}

	// Verify the JWT is still in localStorage before we burn a reCAPTCHA
	// token. rec.gov silently clears 'recaccount' on session expiry while
	// Chrome stays alive, so the chromedp ctx check in the pool is not
	// sufficient — surface this as ErrNotLoggedIn so the pool can re-login
	// and retry.
	sessionStart := time.Now()
	var hasAccount bool
	sErr := chromedp.Run(ctx, chromedp.Evaluate(
		`!!(window.localStorage.getItem('recaccount'))`, &hasAccount,
	))
	t.SessionCheck = time.Since(sessionStart)
	logPhase(ctx, "session_check", sessionStart, t.SessionCheck, sErr)
	if sErr != nil {
		finalize()
		return &HoldResult{Timings: *t}, fmt.Errorf("check session: %w", sErr)
	}
	if !hasAccount {
		finalize()
		return &HoldResult{Timings: *t}, ErrNotLoggedIn
	}
	nightMap := map[string]map[string]string{}
	for day := checkIn; day.Before(checkOut); day = day.AddDate(0, 0, 1) {
		nightMap[day.UTC().Format(iso)] = map[string]string{
			"campsite_id":   campsiteID,
			"campsite_loop": "",
			"campsite_name": "",
		}
	}
	payload := map[string]any{
		"campsiteID":   campsiteID,
		"campgroundID": campgroundID,
		"checkIn":      checkIn.UTC().Format(iso),
		"checkOut":     checkOut.UTC().Format(iso),
		"nightMap":     nightMap,
		"siteKey":      RecaptchaSiteKey,
		"action":       RecaptchaAction,
	}
	payloadJSON, _ := json.Marshal(payload)
	script := `(async (p) => {
		const _t0 = performance.now();
		const token = await grecaptcha.enterprise.execute(p.siteKey, {action: p.action});
		const _tokenMs = performance.now() - _t0;
		const recRaw = window.localStorage.getItem('recaccount');
		if (!recRaw) throw new Error('no recaccount in localStorage');
		const rec = JSON.parse(recRaw);
		const accessToken = rec.access_token;
		const accountID = rec.account && rec.account.account_id;
		if (!accessToken || !accountID) throw new Error('missing access_token or account_id in recaccount');
		const body = {
			reservations: [{
				account_id: accountID,
				campsite_id: p.campsiteID,
				check_in: p.checkIn,
				check_out: p.checkOut,
				reservation_options: {
					night_map: p.nightMap,
					recommendation_referrer: 'campground-vnull:campsitePage',
				},
			}],
			gate_a: {
				value: token,
				description: p.action,
				success: true,
				terminal: 'east',
			},
		};
		const _postStart = performance.now();
		const response = await fetch('/api/camps/reservations/campgrounds/' + p.campgroundID + '/multi', {
			method: 'POST',
			headers: {
				'content-type': 'application/json',
				'authorization': 'Bearer ' + accessToken,
			},
			body: JSON.stringify(body),
			credentials: 'include',
		});
		const text = await response.text();
		const _postMs = performance.now() - _postStart;
		let parsed;
		try { parsed = JSON.parse(text); } catch (_) { parsed = {raw: text}; }
		return {status: response.status, _recaptcha_token_ms: _tokenMs, _recgov_post_ms: _postMs, ...parsed};
	})(` + string(payloadJSON) + `)`
	var out map[string]any
	awaitOpt := func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }
	scriptStart := time.Now()
	scriptErr := chromedp.Run(ctx, chromedp.Evaluate(script, &out, awaitOpt))
	t.ScriptEval = time.Since(scriptStart)
	logPhase(ctx, "script_eval", scriptStart, t.ScriptEval, scriptErr)
	if scriptErr != nil {
		finalize()
		return &HoldResult{Timings: *t}, fmt.Errorf("exec booking: %w", scriptErr)
	}
	t.RecaptchaToken = readMillis(out, "_recaptcha_token_ms")
	t.RecGovPost = readMillis(out, "_recgov_post_ms")
	if t.RecaptchaToken > 0 {
		logPhase(ctx, "recaptcha_token_mint", scriptStart, t.RecaptchaToken, nil)
	}
	if t.RecGovPost > 0 {
		logPhase(ctx, "recgov_post", scriptStart.Add(t.RecaptchaToken), t.RecGovPost, nil)
	}
	finalize()
	result := &HoldResult{OrderID: ExtractOrderID(out), Raw: out, Timings: *t}
	if err := bookingResponseError(out, result.OrderID); err != nil {
		return result, err
	}
	return result, nil
}

// readMillis pulls a duration off the in-page perf markers our injected
// script returns. Missing/garbage values return 0 — observers treat 0 as
// "didn't reach this phase" rather than "phase took 0s".
func readMillis(m map[string]any, key string) time.Duration {
	v, ok := m[key]
	if !ok {
		return 0
	}
	f, ok := v.(float64)
	if !ok || f < 0 {
		return 0
	}
	return time.Duration(f * float64(time.Millisecond))
}

func bookingResponseError(response map[string]any, orderID string) error {
	status, _ := response["status"].(float64)
	if status < 200 || status >= 300 {
		body, _ := json.Marshal(response)
		slog.Warn("rec.gov booking rejected", slog.Any("status", response["status"]), slog.String("body", string(body)))
		msg, _ := response["error"].(string)
		// 401 specifically means our Bearer access_token was rejected
		// (e.g., its TTL expired while the tab sat warm). Surface as
		// ErrAuthExpired so the Pool can re-login in place on the same
		// Chrome session and retry on the warm tab — much cheaper than
		// the full ErrNotLoggedIn relaunch path (~3s vs ~10s).
		if int(status) == 401 {
			detail := msg
			if detail == "" {
				detail = string(body)
			}
			return fmt.Errorf("%w: %s", ErrAuthExpired, detail)
		}
		if msg != "" {
			return fmt.Errorf("rec.gov %v: %s", response["status"], msg)
		}
		return fmt.Errorf("rec.gov returned status %v: %s", response["status"], string(body))
	}
	message, _ := response["error"].(string)
	if ok, exists := response["ok"].(bool); exists && !ok {
		if strings.Contains(strings.ToLower(message), "abnormal activity") {
			return fmt.Errorf("%w: %s", ErrHumanVerification, message)
		}
		if message == "" {
			message = "booking request was rejected"
		}
		return errors.New(message)
	}
	if message != "" {
		return errors.New(message)
	}
	if orderID == "" {
		return errors.New("booking response contained no reservation id")
	}
	return nil
}

// ExtractOrderID returns the identifier used by rec.gov's order-details route.
func ExtractOrderID(m map[string]any) string {
	for _, k := range []string{"reservation_id", "order_id", "id"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	if rs, ok := m["reservations"].([]any); ok && len(rs) > 0 {
		if r, ok := rs[0].(map[string]any); ok {
			for _, k := range []string{"id", "order_id", "reservation_id"} {
				if v, ok := r[k].(string); ok && v != "" {
					return v
				}
			}
			if inner, ok := r["reservation"].(map[string]any); ok {
				for _, k := range []string{"reservation_id", "id", "order_id"} {
					if v, ok := inner[k].(string); ok && v != "" {
						return v
					}
				}
			}
		}
	}
	return ""
}

func findLoginFields(ctx context.Context) (email, password, submit string, err error) {
	js := `(() => {
		const known = [
			['#rec-acct-sign-in-email-address', '#rec-acct-sign-in-password'],
			['#email', '#password'],
		];
		for (const [e, p] of known) {
			if (document.querySelector(e) && document.querySelector(p)) {
				return {email: e, password: p};
			}
		}
		const inputs = Array.from(document.querySelectorAll('input'))
			.filter(i => i.offsetParent !== null);
		const e = inputs.find(i => i.type === 'email' || /email/i.test(i.name || i.id || ''));
		const p = inputs.find(i => i.type === 'password');
		if (!e || !p) return null;
		return {email: '#' + (e.id || ''), password: '#' + (p.id || '')};
	})()`
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var found map[string]string
		if e := chromedp.Run(ctx, chromedp.Evaluate(js, &found)); e != nil {
			return "", "", "", e
		}
		if found != nil && found["email"] != "#" && found["password"] != "#" {
			subJS := `(() => {
				const btns = Array.from(document.querySelectorAll('button'));
				const signin = btns.find(b => /sign\s*in/i.test(b.textContent || ''));
				if (signin && signin.id) return '#' + signin.id;
				const submit = btns.find(b => b.type === 'submit' && b.offsetParent !== null);
				if (submit && submit.id) return '#' + submit.id;
				return 'button[type="submit"]';
			})()`
			var sub string
			if e := chromedp.Run(ctx, chromedp.Evaluate(subJS, &sub)); e != nil {
				return "", "", "", e
			}
			return found["email"], found["password"], sub, nil
		}
		select {
		case <-ctx.Done():
			return "", "", "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return "", "", "", errors.New("login fields not found")
}

func AutodetectChrome() string {
	for _, c := range []string{
		"/usr/bin/google-chrome-stable",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// OrderURL returns the order details URL for a held reservation.
func OrderURL(orderID string) string {
	return fmt.Sprintf("%s/camping/reservations/orderdetails?id=%s", SiteOrigin, orderID)
}

// CampsiteBookingURL opens rec.gov's normal add-to-cart UI with dates filled.
// This gives users a manual fallback when rec.gov requires visible CAPTCHA.
func CampsiteBookingURL(campsiteID string, checkIn, checkOut time.Time) string {
	query := url.Values{
		"startDate": {checkIn.Format("01/02/2006")},
		"endDate":   {checkOut.Format("01/02/2006")},
		"book_now":  {"true"},
	}
	return fmt.Sprintf("%s/camping/campsites/%s?%s", SiteOrigin, campsiteID, query.Encode())
}
