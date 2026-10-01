// Package config loads service configuration from environment variables.
// This is the only package that reads os.Getenv directly — every other
// package receives an already-parsed Config value.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime settings for the service.
type Config struct {
	// HTTP server
	Addr                string
	HTTPReadTimeout     time.Duration
	HTTPWriteTimeout    time.Duration
	HTTPIdleTimeout     time.Duration
	HTTPShutdownTimeout time.Duration

	// Upstream vendor (Frankfurter)
	FrankfurterBaseURL string
	UpstreamTimeout    time.Duration // per-attempt timeout

	// Retry
	RetryMaxAttempts int
	RetryBaseDelay   time.Duration
	RetryMaxDelay    time.Duration
	// CallBudget bounds the whole call (all attempts combined). Not an env
	// var on its own — derived as a multiple of UpstreamTimeout so it always
	// comfortably covers RetryMaxAttempts worth of per-attempt timeouts.
	CallBudget time.Duration

	// Circuit breaker
	BreakerFailureThreshold uint32
	BreakerOpenDuration     time.Duration

	// Bulkhead / vendor HTTP transport tuning
	VendorMaxConcurrentCalls  int
	VendorMaxIdleConns        int
	VendorMaxIdleConnsPerHost int
	VendorIdleConnTimeout     time.Duration
	VendorMaxResponseBytes    int64

	// Inbound rate limiting (per client IP)
	RateLimitRPS   float64
	RateLimitBurst int

	// Rates cache
	CacheFreshTTL    time.Duration
	CacheMaxStaleAge time.Duration
	CacheNegativeTTL time.Duration
	CacheMaxEntries  int

	// Postgres audit log
	PostgresDSN             string
	PostgresMaxConns        int32
	PostgresMinConns        int32
	PostgresMaxConnLifetime time.Duration
	PostgresMaxConnIdleTime time.Duration
	PostgresConnectTimeout  time.Duration

	AuditQueueSize            int
	AuditBatchSize            int
	AuditFlushInterval        time.Duration
	AuditRetryBaseDelay       time.Duration
	AuditRetryMaxDelay        time.Duration
	AuditShutdownFlushTimeout time.Duration

	// Observability
	LogLevel    string
	ServiceName string
}

