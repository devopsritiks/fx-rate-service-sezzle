package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rsharma41/sezzle-fx-service/internal/audit"
	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/ratescache"
	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/resilience"
	"github.com/rsharma41/sezzle-fx-service/internal/money"
)

// currencyCodeRe enforces ISO-4217-shaped input: exactly 3 uppercase
// letters. We don't validate against the real ISO-4217 list here — that
// list changes over time and Frankfurter is the authority on which
// currencies it actually supports, so we let the vendor's own 404 tell us
// "unknown currency" rather than maintaining a duplicate list that can
// drift out of sync.
var currencyCodeRe = regexp.MustCompile(`^[A-Z]{3}$`)

func isValidCurrencyCode(code string) bool {
	return currencyCodeRe.MatchString(code)
}

// RatesHandler handles GET /v1/rates/{base}?symbols=USD,CAD
type RatesHandler struct {
	Vendor VendorClient
	Logger *slog.Logger
	Audit  AuditEnqueuer
}

func (h *RatesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := audit.Record{
		RequestID:  requestIDFromContext(r.Context()),
		OccurredAt: start,
		Endpoint:   "/v1/rates/{base}",
	}
	defer func() { enqueueAudit(h.Audit, rec, start) }()

	base := strings.ToUpper(strings.TrimPrefix(r.URL.Path, "/v1/rates/"))
	rec.BaseCurrency = base
	if !isValidCurrencyCode(base) {
		rec.HTTPStatus, rec.ErrorCode = http.StatusBadRequest, codeInvalidCurrency
		writeError(w, http.StatusBadRequest, codeInvalidCurrency, "base currency must be a 3-letter ISO code, e.g. USD")
		return
	}

	var symbols []string
	if raw := r.URL.Query().Get("symbols"); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.ToUpper(strings.TrimSpace(s))
			if !isValidCurrencyCode(s) {
				rec.HTTPStatus, rec.ErrorCode = http.StatusBadRequest, codeInvalidCurrency
				writeError(w, http.StatusBadRequest, codeInvalidCurrency, "symbols must be 3-letter ISO codes, e.g. USD,CAD")
				return
			}
			symbols = append(symbols, s)
		}
	}

	result, err := h.Vendor.Latest(r.Context(), base, symbols, retryLogHooks(h.Logger, r))
	if err != nil {
		rec.HTTPStatus, rec.ErrorCode = vendorErrorStatusAndCode(err)
		handleVendorError(w, h.Logger, r, err)
		return
	}

	rec.CacheStatus = cacheStatusLabel(result)
	rec.Stale = result.Stale
	rec.HTTPStatus = http.StatusOK

	writeCacheHeaders(w, result)
	writeJSON(w, http.StatusOK, map[string]any{
		"base":        result.Base,
		"date":        result.RatesDate,
		"rates":       result.Rates,
		"stale":       result.Stale,
		"age_seconds": int(result.Age.Seconds()),
	})
}

// ConvertHandler handles GET /v1/convert?from=USD&to=CAD&amount=100.00
type ConvertHandler struct {
	Vendor VendorClient
	Logger *slog.Logger
	Audit  AuditEnqueuer
}

func (h *ConvertHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := audit.Record{
		RequestID:  requestIDFromContext(r.Context()),
		OccurredAt: start,
		Endpoint:   "/v1/convert",
	}
	defer func() { enqueueAudit(h.Audit, rec, start) }()

	from := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("from")))
	to := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("to")))
	amountStr := r.URL.Query().Get("amount")
	rec.FromCurrency, rec.ToCurrency = from, to

	if !isValidCurrencyCode(from) || !isValidCurrencyCode(to) {
		rec.HTTPStatus, rec.ErrorCode = http.StatusBadRequest, codeInvalidCurrency
		writeError(w, http.StatusBadRequest, codeInvalidCurrency, "from and to must be 3-letter ISO codes, e.g. USD")
		return
	}

	amount, err := money.ParseAmount(amountStr)
	if err != nil {
		rec.HTTPStatus, rec.ErrorCode = http.StatusBadRequest, codeInvalidAmount
		writeError(w, http.StatusBadRequest, codeInvalidAmount, err.Error())
		return
	}
	rec.Amount = amount.String()

	// We only ever cache (and ask the vendor for) the RATE, never the
	// converted amount: the cache key has no amount in it, so one cached
	// rates entry serves every amount a caller asks to convert. The
	// decimal math happens locally, on every request, against whatever
	// rate (fresh or stale) the cache layer hands back.
	result, err := h.Vendor.Latest(r.Context(), from, []string{to}, retryLogHooks(h.Logger, r))
	if err != nil {
		rec.HTTPStatus, rec.ErrorCode = vendorErrorStatusAndCode(err)
		handleVendorError(w, h.Logger, r, err)
		return
	}

	rate, ok := result.Rates[to]
	if !ok {
		rec.HTTPStatus, rec.ErrorCode = http.StatusBadGateway, codeUpstreamUnavailable
		writeError(w, http.StatusBadGateway, codeUpstreamUnavailable, "exchange rate currently unavailable, please retry")
		return
	}

	converted := money.Convert(amount, rate)

	rec.Rate = rate.String()
	rec.ConvertedAmount = converted.String()
	rec.CacheStatus = cacheStatusLabel(result)
	rec.Stale = result.Stale
	rec.HTTPStatus = http.StatusOK

	writeCacheHeaders(w, result)
	writeJSON(w, http.StatusOK, map[string]any{
		"from":        from,
		"to":          to,
		"amount":      amount.String(),
		"rate":        rate.String(),
		"converted":   converted.String(),
		"date":        result.RatesDate,
		"stale":       result.Stale,
		"age_seconds": int(result.Age.Seconds()),
	})
}

