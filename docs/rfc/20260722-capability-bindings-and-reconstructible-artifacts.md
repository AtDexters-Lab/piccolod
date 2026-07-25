**Problem:** Piccolo apps cannot declaratively publish a platform-known capability, consume the selected provider without provider-specific discovery, receive capability-derived accelerator access, or mount large reconstructible content outside ordinary app data.
**In scope:** Versioned provider and consumer declarations; one device default per capability; app-private local binding; accelerator discovery and exclusive grant for `ai.inference.openai.v1`; OCI and Hugging Face sources; generic encrypted golden-content materialization; observable lifecycle, failure, and security contracts.
**Out of scope:** An inference runtime; OpenAI semantic conformance testing; provider model, batching, residency, or memory policy; provider-state migration; a general service mesh; private source credentials; automatic mutable-reference refresh policy; an artifact-specific incomplete-install healer; new artifact inventory or storage-diagnostics API fields; multi-node capability execution or artifact activation; peer golden-LV transport implementation; and an implementation work plan.

# RFC: Capability Bindings and Reconstructible Artifacts

**Date:** 2026-07-22

**Status:** Accepted

**Feature gates:** `capability_bindings_v1`, `artifact_bindings_v1`
**Decision ledger:** `20260722-capability-bindings-and-reconstructible-artifacts-decisions.md`

## 1. Decision

Piccolo will treat platform capabilities as relationships between ordinary
installed apps:

- a provider tags the existing listener that serves a recognized capability;
- a consumer asks for that capability on the service that needs it and
  explicitly maps binding properties to environment variables;
- Piccolod selects one device-level default provider for each capability and
  gives each consuming app a private local ingress to that provider;
- a registered capability may grant runtime resources such as GPU/NPU access to
  its selected provider; and
- an app may declare reconstructible content as a named Piccolo artifact and
  mount it read-only from the same golden-LV machinery used for service
  root filesystems.

There is no special system-app class. Catalog and custom apps use the same
contract. The first registered capability is `ai.inference.openai.v1`; its
binding exposes `base_url`, and its selected provider receives the accelerator
grant.

The provider image owns OpenVINO, OVMS, vLLM, llama.cpp, or any other userspace
runtime. Piccolod neither chooses that runtime nor manages models, batching, or
inference memory. A provider cannot assume that declaring a capability grants
accelerator devices: an unselected provider must start and bind its declared
listener without them and may report the capability unavailable until it is
selected.

## 2. Architecture and invariants

The design separates three independent concerns:

1. **Source adapter:** OCI image flattening, OCI artifact extraction, or
   Hugging Face file/directory projection.
2. **Golden content:** one verified, encrypted, read-only golden LV managed by
   the existing materialization, reference, and GC lifecycle.
3. **Consumption:** a service rootfs uses a snapshot; a Piccolo artifact uses a
   read-only attachment.

“Image” and “artifact” do not imply different download or storage protocols.
The same resolved OCI image must reuse the same golden LV whether one app uses
it as a rootfs and another mounts it as an artifact.

The following invariants are load-bearing:

- only the selected provider for an accelerator-owning capability receives its
  accelerator devices;
- the old and new provider must never hold the same exclusive accelerator grant
  concurrently;
- a consumer's private ingress routes only to its selected provider and only
  within the provider-declared capability path;
- incomplete or unverified golden content is never mounted into an app;
- a golden LV remains GC-ineligible while any rootfs snapshot, artifact
  attachment, candidate, rollback, or live container references it; and
- ordinary app start reuses already recorded Ready content and does not require
  an upstream fetch.

## 3. Manifest contract

The exact schema, field types, and validation rules are canonical in
`docs/app-platform/specification.yaml`.

Manifests using `provides` or `consumes` require
`capability_bindings_v1`. Manifests using top-level `artifacts` or
`storage.artifacts` require `artifact_bindings_v1`. These use the existing
`x-piccolo.requires_features` mechanism; this RFC does not introduce a second
feature-policy system.

### 3.1 Provider and consumer

A provider declares a capability on the listener that serves it:

```yaml
listeners:
  - name: inference
    guest_port: 8000
    flow: tcp
    protocol: http
    provides:
      - capability: ai.inference.openai.v1
        base_path: /v3
```

