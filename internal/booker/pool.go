package booker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/brensch/schniffer/internal/metrics"
	"github.com/chromedp/chromedp"
)

// CredentialLookup returns the email + plaintext password for userID. The
// pool calls it on warmup and on relogin; we never store plaintext outside
// the lookup call. Return ("", "", nil) if the user has no credential.
type CredentialLookup func(ctx context.Context, userID string) (email, password string, err error)

// DisableCallback is invoked when a user's credentials are determined to be
// bad (login fails). It should mark the credential disabled in storage and
// notify the user. The pool will not retry that user until it's reset.
type DisableCallback func(ctx context.Context, userID, reason string)

type PoolConfig struct {
	BaseProfileDir   string // e.g. ".cache/recgov-profiles"; per-user dir is <base>/<userID>
	ChromePath       string
	LookupCredential CredentialLookup
	OnDisable        DisableCallback
	RefreshInterval  time.Duration // 0 = default 25min
	WatchdogInterval time.Duration // 0 = default 60s; how often we check sessions for liveness
	Logger           *slog.Logger
}

// Pool is the always-warm browser pool. One Chrome instance per linked user,
// booted at startup and self-healing: a dead session (Chrome process gone /
// chromedp ctx cancelled) is detected and replaced on demand, so a single
// crash doesn't permanently break auto-booking for that user.
type Pool struct {
	cfg PoolConfig

	mu       sync.RWMutex
	sessions map[string]*entry // keyed by userID
}

type entry struct {
	mu       sync.Mutex // serializes operations on the session's main tab.
	session  *Session
	disabled bool
	// prewarmedSite is the campsiteID currently navigated + recaptcha-ready
	// on this session's main tab. "" means no prewarm or stale (we cleared
	// it after the last booking POST). HoldCampsiteFast uses this to decide
	// whether it can skip the ~1.1s nav.
	prewarmedSite string

	// tabsMu guards the schniff-tabs map. Tabs themselves are independent
	// chromedp contexts; each has its own mutex (warmTab.mu) so per-tab
	// operations don't serialize with each other or with the main tab.
	tabsMu sync.Mutex
	tabs   map[int64]*warmTab // keyed by schniff_request.id
}

// warmTab is a dedicated Chrome tab pre-navigated to the schniff's
// campground overview page (/camping/campgrounds/{id}) for one active
// schniff request. The tab stays alive for the schniff's lifetime.
// Empirically verified: a hold POST minted from the campground page is
// accepted by rec.gov (STATUS=200) for any campsite in that campground —
// the booking POST URL is keyed by campgroundID and the recaptcha token
// is action-bound, not URL-bound. So one tab serves every candidate
// campsite in the schniff's campground without ever re-navigating.
type warmTab struct {
	mu         sync.Mutex // serializes Prewarm + Hold on this tab
	tab        *Tab
	campground string // last campground the tab was navigated to ("" until first Prewarm)
	closed     bool
}

func NewPool(cfg PoolConfig) *Pool {
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 25 * time.Minute
	}
	if cfg.WatchdogInterval == 0 {
		cfg.WatchdogInterval = 60 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Pool{cfg: cfg, sessions: map[string]*entry{}}
}

// StartUser launches Chrome for userID and logs in. Safe to call multiple
// times — subsequent calls no-op if a healthy session already exists.
func (p *Pool) StartUser(ctx context.Context, userID string) error {
	p.mu.RLock()
	e, ok := p.sessions[userID]
	p.mu.RUnlock()
	if ok {
		e.mu.Lock()
		alive := !e.disabled && sessionAlive(e.session)
		e.mu.Unlock()
		if alive {
			return nil
		}
	}
	return p.launchUser(ctx, userID)
}

