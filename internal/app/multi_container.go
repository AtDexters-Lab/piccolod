package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/fsutil"
	"piccolod/internal/persistence"
)

const (
	networkAnchorServiceName = "__netns__"
)

const defaultNetworkAnchorImage = "registry.k8s.io/pause:3.9"

func networkAnchorImage() string {
	if raw := strings.TrimSpace(os.Getenv("PICCOLO_NETNS_IMAGE")); raw != "" {
		return raw
	}
	return defaultNetworkAnchorImage
}

func networkAnchorContainerName(instanceID string) string {
	return fmt.Sprintf("%s__netns__", instanceID)
}

func containerNameForService(instanceID, serviceName, primaryService string) string {
	if strings.TrimSpace(serviceName) == "" {
		return instanceID
	}
	if serviceName == primaryService {
		return instanceID
	}
	return fmt.Sprintf("%s__%s", instanceID, serviceName)
}

// selinuxDisableLabel returns the security option to disable SELinux labeling.
// Rootless podman in user namespaces lacks CAP_MAC_ADMIN in the initial namespace,
// so the overlay mount's context= option is silently ignored. Files retain their
// host SELinux labels (container_var_lib_t or unlabeled_t on device-mapper),
// which container_t is not allowed to read. Disabling labeling avoids these denials.
func selinuxDisableLabel() []string {
	return []string{"label=disable"}
}

func primaryServiceFor(def *api.AppDefinition, inst *AppInstance) string {
	if inst != nil && strings.TrimSpace(inst.PrimaryService) != "" {
		return inst.PrimaryService
	}
	if def != nil && strings.TrimSpace(def.PrimaryService) != "" {
		return strings.TrimSpace(def.PrimaryService)
	}
	if def != nil && def.Services != nil {
		if piccoloModeFromExtensions(def.Extensions) == ModeWorkspace && len(def.Services) == 1 {
			for name := range def.Services {
				return name
			}
		}
		return defaultPrimaryServiceName
	}
	return ""
}

func piccoloLabels(instanceID, serviceName, role string) map[string]string {
	return map[string]string{
		"io.piccolo.instance": instanceID,
		"io.piccolo.service":  serviceName,
		"io.piccolo.role":     role,
	}
}

func appRestartPolicy(def *api.AppDefinition) string {
	// Always return "no" - piccolod is the sole lifecycle manager for all containers.
	// We don't want Podman to auto-restart containers outside of piccolod's control.
	return "no"
}

func appNetworkMode(def *api.AppDefinition) string {
	if def != nil && def.Permissions != nil && def.Permissions.Network != nil {
		if def.Permissions.Network.Internet == "deny" {
			return "none"
		}
	}
	return ""
}

// serviceContainerOptions holds all parameters for building a service container spec.
// The rootfs fields are optional and used for both workspace and service mode apps.
type serviceContainerOptions struct {
	layout     appVolumeLayout
	appDef     *api.AppDefinition
	instanceID string
	primary    string
	svcName    string
	anchorID   string

	// Per-app credential for rootless isolation. Always non-nil — volume dirs
	// are chowned to this UID/GID and the :U mount option is added for
	// Podman UID remapping.
	credential *syscall.Credential

	// Block-native rootfs fields (optional, used when rootfsMgr is available)
	rootfsHandle    *persistence.RootfsHandle    // Mounted rootfs from RootfsVolumeManager
	goldenImgConfig *persistence.GoldenImageConfig // Image config from golden LV
}

// buildOriginalCmdFromSlices reconstructs the original command from entrypoint and cmd slices.
func buildOriginalCmdFromSlices(entrypoint, cmd []string) []string {
	var result []string
	result = append(result, entrypoint...)
	result = append(result, cmd...)
	if len(result) == 0 {
		result = []string{"/bin/sh"}
	}
	return result
}

