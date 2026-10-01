package ratescache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsharma41/sezzle-fx-service/internal/cache"
	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/frankfurter"
	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/resilience"
	"github.com/shopspring/decimal"
)

// fakeVendor is a test double for the VendorClient interface so these
// tests can control exactly what the "vendor" returns without going
// through the real breaker/retry/bulkhead stack (that stack has its own
// tests in internal/fxvendor/resilience).
type fakeVendor struct {
	calls   int32
	fn      func(callNum int32) (*frankfurter.LatestRates, error)
	breaker string
}

func (f *fakeVendor) Latest(ctx context.Context, base string, symbols []string, hooks resilience.RetryHooks) (*frankfurter.LatestRates, error) {
	n := atomic.AddInt32(&f.calls, 1)
	return f.fn(n)
}

func (f *fakeVendor) BreakerState() string {
	if f.breaker == "" {
		return "closed"
	}
	return f.breaker
}

func (f *fakeVendor) callCount() int32 {
	return atomic.LoadInt32(&f.calls)
}

func okRates(base string, rates map[string]string) func(int32) (*frankfurter.LatestRates, error) {
	return func(int32) (*frankfurter.LatestRates, error) {
		return parsedRates(base, rates), nil
	}
}

// parsedRates builds a *frankfurter.LatestRates from plain decimal strings.
func parsedRates(base string, rates map[string]string) *frankfurter.LatestRates {
	parsed := make(map[string]decimal.Decimal, len(rates))
	for k, v := range rates {
		parsed[k] = decimal.RequireFromString(v)
	}
	return &frankfurter.LatestRates{Base: base, Date: "2024-01-15", Rates: parsed}
}

func testConfig() Config {
	return Config{
		FreshTTL:    50 * time.Millisecond,
		MaxStaleAge: 200 * time.Millisecond,
		NegativeTTL: 50 * time.Millisecond,
	}
}

func TestRatesCache_MissThenHit(t *testing.T) {
	vendor := &fakeVendor{fn: okRates("USD", map[string]string{"CAD": "1.35"})}
	rc := New(cache.NewLRU(100, cache.Hooks{}), vendor, testConfig(), Hooks{})

	result1, err := rc.Latest(context.Background(), "USD", []string{"CAD"}, resilience.RetryHooks{})
	if err != nil {
		t.Fatalf("unexpected error on first call: %v", err)
	}
	if result1.CacheHit {
		t.Error("expected first call to be a MISS (CacheHit=false)")
	}
	if vendor.callCount() != 1 {
		t.Fatalf("expected 1 vendor call after first request, got %d", vendor.callCount())
	}

	result2, err := rc.Latest(context.Background(), "USD", []string{"CAD"}, resilience.RetryHooks{})
	if err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	if !result2.CacheHit {
		t.Error("expected second call within fresh TTL to be a HIT (CacheHit=true)")
	}
	if vendor.callCount() != 1 {
		t.Errorf("expected still only 1 vendor call (served from cache), got %d", vendor.callCount())
	}
}