// launchUser unconditionally opens a fresh Chrome + login for userID. If an
// entry already exists, the old session is closed and replaced. Used both
// by StartUser (initial warmup) and by the self-healing path inside
// HoldCampsite / RunWatchdog.
func (p *Pool) launchUser(ctx context.Context, userID string) error {
	email, password, err := p.cfg.LookupCredential(ctx, userID)
	if err != nil {
		return fmt.Errorf("lookup credential: %w", err)
	}
	if email == "" {
		return errors.New("no credential")
	}
	profile := filepath.Join(p.cfg.BaseProfileDir, userID)
	sess, err := Open(Config{ProfileDir: profile, ChromePath: p.cfg.ChromePath})
	if err != nil {
		return fmt.Errorf("open chrome: %w", err)
	}
	loginCtx, cancel := context.WithTimeout(sess.Ctx(), 90*time.Second)
	defer cancel()
	if err := sess.Login(loginCtx, email, password); err != nil {
		sess.Close()
		if errors.Is(err, ErrBadCredentials) && p.cfg.OnDisable != nil {
			p.cfg.OnDisable(ctx, userID, "login failed during pool warmup")
		}
		return fmt.Errorf("login: %w", err)
	}

	// Swap in the new session; close any prior corpse.
	p.mu.Lock()
	old, existed := p.sessions[userID]
	p.sessions[userID] = &entry{session: sess}
	p.mu.Unlock()
	if existed && old != nil {
		closeEntryTabs(old)
		if old.session != nil {
			old.session.Close()
		}
	}
	p.cfg.Logger.Info("browser warm", "user", userID, "relaunched", existed)
	return nil
}

// StartAll boots all provided users in parallel. Per-user errors are logged
// but do not block other users; returns once every launch attempt completes.
func (p *Pool) StartAll(ctx context.Context, userIDs []string) {
	var wg sync.WaitGroup
	for _, uid := range userIDs {
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			if err := p.StartUser(ctx, uid); err != nil {
				p.cfg.Logger.Warn("warmup failed", "user", uid, "err", err)
			}
		}(uid)
	}
	wg.Wait()
}

// StopUser closes the Chrome session for userID and forgets it. Used by
// /schniff unlink and bad-creds handling.
func (p *Pool) StopUser(userID string) {
	p.mu.Lock()
	e, ok := p.sessions[userID]
	delete(p.sessions, userID)
	p.mu.Unlock()
	if ok {
		closeEntryTabs(e)
		if e.session != nil {
			e.session.Close()
		}
	}
}

// closeEntryTabs closes any warm tabs attached to the entry. Idempotent
// per-tab (warmTab.closed guard). Called whenever a session is being torn
// down or replaced — the new session can't reuse the old session's chromedp
// contexts, so the tabs must die with the session.
func closeEntryTabs(e *entry) {
	if e == nil {
		return
	}
	e.tabsMu.Lock()
	tabs := e.tabs
	e.tabs = nil
	e.tabsMu.Unlock()
	for _, wt := range tabs {
		wt.mu.Lock()
		if !wt.closed {
			wt.closed = true
			wt.tab.Close()
		}
		wt.mu.Unlock()
	}
}

// Close tears down every session.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.sessions {
		closeEntryTabs(e)
		if e.session != nil {
			e.session.Close()
		}
	}
	p.sessions = map[string]*entry{}
}

// HoldCampsite performs a booking for userID. Serialized per-user; if the
// underlying Chrome has died (chromedp ctx cancelled), the session is
// transparently relaunched and the booking proceeds on the fresh session.
func (p *Pool) HoldCampsite(ctx context.Context, userID, campsiteID, campgroundID string, checkIn, checkOut time.Time) (*HoldResult, error) {
	ctx = WithLogger(ctx, p.cfg.Logger.With(
		"correlation_id", newCorrelationID(),
		"user_id", userID,
		"campsite_id", campsiteID,
		"campground_id", campgroundID,
	))
	res, err := p.holdOnce(ctx, userID, campsiteID, campgroundID, checkIn, checkOut)
	if !errors.Is(err, ErrNotLoggedIn) {
		observeHoldMetrics(res, err)
		// Path is set by the caller wrapper if it knows; if not (direct
		// HoldCampsite caller, no warm-tab context), tag as main_session.
		if res != nil && res.Path == "" {
			res.Path = HoldPathMainSession
		}
		return res, err
	}
	// Session JWT expired silently. Force a full relaunch (login) and try
	// once more on the fresh session.
	p.cfg.Logger.Warn("hold: session not logged in; relaunching and retrying", "user", userID)
	metrics.BookerRelaunchTotal.WithLabelValues("session_expired").Inc()
	if relaunchErr := p.launchUser(ctx, userID); relaunchErr != nil {
		return nil, fmt.Errorf("relaunch after session expiry: %w", relaunchErr)
	}
	res, err = p.holdOnce(ctx, userID, campsiteID, campgroundID, checkIn, checkOut)
	observeHoldMetrics(res, err)
	if res != nil {
		res.Path = HoldPathRelaunchRetry
	}
	return res, err
}

