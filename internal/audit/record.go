// Package audit provides an async, best-effort audit log of every
// request this service handles, written to Postgres off the request's
// hot path. See Writer's doc comment for the full design and the
// drop-under-backpressure tradeoff this phase deliberately makes.
package audit

import "time"

// Record is one audited request. Every field here ends up as one row in
// the audit_log table (see schema.sql) — field-for-field, so there is no
// separate mapping step to keep in sync.
//
// Deliberately NOT included: client IP. An IP address (hashed or not) is
// still personal data under most privacy frameworks once it can be
// correlated across rows to "the same visitor" over time — which is true
// whether it's stored in the clear or as an unsalted/salted hash, since
// the hash is only ever applied to a tiny space (IPv4 is ~4 billion
// values, trivially rainbow-tabled) and a salt just relocates the
// problem to "where do we store the salt, and does rotating it break
// historical correlation anyway." request_id already gives full
// traceability back to this service's own structured logs (which have
// their own, shorter retention) for the rare case an investigation
// genuinely needs network-level detail. The audit table's job is
// "what rate did we serve, when, and under what conditions" — not "who
// asked for it" — so leaving IP out entirely is both simpler and a
// smaller privacy footprint than hashing it would be.
type Record struct {
	RequestID string
	// OccurredAt is when the request was handled, not when this record is
	// eventually written — the whole point of async writing is that these
	// two times can differ by up to AUDIT_FLUSH_INTERVAL or more under
	// backpressure, and the audit record should reflect reality, not
	// write time.
	OccurredAt time.Time
	// Endpoint is the same bounded route-pattern string used for metrics
	// labels (e.g. "/v1/convert", "/v1/rates/{base}") — see
	// internal/httpapi's routePattern — not the raw request path.
	Endpoint string

	BaseCurrency string // /v1/rates
	FromCurrency string // /v1/convert
	ToCurrency   string // /v1/convert

	// Amount, Rate, and ConvertedAmount are decimal strings (as produced
	// by decimal.Decimal.String()), not float64 — see schema.sql for why
	// the column type is NUMERIC. Empty string means "not applicable for
	// this endpoint," stored as SQL NULL.
	Amount          string
	Rate            string
	ConvertedAmount string

	CacheStatus string // "HIT" / "MISS" / "STALE" / "" (e.g. a validation error before any cache lookup)
	Stale       bool

	HTTPStatus int
	ErrorCode  string // our own error code (e.g. "CIRCUIT_OPEN"), empty on success

	LatencyMS int64
}
