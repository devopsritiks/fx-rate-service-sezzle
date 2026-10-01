// Package frankfurter is a thin client for the Frankfurter exchange rate
// API (https://api.frankfurter.app). It is the single seam between this
// service and the upstream vendor: Client.Latest makes exactly one HTTP
// attempt and nothing more. Retries, backoff/jitter, circuit breaking,
// and the concurrency bulkhead live one layer up in
// internal/fxvendor/resilience, which wraps this client. Keeping those
// concerns separate means this package only has to know how to speak
// Frankfurter's API, and the resilience package only has to know how to
// call something fallible — neither has to know about the other's
// internals.
package frankfurter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Client calls the Frankfurter API. It makes a single attempt per call;
// it does not retry or time-bound beyond whatever context it's given —
// that's the resilience layer's job.
type Client struct {
	baseURL string
	http    *http.Client
	// maxResponseBytes caps how much of a response body we'll read, so a
	// misbehaving or compromised upstream can't exhaust our memory by
	// sending a huge (or infinite) body.
	maxResponseBytes int64
}

// Options configures transport-level tuning for calls to Frankfurter.
// These settings exist to stop a slow or flaky vendor from exhausting our
// own process resources (connections, memory) — not to make individual
// calls faster.
type Options struct {
	// PerAttemptDialTimeout bounds TCP connect; the overall per-attempt
	// timeout is still enforced by the context the resilience layer sets,
	// this just keeps a hung dial from tying up a connection slot.
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	MaxResponseBytes    int64
}

// New creates a Frankfurter client with a tuned transport. Idle connection
// limits bound how many sockets we keep open to the vendor at rest;
// IdleConnTimeout recycles connections that have sat unused so we don't
// hold a pool of (possibly half-dead) sockets forever.
func New(baseURL string, opts Options) *Client {
	transport := &http.Transport{
		MaxIdleConns:        opts.MaxIdleConns,
		MaxIdleConnsPerHost: opts.MaxIdleConnsPerHost,
		IdleConnTimeout:     opts.IdleConnTimeout,
	}

	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			// No Timeout set here deliberately: the caller's context
			// (set by the resilience layer's per-attempt timeout) is what
			// bounds this call. A client-level Timeout would apply on top
			// of that and just add a second, less precise deadline.
			Transport: transport,
		},
		maxResponseBytes: opts.MaxResponseBytes,
	}
}

// LatestRates holds the parsed response for a "latest rates" query.
type LatestRates struct {
	Base  string
	Date  string
	Rates map[string]decimal.Decimal
}

// rawLatestResponse mirrors Frankfurter's JSON shape. Frankfurter returns
// rate values as JSON numbers; decoding straight into decimal.Decimal
// keeps us off float64 for the entire pipeline, not just at our own
// arithmetic.
type rawLatestResponse struct {
	Amount decimal.Decimal            `json:"amount"`
	Base   string                     `json:"base"`
	Date   string                     `json:"date"`
	Rates  map[string]decimal.Decimal `json:"rates"`
}

// Latest fetches current rates for base against the given symbols. It is
// exactly one HTTP attempt: it does not retry and relies entirely on ctx
// for cancellation/timeout. symbols may be empty to request all available
// rates.
func (c *Client) Latest(ctx context.Context, base string, symbols []string) (*LatestRates, error) {
	q := url.Values{}
	q.Set("base", base)
	if len(symbols) > 0 {
		q.Set("symbols", strings.Join(symbols, ","))
	}

	reqURL := fmt.Sprintf("%s/latest?%s", c.baseURL, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building upstream request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, newUpstreamStatusError(resp.StatusCode, resp.Header.Get("Retry-After"))
	}

	body := io.LimitReader(resp.Body, c.maxResponseBytes+1)
	var raw rawLatestResponse
	dec := json.NewDecoder(body)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decoding upstream response: %w", err)
	}

	return &LatestRates{
		Base:  raw.Base,
		Date:  raw.Date,
		Rates: raw.Rates,
	}, nil
}

// UpstreamStatusError wraps a non-200 response from the vendor so callers
// can distinguish "vendor said no" (e.g. unknown currency -> 404) from a
// network/transport failure, without leaking raw vendor response bodies.
// RetryAfter is populated when the vendor sent a Retry-After header
// (typically alongside 429); the retry layer honors it over its own
// backoff calculation when present.
type UpstreamStatusError struct {
	StatusCode    int
	RetryAfter    time.Duration
	hasRetryAfter bool
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned status %d", e.StatusCode)
}

// HasRetryAfter reports whether the vendor sent a usable Retry-After header.
func (e *UpstreamStatusError) HasRetryAfter() bool {
	return e.hasRetryAfter
}

func newUpstreamStatusError(code int, retryAfterHeader string) error {
	e := &UpstreamStatusError{StatusCode: code}
	if retryAfterHeader == "" {
		return e
	}
	// Retry-After can be a number of seconds or an HTTP date; we only
	// bother with the seconds form since that's what Frankfurter and most
	// APIs send for 429s.
	if secs, err := time.ParseDuration(retryAfterHeader + "s"); err == nil {
		e.RetryAfter = secs
		e.hasRetryAfter = true
	}
	return e
}

// TransportError wraps a network-level failure (connection refused, DNS
// failure, TLS error, context deadline exceeded, etc.) from the
// underlying http.Client.Do call. The retry layer treats these as
// retryable; UpstreamStatusError for 4xx is not.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("transport error calling upstream: %s", e.Err.Error())
}

func (e *TransportError) Unwrap() error {
	return e.Err
}