A consumer declares the capability on each service that needs discovery and
chooses its environment-variable name explicitly:

```yaml
services:
  worker:
    image: example/consumer:1
    consumes:
      - capability: ai.inference.openai.v1
        env:
          OPENAI_BASE_URL: base_url
```

The map direction is
`ENVIRONMENT_VARIABLE: binding_property`. Piccolod does not generate an
additional automatic variable.

For `ai.inference.openai.v1`:

| Contract field | Value |
|---|---|
| Listener transport | `flow: tcp`, `protocol: http` |
| Required provider field | canonical absolute `base_path` |
| Consumer binding property | `base_url` |
| Local credential | none |
| Runtime grant | GPU/iGPU and NPU device families |

The provider's `base_path` is its real API base. Piccolod composes the private
origin with this path and injects the result as `base_url`. It does not add
`/v1`, translate `/v1` to another path, or adapt the provider API. The `.v1` in
the capability identifier versions Piccolo's contract, not the provider's URL.

Validation is structural:

- the capability and binding property must be registered;
- the listener transport and required provider fields must match the registered
  contract;
- one app cannot provide the same capability from multiple listeners;
- a service cannot consume the same capability more than once;
- binding environment-variable names must be valid and cannot collide with an
  explicit service variable, generated OIDC variable, or another binding; and
- `base_path` must be a canonical absolute path without query or fragment; each
  segment must be terminal after one percent-decode, with no residual escaping,
  separator, backslash, NUL, or dot-segment meaning.

Piccolod does not probe model availability, OpenAI resources, request/response
schemas, streaming, backend support, performance, or output quality.

### 3.2 Piccolo artifacts

A Piccolo artifact is a named, read-only relationship through which an app
consumes reconstructible content. It is not synonymous with an OCI artifact and
is not a separate physical storage type.

Hugging Face example:

```yaml
artifacts:
  model:
    source:
      type: huggingface
      repository: OpenVINO/Qwen3-0.6B-int4-ov
      revision: main
      path: .

services:
  inference:
    image: example/provider:1
    storage:
      artifacts:
        model:
          container: /models/model
```

OCI example:

```yaml
artifacts:
  model:
    source:
      type: oci
      reference: ghcr.io/example/model:latest
      # digest: sha256:<expected-descriptor-digest>
```

V1 supports:

| Source | Required fields | Optional verification |
|---|---|---|
| `oci` | `reference` | expected OCI descriptor `digest` |
| `huggingface` | `repository`, `revision`, `path` | selected-file `digest` |

A Hugging Face `path` may select one file, a directory subtree, or the whole
repository. Directory contents retain their relative layout; a selected file
appears at its basename. A file digest is optional and applies only when one
file is selected. Authors requiring an exact directory version can use an
immutable revision.

Digest fields verify explicit user intent; they are not mandatory. When a
source is unpinned, one materialization attempt still resolves and records a
concrete immutable identity so all bytes in that attempt are consistent.
Resolving the same mutable declaration at a later, separately triggered event
may legitimately produce different content. The policy deciding when such a
new resolution occurs is deferred.

Artifact mounts are always read-only. A declaration must be referenced by at
least one service, and a mount must reference a declaration. Paths must not
overlap another mount target in the same service. An
`ai.inference.openai.v1` provider's artifact targets also cannot overlap the
capability-owned `/dev/dri` or `/dev/accel` device families. Host paths,
writable mode, subpaths, artifact-specific replication settings, and manifest
storage quotas are not part of this contract.

## 4. Capability behavior

### 4.1 Registry and defaults

Capability behavior is Piccolod-owned. Manifests may use registered capability
identifiers but cannot invent binding properties, default rules, or runtime
grants.

Piccolod stores one selected provider per capability:

- the first eligible, fully installed provider becomes default automatically;
- installing another provider does not steal the default and does not require a
  blocking prompt;
- the user may explicitly select another installed provider;
- uninstalling the selected provider, or committing its update without the
  capability declaration, invalidates the selection and chooses a deterministic
  remaining provider when one exists, otherwise the capability becomes
  unavailable;
- a manual Stop or temporary failure retains both the selected identity and its
  host device-node permission; no running container is present to exercise that
  permission, and consumers receive HTTP 503 until the provider is usable; and
