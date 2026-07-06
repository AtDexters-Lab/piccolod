# Creator Guide: Custom Apps on Piccolo

This guide is for app creators who already build SaaS or small-business
software and want to make those apps installable on a private Piccolo machine.
The first path is the Custom App installer: the creator supplies an app
manifest, the business owner installs it on their Piccolo, and Piccolo runs it
as one or more containers on the private machine.

The mental model is simple: keep your application code in OCI images, then
teach Piccolo how to run those images, where durable data lives, which private
endpoints should exist, and how access should be protected.

## What You Bring

Bring these pieces:

- One or more public registry-published OCI images, such as Docker Hub or GHCR
  images.
- A manifest, usually named `app.yaml`, that describes services, listeners,
  storage, inputs, permissions, and Piccolo behavior.
- A migration/setup story that is built into the image entrypoint or normal app
  startup. Custom App installs should not depend on catalog-only
  `init_script` files.
- A clear data model: which paths must survive container recreation and which
  paths are temporary caches.

Do not bring a Kubernetes manifest, Docker Compose file, VM image, host systemd
unit, or host path layout and expect Piccolo to run it directly. Those can be
useful source material, but the Custom App path is Piccolo manifest first.

## Design For One Business

A Piccolo install is private to one business or household. Design the app as a
single-tenant deployment, even if the cloud version is multi-tenant:

- Remove tenant selection from the trusted security boundary. The Piccolo
  instance is already the tenant boundary.
- Keep per-business data in local persistent storage owned by that Piccolo app.
- Make setup work without cloud control-plane provisioning.
- Prefer a first-admin or owner setup path, then let that admin create any
  additional app users the business needs.
- Keep outbound calls explicit. A private Piccolo app can still call payment,
  email, AI, or vendor APIs, but it should not require a shared SaaS backend for
  ordinary local use unless that is core to the product.

## Public Images And Source Protection

Piccolo currently pulls app images from public image registries. Treat the image
as a publicly readable artifact:

- Do not bake customer secrets, license keys, private API tokens, or business
  data into the image.
- Do not include proprietary source code unless you are comfortable with the
  public registry exposure. Compile, package, or otherwise ship only the
  artifact you intend to distribute.
- Put per-install secrets in Piccolo inputs, generated passwords, environment
  variables, or app setup flows.
- Keep image tags intentional. The update flow works best when the installed
  manifest uses a tag you plan to move for that app line, rather than a digest
  that can never drift.

## Install Flow

In the Piccolo Apps UI, the owner chooses `Custom App`, pastes the manifest,
reviews generated inputs, chooses the app address, and installs.

The install artifact is the YAML manifest. For additional patterns, the Piccolo
Store provides real app manifests you can compare against:

