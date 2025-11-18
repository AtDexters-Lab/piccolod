#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "$0")" && pwd)"
cd "$SCRIPT_DIR"

chmod 644 piccolod.spec piccolod.service _service
chmod 755 fetch-release.sh
chmod 755 gen_service_file.sh
