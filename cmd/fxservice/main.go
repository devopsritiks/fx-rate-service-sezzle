// Command fxservice runs the FX Rate Service HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rsharma41/sezzle-fx-service/internal/audit"
	"github.com/rsharma41/sezzle-fx-service/internal/cache"
	"github.com/rsharma41/sezzle-fx-service/internal/config"
	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/frankfurter"
	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/ratescache"
	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/resilience"
	"github.com/rsharma41/sezzle-fx-service/internal/httpapi"
	"github.com/rsharma41/sezzle-fx-service/internal/metrics"
)

// version is overridden at build time via -ldflags "-X main.version=...".
// See the Dockerfile and Makefile for how the build pipeline sets it;
// left as "dev" for a plain `go build`/`go run` so local development
// never has to think about it.
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal_startup_error", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel, cfg.ServiceName)
	slog.SetDefault(logger)

	m := metrics.New(prometheus.DefaultRegisterer, version)

	vendorClient := frankfurter.New(cfg.FrankfurterBaseURL, frankfurter.Options{
		MaxIdleConns:        cfg.VendorMaxIdleConns,
		MaxIdleConnsPerHost: cfg.VendorMaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.VendorIdleConnTimeout,
		MaxResponseBytes:    cfg.VendorMaxResponseBytes,
	})

	resilientClient := resilience.NewClient(vendorClient, resilience.Config{
		Retry: resilience.RetryConfig{
			MaxAttempts:    cfg.RetryMaxAttempts,
			AttemptTimeout: cfg.UpstreamTimeout,
			OverallBudget:  cfg.CallBudget,
			BaseDelay:      cfg.RetryBaseDelay,
			MaxDelay:       cfg.RetryMaxDelay,
		},
		BreakerFailureThreshold: cfg.BreakerFailureThreshold,
		BreakerOpenDuration:     cfg.BreakerOpenDuration,
		MaxConcurrentCalls:      cfg.VendorMaxConcurrentCalls,
	}, resilience.Hooks{
		OnBreakerStateChange: m.OnBreakerStateChange,
		OnCallComplete:       m.OnVendorCallComplete,
		OnBulkheadAcquire:    m.OnBulkheadAcquire,
		OnBulkheadRelease:    m.OnBulkheadRelease,
		Retry: resilience.RetryHooks{
			OnRetry: func(attempt int, retryErr error, delay time.Duration) {
				m.OnRetryAttempt()
			},
		},
	}, logger)

	store := cache.NewLRU(cfg.CacheMaxEntries, cache.Hooks{
		OnEvict: func(key string, reason cache.EvictReason) {
			logger.Info("cache_entry_evicted", "key", key, "reason", string(reason))
			m.OnCacheEvict(key, reason)
		},
	})

	ratesCache := ratescache.New(store, resilientClient, ratescache.Config{
		FreshTTL:    cfg.CacheFreshTTL,
		MaxStaleAge: cfg.CacheMaxStaleAge,
		NegativeTTL: cfg.CacheNegativeTTL,
	}, ratescache.Hooks{
		OnHit: func(key string) {
			logger.Debug("cache_hit", "key", key)
			m.OnCacheHit(key)
		},
		OnNegativeHit: func(key string) {
			logger.Debug("cache_negative_hit", "key", key)
			m.OnCacheNegativeHit(key)
		},
		OnMiss: func(key string) {
			logger.Debug("cache_miss", "key", key)
			m.OnCacheMiss(key)
		},
		OnStale: func(key string, age time.Duration) {
			logger.Warn("cache_serving_stale", "key", key, "age_seconds", int(age.Seconds()))
			m.OnCacheStale(key, age)
		},
		OnSingleflightShared: func(key string) {
			logger.Debug("vendor_call_deduped", "key", key)
			m.OnSingleflightShared(key)
		},
	})

	// Postgres pool + audit writer. pgxpool.NewWithConfig connects lazily
	// — it does NOT block or fail startup if Postgres is unreachable right
	// now, which is exactly what we want: this service's job is serving FX
	// rates, and an audit-log outage must never become an FX-service
	// outage. See internal/audit's package doc for the full async-writer
	// design and the drop-under-backpressure tradeoff.
	pgPool, err := audit.NewPool(context.Background(), audit.PoolConfig{
		DSN:             cfg.PostgresDSN,
		MaxConns:        cfg.PostgresMaxConns,
		MinConns:        cfg.PostgresMinConns,
		MaxConnLifetime: cfg.PostgresMaxConnLifetime,
		MaxConnIdleTime: cfg.PostgresMaxConnIdleTime,
		ConnectTimeout:  cfg.PostgresConnectTimeout,
	})
	if err != nil {
		// Only a malformed DSN reaches here (a config mistake, not a
		// down database) — pgxpool.NewWithConfig itself never dials.
		return err
	}
	defer pgPool.Close()

	// Run the schema migration in the background rather than blocking
	// startup on it: if Postgres is down right now, we still want
	// fxservice to come up and start serving FX rates immediately. The
	// migration (and every subsequent write) will simply keep failing
	// and retrying until Postgres is reachable — see the retry/backoff
	// in internal/audit.Writer for the write side; the migration itself
	// retries with the same backoff shape here.
	go migrateWithRetry(context.Background(), pgPool, logger)

	auditWriter := audit.New(audit.NewPgSink(pgPool), audit.Config{
		QueueSize:            cfg.AuditQueueSize,
		BatchSize:            cfg.AuditBatchSize,
		FlushInterval:        cfg.AuditFlushInterval,
		RetryBaseDelay:       cfg.AuditRetryBaseDelay,
		RetryMaxDelay:        cfg.AuditRetryMaxDelay,
		ShutdownFlushTimeout: cfg.AuditShutdownFlushTimeout,
	}, audit.Hooks{
		OnWritten:        m.OnAuditWritten,
		OnDropped:        m.OnAuditDropped,
		OnWriteError:     m.OnAuditWriteError,
		OnBatchWriteDone: m.OnAuditBatchWriteDone,
	}, logger)

	srv := httpapi.NewServer(httpapi.ServerConfig{
		Addr:           cfg.Addr,
		ReadTimeout:    cfg.HTTPReadTimeout,
		WriteTimeout:   cfg.HTTPWriteTimeout,
		IdleTimeout:    cfg.HTTPIdleTimeout,
		RateLimitRPS:   cfg.RateLimitRPS,
		RateLimitBurst: cfg.RateLimitBurst,
	}, httpapi.Deps{
		Vendor:  ratesCache,
		Logger:  logger,
		Metrics: m,
		Audit:   auditWriter,
		DB:      pgPool,
	})

	// Listen for SIGTERM (k8s pod termination) and SIGINT (ctrl-C locally)
	// so we can drain in-flight requests instead of dropping them.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Point-in-time gauges (cache size, audit queue depth, pgxpool stats)
	// are sampled on a timer rather than updated from every call site —
	// each is a fact about current state, not a discrete event.
	gaugeSamplerStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				entries, _ := ratesCache.CacheStats()
				m.SetCacheEntries(entries)
				m.SetAuditQueueDepth(auditWriter.Depth())
				m.SetPgPoolStats(pgPool.Stat())
			case <-gaugeSamplerStop:
				return
			}
		}
	}()
	defer close(gaugeSamplerStop)

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("server_starting", "addr", cfg.Addr, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("shutdown_signal_received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTPShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful_shutdown_failed", "error", err.Error())
		return err
	}

	// Flush the audit writer only after the HTTP server has fully
	// stopped accepting new requests — nothing can Enqueue after this
	// point, so draining here captures every record from every request
	// this process ever served, up to ShutdownFlushTimeout.
	logger.Info("flushing_audit_queue", "depth", auditWriter.Depth())
	auditWriter.Close()

	logger.Info("shutdown_complete")
	return nil
}

// migrateWithRetry applies the audit schema, retrying with backoff if
// Postgres isn't reachable yet. It runs in the background so a down
// database at startup never blocks or fails fxservice's own boot.
func migrateWithRetry(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) {
	delay := 500 * time.Millisecond
	const maxDelay = 30 * time.Second

	for {
		if err := audit.Migrate(ctx, pool); err != nil {
			logger.Warn("audit_schema_migration_failed_will_retry", "error", err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			if delay *= 2; delay > maxDelay {
				delay = maxDelay
			}
			continue
		}
		logger.Info("audit_schema_migration_applied")
		return
	}
}

func newLogger(level, serviceName string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler).With("service", serviceName)
}
