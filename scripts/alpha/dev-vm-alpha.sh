#!/usr/bin/env bash
# dev-vm-alpha.sh — Manage a Tumbleweed-based dev VM for block-native storage testing.
#
# Uses a VirtualBox template VM (piccolo-dev-template) with:
#   - SSH access (root, key-based auth)
#   - Shared folder: host piccolod repo → guest /piccolod
#   - Required packages: lvm2, drbd-utils, nbd, cryptsetup, podman
#
# Usage:
#   ./scripts/alpha/dev-vm-alpha.sh clone [name]     # Clone template → new VM
#   ./scripts/alpha/dev-vm-alpha.sh start [name]     # Start VM, wait for SSH
#   ./scripts/alpha/dev-vm-alpha.sh ssh [name]       # SSH into the VM
#   ./scripts/alpha/dev-vm-alpha.sh deploy [name]    # Build on host, restart service on VM
#   ./scripts/alpha/dev-vm-alpha.sh destroy [name]   # Power off + delete VM
#   ./scripts/alpha/dev-vm-alpha.sh fresh [name]     # destroy + clone + start + deploy
#   ./scripts/alpha/dev-vm-alpha.sh netlab [name]    # Attach extra NICs for multi-interface tests
#   ./scripts/alpha/dev-vm-alpha.sh ip [name]        # Print VM IP
#   ./scripts/alpha/dev-vm-alpha.sh logs [name]      # Tail piccolod journal on VM
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
USER_OOM_POLICY="$PROJECT_DIR/../piccolo-os/packages/piccolo-os-support/piccolo-user-session-oom.conf"
WORK_DIR="/tmp/claude/piccolo-alpha"
VM_STATE_FILE="$WORK_DIR/alpha-vm-name"
VM_IP_FILE="$WORK_DIR/alpha-vm-ip"
TEMPLATE="piccolod-dev-alpha-template"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)

CMD="${1:-help}"
VM_NAME="${2:-}"

die() { echo "ERROR: $*" >&2; exit 1; }

mkdir -p "$WORK_DIR"

# Resolve VM name: argument > state file > default
resolve_vm() {
  if [[ -n "$VM_NAME" ]]; then
    echo "$VM_NAME"
  elif [[ -f "$VM_STATE_FILE" ]]; then
    cat "$VM_STATE_FILE"
  else
    echo ""
  fi
}

# Get VM IP via VirtualBox guest properties, cache it.
get_ip() {
  local vm="$1"
  local ip=""
  ip=$(timeout 5s VBoxManage guestproperty get "$vm" "/VirtualBox/GuestInfo/Net/0/V4/IP" 2>/dev/null \
    | grep -oP 'Value: \K.*' || true)
  if [[ -n "$ip" && "$ip" != "0.0.0.0" ]]; then
    echo "$ip" > "$VM_IP_FILE"
    echo "$ip"
  elif [[ -f "$VM_IP_FILE" ]]; then
    cat "$VM_IP_FILE"
  fi
}

# Wait for SSH to become reachable.
wait_ssh() {
  local ip="$1"
  echo "==> Waiting for SSH on $ip..."
  for i in $(seq 1 30); do
    if ssh "${SSH_OPTS[@]}" -o ConnectTimeout=2 "root@$ip" true 2>/dev/null; then
      echo "  SSH ready after ~$((i*2))s"
      return 0
    fi
    sleep 2
  done
  echo "WARN: SSH not ready after 60s"
  return 1
}

# ─── Commands ───────────────────────────────────────────

cmd_clone() {
  local name="${VM_NAME:-piccolo-dev-$(date +%s)}"

  # Destroy existing if any
  if [[ -f "$VM_STATE_FILE" ]]; then
    local old
    old=$(cat "$VM_STATE_FILE")
    echo "==> Destroying existing VM: $old"
    VBoxManage controlvm "$old" poweroff 2>/dev/null || true
    sleep 2
    VBoxManage unregistervm "$old" --delete 2>/dev/null || true
    rm -f "$VM_STATE_FILE" "$VM_IP_FILE"
  fi

  echo "==> Cloning $TEMPLATE → $name"
  VBoxManage clonevm "$TEMPLATE" --name "$name" --register
  echo "$name" > "$VM_STATE_FILE"
  echo "  VM created: $name"
}

cmd_start() {
  local vm
  vm=$(resolve_vm)
  [[ -n "$vm" ]] || die "No VM name. Run 'clone' first or pass a name."

  echo "==> Starting VM: $vm"
  VBoxManage startvm "$vm" --type headless 2>/dev/null || VBoxManage startvm "$vm"

  echo "==> Waiting for IP..."
  local ip=""
  for i in $(seq 1 60); do
    ip=$(get_ip "$vm")
    if [[ -n "$ip" ]]; then
      echo "  IP: $ip"
      break
    fi
    sleep 2
  done
  [[ -n "$ip" ]] || die "Could not determine VM IP"

  wait_ssh "$ip" || true

  echo ""
  echo "============================================"
  echo "  VM ready: $vm"
  echo "  IP:       $ip"
  echo "  SSH:      ssh ${SSH_OPTS[*]} root@$ip"
  echo "============================================"
}

cmd_ssh() {
  local vm
  vm=$(resolve_vm)
  [[ -n "$vm" ]] || die "No VM. Run 'clone' first."
  local ip
  ip=$(get_ip "$vm")
  [[ -n "$ip" ]] || die "No IP for VM $vm"
  exec ssh "${SSH_OPTS[@]}" "root@$ip"
}

