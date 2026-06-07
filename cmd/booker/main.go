// booker is a one-shot CLI for testing internal/booker against a single
// campsite. The production path runs through the bot's browser pool; this
// binary is for developer-driven smoke tests inside the docker container.
//
// Usage:
//   REC_GOV_EMAIL=... REC_GOV_PASSWORD=... go run ./cmd/booker \
//     -campsite 10085607 -campground 10085599 -from 2026-06-25 -nights 1
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	api2captcha "github.com/2captcha/2captcha-go"
	"github.com/brensch/schniffer/internal/booker"
)

func main() {
	var (
		campsite   = flag.String("campsite", "10085607", "campsite ID")
		campground = flag.String("campground", "10085599", "parent campground/facility ID")
		fromStr    = flag.String("from", "", "arrival date YYYY-MM-DD (required unless -login-only)")
		nights     = flag.Int("nights", 1, "number of nights")
		profileDir = flag.String("profile", "", "Chrome user-data-dir (default: $CHROME_PROFILE or ~/.cache/recgov-booker)")
		chromePath = flag.String("chrome", "", "path to chrome/chromium binary (default autodetect)")
		loginOnly  = flag.Bool("login-only", false, "stop after ensuring login")
		timeout    = flag.Duration("timeout", 5*time.Minute, "overall timeout")
		debugDir   = flag.String("debug-dir", "/work/.cache/debug", "directory for response dumps")
		use2Cap    = flag.Bool("use-2captcha", false, "force the 2captcha API instead of in-page grecaptcha (needs 2CAPTCHA_API_KEY)")
	)
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if *fromStr == "" && !*loginOnly {
		fatal("-from YYYY-MM-DD is required")
	}
	var arrival time.Time
	if *fromStr != "" {
		t, err := time.Parse("2006-01-02", *fromStr)
		if err != nil {
			fatal("bad -from: %v", err)
		}
		arrival = t
	}
	email := os.Getenv("REC_GOV_EMAIL")
	password := os.Getenv("REC_GOV_PASSWORD")
	if email == "" || password == "" {
		fatal("REC_GOV_EMAIL and REC_GOV_PASSWORD must be set")
	}
	if *profileDir == "" {
		if env := os.Getenv("CHROME_PROFILE"); env != "" {
			*profileDir = env
		} else {
			home, _ := os.UserHomeDir()
			*profileDir = filepath.Join(home, ".cache", "recgov-booker")
		}
	}
	_ = os.MkdirAll(*debugDir, 0o755)

	sess, err := booker.Open(booker.Config{ProfileDir: *profileDir, ChromePath: *chromePath})
	if err != nil {
		fatal("open chrome: %v", err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(sess.Ctx(), *timeout)
	defer cancel()

	slog.Info("logging in")
	if err := sess.Login(ctx, email, password); err != nil {
		fatal("login: %v", err)
	}
	if *loginOnly {
		slog.Info("login-only mode; exiting")
		return
	}
	depart := arrival.AddDate(0, 0, *nights)
	slog.Info("booking", "campsite", *campsite, "campground", *campground,
		"arrival", arrival.Format("2006-01-02"), "depart", depart.Format("2006-01-02"),
		"use_2captcha", *use2Cap)

	var (
		res *booker.HoldResult
	)
	if *use2Cap {
		// xvfb-run strips env vars with POSIX-invalid names (digits-first), so
		// prefer TWOCAPTCHA_API_KEY but fall back to 2CAPTCHA_API_KEY.
		apiKey := os.Getenv("TWOCAPTCHA_API_KEY")
		if apiKey == "" {
			apiKey = os.Getenv("2CAPTCHA_API_KEY")
		}
		if apiKey == "" {
			fatal("TWOCAPTCHA_API_KEY (or 2CAPTCHA_API_KEY) not set")
		}
		pageURL := fmt.Sprintf("%s/camping/campsites/%s", booker.SiteOrigin, *campsite)
		start := time.Now()
		token, terr := solve2Captcha(apiKey, pageURL)
		if terr != nil {
			fatal("2captcha solve: %v", terr)
		}
		slog.Info("2captcha solved", "token_len", len(token), "took", time.Since(start))
		res, err = sess.HoldCampsiteWithToken(ctx, *campsite, *campground, arrival, depart, token)
	} else {
		res, err = sess.HoldCampsite(ctx, *campsite, *campground, arrival, depart)
	}
	if err != nil {
		if res != nil {
			pretty, _ := json.MarshalIndent(res.Raw, "", "  ")
			_ = os.WriteFile(filepath.Join(*debugDir, "last-response.json"), pretty, 0o644)
		}
		fatal("book: %v", err)
	}
	if err != nil {
		if res != nil {
			pretty, _ := json.MarshalIndent(res.Raw, "", "  ")
			_ = os.WriteFile(filepath.Join(*debugDir, "last-response.json"), pretty, 0o644)
		}
		fatal("book: %v", err)
	}
	pretty, _ := json.MarshalIndent(res.Raw, "", "  ")
	_ = os.WriteFile(filepath.Join(*debugDir, "last-response.json"), pretty, 0o644)
	if res.OrderID != "" {
		fmt.Printf("\nReservation held: %s\n", booker.OrderURL(res.OrderID))
	} else {
		fmt.Println("\nCart POST succeeded but no order id found; see debug-dir/last-response.json")
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+strings.TrimRight(format, "\n")+"\n", args...)
	os.Exit(1)
}

// solve2Captcha mints a reCAPTCHA Enterprise v3 token for the rec.gov
// booking action. 2captcha typically takes 15-60s. Returns the token string
// ready to splice into the gate_a.value field.
func solve2Captcha(apiKey, pageURL string) (string, error) {
	client := api2captcha.NewClient(apiKey)
	client.DefaultTimeout = 180
	client.PollingInterval = 5
	cap := api2captcha.ReCaptcha{
		SiteKey:    booker.RecaptchaSiteKey,
		Url:        pageURL,
		Version:    "v3",
		Action:     booker.RecaptchaAction,
		Enterprise: true,
		Score:      0.3,
	}
	token, _, err := client.Solve(cap.ToRequest())
	return token, err
}
