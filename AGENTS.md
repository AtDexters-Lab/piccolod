# Repository Guidelines

This repository contains the `piccolod` Go backend and the SvelteKit UI that is built into `./web` and embedded via `web_embed.go`.

## Project Structure & Modules
- Backend: Go entrypoint in `cmd/piccolod`, domain packages in `internal/*`, shared test fixtures in `testdata/`.
- UI: `ui-next/` (Svelte + TypeScript); built assets land in `web/`.
- Documentation: product specs in `02_product/`, technical docs and RFCs in `docs/`.
- Packaging and tooling: release tooling in `packaging/`, auxiliary services in `tools/`.

## Build, Test, and Development
- `make build` – build the Go server and bundle the UI into `./piccolod`.
- `make run` – build and run locally on `http://localhost:8080` with state in `./run-state`.
- `cd ui-next && npm run dev` – run the UI in dev mode.
- `go test ./...` – run the Go test suite; use `go test ./internal/app -tags=integration` for Podman-based integration tests.
- `make e2e` – build and run Playwright end‑to‑end tests in `ui-next/tests`.

## Coding Style & Naming
- Go: follow `gofmt`; keep package names short and lower‑case (for example `internal/app`, `internal/router`); exported identifiers use `CamelCase`, unexported use `camelCase`.
- UI: prefer 2‑space indentation, Svelte components in `ui-next/src/lib/components`, and route files under `ui-next/src/routes`. All UI contributors must read, internalise, and adhere to `ui-next/docs/foundation.md` before making changes.
- Keep configuration in env vars (for example `PORT`, `PICCOLO_STATE_DIR`), not hard‑coded.

## Testing Guidelines
- New Go code should include `*_test.go` files with `TestXxx` functions in the same package.
- UI and flow tests live in Playwright specs (`*.spec.ts`) under `ui-next/tests`; follow the patterns in existing specs.
- For E2E behavior and lanes, align with `docs/testing/e2e-policy.md`.

## Commit & Pull Request Guidelines
- Use concise, imperative commit subjects that reference the area touched (for example `internal/app: add app manager tests`).
- PRs should describe the change, link to any relevant docs in `docs/` or `02_product/`, and list how it was tested (for example `go test ./...`, `make e2e`, manual UI checks with `make run`).
- Include screenshots or short notes for user‑visible UI changes.

## Agent-Specific Instructions
- Prefer minimal, focused changes; avoid drive‑by refactors.
- Use the `Makefile` and existing scripts where possible, and keep docs in sync when behavior changes.