cmd_deploy() {
  local vm
  vm=$(resolve_vm)
  [[ -n "$vm" ]] || die "No VM. Run 'clone' first."
  local ip
  ip=$(get_ip "$vm")
  [[ -n "$ip" ]] || die "No IP for VM $vm"

  echo "==> Building piccolod on host"
  cd "$PROJECT_DIR"
  make build

  [[ -f "$USER_OOM_POLICY" ]] || die "Piccolo OS user-session OOM policy not found: $USER_OOM_POLICY"
  echo "==> Installing alpha host OOM policy"
  scp "${SSH_OPTS[@]}" "$USER_OOM_POLICY" "root@$ip:/tmp/piccolo-user-session-oom.conf"
  ssh "${SSH_OPTS[@]}" "root@$ip" \
    "install -d -m 0755 /etc/systemd/system/user@.service.d && \
     install -m 0644 /tmp/piccolo-user-session-oom.conf /etc/systemd/system/user@.service.d/90-piccolo-oom.conf && \
     rm -f /tmp/piccolo-user-session-oom.conf && \
     systemctl daemon-reload"

  echo "==> Deploying to VM ($ip)"
  ssh "${SSH_OPTS[@]}" "root@$ip" "cd /piccolod && sudo RUN_PORT=80 make service"

  echo "==> Waiting for HTTP..."
  for i in $(seq 1 30); do
    if curl -sf --connect-timeout 2 "http://$ip/version" >/dev/null 2>&1; then
      local ver
      ver=$(curl -s "http://$ip/version")
      echo "  piccolod is up: $ver"
      return 0
    fi
    sleep 2
  done
  echo "WARN: HTTP not responding after 60s"
}

cmd_destroy() {
  local vm
  vm=$(resolve_vm)
  if [[ -z "$vm" ]]; then
    echo "No active VM found."
    return
  fi
  echo "==> Destroying VM: $vm"
  VBoxManage controlvm "$vm" poweroff 2>/dev/null || true
  sleep 2
  VBoxManage unregistervm "$vm" --delete 2>/dev/null || true
  rm -f "$VM_STATE_FILE" "$VM_IP_FILE"
  echo "  VM destroyed."
}

cmd_fresh() {
  cmd_clone
  if [[ "${PICCOLO_ALPHA_NETLAB:-0}" == "1" ]]; then
    cmd_netlab
  fi
  cmd_start
  cmd_deploy
}

cmd_netlab() {
  local vm
  vm=$(resolve_vm)
  [[ -n "$vm" ]] || die "No VM. Run 'clone' first."

  local running
  running=$(VBoxManage showvminfo "$vm" --machinereadable 2>/dev/null | grep -oP '^VMState="\K[^"]+' || true)
  if [[ "$running" == "running" ]]; then
    echo "==> Attaching runtime network lab NICs to $vm"
    VBoxManage controlvm "$vm" nic2 nat
    VBoxManage controlvm "$vm" cableconnected2 on
    VBoxManage controlvm "$vm" nic3 intnet "piccolo-alpha-lanonly"
    VBoxManage controlvm "$vm" cableconnected3 on
  else
    echo "==> Configuring network lab NICs on $vm"
    VBoxManage modifyvm "$vm" --nic2 nat --cableconnected2 on
    VBoxManage modifyvm "$vm" --nic3 intnet --intnet3 "piccolo-alpha-lanonly" --cableconnected3 on
  fi

  echo "  nic2: NAT (WAN-capable test uplink)"
  echo "  nic3: internal network piccolo-alpha-lanonly (LAN-only static profile in test stage)"
}

cmd_ip() {
  local vm
  vm=$(resolve_vm)
  [[ -n "$vm" ]] || die "No VM."
  get_ip "$vm"
}

cmd_logs() {
  local vm
  vm=$(resolve_vm)
  [[ -n "$vm" ]] || die "No VM."
  local ip
  ip=$(get_ip "$vm")
  [[ -n "$ip" ]] || die "No IP for VM $vm"
  # Filter mDNS noise
  ssh "${SSH_OPTS[@]}" "root@$ip" \
    "journalctl -u piccolod -f --no-pager" \
    | grep -v -e "Announced" -e "PTR record" -e "peer discovery" -e "non-local query" -e "query from" -e "self-response"
}

# ─── Dispatch ───────────────────────────────────────────

case "$CMD" in
  clone)   cmd_clone ;;
  start)   cmd_start ;;
  ssh)     cmd_ssh ;;
  deploy)  cmd_deploy ;;
  destroy) cmd_destroy ;;
  fresh)   cmd_fresh ;;
  netlab)  cmd_netlab ;;
  ip)      cmd_ip ;;
  logs)    cmd_logs ;;
  help|*)
    echo "Usage: $0 <command> [vm-name]"
    echo ""
    echo "Commands:"
    echo "  clone [name]   Clone piccolo-dev-template to a new test VM"
    echo "  start [name]   Start the VM and wait for SSH"
    echo "  ssh [name]     SSH into the VM"
    echo "  deploy [name]  Build on host, restart service on VM"
    echo "  destroy [name] Power off and delete the VM"
    echo "  fresh [name]   destroy + clone + start + deploy"
    echo "  netlab [name]  Attach extra NICs for multi-interface tests"
    echo "  ip [name]      Print VM IP"
    echo "  logs [name]    Tail piccolod journal (filtered)"
    ;;
esac
