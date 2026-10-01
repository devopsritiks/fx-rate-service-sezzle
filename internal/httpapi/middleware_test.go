package httpapi

import "testing"

func TestRoutePattern_CollapsesVariablePathSegmentsToFixedSet(t *testing.T) {
	// This is the cardinality-safety contract for metrics labels: no
	// matter how many distinct currency codes callers request, the
	// "route" label must only ever take one of a small, fixed set of
	// values — never the raw path, which would otherwise produce one
	// time series per currency code queried.
	cases := []struct {
		path string
		want string
	}{
		{"/v1/rates/USD", "/v1/rates/{base}"},
		{"/v1/rates/CAD", "/v1/rates/{base}"},
		{"/v1/rates/EUR", "/v1/rates/{base}"},
		{"/v1/convert", "/v1/convert"},
		{"/health", "/health"},
		{"/ready", "/ready"},
		{"/metrics", "/metrics"},
		{"/not-a-real-path", "other"},
		{"/v1/unknown", "other"},
	}

	for _, tc := range cases {
		if got := routePattern(tc.path); got != tc.want {
			t.Errorf("routePattern(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestRoutePattern_ManyDistinctPathsCollapseToOneLabelValue(t *testing.T) {
	currencies := []string{"USD", "CAD", "EUR", "GBP", "JPY", "AUD", "CHF", "CNY", "INR", "NZD"}
	seen := map[string]bool{}
	for _, c := range currencies {
		seen[routePattern("/v1/rates/"+c)] = true
	}
	if len(seen) != 1 {
		t.Errorf("expected all currency-specific paths to collapse to exactly 1 route label, got %d distinct values: %v", len(seen), seen)
	}
}
