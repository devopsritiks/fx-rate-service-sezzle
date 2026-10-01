-- Idempotent on purpose: CREATE TABLE/INDEX IF NOT EXISTS means running
-- this on every startup is always safe, including against an existing
-- database from a previous run. No migration framework (e.g. golang-
-- migrate, goose) for a single table — one embedded SQL file applied at
-- startup is simpler and sufficient at this scale; a second table would
-- be the point to reconsider that.
--
-- money/rates are NUMERIC, never FLOAT: this is the same reasoning as
-- internal/money using shopspring/decimal instead of float64 everywhere
-- else in this service — binary floating point cannot represent most
-- decimal fractions exactly, which is unacceptable for a financial audit
-- trail. NUMERIC in Postgres is arbitrary-precision base-10, matching
-- decimal.Decimal's own representation.
--
-- Deliberately NOT stored: client IP. See internal/audit/record.go for
-- why, and the README for the fuller tradeoff discussion (hash vs. skip).
CREATE TABLE IF NOT EXISTS audit_log (
    id               BIGSERIAL PRIMARY KEY,
    request_id       TEXT NOT NULL,
    occurred_at      TIMESTAMPTZ NOT NULL,
    endpoint         TEXT NOT NULL,       -- route pattern, e.g. "/v1/convert" — same cardinality-safe value used in metrics labels
    base_currency    TEXT,                -- /v1/rates: the base currency queried
    from_currency    TEXT,                -- /v1/convert: source currency
    to_currency       TEXT,                -- /v1/convert: destination currency
    amount           NUMERIC,             -- /v1/convert: requested amount
    rate             NUMERIC,             -- the exchange rate applied
    converted_amount NUMERIC,             -- /v1/convert: the computed result
    cache_status     TEXT,                -- HIT / MISS / STALE
    stale            BOOLEAN NOT NULL DEFAULT FALSE,
    http_status      INTEGER NOT NULL,
    error_code       TEXT,                -- our own error code (e.g. CIRCUIT_OPEN), null on success
    latency_ms       INTEGER NOT NULL
);

-- The expected query pattern for an audit log is "show me everything in
-- this time range" (an auditor, a compliance report, an incident
-- investigation) — a single index on occurred_at covers that directly.
CREATE INDEX IF NOT EXISTS idx_audit_log_occurred_at ON audit_log (occurred_at);