// observeHoldMetrics emits per-phase histograms for one HoldCampsite call.
// Skipped phases (zero duration) are still recorded so a sequence of zeros
// is visible in the histogram and easy to filter in Grafana.
func observeHoldMetrics(res *HoldResult, err error) {
	if res == nil {
		return
	}
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	t := res.Timings
	obs := func(phase string, d time.Duration) {
		if d <= 0 {
			return
		}
		metrics.BookerHoldDuration.
			WithLabelValues(phase, outcome).
			Observe(d.Seconds())
	}
	obs("nav", t.Nav)
	obs("recaptcha_wait", t.RecaptchaWait)
	obs("session_check", t.SessionCheck)
	obs("script_eval", t.ScriptEval)
	obs("recaptcha_token", t.RecaptchaToken)
	obs("recgov_post", t.RecGovPost)
	obs("total", t.Total)
}

func (p *Pool) holdOnce(ctx context.Context, userID, campsiteID, campgroundID string, checkIn, checkOut time.Time) (*HoldResult, error) {
	e, err := p.ensureAlive(ctx, userID)
	if err != nil {
		return nil, err
	}
	waitStart := time.Now()
	e.mu.Lock()
	metrics.BookerSessionWait.Observe(time.Since(waitStart).Seconds())
	defer e.mu.Unlock()
	if e.disabled {
		return nil, errors.New("session disabled")
	}
	// If the tab is already prewarmed on this site, skip the Nav +
	// RecaptchaWait phases and go straight to the booking script. The
	// recaptcha token is consumed on success, but the grecaptcha binding
	// stays loaded — leave prewarmedSite as-is so a follow-up hold on the
	// same site still skips Nav. Clear it when the booked site differs (we
	// definitely navigated) so the next call doesn't lie about state.
	opCtx, cancel := sessionOperationContext(e.session.Ctx(), ctx)
	defer cancel()
	if e.prewarmedSite == campsiteID {
		return HoldFast(opCtx, campsiteID, campgroundID, checkIn, checkOut)
	}
	e.prewarmedSite = campsiteID
	return e.session.HoldCampsite(opCtx, campsiteID, campgroundID, checkIn, checkOut)
}

