# Repository Guidelines

This repository contains `piccolod`, the control-plane daemon and embedded web portal for Piccolo OS: a headless home/edge OS that provides container orchestration, storage management, and a web admin portal accessible at `http://piccolo.local`.

**Tech Stack:**
- Backend: Go (1.24+) with Gin web framework
- Frontend: Flutter Web embedded in the binary
- Containers: Podman (rootless)

## Project Structure & Modules
- Backend: Go entrypoint in `cmd/piccolod`, domain packages in `internal/*`, shared test fixtures in `testdata/`.
- UI: `ui/` (Flutter Web); built assets land in `web/` and are embedded via `web_embed.go`.
- Documentation: technical docs and RFCs in `docs/`.
- Packaging and tooling: release tooling in `packaging/`, auxiliary services in `tools/`.

## Build, Test, and Development

### Building
```bash
make build          # Build UI + server binary
make run            # Build and run on http://localhost:8080 (state in .run-state/)
make run-fresh      # Build and run with ephemeral state dir (cleanup on exit)
make clean          # Clean all build artifacts
```

### UI Development
```bash
cd ui
flutter run -d chrome --dart-define=API_BASE_URL=http://localhost:8080
```

**Architecture:** Adaptive Shells pattern with ChangeNotifier controllers. Logic in controllers, views are reactive via ListenableBuilder. Read `ui/docs/foundation.md` before making UI changes.

### Testing

**Go Unit Tests:**
```bash
go test ./...                                    # All packages
go test ./internal/app -short                    # Fast unit tests only
go test ./internal/some/package -run TestSpecificName -v
```

**Integration Tests (requires Podman):**
```bash
go test ./internal/app -tags=integration
go test ./internal/app -tags=integration -run TestAppManager_FullLifecycle
```

**Test Environment Variables:**
- `PICCOLO_ALLOW_UNMOUNTED_TESTS=1` - Allow tests without mounted volumes

**E2E Tests:**
- `make e2e` – Playwright end-to-end tests
- See `docs/testing/e2e-policy.md` for lanes and execution details

### Environment Variables
- `PORT` (default: 80) - HTTP server port
- `PICCOLO_STATE_DIR` (default: /var/lib/piccolod) - State directory
- `PICCOLO_DISABLE_MDNS` - Disable mDNS discovery
- `GIN_MODE` (default: release) - Gin framework mode

## Key Architecture Patterns

### Supervisor Pattern
All core services register with `internal/runtime/supervisor/` for coordinated Start/Stop lifecycle. Failure triggers rollback.

### Filesystem State Management
App state persists as JSON files via `FilesystemStateManager` in `internal/app/filesystem.go`. No database.

### Per-App Podman Isolation
Each app gets isolated Podman storage (Root, RunRoot, Imagestore) to avoid image conflicts. See `internal/app/podman_runtime.go`.

### Event Bus
In-process pub-sub in `internal/events/`. Topics: lock state, leadership, device events, exports, audit.

### Service Proxying
`internal/services/` handles port allocation and HTTP/TLS proxying. Endpoints have guest ports (container), host-bind (127.0.0.1), and public ports (0.0.0.0).

### Encrypted Control Volume
Sensitive control plane data uses gocryptfs encryption. Keys managed via `internal/crypt/`.

## Important Entry Points & Paths
- `cmd/piccolod/main.go` - Entry point
- `internal/server/gin_server.go` - HTTP server, routes, middleware
- `internal/app/app_manager.go` - App lifecycle (install/start/stop/uninstall)
- `internal/container/podman.go` - Podman CLI wrapper
- `internal/persistence/service.go` - Storage orchestration
- `internal/services/manager.go` - Port allocation and proxying

## Coding Style & Naming
- Go: follow `gofmt`; keep package names short and lowercase (for example `internal/app`, `internal/router`); exported identifiers use `CamelCase`, unexported use `camelCase`.
- Go: use interfaces for abstraction and testability
- UI: prefer 2-space indentation. All UI contributors must read, internalize, and adhere to `ui/docs/foundation.md` before making changes.
- Keep configuration in env vars (for example `PORT`, `PICCOLO_STATE_DIR`), not hard-coded.

## Testing Guidelines
- New Go code should include `*_test.go` files with `TestXxx` functions in the same package.
- For E2E behavior and lanes, align with `docs/testing/e2e-policy.md`.

## Commit & Pull Request Guidelines
- Use concise, imperative commit subjects that reference the area touched (for example `internal/app: add app manager tests`).
- PRs should describe the change, link to any relevant docs in `docs/`, and list how it was tested (for example `go test ./...`, `make e2e`, manual UI checks with `make run`).
- Include screenshots or short notes for user-visible UI changes.

# Agent Instructions

## Commit Messages
- Don't add authoring
- Should be succinct

## Log parsing helpers
- for piccolod log parsing, to filter out mDNS noise - `grep -v -e "Announced" -e "PTR record" -e "peer discovery" -e "non-local query" -e "query from" -e "self-response" -e "\"/assets/" <log-file>`