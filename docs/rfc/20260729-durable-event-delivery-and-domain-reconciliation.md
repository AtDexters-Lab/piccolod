# RFC Placeholder: Durable Event Delivery and Domain Reconciliation

**Status:** Placeholder; not implementation-approved and not ready for plan
review.

## Scope

### Problem

Piccolod currently mixes direct cross-domain calls with a best-effort
in-process event bus, so correctness can depend on an event that may be
dropped, reordered, or lost on restart, while retries can repeat an effect
whose durable outcome is uncertain.

### In scope

Define a future incremental architecture in which durable desired state or an
existing durable transition remains the source of truth; state changes can
atomically publish durable outbox records; domain owners reconcile their
resources idempotently; and consumers can resume delivery after restart.

### Out of scope

The `v0.2.43` unlock and app-lifecycle remediation; replacing the current event
bus during that work; full event sourcing; an external message broker;
exactly-once delivery or side effects; rewriting app install; moving every
existing event consumer in one release; and creating separate services merely
to satisfy a domain-manager label.

## Locked Direction

This RFC records a follow-up obligation without expanding the current incident
fix:

- Current remediation continues to use the existing per-app durable transition
  for multi-step app operations and narrow idempotent calls to resource owners.
- Durable desired state or the active transition is authoritative. Events do
  not become an independent source of product state.
- Backend projections remain authoritative for clients. UI consumers receive a
  wake or notification and refetch current state instead of maintaining a
  competing state machine.
- Durable delivery is expected to be at-least-once. Consumers must tolerate
  duplicates and replay; a crash after an external effect but before
  acknowledgement must converge safely.
- A durable event guarantee requires the state mutation and outbox append to
  share one atomic commit, plus durable consumer progress. Persisting the event
  alone is insufficient.
- Resource ownership stays with the narrow manager that can inspect and mutate
  that resource. A "domain manager" may be an existing component with a clearer
  interface; it does not imply a new process or background service.

## Candidate Shape

The detailed design is deliberately deferred. The future RFC revision should
evaluate this smallest coherent protocol:

1. A command or reconciler writes authoritative desired state.
2. The same transaction appends an outbox wake record.
3. A domain consumer receives or replays that record and re-reads authoritative
   state rather than trusting an embedded state delta.
4. The domain owner compares desired state with the actual resource and applies
   an idempotent correction.
5. Consumer progress advances only after convergence or a durably classified
   terminal outcome. Retryable failures remain visible and retry indefinitely
   with capped backoff.

The existing in-process bus may remain the low-latency fan-out path. The open
design question is whether a small outbox dispatcher feeds that bus, whether
selected consumers read the outbox directly, or whether both are needed.

## First Candidate Migration

OIDC client lifecycle is a useful first candidate because ownership is
currently split across install, listener-update, restore observers, catalog
sync, and uninstall paths. A later design must preserve the install-time
requirement that some credentials exist before manifest rendering and access
publication; a post-install event alone cannot satisfy that requirement.

This candidate is not authorization to rewrite OIDC install in the current
incident slice. Current remediation changes only uninstall cleanup ownership:
the durable uninstall transition asks the OIDC client owner to ensure that the
app's clients are absent and waits for the confirmed idempotent result.

## Questions Required Before Plan Review

- Which durable store owns the outbox while the control store is locked?
- What ordering is required globally, per app, and per resource?
- How are consumer identity, acknowledgement, retry, retention, compaction, and
  poison records represented?
- Which effects can be inspected after an ambiguous crash, and which require a
  durable idempotency key or compensation?
- How do startup recovery, shutdown cancellation, supersession, and concurrent
  desired-state changes compose?
- Which existing event topics are correctness-bearing today, and which are
  already only UI or telemetry notifications?
- What evidence justifies the first migration and proves that a shared protocol
  is smaller than an owner-local durable transition?

## Preliminary Affected Sites

This is an orientation list, not the complete implementation site list required
before plan review:

- `internal/events/bus.go` — current volatile, best-effort fan-out.
- `internal/persistence/sqlite_control_store.go` and
  `internal/persistence/interfaces.go` — candidate atomic state/outbox boundary
  and durable consumer progress.
- `internal/app/installed_app_transition.go` — existing durable per-app
  transition authority that must not be duplicated.
- `internal/oidc/client.go` — candidate OIDC resource owner.
- `internal/server/gin_app_handlers.go`,
  `internal/server/gin_oidc_handlers.go`, and
  `internal/server/catalog_sync_host.go` — current procedural OIDC call sites.
- `internal/server/gin_server.go` — current event observers and runtime
  start/stop ownership.
- `docs/runtime/runtime-architecture-roadmap.md` — existing desired-state and
  event-driven direction to amend when this design is accepted.
- `docs/rfc/20260621-installed-app-transition-boundary-v2.md` — existing
  durable app-transition contract with which this work must compose.

## Temporal Composition Gate

Before this placeholder enters plan review, it must define the complete
transition surface for publish, delivery, duplicate replay, consumer failure,
restart, cancellation, supersession, partial effect/ack success, retention, and
concurrent reordering. It must also enumerate the final site list and
adversarial tests. Until then, no implementation should cite this document as
an accepted delivery protocol.

## Implementation Notes & Status

- 2026-07-29: Placeholder created to retain the architectural follow-up while
  keeping the `v0.2.43` incident remediation independent and bounded.
- No implementation has been approved or landed under this RFC.
