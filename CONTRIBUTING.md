# Contributing to Kasane

The public Kasane repository contains the Go backend and MCP server. Keep changes focused and describe user-visible behavior in pull requests. The web UI is privately distributed and is not part of this repository's contribution workflow.

Before opening a pull request, run:

```sh
go test ./...
go vet ./...
```

The module targets Go 1.26 and the production image compiles with Go 1.27.1. Keep Go dependencies pinned. Do not commit `.env`, credentials, API keys, generated files, or private UI sources.

Integration tests use PostgreSQL and Qdrant. Set `KASANE_TEST_DATABASE_URL` and `KASANE_TEST_QDRANT_URL` when running them locally, with an isolated database or schema. Store writes commit to PostgreSQL before indexing, so tests should assert durable state separately from eventual search state.

The HTTP contract and MCP behavior are documented in [`docs/implementation.md`](docs/implementation.md). Update it when an endpoint, tool, environment variable, or operational assumption changes. Report security issues privately as described in [`SECURITY.md`](SECURITY.md).
