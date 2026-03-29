# RFC: Namek Enrollment in Setup Wizard

**Date:** 2026-03-28
**Status:** Draft

## Motivation

Remote access via Namek should feel like a native organ of Piccolo, not a bolt-on configured separately. Today, enrollment lives entirely in Settings > Remote Access — a post-setup action most users never discover. The goal is to integrate it into the initial setup so every device gets a globally unique internet address (`yourname.piccolospace.com`) from minute one, with password, passkey, and session all bound to the right domain.

## Summary

1. Auto-enroll the device at boot (no user interaction)
2. Add a **"Choose Your Address"** step to the setup wizard (between Welcome and Password)
3. After claiming an address, **redirect to the remote domain** — the rest of setup happens there
4. Password creation, recovery key, and passkey registration all occur on the remote domain
5. Offline/unenrolled devices get the current LAN-only flow with CA cert step

## The Flow

### Online (enrolled):

```
LAN origin (piccolo-abc123.local):
  Welcome → Address (claim coolname) → generate setup continuation token → redirect

Remote origin (coolname.piccolospace.com):
  exchange token → Password → Recovery → Passkey → Done
```

### Offline (not enrolled):

```
LAN origin (piccolo-abc123.local):
  Welcome → [Address: offline message, skip] → Password → Recovery → Security (CA cert) → Done
```

## Why Setup Migrates to the Remote Domain

1. **Password managers**: The browser saves the password for the domain where it's created. If password is created on `piccolo-abc123.local` but the user logs in at `coolname.piccolospace.com`, auto-generated passwords become inaccessible.

2. **Passkey RP ID**: WebAuthn credentials are bound to the relying party ID (derived from the domain). A passkey registered on the LAN hostname won't work on the remote domain.

3. **Session cookies**: Sessions are origin-bound. Creating the session on the remote domain means the user is immediately authenticated where they'll actually use the device.

4. **Mental model**: The user's Piccolo is `coolname.piccolospace.com`. Setup should establish that identity from the moment it's chosen.

## Architecture

### Auto-Enrollment at Boot

**File:** `internal/identity/service.go`

The identity service already has enrollment logic. Add: if TPM is available, enabled, and not yet enrolled, auto-enroll during `Start()` in a background goroutine with exponential backoff. Idempotent — exits if enrollment succeeds via another path or if identity is disabled.

The goroutine must:
- Not block supervisor startup
- Respect `Stop()` lifecycle (check `stopped`, listen on `stopCh`)
- Track via `recoverWg` for graceful shutdown
- Check `IsEnabled()` to respect disable requests

### Setup Continuation Token

A one-time, short-lived (5 min) token that authorizes setup operations on the remote origin. Generated on the LAN where the user has physical access; exchanged on the remote origin to unlock setup endpoints.

**File:** `internal/auth/manager.go` — new type on `SessionStore`:

```go
type SetupContinuationToken struct {
    Token     string
    UserID    string // empty for pre-setup (no admin user yet)
    ExpiresAt int64
}
```

At most one active token. `CreateSetupContinuationToken()` replaces any existing. `ExchangeSetupContinuationToken(token)` validates, consumes, returns the token info.

### Setup-Gated Remote Endpoints

During initial setup (crypto not initialized), the following LAN-only endpoints need to also work from the remote origin when a valid setup continuation token is presented:

| Endpoint | Current access | With token |
|----------|---------------|------------|
| `POST /crypto/setup` | `lanPublic` | `lanPublic` + remote with token |
| `POST /crypto/recovery-key/generate` | `authed` (admin) | also via token exchange session |
| `POST /identity/setup-hostname` | new, `lanPublic` + `requireSetupState` | same |

**Implementation:** A new middleware `requireSetupContinuation()` that:
1. Checks if crypto is initialized — if yes, 403
2. Reads `X-Setup-Token` header from request
3. Validates the token via `SessionStore.ExchangeSetupContinuationToken()`
4. If valid, allows the request
5. If missing/invalid, falls through to existing LAN-only check

This means the endpoints work via two paths:
- LAN-only (existing, no token needed)
- Remote + valid setup token (new)

Register setup-critical endpoints under a group that applies both `allowLANOnly` OR `requireSetupContinuation`:

```go
// Setup endpoints: LAN-only OR remote with setup continuation token
setupGroup := v1.Group("/")
setupGroup.Use(s.allowLANOrSetupToken())
setupGroup.POST("/crypto/setup", s.handleCryptoSetup)
setupGroup.POST("/identity/setup-hostname", s.requireSetupState(), s.handleIdentitySetHostname)
```

### Boot Response Enhancement

**File:** `internal/server/gin_boot_handler.go`

When screen is "setup", include:
```json
{
  "screen": "setup",
  "namek_enrolled": true,
  "namek_base_domain": "piccolospace.com"
}
```

### Redirect Flow

After the user claims a hostname on the LAN:

1. Frontend calls `POST /api/v1/auth/setup-continuation-token` (new, LAN-only, pre-auth + `requireSetupState`)
2. Backend generates token, returns `{"token": "..."}`
3. Frontend redirects to `https://coolname.piccolospace.com?setup_token=TOKEN`
4. Remote setup wizard parses token from URL
5. Stores token, includes it as `X-Setup-Token` header on subsequent API calls
6. Boot check returns `screen: "setup"` (crypto not initialized)
7. Password creation via `POST /crypto/setup` succeeds (token authorizes it)
8. Session is created on the remote origin — password manager saves for the right domain
9. Recovery key generation uses the new session (admin auth)
10. Passkey registration happens on the remote origin (correct RP ID)
11. Setup completes — user is on the right domain with everything bound correctly

### Offline/Unenrolled Path

