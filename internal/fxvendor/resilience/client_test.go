package resilience

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
)

func testClientConfig() Config {
	return Config{
		Retry: RetryConfig{
			MaxAttempts:    1, // keep breaker tests simple: one call = one attempt
			AttemptTimeout: 200 * time.Millisecond,
			OverallBudget:  500 * time.Millisecond,
			BaseDelay:      5 * time.Millisecond,
			MaxDelay:       20 * time.Millisecond,
		},
		BreakerFailureThreshold: 4, // need >=4 requests before trip math kicks in
		BreakerOpenDuration:     150 * time.Millisecond,
		MaxConcurrentCalls:      10,
	}
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"amount":1,"base":"USD","date":"2024-01-01","rates":{"CAD":1.35}}`))
}

func failHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
}

func TestBreaker_OpensAfterFailureRatio(t *testing.T) {
	vendor := newTestVendor(t, failHandler)

	var transitions []string
	var mu sync.Mutex
	client := NewClient(vendor, testClientConfig(), Hooks{
		OnBreakerStateChange: func(from, to string) {
			mu.Lock()
			transitions = append(transitions, from+"->"+to)
			mu.Unlock()
		},
	}, nil)

	// Drive failures past the threshold; MaxAttempts=1 so each Latest call
	// is exactly one vendor call = one breaker data point.
	for i := 0; i < 5; i++ {
		_, err := client.Latest(context.Background(), "USD", []string{"CAD"}, RetryHooks{})
		if err == nil {
			t.Fatalf("call %d: expected error from failing vendor", i)
		}
	}

	if client.BreakerState() != "open" {
		t.Fatalf("expected breaker to be open after repeated failures, got %q", client.BreakerState())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(transitions) == 0 || transitions[len(transitions)-1] != "closed->open" {
		t.Errorf("expected a closed->open transition to be recorded, got %v", transitions)
	}
}

func TestBreaker_FailsFastWithoutCallingVendorWhenOpen(t *testing.T) {
	var vendorCalls int32
	vendor := newTestVendor(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&vendorCalls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	client := NewClient(vendor, testClientConfig(), Hooks{}, nil)

	// Trip the breaker.
	for i := 0; i < 5; i++ {
		_, _ = client.Latest(context.Background(), "USD", []string{"CAD"}, RetryHooks{})
	}
	if client.BreakerState() != "open" {
		t.Fatalf("precondition failed: breaker not open, state=%q", client.BreakerState())
	}

	callsBefore := atomic.LoadInt32(&vendorCalls)

	_, err := client.Latest(context.Background(), "USD", []string{"CAD"}, RetryHooks{})
	if !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("expected ErrBreakerOpen, got %v", err)
	}

	callsAfter := atomic.LoadInt32(&vendorCalls)
	if callsAfter != callsBefore {
		t.Errorf("expected no additional vendor call while breaker open, calls went from %d to %d", callsBefore, callsAfter)
	}
}

func TestBreaker_HalfOpenThenCloses(t *testing.T) {
	var healthy int32 // 0 = failing, 1 = healthy; flipped mid-test
	vendor := newTestVendor(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&healthy) == 1 {
			okHandler(w, r)
			return
		}
		failHandler(w, r)
	})

	cfg := testClientConfig()
	client := NewClient(vendor, cfg, Hooks{}, nil)

	for i := 0; i < 5; i++ {
		_, _ = client.Latest(context.Background(), "USD", []string{"CAD"}, RetryHooks{})
	}
	if client.BreakerState() != "open" {
		t.Fatalf("precondition failed: breaker not open, state=%q", client.BreakerState())
	}

	// Flip the vendor healthy and wait out the open-state cooldown so the
	// breaker transitions to half-open on the next call.
	atomic.StoreInt32(&healthy, 1)
	time.Sleep(cfg.BreakerOpenDuration + 50*time.Millisecond)

	// gobreaker's MaxRequests is 3 in half-open; a successful call here
	// should start closing it.
	_, err := client.Latest(context.Background(), "USD", []string{"CAD"}, RetryHooks{})
	if err != nil {
		t.Fatalf("expected success on half-open trial request, got %v", err)
	}

	// A few more successful trial calls should fully close the breaker.
	for i := 0; i < 3; i++ {
		_, _ = client.Latest(context.Background(), "USD", []string{"CAD"}, RetryHooks{})
	}

	if client.BreakerState() != "closed" {
		t.Errorf("expected breaker to close after healthy half-open trials, got %q", client.BreakerState())
	}
}

func TestBulkhead_LimitsConcurrentVendorCalls(t *testing.T) {
	const maxConcurrent = 2
	var current, maxSeen int32
	release := make(chan struct{})

	vendor := newTestVendor(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&current, 1)
		for {
			old := atomic.LoadInt32(&maxSeen)
			if n <= old || atomic.CompareAndSwapInt32(&maxSeen, old, n) {
				break
			}
		}
		<-release
		atomic.AddInt32(&current, -1)
		okHandler(w, r)
	})

	cfg := testClientConfig()
	cfg.MaxConcurrentCalls = maxConcurrent
	cfg.Retry.OverallBudget = 5 * time.Second
	cfg.Retry.AttemptTimeout = 5 * time.Second
	client := NewClient(vendor, cfg, Hooks{}, nil)

	var wg sync.WaitGroup
	const totalCallers = 5
	for i := 0; i < totalCallers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = client.Latest(context.Background(), "USD", []string{"CAD"}, RetryHooks{})
		}()
	}

	// Give all goroutines a chance to queue up against the bulkhead.
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	if maxSeen > maxConcurrent {
		t.Errorf("expected at most %d concurrent vendor calls, observed %d", maxConcurrent, maxSeen)
	}
}

func TestClient_PropagatesInboundContextCancellation(t *testing.T) {
	blockedUntilCanceled := make(chan struct{})
	vendor := newTestVendor(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		close(blockedUntilCanceled)
	})

	cfg := testClientConfig()
	cfg.Retry.AttemptTimeout = 2 * time.Second
	cfg.Retry.OverallBudget = 2 * time.Second
	client := NewClient(vendor, cfg, Hooks{}, nil)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := client.Latest(ctx, "USD", []string{"CAD"}, RetryHooks{})
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel() // simulate inbound client disconnect

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error after inbound context cancellation")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("expected call to return promptly after context cancellation")
	}
}

func TestCallOutcome(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil error is success", nil, "success"},
		{"breaker open", gobreaker.ErrOpenState, "breaker_open"},
		{"deadline exceeded is timeout", context.DeadlineExceeded, "timeout"},
		{"wrapped deadline exceeded is timeout", fmt.Errorf("attempt failed: %w", context.DeadlineExceeded), "timeout"},
		{"other error", errors.New("boom"), "error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := callOutcome(tc.err); got != tc.want {
				t.Errorf("callOutcome(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClient_HooksFireOnCallCompleteAndBulkhead(t *testing.T) {
	vendor := newTestVendor(t, okHandler)

	var outcome string
	var sawDuration bool
	var acquireCount, releaseCount int32

	client := NewClient(vendor, testClientConfig(), Hooks{
		OnCallComplete: func(o string, d time.Duration) {
			outcome = o
			sawDuration = d >= 0
		},
		OnBulkheadAcquire: func() { atomic.AddInt32(&acquireCount, 1) },
		OnBulkheadRelease: func() { atomic.AddInt32(&releaseCount, 1) },
	}, nil)

	_, err := client.Latest(context.Background(), "USD", []string{"CAD"}, RetryHooks{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if outcome != "success" {
		t.Errorf("expected OnCallComplete outcome \"success\", got %q", outcome)
	}
	if !sawDuration {
		t.Error("expected OnCallComplete to report a duration")
	}
	if acquireCount != 1 {
		t.Errorf("expected exactly 1 bulkhead acquire, got %d", acquireCount)
	}
	if releaseCount != 1 {
		t.Errorf("expected exactly 1 bulkhead release, got %d", releaseCount)
	}
}
