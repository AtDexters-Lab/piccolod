# Piccolo OS App Platform

The Piccolo OS App Platform enables users to easily install, manage, and run containerized applications with a mobile OS-like experience.

## Quick Start

### Minimal App
```yaml
name: blog
image: ghost:latest
listeners:
  - name: web
    guest_port: 2368
    flow: tcp
    protocol: http
x-piccolo:
  mode: service
```

## Documentation

- **[specification.yaml](./specification.yaml)** - Complete app.yaml specification with all fields documented
- **[examples/](./examples/)** - Common patterns and use cases

## Key Features

### 🏗️ **Flexible Container Sources**
- **Registry images**: Docker Hub, GitHub Container Registry, private registries

### 🔒 **Security by Default**
- **Network isolation**: No internet access by default
- **Encrypted app storage**: Each app gets an isolated encrypted volume under `$PICCOLO_STATE_DIR`
- **Permission model**: Granular control over resources and capabilities
- **Federated storage**: Hot/cold tier policies replicate app state across devices

### ⚡ **Developer Experience**
- **Progressive complexity**: Start simple, add features as needed
- **Smart defaults**: Minimal configuration for common cases
- **Extensibility**: Apps can read their own config for custom behavior

## Architecture

```
Container Sources:
├── Registry Images (ghost:latest, nginx:alpine)

Storage Architecture:
├── State root ($PICCOLO_STATE_DIR; default: /var/lib/piccolod)
└── Per-app encrypted volume ($PICCOLO_STATE_DIR/mounts/app-<name>/)
    ├── data/<volume>  (storage.persistent) → hot/cold replication per policy
    └── disk/          (Podman --root: overlay + metadata + logs) → roaming in workspace mode

Runtime:
├── Podman containers (rootless, daemonless; storage relocated into encrypted volumes)
├── systemd integration (proper lifecycle)
└── mDNS discovery (app.piccolo.local)
```

## Examples by Use Case

| Use Case | Example | Highlights |
|----------|---------|------------|
| **Developer workstation** | [development.yaml](./examples/development.yaml) | Multiple listeners, persistent volumes |
| **Web service** | [web-service.yaml](./examples/web-service.yaml) | Health checks and remote publish |

## API Integration

Apps are deployed via HTTP API with flexible upload methods:

```bash
# Method 1: Inline YAML
curl -X POST /api/v1/apps \
  -H "Content-Type: application/yaml" \
  --data-binary @app.yaml
```

## Development

The app platform is implemented in the `piccolod` daemon using:
- **Podman** for OCI-compliant container runtime
- **systemd** for service lifecycle management  
- **Filesystem state** for app metadata persistence (JSON under `$PICCOLO_STATE_DIR`)
- **Federated storage** for cross-device data sync

See [../../pre-beta-prd.md](../../pre-beta-prd.md) for current scope and architecture notes.
