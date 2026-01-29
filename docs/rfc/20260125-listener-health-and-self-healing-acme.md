# RFC: Listener Health Status and Self-Healing ACME

- **Status:** Draft
- **Date:** 2026-01-25
- **Authors:** Engineering Team
- **Reviewers:** @piccolo-os/core

## 1. Summary

This RFC proposes a comprehensive listener health system and self-healing ACME certificate issuance mechanism. The changes address a critical UX gap where users see grey screens when remote access fails, with no indication of what's wrong or when it will be fixed.

Key components:
1. **Per-listener health status model** with defined states and recovery semantics
2. **Indefinite ACME retry** with rate limit awareness (no more "max attempts" dead-ends)
3. **UI health policies** for graceful degradation and local fallback
4. **App-specific health warnings** on dashboard cards and detail pages

## 2. Motivation

### 2.1 The Grey Screen Problem

A bug in the proxy layer (ACME HTTP-01 challenges blocked by auth middleware) caused certificate issuance failures. Users accessing apps via remote URLs saw:
- Grey screen (TLS handshake fails before any HTTP response)
- No error message or context
- No guidance on what to do

The user had no way to know:
- Remote access was failing
- The system was attempting to fix it
- They could access locally as a workaround

### 2.2 Dead-End After Max Attempts

The current ACME retry logic gives up after 10 attempts:

```go
if attempts > maxCertAttempts {
    m.updateCertFailure(job.id, "max issuance attempts reached")
    return  // Dead end - no more automatic retries
}
```

This doesn't account for transient infrastructure issues that self-resolve (DNS propagation, network outages, etc.). The system should be self-healing.

### 2.3 No Proactive Communication

Certificate status is only visible in Settings > Remote. There's no:
- App-specific health indicator
- Warning banner on affected apps
- Guidance on workarounds (local access)

### 2.4 Rate Limit Blindness

ACME rate limit errors from Let's Encrypt are treated like any other failure. The system should:
- Detect rate limits explicitly
- Back off appropriately (hours/days, not minutes)
- Communicate the specific situation to users

## 3. Goals & Non-Goals

### 3.1 Goals

- **Self-healing:** System recovers automatically from transient failures
- **User awareness:** Clear, contextual health status visible where users need it
- **Graceful degradation:** Local fallback when remote access fails
- **Rate limit compliance:** Respect Let's Encrypt rate limits to avoid blocks
- **Actionable guidance:** Tell users what's happening and what they can do

### 3.2 Non-Goals

- Real-time TLS handshake error interception (impossible at TLS layer)
- Automatic DNS configuration for users
- Indefinite retry for misconfigured domains (user error) - these are classified as `config_error` with weekly probes and manual retry option (see §5.3.1)

## 4. Listener Health Model

**Scope distinction:** This `ListenerHealth` model is **domain-specific** to app listener accessibility (certificate status, backend connectivity). It is intentionally separate from the existing generic system/component health framework at `internal/health/tracker.go` (which powers `/api/v1/health/*` for system diagnostics like storage, network, services). These are distinct concerns:

| Framework | Purpose | API | Examples |
|-----------|---------|-----|----------|
| `internal/health/tracker.go` | System component health | `/api/v1/health/*` | Storage, network, control plane |
| `ListenerHealth` (this RFC) | App listener accessibility | `/api/v1/apps` response | Cert status, backend reachability |

Do not conflate these or try to merge them into a single "health" abstraction—they serve different audiences (ops/debugging vs end-user UX).

### 4.1 Health Status Enum

```go
type ListenerHealthStatus string

const (
    // ListenerHealthOK: Fully operational, cert valid, backend reachable
    ListenerHealthOK ListenerHealthStatus = "ok"

    // ListenerHealthDegraded: Working but with warnings (still usable)
    // Examples: cert expiring soon (<7 days), high backend latency
    ListenerHealthDegraded ListenerHealthStatus = "degraded"

    // ListenerHealthRecovering: Not working, automatic fix in progress
    // Examples: cert issuance pending, retry scheduled
    ListenerHealthRecovering ListenerHealthStatus = "recovering"

    // ListenerHealthError: Not working (unusable), may or may not require user action
    // Examples: backend unreachable, rate limited (long wait), DNS misconfiguration
    // Note: Recoverable=true means system will auto-fix; ActionRequired=true means user must act
    ListenerHealthError ListenerHealthStatus = "error"
)
```

### 4.2 Health Data Structure

```go
type ListenerHealth struct {
    Status         ListenerHealthStatus `json:"status"`
    ReasonCode     string               `json:"reason_code"`               // Machine-readable code (stable for UI mapping, dedup, i18n)
    Reason         string               `json:"reason"`                    // Human-readable explanation (always present)
    Details        *string              `json:"details,omitempty"`         // Technical details (nil when not applicable)
    RecoveryETA    *time.Time           `json:"recovery_eta,omitempty"`    // When next retry/check will occur (nil if not scheduled)
    Recoverable    bool                 `json:"recoverable"`               // Can system self-heal without user action?
    ActionRequired bool                 `json:"action_required"`           // Does user need to do something?
    CertStatuses   map[string]CertHealthStatus `json:"cert_statuses,omitempty"` // Per-cert health (certID → status). Empty if no certs needed.
    LastChecked    time.Time            `json:"last_checked"`
    LastOK         *time.Time           `json:"last_ok,omitempty"`         // When backend was last healthy (nil if never seen healthy)
}

// CertHealthStatus tracks individual certificate health for multi-cert listeners.
// The overall ListenerHealth.Status is the worst-of all CertStatuses + backend health.
type CertHealthStatus struct {
    Status      ListenerHealthStatus `json:"status"`       // ok, recovering, error
    ReasonCode  string               `json:"reason_code"`  // e.g., "cert_pending", "cert_dns_error"
    RecoveryETA *time.Time           `json:"recovery_eta,omitempty"`
}
```

**Field semantics:**

| Field | Type | Optionality | UI Treatment |
|-------|------|-------------|--------------|
| `ReasonCode` | `string` | Required | Used for i18n keys, event deduplication, UI conditionals |
| `Reason` | `string` | Required | Always displayed prominently |
| `Details` | `*string` | Optional (omitempty) | Hidden by default; "Show details" toggle only if non-nil |
| `RecoveryETA` | `*time.Time` | Optional (omitempty) | Shows "Next retry: X" (auto-recoverable) or "Next check: X" (action_required=true) |
| `Recoverable` | `bool` | Required | If `false`, UI shows "requires your action" styling |
| `ActionRequired` | `bool` | Required | If `true`, UI shows actionable guidance |
| `CertStatuses` | `map[string]CertHealthStatus` | Optional (omitempty) | Per-cert health breakdown. Empty if no certs needed (local-only or remote disabled). |
| `LastOK` | `*time.Time` | Optional (omitempty) | When backend was last healthy. Shows "Was healthy X minutes ago" for `backend_unreachable`. Nil if never seen healthy. |

