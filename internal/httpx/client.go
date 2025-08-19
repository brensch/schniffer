package httpx

import (
	"math/rand"
	"net"
	"net/http"
	"time"
)

var defaultClient *http.Client

// Default returns a shared HTTP client with sensible timeouts.
func Default() *http.Client {
	if defaultClient != nil {
		return defaultClient
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	defaultClient = &http.Client{
		Timeout:   20 * time.Second,
		Transport: transport,
	}
	return defaultClient
}

// browserProfile represents a complete browser header set
type browserProfile struct {
	UserAgent       string
	Accept          string
	AcceptLanguage  string
	AcceptEncoding  string
	Connection      string
	UpgradeInsecure string
	SecFetchDest    string
	SecFetchMode    string
	SecFetchSite    string
	SecFetchUser    string
}

// realBrowserProfiles contains a comprehensive list of realistic browser headers
var realBrowserProfiles = []browserProfile{
	// Chrome on Windows
	{
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		Connection:      "keep-alive",
		UpgradeInsecure: "1",
		SecFetchDest:    "document",
		SecFetchMode:    "navigate",
		SecFetchSite:    "none",
		SecFetchUser:    "?1",
	},
	{
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		Connection:      "keep-alive",
		UpgradeInsecure: "1",
		SecFetchDest:    "document",
		SecFetchMode:    "navigate",
		SecFetchSite:    "none",
		SecFetchUser:    "?1",
	},
	{
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		Connection:      "keep-alive",
		UpgradeInsecure: "1",
		SecFetchDest:    "document",
		SecFetchMode:    "navigate",
		SecFetchSite:    "none",
		SecFetchUser:    "?1",
	},
	// Chrome on macOS
	{
		UserAgent:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		Connection:      "keep-alive",
		UpgradeInsecure: "1",
		SecFetchDest:    "document",
		SecFetchMode:    "navigate",
		SecFetchSite:    "none",
		SecFetchUser:    "?1",
	},
	{
		UserAgent:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		Connection:      "keep-alive",
		UpgradeInsecure: "1",
		SecFetchDest:    "document",
		SecFetchMode:    "navigate",
		SecFetchSite:    "none",
		SecFetchUser:    "?1",
	},
	// Chrome on Linux
	{
		UserAgent:       "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		Connection:      "keep-alive",
		UpgradeInsecure: "1",
		SecFetchDest:    "document",
		SecFetchMode:    "navigate",
		SecFetchSite:    "none",
		SecFetchUser:    "?1",
	},
	{
		UserAgent:       "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		Connection:      "keep-alive",
		UpgradeInsecure: "1",
		SecFetchDest:    "document",
		SecFetchMode:    "navigate",
		SecFetchSite:    "none",
		SecFetchUser:    "?1",
	},
	// Firefox on Windows
	{
		UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0",
		Accept:         "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		AcceptLanguage: "en-US,en;q=0.5",
		AcceptEncoding: "gzip, deflate, br",
		Connection:     "keep-alive",
	},
	{
		UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:126.0) Gecko/20100101 Firefox/126.0",
		Accept:         "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		AcceptLanguage: "en-US,en;q=0.5",
		AcceptEncoding: "gzip, deflate, br",
		Connection:     "keep-alive",
	},
	// Firefox on macOS
	{
		UserAgent:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:127.0) Gecko/20100101 Firefox/127.0",
		Accept:         "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		AcceptLanguage: "en-US,en;q=0.5",
		AcceptEncoding: "gzip, deflate, br",
		Connection:     "keep-alive",
	},
	{
		UserAgent:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:126.0) Gecko/20100101 Firefox/126.0",
		Accept:         "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		AcceptLanguage: "en-US,en;q=0.5",
		AcceptEncoding: "gzip, deflate, br",
		Connection:     "keep-alive",
	},
	// Firefox on Linux
	{
		UserAgent:      "Mozilla/5.0 (X11; Linux x86_64; rv:127.0) Gecko/20100101 Firefox/127.0",
		Accept:         "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		AcceptLanguage: "en-US,en;q=0.5",
		AcceptEncoding: "gzip, deflate, br",
		Connection:     "keep-alive",
	},
	{
		UserAgent:      "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:127.0) Gecko/20100101 Firefox/127.0",
		Accept:         "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		AcceptLanguage: "en-US,en;q=0.5",
		AcceptEncoding: "gzip, deflate, br",
		Connection:     "keep-alive",
	},
	// Safari on macOS
	{
		UserAgent:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15",
		Accept:         "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		AcceptLanguage: "en-US,en;q=0.9",
		AcceptEncoding: "gzip, deflate, br",
		Connection:     "keep-alive",
	},
	{
		UserAgent:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15",
		Accept:         "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		AcceptLanguage: "en-US,en;q=0.9",
		AcceptEncoding: "gzip, deflate, br",
		Connection:     "keep-alive",
	},
	// Edge on Windows
	{
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		Connection:      "keep-alive",
		UpgradeInsecure: "1",
		SecFetchDest:    "document",
		SecFetchMode:    "navigate",
		SecFetchSite:    "none",
		SecFetchUser:    "?1",
	},
	{
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36 Edg/125.0.0.0",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		Connection:      "keep-alive",
		UpgradeInsecure: "1",
		SecFetchDest:    "document",
		SecFetchMode:    "navigate",
		SecFetchSite:    "none",
		SecFetchUser:    "?1",
	},
	// Additional Chrome variants with different language preferences
	{
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9,es;q=0.8",
		AcceptEncoding:  "gzip, deflate, br",
		Connection:      "keep-alive",
		UpgradeInsecure: "1",
		SecFetchDest:    "document",
		SecFetchMode:    "navigate",
		SecFetchSite:    "none",
		SecFetchUser:    "?1",
	},
	{
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9,fr;q=0.8",
		AcceptEncoding:  "gzip, deflate, br",
		Connection:      "keep-alive",
		UpgradeInsecure: "1",
		SecFetchDest:    "document",
		SecFetchMode:    "navigate",
		SecFetchSite:    "none",
		SecFetchUser:    "?1",
	},
	// Chrome on Android
	{
		UserAgent:      "Mozilla/5.0 (Linux; Android 10; SM-G973F) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36",
		Accept:         "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage: "en-US,en;q=0.9",
		AcceptEncoding: "gzip, deflate, br",
		Connection:     "keep-alive",
	},
	{
		UserAgent:      "Mozilla/5.0 (Linux; Android 11; Pixel 5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36",
		Accept:         "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage: "en-US,en;q=0.9",
		AcceptEncoding: "gzip, deflate, br",
		Connection:     "keep-alive",
	},
	// Safari on iOS
	{
		UserAgent:      "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
		Accept:         "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		AcceptLanguage: "en-US,en;q=0.9",
		AcceptEncoding: "gzip, deflate, br",
		Connection:     "keep-alive",
	},
	{
		UserAgent:      "Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
		Accept:         "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		AcceptLanguage: "en-US,en;q=0.9",
		AcceptEncoding: "gzip, deflate, br",
		Connection:     "keep-alive",
	},
}

