// Package ratescache sits between handlers and the resilient vendor
// client (internal/fxvendor/resilience). It is the layer that decides
// "do we even need to call the vendor, and if the vendor call fails, do
// we have something safe to fall back to."
//
// This is deliberately a layer *outside* the breaker/bulkhead/retry
// stack, not inside it: those layers answer "how do we safely make one
// call to the vendor," this layer answers "do we need to make that call
// at all right now." Composed outside-in:
//
//	inbound request
//	  -> ratescache   (serve from cache if fresh; on miss/stale-refresh,
//	                    singleflight-dedupe the call; on vendor failure,
//	                    fall back to a still-valid stale entry)
//	       -> resilience.Client   (breaker -> bulkhead -> retry -> attempt)
//
// Two TTLs govern what a cached entry means:
//
//   - FreshTTL: while an entry's age is under this, we serve it directly
//     with no vendor call at all (an X-Cache: HIT).
//   - MaxStaleAge: once an entry's age exceeds FreshTTL we try the
//     vendor again; if that call fails (including "circuit breaker is
//     open"), we fall back to the existing entry as long as its age is
//     still under MaxStaleAge (X-Cache: STALE). Past MaxStaleAge we
//     refuse to serve it at all — a 503, not a silently too-old
//     financial rate.
package ratescache

import (
	"context"
	"errors"
	"time"

	"github.com/rsharma41/sezzle-fx-service/internal/cache"
	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/frankfurter"
	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/resilience"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/singleflight"
)

// VendorClient is the subset of resilience.Client this package depends
// on, defined on the consumer side so tests can use a small fake instead
// of standing up the full breaker/retry/bulkhead stack.
type VendorClient interface {
	Latest(ctx context.Context, base string, symbols []string, hooks resilience.RetryHooks) (*frankfurter.LatestRates, error)
	BreakerState() string
}

// Config controls TTL behavior and negative-cache duration.
type Config struct {
	FreshTTL time.Duration
	// MaxStaleAge is the hard ceiling: an entry older than this is never
	// served, fresh or not — see the fintech-correctness note in the
	// package doc comment.
	MaxStaleAge time.Duration
	// NegativeTTL is how long we remember "the vendor said this
	// base/symbols combination doesn't exist" before trying again. Kept
	// short and separate from FreshTTL: a vendor error response is a
	// different kind of fact than a successful rate, and we don't want a
	// currency that Frankfurter adds support for later to stay blocked for
	// as long as a normal fresh-rate TTL would imply.
	NegativeTTL time.Duration
}

// Hooks lets callers observe cache behavior for a later metrics phase
// without this package depending on Prometheus directly.
type Hooks struct {
	OnHit func(key string)
	// OnNegativeHit fires when a lookup is served from the negative cache
	// — a previously-remembered "the vendor said this currency doesn't
	// exist" — without a vendor call. Distinct from OnHit (a positive
	// rate lookup) so a metrics phase can tell the two apart.
	OnNegativeHit func(key string)
	OnMiss        func(key string)
	OnStale       func(key string, age time.Duration)
	// OnSingleflightShared fires once per caller who received a result
	// from someone else's in-flight vendor call rather than triggering
	// their own — i.e. singleflight.Group.Do's shared=true case. It does
	// not report a count of concurrent waiters (the singleflight package
	// itself doesn't expose that number); a metrics phase would count
	// calls to this hook over a time window as "calls deduped" instead.
	OnSingleflightShared func(key string)
}

// ErrStaleDataUnavailable is returned when the vendor call failed and the
// only cached entry we have is older than MaxStaleAge — we refuse to
// serve it.
var ErrStaleDataUnavailable = errors.New("cached rate exceeds maximum stale age and vendor is unavailable")

// ErrCurrencyNotFound is returned for a negatively-cached (or freshly
// vendor-rejected) base/symbols combination.
var ErrCurrencyNotFound = errors.New("currency not found")

// Result wraps a rates lookup with the cache metadata a handler needs to
// set X-Cache / Cache-Control headers and the stale/age fields in the
// response body.
type Result struct {
	Rates     map[string]decimal.Decimal
	Base      string
	RatesDate string
	Stale     bool
	CacheHit  bool
	Age       time.Duration
}

// RatesCache is the cache-aware facade handlers call instead of talking
// to resilience.Client directly.
type RatesCache struct {
	store  cache.Cache
	vendor VendorClient
	cfg    Config
	hooks  Hooks
	group  singleflight.Group
}

// New builds a RatesCache wrapping vendor with store as its backing
// cache.
func New(store cache.Cache, vendor VendorClient, cfg Config, hooks Hooks) *RatesCache {
	return &RatesCache{store: store, vendor: vendor, cfg: cfg, hooks: hooks}
}

