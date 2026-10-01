# Decision API connection

Kasane can send explicitly selected saved practices to the standalone Rust
relevance API. The connection is optional and disabled by default. Browser and
MCP calls use the same workspace-authorized backend path; the decision token is
never returned to the browser or agent.

The currently available service is a **keyword baseline**, not a trained model.
This integration does not train, promote or change weights. Relevance selection
is not violation detection or a compliance guarantee. Agents must explicitly call
the tool; registering it does not guarantee invocation on every task or edit.

## Connect an existing service

Set on the **Kasane MCP/backend process**, then restart it:

```dotenv
DECISION_API_URL=http://decision-api:8080
DECISION_API_TOKEN=YOUR_EXISTING_DECISION_SERVICE_TOKEN
```

The token must match the decision service's token file (32–4096 printable ASCII
bytes). A local process or custom container secret mount can instead set
`DECISION_API_TOKEN_FILE=/run/secrets/decision_token`; the file takes precedence.
Do not set these variables in the web build or a `VITE_` variable. The origin is
administrator-controlled and accepts no credentials, query string or path.
Redirects are never followed. Requests have a five-second deadline and responses
are bounded to 1 MiB. Operational errors return no selection, not a fallback.

Use HTTPS between hosts, or HTTP only over a trusted private container network
or loopback. Container `localhost` means that container, not the host or another
service. The URL must be reachable from the backend. `/readyz` readiness appears
in the panel, but a successful authenticated relevance call is the credential
check. Readiness alone does not verify the token.

## Run a connected local baseline with Compose

`compose.decision.yaml` adds the decision API to the backend's private Compose
network. It publishes no extra port. Start from this backend directory after
configuring the normal PostgreSQL/public URL variables in `.env`:

1. Create a token in a private directory outside Git (or reuse the existing
   decision token). For a new file only: `mkdir -m 700 secrets`, then
   `openssl rand -hex 32 > secrets/decision_token` and
   `chmod 444 secrets/decision_token`. The parent protects host access while both
   non-root containers can read their mounted file.
2. Set `DECISION_TOKEN_PATH=./secrets/decision_token` in `.env`. Set
   `DECISION_API_SOURCE` to the standalone API checkout if needed; its default
   `../../kasane-decision-api` matches this workspace layout.
3. Run `docker compose -f compose.yaml -f compose.decision.yaml up --build -d`.

The overlay mounts the same token file into both services and sets the backend's
internal origin. The decision service must be healthy before the backend starts.
An existing separately deployed web UI continues to use same-origin `/api`
routing to that backend. No database or Qdrant changes are required.

## Web panel

Deploy this backend before the updated web UI. In a workspace, open **Settings →
Convention relevance**. Select saved practices, enter a task and repository-relative
affected paths, optionally supply context, then choose **Evaluate relevance**.
The list is paginated; selection persists across pages and is limited to 64.
Results include exact practice revisions, inclusion reasons, service mode,
coverage and the decision ID. Coverage applies only to supplied candidates.
Changing the input clears previous results. Nothing is automatically saved as
feedback or shared for training.

Always-include flags and scope patterns are per-request settings. They do not
modify persisted practices. Scope patterns use the Rust API's glob semantics:
`*` stays within one path segment, `**` spans directories. Required and matching
scope inclusion is decided by the Rust service before advisory ranking. If a
request fails, retain conventions; an error does not mean a practice is irrelevant.

## MCP

Use the existing Kasane MCP endpoint and workspace key. Read-only keys can call:

```json
{
  "name": "kasane_relevance",
  "arguments": {
    "workspace_id": "WORKSPACE_UUID",
    "task": "Update HTTP request handling",
    "paths": ["src/http.go"],
    "rules": [
      {"id": "SAVED_PRACTICE_UUID", "required": true, "paths": []}
    ]
  }
}
```

Single-workspace keys may omit `workspace_id`; multi-workspace keys must specify
it. Discover practice IDs with `kasane_search` (`kind=practice`) or the workspace
profile, then read exact records with `kasane_get` when needed. The backend loads
the current content and revisions itself; callers cannot supply replacement text
or read a practice from another workspace. Duplicate IDs, non-practice records,
missing IDs and oversized requests fail as a whole. Practice content is limited
to 8 KiB by the decision API; longer saved records must be edited or excluded
explicitly, never silently truncated.

The result contains the decision API fields plus `rules`, the exact saved records
used for the request. It returns every candidate, including those not selected.
No raw input, credentials or upstream error bodies are added to error messages.

REST equivalents, protected by the usual session/workspace authorization:

- `GET /api/v1/workspaces/{workspace}/decision/status`
- `POST /api/v1/workspaces/{workspace}/decision/relevance` (CSRF required)

The POST body has the same task/paths/context/rules fields as the MCP arguments,
without `workspace_id`. Neither surface changes workspace data or training consent.

## Verification

`go test ./...` covers transport bounds, rejected redirects, safe errors,
malformed responses and cancellation. Set `KASANE_TEST_DATABASE_URL` for the
browser-session/CSRF, workspace isolation and authenticated MCP transport tests.
For the real Rust baseline test also set `KASANE_TEST_DECISION_URL` and
`KASANE_TEST_DECISION_TOKEN`; `TestDecisionLiveService` checks required, matching
scope, keyword and irrelevant cases through the real service. Use a disposable
database: the suite creates isolated schemas and removes them after each test.