- an affected consumer app group restarts only when its injected binding
  environment changes. A provider or route retarget that preserves the
  injected value does not restart the consumer.

Candidate eligibility and existing-selection validity are distinct. An app is
eligible to become a new default when its committed installed manifest declares
the capability and the app is enabled. Once selected, a manual Stop makes it
temporarily unusable but does not invalidate that selection. Transient runtime
health and route availability likewise do not cause automatic reselection.
When invalidation needs a replacement, Piccolod chooses the eligible app with
the oldest durable installation timestamp, using app-instance ID as the
tie-breaker.

Running requests may be interrupted during a switch. Piccolod does not add
capability-specific draining. Provider-owned configuration, models, indexes,
history, and other state are not migrated; the management surface discloses
this before a user-initiated switch or removal.

Provider selection is by app instance. The listener and `base_path` are derived
from the installed manifest because one app cannot publish the same capability
from multiple listeners. Selecting a different app retargets the private route.
A consumer restart is needed only when the resulting injected `base_url`
changes, such as when the selected `base_path` changes. Exact management API
wire shapes and UI text belong to the API specification and implementation
plan, not this RFC.

### 4.2 Private local binding

For each `(consumer app, capability)`, Piccolod owns a loopback-only ingress
inside that app's existing network namespace. This follows the existing app
network and listener architecture; it does not introduce a service mesh,
separate network plane, OAuth flow, bearer token, or caller identity inferred
from an ephemeral source port.

Concretely, Piccolod opens and retains the listening socket through the
consumer's existing network-anchor namespace. The socket remains owned by
Piccolod, not by a provider or sidecar container. Stable port allocation,
rebinding after anchor recreation, and teardown when the binding disappears are
effects of the existing consumer app lifecycle.

All services in an app share its network namespace, so the security principal
is the app. Service-scoped `consumes` controls which containers receive the
binding environment variable, not which sibling container can technically
reach the app-private ingress.

The injected `base_url` is:

```text
private app origin + selected provider base_path
```

Consumer clients append their ordinary resource suffixes. The ingress forwards
only the declared `base_path` itself or a segment-descendant and preserves the
accepted normalized path and query. Requests outside that subtree receive HTTP
404 and never reach the provider. Caller-controlled proxy/platform identity
metadata must not cross this private trust boundary.

Path authorization and forwarding use the same canonical segment
representation. Piccolod rejects request targets with ambiguous path
representations—including invalid or encoded separators, backslashes, empty or
dot segments—before the subtree check, and constructs the forwarded path from
the segments that passed that check rather than forwarding an unchecked raw
path.

The private origin remains stable for the installed binding until that binding
is removed. When no provider has yet supplied a path, the consumer receives the
private origin alone and every request returns HTTP 503. Piccolod recreates the
consumer only when the composed injected value changes; otherwise it retargets
the existing private route without changing the consumer environment.

The private route targets the selected provider's ordinary listener backend but
does not traverse its public listener authentication flow. V1 injects no
provider credential, so a conforming local provider must accept the private
route using `base_url` alone.

The provider owns the private surface it declares. `base_path: /` intentionally
makes the whole listener available through this private binding; a non-root
path limits it to that canonical subtree. Piccolod does not infer which
provider endpoints are inference, administration, or otherwise “related.”

A consumer may install and start without a usable provider. Its private ingress
returns HTTP 503 until one is available. Provider failures and other proxy
responses use standard HTTP status codes; Piccolod does not define
OpenAI-shaped error bodies.

### 4.3 Public and remote listener

The provider's declared listener remains an ordinary Piccolo listener. Existing
LAN, remote/Nexus, port, OIDC, and listener authentication behavior continues
unchanged. Capability binding creates no additional public API.

The local default affects only capability consumers. A non-default provider's
ordinary public listener may remain reachable, but it receives no
capability-derived accelerator grant.

## 5. Accelerator discovery and grant

Piccolo OS owns kernel drivers and firmware. Piccolod discovers devices and
controls container access. The provider owns userspace discovery and backend
selection.

Discovery uses standard Linux interfaces rather than processor-name branches:

- PCI and driver information under `/sys/bus/pci/devices`;
- DRM information and devices under `/sys/class/drm` and `/dev/dri`; and
- accelerator-class information and devices under `/sys/class/accel` and
  `/dev/accel`.

