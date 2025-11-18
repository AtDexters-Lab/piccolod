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

BASE_URL="https://github.com/AtDexters-Lab/piccolod/releases/download/v${VERSION}"

for ARCH in linux-x86_64 linux-aarch64; do
  FILE="piccolod-v${VERSION}-${ARCH}"
  if [[ -f "$FILE" ]]; then
    echo "$FILE already present; skipping download"
    continue
  fi
  echo "Downloading ${FILE} from ${BASE_URL}"
  curl -fsSL -o "$FILE" "${BASE_URL}/${FILE}"
  chmod 755 "$FILE"
done
