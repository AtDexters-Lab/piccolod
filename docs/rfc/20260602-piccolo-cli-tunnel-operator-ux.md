# Piccolo CLI Tunnel Operator UX

**Date:** 2026-06-02
**Status:** Future draft

## Scope

**Problem:** The released Piccolo tunnel transport works for expert SSH and rsync use through OpenSSH `ProxyCommand`, but normal operators still have to manually assemble auth, API host, listener host, and transfer commands.
**In scope:** First-class `piccolo-cli` login/profile handling, listener discovery, copy-pasteable SSH/rsync command generation, thin SSH/rsync wrappers, failure taxonomy, and long-running transfer guidance for mTLS-protected tunnel listeners.
**Out of scope:** Changing the TLS-mux mTLS transport primitive, adding a Piccolo-specific file-transfer protocol, configuring `sshd` inside app containers, replacing OpenSSH/rsync behavior, VPN/P2P transport implementation, browser web-terminal changes, and relaxing TLS/server verification.

This RFC is a follow-up to `docs/rfc/20260601-tls-mux-connection-auth.md`. The core transport decision is validated: Piccolo should keep native OpenSSH/rsync/SCP semantics by acting as a TLS client-cert tunnel wrapper, not as an SSH or file-copy protocol.

---

## Background

Early operator testing of the v1 tunnel shape produced positive signal:

- OpenSSH `ProxyCommand` worked with normal SSH features.
- rsync worked for a long transfer and failed because the target volume filled, not because the tunnel collapsed.
- Host key checking, keepalives, remote commands, and agent forwarding remained native OpenSSH concerns.

The friction was not the transport. The friction was product shape:

- Auth via a copied browser session cookie feels like a debug path.
- Operators need a simple way to discover app listener hosts and ports.
- Common SSH and rsync use should not require hand-written `ProxyCommand` snippets every time.
- Failure text should distinguish auth, listener resolution, app health, upstream refusal, and remote command failure.
- Long transfers need recipes and expectations around keepalives, resume behavior, and progress.

---

## Design Principles

1. **Keep stdout payload-only in tunnel mode.** `piccolo tunnel` must remain safe as an OpenSSH `ProxyCommand`; login URLs, prompts, diagnostics, and progress must never be written to stdout before or during the payload stream.
2. **Use native tools instead of new protocols.** `piccolo ssh`, `piccolo scp`, and `piccolo rsync` are wrappers around OpenSSH-family tools, not Piccolo-specific SSH or file-copy implementations.
3. **Separate API identity from listener identity.** A device profile names the Piccolo API/portal origin. A tunnel target names the listener host and remote port. The CLI must not assume the app/listener host serves the certificate issuance API.
4. **Fail closed and explain on stderr.** If auth or certificate issuance cannot complete before the payload stream starts, the CLI exits nonzero with a concise diagnostic on stderr.
5. **Preserve TLS verification.** The CLI must verify the server certificate for the logical listener host. Physical dial address overrides, future P2P paths, and local testing do not create an `InsecureSkipVerify` default path.

---

## Decisions

### D1 - Add first-class CLI profiles and browser login

`piccolo login <portal-or-api-origin>` creates or updates a named device profile.

Profile state includes:

- profile name;
- Piccolo API/portal origin;
- known remote bases or aliases when available;
- trusted CA material or CA reference when needed for local/internal hostnames;
- cached auth material sufficient to request short-lived tunnel client certificates;
- last validated user identity and device hostname metadata for display.

Primary auth UX:

- `piccolo login https://piccolo.example.com` opens the user's browser.
- The browser uses Piccolo's existing web/passkey/password session flow.
- After approval, the CLI receives a bounded credential or session that can request tunnel certificates.
- The CLI stores the credential in the platform's secure credential store when available, with a restrictive local-file fallback only if explicitly accepted or configured.

Compatibility auth UX:

- `--session-cookie` remains as an expert/debug import path.
- Username/password login may remain for bootstrap and automation, but it is not the preferred operator path.
- Secrets must not be printed in help text, logs, or generated examples.

ProxyCommand rule:

- `piccolo tunnel ...` does not open a browser implicitly when stdout is the payload stream.
- If no usable profile credential exists, it exits nonzero and prints a stderr hint such as `run: piccolo login https://...`.
- An explicit non-ProxyCommand mode may open the browser, but the default tunnel behavior remains payload-safe.

### D2 - Make `piccolo tunnel` profile-aware and quiet by default

`piccolo tunnel <listener-host[:remote-port]>` uses the selected profile to request a Piccolo-session client certificate and then bridges stdio to the TLS stream.

Expected shape:

```text
piccolo tunnel --profile home ssh-drawguess.example.com
ssh -o ProxyCommand="piccolo tunnel ssh-drawguess.example.com" user@ssh-drawguess.example.com
```

Behavior:

- default remote port remains 443 unless the target or flag supplies a port;
- profile lookup happens before any stdout write;
- cert issuance happens before any stdout write;
- diagnostics go to stderr;
- once the TLS stream is established, stdout and stdin are raw payload only;
- no progress indicator is emitted by default in ProxyCommand-compatible mode.

### D3 - Add listener discovery for operators

Operators need one command that answers: "How do I reach this app?"

Expected command:

```text
piccolo apps listeners drawguess
```

Output should include:

- app name and running/stopped state;
- listener name, protocol, flow, remote host, effective remote ports, and whether `connection_auth.mtls` is required;
- SSH-friendly listener detection when protocol is raw and the listener name or metadata indicates SSH;
- copy-pasteable SSH, SCP, and rsync examples for mTLS raw listeners;
- the profile/API origin being used, so API host and app host are not confused.

The command may start from existing app-detail APIs if they already expose enough listener metadata. If not, add a read-only listener-discovery API that returns the same facts without exposing secret material.

### D4 - Add thin wrappers for common native tools

Wrappers should lower daily friction while preserving native behavior.

Expected shapes:

```text
piccolo ssh drawguess
piccolo ssh drawguess --listener ssh -- user@host-command
piccolo rsync ./images drawguess:/mnt/game-data/images/
piccolo scp ./file drawguess:/tmp/file
```

Wrapper behavior:

- resolve app/listener/profile into the same listener host and remote port used by `piccolo tunnel`;
- invoke OpenSSH, SCP, or rsync with an appropriate `ProxyCommand`;
- leave host key checking, known-hosts behavior, SSH identities, agent forwarding, remote command exit status, and rsync semantics to the native tools;
- display the generated native command with `--print-command`;
- provide `--` pass-through for native tool flags.

The wrappers are convenience commands, not a replacement for direct use of OpenSSH/rsync.

### D5 - Improve failure taxonomy without leaking protocol bytes

The CLI should classify failures before and after the payload stream boundary.

Pre-stream failures can be precise:

- missing profile or API origin;
- expired or missing CLI auth;
- login required;
- certificate issuance denied;
- listener not found;
- listener does not require/allow Piccolo tunnel auth;
- app stopped or listener unhealthy when known from preflight;
- TLS server verification failed;
- mTLS admission denied.

Post-stream failures are more constrained:

- once bytes are bridged, SSH/rsync owns many failures;
- remote command failure should be reported as the native tool's exit status and output;
- upstream connect refusal or immediate backend close may be summarized on stderr only when the CLI can identify it without writing protocol data to stdout.

Piccolod should return structured, non-secret error codes for tunnel certificate issuance and listener preflight. The TLS mux may keep wire behavior opaque while continuing to emit audit and metrics internally.

### D6 - Document long-running transfer ergonomics

The recommended rsync path should be documented rather than hidden behind a new transfer protocol.

Documentation should include:

- OpenSSH keepalive flags for long transfers;
- rsync flags for resumable copies, such as partial-file handling and append verification where appropriate;
- volume-full and permission-denied troubleshooting;
- expected behavior when the CLI credential expires after the TLS stream is already established;
- guidance that reconnect is owned by SSH/rsync invocation semantics, not by Piccolo mutating an active byte stream.

If the CLI adds activity counters, they must be opt-in and stderr-only. They should not appear by default in `ProxyCommand` mode.

