#!/usr/bin/env bash
# dev-vm.sh — Build piccolod, inject into a Piccolo OS VDI, and boot a fresh VM.
#
# Usage:
#   ./scripts/dev-vm.sh /path/to/piccolo-os.vdi.xz   # first run (decompresses)
#   ./scripts/dev-vm.sh                                # reuse cached VDI
#   ./scripts/dev-vm.sh --destroy                      # stop + delete current VM
#
# Requires: VBoxManage, qemu-nbd, qemu-img (qemu-utils)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
WORK_DIR="/tmp/claude/piccolo-e2e"
VDI_PRISTINE="$WORK_DIR/piccolo-os-pristine.vdi"
VDI_WORK="$WORK_DIR/piccolo-os.vdi"
MNT="$WORK_DIR/mnt"
TEMPLATE="piccolo-template"
DISK_SIZE_MB=28672  # 28 GB
VM_STATE_FILE="$WORK_DIR/vm-name"
NBD_DEV="/dev/nbd0"

die() { echo "ERROR: $*" >&2; exit 1; }

# --- Destroy mode ---
if [[ "${1:-}" == "--destroy" ]]; then
  if [[ -f "$VM_STATE_FILE" ]]; then
    VM_NAME=$(cat "$VM_STATE_FILE")
    echo "Stopping VM: $VM_NAME"
    VBoxManage controlvm "$VM_NAME" poweroff 2>/dev/null || true
    sleep 2
    VBoxManage unregistervm "$VM_NAME" --delete 2>/dev/null || true
    rm -f "$VM_STATE_FILE"
    echo "VM destroyed."
  else
    echo "No active VM found."
  fi
  exit 0
fi

# --- Ensure working directory ---
mkdir -p "$WORK_DIR" "$MNT"

# --- Step 0: Decompress source VDI if argument given ---
if [[ -n "${1:-}" ]]; then
  SRC="$1"
  if [[ "$SRC" == *.xz ]]; then
    echo "==> Decompressing $SRC → $VDI_PRISTINE"
    xz -dk "$SRC" -c > "$VDI_PRISTINE"
  elif [[ "$SRC" == *.vdi ]]; then
    echo "==> Copying $SRC → $VDI_PRISTINE"
    cp "$SRC" "$VDI_PRISTINE"
  else
    die "Unsupported format: $SRC (expected .vdi or .vdi.xz)"
  fi
fi

[[ -f "$VDI_PRISTINE" ]] || die "No pristine VDI found. Run: $0 /path/to/piccolo-os.vdi.xz"

# --- Step 1: Stop existing VM if running ---
if [[ -f "$VM_STATE_FILE" ]]; then
  OLD_VM=$(cat "$VM_STATE_FILE")
  echo "==> Stopping previous VM: $OLD_VM"
  VBoxManage controlvm "$OLD_VM" poweroff 2>/dev/null || true
  sleep 2
  VBoxManage unregistervm "$OLD_VM" --delete 2>/dev/null || true
  rm -f "$VM_STATE_FILE"
fi

# --- Step 2: Build piccolod ---
echo "==> Building piccolod"
cd "$PROJECT_DIR"
make build

# --- Step 3: Fresh copy of VDI ---
echo "==> Creating fresh VDI copy"
cp "$VDI_PRISTINE" "$VDI_WORK"

# --- Step 4: Resize VDI ---
echo "==> Resizing VDI to ${DISK_SIZE_MB}MB"
VBoxManage modifymedium disk "$VDI_WORK" --resize "$DISK_SIZE_MB"

# --- Step 5: Mount and inject binary ---
echo "==> Mounting VDI via NBD"
sudo modprobe nbd max_part=16
# Disconnect any stale NBD attachment
sudo qemu-nbd --disconnect "$NBD_DEV" 2>/dev/null || true
sudo qemu-nbd --connect="$NBD_DEV" "$VDI_WORK"
sleep 1

# Ensure NBD is disconnected on failure (mount/inject may fail mid-way)
cleanup_nbd() {
  sudo umount "$MNT" 2>/dev/null || true
  sudo qemu-nbd --disconnect "$NBD_DEV" 2>/dev/null || true
}
trap cleanup_nbd EXIT

# Expected layout: MicroOS btrfs with EFI(p1) + BIOS(p2) + root(p3)
ROOT_PART="${NBD_DEV}p3"
[[ -b "$ROOT_PART" ]] || die "Expected partition $ROOT_PART not found"

sudo mount -o subvol=@ "$ROOT_PART" "$MNT"

# MicroOS read-only root is at .snapshots/1/snapshot/
SNAPSHOT="$MNT/.snapshots/1/snapshot"
[[ -d "$SNAPSHOT/usr/bin" ]] || die "Snapshot directory not found at $SNAPSHOT"

echo "==> Injecting dev binary into snapshot"
sudo btrfs property set "$SNAPSHOT" ro false
sudo cp "$PROJECT_DIR/piccolod" "$SNAPSHOT/usr/bin/piccolod"
sudo btrfs property set "$SNAPSHOT" ro true

echo "==> Verifying"
ls -lh "$SNAPSHOT/usr/bin/piccolod"

echo "==> Unmounting"
sudo umount "$MNT"
sudo qemu-nbd --disconnect "$NBD_DEV"
trap - EXIT

# --- Step 6: Create and start VM ---
VM_NAME="piccolo-e2e-$(date +%s)"
echo "==> Creating VM: $VM_NAME from template: $TEMPLATE"

VBoxManage clonevm "$TEMPLATE" --name "$VM_NAME" --register

if ! VBoxManage storageattach "$VM_NAME" \
  --storagectl "SATA" \
  --port 0 --device 0 \
  --type hdd --medium "$VDI_WORK"; then
  echo "Failed to attach disk. Cleaning up."
  VBoxManage unregistervm "$VM_NAME" --delete
  die "Storage attach failed"
fi

VBoxManage startvm "$VM_NAME"
echo "$VM_NAME" > "$VM_STATE_FILE"

# --- Step 7: Wait for IP ---
echo "==> Waiting for VM to boot and acquire IP..."
IP=""
for i in $(seq 1 60); do
  IP=$(VBoxManage guestproperty get "$VM_NAME" "/VirtualBox/GuestInfo/Net/0/V4/IP" 2>/dev/null | grep -oP 'Value: \K.*' || true)
  if [[ -n "$IP" && "$IP" != "0.0.0.0" ]]; then
    break
  fi
  sleep 2
done

if [[ -z "$IP" || "$IP" == "0.0.0.0" ]]; then
  echo "WARN: Could not determine VM IP via guest properties."
  echo "      Check the VirtualBox console window."
  echo "      VM name: $VM_NAME"
  exit 0
fi

echo ""
echo "============================================"
echo "  VM ready: $VM_NAME"
echo "  IP:       $IP"
echo "  Test:     curl http://$IP/version"
echo "  Destroy:  $0 --destroy"
echo "============================================"

# Wait for HTTP to be ready
echo ""
echo "==> Waiting for piccolod HTTP..."
for i in $(seq 1 30); do
  if curl -sf --connect-timeout 2 "http://$IP/version" >/dev/null 2>&1; then
    VERSION=$(curl -s "http://$IP/version")
    echo "  piccolod is up: $VERSION"
    exit 0
  fi
  sleep 2
done

echo "WARN: HTTP not responding after 60s. Check VM console."
echo "      VM name: $VM_NAME"
echo "      IP: $IP"
