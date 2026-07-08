package proxypool

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"sort"
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
	// endpointBad cools an endpoint out of rotation for all targets after a
	// proxy-level failure (transport error / non-200 from the proxy itself).
	endpointBad map[string]time.Time
	// endpointHealth tracks per-(endpoint, target) escalating backoff after
	// upstream 403/429s. Keyed by badKey(url, target); absent means healthy.
	endpointHealth map[string]*epHealth

	statsMu    sync.Mutex
	stats      map[string]*EndpointStat
	statsSince time.Time
}

// backoffLadder is the escalating out-of-rotation duration per consecutive
// failure level for one (endpoint, target). It grows until it caps at a day,
// so a persistently-blocked IP is re-probed roughly daily and rejoins the
// instant it succeeds again. Index = failLevel - 1.
var backoffLadder = []time.Duration{
	2 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

func backoffFor(level int) time.Duration {
	if level <= 0 {
		return 0
	}
	if level > len(backoffLadder) {
		level = len(backoffLadder)
	}
	return backoffLadder[level-1]
}

// epHealth is one (endpoint, target) pair's escalating backoff. An entry
// exists only once the pair has failed; a success deletes it (healthy).
type epHealth struct {
	failLevel int
	nextRetry time.Time
	lastFail  time.Time
}

// EndpointStat is a rolling per-(target, endpoint) tally of upstream
// outcomes, drained by the daily report. Target is the friendly provider
// name when the request was tagged (see WithProvider), else the upstream
// host. Requests counts every demuxed response; Failed counts the ones
// that weren't a clean 2xx (transport error, 4xx, 5xx) — we deliberately
// don't split 403 vs 429, since both usually mean the same thing.
type EndpointStat struct {
	Target   string
	URL      string
	Region   string
	Requests int64
	Failed   int64
}

// badKey namespaces a per-host throttle entry. NUL can't appear in a URL
// or host, so it's a safe separator.
func badKey(url, host string) string { return url + "\x00" + host }

// statKey namespaces a per-(target, endpoint) stat entry.
func statKey(target, url string) string { return target + "\x00" + url }

// providerCtxKey tags a request context with the friendly provider name so
// the pool can attribute stats to it without a hardcoded host→name table.
type providerCtxKey struct{}

// WithProvider returns a context that tags outbound requests with the
// given provider name for rate-limit reporting. The manager sets this from
// the provider registry; requests without it fall back to their host.
func WithProvider(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, providerCtxKey{}, name)
}

func providerFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(providerCtxKey{}).(string); ok {
		return v
	}
	return ""
}

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
		flushAfter:     2 * time.Millisecond,
		maxBatch:       50,
		cooldown:       60 * time.Second,
		endpointBad:    map[string]time.Time{},
		endpointHealth: map[string]*epHealth{},
		stats:          map[string]*EndpointStat{},
		statsSince:     time.Now(),
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
	// All requests in a host group share one upstream, hence one target.
	target := providerFromCtx(batch[0].req.Context())
	if target == "" {
		target = host
	}
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
			p.demux(batch, resp, ep, target)
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