| Need | Store sample |
| --- | --- |
| Simple protected web app with persistent data | [Vaultwarden](https://github.com/AtDexters-Lab/piccolo-store/blob/main/apps/vaultwarden/app.yaml) |
| Public path plus protected UI | [Uptime Kuma](https://github.com/AtDexters-Lab/piccolo-store/blob/main/apps/uptime-kuma/app.yaml) |
| Boolean/password inputs | [ConvertX](https://github.com/AtDexters-Lab/piccolo-store/blob/main/apps/convertx/app.yaml) |
| Multi-service app with Piccolo OIDC | [Immich](https://github.com/AtDexters-Lab/piccolo-store/blob/main/apps/immich/app.yaml) |
| Git/OCI public paths plus protected UI | [Gitea](https://github.com/AtDexters-Lab/piccolo-store/blob/main/apps/gitea/app.yaml) |
| Well-known raw ports and connection admission | [Pi-hole](https://github.com/AtDexters-Lab/piccolo-store/blob/main/apps/pi-hole/app.yaml) |
| Workspace-style container app | [Code Server](https://github.com/AtDexters-Lab/piccolo-store/blob/main/apps/code-server/app.yaml) |

Some store apps use catalog-only `init_script` files. For the Custom App path,
move that setup into the image entrypoint, normal app startup, or the app's own
first-run setup flow.

## Minimal Web App

This is the smallest useful shape for a web application:

```yaml
type: user

inputs:
  __app_address__:
    type: string
    label: "App Address"
    required: true
    validation:
      regex: "^[a-z][a-z0-9]{0,15}$"
      message: "Use lowercase letters and numbers, start with a letter, max 16 chars."

listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http

services:
  main:
    image: ghcr.io/acme/invoice:1.0.0
    bind_ports: [8080]

x-piccolo:
  mode: service
```

`__primary` is a marker. During install, Piccolo replaces it with the
`__app_address__` value chosen by the owner. If the owner enters `invoice`, the
app identity and primary address become `invoice`.

## Manifest Anatomy

Every normal Custom App manifest should have these parts:

| Field | Purpose |
| --- | --- |
| `type` | Usually `user`. Use `system` only for infrastructure apps that should start during boot. |
| `inputs` | Values collected during install, such as app address, admin email, or generated passwords. |
| `listeners` | The private endpoints Piccolo exposes for the app. Service-mode apps require listeners. |
| `services` | Container definitions. Put `image`, `environment`, `bind_ports`, and `storage` here. |
| `permissions` | Optional network, resource, and filesystem permissions. |
| `healthcheck` | Optional HTTP probe through a named listener. |
| `app_config` | Optional free-form YAML copied to `/piccolo/config/app.yaml` in each container. |
| `x-piccolo.mode` | Required. Use `service` for typical apps and `workspace` for roaming developer environments. |

The app identity comes from the primary listener. In author-written YAML for the
Custom App path, define exactly one listener with `name: __primary`, and define
the reserved `__app_address__` input.

## Converting A Cloud SaaS App

Start from the deployable unit you already have and convert it into Piccolo
terms:

| Cloud or SaaS habit | Piccolo equivalent |
| --- | --- |
| Public cloud URL or ingress | A `listener` with `flow: tcp` and `protocol: http` or `websocket`. |
| Container image in ECS/Fly/Render/Railway/etc. | A service under `services.<name>.image`. |
| Docker Compose service | A Piccolo service. Use `after` only for start order, not readiness. |
| Compose `ports` | `services.<name>.bind_ports` plus a Piccolo `listener`. |
| Compose volumes or cloud disks | `services.<name>.storage.persistent`. |
| Object storage for user uploads | Prefer a persistent local path if the app can support it. |
| Managed Postgres/MySQL/Redis | A sidecar service, or an external dependency if the app still needs one. |
| Environment variables | `services.<name>.environment`, often templated from `inputs`. |
| Host files | Avoid host paths. Use persistent storage or `app_config`. |
| Public auth callback | Piccolo listener origin plus `oidc_client.redirect_uri_paths` when using Piccolo OIDC. |

Piccolo rejects top-level `image`, `environment`, and `storage` in service-mode
apps. Put those fields under the service that uses them.

## Services And Ports

Each service must declare `bind_ports`, even if it is an empty list. Every
listener `guest_port` must appear in exactly one service's `bind_ports`.

For a single web process:

```yaml
services:
  main:
    image: ghcr.io/acme/app:1.0.0
    bind_ports: [3000]

listeners:
  - name: __primary
    guest_port: 3000
    flow: tcp
    protocol: http
```

For a web app plus database, the web service owns the public listener and the
database remains internal:

```yaml
primary_service: web

services:
  web:
    image: ghcr.io/acme/orders:2.3.0
    bind_ports: [3000]
    environment:
      DATABASE_HOST: 127.0.0.1
      DATABASE_PORT: "5432"
      DATABASE_NAME: orders
      DATABASE_USER: orders
      DATABASE_PASSWORD: "{{ .Inputs.db_password }}"
  postgres:
    image: postgres:16
    bind_ports: [5432]
    environment:
      POSTGRES_DB: orders
      POSTGRES_USER: orders
      POSTGRES_PASSWORD: "{{ .Inputs.db_password }}"
```

Services in one app share a network namespace. Do not declare the same port in
two services.

## Durable Data

Assume the container filesystem is replaceable. Anything important must be
declared as persistent storage:

```yaml
services:
  web:
    image: ghcr.io/acme/orders:2.3.0
    bind_ports: [3000]
    storage:
      persistent:
        uploads:
          container: /app/uploads
          size_limit: 20GB
      temporary:
        cache:
          container: /app/tmp
          size_limit: 2GB
```

Piccolo chooses the host path and maps it into the app's encrypted per-app
volume. The manifest should name the container path the app expects, not a host
path.

## Inputs, Secrets, And Config

Inputs are rendered before install. Use them for owner-provided values and
generated secrets:

```yaml
inputs:
  __app_address__:
    type: string
    label: "App Address"
    required: true
    validation:
      regex: "^[a-z][a-z0-9]{0,15}$"
      message: "Use lowercase letters and numbers, start with a letter, max 16 chars."
  db_password:
    type: password
    label: "Database Password"
    required: true
    generate: true
```

Then reference them in the manifest:

```yaml
environment:
  DATABASE_PASSWORD: "{{ .Inputs.db_password }}"
```

For structured app settings, use `app_config`. Piccolo writes it to
`/piccolo/config/app.yaml` inside each container:

```yaml
app_config:
  billing:
    currency: INR
  features:
    local_backups: true
```

## Resource Usage

Declare realistic resource needs so the owner understands whether the app fits
on their Piccolo machine:

```yaml
resources:
  priority: normal
  memory:
    min_required: 512MB
    profile: bounded
  storage:
    max: 20GiB
```

Use app-level `resources` for the app's expected shape. Use
`permissions.resources` only for runtime limits such as process count or open
files:

```yaml
permissions:
  resources:
    max_processes: 200
    max_open_files: 2048
```

Do not put `resources` under individual services; current Piccolo resource
policy is app-level.

## Access And Auth

For ordinary web apps, use:

```yaml
flow: tcp
protocol: http
```

If `auth` is omitted on an HTTP or WebSocket listener, Piccolo treats all paths
as `protected`: the user must have a valid Piccolo session and permission to
open the app.

Use explicit auth rules when the app needs a different behavior:

```yaml
listeners:
  - name: __primary
    guest_port: 3000
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/healthz"
          type: exact
          strategy: public
        - path: "/"
          type: prefix
          strategy: headers
```

Strategies:

| Strategy | Meaning |
| --- | --- |
| `protected` | Piccolo requires a session and app permission, then forwards the request without identity headers. |
| `headers` | Same as `protected`, plus Piccolo injects `X-Piccolo-User`, `X-Piccolo-Email`, `X-Piccolo-Name`, and `X-Piccolo-Role`. |
| `oidc_passthrough` | Piccolo forwards the request and the app performs OIDC login against Piccolo. Requires `services.<name>.oidc_client`. |
| `public` | No Piccolo session check for that path. Use intentionally and narrowly. |

For apps with their own user database, decide how Piccolo identity maps to app
users:

- Use `oidc_passthrough` when the app can log in through Piccolo OIDC. The app
  should support first login, invitation, or admin-created accounts for
  additional business users.
- Use `headers` only when the app is designed to trust Piccolo-injected
  identity headers and does not need its own login challenge.
- Use `protected` when Piccolo should gate access but the app still has its own
  local login or admin user model.

### Recommended: App As OIDC Client

When the app needs to know which Piccolo user is currently accessing it, prefer
`oidc_passthrough`. In this mode, Piccolo allows requests through to the app,
and the app performs the normal OIDC login flow against Piccolo.

The manifest has two parts. First, route user-facing paths to the app with
`oidc_passthrough`:

```yaml
listeners:
  - name: __primary
    guest_port: 3000
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/"
          type: prefix
          strategy: oidc_passthrough
```

Second, declare an OIDC client on the service that needs the credentials:

```yaml
services:
  main:
    image: ghcr.io/acme/app:1.0.0
    bind_ports: [3000]
    oidc_client:
      redirect_uri_paths:
        - /auth/callback
      authorize_paths:
        - /auth/login
      ca_mount_path: /etc/ssl/certs/piccolo-ca.crt
      env:
        OIDC_ISSUER: "{{ .System.Auth.Issuer }}"
        OIDC_CLIENT_ID: "{{ .System.Auth.ClientID }}"
        OIDC_CLIENT_SECRET: "{{ .System.Auth.ClientSecret }}"
        NODE_EXTRA_CA_CERTS: /etc/ssl/certs/piccolo-ca.crt
```

Inside the app, configure your OIDC library from those environment variables.
Use the authorization-code flow, request `openid profile email`, and use the
issuer's discovery document to find the authorization, token, JWKS, and
userinfo endpoints. After the callback, the app should create its own local
session, just as it would with any other OIDC provider.

Use the OIDC subject claim, `sub`, as the stable Piccolo user identifier. Use
claims such as `preferred_username`, `name`, `email`, `email_verified`, and the
custom `role` claim for display and app authorization decisions. If the app has
its own user table, link or create an app-local user on first successful OIDC
login. For additional business users, the owner adds users in Piccolo and grants
them app access; the app can then provision or link those users when they first
sign in through Piccolo OIDC.

Auth rules only apply to `flow: tcp` listeners with `protocol: http` or
`protocol: websocket`. Raw TCP, UDP, and TLS-passthrough listeners cannot use
path auth because Piccolo cannot see HTTP paths.

## Network Permissions

Set container network access deliberately:

```yaml
permissions:
  network:
    internet: allow
```

Use `internet: allow` when the app must send email, call payment providers,
fetch package indexes, talk to external APIs, or receive webhooks through an
outbound tunnel. Use `internet: deny` only after testing the app's access path:
it is a hard isolation mode that starts the app without ordinary container
networking.

## Well-Known Ports And Raw Protocols

Most apps should let Piccolo allocate listener ports and publish HTTP names. Use
`port_claim` only when the app must own a specific LAN port, such as DNS on 53:

```yaml
listeners:
  - name: dns
    guest_port: 53
    flow: udp
    protocol: raw
    port_claim: 53
```

Port claims are advanced. They cannot claim Piccolo-reserved ports such as 80,
443, 8080, or 5353, and they are not the right tool for ordinary web apps.

## Development Loop And Logs

During development, use the Custom App path directly:

1. Build and push a public image tag.
2. Install the app from YAML.
3. Open the app status and logs in the Piccolo UI, or use
   `/api/v1/apps/{name}/logs` and `/api/v1/apps/{name}/logs/stream`.
4. Fix the app image, manifest, or inputs.
5. Reinstall while the app is disposable, or use the update flows once you are
   preserving real data.

Write useful startup logs to stdout/stderr. Piccolo captures per-service
container logs, so clear messages for migrations, missing env vars, failed
connections, and ready state make development much faster.

## Updating A Custom App

For code-only changes, publish a new image under the manifest's existing image
reference and use Piccolo's app update flow. The common path is: push a new
image to the same tag that the installed manifest already references, then run
Update Image. Piccolo re-checks non-digest-pinned image references and refreshes
services whose registry digest changed.

For manifest wiring changes, Piccolo has an explicit custom manifest update
flow: prepare the manifest, re-enter or regenerate required inputs, dry-run the
diff, then apply the exact reviewed candidate.

Treat manifest updates as maintenance, not as the first-install story. Keep app
identity, service topology, listener topology, and persistent data boundaries
stable unless the Piccolo UI explicitly supports the change you are making.

## Preflight Checklist

Before handing a manifest to a business owner:

- It defines `x-piccolo.mode`.
- It defines exactly one `listeners[].name: __primary` for service-mode apps.
- It defines the reserved `__app_address__` input.
- Every listener `guest_port` is present in exactly one service `bind_ports`.
- Images are public-registry images and pullable by the Piccolo machine.
- Images do not contain secrets or source code you are unwilling to expose.
- Durable paths are declared under `storage.persistent`.
- Temporary caches are either disposable or declared under `storage.temporary`.
- Top-level `image`, `environment`, `storage`, `build`, and `depends_on` are not used.
- HTTP/WebSocket auth rules are intentional; public paths are narrow.
- Piccolo OIDC, trusted headers, and app-local users have an explicit mapping.
- Resource declarations reflect the app's real memory and storage needs.
- Raw TCP, UDP, TLS-passthrough, and `port_claim` listeners are used only when the app really needs them.
- Setup and migrations do not depend on catalog-only `init_script` files.
- The update strategy is clear: same installed tag for image-only updates, manifest update flow for wiring changes.

## Common Validation Errors

| Error shape | Likely fix |
| --- | --- |
| `apps with listeners must have exactly one listener named '__primary'` | Rename the primary listener to `__primary` and add the `__app_address__` input. |
| `listener ... guest_port ... must be declared in exactly one service bind_ports` | Add the port to the owning service's `bind_ports`, and remove duplicate owners. |
| `image must be specified per-service` | Move `image` under `services.<name>.image`. |
| `environment must be specified per-service` | Move environment variables under the service that needs them. |
| `build is not supported; specify image` | Publish an OCI image first, then reference it in the manifest. |
| `depends_on is not supported` | Use multiple services and optional `after` start order; make services tolerate retries. |
| `auth block not supported on flow: tls or protocol: raw` | Use HTTP/WebSocket TCP for path auth, or remove listener auth from raw/TLS listeners. |
| `privileged containers are not supported` | Remove privileged requirements or redesign the app for unprivileged containers. |

## Where To Go Deeper

Use [specification.yaml](./specification.yaml) as the complete field reference.
Use the [Piccolo Store](https://github.com/AtDexters-Lab/piccolo-store) as the
main source of real sample manifests, then validate through the Custom App
configure step before sharing a manifest with an owner.