### D7 - Keep future P2P/VPN as a dial-path swap

This RFC does not change the future P2P/VPN plan.

The CLI auth/profile/listener UX should compose with both paths:

```text
CLI -> TLS with client cert -> Nexus relay -> piccolod TLS mux -> listener upstream
CLI -> TLS with client cert -> P2P/VPN dial path -> piccolod TLS mux -> listener upstream
```

Profiles may later learn a preferred dial path, but the logical listener host, SNI, server verification, client certificate issuance, and payload bridging contract remain the same.

---

## Proposed Rollout

### Phase 1 - Auth and profile foundation

- Add `piccolo login`.
- Add profile storage and selection.
- Make `piccolo tunnel` consume profiles silently.
- Keep `--session-cookie` as an explicit expert path.
- Add tests for stdout cleanliness, expired auth behavior, and profile selection.

### Phase 2 - Discovery and copy-paste examples

- Add `piccolo apps listeners <app>`.
- Use existing app-detail APIs if sufficient; otherwise add a read-only listener-discovery endpoint.
- Include copy-pasteable SSH/SCP/rsync examples.
- Add OpenAPI docs for any new or expanded API shape.

### Phase 3 - Native-tool wrappers

- Add `piccolo ssh`.
- Add `piccolo scp` and/or `piccolo rsync`.
- Preserve native exit codes and pass-through behavior.
- Add `--print-command` for inspection and debugging.

### Phase 4 - Failure taxonomy and transfer docs

- Standardize CLI error wording and exit categories.
- Add structured server error codes where preflight or cert issuance lacks enough information.
- Publish operator docs for SSH config, rsync recipes, keepalives, and troubleshooting.

---

## Site List

### `../piccolo-cli`

- `main.go`: command routing, `login`, profile selection, tunnel auth, native-tool wrappers, failure text.
- `main_test.go`: stdout cleanliness, profile behavior, login-required behavior, command generation, and relay lifecycle coverage.
- future profile storage module: secure credential storage and local fallback policy.
- future command docs/README: operator examples and troubleshooting.

### `piccolod`

- `internal/server/gin_tunnel_handlers.go`: tunnel certificate issuance error codes and any listener preflight support.
- `internal/server/gin_app_handlers.go`: app/listener read response used for discovery, or read-only discovery endpoint if app details are insufficient.
- `internal/services/types.go`: listener metadata exposed to server/API responses, including mTLS requirement and effective remote hosts/ports.
- `internal/services/tlsmux.go`: no UX behavior change expected; remains the mTLS admission point and audit source.
- `docs/api/openapi.yaml`: schema/docs for any new login callback, profile-supporting endpoint, listener discovery, or structured tunnel error response.
- `docs/rfc/20260601-tls-mux-connection-auth.md`: remains the transport authority; update only if this RFC discovers a mismatch in the tunnel contract.
- future operator docs: SSH config, rsync recipes, keepalive guidance, and failure troubleshooting.

### External/native tools

- OpenSSH `ssh` and `scp`: wrappers must preserve native host-key, identity, agent, keepalive, and exit-code behavior.
- `rsync`: wrapper must preserve native transfer/resume semantics and not invent a Piccolo data-copy protocol.
- OS browser and credential stores: login flow and credential caching depend on platform-specific integrations.

---

## Open Questions

- Should the CLI credential be a regular Piccolo web session, a dedicated CLI session, or an OAuth-style device/session grant?
- Which platforms need secure credential store support in the first CLI release?
- Should non-admin users be allowed to request tunnel certificates for app listeners they are authorized to access, or should v1 CLI tunnel remain admin-only?
- Should listener manifests expose an explicit `usage: ssh` hint, or should CLI examples infer SSH from listener name/protocol only?
- What should the canonical `~/.ssh/config` snippet look like for users who prefer stable SSH host aliases?

---

## Implementation Notes & Status

Future draft only. No implementation has landed from this RFC yet.

The transport foundation landed in `v0.2.24` through `docs/rfc/20260601-tls-mux-connection-auth.md`. This RFC captures the follow-up CLI/operator UX work motivated by early SSH/rsync tunnel testing.
