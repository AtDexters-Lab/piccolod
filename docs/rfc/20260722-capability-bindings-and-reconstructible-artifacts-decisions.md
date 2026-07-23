**Problem:** Compaction and repeated review passes can obscure which capability-binding and reconstructible-artifact requirements the user actually chose.
**In scope:** A durable ledger of explicit product decisions, rejected mechanisms, deferrals, and currently open simplification questions for the associated RFC.
**Out of scope:** Changing the RFC or manifest specification, choosing answers for open questions, and prescribing implementation details that the user has not approved.

# Capability Bindings and Reconstructible Artifacts: Decision Ledger

**Status:** Active
**Related RFC:** `20260722-capability-bindings-and-reconstructible-artifacts.md`
**Source conversation:** Codex root thread
`019f7b42-54c4-7510-8985-84cd86ec442b`, 2026-07-19 through
2026-07-23
**Canonical raw rollout:** `~/.codex/sessions/2026/07/19/rollout-2026-07-19T22-12-52-019f7b42-54c4-7510-8985-84cd86ec442b.jsonl`
**Last audited raw user record:** `U145`
**Propagation state:** Audited against the worktree based on
`1e4a2d6f6ed188852e8771191c6a1b2081336c7f`; D-01 through D-25 are reflected
in the RFC and specification, with no known unpropagated decision.

## How to use this ledger

This file records product intent independently of the current RFC text. The RFC
and specification must conform to locked decisions here; their existing detail
does not by itself create a product requirement.

Evidence labels such as `U62` identify the ordinal of a raw `role=user`
message in the root-thread rollout. Injected instruction/environment records
remain in that raw numbering but were ignored as product evidence. Repeated
dictation turns also remain in the numbering; the ledger uses the corrected or
later version as evidence.
When a short user response such as “agreed” depended on the immediately
preceding proposal, that proposal was used only to identify what the user
accepted. A later explicit correction supersedes an earlier acceptance.

Statuses have these meanings:

- **Locked:** the user explicitly chose or accepted this outcome.
- **Rejected:** do not reintroduce this mechanism unless the user explicitly
  reopens it.
- **Deferred:** intentionally outside V1; it is not a hidden requirement.
- **Open:** discuss one at a time before changing the RFC.

## Product philosophy

### P-01 — Abstract around shared responsibilities, not labels

Architecture follows the invariant and owning lifecycle, not a content label or
an expected common-case size. An OCI image may be larger than an artifact and
an artifact may be smaller than an image; neither label justifies a separate
download, recovery, or storage protocol.

### P-02 — Keep source, materialization, and consumption orthogonal

Treat these as three independent axes:

- **source adapter:** OCI image flattening, OCI artifact extraction, or Hugging
  Face projection;
- **physical content:** one generic Ready golden LV with common staging,
  durability, verification, reference, and GC behavior; and
- **consumer use:** rootfs snapshot or direct read-only artifact attachment.

Source-specific code ends at producing verified golden content. Consumer usage
does not create another materialization identity or lifecycle.

### P-03 — Internal identity must not silently strengthen user intent

A resolved digest/commit gives one materialization attempt consistency and a
cache identity. It does not silently convert a mutable user declaration such as
`latest` or `main` into a permanent product-level pin. Explicitly pinned intent
remains exact; explicitly unpinned intent remains allowed to move between
resolution events.

### P-04 — Generalize existing machinery before creating a special path

When the same invariant already belongs to golden-content creation, extend that
owner for every source and consumer. Do not create an artifact-specific install
protocol, preflight transaction, coordinator, or recovery state merely because
AI artifacts are often large.

Evidence for P-01 through P-04: `U126`, “artifact bada ... image file choti ...
universal rule nahi ... general golden LV creation wale process mein kyon
applicable nahi”; `U127`, “lock ... product philosophy ... persist”.

## Locked decisions

### D-01 — Near-term objective and hardware scope

Piccolod must enable an installed inference provider to use host GPU/iGPU and
NPU devices and expose an OpenAI-compatible API to other apps. Near-term
hardware is Intel N150 and Intel Core Ultra 285H, but discovery must use generic
Linux interfaces rather than product-name branches.