func TestRatesCache_KeyNormalizationHitsSameEntry(t *testing.T) {
	vendor := &fakeVendor{fn: okRates("USD", map[string]string{"CAD": "1.35", "EUR": "0.9"})}
	rc := New(cache.NewLRU(100, cache.Hooks{}), vendor, testConfig(), Hooks{})

	_, err := rc.Latest(context.Background(), "usd", []string{"CAD", "EUR"}, resilience.RetryHooks{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Same logical request, different case and symbol order — must hit the
	// same cache entry and not trigger a second vendor call.
	result, err := rc.Latest(context.Background(), "USD", []string{"eur", "cad"}, resilience.RetryHooks{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.CacheHit {
		t.Error("expected normalized key to hit the same cache entry")
	}
	if vendor.callCount() != 1 {
		t.Errorf("expected exactly 1 vendor call despite case/order differences, got %d", vendor.callCount())
	}
}

func TestRatesCache_ConvertUsesOneCachedEntryForAnyAmount(t *testing.T) {
	// This test documents the design requirement directly: the cache key
	// must never include "amount" — one rates entry must serve unlimited
	// convert requests. We simulate that by calling Latest (as
	// ConvertHandler does) multiple times and confirming only one vendor
	// call happens regardless of how many "amounts" a caller would apply
	// locally afterward.
	vendor := &fakeVendor{fn: okRates("USD", map[string]string{"CAD": "1.35"})}
	rc := New(cache.NewLRU(100, cache.Hooks{}), vendor, testConfig(), Hooks{})

	for i := 0; i < 5; i++ {
		if _, err := rc.Latest(context.Background(), "USD", []string{"CAD"}, resilience.RetryHooks{}); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
	if vendor.callCount() != 1 {
		t.Errorf("expected exactly 1 vendor call across 5 'convert' requests, got %d", vendor.callCount())
	}
}

func TestRatesCache_FreshExpiryTriggersRefresh(t *testing.T) {
	vendor := &fakeVendor{fn: okRates("USD", map[string]string{"CAD": "1.35"})}
	cfg := testConfig()
	cfg.FreshTTL = 20 * time.Millisecond
	rc := New(cache.NewLRU(100, cache.Hooks{}), vendor, cfg, Hooks{})

	rc.Latest(context.Background(), "USD", []string{"CAD"}, resilience.RetryHooks{})
	time.Sleep(40 * time.Millisecond) // let fresh TTL lapse

	result, err := rc.Latest(context.Background(), "USD", []string{"CAD"}, resilience.RetryHooks{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CacheHit {
		t.Error("expected a fresh vendor call after fresh TTL expiry, not a cache hit")
	}
	if vendor.callCount() != 2 {
		t.Errorf("expected 2 vendor calls (initial + refresh after fresh TTL), got %d", vendor.callCount())
	}
}

func TestRatesCache_ServesStaleWhenVendorFails(t *testing.T) {
	var failing int32
	vendor := &fakeVendor{fn: func(int32) (*frankfurter.LatestRates, error) {
		if atomic.LoadInt32(&failing) == 1 {
			return nil, errors.New("vendor down")
		}
		return parsedRates("USD", map[string]string{"CAD": "1.35"}), nil
	}}

	cfg := testConfig()
	cfg.FreshTTL = 20 * time.Millisecond
	cfg.MaxStaleAge = 5 * time.Second

	var staleCalled bool
	var staleAge time.Duration
	rc := New(cache.NewLRU(100, cache.Hooks{}), vendor, cfg, Hooks{
		OnStale: func(key string, age time.Duration) {
			staleCalled = true
			staleAge = age
		},
	})

	// Warm the cache.
	if _, err := rc.Latest(context.Background(), "USD", []string{"CAD"}, resilience.RetryHooks{}); err != nil {
		t.Fatalf("unexpected error warming cache: %v", err)
	}

	// Let it go past fresh TTL, then make the vendor start failing.
	time.Sleep(40 * time.Millisecond)
	atomic.StoreInt32(&failing, 1)

	result, err := rc.Latest(context.Background(), "USD", []string{"CAD"}, resilience.RetryHooks{})
	if err != nil {
		t.Fatalf("expected stale fallback, not an error: %v", err)
	}
	if !result.Stale {
		t.Error("expected Stale=true when vendor fails but a within-bounds cached entry exists")
	}
	if rate, ok := result.Rates["CAD"]; !ok || rate.String() != "1.35" {
		t.Errorf("expected stale rate to still be the previously cached 1.35, got %+v", result.Rates)
	}
	if !staleCalled {
		t.Error("expected OnStale hook to fire")
	}
	if staleAge <= 0 {
		t.Errorf("expected a positive age reported to OnStale, got %v", staleAge)
	}
}

func TestRatesCache_BeyondMaxStaleAgeReturns503Equivalent(t *testing.T) {
	var failing int32
	vendor := &fakeVendor{fn: func(int32) (*frankfurter.LatestRates, error) {
		if atomic.LoadInt32(&failing) == 1 {
			return nil, errors.New("vendor down")
		}
		return parsedRates("USD", map[string]string{"CAD": "1.35"}), nil
	}}

	cfg := testConfig()
	cfg.FreshTTL = 10 * time.Millisecond
	cfg.MaxStaleAge = 30 * time.Millisecond
	rc := New(cache.NewLRU(100, cache.Hooks{}), vendor, cfg, Hooks{})

	if _, err := rc.Latest(context.Background(), "USD", []string{"CAD"}, resilience.RetryHooks{}); err != nil {
		t.Fatalf("unexpected error warming cache: %v", err)
	}

	atomic.StoreInt32(&failing, 1)
	time.Sleep(60 * time.Millisecond) // past both FreshTTL and MaxStaleAge

	_, err := rc.Latest(context.Background(), "USD", []string{"CAD"}, resilience.RetryHooks{})
	if !errors.Is(err, ErrStaleDataUnavailable) {
		t.Fatalf("expected ErrStaleDataUnavailable once cached entry exceeds MaxStaleAge, got %v", err)
	}
}

func TestRatesCache_SingleflightCollapsesConcurrentMisses(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	vendor := &fakeVendor{fn: func(int32) (*frankfurter.LatestRates, error) {
		atomic.AddInt32(&calls, 1)
		<-release // hold every call open until the test releases them
		return parsedRates("USD", map[string]string{"CAD": "1.35"}), nil
	}}

	var sharedCount int32
	rc := New(cache.NewLRU(100, cache.Hooks{}), vendor, testConfig(), Hooks{
		OnSingleflightShared: func(key string) {
			atomic.AddInt32(&sharedCount, 1)
		},
	})

	const numCallers = 50
	var wg sync.WaitGroup
	errs := make([]error, numCallers)
	for i := 0; i < numCallers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := rc.Latest(context.Background(), "USD", []string{"CAD"}, resilience.RetryHooks{})
			errs[i] = err
		}(i)
	}

	time.Sleep(100 * time.Millisecond) // let all goroutines queue up on singleflight
	close(release)
	wg.Wait()

	if vendor.callCount() != 1 {
		t.Errorf("expected exactly 1 vendor call for %d concurrent requests on the same key, got %d", numCallers, vendor.callCount())
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: unexpected error: %v", i, err)
		}
	}
	if atomic.LoadInt32(&sharedCount) == 0 {
		t.Error("expected OnSingleflightShared to fire for at least one deduped caller")
	}
}

func TestRatesCache_NegativeCachingSuppressesRepeatedVendorCalls(t *testing.T) {
	vendor := &fakeVendor{fn: func(int32) (*frankfurter.LatestRates, error) {
		return nil, &frankfurter.UpstreamStatusError{StatusCode: 404}
	}}

	cfg := testConfig()
	cfg.NegativeTTL = 200 * time.Millisecond
	rc := New(cache.NewLRU(100, cache.Hooks{}), vendor, cfg, Hooks{})

	for i := 0; i < 5; i++ {
		_, err := rc.Latest(context.Background(), "USD", []string{"ZZZ"}, resilience.RetryHooks{})
		if !errors.Is(err, ErrCurrencyNotFound) {
			t.Fatalf("call %d: expected ErrCurrencyNotFound, got %v", i, err)
		}
	}

	if vendor.callCount() != 1 {
		t.Errorf("expected exactly 1 real vendor call, remaining requests served from negative cache; got %d calls", vendor.callCount())
	}
}

func TestRatesCache_NegativeCacheExpiresAndRetriesVendor(t *testing.T) {
	vendor := &fakeVendor{fn: func(int32) (*frankfurter.LatestRates, error) {
		return nil, &frankfurter.UpstreamStatusError{StatusCode: 404}
	}}

	cfg := testConfig()
	cfg.NegativeTTL = 20 * time.Millisecond
	rc := New(cache.NewLRU(100, cache.Hooks{}), vendor, cfg, Hooks{})

	rc.Latest(context.Background(), "USD", []string{"ZZZ"}, resilience.RetryHooks{})
	time.Sleep(40 * time.Millisecond)
	rc.Latest(context.Background(), "USD", []string{"ZZZ"}, resilience.RetryHooks{})

	if vendor.callCount() != 2 {
		t.Errorf("expected negative cache to expire and trigger a 2nd vendor call, got %d calls", vendor.callCount())
	}
}

func TestRatesCache_CacheStatsReflectsStore(t *testing.T) {
	vendor := &fakeVendor{fn: okRates("USD", map[string]string{"CAD": "1.35"})}
	rc := New(cache.NewLRU(100, cache.Hooks{}), vendor, testConfig(), Hooks{})

	entries, _ := rc.CacheStats()
	if entries != 0 {
		t.Errorf("expected 0 entries before any calls, got %d", entries)
	}

	rc.Latest(context.Background(), "USD", []string{"CAD"}, resilience.RetryHooks{})

	entries, oldestAge := rc.CacheStats()
	if entries != 1 {
		t.Errorf("expected 1 entry after a call, got %d", entries)
	}
	if oldestAge < 0 {
		t.Errorf("expected non-negative oldest age, got %v", oldestAge)
	}
}

func TestRatesCache_BreakerStatePassesThrough(t *testing.T) {
	vendor := &fakeVendor{breaker: "open"}
	rc := New(cache.NewLRU(100, cache.Hooks{}), vendor, testConfig(), Hooks{})

	if got := rc.BreakerState(); got != "open" {
		t.Errorf("expected BreakerState() to pass through to the vendor, got %q", got)
	}
}
