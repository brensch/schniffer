package providers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/brensch/schniffer/internal/metrics"
	"github.com/brensch/schniffer/internal/proxypool"
)

// observeUpstream records the upstream status code and, when the response
// came via the proxy pool (headers stamped by demux), the upstream
// duration. Duration is silently skipped for direct requests.
func observeUpstream(provider string, resp *http.Response) {
	if resp == nil {
		return
	}
	metrics.ProviderUpstreamStatus.
		WithLabelValues(provider, strconv.Itoa(resp.StatusCode)).
		Inc()
	v := resp.Header.Get(proxypool.UpstreamElapsedHeader)
	if v == "" {
		return
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms < 0 {
		return
	}
	region := resp.Header.Get(proxypool.ProxyRegionHeader)
	metrics.ProviderUpstreamDuration.
		WithLabelValues(provider, region).
		Observe(float64(ms) / 1000.0)
}

// recordFetch records one fetch attempt's outcome. ok must mean "usable
// response" (transport succeeded AND HTTP 200 AND body parsed), not just
// "the request didn't error" — WAF 403s and 429s are failures.
func recordFetch(provider string, start time.Time, ok bool) {
	metrics.ProviderFetchTotal.WithLabelValues(provider, metrics.BoolLabel(ok)).Inc()
	metrics.ProviderFetchDuration.
		WithLabelValues(provider, metrics.BoolLabel(ok)).
		Observe(time.Since(start).Seconds())
}
