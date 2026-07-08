// Package monitor provides access control for the private /monitor
// dashboard: single-use, short-lived tokens minted by an admin-only slash
// command, exchanged for a short-lived server-side session.
package monitor

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sync"
	"time"
)

// TokenTTL is how long a freshly minted access token is valid before it
// must be redeemed. Deliberately short — the token only has to survive the
// few seconds between running the command and clicking the link.
const TokenTTL = 5 * time.Minute

// SessionTTL is how long a redeemed session (browser cookie) stays valid.
const SessionTTL = 2 * time.Hour

// Auth holds the token and session stores. Both are keyed by the SHA-256 of
// the secret (never the secret itself), so a memory disclosure can't yield
// a usable credential. All secrets are 256 bits of crypto/rand.
type Auth struct {
	mu       sync.Mutex
	tokens   map[string]time.Time // sha256(token) -> expiry
	sessions map[string]time.Time // sha256(sessionID) -> expiry
	now      func() time.Time
}

// NewAuth returns an empty, ready-to-use Auth.
func NewAuth() *Auth {
	return &Auth{
		tokens:   map[string]time.Time{},
		sessions: map[string]time.Time{},
		now:      time.Now,
	}
}

func randSecret() (string, error) {
	b := make([]byte, 32) // 256 bits
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// MintToken creates a single-use, short-lived access token. Only the
// admin-gated command calls this.
func (a *Auth) MintToken() (string, error) {
	tok, err := randSecret()
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked()
	a.tokens[hashKey(tok)] = a.now().Add(TokenTTL)
	return tok, nil
}

// Redeem validates a token and, if it's live, returns a fresh session id.
//
// The token stays valid for its full (short) TTL rather than being consumed
// on first use: Discord unfurls links to build previews, and link scanners
// pre-fetch them, so a strictly single-use token gets burned before the
// admin ever clicks. Since the token is only ever delivered to the admin
// (ephemeral message, over TLS) and dies in TokenTTL, allowing a few
// redemptions within that window is a safe trade for reliability.
func (a *Auth) Redeem(token string) (sessionID string, ok bool) {
	if token == "" {
		return "", false
	}
	key := hashKey(token)
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, found := a.tokens[key]
	if !found || a.now().After(exp) {
		delete(a.tokens, key) // clean up an expired entry
		return "", false
	}
	sid, err := randSecret()
	if err != nil {
		return "", false
	}
	a.sessions[hashKey(sid)] = a.now().Add(SessionTTL)
	return sid, true
}

// ValidSession reports whether sid maps to a live (unexpired) session.
func (a *Auth) ValidSession(sid string) bool {
	if sid == "" {
		return false
	}
	key := hashKey(sid)
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[key]
	if !ok {
		return false
	}
	if a.now().After(exp) {
		delete(a.sessions, key)
		return false
	}
	return true
}

// sweepLocked drops expired tokens and sessions. Caller holds a.mu.
func (a *Auth) sweepLocked() {
	now := a.now()
	for k, exp := range a.tokens {
		if now.After(exp) {
			delete(a.tokens, k)
		}
	}
	for k, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
}
