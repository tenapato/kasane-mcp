CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS schema_version(version integer PRIMARY KEY);
CREATE TABLE IF NOT EXISTS workspaces(
 id uuid PRIMARY KEY, name text NOT NULL, created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS memories(
 kind text NOT NULL DEFAULT 'memory' CHECK(kind IN ('memory','stack','practice')),
 id uuid PRIMARY KEY, workspace_id uuid NOT NULL REFERENCES workspaces(id),
 title text NOT NULL, content text NOT NULL, tags text[] NOT NULL DEFAULT '{}', source text NOT NULL DEFAULT '',
 revision bigint NOT NULL DEFAULT 1, indexed_revision bigint NOT NULL DEFAULT 0,
 idempotency_key text, deleted boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 search_vector tsvector NOT NULL DEFAULT ''::tsvector,
 UNIQUE(workspace_id,idempotency_key));
CREATE INDEX IF NOT EXISTS memories_workspace_updated ON memories(workspace_id,updated_at DESC,id DESC);
CREATE OR REPLACE FUNCTION memories_search_vector_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.search_vector := to_tsvector('simple', concat_ws(' ',NEW.title,NEW.content,array_to_string(NEW.tags,' '))); RETURN NEW; END; $$;
DROP TRIGGER IF EXISTS memories_search_vector_trigger ON memories;
CREATE TRIGGER memories_search_vector_trigger BEFORE INSERT OR UPDATE OF title,content,tags ON memories FOR EACH ROW EXECUTE FUNCTION memories_search_vector_update();
UPDATE memories SET search_vector=to_tsvector('simple', concat_ws(' ',title,content,array_to_string(tags,' ')));
CREATE INDEX IF NOT EXISTS memories_search ON memories USING gin(search_vector);
CREATE TABLE IF NOT EXISTS outbox(
 memory_id uuid PRIMARY KEY REFERENCES memories(id), event_kind text NOT NULL CHECK(event_kind IN ('upsert','delete')),
 revision bigint NOT NULL, attempts integer NOT NULL DEFAULT 0, last_error text NOT NULL DEFAULT '',
 next_attempt timestamptz NOT NULL DEFAULT now(), created_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS outbox_due ON outbox(next_attempt,created_at);
CREATE TABLE IF NOT EXISTS owners(username text PRIMARY KEY,password_hash text NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(token_hash text PRIMARY KEY,username text REFERENCES owners(username),csrf_token text NOT NULL,expires_at timestamptz NOT NULL);
CREATE TABLE IF NOT EXISTS agent_keys(id uuid PRIMARY KEY,workspace_id uuid REFERENCES workspaces(id),name text NOT NULL,prefix text NOT NULL,token_hash text UNIQUE NOT NULL,scope text NOT NULL CHECK(scope IN ('read','write')),created_at timestamptz NOT NULL DEFAULT now(),last_used_at timestamptz,revoked_at timestamptz);
INSERT INTO schema_version(version) VALUES(1) ON CONFLICT DO NOTHING;
