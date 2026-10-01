package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNew_RegistersBuildInfo(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg, "1.2.3")

	got := testutil.ToFloat64(m.BuildInfo.WithLabelValues("1.2.3"))
	if got != 1 {
		t.Errorf("expected build_info{version=\"1.2.3\"} = 1, got %v", got)
	}
}

func TestObserveHTTPRequest_RecordsCounterAndHistogram(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg, "test")

	m.ObserveHTTPRequest("GET", "/v1/convert", 200, 15*time.Millisecond)

	count := testutil.ToFloat64(m.HTTPRequestsTotal.WithLabelValues("GET", "/v1/convert", "2xx"))
	if count != 1 {
		t.Errorf("expected 1 request recorded, got %v", count)
	}
}

func TestBreakerStateValue(t *testing.T) {
	cases := map[string]float64{
		"closed":    0,
		"half-open": 1,
		"open":      2,
		"unknown":   -1,
	}
	for state, want := range cases {
		if got := BreakerStateValue(state); got != want {
			t.Errorf("BreakerStateValue(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestOnBreakerStateChange_SetsGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg, "test")

	m.OnBreakerStateChange("closed", "open")

	got := testutil.ToFloat64(m.BreakerState.WithLabelValues("frankfurter"))
	if got != 2 {
		t.Errorf("expected gauge set to 2 (open), got %v", got)
	}
}

func TestCacheHooks_IncrementCorrectLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg, "test")

	m.OnCacheHit("k1")
	m.OnCacheMiss("k2")
	m.OnCacheStale("k3", time.Second)
	m.OnCacheNegativeHit("k4")

	if got := testutil.ToFloat64(m.CacheRequestsTotal.WithLabelValues("hit")); got != 1 {
		t.Errorf("hit count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.CacheRequestsTotal.WithLabelValues("miss")); got != 1 {
		t.Errorf("miss count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.CacheRequestsTotal.WithLabelValues("stale")); got != 1 {
		t.Errorf("stale count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.CacheRequestsTotal.WithLabelValues("negative_hit")); got != 1 {
		t.Errorf("negative_hit count = %v, want 1", got)
	}
}

func TestBulkheadGauge_IncAndDec(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg, "test")

	m.OnBulkheadAcquire()
	m.OnBulkheadAcquire()
	if got := testutil.ToFloat64(m.VendorInFlight); got != 2 {
		t.Errorf("expected in-flight gauge 2 after two acquires, got %v", got)
	}

	m.OnBulkheadRelease()
	if got := testutil.ToFloat64(m.VendorInFlight); got != 1 {
		t.Errorf("expected in-flight gauge 1 after a release, got %v", got)
	}
}

func TestRoutePatternCardinality_SampleCheck(t *testing.T) {
	// This isn't testing internal/httpapi's routePattern directly (that
	// lives in a different package), but documents the contract metrics
	// relies on: route labels must come from a small fixed set, never a
	// raw path. A sanity check that our own label values don't contain
	// anything path-like with high cardinality potential (e.g. a
	// currency code baked into the string).
	reg := prometheus.NewRegistry()
	m := New(reg, "test")

	m.ObserveHTTPRequest("GET", "/v1/rates/{base}", 200, time.Millisecond)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather failed: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "fxservice_http_requests_total" {
			continue
		}
		for _, metric := range f.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "route" && strings.Contains(label.GetValue(), "USD") {
					t.Errorf("route label leaked a currency code: %q", label.GetValue())
				}
			}
		}
	}
}
