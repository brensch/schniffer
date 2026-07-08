package proxypool

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brensch/schniffer/internal/metrics"
)

// UpstreamElapsedHeader carries the proxy worker's reported upstream
// duration (ms) on each demultiplexed response. Providers read this to
// observe ProviderUpstreamDuration. Set internally; never trust an
// incoming request to set it.
const UpstreamElapsedHeader = "X-Proxy-Upstream-Ms"

// ProxyRegionHeader echoes the region that served the request, for the
// same reason as above.
const ProxyRegionHeader = "X-Proxy-Region"

//go:embed endpoints.json
var endpointsRaw []byte

type Endpoint struct {
	URL      string `json:"url"`
	Provider string `json:"provider"`
	Region   string `json:"region"`
}

type endpointsFile struct {
	Endpoints []Endpoint `json:"endpoints"`
}

// Pool batches concurrent outbound HTTP requests into single calls to a
// proxy endpoint, rotating across endpoints round-robin. Implements
// http.RoundTripper so it can be plugged into any net/http client.
type Pool struct {
	endpoints []Endpoint
	secret    string
	client    *http.Client

	flushAfter time.Duration
	maxBatch   int
	cooldown   time.Duration
	rlCooldown time.Duration

	rrIdx   atomic.Uint64
	mu      sync.Mutex
	pending []*pendingReq
	timer   *time.Timer
	// endpointBad cools an endpoint out of rotation for all hosts after a
	// proxy-level failure (transport error / non-200 from the proxy itself).
	endpointBad map[string]time.Time
	// endpointThrottled cools an endpoint out of rotation for ONE upstream
	// host after that host returned 429/403 through it — the egress IP is
	// being rate-limited by that host specifically. Keyed by badKey(url, host).
	endpointThrottled map[string]time.Time

	statsMu    sync.Mutex
	stats      map[string]*EndpointStat
	statsSince time.Time
}

// EndpointStat is a rolling per-endpoint tally of upstream outcomes,
// drained by the daily report. Requests counts demuxed upstream
// responses (not proxy-transport errors).
type EndpointStat struct {
	URL         string
	Region      string
	Requests    int64
	RateLimited int64 // upstream 429
	Forbidden   int64 // upstream 403 (WAF block)
	Cooldowns   int64 // times this endpoint was throttled out of rotation
}

// badKey namespaces a per-host throttle entry. NUL can't appear in a URL
// or host, so it's a safe separator.
func badKey(url, host string) string { return url + "\x00" + host }

type pendingReq struct {
	req  *http.Request
	body string
	done chan pendingResp
}

type pendingResp struct {
	resp *http.Response
	err  error
}

// wire types matching proxy/main.go
type wireReq struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type wireResp struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    string              `json:"body"`
	Error   string              `json:"error,omitempty"`
	Elapsed int64               `json:"elapsed_ms"`
}

type wireBatchReq struct {
	Requests []wireReq `json:"requests"`
}

type wireBatchResp struct {
	Responses []wireResp `json:"responses"`
	Region    string     `json:"region,omitempty"`
}

// New constructs a Pool from the embedded endpoints.json. Returns nil if
// secret is empty or no endpoints are configured — callers should treat that
// as "no proxy" and fall back to a direct client.
func New(secret string) (*Pool, error) {
	if secret == "" {
		return nil, nil
	}
	var f endpointsFile
	if err := json.Unmarshal(endpointsRaw, &f); err != nil {
		return nil, err
	}
	if len(f.Endpoints) == 0 {
		return nil, nil
	}
	return &Pool{
		endpoints: f.Endpoints,
		secret:    secret,
		client:    &http.Client{Timeout: 35 * time.Second},
		// flushAfter is the upper bound on how long we'll wait for more
		// requests to arrive before firing a batch. Measured batches max
		// out around 8 per cycle (well under maxBatch=50), so longer
		// waits would just be tax. 2ms is enough for the cycle's
		// concurrent goroutines to all enqueue without losing a full
		// network roundtrip to the wait.
		flushAfter: 2 * time.Millisecond,
		maxBatch:   50,
		cooldown:   60 * time.Second,
		// rlCooldown parks an egress IP after an upstream throttles it.
		// 5 min is long enough for rec.gov's per-IP window to reset but
		// short enough to return the IP to a 42-strong rotation quickly.
		rlCooldown:        5 * time.Minute,
		endpointBad:       map[string]time.Time{},
		endpointThrottled: map[string]time.Time{},
		stats:             map[string]*EndpointStat{},
		statsSince:        time.Now(),
	}, nil
}

// Endpoints returns a copy of the active endpoint list.
func (p *Pool) Endpoints() []Endpoint {
	out := make([]Endpoint, len(p.endpoints))
	copy(out, p.endpoints)
	return out
}