// EnsureWarmTabForRequest guarantees there is a dedicated Chrome tab open
// for this (userID, requestID) tuple, parked on the schniff's campground
// overview page (/camping/campgrounds/{campgroundID}) with grecaptcha
// loaded. From that tab a follow-up HoldOnRequestTab can book *any*
// campsite in the campground via HoldFast (no re-navigation needed).
// Idempotent: no-ops if the tab is already on this campground.
//
// One-tab-per-schniff is intentional. The manager keeps the tabs alive
// across poll cycles so HoldOnRequestTab pays only the script_eval cost
// (~1.15s = recaptcha mint + rec.gov POST). Tab is closed via
// CloseRequestTab when the schniff is deactivated.
func (p *Pool) EnsureWarmTabForRequest(ctx context.Context, userID string, requestID int64, campgroundID string) error {
	ctx = WithLogger(ctx, p.cfg.Logger.With(
		"correlation_id", newCorrelationID(),
		"user_id", userID,
		"request_id", requestID,
		"campground_id", campgroundID,
		"op", "ensure_warm_tab",
	))
	e, err := p.ensureAlive(ctx, userID)
	if err != nil {
		return err
	}
	e.tabsMu.Lock()
	if e.tabs == nil {
		e.tabs = map[int64]*warmTab{}
	}
	wt, ok := e.tabs[requestID]
	if !ok {
		t, err := e.session.NewTab()
		if err != nil {
			e.tabsMu.Unlock()
			return fmt.Errorf("open tab for request %d: %w", requestID, err)
		}
		wt = &warmTab{tab: t}
		e.tabs[requestID] = wt
	}
	e.tabsMu.Unlock()

	wt.mu.Lock()
	defer wt.mu.Unlock()
	if wt.closed {
		return errors.New("warm tab closed")
	}
	if wt.campground == campgroundID {
		return nil
	}
	// Bound the prewarm so a stuck nav can't block the background loop
	// forever. 60s matches the chromedp Poll budget inside PrewarmCampground.
	pctx, pcancel := context.WithTimeout(wt.tab.Ctx(), 60*time.Second)
	defer pcancel()
	opCtx, opcancel := sessionOperationContext(pctx, ctx)
	defer opcancel()
	if _, err := PrewarmCampground(opCtx, campgroundID); err != nil {
		return fmt.Errorf("prewarm tab: %w", err)
	}
	wt.campground = campgroundID
	return nil
}

// HoldOnRequestTab performs a hold using the warm tab dedicated to
// requestID. If the warm tab matches campsiteID, runs HoldFast (skips Nav
// + recaptcha wait, ~1.15s on prod). If campsiteID differs (rare: schniff
// chose a different candidate than the tab is currently on), navigates the
// tab to the new site first. If there is no warm tab for this request,
// falls back to the main session's HoldCampsite path.
//
// Returns ErrNotLoggedIn the same way HoldCampsite does — caller (or the
// retry layer) is responsible for re-login + retry.
func (p *Pool) HoldOnRequestTab(ctx context.Context, userID string, requestID int64, campsiteID, campgroundID string, checkIn, checkOut time.Time) (*HoldResult, error) {
	ctx = WithLogger(ctx, p.cfg.Logger.With(
		"correlation_id", newCorrelationID(),
		"user_id", userID,
		"request_id", requestID,
		"campsite_id", campsiteID,
		"campground_id", campgroundID,
		"op", "hold_on_request_tab",
	))
	e, err := p.ensureAlive(ctx, userID)
	if err != nil {
		return nil, err
	}
	e.tabsMu.Lock()
	wt := e.tabs[requestID]
	e.tabsMu.Unlock()
	if wt == nil {
		// No dedicated tab — fall back to the main tab.
		p.cfg.Logger.Warn("no warm tab; falling back to main session",
			"user_id", userID, "request_id", requestID, "campsite_id", campsiteID)
		res, err := p.HoldCampsite(ctx, userID, campsiteID, campgroundID, checkIn, checkOut)
		tagPath(res, HoldPathMainSession)
		p.logHoldPath(HoldPathMainSession, userID, requestID, campsiteID, res, err)
		return res, err
	}
	res, path, err := p.holdOnRequestTabOnce(ctx, e, wt, campsiteID, campgroundID, checkIn, checkOut, requestID, userID)
	if !errors.Is(err, ErrNotLoggedIn) {
		tagPath(res, path)
		p.logHoldPath(path, userID, requestID, campsiteID, res, err)
		return res, err
	}
	// The warm tab's JWT silently expired. The whole session is stale —
	// any other warm tab in this entry has the same problem. Force a full
	// relaunch + login (this also closes all warm tabs via closeEntryTabs
	// inside launchUser). The reconcile loop will rebuild the warm tabs
	// on its next tick; for THIS booking we fall back to a fresh
	// HoldCampsite on the new session so the user doesn't miss this race.
	p.cfg.Logger.Warn("warm-tab hold: session not logged in; relaunching and retrying",
		"user_id", userID, "request_id", requestID, "campsite_id", campsiteID)
	metrics.BookerRelaunchTotal.WithLabelValues("warm_tab_session_expired").Inc()
	if relaunchErr := p.launchUser(ctx, userID); relaunchErr != nil {
		return nil, fmt.Errorf("relaunch after warm-tab session expiry: %w", relaunchErr)
	}
	res, err = p.holdOnce(ctx, userID, campsiteID, campgroundID, checkIn, checkOut)
	tagPath(res, HoldPathRelaunchRetry)
	observeHoldMetrics(res, err)
	p.logHoldPath(HoldPathRelaunchRetry, userID, requestID, campsiteID, res, err)
	return res, err
}

