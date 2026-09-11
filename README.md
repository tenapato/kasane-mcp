# Kasane MCP

Kasane is a self-hosted memory service for AI agents. PostgreSQL is the source of truth. Qdrant provides sparse keyword search, with indexing handled in the background. The Go binary exposes the HTTP API and remote MCP endpoint. A separately distributed private web UI can be served by setting `WEB_DIR`.

## Quick start

Install Docker Compose (`curl` and `jq` are also used by the API provisioning example), then create a local environment file:

```sh
cp .env.example .env
```

Replace the sample values with your own secrets. Compose passes `POSTGRES_PASSWORD` separately from the connection URL, so reserved characters such as `/`, `@`, and `:` work without URL encoding. Single-quote values containing `$` in `.env` to prevent Compose interpolation. Start the stack:

```sh
docker compose up --build -d
```

This command builds Kasane and starts PostgreSQL and Qdrant too. Compose sets `DATABASE_URL` and `QDRANT_URL` for the app, and Kasane applies database migrations on startup. You do not need Go installed or separate database installations. Both databases keep their data in named Docker volumes and are accessible only within the Compose network.

Create the first owner. Enter the password without putting it in shell history, pass it only to the one-shot container command, then clear it:

```sh
printf 'Admin password: '; read -r -s KASANE_ADMIN_PASSWORD; printf '\n'
export KASANE_ADMIN_PASSWORD
docker compose exec -e KASANE_ADMIN_PASSWORD app \
  kasane bootstrap --username owner
```

`/healthz` is public. `/readyz` reports PostgreSQL readiness. Stop the services with `docker compose down`; named Postgres and Qdrant volumes keep data.

Create a workspace and an agent key through the API. The key token is returned once, so store it in your secret manager:

```sh
BASE_URL=http://localhost:9090
umask 077
jq -n '{username:"owner",password:env.KASANE_ADMIN_PASSWORD}' | \
  curl -fsS -c cookies.txt -H "Origin: $BASE_URL" -H 'Content-Type: application/json' \
  --data-binary @- "$BASE_URL/api/v1/login" > session.json
CSRF_TOKEN=$(jq -r .csrf_token session.json)
WORKSPACE_ID=$(curl -sS -b cookies.txt -H "Origin: $BASE_URL" \
  -H "X-CSRF-Token: $CSRF_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"default"}' "$BASE_URL/api/v1/workspaces" | jq -r .id)
curl -sS -b cookies.txt -H "Origin: $BASE_URL" \
  -H "X-CSRF-Token: $CSRF_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"agent","scope":"write"}' \
  "$BASE_URL/api/v1/workspaces/$WORKSPACE_ID/keys" | jq
```

Use the returned `token` as `KASANE_MCP_TOKEN`. Keep `session.json` and `cookies.txt` private and remove them after provisioning, then run `unset KASANE_ADMIN_PASSWORD`.

## Run Go directly (optional development setup)

Skip this section when using Docker Compose. To run the Go process outside Docker, provide PostgreSQL and Qdrant instances reachable from your host, set `DATABASE_URL` and `QDRANT_URL`, then run:

```sh
go run ./cmd/kasane migrate
go run ./cmd/kasane serve
```

`serve` also applies migrations automatically; the separate `migrate` command is available when you want to run migrations without starting the server. The bundled Compose databases do not publish host ports by default.

Useful commands are `kasane migrate`, `kasane bootstrap --username NAME`, and `kasane reindex [--workspace UUID]`. The server defaults to `LISTEN_ADDR=:9090`. Set `PUBLIC_URL` to the browser-visible URL. HTTPS is required except for loopback development URLs. HTTPS mode uses secure session cookies. Set `TRUSTED_PROXY_CIDRS` only for proxy networks you control.

## Connect an agent

Create a write or read key through the API or private UI. The token is displayed once. The MCP endpoint is:

```text
https://kasane.example.com/mcp
```

Claude Code supports remote HTTP MCP servers with a bearer header:

```sh
claude mcp add --transport http kasane https://kasane.example.com/mcp \
  --header "Authorization: Bearer $KASANE_MCP_TOKEN"
```

Codex CLI uses `bearer_token_env_var` in `~/.codex/config.toml`:

```toml
[mcp_servers.kasane]
url = "https://kasane.example.com/mcp"
bearer_token_env_var = "KASANE_MCP_TOKEN"
```

Export `KASANE_MCP_TOKEN` before starting either client. These examples use a static bearer token. Kasane does not provide OAuth. Keep tokens out of committed config files.

