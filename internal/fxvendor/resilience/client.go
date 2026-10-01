// Package resilience wraps the raw Frankfurter client with everything
// needed to call a flaky third-party vendor safely from a production
// service: retries with backoff/jitter, a circuit breaker, and a
// concurrency bulkhead.
//
// The layers compose outside-in in a specific order, and the order is
// deliberate:
//
//	inbound request
//	  -> circuit breaker   (is the vendor even worth trying right now?)
//	       -> bulkhead      (how many of "trying it" are we allowed at once?)
//	            -> retry     (keep trying this one call, within a budget)
//	                 -> single HTTP attempt
//
// Circuit breaker is outermost because it answers a question about the
// vendor's overall health across many calls, not about one call's
// mechanics — so one whole Call() (which may itself retry several times
// internally) should count as exactly one success/failure data point to
// the breaker. If retries were outside the breaker, a single slow vendor
// call that retries 3 times would look like 3 separate failures to the
// breaker's trip-ratio math, which over-weights it.
//
// The breaker also gates the bulkhead: when the breaker is open we return
// immediately and never touch the concurrency semaphore at all, which is
// the entire point of "open means fail fast, don't even try" — a tripped
// breaker should cost us nothing, not even a bulkhead slot.
//
// Retry is innermost because it's the only layer that actually knows
// about individual network attempts (per-attempt timeout, which errors
// are worth retrying, backoff between attempts).
package resilience

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/frankfurter"
	"github.com/sony/gobreaker/v2"
)

// Config bundles everything needed to construct a Client.
type Config struct {
	Retry                   RetryConfig
	BreakerFailureThreshold uint32
	BreakerOpenDuration     time.Duration
	MaxConcurrentCalls      int
}

// Hooks lets callers observe breaker state transitions and retry
// attempts without this package depending on a metrics or logging
// package directly. A later observability phase registers Prometheus
// counters/gauges here instead of modifying this file.
type Hooks struct {
	Retry RetryHooks
	// OnBreakerStateChange is called whenever the circuit breaker
	// transitions, with the old and new state names ("closed", "open",
	// "half-open").
	OnBreakerStateChange func(from, to string)
	// OnCallComplete is called once per Latest() call (not per retry
	// attempt — see the package doc comment on why the breaker, and
	// therefore this hook, operates at the whole-call level) with a
	// bounded outcome string ("success", "error", "timeout",
	// "breaker_open") and the call's total duration.
	OnCallComplete func(outcome string, duration time.Duration)
	// OnBulkheadAcquire / OnBulkheadRelease bracket the time a call holds
	// a bulkhead concurrency slot, for an in-flight-calls gauge.
	OnBulkheadAcquire func()
	OnBulkheadRelease func()
}

// ErrBreakerOpen is returned when the circuit breaker is open and a call
// is rejected without attempting the vendor at all.
var ErrBreakerOpen = errors.New("circuit breaker is open: vendor calls are currently suspended")

// Client is the resilient facade over frankfurter.Client that handlers
// should call instead of the raw vendor client.
type Client struct {
	vendor   *frankfurter.Client
	retryCfg RetryConfig
	breaker  *gobreaker.CircuitBreaker[*frankfurter.LatestRates]
	bulk     *bulkhead
	hooks    Hooks
}

// NewClient builds a resilient client wrapping vendor with retry,
// breaker, and bulkhead behavior per cfg.
func NewClient(vendor *frankfurter.Client, cfg Config, hooks Hooks, logger *slog.Logger) *Client {
	settings := gobreaker.Settings{
		Name:        "frankfurter",
		MaxRequests: 3, // how many trial requests we allow through in half-open
		Interval:    0, // never reset closed-state counts on a timer; only on state change
		Timeout:     cfg.BreakerOpenDuration,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			// Require a minimum sample size before tripping, so a single
			// cold-start failure or two doesn't open the breaker — we want
			// a real failure *ratio* over a meaningful number of calls.
			if counts.Requests < cfg.BreakerFailureThreshold {
				return false
			}
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return failureRatio >= 0.5
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			if logger != nil {
				logger.Warn("circuit_breaker_state_change",
					"breaker", name,
					"from", from.String(),
					"to", to.String(),
				)
			}
			if hooks.OnBreakerStateChange != nil {
				hooks.OnBreakerStateChange(from.String(), to.String())
			}
		},
	}

	return &Client{
		vendor:   vendor,
		retryCfg: cfg.Retry,
		breaker:  gobreaker.NewCircuitBreaker[*frankfurter.LatestRates](settings),
		bulk:     newBulkhead(cfg.MaxConcurrentCalls),
		hooks:    hooks,
	}
}

// Latest fetches rates for base against symbols, going through the full
// breaker -> bulkhead -> retry stack. ctx should be the inbound request's
// context: if the caller disconnects or their own deadline expires, that
// propagates all the way down and we stop calling the vendor promptly
// instead of continuing to burn retries/attempts for a response nobody
// will receive.
func (c *Client) Latest(ctx context.Context, base string, symbols []string, hooks RetryHooks) (*frankfurter.LatestRates, error) {
	start := time.Now()

	result, err := c.breaker.Execute(func() (*frankfurter.LatestRates, error) {
		if c.hooks.OnBulkheadAcquire != nil {
			c.hooks.OnBulkheadAcquire()
		}
		if err := c.bulk.acquire(ctx); err != nil {
			if c.hooks.OnBulkheadRelease != nil {
				c.hooks.OnBulkheadRelease()
			}
			return nil, err
		}
		defer func() {
			c.bulk.release()
			if c.hooks.OnBulkheadRelease != nil {
				c.hooks.OnBulkheadRelease()
			}
		}()

		return withRetry(ctx, c.retryCfg, hooks, func(attemptCtx context.Context) (*frankfurter.LatestRates, error) {
			return c.vendor.Latest(attemptCtx, base, symbols)
		})
	})

	if c.hooks.OnCallComplete != nil {
		c.hooks.OnCallComplete(callOutcome(err), time.Since(start))
	}

	if err != nil && errors.Is(err, gobreaker.ErrOpenState) {
		return nil, ErrBreakerOpen
	}
	return result, err
}

// callOutcome classifies a Latest() call's result into a small, fixed
// set of labels for the vendor-call-duration histogram: "success",
// "breaker_open" (we never touched the vendor), "timeout" (the call's
// own context deadline was hit), or "error" (anything else — transport
// failure, 5xx, etc, after retries were exhausted).
func callOutcome(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, gobreaker.ErrOpenState):
		return "breaker_open"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "error"
	}
}

// BreakerState reports the current circuit breaker state as a string
// ("closed", "open", "half-open") for use in /ready and logging.
func (c *Client) BreakerState() string {
	return c.breaker.State().String()
}
