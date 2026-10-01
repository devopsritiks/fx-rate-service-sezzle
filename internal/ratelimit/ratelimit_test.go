package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestLimiter_AllowsUpToBurstThenRejects(t *testing.T) {
	limiter := New(1, 3, Hooks{}) // 1 rps, burst of 3
	defer limiter.Stop()

	handler := limiter.Middleware(func(w http.ResponseWriter, retryAfter time.Duration) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})(newTestHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/convert?from=USD&to=CAD&amount=1", nil)
	req.RemoteAddr = "203.0.113.5:1234"

	var statuses []int
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		statuses = append(statuses, rec.Code)
	}

	// First 3 (the burst) should pass, the rest should be rate limited.
	for i := 0; i < 3; i++ {
		if statuses[i] != http.StatusOK {
			t.Errorf("request %d: expected 200 within burst, got %d", i, statuses[i])
		}
	}
	for i := 3; i < 5; i++ {
		if statuses[i] != http.StatusTooManyRequests {
			t.Errorf("request %d: expected 429 beyond burst, got %d", i, statuses[i])
		}
	}
}

func TestLimiter_TracksPerIPIndependently(t *testing.T) {
	limiter := New(1, 1, Hooks{}) // burst of exactly 1
	defer limiter.Stop()

	handler := limiter.Middleware(func(w http.ResponseWriter, retryAfter time.Duration) {
		w.WriteHeader(http.StatusTooManyRequests)
	})(newTestHandler())

	reqA := httptest.NewRequest(http.MethodGet, "/v1/convert", nil)
	reqA.RemoteAddr = "203.0.113.1:1111"
	reqB := httptest.NewRequest(http.MethodGet, "/v1/convert", nil)
	reqB.RemoteAddr = "203.0.113.2:2222"

	recA1 := httptest.NewRecorder()
	handler.ServeHTTP(recA1, reqA)
	recA2 := httptest.NewRecorder()
	handler.ServeHTTP(recA2, reqA)

	recB1 := httptest.NewRecorder()
	handler.ServeHTTP(recB1, reqB)

	if recA1.Code != http.StatusOK {
		t.Errorf("IP A first request: expected 200, got %d", recA1.Code)
	}
	if recA2.Code != http.StatusTooManyRequests {
		t.Errorf("IP A second request: expected 429 (burst exhausted), got %d", recA2.Code)
	}
	if recB1.Code != http.StatusOK {
		t.Errorf("IP B first request: expected 200 (independent bucket), got %d", recB1.Code)
	}
}

func TestLimiter_SkipsHealthReadyMetrics(t *testing.T) {
	limiter := New(1, 1, Hooks{})
	defer limiter.Stop()

	handler := limiter.Middleware(func(w http.ResponseWriter, retryAfter time.Duration) {
		w.WriteHeader(http.StatusTooManyRequests)
	})(newTestHandler())

	for _, path := range []string{"/health", "/ready", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "203.0.113.9:1234"
		for i := 0; i < 5; i++ {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("%s request %d: expected 200 (should bypass rate limiting), got %d", path, i, rec.Code)
			}
		}
	}
}

func TestLimiter_ResponseIncludesRetryAfter(t *testing.T) {
	limiter := New(1, 1, Hooks{})
	defer limiter.Stop()

	handler := limiter.Middleware(func(w http.ResponseWriter, retryAfter time.Duration) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})(newTestHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/convert", nil)
	req.RemoteAddr = "203.0.113.7:1234"

	handler.ServeHTTP(httptest.NewRecorder(), req) // consume the burst
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429 response")
	}
}

func (l *Limiter) entryCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func TestLimiter_SweepEvictsStaleEntries(t *testing.T) {
	limiter := newWithTTL(1, 1, 50*time.Millisecond, Hooks{})
	defer limiter.Stop()

	limiter.allow("203.0.113.100")
	if n := limiter.entryCount(); n != 1 {
		t.Fatalf("expected 1 entry after first call, got %d", n)
	}

	time.Sleep(100 * time.Millisecond)
	limiter.sweep()

	if n := limiter.entryCount(); n != 0 {
		t.Errorf("expected stale entry to be evicted, map still has %d entries", n)
	}
}