// RoundTrip queues req for batched dispatch and blocks until the response
// (or an error) arrives.
func (p *Pool) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyStr string
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		bodyStr = string(b)
	}
	pr := &pendingReq{
		req:  req,
		body: bodyStr,
		done: make(chan pendingResp, 1),
	}
	p.enqueue(pr)
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case r := <-pr.done:
		return r.resp, r.err
	}
}

func (p *Pool) enqueue(pr *pendingReq) {
	p.mu.Lock()
	p.pending = append(p.pending, pr)
	if len(p.pending) >= p.maxBatch {
		batch := p.pending
		p.pending = nil
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		p.mu.Unlock()
		go p.dispatch(batch)
		return
	}
	if p.timer == nil {
		p.timer = time.AfterFunc(p.flushAfter, p.flush)
	}
	p.mu.Unlock()
}

func (p *Pool) flush() {
	p.mu.Lock()
	batch := p.pending
	p.pending = nil
	p.timer = nil
	p.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	p.dispatch(batch)
}

// dispatch groups a batch by upstream host and sends one sub-batch per
// host. Same-host is the common case (one goroutine per provider), so this
// is usually a single group; when providers coincide, each host is
// dispatched concurrently so a slow host doesn't stall the other. Grouping
// by host keeps per-IP throttle attribution exact — every response in a
// sub-batch shares one upstream, so a 429 unambiguously implicates that
// (endpoint, host) pair.
func (p *Pool) dispatch(batch []*pendingReq) {
	byHost := map[string][]*pendingReq{}
	for _, pr := range batch {
		byHost[pr.req.URL.Host] = append(byHost[pr.req.URL.Host], pr)
	}
	if len(byHost) == 1 {
		for host, group := range byHost {
			p.dispatchHost(host, group)
		}
		return
	}
	var wg sync.WaitGroup
	for host, group := range byHost {
		wg.Add(1)
		go func(host string, group []*pendingReq) {
			defer wg.Done()
			p.dispatchHost(host, group)
		}(host, group)
	}
	wg.Wait()
}

func (p *Pool) dispatchHost(host string, batch []*pendingReq) {
	reqs := make([]wireReq, len(batch))
	for i, pr := range batch {
		h := map[string]string{}
		for k, v := range pr.req.Header {
			if len(v) > 0 {
				h[k] = v[0]
			}
		}
		reqs[i] = wireReq{
			URL:     pr.req.URL.String(),
			Method:  pr.req.Method,
			Headers: h,
			Body:    pr.body,
		}
	}
	payload, err := json.Marshal(wireBatchReq{Requests: reqs})
	if err != nil {
		p.finishAll(batch, nil, err)
		return
	}

	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < min(3, len(p.endpoints)); attempt++ {
		ep, ok := p.pick(tried, host)
		if !ok {
			break
		}
		tried[ep.URL] = true
		dispatchStart := time.Now()
		resp, err := p.sendBatch(ep, payload)
		dispatchSecs := time.Since(dispatchStart).Seconds()
		metrics.ProxyBatchSize.WithLabelValues(ep.URL, ep.Region).Observe(float64(len(batch)))
		metrics.ProxyDispatchDuration.
			WithLabelValues(ep.URL, ep.Region, metrics.BoolLabel(err == nil)).
			Observe(dispatchSecs)
		if err == nil {
			p.demux(batch, resp, ep, host)
			return
		}
		p.markBad(ep.URL)
		lastErr = err
		slog.Warn("proxy batch attempt failed", "endpoint", ep.URL, "region", ep.Region, "err", err)
	}
	if lastErr == nil {
		lastErr = errors.New("no healthy proxy endpoints")
	}
	p.finishAll(batch, nil, lastErr)
}

func (p *Pool) sendBatch(ep Endpoint, payload []byte) (*wireBatchResp, error) {
	req, err := http.NewRequest(http.MethodPost, ep.URL+"/fetch", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &proxyHTTPError{Status: resp.StatusCode, Body: string(b)}
	}
	var br wireBatchResp
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		return nil, err
	}
	return &br, nil
}

type proxyHTTPError struct {
	Status int
	Body   string
}

func (e *proxyHTTPError) Error() string {
	return "proxy returned " + http.StatusText(e.Status) + ": " + e.Body
}