// Load reads configuration from environment variables, falling back to
// sane defaults for anything unset. It returns an error if a value is
// present but cannot be parsed (e.g. a malformed duration).
func Load() (Config, error) {
	cfg := Config{
		Addr:               getString("ADDR", ":8080"),
		FrankfurterBaseURL: getString("FRANKFURTER_BASE_URL", "https://api.frankfurter.dev/v1"),
		LogLevel:           getString("LOG_LEVEL", "info"),
		ServiceName:        getString("SERVICE_NAME", "sezzle-fx-service"),
	}

	var err error
	if cfg.HTTPReadTimeout, err = getDuration("HTTP_READ_TIMEOUT", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.HTTPWriteTimeout, err = getDuration("HTTP_WRITE_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.HTTPIdleTimeout, err = getDuration("HTTP_IDLE_TIMEOUT", 120*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.HTTPShutdownTimeout, err = getDuration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.UpstreamTimeout, err = getDuration("UPSTREAM_TIMEOUT", 4*time.Second); err != nil {
		return Config{}, err
	}

	if cfg.RetryMaxAttempts, err = getInt("RETRY_MAX_ATTEMPTS", 3); err != nil {
		return Config{}, err
	}
	if cfg.RetryBaseDelay, err = getDuration("RETRY_BASE_DELAY", 100*time.Millisecond); err != nil {
		return Config{}, err
	}
	if cfg.RetryMaxDelay, err = getDuration("RETRY_MAX_DELAY", 2*time.Second); err != nil {
		return Config{}, err
	}
	// Overall budget: enough for every attempt to use its full per-attempt
	// timeout back to back, plus headroom for backoff sleeps between them.
	// This is derived, not independently configured, so it can never be set
	// lower than the attempts it's supposed to cover.
	cfg.CallBudget = time.Duration(cfg.RetryMaxAttempts)*cfg.UpstreamTimeout + time.Duration(cfg.RetryMaxAttempts)*cfg.RetryMaxDelay

	if threshold, err := getInt("BREAKER_FAILURE_THRESHOLD", 5); err != nil {
		return Config{}, err
	} else {
		cfg.BreakerFailureThreshold = uint32(threshold)
	}
	if cfg.BreakerOpenDuration, err = getDuration("BREAKER_OPEN_DURATION", 30*time.Second); err != nil {
		return Config{}, err
	}

	if cfg.VendorMaxConcurrentCalls, err = getInt("VENDOR_MAX_CONCURRENT_CALLS", 20); err != nil {
		return Config{}, err
	}
	if cfg.VendorMaxIdleConns, err = getInt("VENDOR_MAX_IDLE_CONNS", 50); err != nil {
		return Config{}, err
	}
	if cfg.VendorMaxIdleConnsPerHost, err = getInt("VENDOR_MAX_IDLE_CONNS_PER_HOST", 10); err != nil {
		return Config{}, err
	}
	if cfg.VendorIdleConnTimeout, err = getDuration("VENDOR_IDLE_CONN_TIMEOUT", 90*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.VendorMaxResponseBytes, err = getInt64("VENDOR_MAX_RESPONSE_BYTES", 1<<20); err != nil {
		return Config{}, err
	}

	if cfg.RateLimitRPS, err = getFloat("RATE_LIMIT_RPS", 10); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitBurst, err = getInt("RATE_LIMIT_BURST", 20); err != nil {
		return Config{}, err
	}

	// Frankfurter (ECB) publishes one new rate per TARGET business day and
	// nothing changes over weekends/holidays, so these defaults are chosen
	// against that cadence, not against typical "web API" freshness:
	//   - 1h fresh TTL: no benefit to polling faster than this since the
	//     rate won't have moved, but short enough that an intra-day vendor
	//     correction or a restart doesn't pin us to very old "fresh" data.
	//   - 48h max stale age: covers a normal weekend (serving Friday's
	//     close through Sat/Sun is correct, not degraded) plus one full
	//     extra business day of vendor-outage buffer. Beyond that, treating
	//     a 2-day-old FX rate as current is a real correctness risk for a
	//     payment checkout, so we fail loudly instead.
	if cfg.CacheFreshTTL, err = getDuration("CACHE_FRESH_TTL", time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.CacheMaxStaleAge, err = getDuration("CACHE_STALE_MAX_AGE", 48*time.Hour); err != nil {
		return Config{}, err
	}
	// Negative cache (e.g. vendor 404 for an unsupported currency) is kept
	// short and independent of the fresh TTL: it's a different kind of
	// fact than a successful rate, and a currency Frankfurter adds support
	// for later shouldn't stay blocked for as long as a normal rate TTL.
	if cfg.CacheNegativeTTL, err = getDuration("CACHE_NEGATIVE_TTL", 60*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.CacheMaxEntries, err = getInt("CACHE_MAX_ENTRIES", 2000); err != nil {
		return Config{}, err
	}

	// Postgres connection. DSN has a working local default (matching the
	// docker-compose postgres service) so `make run` works out of the box
	// against a locally-running Postgres, but nothing here is hardcoded in
	// the sense of "baked into the binary regardless of env" — every one
	// of these is read from an env var, this is just its default value.
	cfg.PostgresDSN = getString("POSTGRES_DSN", "postgres://fx:fx@localhost:5432/fx?sslmode=disable")
	if maxConns, err := getInt("POSTGRES_MAX_CONNS", 10); err != nil {
		return Config{}, err
	} else {
		cfg.PostgresMaxConns = int32(maxConns)
	}
	if minConns, err := getInt("POSTGRES_MIN_CONNS", 2); err != nil {
		return Config{}, err
	} else {
		cfg.PostgresMinConns = int32(minConns)
	}
	if cfg.PostgresMaxConnLifetime, err = getDuration("POSTGRES_MAX_CONN_LIFETIME", time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.PostgresMaxConnIdleTime, err = getDuration("POSTGRES_MAX_CONN_IDLE_TIME", 30*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.PostgresConnectTimeout, err = getDuration("POSTGRES_CONNECT_TIMEOUT", 5*time.Second); err != nil {
		return Config{}, err
	}

	// Audit log async writer. See internal/audit's package doc for the
	// full design; in short: QueueSize bounds memory and is the
	// drop-vs-keep-accepting-requests tradeoff point, BatchSize/FlushInterval
	// control write efficiency vs. staleness of what's durably recorded.
	if cfg.AuditQueueSize, err = getInt("AUDIT_QUEUE_SIZE", 10000); err != nil {
		return Config{}, err
	}
	if cfg.AuditBatchSize, err = getInt("AUDIT_BATCH_SIZE", 100); err != nil {
		return Config{}, err
	}
	if cfg.AuditFlushInterval, err = getDuration("AUDIT_FLUSH_INTERVAL", 2*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.AuditRetryBaseDelay, err = getDuration("AUDIT_RETRY_BASE_DELAY", 200*time.Millisecond); err != nil {
		return Config{}, err
	}
	if cfg.AuditRetryMaxDelay, err = getDuration("AUDIT_RETRY_MAX_DELAY", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.AuditShutdownFlushTimeout, err = getDuration("AUDIT_SHUTDOWN_FLUSH_TIMEOUT", 5*time.Second); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func getString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration for %s=%q: %w", key, v, err)
	}
	return d, nil
}

func getInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer for %s=%q: %w", key, v, err)
	}
	return n, nil
}

func getInt64(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid integer for %s=%q: %w", key, v, err)
	}
	return n, nil
}

func getFloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid float for %s=%q: %w", key, v, err)
	}
	return f, nil
}