Evidence: `U4`, “Intel N150 ... Core Ultra 285H”; `U5`, “NPU bhi imp to
detect”; `U27`, “accessible ... via openai compatible endpoint ... internal
endpoints ... and also over remote access”.

### D-02 — Generic capabilities between ordinary apps

This is a generic Piccolod capability-provider/consumer mechanism, not a special
AI system-app runtime class. Piccolod defines recognized, versioned
capabilities; ordinary installed apps may provide or consume them. One provider
is selected as the device default for each capability.

The first capability is `ai.inference.openai.v1`. Other capabilities such as
vector search may be added later without changing the binding architecture.

Evidence: `U43`, “any service can publish its own local service”; `U44`,
“pre defined capabilities ... kuch apps ... publish ... kuch ... consume ...
system default ... system app distinction ki zarurat hi nahi”.

### D-03 — Provider and consumer manifest placement

A provider declares a capability directly on the existing listener that serves
it. A consumer declares consumption on the specific service that needs the
binding. The consumer explicitly maps binding properties to environment
variables; Piccolod does not also generate environment-variable names.

For example:

```yaml
listeners:
  - name: inference
    provides:
      - capability: ai.inference.openai.v1
        base_path: /v3

services:
  worker:
    consumes:
      - capability: ai.inference.openai.v1
        env:
          OPENAI_BASE_URL: base_url
```

Evidence: `U58`, “sirf yahi rakh sakte hain ... automatic URL generation hata
sakte hain”; `U71`, “service scoped hi theek”; `U72`, “provides declaration bhi
listener level pe”.

### D-04 — Structural validation only

Piccolod validates only what is necessary to construct a binding: recognized
capability, compatible listener transport, unambiguous provider declaration,
known binding property, valid consumer environment-variable name, and no
environment collision. It does not probe or certify OpenAI semantics, models,
runtime readiness, batching, performance, or output quality.

Evidence: `U73`, “rabbit hole”; `U75`, “theek lag raha hai”, accepting the
provider-versus-consumer structural-validation boundary.

### D-05 — Provider base path is preserved

The provider declares its real API base path. Piccolod composes the private
origin with that path and injects the result as `base_url`. It does not add a
fixed `/v1`, rewrite `/v1` to `/v3`, or introduce an API adapter. The `.v1` in
the capability name versions the Piccolo contract, not the provider's HTTP
path.

Evidence: `U80`, “provider ne base path provide kiya hai ... wahi hum base url
mein suffix ... beech mein v1 wala translation kyun necessary”.

### D-06 — Private local ingress; existing public/remote listener

Each consuming app uses a Piccolod-owned loopback ingress inside that app's
existing network namespace. Piccolod proxies it to the selected provider's
ordinary listener backend. The ingress itself establishes app ownership; the
caller is not inferred from an ephemeral source port and the consumer does not
need a new token or OAuth integration.

The provider's ordinary listener continues to supply LAN, remote/Nexus, and
listener-auth behavior. Capability binding adds no separate remote API,
authentication protocol, service mesh, or network plane.

Evidence: `U42`, “loopback ... bhi PicoloD ka proxy ka port”; `U43`, generic
publish/consume mechanism; `U69`-`U70`, existing listener path, remote access,
and authentication are sufficient.

### D-07 — Automatic default selection and opaque provider state

The first eligible provider becomes default automatically. Installing another
provider does not steal the default or require a blocking prompt. The user may
explicitly switch; removal of the default chooses a deterministic replacement
when available. A temporary failure does not silently switch providers. A
consumer may install and start without a usable provider; its stable private
endpoint returns HTTP 503 until the selected provider is usable.

Candidate eligibility and an existing selection are distinct. A manual Stop
retains the selected provider but withdraws its accelerator grant and leaves
consumers on HTTP 503; starting it restores the same selection. Uninstalling the
selected app or committing an update that removes its capability declaration
invalidates the selection and chooses the ordinary deterministic replacement,
or leaves the capability unavailable when none exists.

