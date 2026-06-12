// token-probe inspects the rec.gov access_token to validate three
// assumptions the auth-token-expiry fix depends on:
//
//  1. The access_token in localStorage.recaccount is a JWT (3 dot-
//     separated base64url parts).
//  2. The JWT payload has an `exp` claim we can read.
//  3. After clearing recaccount + re-running Session.Login, a new
//     access_token gets written with a different value (proving the
//     re-login actually round-trips and doesn't no-op).
//
// Bonus check: opens a second tab on the same session and confirms it
// sees the same recaccount via localStorage — that's the "warm tabs
// share the token" assumption the relogin-in-place fix relies on.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/booker"
	"github.com/chromedp/chromedp"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	home, _ := os.UserHomeDir()
	sess, err := booker.Open(booker.Config{ProfileDir: filepath.Join(home, ".cache", "recgov-booker")})
	if err != nil {
		die("open: %v", err)
	}
	defer sess.Close()
	ctx, cancel := context.WithTimeout(sess.Ctx(), 120*time.Second)
	defer cancel()

	email := os.Getenv("REC_GOV_EMAIL")
	password := os.Getenv("REC_GOV_PASSWORD")
	if email == "" || password == "" {
		die("REC_GOV_EMAIL and REC_GOV_PASSWORD must be set")
	}
	if err := sess.Login(ctx, email, password); err != nil {
		die("login: %v", err)
	}
	fmt.Println("=== 1. Login complete, inspecting recaccount.access_token ===")
	tok1, exp1 := readToken(ctx, "main session")
	if tok1 == "" {
		die("no token after login — Session.Login claimed success but localStorage is empty")
	}

	fmt.Println("\n=== 1b. Full recaccount keys (looking for refresh_token / refresh endpoint) ===")
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`window.localStorage.getItem('recaccount')`, &raw,
	)); err != nil {
		die("dump recaccount: %v", err)
	}
	var dec map[string]any
	if err := json.Unmarshal([]byte(raw), &dec); err != nil {
		die("decode recaccount: %v", err)
	}
	keys := []string{}
	for k, v := range dec {
		typ := fmt.Sprintf("%T", v)
		preview := fmt.Sprintf("%v", v)
		if len(preview) > 60 {
			preview = preview[:60] + "..."
		}
		keys = append(keys, fmt.Sprintf("  %s (%s) = %s", k, typ, preview))
	}
	for _, k := range keys {
		fmt.Println(k)
	}
	if _, ok := dec["refresh_token"]; ok {
		fmt.Println("  ✓ refresh_token present — silent refresh is plausible")
	} else {
		fmt.Println("  ✗ no refresh_token field — silent refresh would need a different mechanism")
	}

	fmt.Println("\n=== 1c. Listing all localStorage + sessionStorage keys for context ===")
	var lsKeys []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`Object.keys(window.localStorage)`, &lsKeys,
	)); err != nil {
		die("ls keys: %v", err)
	}
	fmt.Printf("  localStorage keys (%d): %v\n", len(lsKeys), lsKeys)
	var ssKeys []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`Object.keys(window.sessionStorage)`, &ssKeys,
	)); err != nil {
		die("ss keys: %v", err)
	}
	fmt.Printf("  sessionStorage keys (%d): %v\n", len(ssKeys), ssKeys)
	var cookies string
	_ = chromedp.Run(ctx, chromedp.Evaluate(
		`document.cookie`, &cookies,
	))
	fmt.Printf("  cookies: %s\n", cookies)

	fmt.Println("\n=== 2. Decoded JWT payload ===")
	dumpJWT(tok1)

	fmt.Println("\n=== 3. Spawning a sibling tab and reading recaccount from it ===")
	t, err := sess.NewTab()
	if err != nil {
		die("new tab: %v", err)
	}
	defer t.Close()
	if err := chromedp.Run(t.Ctx(),
		chromedp.Navigate("https://www.recreation.gov/"),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		die("warm tab nav: %v", err)
	}
	tok2, exp2 := readTokenOnCtx(t.Ctx(), "sibling tab")
	if tok2 == "" {
		fmt.Println("   ✗ sibling tab sees no recaccount — localStorage is NOT shared across tabs.")
		fmt.Println("     This would break the 'warm tabs survive relogin' assumption.")
	} else if tok1 != tok2 {
		fmt.Printf("   ✗ sibling tab sees a DIFFERENT token (%d chars vs %d). Sharing is partial.\n", len(tok2), len(tok1))
	} else {
		fmt.Printf("   ✓ sibling tab sees the same token (exp=%d). localStorage is shared.\n", exp2)
	}

	fmt.Println("\n=== 4. reloginInPlace: clear recaccount, re-run Login, verify new token ===")
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`window.localStorage.removeItem('recaccount')`, nil,
	)); err != nil {
		die("clear: %v", err)
	}
	fmt.Println("   cleared. checking sibling tab's view BEFORE main re-logs in...")
	tokAfterClear, _ := readTokenOnCtx(t.Ctx(), "sibling tab post-clear")
	if tokAfterClear != "" {
		fmt.Println("   ✗ sibling tab still sees a token after clearing on main. localStorage isn't shared synchronously.")
	} else {
		fmt.Println("   ✓ sibling tab also sees the recaccount cleared.")
	}
	if err := sess.Login(ctx, email, password); err != nil {
		die("re-login: %v", err)
	}
	tok3, exp3 := readToken(ctx, "main session post-relogin")
	if tok3 == "" {
		die("re-login claimed success but no recaccount written")
	}
	if tok3 == tok1 {
		fmt.Println("   ✗ access_token is IDENTICAL after re-login. The form submit probably skipped (cached) or rec.gov returned same token.")
	} else {
		fmt.Printf("   ✓ new access_token differs from the old one. exp1=%d → exp3=%d (delta %ds)\n", exp1, exp3, exp3-exp1)
	}
	tok4, _ := readTokenOnCtx(t.Ctx(), "sibling tab post-relogin")
	switch {
	case tok4 == "":
		fmt.Println("   ✗ sibling tab sees no token after main re-login. Warm tabs would NOT survive.")
	case tok4 != tok3:
		fmt.Println("   ✗ sibling tab still sees old token after main re-login. Sharing is one-way or delayed.")
	default:
		fmt.Println("   ✓ sibling tab sees the new token. relogin-in-place works for warm tabs.")
	}
}