// tagPath stamps the path onto the result if non-nil. Tolerates nil so
// callers don't have to guard every branch.
func tagPath(res *HoldResult, path HoldPath) {
	if res == nil {
		return
	}
	res.Path = path
}

// logHoldPath emits the single high-signal line summarising which Pool
// branch produced this hold. Separate from the per-phase booking_phase
// stream — operators grep this to find the path-choice decision.
func (p *Pool) logHoldPath(path HoldPath, userID string, requestID int64, campsiteID string, res *HoldResult, err error) {
	wall := time.Duration(0)
	orderID := ""
	if res != nil {
		wall = res.Timings.Total
		orderID = res.OrderID
	}
	attrs := []any{
		slog.String("path", string(path)),
		slog.String("user_id", userID),
		slog.Int64("request_id", requestID),
		slog.String("campsite_id", campsiteID),
		slog.Duration("wall", wall),
		slog.String("order_id", orderID),
	}
	if err != nil {
		attrs = append(attrs, slog.String("err", err.Error()))
		p.cfg.Logger.Warn("auto_book_path", attrs...)
		return
	}
	p.cfg.Logger.Info("auto_book_path", attrs...)
}

// holdOnRequestTabOnce runs HoldFast on the schniff's warm tab. Because
// the tab is parked on the campground overview page (not a specific
// campsite), any campsite_id in this campground works without a
// re-navigation. The mismatched-campsite branch from earlier iterations
// is gone; there is no scenario where this tab needs to navigate during
// a hold.
//
// If the warm tab's campground doesn't match the requested one (would
// happen only if a schniff was migrated between campgrounds, which is
// not currently supported), we fall back to the main-session
// HoldCampsite path rather than navigating the warm tab — keeping this
// function on the strict fast path.
func (p *Pool) holdOnRequestTabOnce(ctx context.Context, e *entry, wt *warmTab, campsiteID, campgroundID string, checkIn, checkOut time.Time, requestID int64, userID string) (*HoldResult, HoldPath, error) {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	if wt.closed {
		return nil, HoldPathMainSession, errors.New("warm tab closed")
	}
	if wt.campground != "" && wt.campground != campgroundID {
		p.cfg.Logger.Warn("warm tab campground mismatch; falling back to main session",
			"user_id", userID, "request_id", requestID,
			"tab_campground", wt.campground, "wanted_campground", campgroundID)
		res, err := p.HoldCampsite(ctx, userID, campsiteID, campgroundID, checkIn, checkOut)
		return res, HoldPathMainSession, err
	}
	opCtx, cancel := sessionOperationContext(wt.tab.Ctx(), ctx)
	defer cancel()
	res, err := HoldFast(opCtx, campsiteID, campgroundID, checkIn, checkOut)
	observeHoldMetrics(res, err)
	return res, HoldPathWarmTabFast, err
}

// CloseRequestTab closes the dedicated warm tab for (userID, requestID).
// Called by the manager when a schniff request is deactivated. Safe to
// call multiple times.
func (p *Pool) CloseRequestTab(userID string, requestID int64) {
	p.mu.RLock()
	e, ok := p.sessions[userID]
	p.mu.RUnlock()
	if !ok {
		return
	}
	e.tabsMu.Lock()
	wt, ok := e.tabs[requestID]
	if ok {
		delete(e.tabs, requestID)
	}
	e.tabsMu.Unlock()
	if !ok {
		return
	}
	wt.mu.Lock()
	if !wt.closed {
		wt.closed = true
		wt.tab.Close()
	}
	wt.mu.Unlock()
}