Piccolod does not migrate provider-owned models, indexes, history,
configuration, or other state. A switch/replacement disclosure is sufficient.
Every actual provider-identity change restarts affected consumers even when the
generated base path is unchanged. Running requests may be interrupted; no
capability-specific drain protocol is required.

Evidence: `U46`, “jitna automate ... state ... migrate ... zaroorat nahi ...
disclaimer”; `U61`, “#1 - agreed”, accepting install/start with an unavailable
provider and HTTP 503; `U97`, remove the same-path optimization; `U99`, “running
requests interrupt ho sakti hain ... disclaimer is sufficient”; `U141`,
accepting retained selection plus grant withdrawal/HTTP 503 on manual Stop and
deterministic replacement when uninstall or update removes the capability.

### D-08 — Capability-derived accelerator access

Accelerator access follows from a recognized capability and selection as its
default provider. It does not depend on Store provenance, maintainer identity,
ownership, a special system-app class, or a second hardware permission.
Catalog apps rely on catalog review; installing a custom provider is an
explicit user choice and is not artificially blocked.

Only the selected default receives the capability's accelerator devices.
Non-default providers remain ordinary apps under ordinary resource limits and
receive no capability-derived accelerator grant. In V1, the selected app
generation is the grant unit: all of its service containers receive the device
families. Piccolod does not add a `runtime_service` selector or infer which
container implements the listener.

Evidence: `U62`, “catalog se aaye custom app aaye ... koi bhi authorization ki
zarurat nahi”; `U63`, “resources bhi sirf default ke paas”; `U77`-`U78`,
“Haan, theek lag raha hai. To as a next step ... RFC”, accepting the immediately
preceding all-service-container grant proposal.

### D-09 — Standard Linux hardware environment

Piccolod detects and grants devices through standard PCI sysfs, DRM, and Linux
accelerator-class interfaces. The provider gets the standard read-only sysfs
view needed by upstream runtimes plus the available accelerator device families
under `/dev/dri` and `/dev/accel`.

Piccolod does not define a versioned hardware profile or choose
backend-specific render/NPU nodes. The provider's upstream runtime performs its
normal discovery.

Evidence: `U66`, “raw sysfs expose karna simpler aur better”; `U67`, “upstream
ko sara de sakte hain ... unnecessary restriction nahi”.

### D-10 — Provider owns inference implementation

The provider image owns OpenVINO, OVMS, vLLM, llama.cpp, or another userspace
runtime and decides backend selection, model loading, memory residency,
eviction, batching, and model/API behavior. Piccolod remains indifferent to
those internal choices. A fully stateless/read-only provider is allowed but is
not a platform requirement.

Evidence: `U65`, choose provider-packaged runtimes; `U68`, “vllm ya ovms ...
from Piccolo's perspective provider internally kya kar raha hai koi farak nahi”.

### D-11 — Piccolo artifacts are generic consumption relationships

A Piccolo artifact is a named, read-only manifest relationship through which an
app consumes reconstructible content. It is not an OCI artifact, a model-only
concept, or a separate physical storage type. Transport and storage management
remain artifact-agnostic; an individual provider may initially support only
particular formats.

Evidence: `U18`, “generic artifact”; `U24`-`U25`, “artifact transport and
storage ... management remains artifact-agnostic”; `U91`, distinguishes Piccolo
artifact use from OCI artifact format.

### D-12 — Digest is optional verification, not mandatory pinning

Mutable source references are allowed. A supplied digest verifies the resolved
content, but the manifest need not require one merely to work around an image
pull bug or impose immutability as a product policy. Piccolod records the
resolved immutable content identity so ordinary starts do not refetch it.

Evidence: `U16`, “digest nahi dalte ... immutability ki ... khas zarurat nahi”.

### D-13 — Hugging Face file or directory sources

A Hugging Face source may select one file, a directory subtree, or the whole
repository. Multi-file formats such as OpenVINO IR must materialize as one
verified directory tree; the single-file assumption is not an architectural
limitation.