func readToken(ctx context.Context, label string) (string, int64) {
	return readTokenOnCtx(ctx, label)
}

func readTokenOnCtx(ctx context.Context, label string) (string, int64) {
	var out struct {
		Token string `json:"token"`
		Exp   int64  `json:"exp"`
	}
	js := `(() => {
		const raw = window.localStorage.getItem('recaccount');
		if (!raw) return {token: '', exp: 0};
		try {
			const rec = JSON.parse(raw);
			return {token: rec.access_token || '', exp: 0};
		} catch (_) { return {token: '', exp: 0}; }
	})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &out)); err != nil {
		fmt.Printf("   [%s] eval failed: %v\n", label, err)
		return "", 0
	}
	if out.Token == "" {
		fmt.Printf("   [%s] no token in localStorage\n", label)
		return "", 0
	}
	if exp, err := decodeJWTExp(out.Token); err == nil {
		out.Exp = exp
	}
	now := time.Now().Unix()
	rem := out.Exp - now
	fmt.Printf("   [%s] token_len=%d exp=%d (remaining %ds = %.1f min)\n",
		label, len(out.Token), out.Exp, rem, float64(rem)/60.0)
	return out.Token, out.Exp
}

func dumpJWT(token string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		fmt.Printf("   ✗ access_token has %d dot-separated parts, not 3. NOT a JWT.\n", len(parts))
		fmt.Println("     The watchdog's exp-decode would return -2 for every session.")
		return
	}
	payload, err := decodeJWTPayload(parts[1])
	if err != nil {
		fmt.Printf("   ✗ JWT payload decode failed: %v\n", err)
		return
	}
	pretty, _ := json.MarshalIndent(payload, "   ", "  ")
	fmt.Println("   ✓ access_token is a 3-part JWT. Payload:")
	fmt.Println("   ", string(pretty))
	exp, ok := payload["exp"].(float64)
	if !ok {
		fmt.Println("   ✗ payload has no exp claim. Proactive refresh would never fire.")
		return
	}
	fmt.Printf("   ✓ exp claim found: %d (Unix). Remaining: %s\n",
		int64(exp), time.Until(time.Unix(int64(exp), 0)).Round(time.Second))
}

func decodeJWTExp(token string) (int64, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, fmt.Errorf("not a JWT (%d parts)", len(parts))
	}
	payload, err := decodeJWTPayload(parts[1])
	if err != nil {
		return 0, err
	}
	exp, _ := payload["exp"].(float64)
	return int64(exp), nil
}

func decodeJWTPayload(seg string) (map[string]any, error) {
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	b, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...)
	os.Exit(1)
}
