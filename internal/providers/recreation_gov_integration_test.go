package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// rewriteTransport rewrites outgoing requests to hit a test server instead of the real host.
type rewriteTransport struct{ target *url.URL }

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone to avoid mutating caller's request
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = rt.target.Scheme
	r2.URL.Host = rt.target.Host
	// Ensure Host header matches (some servers check it)
	r2.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

func TestRecreationGov_FetchAllCampgrounds_Paginates(t *testing.T) {
	// Arrange a fake recreation.gov search API with two pages: 100 results then 40.
	var calls []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		start, _ := strconv.Atoi(q.Get("start"))
		calls = append(calls, start)

		// First page returns exactly 'size' results, second page returns 40, then no more calls expected.
		var count int
		if start == 0 {
			count = 100
		} else if start == 100 {
			count = 40
		} else {
			t.Fatalf("unexpected start param: %d", start)
		}

		type result struct {
			Name       string `json:"name"`
			EntityID   string `json:"entity_id"`
			Reservable bool   `json:"reservable"`
		}
		out := struct {
			Results []result `json:"results"`
			Size    int      `json:"size"`
		}{Results: make([]result, 0, count), Size: count}
		for i := 0; i < count; i++ {
			id := start + i + 1
			out.Results = append(out.Results, result{EntityID: fmt.Sprintf("cg-%d", id), Name: fmt.Sprintf("Campground %d", id), Reservable: true})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	targetURL, _ := url.Parse(srv.URL)

	// Wire provider to use our rewrite transport.
	p := NewRecreationGov()
	p.client.Transport = &rewriteTransport{target: targetURL}

	// Act
	got, err := p.FetchAllCampgrounds(context.Background())
	if err != nil {
		t.Fatalf("FetchAllCampgrounds error: %v", err)
	}

	// Assert
	if len(got) != 140 {
		t.Fatalf("expected 140 campgrounds, got %d", len(got))
	}
	if got[0].ID != "cg-1" || got[0].Name != "Campground 1" {
		t.Fatalf("unexpected first item: %+v", got[0])
	}
	last := got[len(got)-1]
	if last.ID != "cg-140" || last.Name != "Campground 140" {
		t.Fatalf("unexpected last item: %+v", last)
	}
	if len(calls) != 2 || calls[0] != 0 || calls[1] != 100 {
		t.Fatalf("unexpected pagination calls: %v", calls)
	}
}