// singleflightValue is what we pass through singleflight.Group.Do, since
// it only carries a single (any, error) pair — we need both the parsed
// rates and whether this particular call found a positive or negative
// vendor result.
type singleflightValue struct {
	rates *frankfurter.LatestRates
	// notFound is set when the vendor told us this currency/symbol
	// combination doesn't exist (so we should negatively cache it) rather
	// than a transient failure (so we should fall back to stale).
	notFound bool
}

// Latest returns rates for base/symbols, serving from cache when fresh,
// falling back to a stale cached entry when the vendor call fails and a
// usable entry exists, and otherwise propagating the vendor error.
//
// retryHooks is passed through to the resilience layer unchanged so
// request-scoped logging (e.g. "this request's 2nd retry attempt") keeps
// working exactly as it did before caching existed.
func (c *RatesCache) Latest(ctx context.Context, base string, symbols []string, retryHooks resilience.RetryHooks) (Result, error) {
	key := cache.Key(base, symbols)

	if entry, ok := c.store.Get(key); ok {
		if entry.Negative && entry.Age() < c.cfg.NegativeTTL {
			if c.hooks.OnNegativeHit != nil {
				c.hooks.OnNegativeHit(key)
			}
			return Result{}, ErrCurrencyNotFound
		}
		if !entry.Negative && entry.Age() < c.cfg.FreshTTL {
			if c.hooks.OnHit != nil {
				c.hooks.OnHit(key)
			}
			return entryToResult(entry, false, true), nil
		}
	}

	if c.hooks.OnMiss != nil {
		c.hooks.OnMiss(key)
	}

	sfResult, err, shared := c.group.Do(key, func() (any, error) {
		rates, callErr := c.vendor.Latest(ctx, base, symbols, retryHooks)
		if callErr != nil {
			var statusErr *frankfurter.UpstreamStatusError
			if errors.As(callErr, &statusErr) && statusErr.StatusCode == 404 {
				return singleflightValue{notFound: true}, nil
			}
			return nil, callErr
		}
		return singleflightValue{rates: rates}, nil
	})

	if shared && c.hooks.OnSingleflightShared != nil {
		c.hooks.OnSingleflightShared(key)
	}

	if err == nil {
		sfv := sfResult.(singleflightValue)
		if sfv.notFound {
			c.store.Set(key, cache.Entry{FetchedAt: time.Now(), Negative: true})
			return Result{}, ErrCurrencyNotFound
		}

		entry := rateToEntry(sfv.rates)
		c.store.Set(key, entry)
		// This is a MISS, not a HIT: it required an actual vendor call.
		// CacheHit=false here is what tells writeCacheHeaders to set
		// X-Cache: MISS rather than HIT.
		return entryToResult(entry, false, false), nil
	}

	// Vendor call failed (transport error, breaker open, etc). Fall back
	// to a stale cached entry if we have one that's still within
	// MaxStaleAge. This is the entire point of this phase: a vendor
	// outage degrades us to "slightly old but clearly labeled" rather
	// than failing every request.
	if entry, ok := c.store.Get(key); ok && !entry.Negative {
		if entry.Age() <= c.cfg.MaxStaleAge {
			if c.hooks.OnStale != nil {
				c.hooks.OnStale(key, entry.Age())
			}
			return entryToResult(entry, true, false), nil
		}
		return Result{}, ErrStaleDataUnavailable
	}

	return Result{}, err
}

func rateToEntry(rates *frankfurter.LatestRates) cache.Entry {
	strRates := make(map[string]string, len(rates.Rates))
	for k, v := range rates.Rates {
		strRates[k] = v.String()
	}
	return cache.Entry{
		Rates:     strRates,
		Base:      rates.Base,
		RatesDate: rates.Date,
		FetchedAt: time.Now(),
	}
}

func entryToResult(entry cache.Entry, stale, cacheHit bool) Result {
	rates := make(map[string]decimal.Decimal, len(entry.Rates))
	for k, v := range entry.Rates {
		// These strings were produced by decimal.String() in rateToEntry,
		// so they always parse; an error here would indicate memory
		// corruption, not bad input.
		d, _ := decimal.NewFromString(v)
		rates[k] = d
	}
	return Result{
		Rates:     rates,
		Base:      entry.Base,
		RatesDate: entry.RatesDate,
		Stale:     stale,
		CacheHit:  cacheHit,
		Age:       entry.Age(),
	}
}

// BreakerState passes through the underlying resilience.Client's circuit
// breaker state, so httpapi's /ready handler has one dependency
// (VendorClient) to ask for both cache and breaker health instead of two.
func (c *RatesCache) BreakerState() string {
	return c.vendor.BreakerState()
}

// CacheStats reports the current entry count and the age of the oldest
// entry, for /ready reporting.
func (c *RatesCache) CacheStats() (entries int, oldestAge time.Duration) {
	return c.store.Len(), c.store.OldestAge()
}
