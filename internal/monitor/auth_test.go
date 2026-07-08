package monitor

import (
	"testing"
	"time"
)

func TestTokenRedeemReusableWithinTTL(t *testing.T) {
	a := NewAuth([]byte("test-signing-key-0123456789abcdef"))
	tok, err := a.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	sid, ok := a.Redeem(tok)
	if !ok || sid == "" {
		t.Fatal("first redeem should succeed with a session id")
	}
	if !a.ValidSession(sid) {
		t.Fatal("session from redeem should be valid")
	}
	// Reusable within TTL: a second redeem (e.g. the admin clicking after a
	// link-preview fetch) also succeeds with a valid session. (The signed
	// cookie is deterministic, so a same-second redeem may be byte-identical
	// — that's harmless.)
	sid2, ok := a.Redeem(tok)
	if !ok || sid2 == "" {
		t.Fatal("token should be redeemable again within its TTL")
	}
	if !a.ValidSession(sid2) {
		t.Fatal("second session should be valid")
	}
}

func TestRedeemRejectsUnknownAndEmpty(t *testing.T) {
	a := NewAuth([]byte("test-signing-key-0123456789abcdef"))
	if _, ok := a.Redeem(""); ok {
		t.Fatal("empty token must be rejected")
	}
	if _, ok := a.Redeem("not-a-real-token"); ok {
		t.Fatal("unknown token must be rejected")
	}
	if a.ValidSession("nope") {
		t.Fatal("unknown session must be rejected")
	}
}

func TestTokenExpiry(t *testing.T) {
	a := NewAuth([]byte("test-signing-key-0123456789abcdef"))
	base := time.Now()
	a.now = func() time.Time { return base }
	tok, _ := a.MintToken()
	// Jump past the token TTL.
	a.now = func() time.Time { return base.Add(TokenTTL + time.Second) }
	if _, ok := a.Redeem(tok); ok {
		t.Fatal("expired token must not redeem")
	}
}

func TestSessionExpiry(t *testing.T) {
	a := NewAuth([]byte("test-signing-key-0123456789abcdef"))
	base := time.Now()
	a.now = func() time.Time { return base }
	tok, _ := a.MintToken()
	sid, ok := a.Redeem(tok)
	if !ok {
		t.Fatal("redeem should succeed")
	}
	if !a.ValidSession(sid) {
		t.Fatal("session should be valid immediately")
	}
	a.now = func() time.Time { return base.Add(SessionTTL + time.Second) }
	if a.ValidSession(sid) {
		t.Fatal("expired session must be invalid")
	}
}

func TestSessionSurvivesRestartWithSameKey(t *testing.T) {
	key := []byte("stable-signing-key-0123456789abcdef")
	a1 := NewAuth(key)
	tok, _ := a1.MintToken()
	sid, ok := a1.Redeem(tok)
	if !ok {
		t.Fatal("redeem should succeed")
	}
	// Simulate a process restart: a brand-new Auth with the same key must
	// still accept the previously issued session cookie.
	a2 := NewAuth(key)
	if !a2.ValidSession(sid) {
		t.Fatal("session should stay valid across restart with the same key")
	}
	// A different key must reject it (rotating the key revokes sessions).
	a3 := NewAuth([]byte("a-totally-different-signing-key-xx"))
	if a3.ValidSession(sid) {
		t.Fatal("session must be rejected under a different signing key")
	}
}

func TestTamperedSessionRejected(t *testing.T) {
	a := NewAuth([]byte("test-signing-key-0123456789abcdef"))
	tok, _ := a.MintToken()
	sid, _ := a.Redeem(tok)
	if a.ValidSession(sid + "x") {
		t.Fatal("tampered signature must be rejected")
	}
	if a.ValidSession("garbage.notbase64") {
		t.Fatal("malformed cookie must be rejected")
	}
}

func TestTokensAreDistinctAndHighEntropy(t *testing.T) {
	a := NewAuth([]byte("test-signing-key-0123456789abcdef"))
	seen := map[string]bool{}
	for range 100 {
		tok, err := a.MintToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) < 40 {
			t.Fatalf("token too short (%d chars) — insufficient entropy", len(tok))
		}
		if seen[tok] {
			t.Fatal("token collision — not random")
		}
		seen[tok] = true
	}
}
