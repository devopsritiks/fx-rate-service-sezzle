package resilience

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"time"

	"github.com/rsharma41/sezzle-fx-service/internal/fxvendor/frankfurter"
)

// RetryConfig controls the retry loop's attempt count and backoff shape.
type RetryConfig struct {
	MaxAttempts    int
	AttemptTimeout time.Duration // per-attempt timeout
	OverallBudget  time.Duration // bounds every attempt combined
	BaseDelay      time.Duration
	MaxDelay       time.Duration
}

// RetryHooks lets callers observe retry behavior without the retry loop
// itself depending on metrics/logging packages. Metrics in a later phase
// plug in here instead of editing this file.
type RetryHooks struct {
	// OnAttempt is called before each attempt, 1-indexed.
	OnAttempt func(attempt int)
	// OnRetry is called after a retryable failure, before sleeping, with
	// the error that triggered the retry and the delay about to be used.
	OnRetry func(attempt int, err error, delay time.Duration)
}

// isRetryable reports whether err is worth retrying. We retry transport
// failures (timeouts, connection errors — these are usually transient),
// 429 (rate limited, the vendor is asking us to slow down and try again),
// and 5xx (vendor-side failure, may well succeed on retry). We do not
// retry 4xx other than 429: a bad currency code or malformed request
// will fail identically every time, so retrying just wastes a call and
// adds latency to a response we already know is an error.
func isRetryable(err error) bool {
	var transportErr *frankfurter.TransportError
	if errors.As(err, &transportErr) {
		return true
	}

	var statusErr *frankfurter.UpstreamStatusError
	if errors.As(err, &statusErr) {
		if statusErr.StatusCode == http.StatusTooManyRequests {
			return true
		}
		if statusErr.StatusCode >= 500 {
			return true
		}
		return false
	}

	// Anything else (e.g. JSON decode failure) is treated as non-retryable:
	// if the vendor returned 200 with a body we can't parse, calling again
	// is unlikely to produce a parseable body on a well-behaved vendor,
	// and we'd rather surface the error than mask it with retries.
	return false
}

// backoffDelay computes the full-jitter exponential backoff delay for the
// given attempt (0-indexed: the delay *before* attempt N+1). Full jitter
// means we pick a random delay between 0 and the exponential cap, rather
// than always sleeping the full computed delay — this spreads retries
// out in time across many concurrent callers instead of having them all
// retry in lockstep (the "thundering herd" problem you get from naive
// exponential backoff when an outage ends and everyone retries at once).
func backoffDelay(attempt int, base, max time.Duration) time.Duration {
	cap := time.Duration(1<<uint(attempt)) * base
	if cap > max || cap <= 0 {
		cap = max
	}
	return time.Duration(rand.Int63n(int64(cap) + 1))
}

// retryAfterOrBackoff returns the vendor's Retry-After duration if the
// error carries one, otherwise falls back to full-jitter backoff.
//
// We deliberately do NOT clamp Retry-After to our own MaxDelay: MaxDelay
// is a cap on *our* exponential backoff guess, not a ceiling the vendor's
// explicit instruction has to respect. If the vendor says "wait 30s"
// after a 429, honoring that is the entire point of reading the header —
// clamping it down to our own small default would mean we keep hammering
// a vendor that just told us to back off, which is the opposite of what
// Retry-After is for.
func retryAfterOrBackoff(err error, attempt int, base, maxBackoff time.Duration) time.Duration {
	var statusErr *frankfurter.UpstreamStatusError
	if errors.As(err, &statusErr) && statusErr.HasRetryAfter() {
		return statusErr.RetryAfter
	}
	return backoffDelay(attempt, base, maxBackoff)
}

// withRetry runs fn up to cfg.MaxAttempts times, applying a per-attempt
// timeout and an overall budget derived from the caller's context. It
// stops immediately — without starting a new attempt or sleeping through
// a backoff — if ctx is canceled (e.g. the inbound client disconnected)
// or the overall budget expires.
func withRetry[T any](ctx context.Context, cfg RetryConfig, hooks RetryHooks, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T

	budgetCtx, cancel := context.WithTimeout(ctx, cfg.OverallBudget)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := budgetCtx.Err(); err != nil {
			if lastErr != nil {
				return zero, lastErr
			}
			return zero, err
		}

		if hooks.OnAttempt != nil {
			hooks.OnAttempt(attempt)
		}

		attemptCtx, attemptCancel := context.WithTimeout(budgetCtx, cfg.AttemptTimeout)
		result, err := fn(attemptCtx)
		attemptCancel()

		if err == nil {
			return result, nil
		}
		lastErr = err

		if !isRetryable(err) {
			return zero, err
		}
		if attempt == cfg.MaxAttempts {
			return zero, err
		}

		delay := retryAfterOrBackoff(err, attempt-1, cfg.BaseDelay, cfg.MaxDelay)
		if hooks.OnRetry != nil {
			hooks.OnRetry(attempt, err, delay)
		}

		timer := time.NewTimer(delay)
		select {
		case <-budgetCtx.Done():
			timer.Stop()
			return zero, lastErr
		case <-timer.C:
		}
	}

	return zero, lastErr
}