// buildServiceContainerSpec builds a container spec for a service container.
// In rootfs mode (rootfsHandle + goldenImgConfig), the entrypoint strategy depends
// on x-piccolo.mode: workspace gets boot.sh wrapping, service/unknown gets the
// image's native entrypoint with --init, and init:image delegates to the image's
// init system. Without rootfs fields, the service image is used directly.
func (m *AppManager) buildServiceContainerSpec(opts serviceContainerOptions) (container.ContainerCreateSpec, error) {
	if opts.appDef == nil || opts.appDef.Services == nil {
		return container.ContainerCreateSpec{}, fmt.Errorf("service container spec requires app definition services")
	}
	svc, ok := opts.appDef.Services[opts.svcName]
	if !ok {
		return container.ContainerCreateSpec{}, fmt.Errorf("unknown service '%s'", opts.svcName)
	}
	if strings.TrimSpace(opts.anchorID) == "" {
		return container.ContainerCreateSpec{}, fmt.Errorf("service container spec requires network anchor id")
	}

	spec := container.ContainerCreateSpec{
		Name:          containerNameForService(opts.instanceID, opts.svcName, opts.primary),
		Image:         svc.Image,
		Environment:   svc.Environment,
		NetworkMode:   fmt.Sprintf("container:%s", opts.anchorID),
		RestartPolicy: appRestartPolicy(opts.appDef),
		Labels:        piccoloLabels(opts.instanceID, opts.svcName, "service"),
		SecurityOpt:   selinuxDisableLabel(), // overlay context= ignored in user namespaces
	}

	// Apply block-native rootfs configuration (golden LV path) if provided
	if opts.rootfsHandle != nil && opts.goldenImgConfig != nil {
		spec.Rootfs = opts.rootfsHandle.MountPath
		spec.RootfsOverlay = opts.rootfsHandle.ReadOnly
		// Don't set ReadOnly — the :O overlay upper layer must be writable.
		// The underlying btrfs mount is already read-only; writes go to the ephemeral overlay.
		spec.Image = ""

		// Apply image config since Podman doesn't do it in --rootfs mode.
		spec.Environment = mergeEnvMaps(parseEnvSlice(opts.goldenImgConfig.Env), spec.Environment)
		spec.WorkingDir = opts.goldenImgConfig.WorkingDir
		spec.User = opts.goldenImgConfig.User

		mode := piccoloModeFromExtensions(opts.appDef.Extensions)

		if svc.Init == "image" {
			// Workspace-only opt-out: image manages its own init (e.g., s6-overlay).
			// UseInit intentionally false — image's init system needs to be PID 1.
			// Parser validation in validateContainerModel prevents this for service mode.
			spec.Entrypoint = opts.goldenImgConfig.Entrypoint
			spec.Command = opts.goldenImgConfig.Cmd
		} else if mode == ModeWorkspace {
			// Workspace default: boot.sh wrapper + catatonit for keep-alive and hooks.
			originalCmd := buildOriginalCmdFromSlices(opts.goldenImgConfig.Entrypoint, opts.goldenImgConfig.Cmd)
			spec.Entrypoint = []string{"/bin/sh", "/piccolo/boot.sh"}
			spec.Command = originalCmd
			spec.UseInit = true

			if err := EnsureWorkspaceAssets(); err != nil {
				return container.ContainerCreateSpec{}, fmt.Errorf("failed to ensure workspace assets: %w", err)
			}

			assetDir := filepath.Join(opts.layout.DataDir, "piccolo-assets")
			if err := os.MkdirAll(assetDir, 0o755); err != nil {
				return container.ContainerCreateSpec{}, fmt.Errorf("failed to create per-app assets dir: %w", err)
			}
			if err := container.ChownIfNeeded(assetDir, int(opts.credential.Uid), int(opts.credential.Gid)); err != nil {
				return container.ContainerCreateSpec{}, fmt.Errorf("failed to chown per-app assets dir: %w", err)
			}
			if err := copyFileWithOwner(BootShHostPath(), filepath.Join(assetDir, "boot.sh"), int(opts.credential.Uid), int(opts.credential.Gid)); err != nil {
				return container.ContainerCreateSpec{}, fmt.Errorf("failed to copy boot.sh to per-app dir: %w", err)
			}
			if err := copyFileWithOwner(PiccoloStartupHostPath(), filepath.Join(assetDir, "piccolo-startup"), int(opts.credential.Uid), int(opts.credential.Gid)); err != nil {
				return container.ContainerCreateSpec{}, fmt.Errorf("failed to copy piccolo-startup to per-app dir: %w", err)
			}
			bootShHost := filepath.Join(assetDir, "boot.sh")
			startupHost := filepath.Join(assetDir, "piccolo-startup")

			spec.Volumes = append(spec.Volumes, container.VolumeMapping{
				Host: bootShHost, Container: "/piccolo/boot.sh", Options: "ro",
			})
			spec.Volumes = append(spec.Volumes, container.VolumeMapping{
				Host: startupHost, Container: "/usr/local/bin/piccolo-startup", Options: "ro",
			})
		} else {
			// Service mode (+ ModeUnknown as defensive default — boot.sh keep-alive
			// should only apply when mode is explicitly workspace).
			// Image's native entrypoint with --init for zombie reaping.
			spec.Entrypoint = opts.goldenImgConfig.Entrypoint
			spec.Command = opts.goldenImgConfig.Cmd
			spec.UseInit = true
		}

		configDir := filepath.Join(opts.layout.DataDir, "piccolo-config")
		if err := os.MkdirAll(configDir, 0o755); err != nil {
			return container.ContainerCreateSpec{}, fmt.Errorf("failed to create piccolo config dir: %w", err)
		}
		if err := container.ChownIfNeeded(configDir, int(opts.credential.Uid), int(opts.credential.Gid)); err != nil {
			return container.ContainerCreateSpec{}, fmt.Errorf("failed to chown piccolo config dir: %w", err)
		}
		spec.Volumes = append(spec.Volumes, container.VolumeMapping{
			Host: configDir, Container: "/piccolo/config", Options: "rw,U",
		})

		if err := writeAppConfig(configDir, opts.appDef.AppConfig, int(opts.credential.Uid), int(opts.credential.Gid)); err != nil {
			return container.ContainerCreateSpec{}, err
		}

		// Write init script file to config dir if fetched from store.
		if svc.InitScript != nil && len(svc.InitScript.FileContent) > 0 {
			if err := writeInitScript(configDir, svc.InitScript.File, svc.InitScript.FileContent,
				int(opts.credential.Uid), int(opts.credential.Gid)); err != nil {
				return container.ContainerCreateSpec{}, err
			}
		}
	}

	if svc.Resources != nil && svc.Resources.Limits != nil {
		spec.Resources = container.ResourceLimits{
			Memory: svc.Resources.Limits.Memory,
			CPU:    fmt.Sprintf("%.1f", svc.Resources.Limits.CPU),
		}
	}

	// Resource limit defaults: pids-limit and nofile are always set to prevent
	// fork bombs and file descriptor exhaustion. Manifest permissions can override.
	const defaultPidsLimit = 4096
	const defaultNofileLimit = 65536
	spec.Resources.PidsLimit = defaultPidsLimit
	spec.Resources.NofileLimit = defaultNofileLimit
	if opts.appDef.Permissions != nil && opts.appDef.Permissions.Resources != nil {
		if opts.appDef.Permissions.Resources.MaxProcesses > 0 {
			spec.Resources.PidsLimit = opts.appDef.Permissions.Resources.MaxProcesses
		}
		if opts.appDef.Permissions.Resources.MaxOpenFiles > 0 {
			spec.Resources.NofileLimit = opts.appDef.Permissions.Resources.MaxOpenFiles
		}
	}

	// In --read-only mode, podman provides tmpfs at /tmp and /run automatically
	// (--read-only-tmpfs, default true). Skip default tmpfs mounts to avoid
	// double-mounting, but still apply app-specific tmpfs from storage.temporary.
	if spec.ReadOnly {
		if err := m.applyServiceStorageAndTmpfs(&spec, svc.Storage, opts.layout, nil, opts.credential); err != nil {
			return container.ContainerCreateSpec{}, err
		}
	} else {
		if err := m.applyServiceStorageAndTmpfs(&spec, svc.Storage, opts.layout, opts.appDef.Extensions, opts.credential); err != nil {
			return container.ContainerCreateSpec{}, err
		}
	}

	m.applyOIDCClientInjection(&spec, svc.OIDCClient)
	if err := container.ValidateContainerSpec(spec); err != nil {
		return container.ContainerCreateSpec{}, fmt.Errorf("invalid service container spec for '%s': %w", opts.svcName, err)
	}

	return spec, nil
}

