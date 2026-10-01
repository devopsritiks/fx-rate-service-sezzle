package metrics

import "time"

// The methods below exist purely to match the hook function signatures
// already defined in internal/fxvendor/resilience, internal/fxvendor/ratescache,
// and internal/cache, so main.go can wire them in with e.g.
// resilience.Hooks{OnBreakerStateChange: m.OnBreakerStateChange}
// without any of those packages importing Prometheus.

// OnBreakerStateChange matches resilience.Hooks.OnBreakerStateChange.
func (m *Metrics) OnBreakerStateChange(from, to string) {
	m.BreakerState.WithLabelValues("frankfurter").Set(BreakerStateValue(to))
}

// OnRetryAttempt matches an attempt-count signature callers can wrap
// resilience.RetryHooks.OnRetry with: every call to this means one retry
// attempt was made (the first attempt of a call is not a "retry").
func (m *Metrics) OnRetryAttempt() {
	m.RetryAttemptsTotal.Inc()
}

// OnVendorCallComplete records one full vendor call's outcome and
// latency. outcome should be one of "success", "error", "timeout", or
// "breaker_open" — a fixed small set, never a raw error string.
func (m *Metrics) OnVendorCallComplete(outcome string, duration time.Duration) {
	m.VendorCallDuration.WithLabelValues(outcome).Observe(duration.Seconds())
}

// OnBulkheadAcquire / OnBulkheadRelease track in-flight vendor calls.
func (m *Metrics) OnBulkheadAcquire() {
	m.VendorInFlight.Inc()
}

func (m *Metrics) OnBulkheadRelease() {
	m.VendorInFlight.Dec()
}

// OnCacheHit matches ratescache.Hooks.OnHit (key string).
func (m *Metrics) OnCacheHit(key string) {
	m.CacheRequestsTotal.WithLabelValues("hit").Inc()
}

// OnCacheMiss matches ratescache.Hooks.OnMiss (key string).
func (m *Metrics) OnCacheMiss(key string) {
	m.CacheRequestsTotal.WithLabelValues("miss").Inc()
}

// OnCacheStale matches ratescache.Hooks.OnStale (key string, age time.Duration).
func (m *Metrics) OnCacheStale(key string, age time.Duration) {
	m.CacheRequestsTotal.WithLabelValues("stale").Inc()
}

// OnCacheNegativeHit records a lookup served from the negative cache
// (a remembered "vendor said this currency doesn't exist"). ratescache
// doesn't currently have a dedicated hook for this path — see the note
// in internal/fxvendor/ratescache/ratescache.go on where this would plug
// in if added.
func (m *Metrics) OnCacheNegativeHit(key string) {
	m.CacheRequestsTotal.WithLabelValues("negative_hit").Inc()
}

// OnSingleflightShared matches ratescache.Hooks.OnSingleflightShared (key string).
func (m *Metrics) OnSingleflightShared(key string) {
	m.CacheSingleflightSharedTotal.Inc()
}

// OnCacheEvict matches cache.Hooks.OnEvict (key string, reason cache.EvictReason).
// Declared with `any` for reason rather than importing internal/cache's
// EvictReason type, so this package has no import-time dependency on
// cache — main.go does the small type-match when wiring the hook.
func (m *Metrics) OnCacheEvict(key string, reason any) {
	m.CacheEvictionsTotal.Inc()
}

// SetCacheEntries updates the current cache size gauge. Called
// periodically (see main.go) rather than from a hook, since cache size
// is a point-in-time fact, not an event.
func (m *Metrics) SetCacheEntries(n int) {
	m.CacheEntries.Set(float64(n))
}

// OnRateLimitRejected matches ratelimit's rejection path.
func (m *Metrics) OnRateLimitRejected() {
	m.RateLimitRejectionsTotal.Inc()
}

// OnAuditWritten matches audit.Hooks.OnWritten(n int).
func (m *Metrics) OnAuditWritten(n int) {
	m.AuditRecordsWrittenTotal.Add(float64(n))
}

// OnAuditDropped matches audit.Hooks.OnDropped(n int).
func (m *Metrics) OnAuditDropped(n int) {
	m.AuditRecordsDroppedTotal.Add(float64(n))
}

// OnAuditWriteError matches audit.Hooks.OnWriteError(err error).
func (m *Metrics) OnAuditWriteError(err error) {
	m.AuditWriteErrorsTotal.Inc()
}

// OnAuditBatchWriteDone matches audit.Hooks.OnBatchWriteDone(d time.Duration).
func (m *Metrics) OnAuditBatchWriteDone(d time.Duration) {
	m.AuditBatchWriteDuration.Observe(d.Seconds())
}

// SetAuditQueueDepth updates the audit-queue-depth gauge. Called
// periodically (see main.go), not from a hook — queue depth is a
// point-in-time fact, not an event.
func (m *Metrics) SetAuditQueueDepth(n int) {
	m.AuditQueueDepth.Set(float64(n))
}

// PgPoolStat is the subset of *pgxpool.Stat this package reads, declared
// locally so internal/metrics doesn't need to import pgx just for a
// sampling method's parameter type. *pgxpool.Stat satisfies this
// implicitly (Go structural typing doesn't even require an explicit
// interface on the pgxpool side) — main.go passes pool.Stat() directly.
type PgPoolStat interface {
	TotalConns() int32
	IdleConns() int32
	AcquiredConns() int32
	MaxConns() int32
	AcquireCount() int64
	AcquireDuration() time.Duration
	EmptyAcquireCount() int64
}

// SetPgPoolStats updates all pgxpool gauges from a sampled pool.Stat()
// call. Called periodically (see main.go), same pattern as
// SetCacheEntries and SetAuditQueueDepth.
func (m *Metrics) SetPgPoolStats(s PgPoolStat) {
	m.PgPoolConnsTotal.Set(float64(s.TotalConns()))
	m.PgPoolConnsIdle.Set(float64(s.IdleConns()))
	m.PgPoolConnsAcquired.Set(float64(s.AcquiredConns()))
	m.PgPoolConnsMax.Set(float64(s.MaxConns()))
	m.PgPoolAcquireCountTotal.Set(float64(s.AcquireCount()))
	m.PgPoolAcquireDurationSecs.Set(s.AcquireDuration().Seconds())
	m.PgPoolEmptyAcquireTotal.Set(float64(s.EmptyAcquireCount()))
}
