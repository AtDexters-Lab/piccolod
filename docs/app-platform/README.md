# Piccolo OS App Platform

The Piccolo OS App Platform enables users to easily install, manage, and run containerized applications with a mobile OS-like experience.

## Quick Start

### Minimal App
```yaml
type: user
inputs:
  __app_address__:
    type: string
    label: "App Address"
    required: true
    validation:
      regex: "^[a-z][a-z0-9]{0,15}$"
      message: "Lowercase letters and numbers only; max 16 chars"
listeners:
  - name: __primary
    guest_port: 2368
    flow: tcp
    protocol: http
services:
  main:
    image: ghost:latest
    bind_ports: [2368]
x-piccolo:
  mode: service
```

## Documentation

- **[creator-guide.md](./creator-guide.md)** - Guide for creators converting existing SaaS or SOHO apps into Custom Apps
- **[specification.yaml](./specification.yaml)** - Complete app.yaml specification with all fields documented
- **[examples/](./examples/)** - Common patterns and use cases

## Key Features

### 🏗️ **Flexible Container Sources**
- **Registry images**: Public Docker Hub, GitHub Container Registry, or similar OCI images

### 🔒 **Security Controls**
- **Network controls**: Apps can explicitly allow or deny container internet access
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
| **Developer workstation** | [development.yaml](./examples/development.yaml) | Workspace persistence and generated access password |
| **Web service** | [web-service.yaml](./examples/web-service.yaml) | Health checks and remote publish |

## API Integration

Apps with `__primary` listeners need install inputs so Piccolo can replace the
`__primary` marker with the chosen app address. First parse the manifest and
collect the input schema:

```bash
curl -X POST /api/v1/apps/configure \
  -H "Content-Type: application/x-yaml" \
  --data-binary @app.yaml
```

Then install with a JSON request that includes both the manifest and input
values:

```json
{
  "app_definition": "<contents of app.yaml>",
  "inputs": {
    "__app_address__": "blog"
  }
}
```

```bash
curl -X POST /api/v1/apps \
  -H "Content-Type: application/json" \
  --data-binary @install.json
```

The raw YAML install form is only suitable for manifests that do not require
inputs.

### Custom App Manifest Updates

Installed custom service-mode apps can apply topology-neutral YAML wiring
changes without uninstalling. The update flow is explicit: prepare the
manifest, re-enter or regenerate any password/generated inputs, dry-run the
diff, then apply the confirmed candidate.

V1 supports existing-service environment changes and additive storage mounts
against the same service images, rootfs volumes, listener topology, and app
identity. It rejects catalog-backed apps, disabled apps, workspace apps, image
reference changes, listener/auth changes, service additions/removals, storage
renames/removals, resource/app_config/auth/healthcheck changes, service-level
OIDC clients, and init-script drift.

For apps that also need new image bits, run the image update path first while
keeping manifest image references stable, then apply the custom manifest YAML
for non-image wiring.

## Development

The app platform is implemented in the `piccolod` daemon using:
- **Podman** for OCI-compliant container runtime
- **systemd** for service lifecycle management  
- **Filesystem state** for app metadata persistence (JSON under `$PICCOLO_STATE_DIR`)
- **Federated storage** for cross-device data sync

See [../../pre-beta-prd.md](../../pre-beta-prd.md) for current scope and architecture notes.
