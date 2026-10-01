package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig controls pgxpool sizing and timeouts. Every field is
// env-driven (see internal/config) — nothing here is hardcoded.
type PoolConfig struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
}

// NewPool builds a pgxpool.Pool from cfg. It does NOT ping or otherwise
// block on the database being reachable — pgxpool connects lazily, which
// is exactly what we want: fxservice must start and begin serving
// traffic even if Postgres is down at boot (see the package doc comment
// on /ready and the "never block the API" design). Callers that do want
// an eager reachability check (e.g. for a one-time startup log line) can
// call pool.Ping themselves with their own timeout.
func NewPool(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parsing postgres DSN: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating postgres pool: %w", err)
	}
	return pool, nil
}

// pgSink writes audit batches to Postgres via pgx's CopyFrom, which is
// the efficient path for inserting many rows in one round trip — no ORM,
// no per-row prepared-statement round trips, just a COPY protocol
// bulk-load of exactly the rows in the batch.
type pgSink struct {
	pool *pgxpool.Pool
}

// NewPgSink wraps pool as an audit.Sink.
func NewPgSink(pool *pgxpool.Pool) Sink {
	return &pgSink{pool: pool}
}

var auditColumns = []string{
	"request_id", "occurred_at", "endpoint",
	"base_currency", "from_currency", "to_currency",
	"amount", "rate", "converted_amount",
	"cache_status", "stale",
	"http_status", "error_code", "latency_ms",
}

func (s *pgSink) WriteBatch(ctx context.Context, records []Record) error {
	if len(records) == 0 {
		return nil
	}

	rows := make([][]any, len(records))
	for i, r := range records {
		amount, err := nullableNumeric(r.Amount)
		if err != nil {
			return fmt.Errorf("record %d: amount %q: %w", i, r.Amount, err)
		}
		rate, err := nullableNumeric(r.Rate)
		if err != nil {
			return fmt.Errorf("record %d: rate %q: %w", i, r.Rate, err)
		}
		converted, err := nullableNumeric(r.ConvertedAmount)
		if err != nil {
			return fmt.Errorf("record %d: converted_amount %q: %w", i, r.ConvertedAmount, err)
		}

		rows[i] = []any{
			r.RequestID, r.OccurredAt, r.Endpoint,
			nullableString(r.BaseCurrency), nullableString(r.FromCurrency), nullableString(r.ToCurrency),
			amount, rate, converted,
			nullableString(r.CacheStatus), r.Stale,
			r.HTTPStatus, nullableString(r.ErrorCode), r.LatencyMS,
		}
	}

	_, err := s.pool.CopyFrom(
		ctx,
		pgx.Identifier{"audit_log"},
		auditColumns,
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("writing audit batch (%d records): %w", len(records), err)
	}
	return nil
}

// nullableString converts an empty string to a SQL NULL rather than
// storing an empty string — e.g. BaseCurrency is meaningless for a
// /v1/convert record, and NULL says "not applicable" instead of implying
// an empty currency code was actually recorded.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableNumeric converts a decimal string (as produced by
// decimal.Decimal.String() — see internal/money) into a pgtype.Numeric.
//
// This indirection matters: pgx's CopyFrom protocol requires every value
// be binary-encoded, and Postgres's binary NUMERIC format is a packed
// digit-group representation, not UTF-8 text — handing CopyFrom a plain
// Go string for a NUMERIC column fails, because there's no binary
// encoding from "string" to "numeric" registered. pgtype.Numeric.Scan
// parses the decimal string into that binary-ready representation, so
// the exact value internal/money computed lands in the column unchanged.
func nullableNumeric(s string) (any, error) {
	if s == "" {
		return nil, nil
	}
	var n pgtype.Numeric
	if err := n.Scan(s); err != nil {
		return nil, fmt.Errorf("parsing %q as numeric: %w", s, err)
	}
	return n, nil
}
