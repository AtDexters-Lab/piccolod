package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

func discoverAcceleratorDevicePaths() ([]string, error) {
	var paths []string
	for _, directory := range []string{"/dev/dri", "/dev/accel"} {
		entries, err := os.ReadDir(directory)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			candidate := filepath.Join(directory, entry.Name())
			info, err := os.Stat(candidate)
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice != 0 {
				paths = append(paths, candidate)
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func setAcceleratorACL(ctx context.Context, uid uint32, devices []string, grant bool) error {
	for _, device := range devices {
		var args []string
		if grant {
			args = []string{"-m", "u:" + strconv.FormatUint(uint64(uid), 10) + ":rw", device}
		} else {
			args = []string{"-x", "u:" + strconv.FormatUint(uint64(uid), 10), device}
		}
		output, err := exec.CommandContext(ctx, "setfacl", args...).CombinedOutput()
		if err != nil {
			// Revocation is idempotent when the ACL entry or node disappeared.
			lower := strings.ToLower(string(output))
			if !grant && strings.Contains(lower, "no such file") {
				continue
			}
			return fmt.Errorf("setfacl %s for uid %d: %w: %s", device, uid, err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func (m *AppManager) discoverAcceleratorPaths() ([]string, error) {
	if m.acceleratorDiscover != nil {
		return m.acceleratorDiscover()
	}
	return discoverAcceleratorDevicePaths()
}

func (m *AppManager) applyAcceleratorPermission(ctx context.Context, uid uint32, devices []string, grant bool) error {
	if m.acceleratorPermission != nil {
		return m.acceleratorPermission(ctx, uid, devices, grant)
	}
	return setAcceleratorACL(ctx, uid, devices, grant)
}

func (m *AppManager) applyAcceleratorPermissions(
	ctx context.Context,
	uids []uint32,
	devices []string,
	grant bool,
) error {
	for _, uid := range sortedUniqueUint32s(uids) {
		if err := m.applyAcceleratorPermission(ctx, uid, devices, grant); err != nil {
			return err
		}
	}
	return nil
}

func (m *AppManager) selectedAcceleratorProvider(state *FilesystemStateManager) (string, error) {
	durable, err := state.loadCapabilityState()
	if err != nil {
		return "", err
	}
	return durable.Defaults[api.CapabilityAIInferenceOpenAIV1], nil
}

func (m *AppManager) desiredAcceleratorDevices(
	state *FilesystemStateManager,
	instanceID string,
	def *api.AppDefinition,
) ([]string, error) {
	selected, err := m.selectedAcceleratorProvider(state)
	if err != nil {
		return nil, err
	}
	if selected != instanceID {
		return nil, nil
	}
	if _, _, provides := providedCapability(def, api.CapabilityAIInferenceOpenAIV1); !provides {
		return nil, nil
	}
	return m.discoverAcceleratorPaths()
}

func (m *AppManager) ensureAcceleratorAccess(
	ctx context.Context,
	state *FilesystemStateManager,
	instanceID string,
	runtimeConfig container.PodmanRuntime,
	def *api.AppDefinition,
	hostUIDs []uint32,
) ([]string, error) {
	if runtimeConfig.Credential == nil {
		return nil, fmt.Errorf("accelerator provider %s has no rootless runtime credential", instanceID)
	}
	devices, err := m.desiredAcceleratorDevices(state, instanceID, def)
	if err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		return nil, nil
	}
	hostUIDs = sortedUniqueUint32s(hostUIDs)
	if len(hostUIDs) == 0 {
		return nil, fmt.Errorf("accelerator provider %s has no effective host UIDs", instanceID)
	}
	for _, uid := range hostUIDs {
		if uid == 0 {
			return nil, fmt.Errorf("accelerator provider %s resolved an invalid host UID", instanceID)
		}
	}
	durable, err := state.loadCapabilityState()
	if err != nil {
		return nil, err
	}
	if grant := durable.AcceleratorGrant; grant != nil {
		if grant.Owner != instanceID {
			return nil, fmt.Errorf(
				"accelerator grant for %s is blocked by persisted ownership on %s",
				instanceID,
				grant.Owner,
			)
		}
		if !slices.Equal(grant.UIDs, hostUIDs) || !slices.Equal(grant.Devices, devices) {
			if err := m.revokeAcceleratorAccess(ctx, state, instanceID); err != nil {
				return nil, fmt.Errorf("withdraw stale accelerator grant for %s: %w", instanceID, err)
			}
			durable, err = state.loadCapabilityState()
			if err != nil {
				return nil, err
			}
		} else {
			if err := m.applyAcceleratorPermissions(ctx, grant.UIDs, grant.Devices, true); err != nil {
				return nil, fmt.Errorf("reapply persisted accelerator grant for %s: %w", instanceID, err)
			}
			return append([]string(nil), grant.Devices...), nil
		}
	}
	for _, app := range state.ListApps() {
		if app == nil || app.InstanceID == instanceID || len(app.AcceleratorDevices) == 0 {
			continue
		}
		return nil, fmt.Errorf(
			"accelerator grant for %s is blocked by unreconciled ownership on %s",
			instanceID,
			app.InstanceID,
		)
	}
	durable.AcceleratorGrant = &acceleratorGrantRecord{
		Owner:   instanceID,
		UIDs:    append([]uint32(nil), hostUIDs...),
		Devices: append([]string(nil), devices...),
	}
	if err := state.storeCapabilityState(durable); err != nil {
		return nil, fmt.Errorf("persist accelerator grant intent for %s: %w", instanceID, err)
	}
	if err := m.applyAcceleratorPermissions(ctx, hostUIDs, devices, true); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupBudget)
		defer cancel()
		if cleanupErr := m.applyAcceleratorPermissions(cleanupCtx, hostUIDs, devices, false); cleanupErr != nil {
			return nil, errors.Join(
				err,
				fmt.Errorf("retain persisted accelerator fence after failed compensation: %w", cleanupErr),
			)
		}
		durable.AcceleratorGrant = nil
		if clearErr := state.storeCapabilityState(durable); clearErr != nil {
			return nil, errors.Join(
				err,
				fmt.Errorf("accelerator compensation succeeded but persisted fence cleanup failed: %w", clearErr),
			)
		}
		return nil, err
	}
	return devices, nil
}

func acceleratorGenerationMatches(app *AppInstance, desired []string) bool {
	if app == nil {
		return len(desired) == 0
	}
	return slices.Equal(app.AcceleratorDevices, desired)
}

func (m *AppManager) revokeAcceleratorAccess(
	ctx context.Context,
	state *FilesystemStateManager,
	instanceID string,
) error {
	if state == nil || strings.TrimSpace(instanceID) == "" {
		return nil
	}
	durable, err := state.loadCapabilityState()
	if err != nil {
		return err
	}
	grant := durable.AcceleratorGrant
	if grant == nil {
		// Upgrade fallback for an older committed generation that predates the
		// durable accelerator fence. Ordinary apps have no accelerator state
		// and make revocation an idempotent no-op.
		app, ok := state.GetApp(instanceID)
		if !ok || app == nil || len(app.AcceleratorDevices) == 0 {
			return nil
		}
		user, err := container.ResolveAppUser(instanceID)
		if err != nil {
			return fmt.Errorf("resolve accelerator owner %s: %w", instanceID, err)
		}
		return m.applyAcceleratorPermission(
			ctx,
			user.Credential.Uid,
			app.AcceleratorDevices,
			false,
		)
	}
	if grant.Owner != instanceID {
		return nil
	}
	if err := m.applyAcceleratorPermissions(ctx, grant.UIDs, grant.Devices, false); err != nil {
		return err
	}
	durable.AcceleratorGrant = nil
	if err := state.storeCapabilityState(durable); err != nil {
		return fmt.Errorf("persist accelerator grant withdrawal for %s: %w", instanceID, err)
	}
	return nil
}

func (m *AppManager) reconcilePersistedAcceleratorGrant(
	ctx context.Context,
	state *FilesystemStateManager,
	alreadyRemoved map[string]struct{},
) error {
	durable, err := state.loadCapabilityState()
	if err != nil {
		return err
	}
	grant := durable.AcceleratorGrant
	if grant == nil || grant.Owner == durable.Defaults[api.CapabilityAIInferenceOpenAIV1] {
		return nil
	}
	if _, removed := alreadyRemoved[grant.Owner]; removed {
		return m.revokeAcceleratorAccess(ctx, state, grant.Owner)
	}
	if owner, ok := state.GetApp(grant.Owner); ok && owner != nil {
		return m.recreateAppForCapabilityEffects(ctx, state, grant.Owner, func() error {
			return m.revokeAcceleratorAccess(ctx, state, grant.Owner)
		})
	}
	// Missing app metadata is not process-absence proof. Quiesce the per-app
	// user boundary before withdrawing the persisted authority.
	if err := m.quiesceAppUserSession(ctx, grant.Owner); err != nil {
		return fmt.Errorf(
			"quiesce uncommitted accelerator owner %s before grant withdrawal: %w",
			grant.Owner,
			err,
		)
	}
	return m.revokeAcceleratorAccess(ctx, state, grant.Owner)
}

func acceleratorHostUIDs(
	instanceID string,
	runtimeConfig container.PodmanRuntime,
	def *api.AppDefinition,
	rootfs map[string]*rootfsMountInfo,
) ([]uint32, error) {
	if runtimeConfig.Credential == nil {
		return nil, fmt.Errorf("accelerator provider %s has no rootless runtime credential", instanceID)
	}
	if def == nil || len(def.Services) == 0 {
		return nil, fmt.Errorf("accelerator provider %s has no services", instanceID)
	}
	subUIDStart, subUIDCount, err := container.LookupSubUIDRange(container.AppUsername(instanceID))
	if err != nil {
		return nil, fmt.Errorf("resolve accelerator subordinate UID range: %w", err)
	}
	serviceNames := make([]string, 0, len(def.Services))
	for serviceName := range def.Services {
		serviceNames = append(serviceNames, serviceName)
	}
	sort.Strings(serviceNames)
	// The outer runtime UID must be able to configure the bind-mounted device;
	// service processes may then execute as subordinate mapped image users.
	hostUIDs := make([]uint32, 0, len(serviceNames)+1)
	hostUIDs = append(hostUIDs, runtimeConfig.Credential.Uid)
	for _, serviceName := range serviceNames {
		info := rootfs[serviceName]
		if info == nil || strings.TrimSpace(info.handle.MountPath) == "" {
			return nil, fmt.Errorf("resolve accelerator user for service %s: rootfs is unavailable", serviceName)
		}
		containerUID, err := imageUserUID(info.handle.MountPath, info.imgConfig.User)
		if err != nil {
			return nil, fmt.Errorf("resolve accelerator user for service %s: %w", serviceName, err)
		}
		mapped, err := mapContainerUIDToHost(
			runtimeConfig.Credential.Uid,
			subUIDStart,
			subUIDCount,
			containerUID,
		)
		if err != nil {
			return nil, fmt.Errorf("resolve accelerator user for service %s: %w", serviceName, err)
		}
		hostUIDs = append(hostUIDs, mapped)
	}
	return sortedUniqueUint32s(hostUIDs), nil
}

func mapContainerUIDToHost(appUID, subUIDStart, subUIDCount, containerUID uint32) (uint32, error) {
	if containerUID == 0 {
		if appUID == 0 {
			return 0, fmt.Errorf("rootless app UID is invalid")
		}
		return appUID, nil
	}
	if containerUID > subUIDCount {
		return 0, fmt.Errorf("container UID %d exceeds mapped range", containerUID)
	}
	mapped := uint64(subUIDStart) + uint64(containerUID) - 1
	if mapped > uint64(^uint32(0)) {
		return 0, fmt.Errorf("mapped UID overflows")
	}
	return uint32(mapped), nil
}

func (m *AppManager) desiredAcceleratorHostUIDs(
	state *FilesystemStateManager,
	instanceID string,
	runtimeConfig container.PodmanRuntime,
	def *api.AppDefinition,
	rootfs map[string]*rootfsMountInfo,
) ([]uint32, error) {
	devices, err := m.desiredAcceleratorDevices(state, instanceID, def)
	if err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		return nil, nil
	}
	return acceleratorHostUIDs(instanceID, runtimeConfig, def, rootfs)
}

func imageUserUID(rootfsPath, userSpec string) (uint32, error) {
	user := strings.TrimSpace(strings.SplitN(userSpec, ":", 2)[0])
	if user == "" {
		return 0, nil
	}
	if numeric, err := strconv.ParseUint(user, 10, 32); err == nil {
		return uint32(numeric), nil
	}

	root, err := os.OpenRoot(rootfsPath)
	if err != nil {
		return 0, fmt.Errorf("open rootfs: %w", err)
	}
	defer root.Close()
	passwd, err := root.Open("etc/passwd")
	if err != nil {
		return 0, fmt.Errorf("open confined /etc/passwd: %w", err)
	}
	defer passwd.Close()
	scanner := bufio.NewScanner(io.LimitReader(passwd, 1<<20))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < 3 || fields[0] != user {
			continue
		}
		uid, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("parse UID for image user %q: %w", user, err)
		}
		return uint32(uid), nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read confined /etc/passwd: %w", err)
	}
	return 0, fmt.Errorf("image user %q is absent from /etc/passwd", user)
}
