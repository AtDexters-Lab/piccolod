# Setup Wizard (Implemented)

## Goal
Guide the user through the initial configuration of their Piccolo node: initializing storage encryption, creating the admin identity, and securing the recovery key.

## Architecture
*   **Location:** `ui/lib/shells/desktop/features/setup/`
*   **Entry Point:** `SetupWizard` widget.
*   **State Management:** `SetupController` (State Machine).
*   **Integration:** Conditionally rendered by `DesktopShell` when `DesktopController.needsSetup` is true. It overlays the entire desktop (Layer D/C) but sits below the Top Bar (Layer A).

## State Machine
The flow is linear, managed by `SetupState`:

1.  **Loading:** Checks `/crypto/status` (mocked).
2.  **Welcome:**
    *   Display: "Hello, [Device Name]".
    *   Action: User clicks "Start Setup".
3.  **Credentials:**
    *   Input: Password & Confirm Password.
    *   Features: `AutofillGroup` for password manager support, inline validation.
    *   Action: Submits to `/crypto/setup` (mocked).
4.  **Recovery:**
    *   Display: 24-word recovery key.
    *   Features: "Copy" button, "Download" button (Web-compatible), Mandatory "I have saved this" checkbox.
    *   Action: User confirms safety -> Setup Complete.
5.  **Complete:**
    *   Action: Triggers callback to `DesktopShell` to unlock the full UI.

## Key Components
*   `_WelcomeStep`: Hero text and call to action.
*   `_CredentialsStep`: Form with validation and loading state.
*   `_RecoveryStep`: Security-critical step with mandatory user confirmation.
*   `Downloader`: Utility (`core/utils/downloader`) handling web-based file downloads for the recovery key.