Evidence: `U81`-`U82`, accepted replacing the single-file assumption with a
file-or-directory `path` contract.

### D-14 — OCI transport is delegated to Podman

OCI images and true OCI artifacts are pulled and materialized through the
existing scratch-backed Podman path. Piccolod places Podman's extracted output
in golden content and does not implement a custom registry resolver,
downloader, redirect/SSRF policy, media-type interpreter, or Piccolo-specific
OCI artifact layout.

Evidence: `U105`-`U107`, “Podman ... sab kuch ... bespoke SSRF ... over
complicate”, followed by “correct” and “freeze”.

### D-15 — One canonical golden LV may serve multiple uses

Resolved immutable content materializes as a golden LV. “Root filesystem” and
“artifact” describe how an app consumes that content, not different physical
content types. The same OCI image identity must reuse the same golden LV when
one app uses it as a rootfs and another mounts it as a Piccolo artifact.
Existing ownership/reference tracking must cover both uses; shared golden
content cannot be collected until every rootfs and artifact use has released
it.

Artifact content stays off the core volume. In a future multi-node deployment,
ready golden content follows the existing one-time peer pre-seeding/block-copy
architecture; peers do not each download it independently and it does not
require continuous DRBD replication.

Evidence: `U90`, artifact “golden LV ke roop mein hi ... persist”; `U91`, “same
image ... artifact ... root fs ... exact same golden LV ... reuse”; `U89`-`U90`,
accepted peer pre-seeding after the DRBD distinction was clarified; `U92`,
“haan sahi hai”, accepting the immediately preceding shared-reference/GC rule.

### D-16 — Reuse existing lifecycle and storage paths narrowly

Existing install, clone, update, and uninstall paths remain responsible for
their own capability/artifact effects and cleanup. Small shared helpers are
allowed for repeated mechanics, but the feature does not create a common
durable lifecycle coordinator. Artifact creation uses the existing golden-LV
capacity checks and pool guard rather than a global space-reservation system.

Evidence: `U94`, “coordinator ko ... completely hata dena”; `U112`, “shared
helper ... simple ... still robust”.

### D-17 — Generic capability failure contract

Piccolod-defined binding failures use standard HTTP status codes. Exact
OpenAI-shaped JSON error bodies are not part of the generic capability
contract.

Evidence: `U115`, “generic across capability providers ... standard HTTP status
hi”.

### D-18 — Pending artifact work has no capability authority

An accepted artifact download or materialization does not reserve or freeze a
capability default, provider selection, binding, or accelerator grant. Those
outcomes always follow the independently authoritative capability state at
publication/commit time.

Consequently, another provider installed or selected while provider A is
downloading remains authoritative; A cannot later steal that default merely
because its work started earlier. If no default exists when A becomes
publishable, the ordinary first-provider rule may select it. A consumer binds
to the current default, or remains installed with HTTP 503 when none is usable.
Capability-state drift alone does not require `ReviewRequired`,
resume/discard, or a pending-install reservation.

This does not loosen artifact content identity: the immutable identity resolved
for the accepted artifact work remains bound and verified independently.

Evidence: `U124`, “B ... default select ho gaya hai to wo ... independently
authoritative state hai; A ka download ho raha hai nahi ho raha kya farak
padta hai”.

### D-19 — Every source uses one golden-materialization lifecycle

OCI images, OCI artifacts, and Hugging Face files/directories use one generic
golden-content materialization lifecycle. Common ownership includes staging LV
creation, encryption/filesystem preparation, long-running work, restart/retry,
progress, capacity handling, Ready publication, deduplication, references, and
GC. A small source follows the same path and merely completes quickly.

Only the source adapter differs: Podman image flattening, Podman artifact
extraction, or Hugging Face snapshot projection. Once Ready, rootfs and artifact
consumption differ only in snapshot versus read-only attachment.

An artifact-bearing app therefore does not require a separate install preflight
or lifecycle redesign. Its accepted ordinary app lifecycle invokes the same
golden materializer used by images.

Evidence: `U126`, “artifact support ko ... normal image download se separate
kyon ... mechanism ... general golden LV creation ... applicable”; `U127`,
“lock karte hain”.

