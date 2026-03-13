# Remote Access UI Implementation Guide

This document serves as the comprehensive reference for the frontend team implementing the Piccolo OS Remote Access ("Nexus") management interface.

## 1. Overview

The Remote Access feature allows a Piccolo node to be accessible from the public internet via a secure, device-terminated TLS tunnel (Nexus). It handles:
- **Tunneling:** Websocket connection to a Nexus relay.
- **TLS Termination:** Automatic Let's Encrypt certificates (HTTP-01 or DNS-01).
- **Aliases:** Custom hostnames for specific services.

**Base API Path:** `/api/v1/remote`

**Architecture Note:** The backend does not currently support Server-Sent Events (SSE) or Websockets for UI notifications. **The UI must poll** `GET /api/v1/remote/status` every 5-10 seconds to maintain an up-to-date dashboard state.

---

## 2. Dashboard & Status

The entry point is the **Status** endpoint, which dictates the UI state.

**GET** `/api/v1/remote/status`

### Response & UI States

The backend returns a specific `state` string derived from the configuration and health. Use this to drive the main dashboard view.

| State | Condition | UI Implication |
| :--- | :--- | :--- |
| **`disabled`** | `enabled: false`, no config | **Fresh Install.** Show "Setup Remote Access" CTA / Setup Wizard. |
| **`stopped`** | `enabled: false`, config exists | **Paused.** Config is saved but off. Show Setup Wizard (pre-filled) or "Resume". |
| **`preflight_required`** | `enabled: true`, no preflight run | **Incomplete.** Configuration is saved but not validated. Prompt to run Preflight. |
| **`active`** | `enabled: true`, no issues | **Healthy.** Show full dashboard (Connection: Connected). |
| **`warning`** | `enabled: true` + warnings present | **Degraded.** Show dashboard with warning badges (e.g., "Portal hostname missing"). |
| **`error`** | `enabled: true` + cert expired | **Critical.** Show error alert. Certificate is invalid/expired. |

### Key Fields for Dashboard
*   `portal_hostname`: The main entry point.
*   `latency_ms`: Round-trip time to Nexus relay.
*   `next_renewal`: Date when the certificate will attempt renewal.
*   `warnings`: Array of strings. **Must be displayed prominently.**
    *   *"Portal hostname missing"* -> Critical configuration gap.
    *   *"Certificate renewal due soon"* -> Warning.

---

## 3. Configuration Flow (Setup Wizard)

The setup process should be a multi-step wizard to ensure valid configuration. **Note: The backend is stateless during the wizard.** The UI must persist the form data in memory until the final step.

### Configuration Modes

Piccolo supports two remote access configuration modes:

| Mode | API Endpoint | Challenge | DNS Provider | Wildcard Certs |
|------|--------------|-----------|--------------|----------------|
| **User-managed** | `POST /api/v1/remote/configure` | HTTP-01 | N/A | No |
| **Managed** (Piccolo Space) | `POST /api/v1/remote/managed/configure` | DNS-01 | Piccolo orchestrator | Yes |

### Step 1: Nexus Helper (Optional but Helpful)
If the user is self-hosting the Nexus relay (user-managed mode), they need to run a command on their VPS.
*   **GET** `/api/v1/remote/nexus-guide`
*   **UI:** Display the `command` and `notes`.
*   **Verification:** Use **POST** `/api/v1/remote/nexus-guide/verify` to validate credentials.
    *   **Payload:** `{ "endpoint": "...", "portal_hostname": "...", "jwt_secret": "..." }`
    *   **Stateless:** This validates the connection but **does not** save the config to disk.
    *   **UI Logic:** On success, store these credentials in the wizard state (memory) and proceed to Step 2.

### Step 2: Preflight Checks
Before finalizing, run validation to ensure the network environment is ready.
*   **POST** `/api/v1/remote/preflight`
*   **Payload:** Send the configuration gathered so far (from Step 1) to validate it against the backend.
    *   Example: `{ "endpoint": "...", "device_secret": "...", "portal_hostname": "..." }`
*   **Returns:** A list of checks with `name`, `status` (`pass`, `warn`, `fail`), and `detail`.
*   **UI:** Show the list. Block progress if any check status is `fail`.

### Step 3a: User-Managed Configuration (HTTP-01)
**POST** `/api/v1/remote/configure`

This mode is for users self-hosting their Nexus relay. It uses HTTP-01 challenge which requires port 80 to be accessible from the internet.

#### Fields:
1.  **Endpoint (`endpoint`):**
    *   URL of the Nexus relay (e.g., `wss://nexus.example.com`).
