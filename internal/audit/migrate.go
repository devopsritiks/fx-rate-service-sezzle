package audit

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaSQL is embedded at build time so the binary carries its own
// schema — no separate migration file to ship, mount, or get out of sync
// with the code that depends on it. The SQL itself is idempotent
// (CREATE TABLE/INDEX IF NOT EXISTS), so running Migrate on every
// startup, against a database that already has the schema applied, is
// always safe.
//
//go:embed schema.sql
var schemaSQL string

// Migrate applies schema.sql. Safe to call every time the process
// starts, including against a database that already has the schema.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("applying audit schema: %w", err)
	}
	return nil
}