### D-20 — Pinned and unpinned source intent remain distinct

An explicitly supplied digest/immutable revision is an exact verification
constraint. Without one, a mutable tag, branch, or revision may legitimately
resolve to different content at different resolution events.

Each individual resolution/materialization attempt still binds one concrete
descriptor digest or commit and obtains all bytes consistently from that
identity. The recorded resolved identity is the golden-content key and Ready
evidence for that attempt; it is not a permanent pin added to the manifest.
When a later resolution event produces the same identity, reuse the Ready
golden LV. When it produces a different identity, materializing new golden
content is valid.

Evidence: `U126`, “manifest mein digest pinned hai to nahi change hoga, aur nahi
hai to change hone mein dikkat kya hai”; `U127`, “lock karte hain”.

### D-21 — Reuse the existing app leadership lifecycle

Capability and artifact work runs inside the existing leader-owned app
lifecycle. The ordinary kernel-leader install gate decides whether new install
work may begin, and the existing per-app leadership lifecycle owns runtime
quiescence. OCI image and artifact materialization do not create a parallel
leadership authority.

V1 therefore adds no capability/artifact-specific feature policy, leadership
generation protocol, stale-work token, demotion barrier, or durable
effect-executor state. The current product is single-node; generic
install/materialization behavior across a future real leadership change must be
solved once in the existing cluster lifecycle for ordinary OCI pulls and
artifacts together.

Evidence: `U129`, “simply bas leader ka check ... already app installation ke
upar”; `U130`, agreement that the existing gate is sufficient and the separate
machinery was premature multi-node work.

### D-22 — Separate architecture, specification, and implementation detail

The RFC records product decisions, externally observable behavior, and
load-bearing invariants. Exact public manifest/API schemas and validation rules
belong in `docs/app-platform/specification.yaml`. File/function work breakdown,
internal sequencing, exhaustive crash permutations, exact UX copy, and
individual test enumeration belong in a later implementation plan.

Failure semantics remain RFC contracts when externally meaningful, but the RFC
does not prescribe an implementation state machine merely to demonstrate that
contract. Document volume is not evidence of architectural completeness.

Evidence: `U132`, agreement with the proposed RFC/specification/implementation
plan boundary.

### D-23 — OCI source projection is Podman-classified

`artifacts.source.type: oci` has no author-supplied image-versus-artifact
discriminator. Piccolo consumption intent is already expressed by declaring a
Piccolo artifact; registry packaging does not become another app-facing
semantic requirement.

Piccolod first invokes the ordinary Podman image materialization path. If
Podman succeeds, the resolved image's flattened golden LV is reused. Only
Podman's specific non-image-artifact classification selects native artifact
pull/extraction; unrelated transport, authentication, digest, or capacity
errors do not trigger fallback. Thus Podman, not Piccolod media-type logic or a
manifest `kind`, decides the OCI projection.

Evidence: `U134`, rejecting an explicit source kind as semantically redundant
and requiring actual Podman cross-behavior to be tested rather than assumed.

### D-24 — Providers boot without accelerator authority; handoff is sequential

Declaring a capability does not guarantee its accelerator grant. A provider
must start and bind its declared listener without capability-derived devices;
it may remain idle or report the capability unavailable until selected. After
the app installation commits and it becomes the default, Piccolod recreates it
with the grant. Losing the grant likewise uses container recreation rather than
device hot-plug.

Only the committed selected provider receives the accelerator. Piccolod does
not give an installing or switching candidate a temporary grant and does not
overlap old and new accelerator ownership. Provider handoff remains sequential;
the already accepted consumer interruption is sufficient.

Evidence: `U142`, “accelerator ke bina boot karna possible hona chahiye ... koi
bhi provider ... nahi maan ke chal sakta ki usko accelerator milega”; `U144`,
“dual clutch ko ditch karte hain”.

### D-25 — The provider owns its declared private capability surface