Call `kasane_help` with `{}` for usage guidance and examples. The MCP tools are `kasane_help`, `kasane_remember`, `kasane_search`, `kasane_get`, `kasane_context`, `kasane_forget`, `kasane_profile`, `kasane_workspaces`, `kasane_workspace_create`, and `kasane_move`. Keys can access one workspace, selected workspaces, or all current and future workspaces. Multi-workspace keys supply `workspace_id` on each memory/profile call; single-workspace keys can omit it. Workspace creation is a separate permission for multi-workspace write keys. See [`docs/api.md`](docs/api.md) for request fields and response shapes.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | required | PostgreSQL connection string |
| `PGUSER`, `PGPASSWORD`, `PGDATABASE` | set by Compose | PostgreSQL credentials and database when omitted from `DATABASE_URL` |
| `QDRANT_URL` | `http://localhost:6333` | Qdrant REST HTTP endpoint, `http://qdrant:6333` in Compose |
| `QDRANT_API_KEY` | empty | Optional Qdrant API key |
| `MCP_PUBLIC_URL` | same as `PUBLIC_URL` | Optional dedicated MCP origin; its root is the MCP endpoint |
| `PUBLIC_URL` | `http://localhost:9090` | Public URL and cookie security mode |
| `LISTEN_ADDR` | `:9090` | HTTP listen address |
| `TRUSTED_PROXY_CIDRS` | empty | CIDRs allowed to provide proxy headers |
| `WEB_DIR` | empty | Optional directory containing the private web UI |
| `KASANE_ADMIN_PASSWORD` | empty | Bootstrap password from environment |
| `KASANE_ADMIN_PASSWORD_FILE` | empty | File containing bootstrap password |

## Development

```sh
make test
make vet
```

Integration tests use `KASANE_TEST_DATABASE_URL` and `KASANE_TEST_QDRANT_URL`. Keep test data isolated from development data. Read [`docs/api.md`](docs/api.md) for the HTTP contract, memory model, indexing behavior, and profile kinds (`memory`, `stack`, `practice`).

In production, put an HTTPS reverse proxy in front of Kasane and set `PUBLIC_URL` to the exact public origin. The proxy must preserve the `Host` header so it matches `PUBLIC_URL`. Set `TRUSTED_PROXY_CIDRS` only to the proxy's network ranges when you need client-IP handling from `X-Forwarded-For`.

Contributions and vulnerability reports are covered by [`CONTRIBUTING.md`](CONTRIBUTING.md) and [`SECURITY.md`](SECURITY.md). Kasane's public MCP/backend code is released under the [MIT License](LICENSE).

## Separate MCP hostname

To use a dedicated hostname, set these origins in your `.env` (replace the example domain):

```dotenv
PUBLIC_URL=https://kasane.example.com
MCP_PUBLIC_URL=https://mcp.kasane.example.com
```

Point both DNS names at your reverse proxy and configure HTTPS for both. Proxy website traffic to Kasane and route `/` on the MCP hostname to the same app port. Preserve the incoming `Host` header. The agent endpoint is `https://mcp.kasane.example.com`; the private panel remains at `https://kasane.example.com/panel`.

When a separate MCP hostname is configured, its root `/` serves MCP and `/mcp` returns 404, while browser/API routes accept only `PUBLIC_URL`. Agent bearer keys are still required. Requests with an Origin header must match the MCP origin. Leaving `MCP_PUBLIC_URL` empty preserves the original single-host setup for local development. Restart the app after changing either setting.

## Store database data on a dedicated disk

For Dokploy, set these in the Compose service's Environment tab:

```dotenv
POSTGRES_STORAGE=/mnt/data2tb/kasane/postgres
QDRANT_STORAGE=/mnt/data2tb/kasane/qdrant
```

Before deploying, run on the Docker host (not inside the app container):

```sh
mountpoint -q /mnt/data2tb && sudo mkdir -p /mnt/data2tb/kasane/postgres /mnt/data2tb/kasane/qdrant
```

Ensure the disk is mounted at `/mnt/data2tb` before Docker starts, including after a reboot. PostgreSQL records and Qdrant indexes will live in those directories. Application images and Docker build caches continue to use Docker's own storage directory. Leave the variables empty for the existing named-volume setup used locally.

Changing these paths does not move existing data. For an existing installation, stop writes and back up/migrate its data before switching mounts; otherwise Kasane will see empty databases. Keep backups on separate storage.

## Invitation-only accounts

The bootstrap account is the instance administrator. Public account creation requires a valid invitation; joining `/waitlist` does not grant access. In the private web panel, the administrator opens **Invitations** to review the queue, issue a link, or revoke an unused invitation. Links expire after seven days and can be used once. Copy the link and send it to the intended recipient yourself; this release does not send email automatically.

Acceptance creates a regular account and a private **Personal** workspace. Users can create additional project workspaces. Workspaces, memories, keys, and usage counts are private to their user. Even the instance administrator cannot browse another user's project data. An agent key with all-workspaces access covers only the key owner's current and future workspaces.

On upgrade, migrations assign existing workspaces to the original administrator and retain existing agent keys. Deploy the backend before the updated web UI, then sign in again or reload to receive your role. No new environment variables are required. Behind a reverse proxy, configure `TRUSTED_PROXY_CIDRS` with only your controlled proxy networks so public signup rate limits use the actual client IP.
