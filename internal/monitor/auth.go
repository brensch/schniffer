// Package monitor provides access control for the private /monitor
// dashboard: single-use, short-lived tokens minted by an admin-only slash
// command, exchanged for a long-lived, signed browser session.
package monitor

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TokenTTL is how long a freshly minted access token is valid before it
// must be redeemed. Deliberately short — the token only has to survive the
// few seconds between running the command and clicking the link.
const TokenTTL = 5 * time.Minute

// SessionTTL is how long a redeemed browser session stays valid. Long by
// design: redeeming a link should keep that browser logged in effectively
// indefinitely. Rotating the signing key revokes all sessions at once.
const SessionTTL = 365 * 24 * time.Hour

// Auth mints short-lived access tokens (held in memory) and, on redemption,
// issues stateless HMAC-signed session cookies. Because sessions are signed
// rather than stored, they survive process restarts as long as the signing
// key is stable.
type Auth struct {
	mu      sync.Mutex
	tokens  map[string]time.Time // sha256(token) -> expiry
	signKey []byte
	now     func() time.Time
}

// NewAuth returns an Auth that signs sessions with signingKey. If the key
// is empty a random ephemeral one is generated — sessions then work but do
// not survive a restart, so pass a persisted key in production (see
// LoadOrCreateSigningKey).
func NewAuth(signingKey []byte) *Auth {
	if len(signingKey) == 0 {
		signingKey = make([]byte, 32)
		_, _ = rand.Read(signingKey)
	}
	return &Auth{
		tokens:  map[string]time.Time{},
		signKey: signingKey,
		now:     time.Now,
	}
}

// LoadOrCreateSigningKey reads a 32-byte signing key from path, creating it
// (0600) with fresh randomness if absent. Keep path on a persisted volume
// so sessions survive restarts.
func LoadOrCreateSigningKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
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

// MintToken creates a short-lived access token. Only the admin-gated
// command calls this.
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

// Redeem validates a token and, if it's live, returns a signed session
// cookie value. The token stays valid for its full (short) TTL rather than
// being consumed on first use: Discord unfurls links and scanners pre-fetch
// them, so a strictly single-use token would be burned before the admin
// clicks. It is only ever delivered to the admin and dies in TokenTTL.
func (a *Auth) Redeem(token string) (cookieValue string, ok bool) {
	if token == "" {
		return "", false
	}
	key := hashKey(token)
	a.mu.Lock()
	exp, found := a.tokens[key]
	if !found || a.now().After(exp) {
		delete(a.tokens, key)
		a.mu.Unlock()
		return "", false
	}
	a.mu.Unlock()
	return a.newSession(), true
}

// newSession builds a "<base64(expiryUnix)>.<base64(hmac)>" cookie value.
func (a *Auth) newSession() string {
	payload := strconv.FormatInt(a.now().Add(SessionTTL).Unix(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(a.sign(payload))
}

func (a *Auth) sign(payload string) []byte {
	m := hmac.New(sha256.New, a.signKey)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// ValidSession verifies a session cookie's signature (constant time) and
// expiry. Stateless: no lookup, so it holds across restarts.
func (a *Auth) ValidSession(cookieValue string) bool {
	parts := strings.SplitN(cookieValue, ".", 2)
	if len(parts) != 2 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	if !hmac.Equal(sig, a.sign(string(payload))) {
		return false
	}
	exp, err := strconv.ParseInt(string(payload), 10, 64)
	if err != nil {
		return false
	}
	return a.now().Before(time.Unix(exp, 0))
}

// sweepLocked drops expired tokens. Caller holds a.mu.
func (a *Auth) sweepLocked() {
	now := a.now()
	for k, exp := range a.tokens {
		if now.After(exp) {
			delete(a.tokens, k)
		}
	}
}