`base_path` is the provider's explicit declaration of which listener subtree
private capability consumers may access without the listener's public
authentication flow. Piccolod enforces that declared boundary but does not
interpret endpoint purpose or protect a provider from its own declaration.

`base_path: /` is valid and intentionally exposes the provider's whole listener
through the private binding. A non-root path confines access to that canonical
segment subtree. In both cases, authorization and forwarding use the same
canonical path representation so a request cannot escape the declared surface.

Evidence: `U145`, “provider ne apna base path ... expect kar raha hai consumer
is pe request marega to ye uski responsibility hai ... humme beech mein padne
ki kya zarurat”.

## Explicitly rejected or superseded mechanisms

| ID | Do not reintroduce without explicit reopening | Evidence |
|---|---|---|
| R-01 | Trusted Store URL, maintainer, provenance, or `system` app type as the runtime accelerator authority. This supersedes the earlier `U30` Store-trust acceptance. | `U62` |
| R-02 | OAuth `client_credentials`, injected bearer tokens, a bespoke AI credential store, or a separate internal network plane for local capability calls. | `U39`-`U42` |
| R-03 | Inferring caller identity from ephemeral TCP source ports or process lookup. | `U40`-`U42` |
| R-04 | Automatically generated capability environment-variable names alongside explicit `consumes.env`. | `U58` |
| R-05 | Piccolo-specific named/versioned hardware profiles or inventories that upstream runtimes must understand. | `U66` |
| R-06 | Piccolod choosing a subset of DRM/NPU nodes on behalf of upstream runtimes. | `U67` |
| R-07 | Piccolod-owned backend selection, model residency, eviction, batching, or an inference runtime implementation. | `U68` |
| R-08 | Capability semantic/conformance probing by Piccolod. | `U73`-`U75` |
| R-09 | A virtual `/v1` path, `/v1` to provider-path rewrite, or an OpenAI adapter in Piccolod. | `U80` |
| R-10 | Mandatory digest pinning as a product policy. | `U16` |
| R-11 | A separate artifact LV/storage type or separate OCI image flattener for artifact use. | `U90`-`U96` |
| R-12 | A custom OCI resolver/downloader, bespoke OCI-only SSRF layer, media-type interpretation, or custom artifact projection layout. | `U105`-`U107` |
| R-13 | Multi-node feature/materializer voting, version floors, or similar coordination in V1. | `U89`-`U92` |
| R-14 | A global thin-pool reservation coordinator introduced by this feature. | `U94` |
| R-15 | Avoiding consumer restarts merely because old and new providers yield the same path. | `U97` |
| R-16 | Capability-specific graceful stream draining during provider switches. | `U99` |
| R-17 | Automatic Store capability indexing/discovery in V1. | `U100` |
| R-18 | User-facing manual artifact-download cancellation API/UI. | `U110` |
| R-19 | A common durable install/clone/update/uninstall transaction coordinator. | `U112` |
| R-20 | Selecting a provider listener in the default API or exposing `base_path` in ordinary capability inventory/UI. | `U114` |
| R-21 | Exact OpenAI-shaped Piccolod error bodies. | `U115` |
| R-22 | Reserving/freezing a capability outcome for pending artifact work, or creating capability-only `ReviewRequired`/resume/discard states when authoritative default or binding state changes. | `U124`; see D-18. |
| R-23 | Artifact-specific download, preflight, durability, recovery, or storage machinery justified by assuming artifacts are large and ordinary images are small. | `U126`-`U127`; see D-19 and P-01/P-04. |
| R-24 | Treating an internally resolved digest/commit as a permanent product-level pin for an unpinned manifest, or forbidding legitimate content changes between later resolution events. | `U126`-`U127`; see D-20 and P-03. |
| R-25 | Capability/artifact-specific `FeaturePolicy`, leadership generations, stale-work tokens, demotion/effect barriers, or durable effect-executor ownership layered beside the existing app leadership lifecycle. | `U129`-`U130`; see D-21. |
| R-26 | Keeping file/function site lists, internal state-machine prescriptions, exhaustive crash/test matrices, or exact UX prose in the RFC when they are not externally observable product contracts. | `U131`-`U132`; see D-22. |
| R-27 | Requiring an app author to declare `kind: image | artifact` for an OCI source, or making that packaging choice part of Piccolo artifact consumption semantics. | `U134`; see D-23. |
| R-28 | Giving a pre-commit or switching candidate a temporary accelerator grant, overlapping old and new providers as a “dual-clutch” handoff, or adding candidate hardware authority to reduce accepted switch downtime. | `U143`-`U144`; see D-24. |
| R-29 | Forbidding `base_path: /`, requiring a capability-dedicated listener, or adding endpoint-purpose review so Piccolod can protect a provider from the private surface it explicitly declared. | `U145`; see D-25. |

