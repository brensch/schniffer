// Package metrics owns every Prometheus metric exported by schniffer.
//
// All metrics live here so cardinality and naming stay reviewable in one
// place. Hot-path code calls helpers like Observe* / Inc* — never touches
// the prometheus client directly — which keeps the rest of the codebase
// free of import churn if we swap registries later.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Reg is the registry exposed by /metrics. Kept separate from
// prometheus.DefaultRegisterer so we control exactly what's exported.
var Reg = prometheus.NewRegistry()

var (
	// Bucket sets reused across families. Picked from observed scrape +
	// booking latencies: scrapes land in the 100ms–2s band, booker phases
	// run from ~50ms (script eval) to tens of seconds (cold nav).

	bucketsHTTP = []float64{
		0.05, 0.1, 0.2, 0.35, 0.5, 0.75, 1, 1.5, 2, 3, 5, 7.5, 10, 15, 20, 30,
	}
	bucketsDB = []float64{
		0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
	}
	bucketsBooker = []float64{
		0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 7.5, 10, 15, 20, 30, 45, 60, 90, 120,
	}
	bucketsBatchSize = prometheus.LinearBuckets(1, 2, 26) // 1,3,5…51
)

var (
	ProviderFetchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "schniffer_provider_fetch_duration_seconds",
			Help:    "Wall time of a single FetchAvailability call (one campground, one date range bucket).",
			Buckets: bucketsHTTP,
		},
		[]string{"provider", "success"},
	)

	ProviderUpstreamDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "schniffer_provider_upstream_duration_seconds",
			Help:    "Upstream time reported by the proxy worker (elapsed_ms in proxy response). Excludes our network leg to the proxy.",
			Buckets: bucketsHTTP,
		},
		[]string{"provider", "region"},
	)

	ProviderFetchTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "schniffer_provider_fetch_total",
			Help: "Count of FetchAvailability calls by provider and outcome.",
		},
		[]string{"provider", "success"},
	)

	ProviderUpstreamStatus = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "schniffer_provider_upstream_status_total",
			Help: "Upstream HTTP responses by provider and status code. Separates WAF blocks (403) and throttles (429) from real availability data (200).",
		},
		[]string{"provider", "status"},
	)

	ProxyBatchSize = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "schniffer_proxy_batch_size",
			Help:    "Number of requests coalesced into one proxy dispatch.",
			Buckets: bucketsBatchSize,
		},
		[]string{"endpoint", "region"},
	)

	ProxyDispatchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "schniffer_proxy_dispatch_duration_seconds",
			Help:    "Wall time of one batched POST to a proxy endpoint (our side, including the proxy worker).",
			Buckets: bucketsHTTP,
		},
		[]string{"endpoint", "region", "success"},
	)

	ProxyEndpointBadTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "schniffer_proxy_endpoint_bad_total",
			Help: "Times a proxy endpoint was marked unhealthy and cooled down.",
		},
		[]string{"endpoint"},
	)

	PollCycleDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "schniffer_poll_cycle_duration_seconds",
			Help:    "Wall time of one PollProvider cycle.",
			Buckets: bucketsHTTP,
		},
		[]string{"provider"},
	)

	PollCycleCampgrounds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "schniffer_poll_cycle_campgrounds",
			Help:    "Distinct (provider, campground) pairs scraped in a single poll cycle.",
			Buckets: prometheus.LinearBuckets(0, 5, 21),
		},
		[]string{"provider"},
	)

	PollCycleFailed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "schniffer_poll_cycle_failed_campgrounds_total",
			Help: "Campground fetches that failed within a poll cycle.",
		},
		[]string{"provider"},
	)

	BookerHoldDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "schniffer_booker_hold_duration_seconds",
			Help:    "Per-phase wall time of a HoldCampsite attempt. phase=total covers the whole call.",
			Buckets: bucketsBooker,
		},
		[]string{"phase", "outcome"},
	)

	BookerSessionWait = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "schniffer_booker_session_wait_seconds",
			Help:    "Time HoldCampsite spent waiting for the per-user session lock before doing any work.",
			Buckets: bucketsBooker,
		},
	)

	BookerRelaunchTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "schniffer_booker_relaunch_total",
			Help: "Times the pool tore down and relaunched a user's Chrome session.",
		},
		[]string{"reason"},
	)

	WebRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "schniffer_web_request_duration_seconds",
			Help:    "Wall time of a single HTTP request to the user-facing web server.",
			Buckets: bucketsHTTP,
		},
		[]string{"route", "method", "status"},
	)

	WebRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "schniffer_web_requests_total",
			Help: "Count of HTTP requests to the web server by route, method, and status.",
		},
		[]string{"route", "method", "status"},
	)

	WebRequestsInFlight = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "schniffer_web_requests_in_flight",
			Help: "Number of HTTP requests currently being served by the web server.",
		},
	)

	WebResponseBytes = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "schniffer_web_response_bytes",
			Help: "Response body size in bytes, by route. Helps spot endpoints shipping unnecessarily heavy payloads.",
			// Geometric from 256B to 4MB.
			Buckets: prometheus.ExponentialBuckets(256, 4, 8),
		},
		[]string{"route"},
	)

	DBQueryDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "schniffer_db_query_duration_seconds",
			Help:    "SQLite query wall time. The query label is the first 80 chars normalized; cardinality is bounded by the small set of distinct queries in the codebase.",
			Buckets: bucketsDB,
		},
		[]string{"query"},
	)
)

func init() {
	Reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		ProviderFetchDuration,
		ProviderUpstreamDuration,
		ProviderFetchTotal,
		ProviderUpstreamStatus,
		ProxyBatchSize,
		ProxyDispatchDuration,
		ProxyEndpointBadTotal,
		PollCycleDuration,
		PollCycleCampgrounds,
		PollCycleFailed,
		BookerHoldDuration,
		BookerSessionWait,
		BookerRelaunchTotal,
		WebRequestDuration,
		WebRequestsTotal,
		WebRequestsInFlight,
		WebResponseBytes,
		DBQueryDuration,
	)
}

// Handler returns the http.Handler that serves /metrics.
func Handler() http.Handler {
	return promhttp.HandlerFor(Reg, promhttp.HandlerOpts{Registry: Reg})
}

// Seconds is a small helper so callers can write:
//
//	defer metrics.ProviderFetchDuration.WithLabelValues(p, "true").Observe(metrics.Seconds(start))
//
// without sprinkling time.Since(...).Seconds() everywhere.
func Seconds(start time.Time) float64 { return time.Since(start).Seconds() }

// BoolLabel maps a bool to the canonical "true"/"false" label used across
// success/outcome dimensions so we don't accidentally split them ("ok"/"true").
func BoolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
