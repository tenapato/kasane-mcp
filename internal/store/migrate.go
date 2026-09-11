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
	// Keep upgrades safe for databases initialized by older versions. The
	// schema file carries the same definitions for fresh databases.
	if _, e = tx.Exec(ctx, `ALTER TABLE agent_keys ADD COLUMN IF NOT EXISTS access_mode text NOT NULL DEFAULT 'single'`); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `ALTER TABLE agent_keys ADD COLUMN IF NOT EXISTS can_create_workspaces boolean NOT NULL DEFAULT false`); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='agent_keys'::regclass AND conname='agent_keys_access_mode_check') THEN
			ALTER TABLE agent_keys ADD CONSTRAINT agent_keys_access_mode_check CHECK(access_mode IN ('single','selected','all'));
		END IF;
	END $$`); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS agent_key_workspaces(key_id uuid NOT NULL REFERENCES agent_keys(id) ON DELETE CASCADE,workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,PRIMARY KEY(key_id,workspace_id))`); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO agent_key_workspaces(key_id,workspace_id) SELECT id,workspace_id FROM agent_keys WHERE workspace_id IS NOT NULL ON CONFLICT DO NOTHING`); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO schema_version(version) VALUES(2) ON CONFLICT DO NOTHING`); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
