package fsutil

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// IDMapConfig specifies the UID/GID mapping for an idmapped mount.
// The mapping creates two ranges:
//   - 0 → AppUID/AppGID (1:1 mapping for the container root user)
//   - 1..SubUIDCount → SubUIDStart..SubUIDStart+SubUIDCount (bulk mapping)
type IDMapConfig struct {
	AppUID      uint32
	AppGID      uint32
	SubUIDStart uint32
	SubUIDCount uint32
	SubGIDStart uint32
	SubGIDCount uint32
}

// CreateIDMappedMount creates an idmapped bind mount of source at target.
// The mount translates on-disk UIDs/GIDs according to the IDMapConfig,
// allowing containers to access files owned by different UIDs.
//
// Requires CAP_SYS_ADMIN and kernel >= 5.15 (btrfs idmap support).
func CreateIDMappedMount(source, target string, config IDMapConfig) error {
	usernsFd, err := createUserNamespace(config)
	if err != nil {
		return fmt.Errorf("create user namespace: %w", err)
	}
	defer syscall.Close(usernsFd)

	// Clone the mount tree at source.
	treeFd, err := unix.OpenTree(-1, source,
		unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_RECURSIVE)
	if err != nil {
		return fmt.Errorf("open_tree %s: %w", source, err)
	}
	defer syscall.Close(treeFd)

	// Apply the idmap from our user namespace.
	attr := unix.MountAttr{
		Attr_set:  unix.MOUNT_ATTR_IDMAP,
		Userns_fd: uint64(usernsFd),
	}
	if err := unix.MountSetattr(treeFd, "", unix.AT_EMPTY_PATH|unix.AT_RECURSIVE, &attr); err != nil {
		return fmt.Errorf("mount_setattr idmap: %w", err)
	}

	// Ensure target directory exists.
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("mkdir target %s: %w", target, err)
	}

	// Move the idmapped tree to the target path.
	if err := unix.MoveMount(treeFd, "", -1, target,
		unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount to %s: %w", target, err)
	}

	return nil
}

// createUserNamespace creates a child process in a new user namespace
// with the UID/GID mappings from config, and returns an fd to the
// namespace. The child exits after the parent obtains the fd.
func createUserNamespace(config IDMapConfig) (int, error) {
	// Create pipe using raw syscall to get integer fds (not os.File).
	// The child process after clone3 must NOT use Go runtime (allocator,
	// scheduler, GC) — only raw syscalls are safe in the forked child.
	var pipeFds [2]int
	if err := syscall.Pipe2(pipeFds[:], syscall.O_CLOEXEC); err != nil {
		return -1, fmt.Errorf("pipe2: %w", err)
	}
	readFd := pipeFds[0]
	writeFd := pipeFds[1]

	// clone3 with CLONE_NEWUSER.
	type cloneArgs struct {
		flags      uint64
		pidfd      uint64
		childTID   uint64
		parentTID  uint64
		exitSignal uint64
		stack      uint64
		stackSize  uint64
		tls        uint64
	}

	args := cloneArgs{
		flags: unix.CLONE_NEWUSER,
	}

	// Lock the OS thread — clone3 must be called from the same OS thread
	// that will inspect /proc/<child>/ns/user.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	childPid, _, errno := syscall.RawSyscall(
		unix.SYS_CLONE3,
		uintptr(unsafe.Pointer(&args)),
		unsafe.Sizeof(args),
		0,
	)

	if errno != 0 {
		syscall.Close(readFd)
		syscall.Close(writeFd)
		return -1, fmt.Errorf("clone3(CLONE_NEWUSER): %w", errno)
	}

	if childPid == 0 {
		// Child: ONLY RawSyscall allowed — syscall.Close/Read/Exit use
		// Syscall() which calls entersyscall()/exitsyscall() and can
		// deadlock in the cloned child where the Go runtime is inconsistent.
		syscall.RawSyscall(syscall.SYS_CLOSE, uintptr(writeFd), 0, 0)
		var buf [1]byte
		syscall.RawSyscall(syscall.SYS_READ, uintptr(readFd), uintptr(unsafe.Pointer(&buf[0])), 1) //nolint:errcheck
		syscall.RawSyscall(syscall.SYS_CLOSE, uintptr(readFd), 0, 0)
		syscall.RawSyscall(syscall.SYS_EXIT, 0, 0, 0)
	}

	// Parent: close read end, keep write end to signal child later.
	syscall.Close(readFd)

	// reapChild signals the child to exit and waits to avoid zombies.
	// Must be called on every error path after clone3 succeeds.
	reapChild := func() {
		syscall.Close(writeFd)
		var wstatus syscall.WaitStatus
		syscall.Wait4(int(childPid), &wstatus, 0, nil) //nolint:errcheck
	}

	// Write UID map. Only include the subordinate range if SubUIDCount > 0.
	uidMap := fmt.Sprintf("0 %d 1\n", config.AppUID)
	if config.SubUIDCount > 0 {
		uidMap += fmt.Sprintf("1 %d %d\n", config.SubUIDStart, config.SubUIDCount)
	}
	if err := writeProc(int(childPid), "uid_map", uidMap); err != nil {
		reapChild()
		return -1, fmt.Errorf("write uid_map: %w", err)
	}

	// Deny setgroups before writing gid_map (required by kernel).
	if err := writeProc(int(childPid), "setgroups", "deny"); err != nil {
		reapChild()
		return -1, fmt.Errorf("write setgroups: %w", err)
	}

	// Write GID map. Only include the subordinate range if SubGIDCount > 0.
	gidMap := fmt.Sprintf("0 %d 1\n", config.AppGID)
	if config.SubGIDCount > 0 {
		gidMap += fmt.Sprintf("1 %d %d\n", config.SubGIDStart, config.SubGIDCount)
	}
	if err := writeProc(int(childPid), "gid_map", gidMap); err != nil {
		reapChild()
		return -1, fmt.Errorf("write gid_map: %w", err)
	}

	// Open the child's user namespace fd.
	nsPath := fmt.Sprintf("/proc/%d/ns/user", childPid)
	nsFd, err := syscall.Open(nsPath, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		reapChild()
		return -1, fmt.Errorf("open %s: %w", nsPath, err)
	}

	// Signal child to exit and reap.
	syscall.Close(writeFd)
	var wstatus syscall.WaitStatus
	syscall.Wait4(int(childPid), &wstatus, 0, nil) //nolint:errcheck

	return nsFd, nil
}

// writeProc writes content to /proc/<pid>/<file>.
func writeProc(pid int, file, content string) error {
	path := "/proc/" + strconv.Itoa(pid) + "/" + file
	return os.WriteFile(path, []byte(content), 0o644)
}
