CREATE TABLE IF NOT EXISTS workspace_usage_daily(
 workspace_id uuid NOT NULL REFERENCES workspaces(id),
 day date NOT NULL,
 retrievals bigint NOT NULL DEFAULT 0,
 baseline_tokens bigint NOT NULL DEFAULT 0,
 returned_tokens bigint NOT NULL DEFAULT 0,
 saved_tokens bigint NOT NULL DEFAULT 0,
 PRIMARY KEY(workspace_id,day)
);