If the device is not enrolled when the user reaches the Address step:
- Show informational message: "You can set up remote access later in Settings > Remote Access"
- Skip to Password step on the LAN origin
- Security step (CA cert download) shown as today
- Standard local-only setup completes

## Frontend Changes

### Setup State Machine

```dart
enum SetupState {
  // ... existing ...
  remoteAddress,  // NEW: between welcome and credentials
}
```

**Transitions (online):**
```
welcome → remoteAddress → [redirect to remote] → credentials → recovery → complete
```

**Transitions (offline):**
```
welcome → remoteAddress → [skip] → credentials → recovery → security → complete
```

### Setup Controller

New fields:
```dart
bool _namekEnrolled;          // from boot response
String _namekBaseDomain;      // from boot response, fallback 'piccolospace.com'
String? _chosenHostname;      // user's claimed hostname
String? _setupToken;          // continuation token (from URL on remote, or generated on LAN)
bool _settingHostname;        // in-flight SetHostname
String? _hostnameError;       // server validation error
```

New methods:
- `submitHostname(hostname)` — calls setup-hostname, on success generates continuation token and redirects
- `skipAddress()` — proceeds to credentials locally
- `_exchangeSetupToken()` — called on remote origin load, validates token before boot check
- `_refreshEnrollmentStatus()` — re-checks boot response when entering address step (auto-enrollment may have completed)

### API Client: Setup Token Header

When `_setupToken` is set, the `ApiClient` includes `X-Setup-Token: <token>` on all requests. This is cleared after crypto/setup creates a real session.

### Setup Wizard UI

**Stepper (online):** Welcome → Address → Password → Recovery → Passkey
**Stepper (offline):** Welcome → Address → Password → Recovery → Security

The stepper adapts based on whether a hostname was claimed. The Security step (CA cert) only shows in the offline path.

**`_RemoteAddressStep` widget:**

Enrolled mode:
- Text field with `.{baseDomain}` suffix
- Continue calls `submitHostname`, shows server errors inline
- "Skip for now" link

Not enrolled mode:
- Informational message about remote access
- Continue skips to password

## Security

### Setup continuation token
- **One-time use**: consumed on first API call that validates it
- **Short-lived**: 5-minute TTL
- **LAN-generated**: only the LAN setup wizard can create it (requires physical/LAN access)
- **Gated scope**: only unlocks setup-phase endpoints (crypto not initialized check)
- **No privilege escalation**: the token doesn't grant admin access — it grants the ability to *create* the admin (which anyone with LAN access can already do)

### LAN race during setup window
Any LAN peer could theoretically call setup-hostname before the rightful owner. This is consistent with the existing threat model: `crypto/setup` has the same exposure. LAN is treated as a physical-access trust zone during setup.

### Token in URL
The setup continuation token appears in the redirect URL query string. It's single-use and short-lived, so URL history/logs exposure is bounded. Same pattern as invite tokens.

## Error Handling

| Scenario | Behavior |
|----------|----------|
| No internet at boot | Auto-enrollment retries in background. Address step shows offline message. |
| No TPM | Not enrolled. Address step shows offline message. |
| Enrolled, hostname taken | Server error shown inline, user picks another |
| Enrollment in progress when user reaches address | Re-check boot response. If still not enrolled, show offline message. |
| Token generation fails | Fall back to local setup (skip redirect) |
| Token exchange fails on remote | Show login screen (user enters password they haven't set yet → show setup screen) |
| Remote domain unreachable after redirect | Browser error. User can navigate back to LAN origin. |
| Cert not ready on remote domain | Nexus relay terminates TLS — valid HTTPS regardless of device cert status |

## Related: Gateway PreferredName (RFC 2, independent)

The `piccolo.local` gateway device list should show the Namek custom hostname as the display label for enrolled devices. This is independent work:

- Add `preferredName` field to mDNS Manager (persistent, survives `rebuildServiceMetadata`)
- Propagate custom hostname via `applyNamekState` → `SetPreferredName`
- Add `preferred_name` to network peers API and WebSocket event
- Update Flutter `DiscoveredPeer`/`NetworkSelf` models to use `preferredName` for `displayName`

## Testing

### Backend
- Auto-enrollment: idempotent, retries on failure, respects stop/disable
- `requireSetupState` middleware: allows pre-crypto-init, blocks post-init
- Setup continuation token: one-time use, expiry, exchange
- `allowLANOrSetupToken` middleware: LAN passes without token, remote passes with token, remote without token blocked
- Setup-hostname delegation to existing handler

### Frontend
- Address step: enrolled mode (submit, error, retry), offline mode (skip)
- Token generation + redirect flow
- Token exchange on remote origin load
- Stepper: 5 steps online (no Security), 5 steps offline (no Passkey, yes Security)
- Password saved on correct domain (manual verification)

### E2E
- Fresh setup with internet → claim address → redirect → password on remote → passkey → done
- Fresh setup without internet → skip → password on LAN → CA cert → done
- Hostname taken → inline error → pick another → success

## Critical Files

**Backend:**
- `internal/identity/service.go` — auto-enrollment
- `internal/auth/manager.go` — `SetupContinuationToken` type, create/exchange methods
- `internal/server/gin_middleware.go` — `requireSetupState`, `allowLANOrSetupToken`
- `internal/server/gin_server.go` — route registration
- `internal/server/gin_boot_handler.go` — enrollment status in boot response
- `internal/server/gin_auth_handlers.go` — token generation + exchange endpoints

**Frontend:**
- `ui/lib/shells/desktop/features/setup/setup_controller.dart` — state machine, token handling, redirect
- `ui/lib/shells/desktop/features/setup/setup_wizard.dart` — address step, adaptive stepper
- `ui/lib/core/services/api_client.dart` — `X-Setup-Token` header injection
