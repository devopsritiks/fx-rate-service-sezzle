// Package metrics defines every Prometheus metric this service exports
// and provides small hook-shaped methods (OnXxx) that match the Hooks
// structs already defined in internal/httpapi, internal/fxvendor/*, and
// internal/ratelimit. Nothing in those packages imports Prometheus
// directly — main.go is the only place that wires a *metrics.Metrics
// into their hook fields, so metrics stay purely an observer and never a
// dependency of business logic.
//
// On cardinality (why labels are chosen the way they are below): every
// distinct combination of label values becomes its own time series that
// Prometheus stores and indexes until retention expiry. A label whose
// values are unbounded or caller-controlled — a raw URL path, a client
// IP, a currency code, an amount — means the number of series grows
// without bound as traffic varies, which is the single most common way
// to make a Prometheus instance slow or fall over on memory. Every label
// used here is one of: a route *pattern* (a small fixed set we define,
// e.g. "/v1/rates/{base}", never the literal requested path), an HTTP
// method, an HTTP status code, or a small fixed outcome enum (e.g.
// "hit"/"miss"/"stale"). Nothing here is ever keyed by IP, currency code,
// or amount.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "fxservice"

// Metrics holds every collector this service exports. Construct once
// with New and register it with RegisterOn (or just use
// prometheus.DefaultRegisterer, which promhttp.Handler() already serves).
type Metrics struct {
	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec

	VendorCallDuration *prometheus.HistogramVec
	RetryAttemptsTotal prometheus.Counter

	BreakerState *prometheus.GaugeVec

	CacheRequestsTotal           *prometheus.CounterVec
	CacheEvictionsTotal          prometheus.Counter
	CacheEntries                 prometheus.Gauge
	CacheSingleflightSharedTotal prometheus.Counter

	RateLimitRejectionsTotal prometheus.Counter

	VendorInFlight prometheus.Gauge

	BuildInfo *prometheus.GaugeVec

	// Audit log (async writer + pgxpool)
	AuditQueueDepth          prometheus.Gauge
	AuditRecordsWrittenTotal prometheus.Counter
	AuditRecordsDroppedTotal prometheus.Counter
	AuditWriteErrorsTotal    prometheus.Counter
	AuditBatchWriteDuration  prometheus.Histogram

	// pgxpool stats, sampled periodically (see main.go) — point-in-time
	// facts, not events, same reasoning as CacheEntries above.
	PgPoolConnsTotal          prometheus.Gauge
	PgPoolConnsIdle           prometheus.Gauge
	PgPoolConnsAcquired       prometheus.Gauge
	PgPoolConnsMax            prometheus.Gauge
	PgPoolAcquireCountTotal   prometheus.Gauge
	PgPoolAcquireDurationSecs prometheus.Gauge
	PgPoolEmptyAcquireTotal   prometheus.Gauge
}

// httpDurationBuckets spans 1ms to 5s. This is a cache-first service —
// most responses should be a cache hit and return in low single-digit
// milliseconds; the upper end covers a cache miss that has to go through
// the full retry+vendor-call path.
var httpDurationBuckets = []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}

// vendorDurationBuckets spans 10ms to 10s. The upper end is sized against
// UPSTREAM_TIMEOUT (default 4s) multiplied by a few retry attempts plus
// backoff, so a slow-but-eventually-successful call still lands inside
// the histogram's range instead of all piling into a single +Inf bucket.
var vendorDurationBuckets = []float64{.01, .025, .05, .1, .25, .5, 1, 2, 4, 8, 10}

// auditBatchDurationBuckets spans 1ms to 2s. A healthy batch insert of
// up to AUDIT_BATCH_SIZE rows via CopyFrom should be low single-digit
// milliseconds; the upper end covers a slow or momentarily-contended
// Postgres without everything piling into +Inf.
var auditBatchDurationBuckets = []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2}

// BreakerStateValue maps a resilience.Client.BreakerState() string to the
// numeric value the gauge reports. Numeric, not a label, because breaker
// state is a single value that changes over time for one fixed series —
// exactly what a gauge is for; encoding it as a label instead would mean
// three permanently-present series (one per state) where only one is
// ever "current," which is more confusing to query than it's worth.
func BreakerStateValue(state string) float64 {
	switch state {
	case "closed":
		return 0
	case "half-open":
		return 1
	case "open":
		return 2
	default:
		return -1
	}
}