func (m *AppManager) applyServiceStorageAndTmpfs(spec *container.ContainerCreateSpec, storage *api.AppStorage, layout appVolumeLayout, extensions map[string]interface{}, cred *syscall.Credential) error {
	if spec == nil {
		return nil
	}
	if layout.DataDir == "" {
		return fmt.Errorf("container spec requires app volume layout")
	}

	// Rootless volume options: :U so Podman remaps ownership into the
	// container's UID namespace. Without :U, host-root-owned dirs appear
	// as nobody inside the container.
	volOpts := "rw,U"

	mountedPaths := map[string]struct{}{}

	if storage != nil {
		for volName, vol := range storage.Persistent {
			host := filepath.Join(layout.DataDir, volName)
			if err := ensureDir(host, 0o777); err != nil {
				return fmt.Errorf("ensure persistent volume '%s': %w", volName, err)
			}
			if err := container.ChownIfNeeded(host, int(cred.Uid), int(cred.Gid)); err != nil {
				return fmt.Errorf("chown persistent volume '%s': %w", volName, err)
			}
			spec.Volumes = append(spec.Volumes, container.VolumeMapping{
				Host:      host,
				Container: vol.Container,
				Options:   volOpts,
			})
			mountedPaths[vol.Container] = struct{}{}
		}
		for _, vol := range storage.Temporary {
			mountedPaths[vol.Container] = struct{}{}
		}
	}

	// Canonical tmpfs mounts: honor x-piccolo.tmpfs when present, otherwise defaults.
	// When extensions is nil (read-only mode), skip canonical tmpfs entirely —
	// podman's --read-only-tmpfs provides /tmp and /run automatically.
	if extensions != nil {
		for _, p := range tmpfsMountsFromExtensions(extensions) {
			if _, ok := mountedPaths[p]; ok {
				continue
			}
			spec.Tmpfs = append(spec.Tmpfs, container.TmpfsMount{Container: p})
		}
	}

	if storage != nil {
		for _, vol := range storage.Temporary {
			opts := "rw"
			if vol.SizeLimit != "" {
				if sizeOpt := tmpfsSizeOpt(vol.SizeLimit); sizeOpt != "" {
					opts = opts + "," + sizeOpt
				}
			}
			spec.Tmpfs = append(spec.Tmpfs, container.TmpfsMount{
				Container: vol.Container,
				Options:   opts,
			})
		}
	}

	return nil
}

