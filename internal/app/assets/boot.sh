#!/bin/sh
# /piccolo/boot.sh - Universal wrapper for Piccolo Workspace containers
# Injected by piccolod as a read-only host mount.
#
# This script provides the "VPS Experience" by:
# 1. Running optional user startup hooks
# 2. Executing the container's original command
# 3. Keeping the container alive if the command exits cleanly

set -e

# 1. Run user startup hook (non-blocking)
# The hook lives on the container's persistent overlay filesystem.
USER_HOOK="/piccolo/config/start.sh"

if [ -f "$USER_HOOK" ]; then
    # Auto-chmod: make executable if it exists but isn't
    if [ ! -x "$USER_HOOK" ]; then
        echo "[piccolo] Making $USER_HOOK executable"
        chmod +x "$USER_HOOK"
    fi
    echo "[piccolo] Starting user hook: $USER_HOOK"
    "$USER_HOOK" &
fi

# 2. Run the image's original command
# We DO NOT use 'exec' so we can catch the exit code.
echo "[piccolo] Starting primary command: $@"
"$@"
EXIT_CODE=$?

# 3. Smart Fallback
if [ $EXIT_CODE -eq 0 ]; then
    # Success! The command finished (e.g., 'bash' exited immediately).
    # In a normal container, this means death.
    # In a Workspace, this means "Ready for terminal access".
    echo "[piccolo] Primary command exited cleanly (0). Staying alive for workspace access..."
    exec tail -f /dev/null
else
    # Failure. Let the container die so the user sees the error/restart policy.
    echo "[piccolo] Primary command failed with code $EXIT_CODE."
    exit $EXIT_CODE
fi
