// Package cache provides a small cache abstraction for vendor rate
// lookups, plus an in-memory LRU implementation.
//
// The Cache interface exists so the call site (internal/fxvendor/ratescache)
// never depends on *how* entries are stored — today that's an in-process
// LRU map; swapping to Redis later (see the tradeoff note on Cache below)
// means writing a new implementation of this interface and changing one
// line of wiring in main.go. Nothing in the handlers or the resilience
// layer would need to change.
package cache

import "time"

// Entry is one cached rates lookup result, plus the bookkeeping needed to
// decide fresh/stale/expired and to report that state to callers.
type Entry struct {
	Rates map[string]string // currency code -> decimal string (avoids re-importing decimal here)
	Base  string
	// RatesDate is the vendor's own "as of" date for these rates (e.g.
	// "2024-01-15"), not when we fetched them — this is what a caller
	// actually cares about for a financial rate.
	RatesDate string
	// FetchedAt is when we stored this entry, used to compute Age and to
	// decide fresh vs. stale vs. expired against the configured TTLs.
	FetchedAt time.Time
	// Negative marks a cached failure (e.g. vendor 404 for an unsupported
	// currency pair) rather than a successful rates lookup. Negative
	// entries are never served as "stale" fallback data — see
	// internal/fxvendor/ratescache for how this is used.
	Negative bool
}

// Age returns how long ago this entry was fetched.
func (e Entry) Age() time.Duration {
	return time.Since(e.FetchedAt)
}

// Cache is the interface the rest of the service depends on. An
// in-memory implementation (LRU, this package) and a hypothetical Redis
// implementation both satisfy it identically from the caller's point of
// view.
//
// Tradeoff: in-memory vs. Redis when running multiple pods
//
// In-memory (what we ship in this phase): each pod holds its own cache.
// That means:
//   - Each pod's cache miss triggers its own vendor call — with N pods,
//     a synchronized cold start (e.g. a rolling deploy) can produce up to
//     N independent vendor calls for the same key instead of 1. Given
//     Frankfurter rates only change once a day, this is a bounded, cheap
//     cost, not a real scaling problem at typical pod counts.
//   - Hit ratio is a per-pod number. A freshly scaled-up pod starts at 0%
//     hit ratio and dashboards need to look at this per-pod (or as a
//     distribution) rather than assuming one fleet-wide number.
//   - Pods can briefly disagree: one pod might be serving a slightly
//     different "as of" date than another, if their fetches landed at
//     different times. For a BNPL checkout price, this is a minor
//     consistency gap we accept.
//   - Upside: zero extra network hop, zero extra infrastructure
//     dependency, and critically, isolated blast radius — if the cache
//     layer itself breaks, it breaks one pod, not the whole fleet.
//
// Redis (shared cache): one vendor call warms every pod, hit ratio is a
// single fleet-wide number, and all pods see the same "as of" date. The
// cost is a network hop on every cache read (still far cheaper than
// calling Frankfurter), one more operational dependency, and a new
// shared-fate failure mode — if Redis has a bad day, every pod's cache
// layer has a bad day at the same time, which is exactly the kind of
// synchronized failure phase 2 (circuit breaker, bulkhead) was designed
// to avoid at the vendor layer.
//
// Given Frankfurter updates once a day, the per-pod vendor call overhead
// of in-memory caching is negligible — this is the right default here.
// Redis becomes the right call once vendor call *volume or cost* is the
// dominant concern, or once cross-pod consistency genuinely matters for
// a specific feature — not as a default upgrade.
type Cache interface {
	// Get returns the entry for key and whether it was found at all.
	// Callers are responsible for interpreting Entry.Age() against their
	// own fresh/stale/expired thresholds — the cache itself has no
	// opinion on TTLs, it just stores and evicts by capacity.
	Get(key string) (Entry, bool)
	// Set stores (or overwrites) the entry for key.
	Set(key string, entry Entry)
	// Len reports the current number of entries, for /ready reporting.
	Len() int
	// OldestAge reports the age of the oldest entry currently stored, or
	// zero if the cache is empty — also for /ready reporting.
	OldestAge() time.Duration
}