// SpoofChromeHeaders sets a randomly selected realistic browser header set on the request.
func SpoofChromeHeaders(r *http.Request) {
	// Select a random browser profile
	profile := realBrowserProfiles[rand.Intn(len(realBrowserProfiles))]
	
	// Set the headers from the selected profile
	r.Header.Set("User-Agent", profile.UserAgent)
	r.Header.Set("Accept", profile.Accept)
	r.Header.Set("Accept-Language", profile.AcceptLanguage)
	// Don't set Accept-Encoding - let Go's HTTP client handle compression automatically
	r.Header.Set("Connection", profile.Connection)
	
	// Set optional headers if they exist in the profile
	if profile.UpgradeInsecure != "" {
		r.Header.Set("Upgrade-Insecure-Requests", profile.UpgradeInsecure)
	}
	if profile.SecFetchDest != "" {
		r.Header.Set("Sec-Fetch-Dest", profile.SecFetchDest)
	}
	if profile.SecFetchMode != "" {
		r.Header.Set("Sec-Fetch-Mode", profile.SecFetchMode)
	}
	if profile.SecFetchSite != "" {
		r.Header.Set("Sec-Fetch-Site", profile.SecFetchSite)
	}
	if profile.SecFetchUser != "" {
		r.Header.Set("Sec-Fetch-User", profile.SecFetchUser)
	}
}