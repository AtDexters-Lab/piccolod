[![Tagged Release](https://github.com/AtDexters-Lab/piccolod/actions/workflows/release.yml/badge.svg)](https://github.com/AtDexters-Lab/piccolod/actions/workflows/release.yml)

# Piccolo OS – `piccolod`

This repository contains `piccolod`, the control-plane daemon and embedded web portal for Piccolo OS: a headless home/edge OS that turns spare hardware into a dependable, always-on sandbox at `http://piccolo.local`.

## What Piccolo OS Provides
- **Local-first appliance** – fully usable on a LAN with no cloud dependency; remote access is optional and self-hosted.
- **Admin portal** – responsive web UI for setup, storage, app lifecycle, cluster status, and updates.
- **Container orchestration** – `piccolod` manages user and system apps as containers, with device-terminated TLS and proxying.
- **Storage & clustering** – in-process storage orchestration (AionFS) and optional multi-node clusters with fast failover.

## Repository Layout
- `cmd/piccolod` – Go entrypoint for the daemon.
- `internal/` – core packages (API, runtime, persistence, auth, cluster, etc.).
- `ui-next/` – SvelteKit + TypeScript portal UI, built into `web/` and embedded by `web_embed.go`.
- `docs/` – technical docs, runtime and testing notes, RFCs.

## Build & Run (Local)
Prerequisites: Go (matching `go.mod`), Node.js 20+, npm.

```bash
make build        # build UI + piccolod binary
make run          # run on http://localhost:8080 with local state
```

For UI-only development:

```bash
cd ui-next
npm ci
npm run dev
```

## Testing
- Go unit tests: `go test ./...`
- App manager tests: see `internal/app/README_TESTING.md` for unit vs integration (Podman) suites.
- Portal E2E tests: `make e2e` (Playwright) and `docs/testing/e2e-policy.md` for lanes and env flags.