// writeInitScript writes a fetched init script file to the config directory,
// creating intermediate directories as needed. The path is validated to stay
// within configDir to prevent traversal.
func writeInitScript(configDir, relPath string, content []byte, uid, gid int) error {
	scriptPath := filepath.Join(configDir, relPath)

	// Containment check: resolved path must stay under configDir.
	rel, err := filepath.Rel(configDir, scriptPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("init script path escapes config dir: %s", relPath)
	}

	scriptDir := filepath.Dir(scriptPath)
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		return fmt.Errorf("failed to create init script dir: %w", err)
	}
	// Chown intermediate directories for rootless UID remapping.
	if err := container.ChownIfNeeded(scriptDir, uid, gid); err != nil {
		return fmt.Errorf("failed to chown init script dir: %w", err)
	}

	if err := os.WriteFile(scriptPath, content, 0o755); err != nil {
		return fmt.Errorf("failed to write init script: %w", err)
	}
	if err := container.ChownIfNeeded(scriptPath, uid, gid); err != nil {
		return fmt.Errorf("failed to chown init script: %w", err)
	}
	return nil
}

// writeAppConfig materializes the app_config manifest field as /piccolo/config/app.yaml.
// When appConfig is nil, any existing app.yaml is removed to prevent stale config.
func writeAppConfig(configDir string, appConfig interface{}, uid, gid int) error {
	appConfigPath := filepath.Join(configDir, "app.yaml")

	if appConfig == nil {
		if err := os.Remove(appConfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("failed to remove stale app_config: %w", err)
		}
		return nil
	}

	data, err := yaml.Marshal(appConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal app_config: %w", err)
	}
	if err := fsutil.AtomicWriteFile(appConfigPath, data, 0o644); err != nil {
		return fmt.Errorf("failed to write app_config: %w", err)
	}
	if err := container.ChownIfNeeded(appConfigPath, uid, gid); err != nil {
		return fmt.Errorf("failed to chown app_config: %w", err)
	}
	return nil
}

