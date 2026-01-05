# RFC: Workspace "VPS Mode" and Smart Image Picker

**Date:** 2025-12-30
**Status:** Draft

## 1. Summary
This RFC proposes a "Micro-IaaS" (Infrastructure as a Service) experience for Piccolo OS, allowing users to launch persistent, mutable containers ("Workspaces") that behave like lightweight Virtual Private Servers (VPS). This eliminates the friction of building Docker images on an external machine for simple or experimental apps. It introduces a "Smart Base Image Picker" that wraps `podman search` and a universal `boot.sh` wrapper that ensures containers remain accessible (keep-alive) even if their main process exits.

## 2. Problem Statement
Currently, to deploy a custom app on Piccolo, a user must:
1.  Write a `Dockerfile`.
2.  Build the image on a separate development machine.
3.  Push the image to a container registry.
4.  Write a Piccolo `app.yaml` manifest.
5.  Install via the "Custom App" wizard.

This workflow is "high friction" for tinkerers who just want to host a static HTML site from a public repo, run a private Node.js app without CI/CD, or experiment with a "blank slate" Linux environment.

## 3. Proposed Solution: Micro-IaaS / VPS Mode

### 3.1 Philosophy: "Container as a VM"
Instead of treating containers as immutable artifacts ("cattle"), Workspace Mode treats them as persistent, mutable servers ("pets").
*   **Persistence:** The root filesystem is mutable and persists across reboots (`x-piccolo.mode: workspace`).
*   **Access:** The user interacts primarily via a Web Terminal (SSH/exec).
*   **Lifecycle:** The container adapts to the workload, staying alive even if the primary process is a shell that exits immediately.

### 3.2 Architecture: The Universal Wrapper

To achieve the "VPS Experience" reliably across *any* standard Docker image (Nginx, Node, Ubuntu), Piccolo injects a smart `boot.sh` wrapper into **all** containers where `x-piccolo.mode: workspace`.

**The `boot.sh` Logic:**
```bash
#!/bin/sh
# /piccolo/boot.sh - Injected by piccolod (Read-Only Host Mount)

# 1. Run user startup hook (non-blocking)
# We check a specific config path to avoid colliding with image-native paths like /workspace
USER_HOOK="/piccolo/config/start.sh"

if [ -x "$USER_HOOK" ]; then
    echo "Starting user hook: $USER_HOOK"
    "$USER_HOOK" &
fi

# 2. Run the image's original command (e.g. "nginx", "node", or "/bin/bash")
# We DO NOT use 'exec' so we can catch the exit code.
echo "Starting primary command: $@"
"$@"
EXIT_CODE=$?

# 3. Smart Fallback
if [ $EXIT_CODE -eq 0 ]; then
    # Success! The command finished (e.g., 'bash' exited immediately).
    # In a normal container, this means death.
    # In a Workspace, this means "Ready for SSH".
    echo "Primary command exited cleanly (0). Staying alive for workspace access..."
    exec tail -f /dev/null
else
    # Failure. Let the container die so the user sees the error/restart policy.
    echo "Primary command failed with code $EXIT_CODE."
    exit $EXIT_CODE
fi
```

**Implementation Details:**
*   **Mount:** `boot.sh` is mounted as a **read-only** bind mount from the host system into `/piccolo/boot.sh`. It does NOT live on the user's persistent volume.
*   **User Hook:** `start.sh` lives at `/piccolo/config/start.sh` on the container's mutable root filesystem (the overlay). Since `mode: workspace` persists the overlay, this file persists automatically.
*   **Entrypoint Injection:** `piccolod` inspects the image configuration (Entrypoint + Cmd) and constructs the new entrypoint: `["/bin/sh", "/piccolo/boot.sh", "original_entrypoint", "original_args"...]`.
*   **PID 1 Safety:** We explicitly use Podman's `--init` flag for all workspaces. This ensures a proper init process (like `catatonit`) handles signal forwarding and zombie reaping.
*   **Compatibility:** Workspace images **must** provide `/bin/sh` and basic coreutils (`tail`). Distroless or `scratch` images are explicitly incompatible with Workspace Mode.

## 4. Feature: Smart Base Image Picker

To allow users to choose their "OS" easily:

### 4.1 Curated Images ("Featured")
A `catalog.json` served by `piccolo-store` defines curated, high-quality workspaces.
*   **Node.js:** `node:20-alpine` (Pinned version, small)
*   **Python:** `python:3.11-slim` (Pinned version)
*   **Static Web:** `nginx:alpine` (Pinned version)
*   **Blank OS:** `ubuntu:22.04` (Pinned version)

### 4.2 Pass-Through Registry Search
Users can search Docker Hub directly from the UI.
*   **Mechanism:** `piccolod` wraps the local `podman search` CLI.
*   **Privacy:** Requests originate from the user's device IP (no central proxy).
*   **Auth:** Uses the user's local `podman login` credentials if available.
*   **Tag Limitations:** `podman search` does not list tags.
    *   **MVP:** The UI defaults to `latest` but displays a clear warning: *"Using 'latest' tag. Recommended: Manually specify a version (e.g., :16) to prevent breaking changes."*
    *   We do not implement a complex tag-listing API in this phase.

## 5. User Workflows

### Scenario A: Public Static Site (Nginx)
1.  **Create:** User selects "Nginx" from Featured list.
2.  **Runtime:** `boot.sh` runs `nginx -g daemon off;`. It blocks forever. Site works.
3.  **Deploy:** User opens Terminal, clones their HTML into `/usr/share/nginx/html`.
4.  **Expose:** User adds Listener: Public 80 -> Container 80.

### Scenario B: Private Node.js App
1.  **Create:** User selects "Node.js 20".
2.  **Runtime:** `boot.sh` runs `node` (REPL). It exits immediately. `boot.sh` catches exit code 0 and runs `tail -f`. Container stays green.
3.  **Install:** User opens Terminal, clones private repo (interactive auth), runs `npm install`.
4.  **Persistence:** User creates `/piccolo/config/start.sh` containing `pm2 start server.js`.
5.  **Restart:** User restarts container. `boot.sh` sees `start.sh`, runs it (server starts), then runs `node` (exits), then falls back to `tail -f`.
6.  **Result:** App is running, accessible, and persistent.

## 6. Technical Execution Plan

### Phase 1: Container Backend Updates
*   Implement `SearchRegistry` in `PodmanCLI`.
*   Add `boot.sh` script asset to `piccolod` binary (embedded).

### Phase 2: App Manager Logic
*   Implement `InstallWorkspace(image, name)` method.
*   Update `appDefToContainerSpec`:
    *   Inject `--init`.
    *   Mount `boot.sh`.
    *   Rewrite Entrypoint to wrap original command.

### Phase 3: API & UI
*   `GET /api/v1/images/search`.
*   New "Create Workspace" Wizard in Flutter UI.

## 7. Security Considerations
*   **Privileges:** Workspaces run as unprivileged (rootless) containers.
*   **Isolation:** Per-app encrypted volumes.
*   **No SSHD:** We do NOT run `sshd` inside the container; access is via `podman exec` (Web Terminal).

## Implementation Notes & Status
*   _Pending Implementation_