The near-term validation targets are Intel N150 and Intel Core Ultra 285H, but
the runtime contract is generic. The provider receives the standard read-only
sysfs view used by upstream runtimes and every available device in the granted
DRM and accelerator families. Piccolod does not publish a versioned hardware
profile or select render, media, or NPU nodes on the provider's behalf.

For `ai.inference.openai.v1`:

- only the selected default provider receives the capability-derived devices;
- all service containers in that provider app receive those device families;
- a non-default provider receives none merely because it declares the
  capability;
- a fresh install or different-provider switching candidate receives no
  provisional grant;
- a replacement generation of the already-selected app inherits that app's
  grant after the previous generation is proven absent;
- when no default exists, no app receives the capability-derived devices; and
- a selected provider's temporary failure does not transfer the grant.

An unselected provider must remain startable and listener-capable without these
devices; it may stay idle or return HTTP 503 for inference requests. Initial
install therefore completes without accelerator authority. If the committed
app becomes the default, Piccolod recreates its containers with the grant.

Accelerator entitlement and host device-node permission belong to the selected
app instance, not to a particular container generation. Stop, start, crash, and
same-app replacement do not revoke that permission. Running containers receive
the device mappings when they are created; a stopped app has no process or
mapping that can exercise the retained permission. Same-app replacement proves
the previous generation absent and creates the replacement with the inherited
grant, without a device-free intermediate generation or a second recreation.

The grant includes whatever host permission is necessary for the provider's
configured container user to open the mapped nodes. Before a different provider
starts with those devices, Piccolod must establish that the old provider's
mapped containers are absent, revoke the old app's host device-node permission,
and grant the new selected app. Uninstall or removal of the selected capability
declaration follows the same ownership-transfer boundary.

This grant follows from the registered capability and current default. Catalog
provenance, maintainer identity, a system-app label, and the broad legacy
`permissions.filesystem.device_access` field are not additional authorities.
Installing and selecting a custom provider is an explicit user choice.

Cross-capability accelerator scheduling is deferred until another recognized
capability creates a real sharing requirement.

## 6. Golden-content materialization

### 6.1 One lifecycle, source-specific adapters

All sources use the existing generalized golden-content lifecycle for
allocation, encryption, filesystem creation, materialization, verification,
Ready publication, references, restart/retry, progress, and GC. Artifact size
does not create a separate install or download protocol.

Only the adapter differs:

- **OCI object accepted as an image by Podman:** use the existing
  scratch-backed pull/flatten path.
- **OCI object classified by Podman as a non-image artifact:** use Podman's
  native artifact pull and extraction into the staging golden LV.
- **Hugging Face:** resolve the declared repository, revision, and file or
  directory to a concrete revision and project the selected content into the
  staging golden LV.

An OCI source has no app-supplied image-versus-artifact discriminator.
Piccolod tries the ordinary image materialization path first. Success produces
the canonical flattened image golden LV. Only Podman's specific non-image
artifact classification selects artifact pull/extraction; an unrelated
transport, authentication, digest, capacity, or generic pull failure does not.
This keeps registry packaging out of Piccolo consumption semantics while
avoiding a second download for ordinary images.

Podman owns OCI registry transport, descriptor/blob verification, and extraction
semantics. Piccolod does not implement another OCI registry client, media-type
interpreter, custom artifact layout, or OCI-specific downloader policy.

This dispatch was verified with Podman v5.8.2: its ordinary image pull rejected
Podman's published single-file test artifact as a non-image artifact before
copying its payload; artifact pull accepted both that artifact and an ordinary
container image. Extracting the ordinary image through the artifact path
produced its compressed layer blob, whereas the image path produced the merged
root filesystem. Therefore always using either path would be incorrect, but the
manifest does not need to choose between them.

Hugging Face inputs are structured repository/revision/path fields rather than
arbitrary source URLs. The adapter confines paths to the selected repository
tree and verifies streamed content. Private registry and Hugging Face
credentials are outside V1.

### 6.2 Storage representation and reuse

Materialized content uses the existing block-native shape:

```text
dm-thin golden LV -> LUKS -> filesystem -> verified content root
```