func (p *Pool) demux(batch []*pendingReq, br *wireBatchResp, ep Endpoint, target string) {
	if len(br.Responses) != len(batch) {
		p.finishAll(batch, nil, errors.New("proxy response length mismatch"))
		return
	}
	sawBlock, sawOK := false, false
	for i, pr := range batch {
		wr := br.Responses[i]
		p.recordResult(target, ep, wr)
		switch {
		case wr.Status == http.StatusTooManyRequests || wr.Status == http.StatusForbidden:
			sawBlock = true
		case wr.Status >= 200 && wr.Status < 300:
			sawOK = true
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
	// A 403/429 escalates this (endpoint, target)'s backoff; a clean 2xx
	// with no blocks clears it back to healthy.
	if sawBlock {
		p.onFailure(ep, target)
	} else if sawOK {
		p.onSuccess(ep, target)
	}
}

// recordResult tallies one demuxed upstream response against a
// (target, endpoint) pair. A response is Failed unless it's a clean 2xx —
// transport error, 4xx, and 5xx all count, without distinguishing 403 from
// 429.
func (p *Pool) recordResult(target string, ep Endpoint, wr wireResp) {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	key := statKey(target, ep.URL)
	s := p.stats[key]
	if s == nil {
		s = &EndpointStat{Target: target, URL: ep.URL, Region: ep.Region}
		p.stats[key] = s
	}
	s.Requests++
	if wr.Error != "" || wr.Status < 200 || wr.Status >= 300 {
		s.Failed++
	}
}

// onFailure escalates the (endpoint, target) backoff after a 403/429 and
// parks the endpoint out of that target's rotation for the new duration.
// Only this pair is affected — the endpoint stays healthy for other targets.
func (p *Pool) onFailure(ep Endpoint, target string) {
	key := badKey(ep.URL, target)
	now := time.Now()
	p.mu.Lock()
	h := p.endpointHealth[key]
	if h == nil {
		h = &epHealth{}
		p.endpointHealth[key] = h
	}
	h.failLevel++
	h.lastFail = now
	h.nextRetry = now.Add(backoffFor(h.failLevel))
	level := h.failLevel
	p.mu.Unlock()

	metrics.ProxyEndpointBadTotal.WithLabelValues(ep.URL).Inc()
	slog.Warn("proxy endpoint backing off",
		"endpoint", ep.URL, "region", ep.Region, "target", target,
		"fail_level", level, "backoff", backoffFor(level))
}

// onSuccess clears any backoff for the (endpoint, target) pair — one good
// response restores it to healthy, so a recovered IP rejoins immediately.
func (p *Pool) onSuccess(ep Endpoint, target string) {
	key := badKey(ep.URL, target)
	p.mu.Lock()
	delete(p.endpointHealth, key)
	p.mu.Unlock()
}

// IPHealth is one endpoint's state for one target, for the dashboard.
type IPHealth struct {
	Region     string `json:"region"`
	URL        string `json:"url"`
	State      string `json:"state"` // healthy | backing_off | blocked
	FailLevel  int    `json:"failLevel"`
	RetryInSec int64  `json:"retryInSec"`
	Requests   int64  `json:"requests"`
	Failed     int64  `json:"failed"`
}

func stateRank(s string) int {
	switch s {
	case "blocked":
		return 2
	case "backing_off":
		return 1
	default:
		return 0
	}
}

// HealthByTarget returns, for every target that has seen traffic, the state
// of all pool endpoints (healthy / backing_off / blocked) with their tallies
// and time-to-next-probe. Worst state first. Powers the dashboard IP view.
func (p *Pool) HealthByTarget() map[string][]IPHealth {
	// Snapshot stats (targets + tallies) and health under their own locks —
	// never nested, so no deadlock with the hot dispatch path.
	p.statsMu.Lock()
	targets := map[string]struct{}{}
	tally := make(map[string]EndpointStat, len(p.stats))
	for k, s := range p.stats {
		targets[s.Target] = struct{}{}
		tally[k] = *s
	}
	p.statsMu.Unlock()

	now := time.Now()
	p.mu.Lock()
	health := make(map[string]epHealth, len(p.endpointHealth))
	for k, h := range p.endpointHealth {
		health[k] = *h
	}
	p.mu.Unlock()

	out := make(map[string][]IPHealth, len(targets))
	for target := range targets {
		rows := make([]IPHealth, 0, len(p.endpoints))
		for _, ep := range p.endpoints {
			ih := IPHealth{Region: ep.Region, URL: ep.URL, State: "healthy"}
			if s, ok := tally[statKey(target, ep.URL)]; ok {
				ih.Requests, ih.Failed = s.Requests, s.Failed
			}
			if h, ok := health[badKey(ep.URL, target)]; ok && h.failLevel > 0 {
				ih.FailLevel = h.failLevel
				if h.nextRetry.After(now) {
					ih.RetryInSec = int64(h.nextRetry.Sub(now).Seconds())
				}
				if h.failLevel >= len(backoffLadder) {
					ih.State = "blocked"
				} else {
					ih.State = "backing_off"
				}
			}
			rows = append(rows, ih)
		}
		sort.Slice(rows, func(i, j int) bool {
			if ri, rj := stateRank(rows[i].State), stateRank(rows[j].State); ri != rj {
				return ri > rj
			}
			if rows[i].FailLevel != rows[j].FailLevel {
				return rows[i].FailLevel > rows[j].FailLevel
			}
			return rows[i].Region < rows[j].Region
		})
		out[target] = rows
	}
	return out
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

// HasFailures reports whether the tallies contain any failed request —
// the trigger for the daily problemos post.
func HasFailures(stats []EndpointStat) bool {
	for _, s := range stats {
		if s.Failed > 0 {
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

// pick returns the next endpoint for target in round-robin order, skipping
// ones already tried this dispatch, globally cooled down (proxy failure), or
// backing off for this target (escalating after 403/429). If every endpoint
// is unavailable it falls back to any untried endpoint — trying a backed-off
// IP beats dropping the request.
func (p *Pool) pick(skip map[string]bool, target string) (Endpoint, bool) {
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
		if h, ok := p.endpointHealth[badKey(ep.URL, target)]; ok && h.nextRetry.After(now) {
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
