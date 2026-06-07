package booker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

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
	mu       sync.Mutex // serializes operations on this session (one Chrome window, one nav at a time)
	session  *Session
	disabled bool
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
	if existed && old != nil && old.session != nil {
		old.session.Close()
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
	if ok && e.session != nil {
		e.session.Close()
	}
}

// Close tears down every session.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.sessions {
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
	e, err := p.ensureAlive(ctx, userID)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disabled {
		return nil, errors.New("session disabled")
	}
	opCtx, cancel := sessionOperationContext(e.session.Ctx(), ctx)
	defer cancel()
	return e.session.HoldCampsite(opCtx, campsiteID, campgroundID, checkIn, checkOut)
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
			// (chromedp ctx not yet cancelled but Chrome unresponsive).
			pa.e.mu.Lock()
			pingCtx, pingCancel := context.WithTimeout(pa.e.session.Ctx(), 3*time.Second)
			var v bool
			perr := chromedp.Run(pingCtx, chromedp.Evaluate(`true`, &v))
			pingCancel()
			pa.e.mu.Unlock()
			if perr == nil {
				continue
			}
			p.cfg.Logger.Warn("watchdog: session ping failed; will relaunch", "user", pa.id, "err", perr)
		} else {
			p.cfg.Logger.Warn("watchdog: session dead; will relaunch", "user", pa.id)
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
