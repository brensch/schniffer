package booker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"
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
	Logger           *slog.Logger
}

// Pool is the always-warm browser pool. One Chrome instance per linked user,
// booted at startup, never evicted. A background goroutine periodically
// refreshes each session to keep the JWT alive.
type Pool struct {
	cfg PoolConfig

	mu       sync.RWMutex
	sessions map[string]*entry // keyed by userID
}

type entry struct {
	session  *Session
	mu       sync.Mutex // serializes operations on this session
	disabled bool
}

func NewPool(cfg PoolConfig) *Pool {
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 25 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Pool{cfg: cfg, sessions: map[string]*entry{}}
}

// StartUser launches Chrome for userID and logs in. Safe to call multiple
// times — subsequent calls no-op if the session already exists.
func (p *Pool) StartUser(ctx context.Context, userID string) error {
	p.mu.RLock()
	_, ok := p.sessions[userID]
	p.mu.RUnlock()
	if ok {
		return nil
	}
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
	p.mu.Lock()
	p.sessions[userID] = &entry{session: sess}
	p.mu.Unlock()
	p.cfg.Logger.Info("browser warm", "user", userID)
	return nil
}

// StartAll boots all provided users in parallel. Errors per-user are logged
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

// HoldCampsite performs a booking for userID. Serialized per-user; if
// concurrent calls arrive for the same user they queue behind one another
// (one Chrome window, one nav at a time).
func (p *Pool) HoldCampsite(ctx context.Context, userID, campsiteID, campgroundID string, checkIn, checkOut time.Time) (*HoldResult, error) {
	p.mu.RLock()
	e, ok := p.sessions[userID]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no warm session for user %s", userID)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disabled {
		return nil, errors.New("session disabled")
	}
	return e.session.HoldCampsite(ctx, campsiteID, campgroundID, checkIn, checkOut)
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
		p.mu.RLock()
		e := p.sessions[id]
		p.mu.RUnlock()
		if e == nil {
			continue
		}
		e.mu.Lock()
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := e.session.Refresh(rctx); err != nil {
			p.cfg.Logger.Warn("session refresh failed", "user", id, "err", err)
		}
		cancel()
		e.mu.Unlock()
	}
}

// HasUser reports whether the pool currently has a warm session for userID.
func (p *Pool) HasUser(userID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.sessions[userID]
	return ok
}