func (p *Pool) demux(batch []*pendingReq, br *wireBatchResp, ep Endpoint, host string) {
	if len(br.Responses) != len(batch) {
		p.finishAll(batch, nil, errors.New("proxy response length mismatch"))
		return
	}
	throttled := false
	for i, pr := range batch {
		wr := br.Responses[i]
		p.recordResult(ep, wr)
		if wr.Status == http.StatusTooManyRequests || wr.Status == http.StatusForbidden {
			throttled = true
		}
		if wr.Error != "" {
			pr.done <- pendingResp{err: errors.New(wr.Error)}
			continue
		}
		hdr := http.Header{}
		for k, vv := range wr.Headers {
			ck := textproto.CanonicalMIMEHeaderKey(k)
			for _, v := range vv {
				hdr.Add(ck, v)
			}
		}
		hdr.Set(ProxyRegionHeader, ep.Region)
		hdr.Set("X-Proxy-Provider", ep.Provider)
		if wr.Elapsed > 0 {
			hdr.Set(UpstreamElapsedHeader, strconv.FormatInt(wr.Elapsed, 10))
		}
		resp := &http.Response{
			StatusCode: wr.Status,
			Status:     http.StatusText(wr.Status),
			Header:     hdr,
			Body:       io.NopCloser(strings.NewReader(wr.Body)),
			Request:    pr.req,
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
		}
		resp.ContentLength = int64(len(wr.Body))
		pr.done <- pendingResp{resp: resp}
	}
	if throttled {
		p.throttle(ep, host)
	}
}

// recordResult tallies one demuxed upstream response for the daily report.
func (p *Pool) recordResult(ep Endpoint, wr wireResp) {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	s := p.stats[ep.URL]
	if s == nil {
		s = &EndpointStat{URL: ep.URL, Region: ep.Region}
		p.stats[ep.URL] = s
	}
	s.Requests++
	switch wr.Status {
	case http.StatusTooManyRequests:
		s.RateLimited++
	case http.StatusForbidden:
		s.Forbidden++
	}
}

// throttle parks an endpoint for one upstream host after that host
// rate-limited (429) or blocked (403) it. Other hosts keep using the
// endpoint; only this (endpoint, host) pair is cooled down.
func (p *Pool) throttle(ep Endpoint, host string) {
	p.mu.Lock()
	p.endpointThrottled[badKey(ep.URL, host)] = time.Now().Add(p.rlCooldown)
	p.mu.Unlock()

	p.statsMu.Lock()
	if s := p.stats[ep.URL]; s != nil {
		s.Cooldowns++
	}
	p.statsMu.Unlock()

	metrics.ProxyEndpointBadTotal.WithLabelValues(ep.URL).Inc()
	slog.Warn("proxy endpoint throttled by upstream",
		"endpoint", ep.URL, "region", ep.Region, "host", host,
		"cooldown", p.rlCooldown)
}

// DrainStats returns and clears the per-endpoint tallies, along with the
// time collection started. Called once per day by the report.
func (p *Pool) DrainStats() ([]EndpointStat, time.Time) {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	out := make([]EndpointStat, 0, len(p.stats))
	for _, s := range p.stats {
		out = append(out, *s)
	}
	since := p.statsSince
	p.stats = map[string]*EndpointStat{}
	p.statsSince = time.Now()
	return out, since
}

// Snapshot returns a copy of the current tallies without clearing them,
// along with the time collection started. Used by the on-demand
// /schniff ratelimits command; the daily DrainStats still owns the reset.
func (p *Pool) Snapshot() ([]EndpointStat, time.Time) {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	out := make([]EndpointStat, 0, len(p.stats))
	for _, s := range p.stats {
		out = append(out, *s)
	}
	return out, p.statsSince
}

// HasRateLimiting reports whether the tallies contain any 429 or 403 —
// the trigger for the daily problemos post.
func HasRateLimiting(stats []EndpointStat) bool {
	for _, s := range stats {
		if s.RateLimited+s.Forbidden > 0 {
			return true
		}
	}
	return false
}

func (p *Pool) finishAll(batch []*pendingReq, resp *http.Response, err error) {
	for _, pr := range batch {
		pr.done <- pendingResp{resp: resp, err: err}
	}
}

// pick returns the next healthy endpoint for host in round-robin order,
// skipping ones already tried this dispatch, globally cooled down (proxy
// failure), or throttled for this specific host (upstream 429/403). If
// every endpoint is unavailable it falls back to any untried endpoint —
// trying a throttled IP beats dropping the request.
func (p *Pool) pick(skip map[string]bool, host string) (Endpoint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := len(p.endpoints)
	for range n {
		idx := int(p.rrIdx.Add(1)) % n
		if idx < 0 {
			idx += n
		}
		ep := p.endpoints[idx]
		if skip[ep.URL] {
			continue
		}
		if t, bad := p.endpointBad[ep.URL]; bad && t.After(now) {
			continue
		}
		if t, thr := p.endpointThrottled[badKey(ep.URL, host)]; thr && t.After(now) {
			continue
		}
		return ep, true
	}
	for _, ep := range p.endpoints {
		if !skip[ep.URL] {
			return ep, true
		}
	}
	return Endpoint{}, false
}

func (p *Pool) markBad(url string) {
	p.mu.Lock()
	p.endpointBad[url] = time.Now().Add(p.cooldown)
	p.mu.Unlock()
	metrics.ProxyEndpointBadTotal.WithLabelValues(url).Inc()
}