**Invariant:** `Recoverable == false` implies `ActionRequired == true` (if system can't fix it, user must).

**Health aggregation:** The top-level `Status` is the **worst-of** all `CertStatuses` + backend health. Priority order: `error` > `recovering` > `degraded` > `ok`. **Tie-breaker:** When multiple certs have the same status, prefer `actionRequired=true` (config errors) over auto-recoverable errors—this ensures actionable issues surface to users. This allows the badge to show the most actionable issue while `CertStatuses` provides drill-down detail.

**ReasonCode values (canonical namespace):**

All certificate-related codes use `cert_*` prefix for consistency across Certificate.FailureCode, ListenerHealth.ReasonCode, and UI/i18n keys.

| ReasonCode | Status | Recoverable | ActionRequired | Description |
|------------|--------|-------------|----------------|-------------|
| `cert_pending` | recovering | true | false | Certificate issuance in progress |
| `cert_retry_scheduled` | recovering | true | false | Issuance failed, retry scheduled |
| `cert_rate_limited` | error | true | false | Let's Encrypt rate limit (long wait) |
| `cert_dns_error` | error | false | true | DNS not configured correctly |
| `cert_domain_unreachable` | error | false | true | Domain doesn't resolve to Piccolo |
| `cert_caa_forbidden` | error | false | true | CAA record blocks Let's Encrypt |
| `cert_connection_failed` | recovering* | true | false | Can't reach domain from internet (see hybrid handling §5.3.2) |
| `cert_unauthorized` | recovering | true | false | Challenge verification failed (transient) |
| `cert_acme_error` | recovering | true | false | Other ACME error (transient) |
| `cert_unknown_error` | recovering | true | false | Non-ACME error |
| `cert_expiring_soon` | degraded | true | false | Certificate expires within 7 days |
| `backend_unreachable` | error | true | false | App backend not responding (unusable) |
| `ok` | ok | true | false | Fully operational |

*`cert_connection_failed` starts as transient/recovering but escalates to error/action_required after persistent failures (see §5.3.2).

**Example: Non-recoverable config error:**
```go
ListenerHealth{
    Status:         ListenerHealthError,
    ReasonCode:     "cert_dns_error",
    Reason:         "Domain not configured",
    Details:        strPtr("DNS lookup for immich.example.com returned NXDOMAIN"),
    RecoveryETA:    cert.RetryAt, // Weekly probe scheduled (7 days)
    Recoverable:    false,        // Requires user action to fully fix
    ActionRequired: true,
}
// Note: RecoveryETA for config errors is the weekly probe time, not a "fix" time.
// The probe will re-check if user fixed the issue externally.
```

### 4.3 Listener → Certificate Mapping

Health derivation requires knowing which certificate(s) a listener depends on. The mapping rules are:

**Remote vs LAN hostname schemes:**
- **Remote**: `<label>.<portal>` (dot separator) - e.g., `immich.portal.example.com`
- **LAN (mDNS)**: `<label>-<base>` (hyphen separator) - e.g., `immich-piccolo.local`

Certificate IDs use the **remote** hostname scheme since certs are for remote TLS.

```go
// Remote hostname derivation already exists at internal/server/gin_server.go:1145
// (remoteServiceHostname). We extract that logic to a shared helper:
//
//   func remoteServiceHostname(derivedHostLabel, portalHostname string) string {
//       if derivedHostLabel == "" {
//           return ""
//       }
//       base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(portalHostname)), ".")
//       return strings.ToLower(derivedHostLabel) + "." + base
//   }
//
// This follows RFC 20260114: remote hostnames are <label>.<portal> (dot separator),
// distinct from LAN mDNS which uses hyphen separator.

// resolveCertificatesForListener determines which certificates a listener's health depends on.
// Returns (certIDs, requiresRemoteCert). If requiresRemoteCert is false, the listener
// doesn't need certificates (local-only or remote disabled).
//
// A listener may have MULTIPLE certificates:
// - Default remote hostname cert (wildcard or per-host depending on solver)
// - One or more alias certs (custom domains pointed to this listener)
//
// Health tracks ALL of them: if any cert has issues, the listener shows degraded/error
// for that access path. UI can show which specific certs are affected.
//
// Note: Aliases are stored in remote.Config.Aliases (keyed by listener name), not on
// ServiceEndpoint. The alias inventory must be passed separately.
func resolveCertificatesForListener(ep ServiceEndpoint, remoteStatus *remote.Status, aliases []remote.Alias) ([]string, bool) {
    // 1. Remote access disabled → no certificates needed
    if remoteStatus == nil || !remoteStatus.Enabled {
        return nil, false
    }

    var certIDs []string
    portal := remoteStatus.PortalHostname

    // 2. Resolve default remote hostname cert (if listener has derived host label)
    if ep.DerivedHostLabel != "" {
        switch remoteStatus.Solver {
        case "dns-01":
            // DNS-01 uses wildcard cert for all listeners
            certIDs = append(certIDs, "wildcard")

        case "http-01":
            // HTTP-01 issues per-listener certs based on derived remote hostname
            derivedHost := remoteServiceHostname(ep.DerivedHostLabel, portal)
            certIDs = append(certIDs, "host:"+derivedHost)
        }
    }

    // 3. Add any alias certs for this listener
    // Current schema: Alias.Listener stores listener name only (e.g., "web"), not "app/listener".
    // This matches gin_oidc_handlers.go:224 which compares `alias.Listener == ep.Name`.
    // Note: This means aliases are per-listener across ALL apps. If two apps have a "web"
    // listener, an alias for "web" applies to both. This is the current behavior.
    for _, alias := range aliases {
        if alias.Listener == ep.Name {
            certIDs = append(certIDs, "alias:"+alias.Hostname)
        }
    }

    // 4. If no certs found (raw/tls listener with no aliases), no cert needed
    if len(certIDs) == 0 {
        return nil, false
    }

    return certIDs, true
}
```

**Certificate ID scheme (matches `internal/remote/manager.go`):**

| ID Format | Description | Example |
|-----------|-------------|---------|
| `portal` | Portal hostname cert | `portal` |
| `wildcard` | Wildcard cert for dns-01 | `wildcard` |
| `host:<hostname>` | Per-listener cert for http-01 | `host:immich.portal.example.com` |
| `alias:<hostname>` | User-configured custom domain | `alias:photos.example.com` |

**Note on `portal` cert:** The `portal` cert covers the root portal hostname (e.g., `portal.example.com`) and is used for piccolod's main web UI, not app listeners. It is **not** tracked in `ListenerHealth.CertStatuses`—portal health is a system-wide concern shown in Remote Access settings, not per-app. If the portal cert fails, users accessing the portal UI directly will see a TLS error (this is rare since most users access via `piccolo.local` on LAN).

**Mapping summary (for listener health):**

| Condition | Certificate ID | Notes |
|-----------|---------------|-------|
| Remote disabled | (none) | No cert needed; health still reports backend status |
| No DerivedHostLabel | (none) | Raw/TLS listeners don't get host-based certs |
| Listener has alias | `alias:<hostname>` | User-configured custom domain |
| Solver = dns-01 | `wildcard` | Shared wildcard cert for all listeners |
| Solver = http-01 | `host:<label>.<portal>` | Per-listener cert, e.g., `host:immich.portal.example.com` |

**Health field presence:**
`primary_listener_health` is **always present** for running apps. It reports:
- **Backend connectivity** (always checked, regardless of remote setting)
- **Certificate status** (only when remote is enabled and `requiresRemoteCert == true`)

This ensures app cards always show health badges for backend issues, even when remote access is disabled.

**Health semantics for special app states:**

| App State | `primary_listener_health` | Dashboard Badge | Rationale |
|-----------|---------------------------|-----------------|-----------|
| **Running** | Present, computed normally | Shows status | Normal case |
| **Stopped** | `null` | No badge (grey/inactive styling) | Backend check is meaningless for stopped apps. Don't show `backend_unreachable` for expected state. |
| **Raw-only** (no HTTP/WS) | `null` | No badge | No primary listener exists. Raw listeners don't participate in host-based routing. |
| **Starting** | Present, `status: recovering`, `reason_code: app_starting` | Yellow spinner | Transient state; backend not yet ready |
| **Error** (failed to start) | Present, `status: error`, `reason_code: app_error` | Red badge | App crashed or failed health check |

**Implementation:**
```go
func (s *ServiceManager) computeAppHealth(inst *app.AppInstance) *ListenerHealth {
    // Stopped apps don't have health (expected state, not an error)
    if inst.Status == app.StatusStopped {
        return nil
    }

    // Raw-only apps have no primary listener
    primaryName, _ := hostname.ResolvePrimaryListener(inst.Manifest.Listeners)
    if primaryName == "" {
        return nil
    }

    // ... normal health derivation
}
```

**Primary listener selection:** Uses existing `hostname.ResolvePrimaryListener()` from `internal/hostname/hostname.go`:
1. If a listener has `Primary=true`, it becomes primary (only one allowed)
2. `Primary=true` is not allowed on `flow:tls` or `protocol:raw` listeners
3. If no explicit primary, the **first HTTP/WebSocket listener** becomes primary
4. Returns `""` if there are no eligible primary listeners (raw-only apps)

### 4.4 Health Derivation Logic

Listener health is derived from multiple signals. Uses failure classification (§5.3) for determining recoverability:

```go
// Helper for optional string fields
func strPtr(s string) *string {
    if s == "" {
        return nil
    }
    return &s
}

// deriveListenerHealth computes health from multiple signals.
// Parameters:
//   - certIDs: certificate IDs this listener depends on (from resolveCertificatesForListener)
//   - certs: map of certID → Certificate for lookup
//   - backendOK: whether backend TCP check passed
//   - lastOK: when backend was last healthy (from BackendHealthState.lastOK), nil if never seen healthy
//
// Returns aggregated health with per-cert breakdown in CertStatuses.
func deriveListenerHealth(certIDs []string, certs map[string]*Certificate, backendOK bool, lastOK *time.Time) ListenerHealth {
    certStatuses := make(map[string]CertHealthStatus)

    // Track worst status for aggregation (error > recovering > degraded > ok)
    worstStatus := ListenerHealthOK
    worstReasonCode := "ok"
    worstReason := "Operational"
    var worstDetails *string
    var worstRecoveryETA *time.Time
    recoverable := true
    actionRequired := false

    // 1. Check each certificate
    for _, certID := range certIDs {
        cert := certs[certID]

        var cs CertHealthStatus
        if cert == nil {
            // Certificate not yet issued
            cs = CertHealthStatus{
                Status:     ListenerHealthRecovering,
                ReasonCode: "cert_pending",
            }
        } else {
            switch cert.Status {
            case "error":
                switch cert.FailureClass {
                case FailureClassRateLimited:
                    cs = CertHealthStatus{
                        Status:      ListenerHealthError,
                        ReasonCode:  "cert_rate_limited",
                        RecoveryETA: cert.RetryAt,
                    }
                case FailureClassConfigError:
                    cs = CertHealthStatus{
                        Status:      ListenerHealthError,
                        ReasonCode:  cert.FailureCode,
                        RecoveryETA: cert.RetryAt,
                    }
                default: // Transient
                    cs = CertHealthStatus{
                        Status:      ListenerHealthRecovering,
                        ReasonCode:  "cert_retry_scheduled",
                        RecoveryETA: cert.RetryAt,
                    }
                }
            case "pending":
                cs = CertHealthStatus{
                    Status:     ListenerHealthRecovering,
                    ReasonCode: "cert_pending",
                }
            case "ok":
                if cert.ExpiresAt != nil && time.Until(*cert.ExpiresAt) < 7*24*time.Hour {
                    cs = CertHealthStatus{
                        Status:     ListenerHealthDegraded,
                        ReasonCode: "cert_expiring_soon",
                    }
                } else {
                    cs = CertHealthStatus{
                        Status:     ListenerHealthOK,
                        ReasonCode: "ok",
                    }
                }
            }
        }
        certStatuses[certID] = cs

        // Update worst-of aggregation with tie-breaker for actionRequired
        // When status is equal, prefer config errors (actionRequired) over rate limits
        isConfigError := cert != nil && cert.FailureClass == FailureClassConfigError
        shouldUpdate := statusWorseThan(cs.Status, worstStatus) ||
            (cs.Status == worstStatus && isConfigError && !actionRequired)

        if shouldUpdate {
            worstStatus = cs.Status
            worstReasonCode = cs.ReasonCode
            worstReason = reasonForCode(cs.ReasonCode)
            worstRecoveryETA = cs.RecoveryETA
            if cert != nil && cert.FailureReason != "" {
                worstDetails = strPtr(cert.FailureReason)
            }
            // Config errors require user action
            if isConfigError {
                recoverable = false
                actionRequired = true
            }
        }
    }

    // 2. Check backend connectivity
    if !backendOK {
        backendStatus := ListenerHealthError
        if statusWorseThan(backendStatus, worstStatus) {
            worstStatus = backendStatus
            worstReasonCode = "backend_unreachable"
            worstReason = "Backend not responding"
            worstDetails = nil
            worstRecoveryETA = nil
            // Backend issues are auto-recoverable
            recoverable = true
            actionRequired = false
        }
    }

    // 3. Build final health (nil CertStatuses if no certs needed)
    var finalCertStatuses map[string]CertHealthStatus
    if len(certStatuses) > 0 {
        finalCertStatuses = certStatuses
    }

    return ListenerHealth{
        Status:         worstStatus,
        ReasonCode:     worstReasonCode,
        Reason:         worstReason,
        Details:        worstDetails,
        RecoveryETA:    worstRecoveryETA,
        Recoverable:    recoverable,
        ActionRequired: actionRequired,
        CertStatuses:   finalCertStatuses,
        LastChecked:    time.Now(),
        LastOK:         lastOK, // From BackendHealthState; shows "was healthy X ago" for backend_unreachable
    }
}

// statusWorseThan returns true if a is worse than b.
// Priority: error > recovering > degraded > ok
func statusWorseThan(a, b ListenerHealthStatus) bool {
    priority := map[ListenerHealthStatus]int{
        ListenerHealthOK:         0,
        ListenerHealthDegraded:   1,
        ListenerHealthRecovering: 2,
        ListenerHealthError:      3,
    }
    return priority[a] > priority[b]
}

// reasonForCode returns human-readable reason for any health code.
// Covers all canonical codes: error states (cert_*), non-error states, backend codes, and app states.
func reasonForCode(code string) string {
    reasons := map[string]string{
        // Config error states (require user action)
        "cert_dns_error":               "Domain DNS not configured",
        "cert_domain_unreachable":      "Domain doesn't resolve to this device",
        "cert_caa_forbidden":           "CAA record blocks certificate issuance",
        "cert_rejected_identifier":     "Domain is blocked by certificate authority",
        "cert_invalid_contact":         "Invalid account contact email",
        "cert_account_error":           "Certificate account needs re-registration",
        "cert_unauthorized_persistent": "Challenge verification persistently failing",

        // Transient error states (auto-recoverable)
        "cert_connection_failed": "Unable to reach this device from internet",
        "cert_rate_limited":      "Rate limited by Let's Encrypt",
        "cert_unauthorized":      "Challenge verification failed",
        "cert_acme_error":        "Certificate authority error",
        "cert_unknown_error":     "Unknown error",

        // Recovering/pending states
        "cert_pending":         "Certificate issuance in progress",
        "cert_retry_scheduled": "Certificate issuance failed, retrying",

        // Degraded states
        "cert_expiring_soon": "Certificate expiring soon",

        // OK state
        "ok": "Operational",

        // Backend states
        "backend_unreachable": "Backend not responding",

        // App states (for stopped/starting/error apps)
        "app_starting": "App is starting up",
        "app_error":    "App failed to start",
    }
    if r, ok := reasons[code]; ok {
        return r
    }
    return "Unknown status"
}
```

**Precedence when both cert and backend are unhealthy:**

The derivation checks certificate status before backend connectivity. This is intentional:

1. **Remote access flow:** Certificate errors are the primary blocker for remote access—if the cert is invalid, TLS handshake fails before traffic reaches the backend. Surfacing cert issues first matches the user's troubleshooting order.

2. **Backend issues are transient:** Backend unreachability typically auto-resolves when the container restarts, whereas cert issues often require user intervention (DNS config, CAA records).

3. **UI aggregation:** The `ListenerHealth` struct represents the *most actionable* issue. If users need independent signals, the implementation can expose `backend_ok: bool` as a separate field on the endpoint response for debugging, but the primary health status remains single-valued for badge rendering.

**Alternative considered (rejected):** Split into `remote_health` + `backend_health` and let UI aggregate. Rejected because it pushes complexity to clients and most users only care about "can I access this app?"—a single status answers that directly.

### 4.5 Health Storage and Events

Health status changes emit events via the existing event bus for UI reactivity:

```go
// New event topics (added to internal/events/bus.go)
TopicCertificateChanged    Topic = "certificate_changed"     // Emitted by remote manager
TopicListenerHealthChanged Topic = "listener_health_changed" // Emitted by health aggregator

// Certificate change payload (from remote manager)
type CertificateChangedEvent struct {
    CertID       string       `json:"cert_id"`
    Status       string       `json:"status"`        // "ok", "pending", "error"
    FailureClass FailureClass `json:"failure_class,omitempty"`
    FailureCode  string       `json:"failure_code,omitempty"`
    Timestamp    time.Time    `json:"timestamp"`
}

// Listener health payload (computed by health aggregator, consumed by UI)
type ListenerHealthEvent struct {
    App       string         `json:"app"`
    Listener  string         `json:"listener"`
    Health    ListenerHealth `json:"health"`
    Timestamp time.Time      `json:"timestamp"`
}
```

**Why `TopicCertificateChanged` when `TopicRemoteConfigChanged` exists?**

`TopicRemoteConfigChanged` (internal/events/bus.go:20) is already emitted on every remote manager `save()` call (`manager.go:801`), which includes cert status updates. **Technically, subscribing to `TopicRemoteConfigChanged` and inspecting the `Certificates[]` field would work for correctness.**

However, `TopicCertificateChanged` is an **optimization and semantics improvement**:

1. **Efficiency:** `TopicRemoteConfigChanged` broadcasts the entire `remote.Status` on every save. A single cert retry would trigger full config reprocessing by all subscribers. `TopicCertificateChanged` is per-cert, so the health aggregator only recomputes affected listeners.

2. **Semantics:** `TopicRemoteConfigChanged` conflates config changes (enable/disable, solver change) with operational events (cert retry succeeded). Separate topics make subscriber intent clearer.

3. **Payload:** `CertificateChangedEvent` carries typed failure classification (`FailureClass`, `FailureCode`) needed for health computation. Extracting this from the `Certificate` struct in `remote.Status` would require the subscriber to diff previous/current state.

**Alternative (simpler but less efficient):** Skip `TopicCertificateChanged` and subscribe to `TopicRemoteConfigChanged`, diff the `Certificates[]` array to detect changes. This works but is less elegant and less efficient for high-frequency cert retries.

**Event flow:** Remote manager emits `TopicCertificateChanged` → Health aggregator (services layer) subscribes, looks up affected listeners, recomputes health, emits `TopicListenerHealthChanged` → UI/WebSocket subscribers receive listener-level events. See §5.3.1 for architecture diagram.

**Backend health debounce:**

Backend health checks already exist at `internal/services/manager.go:613` (`checkBackends()`). Currently it runs every 15 seconds (ticker at line 592) and logs warnings for unreachable backends. We extend this existing function with debounce tracking and event emission rather than recreating it:

Without debouncing, transient network hiccups would cause event spam and UI badge flicker. Add debounce logic to `checkBackends()`:

**Caveat: TCP reachable ≠ app healthy.** Current health checks use TCP dial—this confirms the port is open and a process is listening, but doesn't guarantee the app is functioning correctly (e.g., app may be stuck in startup, returning 500s, or deadlocked). A future enhancement could add optional HTTP health endpoint probing (`/health`, `/healthz`), but TCP is sufficient for the initial implementation—it catches the common failure mode (container not running / port not bound).

```go
// BackendHealthState tracks debounced health per endpoint
type BackendHealthState struct {
    mu              sync.RWMutex
    lastOK          map[string]time.Time      // endpoint key → last successful check time
    failureCount    map[string]int            // endpoint key → consecutive failure count
    lastEmittedOK   map[string]bool           // endpoint key → last emitted health state
}

const (
    backendFailureThreshold = 3  // Consecutive failures before reporting unhealthy
    backendRecoveryThreshold = 1 // Consecutive successes before reporting healthy
)

func (s *BackendHealthState) recordCheck(key string, ok bool) (shouldEmit bool, isHealthy bool) {
    s.mu.Lock()
    defer s.mu.Unlock()

    prevEmittedOK, exists := s.lastEmittedOK[key]

    if ok {
        s.lastOK[key] = time.Now()
        s.failureCount[key] = 0

        if !exists {
            // First check for this endpoint - initialize as healthy, don't emit
            s.lastEmittedOK[key] = true
            return false, true
        }
        if !prevEmittedOK {
            // Was unhealthy, now healthy - emit recovery event
            s.lastEmittedOK[key] = true
            return true, true
        }
        // Already healthy - don't emit
        return false, true
    }

    // Failed check
    s.failureCount[key]++

    if !exists {
        // First check failed - initialize tracking but don't emit yet (need threshold)
        s.lastEmittedOK[key] = true // Assume was healthy
        return false, true          // Report as healthy until threshold crossed
    }
    if s.failureCount[key] >= backendFailureThreshold && prevEmittedOK {
        // Crossed threshold from healthy - emit unhealthy event
        s.lastEmittedOK[key] = false
        return true, false
    }
    // Below threshold or already emitted unhealthy - don't emit
    return false, prevEmittedOK // Return current emitted state (false = unhealthy)
}
```

**Debounce behavior:**
- **Failure threshold:** 3 consecutive failures (45 seconds) before marking unhealthy
- **Recovery threshold:** 1 success immediately recovers (fail-fast recovery)
- **Event-on-change only:** Events emitted only when transitioning between healthy/unhealthy
- **Last OK tracking:** `lastOK` field enables "was healthy X minutes ago" in UI

This prevents:
- Badge flicker from transient TCP hiccups
- Event spam flooding WebSocket connections
- Log noise from momentary backend restarts

**Integration with existing WebSocket infrastructure:** See §7.4 for how this integrates with the existing progress stream endpoint.

## 5. Self-Healing ACME Retries

### 5.1 Indefinite Retry Model

Remove the hard `maxCertAttempts = 10` cap (`internal/remote/manager.go:1013`). Use class-specific backoff instead:

**Reuse existing `certBackoff()`:** The existing function at `internal/remote/manager.go:1285` already implements exponential backoff (1min → 1hr with jitter). Extend it to support failure class:

```go
// Evolve existing certBackoff() to support failure class
// internal/remote/manager.go - replace existing certBackoff()

func certBackoff(attempt int, class FailureClass) time.Duration {
    switch class {
    case FailureClassRateLimited:
        return rateLimitBackoff(attempt)
    case FailureClassConfigError:
        return configErrorProbeInterval // 168 hours (weekly)
    default: // FailureClassTransient
        return transientBackoff(attempt)
    }
}

// transientBackoff: reuse existing logic, extend for long-term retries
func transientBackoff(attempt int) time.Duration {
    // Existing logic: 1min → 1hr exponential (attempts 1-10)
    if attempt <= 10 {
        if attempt <= 1 {
            return time.Minute
        }
        shift := attempt - 1
        if shift > 6 {
            shift = 6
        }
        delay := time.Duration(1<<shift) * time.Minute
        if delay > time.Hour {
            delay = time.Hour
        }
        jitter := time.Duration(rand.Int63n(int64(delay / 5)))
        return delay + jitter
    }
    // NEW: Long-term retries for persistent transient failures
    base := 24 * time.Hour
    jitter := time.Duration(rand.Int63n(int64(4 * time.Hour)))
    return base + jitter
}

// rateLimitBackoff: Conservative backoff for LE rate limits
func rateLimitBackoff(attempt int) time.Duration {
    switch {
    case attempt <= 1:
        return 12*time.Hour + time.Duration(rand.Int63n(int64(12*time.Hour)))
    case attempt <= 3:
        return 24*time.Hour + time.Duration(rand.Int63n(int64(24*time.Hour)))
    default:
        return 72*time.Hour + time.Duration(rand.Int63n(int64(96*time.Hour)))
    }
}
```

**Jitter implementation note:** The jitter uses `math/rand` which should be seeded per-device/process to avoid synchronized retries across a fleet (e.g., all devices retrying at the same second after an outage). In production, seed with a combination of device ID + current time at startup. For tests, inject a fixed seed or mock `rand` source for deterministic behavior:

```go
// Production: seed at startup
func init() {
    rand.Seed(time.Now().UnixNano() ^ int64(deviceIDHash()))
}

// Tests: use injected rand source or fixed seed for determinism
type BackoffConfig struct {
    RandSource rand.Source // nil = use global rand
}
```

```go
// parseRetryAfter extracts Retry-After from lego's RateLimitedError.
// lego v4.31.0+ exposes this via legoacme.RateLimitedError.RetryAfter field.
// Returns nil if not a rate limit error or if Retry-After is unparseable.
// Note: Uses legoacme alias (see §5.2 import note)
func parseRetryAfter(err error) *time.Time {
    var rle *legoacme.RateLimitedError
    if !errors.As(err, &rle) || rle.RetryAfter == "" {
        return nil
    }

    // Retry-After can be seconds (integer) or HTTP-date
    if secs, parseErr := strconv.Atoi(rle.RetryAfter); parseErr == nil {
        t := time.Now().Add(time.Duration(secs) * time.Second)
        return &t
    }
    if t, parseErr := http.ParseTime(rle.RetryAfter); parseErr == nil {
        return &t
    }
    return nil
}
```

**Rate limit considerations:**
- Let's Encrypt "Duplicate Certificate" limit: 5 per week → need multi-day backoff
- "Failed Validation" limit: 5 per account/hostname/hour → 12hr minimum backoff
- "New Orders" limit: 300 per 3 hours → less likely to hit, but still need caution
- Server-provided `Retry-After`: Prefer when available via `legoacme.RateLimitedError.RetryAfter` (lego v4.31.0+), fall back to conservative backoffs above

### 5.2 Rate Limit Detection

Parse ACME errors to detect rate limits. The codebase uses **lego v4.31.0** (`github.com/go-acme/lego/v4`), which provides typed error types including `legoacme.RateLimitedError`:

```go
// In internal/remote/acme/issuer.go
// Note: Use alias "legoacme" to avoid collision with package acme (this file's package)

import (
    legoacme "github.com/go-acme/lego/v4/acme"
)

func isRateLimitError(err error) bool {
    if err == nil {
        return false
    }

    // 1. Check lego's RateLimitedError (preferred, includes RetryAfter)
    var rle *legoacme.RateLimitedError
    if errors.As(err, &rle) {
        return true
    }

    // 2. Fallback: Check ProblemDetails for rate limit type
    var problemDetails *legoacme.ProblemDetails
    if errors.As(err, &problemDetails) {
        // Check ACME problem type
        if problemDetails.Type == "urn:ietf:params:acme:error:rateLimited" {
            return true
        }
        // Also check HTTP status for rate limits (429 Too Many Requests)
        if problemDetails.HTTPStatus == 429 {
            return true
        }
    }

    // 2. Fallback: string matching for edge cases or wrapped errors
    // This catches rate limits that may not be properly typed (e.g., from HTTP layer)
    // Note: Only use after typed checks fail, as err.Error() loses type information
    errStr := strings.ToLower(err.Error())
    rateLimitPatterns := []string{
        "rate limit",
        "too many requests",
        "too many certificates",
        "too many failed authorizations",
        "ratelimited",
    }
    for _, pattern := range rateLimitPatterns {
        if strings.Contains(errStr, pattern) {
            return true
        }
    }

    return false
}
```

**Note:** lego v4.31.0 exposes `*legoacme.ProblemDetails` with `Type` and `HTTPStatus` fields. There is no separate `acme.HTTPError` type. Always check typed errors via `errors.As()` before falling back to string matching, as `err.Error()` loses type information.

### 5.3 Failure Classification

To prevent infinite churning on user errors (which conflicts with the non-goal "misconfigured domains"), classify failures into distinct categories with different retry behaviors:

```go
type FailureClass string

const (
    // FailureClassTransient: Temporary issue, retry with normal backoff
    // Examples: network timeout, DNS propagation delay, ACME server hiccup
    FailureClassTransient FailureClass = "transient"

    // FailureClassRateLimited: Let's Encrypt rate limit hit, long backoff required
    // Examples: too many certificates, too many failed authorizations
    FailureClassRateLimited FailureClass = "rate_limited"

    // FailureClassConfigError: User configuration issue, requires user action
    // Examples: invalid domain, DNS not pointing to Piccolo, domain not owned
    // Auto-retries weekly (see §5.3.1 config-error recovery) to catch external fixes
    FailureClassConfigError FailureClass = "config_error"
)

// classifyFailure determines the failure class from ACME error
// classifyFailure returns (FailureClass, FailureCode) where FailureCode uses
// canonical namespace "cert_*" for certificate-related errors. These codes are
// used consistently across:
//   - Certificate.FailureCode
//   - ListenerHealth.ReasonCode
//   - UI policy keys / i18n keys
func classifyFailure(err error) (FailureClass, string) {
    var pd *legoacme.ProblemDetails
    if !errors.As(err, &pd) {
        return FailureClassTransient, "cert_unknown_error"
    }

    switch pd.Type {
    case "urn:ietf:params:acme:error:rateLimited":
        return FailureClassRateLimited, "cert_rate_limited"

    case "urn:ietf:params:acme:error:dns":
        // DNS resolution failed - likely user config error
        return FailureClassConfigError, "cert_dns_error"

    case "urn:ietf:params:acme:error:unauthorized":
        // Domain validation failed - could be transient or config
        // Certain detail strings indicate definite config errors
        if strings.Contains(pd.Detail, "No valid IP addresses") ||
           strings.Contains(pd.Detail, "NXDOMAIN") ||
           strings.Contains(pd.Detail, "Incorrect TXT record") {
            return FailureClassConfigError, "cert_domain_unreachable"
        }
        // Otherwise starts as transient but may escalate (see §5.3.3)
        return FailureClassTransient, "cert_unauthorized"

    case "urn:ietf:params:acme:error:connection":
        // Connection failed - see §5.3.2 for hybrid handling
        return FailureClassTransient, "cert_connection_failed"

    case "urn:ietf:params:acme:error:caa":
        // CAA record blocks issuance - user must fix DNS
        return FailureClassConfigError, "cert_caa_forbidden"

    case "urn:ietf:params:acme:error:rejectedIdentifier":
        // Domain is on a blocklist or policy prohibits issuance
        return FailureClassConfigError, "cert_rejected_identifier"

    case "urn:ietf:params:acme:error:invalidContact":
        // Invalid contact email - user must fix account
        return FailureClassConfigError, "cert_invalid_contact"

    case "urn:ietf:params:acme:error:accountDoesNotExist":
        // Account was deactivated/deleted - needs re-registration
        return FailureClassConfigError, "cert_account_error"

    default:
        // Unknown error types start as transient
        return FailureClassTransient, "cert_acme_error"
    }
}
```

### 5.3.3 Escalation Rules for Persistent Transient Errors

Similar to connection errors (§5.3.2), other transient errors may indicate config problems if they persist. Apply escalation rules:

```go
const (
    // Escalation thresholds (similar to connection errors)
    unauthorizedEscalateAfterAttempts = 5
    unauthorizedEscalateAfterDuration = 24 * time.Hour
)

// handleUnauthorizedFailure applies hybrid escalation for persistent unauthorized errors.
// Called from updateCertFailure when code == "cert_unauthorized"
func (m *Manager) handleUnauthorizedFailure(cert *Certificate) (FailureClass, string) {
    cert.TransientAttempts++

    // Track when unauthorized errors started
    if cert.FirstUnauthorizedAt == nil {
        now := time.Now()
        cert.FirstUnauthorizedAt = &now
    }

    // Escalate to config error if persistent
    firstFailure := *cert.FirstUnauthorizedAt
    attempts := cert.TransientAttempts

    if attempts >= unauthorizedEscalateAfterAttempts ||
       time.Since(firstFailure) > unauthorizedEscalateAfterDuration {
        return FailureClassConfigError, "cert_unauthorized_persistent"
    }

    return FailureClassTransient, "cert_unauthorized"
}
```

**Escalation rationale:** If `unauthorized` errors persist for 5 attempts OR 24 hours, they're likely config-related (wrong domain, firewall blocking challenge verification, etc.) rather than transient network issues. Escalating to `config_error` triggers:
- Weekly probe instead of aggressive retry
- `action_required=true` in UI
- User guidance to check domain config

### 5.3.4 Certificate Status Updates

Update the certificate status with failure classification and per-class attempt tracking:

```go
type Certificate struct {
    // ... existing fields ...

    FailureClass       FailureClass `json:"failure_class"`        // Classification of failure
    FailureCode        string       `json:"failure_code"`         // Machine-readable code (canonical "cert_*" namespace)

    // Per-class attempt tracking to avoid cross-class backoff jumps
    // e.g., 10 transient failures followed by 1 rate-limit shouldn't start at 3-7 day backoff
    TransientAttempts  int          `json:"transient_attempts"`   // Attempts with transient failures
    RateLimitAttempts  int          `json:"rate_limit_attempts"`  // Attempts with rate-limit failures
    ConnectionAttempts int          `json:"connection_attempts"`  // Attempts with connection failures (for hybrid handling)

    // Failure timing for hybrid escalation (see §5.3.2, §5.3.3)
    FirstConnectionFailureAt  *time.Time `json:"first_connection_failure_at,omitempty"`  // When connection failures started (for 24h escalation)
    FirstUnauthorizedAt       *time.Time `json:"first_unauthorized_at,omitempty"`        // When unauthorized failures started (for 24h escalation)
}

func (m *Manager) updateCertFailure(id string, reason string, err error) {
    class, code := classifyFailure(err)

    // Handle connection errors with hybrid approach (see §5.3.2)
    if code == "cert_connection_failed" {
        class, code = m.handleConnectionFailure(cfg.Certificates[i])
    }

    cfg.Certificates[i].Status = "error"
    cfg.Certificates[i].FailureReason = reason
    cfg.Certificates[i].FailureClass = class
    cfg.Certificates[i].FailureCode = code

    // Increment class-specific attempt counter
    switch class {
    case FailureClassTransient:
        cfg.Certificates[i].TransientAttempts++
    case FailureClassRateLimited:
        cfg.Certificates[i].RateLimitAttempts++
    }

    // Compute retry time based on failure class using class-specific attempts
    now := time.Now()
    switch class {
    case FailureClassRateLimited:
        // Prefer server-provided Retry-After if available (lego v4.31.0+)
        if retryAt := parseRetryAfter(err); retryAt != nil {
            cfg.Certificates[i].RetryAt = retryAt
        } else {
            // Fall back to conservative backoff
            attempts := cfg.Certificates[i].RateLimitAttempts
            retry := now.Add(rateLimitBackoff(attempts))
            cfg.Certificates[i].RetryAt = &retry
        }

    case FailureClassConfigError:
        // Schedule weekly probe (not fully paused - see config-error recovery above)
        probe := now.Add(configErrorProbeInterval)
        cfg.Certificates[i].RetryAt = &probe

    case FailureClassTransient:
        attempts := cfg.Certificates[i].TransientAttempts
        retry := now.Add(transientBackoff(attempts))
        cfg.Certificates[i].RetryAt = &retry
    }

    // Append event with retention policy (see §5.3.3)
    m.appendEventWithRetention(cfg, Event{CertID: id, ...})

    // Signal scheduler to re-evaluate
    m.notifySchedulerWake()

    // Emit certificate status change event (NOT listener health directly)
    // The remote manager doesn't know which listeners are impacted (especially
    // for wildcard certs). A health-aggregator in the server/services layer
    // subscribes to this and computes + emits listener_health_changed.
    m.events.Publish(events.TopicCertificateChanged, CertificateChangedEvent{
        CertID:       id,
        Status:       "error",
        FailureClass: class,
        FailureCode:  code,
    })
}
```

**Event architecture (two-stage):**

```
┌─────────────────────┐     TopicCertificateChanged     ┌──────────────────────┐
│   Remote Manager    │ ──────────────────────────────► │  Health Aggregator   │
│  (cert issuance)    │                                 │  (services layer)    │
└─────────────────────┘                                 └──────────┬───────────┘
                                                                   │
                                                                   │ 1. Look up which listeners
                                                                   │    depend on this cert
                                                                   │ 2. Recompute health for each
                                                                   │ 3. Emit listener events
                                                                   ▼
                                                        TopicListenerHealthChanged
                                                                   │
                                                                   ▼
                                                        ┌─────────────────────┐
                                                        │   WebSocket / UI    │
                                                        └─────────────────────┘
```

**Why two-stage?**
- Remote manager knows cert status but NOT which listeners use it
- Wildcard cert affects ALL HTTP/WS listeners for dns-01 solvers
- Alias certs are mapped to specific listeners
- The services layer already has listener→cert mapping (via `resolveCertificatesForListener`)

**Health aggregator implementation (in services layer):**
```go
// In internal/services/health_aggregator.go

func (s *ServiceManager) startHealthAggregator() {
    ch, _ := s.events.SubscribeWithCancel(events.TopicCertificateChanged, 64)
    go func() {
        for evt := range ch {
            certEvt := evt.Payload.(CertificateChangedEvent)
            s.recomputeAffectedListenerHealth(certEvt.CertID)
        }
    }()
}

func (s *ServiceManager) recomputeAffectedListenerHealth(certID string) {
    // Find all listeners that depend on this cert
    for _, ep := range s.endpoints {
        certIDs, _ := resolveCertificatesForListener(ep, s.remoteStatus, s.aliases)
        for _, cid := range certIDs {
            if cid == certID {
                // Recompute and emit listener health
                health := s.deriveListenerHealth(ep)
                s.events.Publish(events.TopicListenerHealthChanged, ListenerHealthEvent{
                    App:      ep.App,
                    Listener: ep.Name,
                    Health:   health,
                })
                break
            }
        }
    }
}
```

**Config-error recovery:**

Config errors (DNS misconfiguration, CAA records, port forwarding) are often fixed outside Piccolo's control (user updates DNS records, fixes router settings). The system needs multiple recovery paths:

1. **Automatic: Config change detection** - When remote config is updated within Piccolo, call `requeueConfigErrorCerts()` to reset attempts and re-enable retries.

2. **Manual: Retry now button** - UI provides a "Retry" button that calls the existing `POST /api/v1/remote/certificates/{id}/renew` endpoint. This immediately requeues the cert for issuance, regardless of failure class. The `/renew` semantics already cover this: "request immediate issuance" (whether renewal or retry).

3. **Automatic: Weekly probe** - Scheduler retries config_error certs once per week (168 hours) to catch external fixes the user made without telling Piccolo. This is conservative enough to avoid churning on truly broken configs while still enabling self-healing for common cases like "user forgot to click retry after fixing DNS".

```go
const configErrorProbeInterval = 7 * 24 * time.Hour // Weekly

func (m *Manager) computeNextWakeTime() time.Duration {
    // ... existing logic for transient/rate-limited certs ...

    // Also schedule weekly probes for config_error certs
    for _, c := range cfg.Certificates {
        if c.FailureClass == FailureClassConfigError {
            // If last attempt was > 1 week ago, probe soon
            if c.LastAttempt != nil && time.Since(*c.LastAttempt) >= configErrorProbeInterval {
                return 0 // Due now
            }
            // Otherwise, schedule probe for 1 week after last attempt
            if c.LastAttempt != nil {
                probeAt := c.LastAttempt.Add(configErrorProbeInterval)
                if probeAt.Before(earliest) {
                    earliest = probeAt
                }
            }
        }
    }
    // ... rest of logic ...
}
```

**API for manual retry:** Reuse the existing `POST /api/v1/remote/certificates/{id}/renew` endpoint. The existing semantics ("request immediate issuance") apply to both renewal of expiring certs and retry of failed certs. No new endpoint needed.

### 5.3.2 Hybrid Connection Error Handling

Connection errors (`cert_connection_failed`) are ambiguous: they could be transient (ISP outage, temporary firewall) or persistent (port not forwarded, firewall rule). Use a hybrid approach:

```go
const (
    connectionEscalateAfterAttempts = 5
    connectionEscalateAfterDuration = 24 * time.Hour
)

func (m *Manager) handleConnectionFailure(cert *Certificate) (FailureClass, string) {
    cert.ConnectionAttempts++

    // Check if we should escalate to config error
    firstConnectionFailure := cert.FirstConnectionFailureAt
    if firstConnectionFailure == nil {
        now := time.Now()
        cert.FirstConnectionFailureAt = &now
        firstConnectionFailure = &now
    }

    persistent := cert.ConnectionAttempts >= connectionEscalateAfterAttempts ||
                  time.Since(*firstConnectionFailure) >= connectionEscalateAfterDuration

    if persistent {
        // Escalate: treat as config error requiring user action
        return FailureClassConfigError, "cert_connection_failed"
    }

    // Still treating as transient
    return FailureClassTransient, "cert_connection_failed"
}

// Reset connection tracking when cert succeeds or config changes
func (m *Manager) resetConnectionTracking(cert *Certificate) {
    cert.ConnectionAttempts = 0
    cert.FirstConnectionFailureAt = nil
}
```

**Behavior:**
- First 5 attempts OR first 24 hours: treat as transient, keep retrying
- After threshold: escalate to `config_error`, pause retries, show "action required" in UI
- User fixing port forwarding + triggering config reload resets tracking

### 5.3.3 Event Retention Policy

Indefinite retries would cause unbounded growth of `cfg.Events` (currently every failure appends an event). To prevent config file bloat:

```go
const (
    maxEventsTotal     = 100  // Max events in config overall
    maxEventsPerCert   = 10   // Max events per certificate ID
    dedupeWindowMinutes = 60  // Dedupe identical consecutive errors within this window
)

// Event extends the existing remote.Event struct (internal/remote/manager.go:86)
// by adding CertID for robust retention/deduplication.
//
// Existing schema (preserved for backward compatibility):
//   Timestamp time.Time `json:"ts"`
//   Level     string    `json:"level"`
//   Source    string    `json:"source"`
//   Message   string    `json:"message"`
//   NextStep  string    `json:"next_step,omitempty"`
//
// New field:
//   CertID    string    `json:"cert_id,omitempty"`  // e.g., "host:immich-piccolo.example.com"
//
// Events without CertID (e.g., general remote config events) are exempt from
// per-certificate trimming but still subject to total event limit.

func (m *Manager) appendEventWithRetention(cfg *Config, evt Event) {
    // 1. Dedupe: Skip if last event for same cert has identical message within window
    for i := len(cfg.Events) - 1; i >= 0; i-- {
        last := cfg.Events[i]
        if last.CertID != "" && last.CertID == evt.CertID && last.Message == evt.Message {
            if time.Since(last.Timestamp) < dedupeWindowMinutes*time.Minute {
                // Update timestamp only, don't append duplicate
                cfg.Events[i].Timestamp = evt.Timestamp
                return
            }
            break
        }
    }

    // 2. Append new event
    cfg.Events = append(cfg.Events, evt)

    // 3. Trim by certificate: keep only last N events per cert ID
    cfg.Events = trimEventsByCert(cfg.Events, maxEventsPerCert)

    // 4. Trim total: keep only last N events overall
    if len(cfg.Events) > maxEventsTotal {
        cfg.Events = cfg.Events[len(cfg.Events)-maxEventsTotal:]
    }
}

func trimEventsByCert(events []Event, maxPerCert int) []Event {
    // Group events by cert ID, keep last maxPerCert per group.
    // Events without CertID (general events) are always kept (not subject to per-cert trimming).
    // Preserves overall chronological order.
    certCounts := make(map[string]int)
    var result []Event

    // Iterate in reverse to keep most recent
    for i := len(events) - 1; i >= 0; i-- {
        evt := events[i]
        // Events without CertID pass through; cert events are counted
        if evt.CertID == "" || certCounts[evt.CertID] < maxPerCert {
            result = append([]Event{evt}, result...)
            if evt.CertID != "" {
                certCounts[evt.CertID]++
            }
        }
    }
    return result
}
```

**Retention behavior:**
- Consecutive identical failures (same CertID + Message) within 60 minutes → single event with updated timestamp
- Max 10 events per certificate → older cert-specific events dropped
- Events without CertID (general remote config events) → exempt from per-cert trimming
- Max 100 events total → FIFO eviction (affects all events)
- Successful issuance events are never deduped (always logged)

**Schema migration:** Adding `cert_id` field to existing Event struct is additive; existing persisted events without the field will have `CertID == ""` and be exempt from per-cert trimming.

### 5.4 RetryAt-Driven Scheduler

The current scheduler runs on a fixed 1-hour interval, which makes minute-level backoff ineffective (a 1-minute retry becomes "next hour"). To enable responsive retries, change to a **RetryAt-driven scheduler with wake signal**:

```go
type Manager struct {
    // ... existing fields ...

    // Wake signal: triggered when RetryAt is set/changed to interrupt sleeping timer
    scheduleWakeCh chan struct{}
}

func NewManager(...) *Manager {
    return &Manager{
        // ...
        scheduleWakeCh: make(chan struct{}, 1), // Buffered to avoid blocking
    }
}

// NotifySchedulerWake signals the scheduler to re-evaluate wake time.
// Called whenever RetryAt is set or changed (e.g., after updateCertFailure).
func (m *Manager) notifySchedulerWake() {
    select {
    case m.scheduleWakeCh <- struct{}{}:
    default:
        // Already pending wake, no need to queue another
    }
}

func (m *Manager) runRenewScheduler() {
    // Initial scan on startup
    m.scanAndQueueRenewals()

    for {
        // Compute next wake time based on earliest RetryAt or renewal
        nextWake := m.computeNextWakeTime()

        // Cap at 1 hour to ensure periodic health checks even if no retries due
        if nextWake > time.Hour {
            nextWake = time.Hour
        }
        // Minimum 10 seconds to prevent busy-loop on clock skew
        if nextWake < 10*time.Second {
            nextWake = 10 * time.Second
        }

        timer := time.NewTimer(nextWake)
        select {
        case <-m.stopCh:
            // Clean shutdown: stop timer and drain if needed
            if !timer.Stop() {
                <-timer.C // Drain the channel if timer already fired
            }
            return
        case <-m.scheduleWakeCh:
            // RetryAt changed - re-evaluate immediately
            // IMPORTANT: Must drain timer channel after Stop() to prevent spurious wakeup
            // on next iteration. timer.Stop() returns false if timer already expired.
            if !timer.Stop() {
                select {
                case <-timer.C: // Drain if fired
                default: // Already consumed or not fired
                }
            }
            // Don't scan yet, just recalculate wake time
            continue
        case <-timer.C:
            m.scanAndQueueRenewals()
        }
    }
}

// Note on timer.Stop() pattern:
// After timer.Stop(), if the timer had already fired (Stop returns false),
// the channel may have a pending value that must be drained. Without draining,
// the next iteration's select could receive the stale timer event instead of
// waiting for the new timer. The select-with-default pattern handles the race
// where another goroutine might have already consumed the value.

func (m *Manager) computeNextWakeTime() time.Duration {
    m.mu.RLock()
    cfg := m.config
    m.mu.RUnlock()

    now := time.Now()
    earliest := now.Add(time.Hour) // Default: 1 hour

    for _, c := range cfg.Certificates {
        // Check RetryAt for failed certs (including config_error weekly probes)
        // Note: config_error certs are NOT skipped - they have RetryAt set for weekly probes
        if c.RetryAt != nil && c.RetryAt.Before(earliest) {
            earliest = *c.RetryAt
        }
        // Check NextRenewal for valid certs
        if c.NextRenewal != nil && c.NextRenewal.Before(earliest) {
            earliest = *c.NextRenewal
        }
    }

    if earliest.Before(now) {
        return 0 // Due now
    }
    return earliest.Sub(now)
}

func (m *Manager) updateCertFailure(id string, reason string, class FailureClass) {
    // ... update cert status ...

    // Signal scheduler to re-evaluate wake time
    m.notifySchedulerWake()
}
```

**Key design points:**
- `scheduleWakeCh` is buffered (size 1) to avoid blocking callers
- Wake signal only triggers re-evaluation, not immediate scan (prevents thundering herd)
- Config-error certs have `RetryAt` set for weekly probes, so they participate in scheduling (not skipped)

## 6. UI Health Policies

### 6.1 Policy Framework

Define standard behaviors for each health status:

```dart
enum ListenerHealthAction {
  none,              // No action needed
  showWarningBadge,  // Show warning indicator on app card
  showInfoBanner,    // Show info banner in app detail
  showRecoveryOverlay,  // Show overlay with recovery info
  showErrorOverlay,  // Show overlay with error and local fallback
}

class ListenerHealthPolicy {
  final ListenerHealthAction cardAction;
  final ListenerHealthAction detailAction;
  final bool offerLocalFallback;
  final String? fallbackMessage;
}

Map<String, ListenerHealthPolicy> healthPolicies = {
  'ok': ListenerHealthPolicy(
    cardAction: ListenerHealthAction.none,
    detailAction: ListenerHealthAction.none,
    offerLocalFallback: false,
  ),

  'degraded': ListenerHealthPolicy(
    cardAction: ListenerHealthAction.showWarningBadge,
    detailAction: ListenerHealthAction.showInfoBanner,
    offerLocalFallback: false,
  ),

  'recovering': ListenerHealthPolicy(
    cardAction: ListenerHealthAction.showWarningBadge,
    detailAction: ListenerHealthAction.showRecoveryOverlay,
    offerLocalFallback: true,
    fallbackMessage: "Remote access is being set up. You can access this app locally while we work on it.",
  ),

  'error': ListenerHealthPolicy(
    cardAction: ListenerHealthAction.showWarningBadge,
    detailAction: ListenerHealthAction.showErrorOverlay,
    offerLocalFallback: true,
    fallbackMessage: "Remote access is temporarily unavailable.",
  ),
};
```

### 6.2 Local Fallback Behavior

When remote access fails, offer local fallback based on access context. The key insight is that the "Access Locally" button only works when the user is on the same network—so we should only show it as a clickable action when we know they're local.

| Portal Access | App Access Attempt | Listener Status | Behavior |
|---------------|-------------------|-----------------|----------|
| Local (`piccolo.local`) | App via dashboard | Any remote error | Auto-redirect to local app URL |
| Local (`piccolo.local`) | App via dashboard | `ok` | Normal flow (local or remote per preference) |
| Remote (`portal.example.com`) | Remote app URL | `recovering` | Show overlay with status + copyable local URL |
| Remote (`portal.example.com`) | Remote app URL | `error` | Show overlay with error + copyable local URL |

**Key behavior notes:**
- When user is on LAN (accessed portal via `piccolo.local`), app links default to local URLs, avoiding remote access issues entirely
- When user is remote, the overlay shows the local URL as **copyable text** (not a clickable button) with clear messaging that it requires LAN access
- The overlay only appears when user explicitly tries to access a remote URL that's unhealthy
- Example: `https://immich.portal.example.com` → `http://immich-piccolo.local`

**LAN fallback URL field:**

Current state:
- `local_url` / `lan_port_url` from `formatServiceEndpoint()` is **intentionally nil on secure/remote requests** (`gin_server.go:1113-1116`) to prevent mixed-content issues
- `lan_host_url` is only present when mDNS is enabled (`gin_server.go:1092-1098`)
- `ServiceEndpoint.LocalURL` field (`services/types.go:33`) exists but isn't populated anywhere

For the overlay to show a copyable LAN fallback URL when accessed remotely, we need a **request-independent** URL field. Add `lan_fallback_url` to the endpoint response:

```go
// In formatServiceEndpoint(), add a request-independent LAN fallback URL:

// lan_fallback_url: always present, computed from device state (not request)
// - If mDNS enabled: http://immich-piccolo.local (host-based)
// - If mDNS disabled: http://<device-ip>:<public-port> (port-based)
// This is distinct from local_url which is nil on secure requests.
if ep.DerivedHostLabel != "" && s.mdnsManager != nil {
    lanBase := s.mdnsManager.Hostname()
    lanHostname := hostnamepkg.DeriveLANHostname(ep.DerivedHostLabel, lanBase)
    result["lan_fallback_url"] = fmt.Sprintf("%s://%s", scheme, lanHostname)
} else {
    // Port-based fallback using device IP (requires passing device IP to this function)
    result["lan_fallback_url"] = fmt.Sprintf("http://%s:%d", deviceIP, ep.PublicPort)
}
```

**⚠️ mDNS disabled experience:** When mDNS is disabled (`PICCOLO_DISABLE_MDNS=1`), `lan_fallback_url` is a port-based URL (e.g., `http://192.168.1.100:8080`). The UI simply uses whatever the backend provides.

```dart
// UI uses the new request-independent field
String get localFallbackUrl => endpoint.lanFallbackUrl; // Always present

// Label indicates mDNS vs port-based (can detect from URL format)
String get localFallbackLabel {
  if (endpoint.lanFallbackUrl.contains('.local')) {
    return "Access via ${endpoint.lanFallbackUrl}";
  }
  return "Access via IP (mDNS disabled)";
}
```

**Portal access detection (reuse existing client-side logic):**

The UI already detects local vs remote access effectively at `ui/lib/features/apps/app_launcher.dart:44-58`:

```dart
static bool _isLocalAccess(String host) {
  return host.endsWith('.local') || _isLoopback(host) || _isIpAddress(host);
}

static bool _isIpAddress(String host) {
  // IPv4: digits and dots - matches ALL IPv4 addresses (not just RFC 1918)
  if (RegExp(r'^\d{1,3}(\.\d{1,3}){3}$').hasMatch(host)) return true;
  // IPv6: contains colon (may be bracketed) - matches ALL IPv6 addresses
  if (host.contains(':')) return true;
  return false;
}
```

This covers:
- mDNS access (`.local` suffix)
- IP-based access (any IPv4/IPv6 including RFC 1918, link-local, ULA)
- Localhost access

**No new `access_context` field needed.** The existing `Uri.base.host` check is sufficient because:
1. The `_isIpAddress()` regex matches ANY IP address, not just specific ranges
2. All LAN access scenarios (IP, mDNS, localhost) are covered
3. Server-side detection using `c.Request.Host` would give the same result (sees the same hostname the client used)

**⚠️ Caveat: Hairpin NAT / Split-DNS.** If a LAN user accesses the remote hostname (e.g., `immich.portal.example.com`) from within the LAN—via hairpin NAT, split-horizon DNS, or manual hosts file—both client and server see "remote" even though the user is physically local. This is intentional: hostname-based detection is authoritative for the *access path*, not physical location. In this scenario the TLS cert must still be valid, so local fallback guidance is less relevant.

**Local fallback URLs (already provided):**

The backend already provides multiple URL options via `formatServiceEndpoint()` at `internal/server/gin_server.go:1064-1108`:
- `lan_host_url`: mDNS hostname URL (e.g., `http://immich-piccolo.local`)
- `lan_port_url` / `local_url`: Port-based URL (e.g., `http://piccolo.local:8096`)
- `remote_url`: Remote hostname URL

The UI can use these directly without needing additional server-side access context detection.

**Client-side local access detection (reuse existing):**
```dart
// In ui/lib/features/apps/app_launcher.dart (already exists)
static bool _isLocalAccess(String host) {
  return host.endsWith('.local') || _isLoopback(host) || _isIpAddress(host);
}

// Usage in overlay/banner widgets:
bool get isLocalAccess => AppLauncher.isLocalAccess(Uri.base.host.toLowerCase());
```

Make `_isLocalAccess` public (rename to `isLocalAccess`) so overlay widgets can reuse it

**⚠️ Limitation: Direct bookmarks bypass the portal overlay.**

The overlay protection only works when navigation originates from within the portal UI (clicking app cards, links). If a user:
- Bookmarks a remote app URL directly (e.g., `https://immich.portal.example.com`)
- Types the URL directly in the browser
- Clicks an external link to the app

...and the certificate is not yet issued or has failed, they will see a **browser TLS error** (e.g., "ERR_SSL_PROTOCOL_ERROR" or "SEC_ERROR_UNKNOWN_ISSUER"). This is fundamentally un-interceptable because TLS handshake fails before any HTTP/UI can load.

**Guidance for users:** If you see a TLS error accessing a remote app, check the portal's Remote Access settings for certificate status and any action required.

**⚠️ Requirement: Health-gate all portal-initiated "Open App" actions.**

To prevent users from being punted into a TLS error tab, all portal-initiated navigation to remote app URLs **must be health-gated**:

1. **App cards "Open" button:** Check `primary_listener_health.status` before navigation
   - If status is `ok`: Navigate directly to remote URL
   - If status is `recovering`/`error`: Show overlay instead of navigating

2. **App detail "Open" button:** Same health check before navigation

3. **Any other "launch app" UI:** Always check health first

```dart
void openApp(AppInstanceWithHealth app) async {
  var health = app.primaryListenerHealth;
  final remoteUrl = app.endpoints.first.remoteHostUrl;

  // If health is null/unknown, fetch it first before deciding
  if (health == null) {
    health = await fetchListenerHealth(app.id, app.primaryListenerName);
  }

  // Health-gate: only navigate if explicitly healthy
  // Unknown/null after fetch attempt => block and show loading overlay
  if (health != null && health.status == 'ok') {
    launchUrl(remoteUrl);
  } else if (health != null) {
    showHealthOverlay(app, health);
  } else {
    // Still null after fetch - show "checking health" state
    showHealthCheckingOverlay(app);
  }
}
```

**Important:** Never treat `health == null` as OK. Unknown health could mean:
- Health hasn't been computed yet (app just started)
- API call failed
- Certificate status is being resolved

Navigating with unknown health can result in TLS errors. Always block and fetch/show overlay when health is null.

**Note:** The hostname scheme uses 2-level mDNS format per RFC 20260122 (`<app>-<lan-base>`, e.g., `immich-piccolo.local`). This is already implemented in `internal/hostname/hostname.go:DeriveLANHostname()`.

```dart
class LocalFallbackOverlay extends StatelessWidget {
  final ListenerHealth health;
  final String appName;
  final String lanFallbackUrl;

  // Client-side detection - reuses existing AppLauncher logic
  bool get isLocalAccess => AppLauncher.isLocalAccess(Uri.base.host.toLowerCase());

  Widget build(BuildContext context) {
    return Container(
      // Semi-transparent overlay
      child: Column(
        children: [
          Icon(_iconForStatus(health.status)),
          Text(health.reason),
          if (health.recoveryEta != null)
            // "Next check" for config errors (user must fix), "Next retry" for auto-recoverable
            Text("${health.actionRequired ? 'Next check' : 'Next retry'}: ${_formatTime(health.recoveryEta)}"),
          SizedBox(height: 16),

          // Different UX based on detected access context
          if (isLocalAccess) ...[
            // User is on LAN - button will work
            ElevatedButton(
              onPressed: () => _openLocalUrl(lanFallbackUrl),
              child: Text("Access Locally"),
            ),
          ] else ...[
            // User is remote - show copyable URL with clear expectations
            Text("Local access available on your home network:",
                 style: TextStyle(fontSize: 12)),
            SizedBox(height: 4),
            _CopyableUrl(url: lanFallbackUrl),
          ],

          SizedBox(height: 8),
          Text(
            "Local access requires being on the same network as your Piccolo device",
            style: TextStyle(fontSize: 12, color: Colors.grey),
          ),
        ],
      ),
    );
  }
}

class _CopyableUrl extends StatelessWidget {
  final String url;

  Widget build(BuildContext context) {
    return InkWell(
      onTap: () {
        Clipboard.setData(ClipboardData(text: url));
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(content: Text("URL copied to clipboard")),
        );
      },
      child: Container(
        padding: EdgeInsets.symmetric(horizontal: 12, vertical: 8),
        decoration: BoxDecoration(
          color: Colors.grey.shade200,
          borderRadius: BorderRadius.circular(4),
        ),
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            Text(url, style: TextStyle(fontFamily: 'monospace')),
            SizedBox(width: 8),
            Icon(Icons.copy, size: 16),
          ],
        ),
      ),
    );
  }
}
```

### 6.3 App Card Health Badge

Add a health indicator to app cards on the dashboard:

```dart
class AppCard extends StatelessWidget {
  final AppSummary app;
  final ListenerHealth? primaryListenerHealth;

  Widget build(BuildContext context) {
    return Card(
      child: Stack(
        children: [
          // Existing card content
          _buildCardContent(),

          // Health badge overlay (top-right corner)
          if (primaryListenerHealth != null &&
              primaryListenerHealth!.status != 'ok')
            Positioned(
              top: 8,
              right: 8,
              child: _HealthBadge(health: primaryListenerHealth!),
            ),
        ],
      ),
    );
  }
}

class _HealthBadge extends StatelessWidget {
  final ListenerHealth health;

  Widget build(BuildContext context) {
    return Tooltip(
      message: health.reason,
      child: Container(
        padding: EdgeInsets.all(4),
        decoration: BoxDecoration(
          color: _colorForStatus(health.status),
          borderRadius: BorderRadius.circular(4),
        ),
        child: Icon(
          _iconForStatus(health.status),
          size: 16,
          color: Colors.white,
        ),
      ),
    );
  }

  Color _colorForStatus(String status) {
    switch (status) {
      case 'degraded': return PiccoloTheme.warning;
      case 'recovering': return PiccoloTheme.warning;
      case 'error': return PiccoloTheme.critical;
      default: return PiccoloTheme.success;
    }
  }

  IconData _iconForStatus(String status) {
    switch (status) {
      case 'degraded': return Icons.warning_amber;
      case 'recovering': return Icons.hourglass_empty;
      case 'error': return Icons.error_outline;
      default: return Icons.check_circle;
    }
  }
}
```

### 6.4 App Detail Health Banner

Show detailed health information in the app detail page. The `details` field is hidden by default and shown via an expandable section:

```dart
class AppDetailHealthBanner extends StatefulWidget {
  final ListenerHealth health;
  final String lanFallbackUrl;

  // Client-side detection - reuses existing AppLauncher logic
  bool get isLocalAccess => AppLauncher.isLocalAccess(Uri.base.host.toLowerCase());

  // Computed from health.certStatuses: the cert ID(s) that need action
  // For single unhealthy cert: that cert's ID
  // For multiple: the "worst" one (matches aggregated reason_code)
  String? get actionableCertId {
    if (health.certStatuses == null || health.certStatuses!.isEmpty) return null;
    // Find cert matching the aggregated reason_code (the "worst" one shown to user)
    for (final entry in health.certStatuses!.entries) {
      if (entry.value.reasonCode == health.reasonCode) {
        return entry.key;
      }
    }
    // Fallback: first unhealthy cert
    for (final entry in health.certStatuses!.entries) {
      if (entry.value.status != 'ok') {
        return entry.key;
      }
    }
    return null;
  }

  @override
  State<AppDetailHealthBanner> createState() => _AppDetailHealthBannerState();
}

class _AppDetailHealthBannerState extends State<AppDetailHealthBanner> {
  bool _showDetails = false;

  @override
  Widget build(BuildContext context) {
    final health = widget.health;
    if (health.status == 'ok') return SizedBox.shrink();

    return Container(
      padding: EdgeInsets.all(16),
      color: _backgroundForStatus(health.status),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            children: [
              Icon(_iconForStatus(health.status)),
              SizedBox(width: 12),
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text(health.reason, style: TextStyle(fontWeight: FontWeight.bold)),
                    if (health.recoveryEta != null)
                      // "Next check" for config errors (user must fix), "Next retry" for auto-recoverable
                      Text("${health.actionRequired ? 'Next check' : 'Next retry'}: ${_formatTime(health.recoveryEta)}"),
                    // Show guidance and CTAs based on actionRequired
                    if (health.actionRequired) ...[
                      SizedBox(height: 4),
                      Text("Action required - check Remote Access settings.",
                           style: TextStyle(fontSize: 12, color: Colors.orange)),
                    ] else
                      Text("No action needed - the system is working on it.",
                           style: TextStyle(fontSize: 12, color: Colors.grey)),
                  ],
                ),
              ),
              // CTAs column
              Column(
                mainAxisSize: MainAxisSize.min,
                children: [
                  // Action Required: CTA to Remote settings + Retry button
                  if (health.actionRequired) ...[
                    TextButton(
                      onPressed: () => _navigateToRemoteSettings(),
                      child: Text("Settings"),
                    ),
                    TextButton(
                      onPressed: () => _retryNow(widget.actionableCertId),
                      child: Text("Retry Now"),
                    ),
                  ],
                  // Access Locally: clickable only when on LAN, otherwise copyable URL
                  if (widget.isLocalAccess)
                    TextButton(
                      onPressed: () => _openLocalUrl(widget.lanFallbackUrl),
                      child: Text("Access Locally"),
                    )
                  else
                    _CopyableUrl(url: widget.lanFallbackUrl, compact: true),
                ],
              ),
            ],
          ),
          // Collapsible technical details
          if (health.details != null && health.details!.isNotEmpty) ...[
            SizedBox(height: 8),
            GestureDetector(
              onTap: () => setState(() => _showDetails = !_showDetails),
              child: Row(
                children: [
                  Icon(_showDetails ? Icons.expand_less : Icons.expand_more, size: 16),
                  Text(_showDetails ? "Hide details" : "Show details",
                       style: TextStyle(fontSize: 12, color: Colors.blue)),
                ],
              ),
            ),
            if (_showDetails)
              Padding(
                padding: EdgeInsets.only(top: 8),
                child: Text(health.details!,
                           style: TextStyle(fontSize: 11, fontFamily: 'monospace')),
              ),
          ],
        ],
      ),
    );
  }

  void _navigateToRemoteSettings() {
    // Navigate to Remote Access settings page
    Navigator.of(context).pushNamed('/settings/remote');
  }

  void _retryNow(String? certId) async {
    if (certId == null) return;
    // Call existing /renew endpoint to trigger immediate retry
    await api.post('/api/v1/remote/certificates/$certId/renew');
    // Show snackbar feedback
    ScaffoldMessenger.of(context).showSnackBar(
      SnackBar(content: Text('Retry queued')),
    );
  }
}
```

**UX rule: action_required=true CTAs.** When `action_required` is true, the overlay/banner **must** include:
1. **"Settings" button:** Direct navigation to Remote Access settings (where user can fix DNS, CAA records, etc.)
2. **"Retry Now" button:** Calls `POST /api/v1/remote/certificates/{id}/renew` to immediately requeue issuance (useful after user fixes the external issue)

This ensures users have clear next steps when manual intervention is needed, rather than just waiting for the weekly probe.

## 7. API Changes

### 7.1 Listener Health Endpoint

New endpoint to get health status for app listeners:

```
GET /api/v1/apps/{appId}/listeners/{listenerName}/health

Response (recovering - cert pending):
{
  "status": "recovering",
  "reason_code": "cert_pending",
  "reason": "Certificate issuance in progress",
  "details": "Waiting for ACME challenge verification",
  "recovery_eta": "2026-01-25T15:30:00Z",
  "recoverable": true,
  "action_required": false,
  "cert_statuses": {
    "host:immich.portal.example.com": {
      "status": "recovering",
      "reason_code": "cert_pending"
    }
  },
  "last_checked": "2026-01-25T14:25:00Z"
}
```

**Example: Config error (action required, weekly probe scheduled):**
```
{
  "status": "error",
  "reason_code": "cert_dns_error",
  "reason": "Domain DNS not configured",
  "details": "DNS lookup for immich.portal.example.com returned NXDOMAIN",
  "recovery_eta": "2026-02-01T14:25:00Z",
  "recoverable": false,
  "action_required": true,
  "cert_statuses": {
    "host:immich.portal.example.com": {
      "status": "error",
      "reason_code": "cert_dns_error",
      "recovery_eta": "2026-02-01T14:25:00Z"
    }
  },
  "last_checked": "2026-01-25T14:25:00Z"
}
```

Note: Config errors have `recovery_eta` set to the weekly probe time (7 days). This is when the system will automatically re-check if the user fixed the issue externally. Users can also click "Retry Now" to trigger immediate re-check.

### 7.2 App Summary with Health

Extend app summary to include primary listener health using an **API response DTO** that wraps the persisted `AppInstance`:

```go
// In internal/server/gin_app_handlers.go

// AppInstanceWithHealth is an API response DTO that adds derived health state
// to the persisted AppInstance. This keeps ephemeral/derived data out of the
// on-disk app state (internal/app/types.go:AppInstance).
type AppInstanceWithHealth struct {
    *app.AppInstance                            // Embed persisted app state
    PrimaryListenerHealth *ListenerHealth `json:"primary_listener_health,omitempty"`
}

func (s *GinServer) handleListApps(c *gin.Context) {
    instances := s.appManager.ListInstances()
    result := make([]*AppInstanceWithHealth, len(instances))
    for i, inst := range instances {
        result[i] = &AppInstanceWithHealth{
            AppInstance:           inst,
            PrimaryListenerHealth: s.deriveListenerHealth(inst),
        }
    }
    c.JSON(http.StatusOK, gin.H{"data": result})
}
```

**API Response:**
```
GET /api/v1/apps

Response:
{
  "data": [
    {
      "id": "immich",
      "name": "Immich",
      "status": "running",
      "primary_listener_health": {
        "status": "recovering",
        "reason_code": "cert_pending",
        "reason": "Certificate issuance in progress",
        "recovery_eta": "2026-01-25T15:30:00Z",
        "recoverable": true,
        "action_required": false,
        "cert_statuses": {
          "host:immich.portal.example.com": {
            "status": "recovering",
            "reason_code": "cert_pending"
          }
        }
      }
    }
  ]
}
```

**Design note:** The DTO pattern keeps the persisted `app.AppInstance` struct clean (no ephemeral/derived fields) while providing health data to the API. Health is computed on-demand from certificate status and backend connectivity.

### 7.3 Certificate Status Extension

Extend certificate response with failure classification:

```
GET /api/v1/remote/certificates

Response:
{
  "certificates": [
    {
      "id": "host:immich-piccolo.example.com",
      "domains": ["immich-piccolo.example.com"],
      "status": "error",
      "failure_class": "rate_limited",
      "failure_code": "cert_rate_limited",
      "failure_reason": "too many failed authorizations recently",
      "retry_at": "2026-01-26T14:00:00Z",
      "attempts": 12
    }
  ]
}
```

**Example: Config error (no auto-retry):**
```
{
  "certificates": [
    {
      "id": "host:immich.example.com",
      "domains": ["immich.example.com"],
      "status": "error",
      "failure_class": "config_error",
      "failure_code": "cert_dns_error",
      "failure_reason": "DNS lookup returned NXDOMAIN",
      "retry_at": "2026-02-01T14:00:00Z",
      "attempts": 3
    }
  ]
}
```

### 7.4 Health Events via WebSocket

Extend the existing progress stream endpoint to support multi-topic subscriptions, including listener health events.

**Endpoint:** `GET /api/v1/events/progress/stream`

**Query Parameters:**

| Parameter | Required | Description |
|-----------|----------|-------------|
| `topics` | No | Comma-separated list of topics. Valid: `task_progress`, `listener_health`. **Default: `task_progress`** (backward compatible) |
| `task_id` | If `task_progress` in topics | Required filter for task progress events |
| `app` | No | Optional filter for listener health events. If omitted, receives all apps' health events |

**Validation Rules:**
- If `topics` is omitted, defaults to `task_progress` (preserves pre-RFC behavior)
- If `task_progress` is in `topics`, `task_id` must be provided (400 error otherwise)
- Unknown topics in the list are ignored (allows forward compatibility)
- `app` filter only applies to `listener_health` topic; ignored if that topic isn't subscribed

**Example requests:**
```
# Task progress only (existing behavior)
GET /api/v1/events/progress/stream?topics=task_progress&task_id=install-immich-123

# Listener health for specific app
GET /api/v1/events/progress/stream?topics=listener_health&app=immich

# Both topics - e.g., during app install with remote access enabled
GET /api/v1/events/progress/stream?topics=task_progress,listener_health&task_id=install-immich-123&app=immich

# All listener health events (dashboard overview)
GET /api/v1/events/progress/stream?topics=listener_health
```

**Implementation in `internal/server/gin_progress_stream.go`:**

```go
func (s *GinServer) handleGinTaskProgressStream(c *gin.Context) {
    // Parse topics with backward-compatible default
    topicsParam := strings.TrimSpace(c.Query("topics"))
    if topicsParam == "" {
        // Backward compatibility: omitted topics defaults to task_progress only
        // (matches pre-RFC behavior where endpoint only served task progress)
        topicsParam = "task_progress"
    }
    topics := strings.Split(topicsParam, ",")

    // Parse filters
    taskID := strings.TrimSpace(c.Query("task_id"))
    appFilter := strings.TrimSpace(c.Query("app"))

    // Build subscription set and validate required filters
    var subscribeTaskProgress, subscribeListenerHealth bool
    for _, t := range topics {
        switch strings.TrimSpace(t) {
        case "task_progress":
            subscribeTaskProgress = true
        case "listener_health":
            subscribeListenerHealth = true
        }
    }

    if !subscribeTaskProgress && !subscribeListenerHealth {
        writeGinError(c, http.StatusBadRequest, "No valid topics specified")
        return
    }

    // task_id required when subscribing to task_progress
    if subscribeTaskProgress && taskID == "" {
        writeGinError(c, http.StatusBadRequest, "task_id required when subscribing to task_progress")
        return
    }

    conn, err := wsupgrader.Upgrade(c.Writer, c.Request, nil)
    if err != nil {
        return
    }
    defer conn.Close()

    // ... context, mutex, sendJSON setup ...

    // Subscribe to requested topics
    var unsubscribes []func()
    merged := make(chan events.Event, 256)

    if subscribeTaskProgress {
        ch, unsub := s.events.SubscribeWithCancel(events.TopicTaskProgress, 256)
        unsubscribes = append(unsubscribes, unsub)
        go forwardEvents(ch, merged)
    }
    if subscribeListenerHealth {
        ch, unsub := s.events.SubscribeWithCancel(events.TopicListenerHealthChanged, 256)
        unsubscribes = append(unsubscribes, unsub)
        go forwardEvents(ch, merged)
    }
    defer func() {
        for _, unsub := range unsubscribes {
            unsub()
        }
    }()

    // ... keepalive ticker, read goroutine setup ...

    for {
        select {
        // ... keepalive, context done cases ...
        case evt, ok := <-merged:
            if !ok {
                return
            }
            switch payload := evt.Payload.(type) {
            case events.TaskProgressEvent:
                if payload.TaskID == taskID {
                    sendJSON(progressMessage{Type: "task_progress", Payload: payload})
                    if payload.IsComplete {
                        cancel()
                    }
                }
            case events.ListenerHealthEvent:
                if appFilter == "" || payload.App == appFilter {
                    sendJSON(progressMessage{Type: "listener_health", Payload: payload})
                }
            }
        }
    }
}
```

**WebSocket message format:**

```json
{
  "type": "listener_health",
  "payload": {
    "app": "immich",
    "listener": "http",
    "health": {
      "status": "ok",
      "reason_code": "ok",
      "reason": "Operational",
      "recoverable": true,
      "action_required": false
    },
    "timestamp": "2026-01-25T15:30:00Z"
  }
}
```

**Message types:**
- `"type": "task_progress"` - Task lifecycle events (install, uninstall, etc.)
- `"type": "listener_health"` - Listener health status changes
- `"type": "keepalive"` - Sent every 15 seconds to keep connection alive

## 8. Implementation Plan

### Phase 1: ACME Self-Healing (Backend)

1. Add failure classification (`FailureClass`, `classifyFailure()`) in `internal/remote/acme/issuer.go`
2. Add `FailureClass`, `FailureCode` fields to Certificate struct
3. Implement separate backoff functions: `transientBackoff()`, `rateLimitBackoff()`
4. Refactor `runRenewScheduler()` to be RetryAt-driven with wake signal (`scheduleWakeCh`)
5. Add `computeNextWakeTime()` helper (config-error certs included with weekly probe RetryAt)
6. Add `notifySchedulerWake()` called from `updateCertFailure()`
7. Config-error certs: schedule weekly probe (7 days), enable manual retry via existing `/renew` endpoint
8. Add event retention policy (`appendEventWithRetention()`) to prevent unbounded config growth
9. Add tests for rate limit detection, backoff logic, scheduler timing, and event retention

### Phase 2: Listener Health Model (Backend)

1. Define `ListenerHealth` and `ListenerHealthStatus` types (including `ActionRequired` field)
2. Implement `deriveListenerHealth()` function with `requiresRemoteCert` parameter
3. Add health computation to `ServiceManager.checkBackends()`
4. Store health state per-listener
5. Add `TopicListenerHealthChanged` event topic
6. Emit events on health status changes

### Phase 3: API Extensions (Backend)

1. Add `/api/v1/apps/{appId}/listeners/{listenerName}/health` endpoint
2. Create `AppInstanceWithHealth` response DTO (wraps `AppInstance`, adds `primary_listener_health`)
3. Extend certificate response with failure classification fields
4. Extend `/api/v1/events/progress/stream` to support `topics` parameter
5. Add `listener_health` topic subscription with `app` filter

### Phase 4: UI Health Display (Frontend)

1. Add `ListenerHealth` model to frontend
2. Create `_HealthBadge` widget for app cards
3. Add health badge to `AppCard` widget
4. Create `AppDetailHealthBanner` widget
5. Add banner to app detail page
6. Subscribe to health events for reactive updates

### Phase 5: Local Fallback UI (Frontend)

1. Create `LocalFallbackOverlay` widget
2. Make `AppLauncher.isLocalAccess()` public for reuse by overlay widgets
3. Add overlay trigger based on health policy
4. Use backend-provided `lanFallbackUrl` from endpoint response
5. Add "Access Locally" button with disclaimer

### Phase 6: Testing and Documentation

1. Add integration tests for self-healing retry
2. Add E2E tests for health badge display
3. Add E2E tests for local fallback overlay
4. Update remote access documentation
5. Add troubleshooting guide for common scenarios

## 9. Security Considerations

### 9.1 Rate Limit Information Disclosure

Rate limit status is visible to authenticated users. This is acceptable:
- Only authenticated users can access app health
- Rate limits are not sensitive (public Let's Encrypt policy)
- Transparency helps users understand system state

### 9.2 Local Fallback Security

Local fallback only works when physically on the same network:
- mDNS/DNS resolution required for `.local` domains
- No security bypass - user must authenticate on local access
- Clear disclaimer about network requirement

## 10. Relationship to Other RFCs

| RFC | Relationship |
|-----|--------------|
| RFC 20260122 | **Depends on:** Uses cert status from remote manager |
| RFC 20260114 | **Depends on:** Uses hostname scheme for local URLs |
| RFC 20260112 | **Extends:** Health affects auth flow (overlay before redirect) |

## 11. Open Questions

1. ~~**Health check frequency:**~~ **Resolved:** RetryAt-driven scheduler handles this. Scheduler wakes up based on earliest RetryAt, with 1-hour cap for regular health checks. See §5.4.

2. **User notification preference:** Should there be a setting to disable health overlays for power users who prefer raw errors?

3. **Health history:** Should we store health history for debugging, or only current state?

4. ~~**Multi-listener apps:**~~ **Resolved:** Aggregation uses worst-of:
   - **Per-listener:** Worst-of all CertStatuses + backend health (see `statusWorseThan()` in §4.4)
   - **Per-app card:** Shows `primary_listener_health.status` only (primary listener is the "main" HTTP entry point)
   - **App detail page:** Shows per-listener breakdown for all listeners
   - Priority order: `error` > `recovering` > `degraded` > `ok`

## 12. Implementation Notes & Status

- **Status:** Draft
- **Depends on:** Fix for ACME HTTP-01 challenges (commit 4d2d12a)
- **Related issue:** Grey screen on remote access failure
