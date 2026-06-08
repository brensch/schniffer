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

// ErrSiteUnavailable is returned when rec.gov rejects the booking with a 400.
// In practice this means the site was taken between our availability poll and
// our POST — someone else (often another bot) got there first.
var ErrSiteUnavailable = errors.New("site no longer available")

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

type HoldResult struct {
	OrderID string
	Raw     map[string]any
}

// HoldCampsite navigates to the campsite page, mints a fresh reCAPTCHA
// Enterprise token in-page, and POSTs to the multi-reservation endpoint.
func (s *Session) HoldCampsite(ctx context.Context, campsiteID, campgroundID string, checkIn, checkOut time.Time) (*HoldResult, error) {
	const iso = "2006-01-02T00:00:00.000Z"
	if !checkIn.Before(checkOut) {
		return nil, errors.New("check-in must be before check-out")
	}
	if err := chromedp.Run(ctx,
		chromedp.Navigate(fmt.Sprintf("%s/camping/campsites/%s", SiteOrigin, campsiteID)),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return nil, fmt.Errorf("nav: %w", err)
	}
	if err := chromedp.Run(ctx, chromedp.Poll(
		`typeof grecaptcha !== 'undefined' && grecaptcha.enterprise && typeof grecaptcha.enterprise.execute === 'function'`,
		nil, chromedp.WithPollingTimeout(30*time.Second),
	)); err != nil {
		return nil, fmt.Errorf("wait recaptcha: %w", err)
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
		const token = await grecaptcha.enterprise.execute(p.siteKey, {action: p.action});
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
		let parsed;
		try { parsed = JSON.parse(text); } catch (_) { parsed = {raw: text}; }
		return {status: response.status, ...parsed};
	})(` + string(payloadJSON) + `)`
	var out map[string]any
	awaitOpt := func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &out, awaitOpt)); err != nil {
		return nil, fmt.Errorf("exec booking: %w", err)
	}
	result := &HoldResult{OrderID: ExtractOrderID(out), Raw: out}
	if err := bookingResponseError(out, result.OrderID); err != nil {
		return result, err
	}
	return result, nil
}

func bookingResponseError(response map[string]any, orderID string) error {
	status, _ := response["status"].(float64)
	if status < 200 || status >= 300 {
		body, _ := json.Marshal(response)
		slog.Warn("rec.gov booking rejected", slog.Any("status", response["status"]), slog.String("body", string(body)))
		if int(status) == 400 {
			return fmt.Errorf("%w (rec.gov 400)", ErrSiteUnavailable)
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