// ListRequestTabs returns the requestIDs currently holding a warm tab for
// userID. Used by the manager's reconcile loop to figure out which tabs
// to close (active schniff set is the source of truth).
func (p *Pool) ListRequestTabs(userID string) []int64 {
	p.mu.RLock()
	e, ok := p.sessions[userID]
	p.mu.RUnlock()
	if !ok {
		return nil
	}
	e.tabsMu.Lock()
	defer e.tabsMu.Unlock()
	out := make([]int64, 0, len(e.tabs))
	for id := range e.tabs {
		out = append(out, id)
	}
	return out
}

// PrewarmFor navigates the user's hot tab to /camping/campsites/{campsiteID}
// and waits for grecaptcha.enterprise.execute to be callable, so a follow-up
// HoldCampsite call on the same (user, site) skips Nav + RecaptchaWait
// (~1.1s on prod). Idempotent: if the tab is already on this site, returns
// immediately. Safe to fire-and-forget — errors are logged and the next
// HoldCampsite will fall back to the full nav path.
//
// Serialized per-user via e.mu, same lock as HoldCampsite, so a prewarm in
// progress will not race a concurrent hold.
func (p *Pool) PrewarmFor(ctx context.Context, userID, campsiteID string) error {
	ctx = WithLogger(ctx, p.cfg.Logger.With(
		"correlation_id", newCorrelationID(),
		"user_id", userID,
		"campsite_id", campsiteID,
		"op", "prewarm",
	))
	e, err := p.ensureAlive(ctx, userID)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disabled {
		return errors.New("session disabled")
	}
	if e.prewarmedSite == campsiteID {
		return nil
	}
	opCtx, cancel := sessionOperationContext(e.session.Ctx(), ctx)
	defer cancel()
	if _, err := PrewarmCampsite(opCtx, campsiteID); err != nil {
		// Don't latch prewarmedSite on failure — next call retries.
		return err
	}
	e.prewarmedSite = campsiteID
	return nil
}

// ensureAlive returns a live entry for userID. If the current entry's
// Chrome has died, it is closed and a fresh session is launched + logged in
// before returning. Holds no locks across the (slow) launch.
func (p *Pool) ensureAlive(ctx context.Context, userID string) (*entry, error) {
	p.mu.RLock()
	e, ok := p.sessions[userID]
	p.mu.RUnlock()
	if ok {
		e.mu.Lock()
		alive := !e.disabled && sessionAlive(e.session)
		e.mu.Unlock()
		if alive {
			return e, nil
		}
		p.cfg.Logger.Warn("pool session not alive; relaunching", "user", userID)
	} else {
		p.cfg.Logger.Warn("pool has no entry for user; launching", "user", userID)
	}
	if err := p.launchUser(ctx, userID); err != nil {
		return nil, err
	}
	p.mu.RLock()
	e = p.sessions[userID]
	p.mu.RUnlock()
	if e == nil {
		return nil, fmt.Errorf("no entry for user %s after launch", userID)
	}
	return e, nil
}

// sessionAlive returns true if the chromedp context for this session is
// still healthy. We treat a cancelled session ctx as definitive evidence
// that Chrome is gone; anything else, we trust until proven otherwise.
func sessionAlive(s *Session) bool {
	if s == nil {
		return false
	}
	return s.Ctx().Err() == nil
}

// RunRefreshLoop nav's each session to the homepage every cfg.RefreshInterval
// to keep cookies + JWT alive. Blocks until ctx is cancelled.
func (p *Pool) RunRefreshLoop(ctx context.Context) {
	t := time.NewTicker(p.cfg.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refreshAll(ctx)
		}
	}
}

