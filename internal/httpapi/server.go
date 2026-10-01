// Package httpapi wires up the HTTP transport: routes, middleware chain,
// and the http.Server itself with its timeouts. Business logic for each
// endpoint lives in handlers.go; this file is just wiring.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rsharma41/sezzle-fx-service/internal/audit"
	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/ratescache"
	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/resilience"
	"github.com/rsharma41/sezzle-fx-service/internal/metrics"
	"github.com/rsharma41/sezzle-fx-service/internal/ratelimit"
	"github.com/rsharma41/sezzle-fx-service/internal/webui"
)

// VendorClient is the interface handlers depend on, satisfied by
// *ratescache.RatesCache. Defining it here (consumer side) rather than
// depending on the concrete type directly keeps the handlers' dependency
// explicit and narrow, and lets handler tests use a small fake instead
// of standing up the full cache/breaker/retry stack.
type VendorClient interface {
	Latest(ctx context.Context, base string, symbols []string, hooks resilience.RetryHooks) (ratescache.Result, error)
	BreakerState() string
	CacheStats() (entries int, oldestAge time.Duration)
}

// AuditEnqueuer is the interface handlers depend on to record an audit
// entry, satisfied by *audit.Writer. Defined consumer-side like
// VendorClient above, and — critically — Enqueue is expected to never
// block or error; handlers call it fire-and-forget after writing the
// response, so it can never affect the response itself.
type AuditEnqueuer interface {
	Enqueue(rec audit.Record)
}

// Deps holds everything handlers need. Keeping this as an explicit
// struct (rather than globals) makes it obvious what each handler
// depends on and keeps the door open for table-driven tests later.
// Metrics and Audit are optional (nil-safe) so existing handler tests
// that don't care about them don't need to construct one.
type Deps struct {
	Vendor  VendorClient
	Logger  *slog.Logger
	Metrics *metrics.Metrics
	Audit   AuditEnqueuer
	DB      DBStatus
}

// DBStatus is the interface /ready uses to report Postgres health in its
// body without failing readiness over it — same pattern and same
// reasoning as VendorClient.BreakerState() for the vendor. Optional
// (nil-safe): if Audit/DB are never wired (e.g. in a unit test), /ready
// just reports "disabled" rather than needing a fake.
type DBStatus interface {
	Ping(ctx context.Context) error
}

// ServerConfig holds the HTTP-transport-level settings NewServer needs:
// listen address, server timeouts, and inbound rate limit settings.
type ServerConfig struct {
	Addr           string
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	IdleTimeout    time.Duration
	RateLimitRPS   float64
	RateLimitBurst int
}

// NewServer builds the http.Server with routes, middleware, and the
// read/write/idle timeouts that protect us from slow-client and
// slow-vendor resource exhaustion.
func NewServer(cfg ServerConfig, deps Deps) *http.Server {
	mux := http.NewServeMux()

	mux.Handle("/v1/rates/", &RatesHandler{Vendor: deps.Vendor, Logger: deps.Logger, Audit: deps.Audit})
	mux.Handle("/v1/convert", &ConvertHandler{Vendor: deps.Vendor, Logger: deps.Logger, Audit: deps.Audit})
	mux.HandleFunc("/health", HealthHandler)
	mux.Handle("/ready", &ReadyHandler{Vendor: deps.Vendor, DB: deps.DB})
	mux.Handle("/metrics", promhttp.Handler())
	// Demo UI, served from the same origin as the API so its fetch()
	// calls to /v1/... never hit a cross-origin request at all — no CORS
	// configuration needed anywhere in this service.
	mux.Handle("/", webui.Handler())

	var rateLimitHooks ratelimit.Hooks
	if deps.Metrics != nil {
		rateLimitHooks.OnRejected = deps.Metrics.OnRateLimitRejected
	}
	limiter := ratelimit.New(cfg.RateLimitRPS, cfg.RateLimitBurst, rateLimitHooks)

	var handler http.Handler = mux
	handler = limiter.Middleware(writeTooManyRequests)(handler)
	handler = metricsMiddleware(deps.Metrics)(handler)
	handler = loggingMiddleware(deps.Logger)(handler)
	handler = requestIDMiddleware(handler)

	return &http.Server{
		Addr:         cfg.Addr,
		Handler:      handler,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}
}