// cacheStatusLabel maps a ratescache.Result to the same HIT/MISS/STALE
// string used in the X-Cache response header, so the audit log and what
// a caller actually saw agree exactly.
func cacheStatusLabel(result ratescache.Result) string {
	switch {
	case result.Stale:
		return "STALE"
	case result.CacheHit:
		return "HIT"
	default:
		return "MISS"
	}
}

// vendorErrorStatusAndCode mirrors handleVendorError's status/code
// mapping, so the audit record's HTTPStatus/ErrorCode always match what
// the caller actually received without duplicating the response body
// itself into the audit log.
func vendorErrorStatusAndCode(err error) (int, string) {
	switch {
	case errors.Is(err, ratescache.ErrCurrencyNotFound):
		return http.StatusNotFound, codeNotFound
	case errors.Is(err, ratescache.ErrStaleDataUnavailable):
		return http.StatusServiceUnavailable, codeStaleDataUnavailable
	case errors.Is(err, resilience.ErrBreakerOpen):
		return http.StatusServiceUnavailable, codeCircuitOpen
	default:
		return http.StatusBadGateway, codeUpstreamUnavailable
	}
}

// enqueueAudit builds the final latency and fires the audit record,
// fire-and-forget. audit may be nil (e.g. in handler tests, or if
// AUDIT_QUEUE_SIZE/Postgres wiring is disabled) — nil-safe, no-op.
func enqueueAudit(enqueuer AuditEnqueuer, rec audit.Record, start time.Time) {
	if enqueuer == nil {
		return
	}
	rec.LatencyMS = time.Since(start).Milliseconds()
	enqueuer.Enqueue(rec)
}

// writeCacheHeaders sets X-Cache and Cache-Control based on the cache
// layer's result, so a caller (or a CDN/gateway in front of us) can see
// at a glance whether this response came straight from the vendor, from
// a fresh cache entry, or from a stale fallback — and knows how long it's
// safe to cache this response itself.
//
// X-Cache: MISS means this response required a vendor call (cache was
// cold or past its fresh TTL and the vendor answered). HIT means served
// directly from a fresh cache entry, no vendor call. STALE means the
// vendor call failed and we fell back to an aging-but-still-within-
// max-stale-age entry — callers MUST check the "stale" field in the body
// too, since that's the one guaranteed to survive being proxied through
// something that drops headers.
func writeCacheHeaders(w http.ResponseWriter, result ratescache.Result) {
	switch {
	case result.Stale:
		w.Header().Set("X-Cache", "STALE")
		// Stale data is already degraded; don't let anything downstream
		// cache it further and potentially extend how stale a client's own
		// view becomes.
		w.Header().Set("Cache-Control", "no-store")
	case result.CacheHit:
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(result.Age.Seconds())))
	default:
		w.Header().Set("X-Cache", "MISS")
		w.Header().Set("Cache-Control", "public, max-age=0")
	}
}

// retryLogHooks builds resilience.RetryHooks that log each retry attempt
// with the request ID, so a slow/erroring vendor call is traceable in our
// logs without needing metrics yet. A later observability phase adds a
// second set of hooks (or composes with these) to increment Prometheus
// counters here instead of changing handler code.
func retryLogHooks(logger *slog.Logger, r *http.Request) resilience.RetryHooks {
	reqID := requestIDFromContext(r.Context())
	return resilience.RetryHooks{
		OnRetry: func(attempt int, err error, delay time.Duration) {
			logger.Warn("vendor_call_retry",
				"request_id", reqID,
				"attempt", attempt,
				"error", err.Error(),
				"delay_ms", delay.Milliseconds(),
			)
		},
	}
}