func (p *Pool) refreshAll(ctx context.Context) {
	p.mu.RLock()
	ids := make([]string, 0, len(p.sessions))
	for id := range p.sessions {
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	for _, id := range ids {
		e, err := p.ensureAlive(ctx, id)
		if err != nil {
			p.cfg.Logger.Warn("refresh: ensure alive failed", "user", id, "err", err)
			continue
		}
		e.mu.Lock()
		timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 30*time.Second)
		rctx, cancel := sessionOperationContext(e.session.Ctx(), timeoutCtx)
		if err := e.session.Refresh(rctx); err != nil {
			p.cfg.Logger.Warn("session refresh failed", "user", id, "err", err)
		}
		// Refresh navigates to the homepage — any prewarm is stale now.
		e.prewarmedSite = ""
		cancel()
		timeoutCancel()
		e.mu.Unlock()
	}
}

// RunWatchdog periodically pings each session to catch dead Chrome
// processes between bookings, so the next hit doesn't pay the cold-start
// cost. Blocks until ctx is cancelled.
func (p *Pool) RunWatchdog(ctx context.Context) {
	t := time.NewTicker(p.cfg.WatchdogInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.watchdogSweep(ctx)
		}
	}
}

func (p *Pool) watchdogSweep(ctx context.Context) {
	p.mu.RLock()
	type pair struct {
		id string
		e  *entry
	}
	pairs := make([]pair, 0, len(p.sessions))
	for id, e := range p.sessions {
		pairs = append(pairs, pair{id, e})
	}
	p.mu.RUnlock()
	for _, pa := range pairs {
		pa.e.mu.Lock()
		dead := pa.e.disabled || !sessionAlive(pa.e.session)
		pa.e.mu.Unlock()
		if !dead {
			// Also do a quick liveness ping so we catch hung browsers
			// (chromedp ctx not yet cancelled but Chrome unresponsive),
			// and verify the JWT is still in localStorage so we catch
			// silent server-side session expiry before the next booking.
			pa.e.mu.Lock()
			pingCtx, pingCancel := context.WithTimeout(pa.e.session.Ctx(), 3*time.Second)
			var loggedIn bool
			perr := chromedp.Run(pingCtx, chromedp.Evaluate(
				`!!(window.localStorage.getItem('recaccount'))`, &loggedIn,
			))
			pingCancel()
			pa.e.mu.Unlock()
			if perr == nil && loggedIn {
				continue
			}
			reason := "watchdog_jwt_missing"
			if perr != nil {
				reason = "watchdog_ping_failed"
				p.cfg.Logger.Warn("watchdog: session ping failed; will relaunch", "user", pa.id, "err", perr)
			} else {
				p.cfg.Logger.Warn("watchdog: session not logged in; will relaunch", "user", pa.id)
			}
			metrics.BookerRelaunchTotal.WithLabelValues(reason).Inc()
		} else {
			p.cfg.Logger.Warn("watchdog: session dead; will relaunch", "user", pa.id)
			metrics.BookerRelaunchTotal.WithLabelValues("watchdog_dead").Inc()
		}
		if err := p.launchUser(ctx, pa.id); err != nil {
			p.cfg.Logger.Warn("watchdog: relaunch failed", "user", pa.id, "err", err)
		}
	}
}

// sessionOperationContext keeps chromedp's browser metadata from sessionCtx
// while propagating the caller's cancellation and deadline.
func sessionOperationContext(sessionCtx, callerCtx context.Context) (context.Context, context.CancelFunc) {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if deadline, ok := callerCtx.Deadline(); ok {
		ctx, cancel = context.WithDeadline(sessionCtx, deadline)
	} else {
		ctx, cancel = context.WithCancel(sessionCtx)
	}
	stop := context.AfterFunc(callerCtx, cancel)
	if callerCtx.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stop()
		cancel()
	}
}

// HasUser reports whether the pool currently has a warm session for userID.
// Returns true even if the session has died — callers should treat this as
// "the user is linked and we'll try"; the actual aliveness check + relaunch
// happens inside HoldCampsite.
func (p *Pool) HasUser(userID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.sessions[userID]
	return ok
}
