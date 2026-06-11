package booker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// loggerKey carries a slog.Logger pre-bound with booking-correlation fields
// (correlation_id, user_id, campsite_id, request_id, etc) so internal
// phase-completion lines emit those attributes without each helper re-deriving
// them. Callers attach via WithLogger; absent context returns slog.Default().
type loggerKey struct{}

// WithLogger returns a copy of ctx carrying lg as the booking phase logger.
// Each phase emits a single "booking_phase" log line at completion with
// phase + duration, plus whatever fields lg already has bound.
func WithLogger(ctx context.Context, lg *slog.Logger) context.Context {
	if lg == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerKey{}, lg)
}

// newCorrelationID returns a short opaque token suitable for correlating
// every phase line emitted during a single booking. Not security-sensitive;
// short on purpose so grep'd logs stay readable.
func newCorrelationID() string {
	return fmt.Sprintf("bkg-%d", time.Now().UnixNano()%1_000_000_000)
}

func loggerFrom(ctx context.Context) *slog.Logger {
	if v := ctx.Value(loggerKey{}); v != nil {
		if lg, ok := v.(*slog.Logger); ok {
			return lg
		}
	}
	return slog.Default()
}

// logPhase emits the standard booking-phase line. Called at the end of every
// timed section so an operator can reconstruct the exact wall-clock ordering
// of a single booking from grep'd logs.
func logPhase(ctx context.Context, phase string, started time.Time, d time.Duration, err error) {
	lg := loggerFrom(ctx)
	attrs := []any{
		slog.String("phase", phase),
		slog.Time("started_at", started),
		slog.Duration("duration", d),
	}
	if err != nil {
		attrs = append(attrs, slog.String("err", err.Error()))
		lg.Warn("booking_phase", attrs...)
		return
	}
	lg.Info("booking_phase", attrs...)
}

// PrewarmCampground navigates to the campground overview page and waits
// until grecaptcha.enterprise.execute is callable. After this returns, a
// follow-up HoldFast on this tab can book *any* campsite in this
// campground — the booking POST URL is keyed by campgroundID, and the
// recaptcha token is bound to the action ("campsiteListBooking") not the
// URL. Verified empirically: hold POSTs from /camping/campgrounds/{id}
// succeed against rec.gov with STATUS=200 + order_id.
//
// This is the parking strategy for per-schniff warm tabs: one tab per
// schniff, parked on its campground page once, never re-navigated. When
// arbitration picks any campsite in that campground we HoldFast on the
// same tab.
//
// Safe to call repeatedly; if the tab is already on the right page and
// recaptcha is loaded, the second call only re-checks the readiness probe.
func PrewarmCampground(ctx context.Context, campgroundID string) (PrewarmTimings, error) {
	var t PrewarmTimings
	navStart := time.Now()
	navErr := chromedp.Run(ctx,
		chromedp.Navigate(fmt.Sprintf("%s/camping/campgrounds/%s", SiteOrigin, campgroundID)),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
	t.Nav = time.Since(navStart)
	logPhase(ctx, "prewarm_nav", navStart, t.Nav, navErr)
	if navErr != nil {
		return t, fmt.Errorf("nav: %w", navErr)
	}

	recStart := time.Now()
	recErr := chromedp.Run(ctx, chromedp.Poll(
		`typeof grecaptcha !== 'undefined' && grecaptcha.enterprise && typeof grecaptcha.enterprise.execute === 'function'`,
		nil, chromedp.WithPollingTimeout(30*time.Second),
	))
	t.RecaptchaWait = time.Since(recStart)
	logPhase(ctx, "prewarm_recaptcha_wait", recStart, t.RecaptchaWait, recErr)
	if recErr != nil {
		return t, fmt.Errorf("wait recaptcha: %w", recErr)
	}
	return t, nil
}

// PrewarmCampsite is retained for callers (cmd/booker-bench) that want to
// park on a specific campsite page. New code should prefer
// PrewarmCampground — booking POSTs work the same from either page, and
// the campground page lets one tab serve every campsite in the campground
// without re-navigating.
func PrewarmCampsite(ctx context.Context, campsiteID string) (PrewarmTimings, error) {
	var t PrewarmTimings
	navStart := time.Now()
	navErr := chromedp.Run(ctx,
		chromedp.Navigate(fmt.Sprintf("%s/camping/campsites/%s", SiteOrigin, campsiteID)),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
	t.Nav = time.Since(navStart)
	logPhase(ctx, "prewarm_nav", navStart, t.Nav, navErr)
	if navErr != nil {
		return t, fmt.Errorf("nav: %w", navErr)
	}

	recStart := time.Now()
	recErr := chromedp.Run(ctx, chromedp.Poll(
		`typeof grecaptcha !== 'undefined' && grecaptcha.enterprise && typeof grecaptcha.enterprise.execute === 'function'`,
		nil, chromedp.WithPollingTimeout(30*time.Second),
	))
	t.RecaptchaWait = time.Since(recStart)
	logPhase(ctx, "prewarm_recaptcha_wait", recStart, t.RecaptchaWait, recErr)
	if recErr != nil {
		return t, fmt.Errorf("wait recaptcha: %w", recErr)
	}
	return t, nil
}

// PrewarmTimings carries latency for the pre-payable portion of a hold.
type PrewarmTimings struct {
	Nav           time.Duration
	RecaptchaWait time.Duration
}

// HoldFast performs only the booking-critical portion: session check + the
// recaptcha mint + POST script. It does NOT navigate — caller must have
// already loaded a page where grecaptcha.enterprise is initialized
// (typically via PrewarmCampsite).
func HoldFast(ctx context.Context, campsiteID, campgroundID string, checkIn, checkOut time.Time) (*HoldResult, error) {
	const iso = "2006-01-02T00:00:00.000Z"
	if !checkIn.Before(checkOut) {
		return nil, errors.New("check-in must be before check-out")
	}
	t := &HoldTimings{}
	totalStart := time.Now()
	// finalize captures the final wall time on every return path so
	// res.Timings.Total is meaningful (and the matching phase log fires).
	// Caller-visible HoldResult.Timings is a value copy of *t at the moment
	// of return, so we must set Total BEFORE constructing the result.
	finalize := func() {
		t.Total = time.Since(totalStart)
		logPhase(ctx, "hold_fast_total", totalStart, t.Total, nil)
	}

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
			"campsite_id": campsiteID, "campsite_loop": "", "campsite_name": "",
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
			gate_a: {value: token, description: p.action, success: true, terminal: 'east'},
		};
		const _postStart = performance.now();
		const response = await fetch('/api/camps/reservations/campgrounds/' + p.campgroundID + '/multi', {
			method: 'POST',
			headers: {'content-type': 'application/json', 'authorization': 'Bearer ' + accessToken},
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
	// The in-page perf markers are the source of truth for the script's
	// internal breakdown; emit them as phase lines so they appear in the
	// same correlated stream as everything else.
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

// Tab represents an additional Chrome tab in an existing Session. Each tab
// has its own page/target so multiple tabs can be parked on different
// campsite pages and fired independently.
type Tab struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// NewTab opens a new tab in the session's Chrome instance.
func (s *Session) NewTab() (*Tab, error) {
	ctx, cancel := chromedp.NewContext(s.ctx)
	if err := chromedp.Run(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("open tab: %w", err)
	}
	return &Tab{ctx: ctx, cancel: cancel}, nil
}

func (t *Tab) Ctx() context.Context { return t.ctx }
func (t *Tab) Close()                { t.cancel() }
