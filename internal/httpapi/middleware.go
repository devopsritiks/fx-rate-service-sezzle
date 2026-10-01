package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rsharma41/sezzle-fx-service/internal/metrics"
)

type contextKey string

const requestIDKey contextKey = "request_id"

// requestIDMiddleware assigns a request ID (reusing an inbound X-Request-Id
// if the caller supplied one, e.g. propagated from an upstream gateway),
// stores it on the request context, and echoes it back in the response
// header so a caller can correlate their request with our logs.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// loggingMiddleware emits one structured log line per request with
// status code and latency — the minimum needed to debug "is the service
// slow/erroring" without a dashboard in front of you.
func loggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(sw, r)

			logger.Info("http_request",
				"request_id", requestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// statusRecorder captures the status code written by downstream handlers
// so the logging middleware can report it after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// routePattern maps a request path to a small, fixed set of route
// labels for metrics. This is deliberately NOT the raw r.URL.Path: the
// raw path for /v1/rates/{base} has one distinct value per currency code
// a caller asks for, and a cardinality-unbounded label is exactly what
// blows up a Prometheus instance's memory and query latency over time —
// every unique label combination becomes its own permanently-stored time
// series. Matching against the same small set of patterns our
// http.ServeMux is registered with keeps the route label's cardinality
// fixed regardless of how many distinct currencies or paths callers hit.
func routePattern(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/rates/"):
		return "/v1/rates/{base}"
	case path == "/v1/convert":
		return "/v1/convert"
	case path == "/health":
		return "/health"
	case path == "/ready":
		return "/ready"
	case path == "/metrics":
		return "/metrics"
	default:
		// Anything else (404s, typos, probing) collapses into one bucket
		// rather than letting an arbitrary caller-supplied path become its
		// own label value.
		return "other"
	}
}

// metricsMiddleware records RED metrics (rate, errors via status class,
// duration) for every inbound request. m may be nil (e.g. in tests that
// don't care about metrics), in which case this is a no-op passthrough.
func metricsMiddleware(m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if m == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(sw, r)

			m.ObserveHTTPRequest(r.Method, routePattern(r.URL.Path), sw.status, time.Since(start))
		})
	}
}
