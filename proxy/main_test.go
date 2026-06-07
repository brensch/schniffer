package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"GZIP", true},
		{"deflate, gzip", true},
		{"gzip;q=1.0, identity;q=0.5", true},
		{"deflate", false},
		{"identity", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/fetch", nil)
		if c.header != "" {
			r.Header.Set("Accept-Encoding", c.header)
		}
		if got := acceptsGzip(r); got != c.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

func TestHandleFetch_GzipRoundtrip(t *testing.T) {
	// Spin up a fake upstream the proxy will batch-fetch from.
	payload := `{"campsites":{` +
		strings.Repeat(`"k":"vvvvvvvvvvvvvvvvvvvv",`, 200) + `"end":1}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	secret = "test-secret"

	body := `{"requests":[{"url":"` + upstream.URL + `","method":"GET"}]}`
	r := httptest.NewRequest(http.MethodPost, "/fetch", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-secret")
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()

	handleFetch(w, r)

	res := w.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.StatusCode, w.Body.String())
	}
	if got := res.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding=%q, want gzip", got)
	}
	raw, _ := io.ReadAll(res.Body)
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not valid gzip: %v", err)
	}
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	if !bytes.Contains(out, []byte(`campsites`)) {
		t.Fatalf("decoded body missing payload: %s", out)
	}
	// Compression must actually shrink the payload (it's ~5KB of repeats).
	if len(raw) >= len(out)/2 {
		t.Errorf("expected meaningful compression: raw=%d decoded=%d", len(raw), len(out))
	}
}

func TestHandleFetch_NoGzipWhenNotAccepted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	secret = "test-secret"
	body := `{"requests":[{"url":"` + upstream.URL + `","method":"GET"}]}`
	r := httptest.NewRequest(http.MethodPost, "/fetch", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-secret")
	// No Accept-Encoding header.
	w := httptest.NewRecorder()

	handleFetch(w, r)

	if got := w.Result().Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding should be empty when client did not request gzip, got %q", got)
	}
}