// handleVendorError maps a vendor/resilience/cache-layer error to a safe,
// uniform API response. The real error is logged with the request ID for
// our own debugging; the caller only ever sees a generic message, never
// the vendor's raw error text or our internal details.
func handleVendorError(w http.ResponseWriter, logger *slog.Logger, r *http.Request, err error) {
	reqID := requestIDFromContext(r.Context())

	if errors.Is(err, ratescache.ErrCurrencyNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, "currency not found")
		return
	}

	if errors.Is(err, ratescache.ErrStaleDataUnavailable) {
		// We have a cached entry, but it's older than we're willing to
		// serve, and the vendor call to refresh it failed. This is the
		// fintech-correctness line from the package doc: silently serving
		// a 2-day-old FX rate on a payment screen is a real risk, so we
		// fail loudly instead.
		logger.Warn("stale_data_exceeds_max_age",
			"request_id", reqID,
		)
		writeError(w, http.StatusServiceUnavailable, codeStaleDataUnavailable, "exchange rate data is too old to serve and the provider is currently unavailable")
		return
	}

	if errors.Is(err, resilience.ErrBreakerOpen) {
		// Circuit is open and we had no cached fallback at all (cold
		// cache). We deliberately did not call the vendor. 503 signals "we
		// know we're degraded right now, try again later" rather than 502
		// (which implies we tried and the vendor failed).
		logger.Warn("request_rejected_breaker_open",
			"request_id", reqID,
		)
		writeError(w, http.StatusServiceUnavailable, codeCircuitOpen, "exchange rate provider is temporarily unavailable, please retry shortly")
		return
	}

	logger.Error("vendor_call_failed",
		"request_id", reqID,
		"error", err.Error(),
	)
	writeError(w, http.StatusBadGateway, codeUpstreamUnavailable, "exchange rate provider is currently unavailable, please retry")
}

// HealthHandler is the liveness probe: if the process can respond at all,
// it's alive. No upstream dependency check here on purpose — liveness
// should only fail when the process itself is broken, not when a
// downstream vendor is having a bad day (that's what readiness is for).
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ReadyHandler is the readiness probe.
//
// Deliberately: this always returns 200, even when the circuit breaker is
// open and even now that stale-serving exists. The body reports breaker
// state and cache stats for humans and dashboards, but it is not used as
// a scheduling signal to pull the pod out of rotation.
//
// Why this is unchanged from phase 2, and if anything more clearly
// correct now: Kubernetes readiness is a per-pod signal. If every pod's
// readiness depended on a shared external vendor being reachable, the
// moment Frankfurter has a bad few minutes, every pod fails readiness at
// once and the whole Service goes to zero available pods — "one vendor
// is slow" becomes "our entire fleet is down," entirely self-inflicted.
// Now that caching exists, a pod with the breaker open can keep serving
// correct, clearly-labeled stale rates (X-Cache: STALE) right up to
// MaxStaleAge — so "vendor unreachable" even more clearly does not mean
// "this pod has nothing useful to do." That's the whole point of this
// phase, and readiness staying green is what lets it actually help: if
// readiness failed the instant the breaker opened, k8s would pull the
// pod before it ever got to serve the stale data we just built.
//
// One edge case worth naming rather than silently handling: a pod that
// is cache-cold (e.g. just started) AND has its breaker open AND has
// nothing to serve at all is arguably "truly not ready." We don't fail
// readiness for this today, on purpose — the obvious way to detect it
// (zero cache entries) is exactly the state every pod is in right after
// a synchronized fleet restart, which would reintroduce the same
// thundering-herd-of-pod-evictions problem this design avoids elsewhere.
// We'd rather this show up as a loud structured log and a future
// Prometheus alert ("zero servable entries while breaker open") than as
// readiness-driven pod eviction logic we might get wrong under exactly
// the conditions it's meant to protect against.
//
// The database follows this exact same reasoning, added this phase: a
// Postgres outage degrades us to "not writing audit records right now"
// (see internal/audit's design), not "can't serve FX rates" — the two
// are entirely unrelated capabilities. Failing readiness because the
// audit sink is down would pull healthy, rate-serving pods out of
// rotation over a concern that has nothing to do with what they're
// actually there to do. So /ready reports DB reachability in the body,
// same as breaker state, and never lets it affect the status code.
type ReadyHandler struct {
	Vendor VendorClient
	DB     DBStatus
}

func (h *ReadyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	breakerState := "unknown"
	var entries int
	var oldestAge time.Duration
	if h.Vendor != nil {
		breakerState = h.Vendor.BreakerState()
		entries, oldestAge = h.Vendor.CacheStats()
	}

	dbStatus := "disabled"
	if h.DB != nil {
		dbStatus = "up"
		if err := h.DB.Ping(r.Context()); err != nil {
			dbStatus = "down"
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ready",
		"dependencies": map[string]any{
			"frankfurter": map[string]string{
				"circuit_breaker": breakerState,
			},
			"postgres": map[string]string{
				"status": dbStatus,
			},
		},
		"cache": map[string]any{
			"entries":              entries,
			"oldest_entry_age_sec": int(oldestAge.Seconds()),
		},
	})
}
