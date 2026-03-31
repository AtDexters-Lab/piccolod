# piccolod

On-device daemon for [Piccolo OS](https://github.com/AtDexters-Lab/piccolo-os) — admin portal, app management, storage, and encrypted control plane.

![Stage: Alpha](https://img.shields.io/badge/Stage-Alpha-orange)
[![Tagged Release](https://github.com/AtDexters-Lab/piccolod/actions/workflows/release.yml/badge.svg)](https://github.com/AtDexters-Lab/piccolod/actions/workflows/release.yml)

> [!IMPORTANT]
> **piccolod runs as part of Piccolo OS.** To try it, flash a Piccolo OS image — see the
> [install guide](https://github.com/AtDexters-Lab/piccolo-os#install-and-quick-start).
> The instructions below are for development.

## What It Does

piccolod is the single process that runs on every Piccolo OS device. It serves the admin portal, manages containerized apps, encrypts all persistent data, and — optionally — connects to a Nexus relay for remote access.

```
  piccolo.local ------> [ Admin Portal ]
                               |
  <app>-piccolo.local > [ Service Proxy ] -- [ App Manager ]
                               |                    |
                       [ mDNS Discovery ]  [ Encrypted Storage ]
                               |
                   outbound tunnel (optional)
                               |
                        [ Nexus Relay ]
```

- **Admin portal** — embedded Flutter web UI for setup, app lifecycle, storage, and device management, served at `http://piccolo.local`.
- **App management** — install, run, and manage containerized apps via rootless Podman. Each app runs as a dedicated Linux system user with isolated storage. Block-native rootfs uses golden LV snapshots instead of container image layers.
- **Encrypted storage** — all persistent data lives in LUKS2-encrypted volumes on an LVM thin pool. A password-derived master key (SDEK, Argon2id) wraps per-volume keys.
- **Service proxying** — each app gets allocated ports with HTTP or raw TCP reverse proxying, and optional per-app auth (header injection or full OIDC flow).
- **Auth & SSO** — password + passkey (WebAuthn) login for the portal. Built-in OIDC identity provider for app SSO — apps can delegate authentication to piccolod.
- **mDNS** — advertises `piccolo.local` and per-app hostnames (`<app>-piccolo.local`) on the LAN. Gateway leader election coordinates which device owns the apex hostname when multiple devices are present.
- **Remote access** — optional, via an outbound tunnel to a [Nexus relay](https://github.com/AtDexters-Lab/nexus-proxy-server). The device never accepts inbound connections — all remote traffic flows through the relay with device-terminated TLS. Managed remote access uses [namek-server](https://github.com/AtDexters-Lab/namek-server) with TPM-attested device identity.

## Build & Run (Development)

Prerequisites: Go (matching `go.mod`) and the Flutter SDK (with web support + Chrome).

```bash
make build          # Build UI + piccolod binary
make run            # Build and run on http://localhost:8080 (requires sudo)
make run-fresh      # Same, but with ephemeral state dir (requires sudo)
make clean          # Clean all build artifacts
```

For UI-only development (see `ui/docs/dev-guide.md`):

```bash
cd ui
flutter run -d chrome --dart-define=API_BASE_URL=http://localhost:8080
```

## Testing

- **Unit tests:** `go test ./...`
- **Integration tests (requires Podman):** `go test ./internal/app -tags=integration`
- See `internal/app/README_TESTING.md` for unit vs integration test details.

## Configuration

piccolod is configured via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `80` | HTTP listen port (`8080` via `make run`) |
| `PICCOLO_CORE_ROOT` | `/piccolo-core` | Core state directory (control store, crypto keys, certs) |
| `PICCOLO_DISABLE_MDNS` | — | Disable mDNS (`1`) |
| `GIN_MODE` | `release` | Gin mode (`debug` for verbose logging) |

See source for additional override variables (`PICCOLO_NAMEK_URL`, `PICCOLO_ACME_DIR_URL`, `PICCOLO_USE_SWTPM`, `PICCOLO_PODMAN_ROOT`, etc.).

## Repository Layout

```
cmd/piccolod/       Entry point
internal/           Core packages: server, app, persistence, auth, remote, services, ...
ui/                 Flutter Web portal
web/                Compiled UI assets (embedded into binary)
docs/               RFCs, architecture docs, testing notes
tools/              Development tooling (remote stack, integration tests)
```

## The Piccolo Ecosystem

| Component | Role |
|-----------|------|
| [piccolo-os](https://github.com/AtDexters-Lab/piccolo-os) | OS images, install guides, and project hub |
| [piccolod](https://github.com/AtDexters-Lab/piccolod) | On-device daemon — portal, app management, encryption |
| [namek-server](https://github.com/AtDexters-Lab/namek-server) | Orchestrator — device auth, DNS, certificates |
| [nexus-proxy-server](https://github.com/AtDexters-Lab/nexus-proxy-server) | Edge relay — remote access with device-terminated TLS |
| [piccolo-store](https://github.com/AtDexters-Lab/piccolo-store) | App catalog — manifests for installable apps |

## License

AGPL-3.0 — see [LICENSE](./LICENSE).
