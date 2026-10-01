package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/ratescache"
	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/resilience"
	"github.com/shopspring/decimal"
)

func TestIsValidCurrencyCode(t *testing.T) {
	cases := []struct {
		code string
		want bool
	}{
		{"USD", true},
		{"CAD", true},
		{"usd", false},
		{"US", false},
		{"USDA", false},
		{"", false},
		{"12A", false},
	}

	for _, tc := range cases {
		if got := isValidCurrencyCode(tc.code); got != tc.want {
			t.Errorf("isValidCurrencyCode(%q) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

// fakeVendorClient is a test double for VendorClient so handler tests can
// control the exact error/result without standing up the full
// cache/retry/breaker/bulkhead stack.
type fakeVendorClient struct {
	result       ratescache.Result
	err          error
	breakerState string
	cacheEntries int
	oldestAge    time.Duration
}

func (f *fakeVendorClient) Latest(ctx context.Context, base string, symbols []string, hooks resilience.RetryHooks) (ratescache.Result, error) {
	return f.result, f.err
}

func (f *fakeVendorClient) BreakerState() string {
	return f.breakerState
}

func (f *fakeVendorClient) CacheStats() (int, time.Duration) {
	return f.cacheEntries, f.oldestAge
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestConvertHandler_BreakerOpenReturns503(t *testing.T) {
	h := &ConvertHandler{
		Vendor: &fakeVendorClient{err: resilience.ErrBreakerOpen},
		Logger: testLogger(),
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/convert?from=USD&to=CAD&amount=100.00", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when breaker open, got %d", rec.Code)
	}

	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body.Error.Code != codeCircuitOpen {
		t.Errorf("expected error code %q, got %q", codeCircuitOpen, body.Error.Code)
	}
}

func TestConvertHandler_CurrencyNotFoundReturns404(t *testing.T) {
	h := &ConvertHandler{
		Vendor: &fakeVendorClient{err: ratescache.ErrCurrencyNotFound},
		Logger: testLogger(),
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/convert?from=USD&to=CAD&amount=100.00", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestConvertHandler_StaleDataUnavailableReturns503(t *testing.T) {
	h := &ConvertHandler{
		Vendor: &fakeVendorClient{err: ratescache.ErrStaleDataUnavailable},
		Logger: testLogger(),
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/convert?from=USD&to=CAD&amount=100.00", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when cached data exceeds max stale age, got %d", rec.Code)
	}

	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body.Error.Code != codeStaleDataUnavailable {
		t.Errorf("expected error code %q, got %q", codeStaleDataUnavailable, body.Error.Code)
	}
}

func TestReadyHandler_ReturnsOKEvenWithBreakerOpen(t *testing.T) {
	h := &ReadyHandler{Vendor: &fakeVendorClient{breakerState: "open", cacheEntries: 3, oldestAge: 90 * time.Second}}

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// The whole point of this phase's /ready design: readiness must stay
	// 200 even when the breaker is open, so k8s doesn't pull every pod out
	// of rotation the moment the vendor has a bad few minutes.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected /ready to always return 200, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	deps, ok := body["dependencies"].(map[string]any)
	if !ok {
		t.Fatalf("expected dependencies field in /ready body, got %+v", body)
	}
	frank, ok := deps["frankfurter"].(map[string]any)
	if !ok {
		t.Fatalf("expected frankfurter field in dependencies, got %+v", deps)
	}
	if frank["circuit_breaker"] != "open" {
		t.Errorf("expected circuit_breaker state %q in /ready body, got %+v", "open", frank)
	}

	cacheInfo, ok := body["cache"].(map[string]any)
	if !ok {
		t.Fatalf("expected cache field in /ready body, got %+v", body)
	}
	if cacheInfo["entries"] != float64(3) {
		t.Errorf("expected cache.entries=3, got %+v", cacheInfo["entries"])
	}
	if cacheInfo["oldest_entry_age_sec"] != float64(90) {
		t.Errorf("expected cache.oldest_entry_age_sec=90, got %+v", cacheInfo["oldest_entry_age_sec"])
	}
}

func TestConvertHandler_StaleResponseSetsHeadersAndBody(t *testing.T) {
	h := &ConvertHandler{
		Vendor: &fakeVendorClient{result: ratescache.Result{
			Base:      "USD",
			RatesDate: "2024-01-12",
			Rates:     map[string]decimal.Decimal{"CAD": decimal.RequireFromString("1.35")},
			Stale:     true,
			Age:       3 * time.Hour,
		}},
		Logger: testLogger(),
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/convert?from=USD&to=CAD&amount=100.00", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for stale-but-within-bounds data, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Cache"); got != "STALE" {
		t.Errorf("expected X-Cache: STALE, got %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("expected Cache-Control: no-store for stale response, got %q", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body["stale"] != true {
		t.Errorf("expected stale:true in body, got %+v", body["stale"])
	}
	if body["age_seconds"] != float64(3*3600) {
		t.Errorf("expected age_seconds=10800, got %+v", body["age_seconds"])
	}
}
