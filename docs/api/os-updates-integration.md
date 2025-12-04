# Piccolo OS Update Integration Guide

This document outlines the integration flow for the Piccolo OS update mechanism.

**Philosophy: "Auto-Pilot"**
Piccolo OS (via MicroOS) is designed to update itself automatically.
1.  **Automatic Staging:** A background timer runs daily. It downloads updates and installs them into a new "offline" snapshot.
2.  **User Notification:** When an update is ready (staged), the UI simply notifies the user that a **Reboot** is required to apply it.
3.  **Manual Check:** The user can manually force a check/update (e.g., "Check Now") if they don't want to wait for the daily timer.

---

## The Update State Machine

### 1. Idle (System Up to Date)
The normal operating state. No updates are pending.

*   **Detection:** `pending` is `false`.
*   **UI:**
    *   Status: "System is up to date".
    *   Show: `current_version`.
    *   Action: **"Check for Updates"** button (calls `POST /updates/os/apply`).

### 2. Preparing Update (Background)
The system is actively downloading/installing updates. This happens automatically in the background (daily) OR because the user clicked "Check for Updates".

*   **Detection:**
    *   API calls return `429 Too Many Requests` (`"transactional-update in progress"`).
    *   OR `rpm_updates_available` > 0 but `pending` is `false` (rare transient state before download completes).
*   **UI:**
    *   Status: "Preparing updates..." or "Checking for updates...".
    *   Visuals: Progress spinner.
    *   Action: None (ReadOnly). Poll status every 10s.

### 3. Update Ready (Reboot Required)
An update has been successfully installed to a new snapshot. It is waiting for a reboot to become active.

*   **Detection:**
    *   **`pending` is `true`**.
    *   **`requires_reboot` is `true`**.
    *   `available_version` shows the new version string.
*   **UI:**
    *   Status: **"Update Ready to Install"**.
    *   Message: "Version `available_version` is ready. Restart to apply."
    *   Action: **"Restart Now"** (Primary Call-to-Action). calls `POST /updates/os/reboot`.

### 4. Rollback Available
Allows the user to revert to the previous system state if the current update caused issues.

*   **Detection:** Logic handled by UI (e.g., `default_snapshot_id` != `active_snapshot_id`).
*   **Action:** **"Rollback"** button. calls `POST /updates/os/rollback`.
*   **Outcome:** After rollback, the state becomes "Update Ready" (pending=true), requiring a reboot to return to the old version.

---

## API Reference

### 1. Get Status
`GET /api/v1/updates/os`

Returns the current state. Use this to drive the UI.

```json
{
  "current_version": "v0.1.0",
  "available_version": "v0.1.1",     // Shows new version if pending=true
  "pending": true,                   // TRUE = Update is staged, waiting for reboot
  "requires_reboot": true,           // TRUE = Show "Restart Now" button
  "meta": {
    "derived_outcome": "pending-reboot"
  }
}
```

### 2. Force Check / Download
`POST /api/v1/updates/os/apply`

*   **Use Case:** "Check for Updates" button.
*   **Behavior:** Forces the `transactional-update` process to run immediately.
*   **Response:**
    *   `200 OK`: Started successfully. State becomes "Preparing Update".
    *   `429 Too Many Requests`: Already running (auto-update is active). UI should just show "Preparing Update".

### 3. Reboot
`POST /api/v1/updates/os/reboot`

*   **Use Case:** "Restart Now" button (only visible when `pending=true`).
*   **Behavior:** Immediately reboots the appliance.

### 4. Rollback
`POST /api/v1/updates/os/rollback`

*   **Use Case:** Troubleshooting.
*   **Behavior:** Sets the previous snapshot as the target.
*   **Follow-up:** Returns `200 OK`. State becomes `pending=true`. User must then click "Restart Now".

---

## UI Implementation Summary

```javascript
// Pseudocode for UpdateComponent

if (status.pending) {
  // STATE: Update Ready
  return (
    <Banner type="info">
      <Text>Update {status.available_version} is ready.</Text>
      <Button onClick={api.reboot}>Restart Now</Button>
    </Banner>
  );
} else if (is429(status)) {
  // STATE: Preparing
  return <Spinner label="Checking for updates..." />;
} else {
  // STATE: Idle
  return (
    <Panel>
      <Text>Version: {status.current_version}</Text>
      <Text>System is up to date.</Text>
      <Button onClick={api.apply}>Check for Updates</Button>
    </Panel>
  );
}
```