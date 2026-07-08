package monitor

import (
	"testing"
	"time"
)

func TestTokenRedeemSingleUse(t *testing.T) {
	a := NewAuth()
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
	// Single use: a second redeem of the same token must fail.
	if _, ok := a.Redeem(tok); ok {
		t.Fatal("token must not be redeemable twice")
	}
}

func TestRedeemRejectsUnknownAndEmpty(t *testing.T) {
	a := NewAuth()
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
	a := NewAuth()
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
	a := NewAuth()
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

func TestTokensAreDistinctAndHighEntropy(t *testing.T) {
	a := NewAuth()
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