The golden identity is derived from source kind, resolved immutable content
identity, and any selected projection. It does not include whether an app will
consume the content as a rootfs or artifact.

Any shortened storage key is only an index, not proof of identity. Before
reusing Ready content, Piccolod compares its complete durable golden identity;
unequal identities never share an LV even when their shortened keys collide.

Consequently:

- the same resolved OCI image identity reuses the same golden LV for a rootfs
  origin and an artifact;
- a rootfs consumer receives the existing snapshot behavior;
- an artifact consumer receives an app-private read-only attachment to the
  Ready golden LV; and
- both consumption modes participate in the same reference and GC accounting.

No writable per-app artifact snapshot is created. Payload data and temporary
materialization data stay off the Piccolo core volume. Materialization reuses
the existing golden-LV capacity checks and pool guard rather than adding a
global reservation coordinator.

In a future multi-node deployment, Ready golden content will follow the
existing storage architecture's one-time peer pre-seeding/block-copy path.
Peers will not independently fetch identical content, and this RFC does not add
continuous artifact replication or implement that peer path.

### 6.3 Publication and failure

One materialization attempt binds a concrete resolved identity before Ready
publication. Content becomes Ready only after the adapter completes and its
verification evidence is durable. Incomplete staging is never attached.

If matching Ready content already exists, the lifecycle reuses it and acquires
a new reference. If materialization fails, the staging ownership is cleaned or
recovered through the existing golden lifecycle; an existing committed app
runtime remains unchanged.

Artifact attachment is read-only and app-private. Shared raw golden mounts are
not exposed to arbitrary app identities. During materialization, no source
adapter may write outside its staging root; entries that would do so and
unsupported device or special nodes fail before publication. This extraction
boundary does not turn ordinary payload symlinks into a second intra-app
sandbox: when the same OCI content is mounted as an artifact, its payload
semantics remain inside the consuming app's existing trust boundary.

## 7. Lifecycle and temporal contracts

This feature composes with existing app and golden-content lifecycle owners; it
does not introduce a common install/clone/update/uninstall coordinator.

### 7.1 Install, update, clone, and uninstall

Each existing lifecycle path remains responsible for applying and releasing its
own capability bindings, accelerator effects, and golden references. Small
shared helpers may implement repeated mechanics.

Ordinary app lifecycle commits do not independently publish an automatic
capability default and leave its effects for later. Existing app reconciliation
derives that default and applies the resulting provider and consumer effects as
one convergence step. Explicit administrator selection remains a synchronous
cross-app operation with post-commit repair semantics.

Stopping the selected provider makes its binding unavailable without changing
its selected identity or host device-node permission; starting it reuses both.
Uninstall or a committed update that removes the selected capability declaration
invalidates the selection and runs the ordinary deterministic replacement rule,
including revocation before another app receives the devices.

A provider install starts and proves listener readiness without
capability-derived devices. Only after the install commits may automatic
selection trigger recreation with the grant. A provider that cannot bind its
listener while unselected fails the ordinary install readiness check; it does
not receive pre-commit candidate authority. This rule does not make a new
generation of the already-selected app a new authority candidate: after the old
generation is absent, the replacement starts with the selected app's existing
grant.

Artifact work has no capability authority. A provider materializing content
does not reserve or freeze a default, binding, or accelerator grant. At
publication, current independently authoritative capability state wins. Thus a
second provider selected while the first is still materializing remains the
default; the first cannot later steal that selection.

Updates keep the currently committed runtime and golden references until the
ordinary app transition commits a replacement or rolls back. Uninstall removes
runtime effects before releasing the final golden references.

### 7.2 Provider and binding changes

Switching to a different provider has two externally observable phases:

1. consumer ingresses become unavailable for the changing route; and
2. after the old provider's processes and host permission are absent, the new
   selected provider starts with devices and Piccolod retargets the ingresses.

Affected consumers are recreated only if their injected binding environment
changes. A selected app's ordinary replacement retains its authority: after the
old generation is absent, the replacement starts once with devices. It does not
commit a device-free generation and then recreate it.

Requests already in flight may be interrupted. Failure and rollback use the
existing app lifecycle; this RFC adds no independent capability transaction or
durable job system.

### 7.3 Restart and retry

