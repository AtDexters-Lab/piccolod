# Dev VM Testing Scripts

Scripts for testing piccolod against VirtualBox VMs. Two flows exist for different
stages of development.

## Directory Structure

```
scripts/
  production/           # Sealed MicroOS image — final e2e validation
    dev-vm.sh           # Build binary, inject into VDI, boot VM
    dev-vm-test.sh      # HTTP-only test stages (no SSH)
  alpha/                # Tumbleweed dev VM — storage stack development
    dev-vm-alpha.sh     # VM lifecycle: clone, deploy, SSH, destroy
    dev-vm-alpha-test.sh # HTTP + SSH test stages (storage inspection)
```

## Production Flow (`scripts/production/`)

Uses a sealed Piccolo OS (MicroOS) VDI image. The binary is injected into the
read-only btrfs snapshot via NBD+qemu-nbd. No SSH access — all testing is via
HTTP API.

**When to use:** Final validation before release. Tests the exact production image
with only the binary replaced.

```bash
# First run (decompress VDI)
./scripts/production/dev-vm.sh /path/to/piccolo-os.vdi.xz

# Subsequent runs (reuse cached VDI)
./scripts/production/dev-vm.sh

# Run all test stages
./scripts/production/dev-vm-test.sh <IP> all

# Run specific stage
./scripts/production/dev-vm-test.sh <IP> setup
./scripts/production/dev-vm-test.sh <IP> service-app

# Destroy VM
./scripts/production/dev-vm.sh --destroy
```

**Prerequisites:** VBoxManage, qemu-nbd, qemu-img, a `piccolo-template` VM in VirtualBox.

## Alpha Flow (`scripts/alpha/`)

Uses a Tumbleweed-based dev VM (`piccolo-dev-template`) with the piccolod repo
mounted as a shared folder at `/piccolod`. Has full SSH access for interactive
debugging of the storage stack (LVM, DRBD, NBD, LUKS, overlay).

**When to use:** During development of storage-layer changes. Provides SSH access
for inspecting block devices, mount tables, kernel modules, and DRBD/NBD state.

```bash
# Create fresh VM from template and deploy
./scripts/alpha/dev-vm-alpha.sh fresh

# Deploy code changes (build on host, restart service on VM)
./scripts/alpha/dev-vm-alpha.sh deploy

# SSH into VM for manual inspection
./scripts/alpha/dev-vm-alpha.sh ssh

# Run all test stages
./scripts/alpha/dev-vm-alpha-test.sh <IP> all

# Run storage-specific stages
./scripts/alpha/dev-vm-alpha-test.sh <IP> prereq
./scripts/alpha/dev-vm-alpha-test.sh <IP> storage-inspect
./scripts/alpha/dev-vm-alpha-test.sh <IP> overlay-verify

# View logs
./scripts/alpha/dev-vm-alpha.sh logs

# Destroy and start fresh
./scripts/alpha/dev-vm-alpha.sh fresh
```

### Template VM Setup (`piccolo-dev-template`)

One-time setup for the Tumbleweed template VM:

1. **Create VM** in VirtualBox: openSUSE Tumbleweed, 4GB RAM, 2 vCPUs
2. **Two disks**: 20GB root (sda), 20GB data (sdb) for LVM thin pool testing
3. **SSH**: Enable sshd, add your SSH public key to `/root/.ssh/authorized_keys`
4. **Shared folder**: Host `~/projects/piccolo/piccolod` → guest `/piccolod` (auto-mount)
5. **Guest Additions**: Install for shared folders and IP detection
6. **Install packages**:

```bash
# Build tools
zypper install go make gcc

# Storage stack
zypper install lvm2 thin-provisioning-tools cryptsetup device-mapper

# Block device stack
zypper install drbd-utils nbd
# If drbd kernel module missing:
zypper install drbd-kmp-default

# Container runtime
zypper install podman crun

# Verify kernel modules
modprobe overlay dm-thin-pool nbd drbd
lsmod | grep -E 'overlay|dm_thin|nbd|drbd'
```

7. **Snapshot** the VM (so `clone` creates a clean copy)

### Interactive Debugging Workflow

```bash
# 1. Fresh VM
./scripts/alpha/dev-vm-alpha.sh fresh

# 2. Edit code
vim internal/storage/lvm/pool.go

# 3. Build + deploy (restarts piccolod service)
./scripts/alpha/dev-vm-alpha.sh deploy

# 4. Test specific stage
./scripts/alpha/dev-vm-alpha-test.sh $IP storage-inspect

# 5. SSH for manual inspection
./scripts/alpha/dev-vm-alpha.sh ssh
# On VM:
journalctl -u piccolod -f | grep -v -e Announced -e "PTR record" -e "peer discovery"
lvs piccolo-data-vg
cat /proc/mounts | grep overlay
drbdadm status

# 6. Iterate: edit → deploy → test
./scripts/alpha/dev-vm-alpha.sh deploy
./scripts/alpha/dev-vm-alpha-test.sh $IP all
```