2.  **Device Secret (`device_secret`):**
    *   Shared secret for authentication.
3.  **Portal Hostname (`portal_hostname`):**
    *   **CRITICAL:** The fully-qualified domain name for the admin panel (e.g., `portal.home.example.com`).
    *   App subdomains will be derived from this hostname (e.g., `myapp.portal.home.example.com`).

#### Behavior:
*   Always uses HTTP-01 challenge (solver is implicit)
*   Issues a portal certificate for `portal_hostname`
*   Issues per-listener certificates for HTTP/WS app endpoints (`<listener>.<portal_hostname>`)
*   **No wildcard support** - each app listener gets its own certificate
*   Apps installed before or after remote enable are queued automatically
*   Failed issuances are retried with exponential backoff

#### Payload Example
```json
{
  "endpoint": "wss://nexus.piccolo.link",
  "device_secret": "my-secret-key",
  "portal_hostname": "piccolo.home.piccolo.link"
}
```

### Step 3b: Managed Configuration (DNS-01 via Piccolo Orchestrator)
**POST** `/api/v1/remote/managed/configure`

This mode is for Piccolo Space managed devices. It uses DNS-01 challenge via the Piccolo orchestrator, which supports wildcard certificates.

#### Fields:
1.  **Orchestrator Endpoint (`orchestrator_endpoint`):**
    *   URL of the Piccolo orchestrator API.
2.  **Device Token (`device_token`):**
    *   Device authentication token from Piccolo Space.
3.  **Portal Hostname (`portal_hostname`):**
    *   The fully-qualified domain name for the admin panel (e.g., `portal.home.example.com`).

#### Behavior:
*   Always uses DNS-01 challenge (solver is automatic)
*   Issues a portal certificate for `portal_hostname`
*   Issues a **wildcard certificate** covering `*.<portal_hostname>` and `<portal_hostname>`
*   All apps automatically covered by wildcard - no per-listener certificates needed
*   Failed issuances are retried with exponential backoff

#### Payload Example
```json
{
  "orchestrator_endpoint": "https://api.piccolo.space",
  "device_token": "device-auth-token",
  "portal_hostname": "piccolo.home.piccolo.link"
}
```

*   **Action:** This is the **Commit** step. It saves the configuration to disk and enables the service.
*   **Success:** Transition UI to "Active" dashboard.


---

## 4. Management Features

### Disabling Remote Access
*   **POST** `/api/v1/remote/disable`
*   **UI:** Destructive action confirmation. "This will stop external access immediately."

### Rotating Secrets
If the connection is stuck or the secret leaked.
*   **POST** `/api/v1/remote/rotate`
*   **UI:** "Rotate Credentials" button. Updates the `device_secret` returned in the response.

### Certificate Management
*   **GET** `/api/v1/remote/certificates`
*   **UI:** Table showing domains, expiry, and status.
*   **Action:** "Renew Now" button -> **POST** `/api/v1/remote/certificates/{id}/renew`.

### Activity Log
*   **GET** `/api/v1/remote/events`
*   **UI:** A timeline or log view. Useful for debugging connection issues.
    *   `level`: `info`, `warn`.
    *   `message`: Human-readable event.

---

## 5. Aliases (Advanced)

Allows exposing other services on custom subdomains.

*   **GET** `/api/v1/remote/aliases`
*   **Create:** **POST** `/api/v1/remote/aliases`
    *   `listener`: The internal service name.
        *   **UI Hint:** Populate this as a dropdown by fetching **GET** `/api/v1/services`. Use the `name` field from the returned services list.
    *   `hostname`: The desired public hostname.
*   **Delete:** **DELETE** `/api/v1/remote/aliases/{id}`

---

## 6. Common Warnings & Handling

| Warning String | Implication | UI Action |
| :--- | :--- | :--- |
| `"Portal hostname missing"` | Remote access is on, but you can't reach the admin panel. | **High Priority Alert.** Prompt user to Configure -> set `portal_hostname`. |
| `"Certificate renewal due soon"` | Cert expires in < 7 days. | Amber warning badge. "Check Logs" or "Renew Now". |
| `"Alias <host> certificate pending"` | An alias is waiting for certificate issuance. | Show status badge in the aliases list. |

---

## 7. Security Notes

*   **CSRF:** All `POST/DELETE` requests require the `X-CSRF-Token` header.
*   **Locked State:** If the disk encryption is locked (`423 Locked`), remote configuration cannot be changed. The UI should redirect to the Unlock screen or show a modal.