Ready golden identity and reference records are authoritative after restart.
Partial materialization remains non-mountable and is resumed or cleaned by the
existing golden lifecycle. Capability defaults are reconstructed from their
ordinary durable state; private ingresses and device grants are reconciled as
effects.

If a fresh app install fails and cleanup cannot prove that all candidate
processes are absent, Piccolod retains the candidate-owned resources fail
closed. It does not publish an installed app and does not run a new
artifact-specific background orphan healer. An explicit retry or ordinary
restart recovery may prove absence and clean or reuse those resources; unrelated
installed apps continue through their existing lifecycle.

Retrying an explicitly pinned source must continue to satisfy its pin. Retrying
recorded Ready content does not silently follow a mutable tag or branch.
Choosing when to perform a fresh resolution of an unpinned declaration remains
deferred.

### 7.4 Leadership boundary

V1 reuses the existing app lifecycle:

- new install/materialization begins through the ordinary kernel-leader-gated
  install path; and
- the existing per-app leadership lifecycle owns runtime quiescence.

There is no capability/artifact-specific feature policy, leadership generation,
stale-work token, demotion barrier, or effect-executor ledger. The current
product is single-node. Generic in-flight install behavior across future real
leadership changes must be solved once in the existing cluster lifecycle for
ordinary OCI pulls and artifacts together.

### 7.5 Temporal composition summary

The following table records outcomes, not an implementation state machine:

| Canonical event | Owner and required outcome |
|---|---|
| Start or activation | The existing app lifecycle requests bindings, grants, and golden references. A fresh or unselected provider starts without capability-derived devices. The already-selected app starts or replaces its stopped generation with its retained host permission and mapped devices. No artifact mount is created before Ready. |
| Normal completion or commit | The ordinary app transition publishes the new runtime and then releases superseded golden references. A provider handoff publishes usable new access only after old accelerator ownership is absent. |
| Failure before visible effect | Candidate work fails without changing the committed runtime, default, or existing golden references. |
| Pause or suspension | This feature adds no manual pause state. Ordinary app stop may make a binding unavailable while retaining its selected provider, host device-node permission, and durable golden references. |
| Resume or reacquisition | Existing app reconciliation restores private ingress, Ready attachments, and the selected provider's grant from authoritative app/default/reference state. |
| Cancellation, interruption, or abort | Existing shutdown, update, uninstall, and app-lifecycle cancellation rules own cleanup. V1 adds no user-facing active-download cancellation contract. |
| Supersession, handoff, or owner change | Current capability state wins independently of pending artifact work. Accelerator ownership cannot overlap during a provider handoff. |
| Retry or replay | A retry of one accepted resolution uses its concrete identity and explicit verification constraint; a fresh mutable-source resolution is a separate policy event. |
| Restart or recovery | Ready identity/reference records are reused. Partial content stays non-mountable and follows existing golden recovery or cleanup. |
| Rollback or compensation | Existing app rollback retains or restores the previously committed runtime and its references; candidate-only effects are removed. |
| One-sided effect or persistence success | The affected binding/runtime remains unavailable until ordinary reconciliation proves the durable selection, grant, attachment, and live effects agree. It must not fail open to an unrecorded mount or duplicate grant. |
| Concurrent overlap or reordering | Identical golden identities deduplicate under the golden-content owner. Capability selection is evaluated independently at publication; pending materialization cannot reserve it. |

Effect ordering is constrained only where an invariant requires it:

- verified Ready state precedes artifact attachment;
- an app reference precedes its live attachment and outlives that attachment;
- old accelerator mapping and host access are absent before a different app
  receives the new grant;
- a changed binding is unavailable until its selected provider and route are
  usable; and
- superseded golden references are released only after the corresponding old
  candidate, rollback, and live containers are absent.

Execution remains owned by the existing app lifecycle and generalized
golden-content lifecycle. Source adapters do not become independent durable job
owners, and no capability lock or authority is held for the duration of a
download. Existing lifecycle serialization and golden deduplication own
concurrent work; this RFC adds no cross-lifecycle lock order.

The later implementation plan must deliberately cover at least these composed
events: provider change during consumer restart, source failure during an app
update, daemon interruption before and after Ready, uninstall racing a
reference acquisition, and two consumers resolving the same golden identity.
These cases verify the invariants above without prescribing function-level test
cases here.

