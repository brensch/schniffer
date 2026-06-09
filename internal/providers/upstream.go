package providers

import (
	"net/http"
	"strconv"

	"github.com/brensch/schniffer/internal/metrics"
	"github.com/brensch/schniffer/internal/proxypool"
)

// observeUpstream records ProviderUpstreamDuration from headers stamped
// by the proxy pool's demux. Missing headers (e.g. when the request went
// direct, not via the pool) are silently skipped.
func observeUpstream(provider string, resp *http.Response) {
	if resp == nil {
		return
	}
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
