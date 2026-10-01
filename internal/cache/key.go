package cache

import (
	"sort"
	"strings"
)

// Key builds a normalized cache key for a base currency + symbol set, so
// that e.g. base=USD symbols=CAD,EUR and base=USD symbols=EUR,CAD hit the
// exact same entry. Symbols are upper-cased, deduplicated, and sorted
// before joining — order and case in the caller's request must never
// affect which cache entry is used.
func Key(base string, symbols []string) string {
	base = strings.ToUpper(strings.TrimSpace(base))

	seen := make(map[string]struct{}, len(symbols))
	normalized := make([]string, 0, len(symbols))
	for _, s := range symbols {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		normalized = append(normalized, s)
	}
	sort.Strings(normalized)

	if len(normalized) == 0 {
		return base
	}
	return base + ":" + strings.Join(normalized, ",")
}
