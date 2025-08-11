package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// helper to build a provider pointed at a test server via transport rewrite
func newRecreationGovForTest(t *testing.T, srv *httptest.Server) *RecreationGov {
	t.Helper()
	targetURL, _ := url.Parse(srv.URL)
	p := NewRecreationGov()
	p.client.Transport = &rewriteTransport{target: targetURL}
	return p
}

func TestRecreationGov_FetchAvailability_URLQueryEncoded(t *testing.T) {
	// Arrange a fake API that captures the raw query string.
	var captured []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/camps/availability/campground/") {
			http.NotFound(w, r)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/month") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		captured = append(captured, r.URL.RawQuery)
		// Minimal valid body
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"campsites": map[string]any{}})
	}))
	defer srv.Close()

	p := newRecreationGovForTest(t, srv)

	// Use a date with plus signs and colons to ensure encoding is necessary.
	start := time.Date(2024, 12, 15, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	_, err := p.FetchAvailability(context.Background(), "12345", start, end)
	if err != nil {
		t.Fatalf("FetchAvailability returned error: %v", err)
	}

	if len(captured) == 0 {
		t.Fatalf("server did not receive any requests")
	}
	// Verify each query contains properly encoded start_date; plus and colon should be percent-encoded.
	for _, raw := range captured {
		q, _ := url.ParseQuery(raw)
		got := q.Get("start_date")
		if got == "" {
			t.Fatalf("start_date missing from query: %q", raw)
		}
		if strings.ContainsAny(raw, "+ :") {
			t.Fatalf("query appears not encoded: %q", raw)
		}
		// Check format ends with Z and has milliseconds
		if !strings.HasSuffix(got, "Z") || len(got) < len("2006-01-02T15:04:05.000Z") {
			t.Fatalf("unexpected start_date format: %q", got)
		}
	}
}
