#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "$0")" && pwd)"
cd "$SCRIPT_DIR"

SPEC_FILE="piccolod.spec"
VERSION="$(sed -n 's/^Version:[[:space:]]*//p' "$SPEC_FILE" | head -n 1)"

if [[ -z "$VERSION" ]]; then
  echo "Failed to determine version from ${SPEC_FILE}" >&2
  exit 1
fi

cat > _service <<EOF
<services>
  <service name="download_url">
    <param name="url">https://github.com/AtDexters-Lab/piccolod/releases/download/v${VERSION}/piccolod-v${VERSION}-linux-x86_64</param>
    <param name="filename">piccolod-v${VERSION}-linux-x86_64</param>
  </service>
  <service name="download_url">
    <param name="url">https://github.com/AtDexters-Lab/piccolod/releases/download/v${VERSION}/piccolod-v${VERSION}-linux-aarch64</param>
    <param name="filename">piccolod-v${VERSION}-linux-aarch64</param>
  </service>
</services>
EOF
