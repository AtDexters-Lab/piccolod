# Setup Wizard (Implemented)

## Goal
Guide the user through the initial configuration of their Piccolo node: initializing storage encryption, creating the admin identity, and securing the recovery key.

## Architecture
*   **Location:** `ui/lib/shells/desktop/features/setup/`
*   **Entry Point:** `SetupWizard` widget.
*   **State Management:** `SetupController` (State Machine).
*   **Integration:** Conditionally rendered by `DesktopShell` when `DesktopController.needsSetup` is true. It overlays the entire desktop (Layer D/C) but sits below the Top Bar (Layer A).

## State Machine

The flow is managed by `SetupState`:



1.  **Loading:** Checks `/crypto/status` and `/auth/session`.

2.  **Welcome:**

    *   Display: "Hello, [Device Name]".

    *   Action: User clicks "Start Setup".

3.  **Credentials (Setup):**

    *   Input: Password & Confirm Password (using `PasswordSetForm` with strength indicator).

    *   Action:

        1.  `/crypto/setup` (Initialize encryption)

        2.  `/crypto/unlock` (Unlock storage)

        3.  `/auth/setup` (Create admin account)

        4.  Generate Recovery Key.

4.  **Unlock / Login:**

    *   States: `unlock` (Device locked) or `login` (Device unlocked but unauthenticated).

    *   Action: Authenticate user and fetch CSRF token.

5.  **Recovery (Reset Password):**

    *   State: `forgotPassword`.

    *   Input: Recovery Key + New Password.

    *   Action: Calls `/crypto/reset-password`.

6.  **Recovery Key Display:**

    *   Display: 24-word recovery key.

    *   Features: "Copy" (WASM-compatible), "Download" (WASM-compatible).

7.  **Complete:**

    *   Action: Triggers callback to `DesktopShell` to unlock the full UI.



## Key Components

*   `SetupWizard`: Main coordinator widget with Modal Barrier.

*   `PasswordSetForm`: Reusable widget for password entry with validation, visibility toggle, and strength meter.

*   `Downloader` & `Clipboard`: WASM-compatible utilities in `core/utils`.