## 8. Security and trust boundaries

- A private ingress is reachable only inside its consumer app's existing
  network namespace and only for the provider-declared capability path.
- The declared capability path is the intentional public-auth bypass boundary.
  `/` exposes the whole listener; a non-root path exposes only its canonical
  segment subtree. Piccolod enforces that boundary without interpreting
  provider endpoint purpose.
- Caller-supplied Piccolo/proxy identity metadata is not trusted across the
  private ingress. The ordinary public listener keeps its existing
  authentication contract.
- A selected provider is user-authorized code receiving powerful host devices.
  Installation and default-selection review make that accelerator effect
  visible, but no second provenance or permission authority is introduced.
- Source content is untrusted until verified Ready. It stays in encrypted
  staging, cannot escape its projection root, and is exposed only through
  read-only app-private attachments.
- Explicit digests constrain the candidate that supplied them. An unpinned
  source is allowed to change only at a later resolution event, never midway
  through one materialization attempt.

## 9. Failure contract

| Condition | Observable behavior |
|---|---|
| No usable selected provider | Consumer private ingress returns HTTP 503 |
| Selected provider is stopped or temporarily fails | Selection and host device-node permission are retained; no stopped container can exercise them; consumers receive HTTP 503; no implicit provider switch |
| Selected provider is uninstalled or its committed manifest removes the capability | Revoke its permission, select the deterministic eligible replacement or become unavailable, and restart consumers only if their injected binding changes |
| Provider route or binding changes | Retarget the private ingress; restart affected consumers only if their injected binding environment changes; in-flight requests may be interrupted |
| Old accelerator access cannot be revoked | New provider does not receive the devices |
| Provider cannot bind its listener without accelerator devices | Its install/update candidate fails without changing the committed default or grant |
| Source resolution, download, or verification fails | Candidate is not published; committed runtime remains |
| Explicit digest mismatches | Candidate fails and acquires no Ready reference |
| Materialization is incomplete after interruption | Content remains non-mountable; existing lifecycle recovers or cleans it |
| Ready content is referenced | GC cannot remove its golden LV |
| Recorded Ready content is locally absent | Reconstruct the recorded identity before starting the dependent app; never substitute a moved mutable reference |
| A shortened golden storage key collides | Compare complete durable identities and disambiguate storage; never reuse unequal content |
| Existing capacity guard rejects materialization | Candidate fails without disturbing committed runtime |
| Private request leaves the declared path subtree | Return HTTP 404 without reaching the provider |

Errors use existing generic Piccolod status and task/app-detail surfaces. This
RFC does not prescribe OpenAI-shaped JSON, exact UI copy, a manual
download-cancel API, or a new durable job identity.

## 10. Compatibility and rollout

The two feature gates are advertised only when their complete local
parser/runtime behavior is implemented. Parser acceptance and catalog
required-feature filtering must recognize a gate in the same release. Older
versions continue to reject manifests requiring unknown features and omit
incompatible catalog entries instead of silently ignoring the fields.
`artifact_bindings_v1` also requires an OS Podman version that provides the
native artifact pull, inspect, and extract behavior used in §6.1; the current
contract was validated with Podman v5.8.2.

Multi-node capability execution, artifact activation, version coordination, and
peer-copy implementation remain deferred. The storage and leadership
architectures are reused when those capabilities are implemented; V1 does not
pre-design a parallel protocol.

## 11. Affected responsibility owners

The later implementation plan will provide the exact file/function site list.
At RFC altitude, the affected owners are:

- the canonical manifest specification and parser/types;
- catalog required-feature eligibility/filtering;
- installed-app default, environment, update, clone, and uninstall composition;
- existing listener proxy, network-anchor, and app-private namespace plumbing;
- container device and read-only mount construction;
- persistence golden-content materialization, references, attachments, and GC;
- existing encrypted capability selection state and app reconciliation; and
- existing administrator API/UI surfaces for default selection and visible
  install/switch effects.

No separate daemon, model sidecar, FUSE filesystem, authentication service,
network plane, storage subsystem, or lifecycle coordinator is required.

## 12. Status

This RFC and `docs/app-platform/specification.yaml` define the accepted
contract. Implementation is not part of this RFC pass.
