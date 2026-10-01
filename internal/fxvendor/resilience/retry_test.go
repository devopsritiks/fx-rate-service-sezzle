package resilience

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/frankfurter"
)

func testRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:    3,
		AttemptTimeout: 200 * time.Millisecond,
		OverallBudget:  2 * time.Second,
		BaseDelay:      5 * time.Millisecond,
		MaxDelay:       20 * time.Millisecond,
	}
}

// newTestVendor starts an httptest server and a frankfurter.Client
// pointed at it, so tests exercise the real HTTP path rather than a fake
// in-process function.
func newTestVendor(t *testing.T, handler http.HandlerFunc) *frankfurter.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return frankfurter.New(srv.URL, frankfurter.Options{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
		MaxResponseBytes:    1 << 20,
	})
}

func TestRetry_SucceedsAfterTransientFailures(t *testing.T) {
	var calls int32
	vendor := newTestVendor(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"amount":1,"base":"USD","date":"2024-01-01","rates":{"CAD":1.35}}`))
	})

	var attempts, retries int32
	hooks := RetryHooks{
		OnAttempt: func(attempt int) { atomic.AddInt32(&attempts, 1) },
		OnRetry:   func(attempt int, err error, delay time.Duration) { atomic.AddInt32(&retries, 1) },
	}

	result, err := withRetry(context.Background(), testRetryConfig(), hooks, func(ctx context.Context) (*frankfurter.LatestRates, error) {
		return vendor.Latest(ctx, "USD", []string{"CAD"})
	})

	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if result.Base != "USD" {
		t.Errorf("unexpected result: %+v", result)
	}
	if calls != 3 {
		t.Errorf("expected vendor to be called 3 times, got %d", calls)
	}
	if attempts != 3 {
		t.Errorf("expected 3 OnAttempt calls, got %d", attempts)
	}
	if retries != 2 {
		t.Errorf("expected 2 OnRetry calls, got %d", retries)
	}
}

func TestRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	vendor := newTestVendor(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, err := withRetry(context.Background(), testRetryConfig(), RetryHooks{}, func(ctx context.Context) (*frankfurter.LatestRates, error) {
		return vendor.Latest(ctx, "USD", []string{"CAD"})
	})

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 3 {
		t.Errorf("expected exactly MaxAttempts (3) calls, got %d", calls)
	}
}

func TestRetry_DoesNotRetry4xx(t *testing.T) {
	var calls int32
	vendor := newTestVendor(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	})

	_, err := withRetry(context.Background(), testRetryConfig(), RetryHooks{}, func(ctx context.Context) (*frankfurter.LatestRates, error) {
		return vendor.Latest(ctx, "USD", []string{"CAD"})
	})

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 call (no retry on 4xx), got %d", calls)
	}
}

func TestRetry_RetriesOnTimeout(t *testing.T) {
	var calls int32
	vendor := newTestVendor(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 {
			time.Sleep(500 * time.Millisecond) // longer than AttemptTimeout
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"amount":1,"base":"USD","date":"2024-01-01","rates":{"CAD":1.35}}`))
	})

	cfg := testRetryConfig()
	cfg.AttemptTimeout = 100 * time.Millisecond
	cfg.OverallBudget = 3 * time.Second

	result, err := withRetry(context.Background(), cfg, RetryHooks{}, func(ctx context.Context) (*frankfurter.LatestRates, error) {
		return vendor.Latest(ctx, "USD", []string{"CAD"})
	})

	if err != nil {
		t.Fatalf("expected eventual success, got error: %v", err)
	}
	if result == nil || result.Base != "USD" {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestRetry_HonorsRetryAfterHeader(t *testing.T) {
	var calls int32
	var firstCallTime, secondCallTime time.Time
	vendor := newTestVendor(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			firstCallTime = time.Now()
			w.Header().Set("Retry-After", "1") // 1 second
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		secondCallTime = time.Now()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"amount":1,"base":"USD","date":"2024-01-01","rates":{"CAD":1.35}}`))
	})

	cfg := testRetryConfig()
	cfg.BaseDelay = 5 * time.Millisecond // much shorter than Retry-After, to prove we used the header
	cfg.OverallBudget = 3 * time.Second

	_, err := withRetry(context.Background(), cfg, RetryHooks{}, func(ctx context.Context) (*frankfurter.LatestRates, error) {
		return vendor.Latest(ctx, "USD", []string{"CAD"})
	})

	if err != nil {
		t.Fatalf("expected eventual success, got error: %v", err)
	}
	gap := secondCallTime.Sub(firstCallTime)
	if gap < 900*time.Millisecond {
		t.Errorf("expected retry to wait ~1s honoring Retry-After, waited %v", gap)
	}
}

func TestRetry_StopsImmediatelyOnContextCancel(t *testing.T) {
	var calls int32
	vendor := newTestVendor(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled, simulating a client that already disconnected

	start := time.Now()
	_, err := withRetry(ctx, testRetryConfig(), RetryHooks{}, func(attemptCtx context.Context) (*frankfurter.LatestRates, error) {
		return vendor.Latest(attemptCtx, "USD", []string{"CAD"})
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error due to canceled context")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("expected near-immediate return on canceled context, took %v", elapsed)
	}
	if calls > 1 {
		t.Errorf("expected at most 1 call on pre-canceled context, got %d", calls)
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"transport error", &frankfurter.TransportError{Err: errors.New("dial tcp: connection refused")}, true},
		{"500", &frankfurter.UpstreamStatusError{StatusCode: 500}, true},
		{"502", &frankfurter.UpstreamStatusError{StatusCode: 502}, true},
		{"429", &frankfurter.UpstreamStatusError{StatusCode: 429}, true},
		{"400", &frankfurter.UpstreamStatusError{StatusCode: 400}, false},
		{"404", &frankfurter.UpstreamStatusError{StatusCode: 404}, false},
		{"other error", errors.New("decode failure"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err); got != tc.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
