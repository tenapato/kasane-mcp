package store

import (
	"context"
	_ "embed"
)

//go:embed schema.sql
var schema string

func (s *Store) Migrate(ctx context.Context) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(918273645)`); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_version(version integer PRIMARY KEY)`); e != nil {
		return e
	}
	var version *int
	if e = tx.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); e != nil {
		return e
	}
	if version == nil {
		if _, e = tx.Exec(ctx, schema); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