// New creates all collectors and registers them with reg.
func New(reg prometheus.Registerer, version string) *Metrics {
	m := &Metrics{
		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_requests_total",
			Help:      "Total inbound HTTP requests, labeled by method, route pattern, and status code.",
		}, []string{"method", "route", "status"}),

		HTTPRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_duration_seconds",
			Help:      "Inbound HTTP request latency in seconds, labeled by method and route pattern only (status is on the counter, not here, to keep histogram series count down).",
			Buckets:   httpDurationBuckets,
		}, []string{"method", "route"}),

		VendorCallDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "vendor_call_duration_seconds",
			Help:      "Latency of a full call to the Frankfurter vendor (including retries), labeled by outcome.",
			Buckets:   vendorDurationBuckets,
		}, []string{"outcome"}),

		RetryAttemptsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "vendor_retry_attempts_total",
			Help:      "Total retry attempts made against the vendor (not counting the first attempt of each call).",
		}),

		BreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "circuit_breaker_state",
			Help:      "Current circuit breaker state: 0=closed, 1=half-open, 2=open.",
		}, []string{"breaker"}),

		CacheRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_requests_total",
			Help:      "Total rates-cache lookups, labeled by result: hit, miss, stale, or negative_hit.",
		}, []string{"result"}),

		CacheEvictionsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_evictions_total",
			Help:      "Total cache entries evicted due to capacity pressure (LRU).",
		}),

		CacheEntries: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "cache_entries",
			Help:      "Current number of distinct base+symbols entries held in the rates cache.",
		}),

		CacheSingleflightSharedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_singleflight_shared_total",
			Help:      "Total requests that received a result from another in-flight caller's vendor call instead of making their own (thundering-herd protection).",
		}),

		RateLimitRejectionsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rate_limit_rejections_total",
			Help:      "Total inbound requests rejected with 429 by the per-IP rate limiter.",
		}),

		VendorInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "vendor_inflight_calls",
			Help:      "Current number of calls to the vendor in flight through the bulkhead.",
		}),

		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "build_info",
			Help:      "Always 1; version is reported as a label. A single fixed-cardinality series per process, not a per-request label.",
		}, []string{"version"}),

		AuditQueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "audit_queue_depth",
			Help:      "Current number of audit records buffered waiting to be written to Postgres.",
		}),
		AuditRecordsWrittenTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "audit_records_written_total",
			Help:      "Total audit records successfully written to Postgres.",
		}),
		AuditRecordsDroppedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "audit_records_dropped_total",
			Help:      "Total audit records dropped because the in-memory queue was full. Never blocks the API; see internal/audit's design doc.",
		}),
		AuditWriteErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "audit_write_errors_total",
			Help:      "Total failed batch write attempts to Postgres (each retried with backoff).",
		}),
		AuditBatchWriteDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "audit_batch_write_duration_seconds",
			Help:      "Latency of one audit batch write attempt to Postgres (success or failure).",
			Buckets:   auditBatchDurationBuckets,
		}),

		PgPoolConnsTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "pg_pool_conns_total",
			Help: "pgxpool: total connections currently held by the pool (idle + in-use).",
		}),
		PgPoolConnsIdle: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "pg_pool_conns_idle",
			Help: "pgxpool: idle connections currently available for use.",
		}),
		PgPoolConnsAcquired: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "pg_pool_conns_acquired",
			Help: "pgxpool: connections currently checked out and in use.",
		}),
		PgPoolConnsMax: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "pg_pool_conns_max",
			Help: "pgxpool: the configured maximum pool size (POSTGRES_MAX_CONNS).",
		}),
		PgPoolAcquireCountTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "pg_pool_acquire_count_total",
			Help: "pgxpool: lifetime count of successful connection acquisitions (monotonic counter sourced from pool.Stat(), exposed as a gauge since we sample rather than increment it ourselves).",
		}),
		PgPoolAcquireDurationSecs: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "pg_pool_acquire_duration_seconds_total",
			Help: "pgxpool: lifetime cumulative time spent waiting to acquire a connection, in seconds.",
		}),
		PgPoolEmptyAcquireTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "pg_pool_empty_acquire_count_total",
			Help: "pgxpool: lifetime count of acquisitions that had to wait because no idle connection was immediately available — sustained growth here means the pool is undersized for the load.",
		}),
	}

	reg.MustRegister(
		m.HTTPRequestsTotal,
		m.HTTPRequestDuration,
		m.VendorCallDuration,
		m.RetryAttemptsTotal,
		m.BreakerState,
		m.CacheRequestsTotal,
		m.CacheEvictionsTotal,
		m.CacheEntries,
		m.CacheSingleflightSharedTotal,
		m.RateLimitRejectionsTotal,
		m.VendorInFlight,
		m.BuildInfo,
		m.AuditQueueDepth,
		m.AuditRecordsWrittenTotal,
		m.AuditRecordsDroppedTotal,
		m.AuditWriteErrorsTotal,
		m.AuditBatchWriteDuration,
		m.PgPoolConnsTotal,
		m.PgPoolConnsIdle,
		m.PgPoolConnsAcquired,
		m.PgPoolConnsMax,
		m.PgPoolAcquireCountTotal,
		m.PgPoolAcquireDurationSecs,
		m.PgPoolEmptyAcquireTotal,
	)

	m.BuildInfo.WithLabelValues(version).Set(1)

	return m
}

// ObserveHTTPRequest records one inbound request's RED data. route must
// be a bounded pattern (see routePattern in internal/httpapi), never a
// raw path.
func (m *Metrics) ObserveHTTPRequest(method, route string, status int, duration time.Duration) {
	m.HTTPRequestsTotal.WithLabelValues(method, route, statusLabel(status)).Inc()
	m.HTTPRequestDuration.WithLabelValues(method, route).Observe(duration.Seconds())
}

// statusLabel formats an HTTP status as a label value. Status codes are
// a small, fixed, well-known set (not caller-controlled in any way that
// grows unboundedly), so this is safe to use directly as a label.
func statusLabel(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return "other"
	}
}
