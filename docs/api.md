# Kasane API

The HTTP API uses JSON and is rooted at `/api/v1`. Error responses have the shape `{ "error": "human-readable message" }`. IDs are UUID strings and timestamps are RFC3339 values. Collection fields are empty arrays rather than `null`.

## Owner session

`POST /api/v1/login` accepts `{ "username": string, "password": string }`. It requires an `Origin` header equal to `PUBLIC_URL`. On success it sets the `kasane_session` HttpOnly cookie and returns `{ "username": string, "csrf_token": string }`.

`GET /api/v1/session` requires the session cookie and returns the username and CSRF token. `POST /api/v1/logout` requires the cookie, matching `Origin`, and `X-CSRF-Token`; it revokes the session and returns `{ "ok": true }`.

All authenticated state-changing owner requests require both the matching `Origin` and `X-CSRF-Token`. Read requests require only the session cookie.

## Workspaces and keys

`GET /api/v1/workspaces` returns `{ "workspaces": Workspace[] }`.

`POST /api/v1/workspaces` accepts `{ "name": string }` and returns a `Workspace` with status 201.

`POST /api/v1/workspaces/{workspace}/keys` accepts `{ "name": string, "scope": "read" | "write", "access_mode": "single" | "selected" | "all", "workspace_ids": string[], "can_create_workspaces": boolean }`. Access defaults to `single` and workspace creation to false. For `selected`, pass workspace IDs (maximum 500); the home workspace in the URL is always included. For other modes omit `workspace_ids`. Creation permission requires a multi-workspace write key. `all` includes future workspaces; `selected` additionally gains any workspaces the key creates. Keys are listed and revoked from their home workspace. Returned key metadata includes these access fields. It returns status 201 with `{ "key": Key, "token": string }`. The token is shown only in this response. `GET` on the same path returns `{ "keys": Key[] }`. `DELETE /api/v1/workspaces/{workspace}/keys/{key}` revokes a key and returns `{ "ok": true }`.

## Memories

`POST /api/v1/workspaces/{workspace}/memories` creates a memory. Its body is `{ "title": string, "content": string, "tags": string[], "source": string, "kind": "memory" | "stack" | "practice", "idempotency_key": string }`. `kind` defaults to `memory`; `tags`, `source`, and `idempotency_key` are optional. It returns a `Memory` with status 201.

`GET /api/v1/workspaces/{workspace}/memories` supports `q`, `tag`, `kind`, `limit` (default 50), and `offset` (default 0). Empty `q` lists recently updated memories. It returns `{ "memories": Memory[], "degraded": boolean, "total": number }`.

`GET /api/v1/workspaces/{workspace}/memories/{memory}` returns the current memory. `PUT` accepts the create fields plus `expected_revision` and returns the updated memory. A stale revision returns 409; a missing revision is invalid input (400). `DELETE` returns `{ "ok": true }` and removes the memory from live retrieval immediately; vector cleanup is asynchronous.

A memory includes `id`, `workspace_id`, `kind`, `title`, `content`, `tags`, `source`, `revision`, `indexed_revision`, `indexing_status`, `created_at`, and `updated_at`. Content is preserved verbatim. Titles are limited to 240 bytes, content to 65,536 bytes, and tags to 32 entries of 64 bytes each. The Development profile consists of `stack` and `practice` entries, with a combined maximum of 100 per workspace. Profile entries are searchable and are returned together by `kasane_profile`.

List results report the total matching database count. Ranked Qdrant search results report the number of returned hits in `total`; use `limit` and `offset` for bounded pagination. No semantic model or AI provider is used. Newly written entries may remain `pending` until the worker indexes them; listing and direct reads are immediate.

`POST /api/v1/workspaces/{workspace}/reindex` queues live memories for indexing. `GET /api/v1/status` returns PostgreSQL and Qdrant state, queue counts, memory count, and `mcp_url`.

## Health

`GET /healthz` is public and returns `{ "status": "ok" }`. `GET /readyz` checks PostgreSQL and returns 503 while it is unavailable. The container healthcheck calls `kasane healthcheck`, which checks `/healthz` on `LISTEN_ADDR`.

## MCP

Connect an MCP client to the root of the dedicated `MCP_PUBLIC_URL` hostname, or `/mcp` for single-host deployments, using `Authorization: Bearer <agent-token>`. Read keys may call retrieval tools; write keys are required for remember, forget, and move. Agents cannot manage keys or delete workspaces.

Use `kasane_workspaces` with `{}` to list accessible workspaces and key permissions. Multi-workspace keys must pass `workspace_id` on every memory, search, context, or profile call. Single-workspace keys can omit it, but cannot select another workspace. No active workspace is stored between calls.

`kasane_workspace_create` accepts `{ "name": string }` and returns the created workspace. It requires the separate creation permission on a multi-workspace write key. Creation and granting access are atomic. Creation is not idempotent: after an ambiguous response, list workspaces before retrying.

`kasane_move` accepts `{ "workspace_id": string, "target_workspace_id": string, "id": string, "expected_revision": integer }`. Both projects must be writable by the key. It preserves ID, authored fields, and creation time, increments revision, clears the original create idempotency key, and queues vector reindexing. The source immediately loses access. After an ambiguous response, read the memory in the destination before retrying.

Call `kasane_help` with `{}` for the complete guide. Available tools are `kasane_help`, `kasane_workspaces`, `kasane_workspace_create`, `kasane_move`, `kasane_remember`, `kasane_search`, `kasane_get`, `kasane_context`, `kasane_forget`, and `kasane_profile`. Search is keyword based. `kasane_context` and `kasane_profile` return exact authored text, provenance IDs, character counts, and truncation status. Their character budget defaults to 12,000 and has a maximum of 40,000. Indexing is asynchronous, so writes are durable before they become searchable.