## Deferred decisions

| ID | Deferred item | Evidence or boundary |
|---|---|---|
| F-01 | Provider implementation: initial maintained image, OVMS versus vLLM versus llama.cpp, backend choice, batching, memory residency, exact RAM-percentage limits, and model policy. | `U68`; provider is opaque to Piccolod. |
| F-02 | Provider-state migration or standardization across default changes. | `U46`; disclaimer is sufficient. |
| F-03 | Multi-node feature/version coordination and remote effect execution. | `U89`-`U92`; V1 remains bounded while preserving peer pre-seeding as the eventual storage path. |
| F-04 | Capability-indexed Store discovery/ranking. | `U100`. |
| F-05 | Consumer-specific post-materialization interpretation, conversion, archive extraction, shard handling, or model validation by Piccolod. Podman's OCI pull/extract step in D-14 is not deferred. | `U24`-`U25`, `U105`-`U107`; consuming app owns content meaning. |
| F-06 | When an unpinned source is resolved again: app start, app update, explicit refresh, or a future automatic cadence. | `U126`-`U127`; mutability semantics are locked by D-20, while refresh timing remains a separate policy decision. |

## Resolved simplification questions

### O-01 — Resolved by D-18 on 2026-07-23

Pending artifact work has no capability authority. Current authoritative
default/binding/grant state wins, and capability drift does not create a fresh
review or reservation lifecycle. Artifact content identity remains bound
separately.

### O-02 — Resolved by D-19 and D-20 on 2026-07-23

Artifact-bearing installs do not receive a special preflight or lifecycle.
Every source uses the common golden materializer, and only the source adapter
differs. A pinned source remains exact; an unpinned source may change between
resolution events while each individual attempt remains internally consistent.
The cadence for future re-resolution is deferred separately as F-06.

### O-03 — Resolved by D-21 on 2026-07-23

V1 reuses the existing kernel-leader install gate and per-app leadership
lifecycle. It does not add a capability/artifact-specific multi-node protocol.
Generic in-flight install behavior across future real failover belongs to the
existing cluster lifecycle and is deferred with multi-node enablement.

### O-04 — Resolved by D-22 on 2026-07-23

The RFC retains decisions, observable contracts, and load-bearing invariants.
Exact public schemas remain in the specification. Internal implementation
mechanics and exhaustive test/UX enumeration move to a later implementation
plan.

## Open simplification questions

None.

## Review and update guardrails

1. Resolve one open product question at a time.
2. Update this ledger immediately after the user resolves a question, before
   propagating the result into the RFC or specification.
3. A review finding should map to a locked outcome or an existing touched
   invariant. A newly discovered correctness or security collision may instead
   request a new or reopened decision, but the reviewer's preferred mechanism
   does not automatically become a requirement.
4. Rejected mechanisms remain rejected even when a reviewer finds them
   convenient. Reopening one requires presenting the concrete collision to the
   user and recording the new decision here.
5. Decision IDs are immutable. A later explicit user decision may mark an old
   entry **Superseded**, but the old entry remains with `superseded_by`, date,
   and replacement evidence.
6. After all open questions are resolved, run one soundness review and then one
   minimizer pass against the original objective and this ledger. Do not restart
   an unbounded review/minimization loop.
7. After propagating ledger decisions, record the RFC/spec revision or commit
   audited and list any decision IDs that are not yet reflected. Source coverage
   and document propagation are separate checks.
