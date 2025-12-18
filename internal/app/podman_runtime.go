package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"piccolod/internal/container"
	"piccolod/internal/state/paths"
)

func (m *AppManager) podmanRuntimeForApp(appName string, layout appVolumeLayout) (container.PodmanRuntime, error) {
	volID := layout.VolumeID
	if volID == "" {
		volID = appVolumeID(appName)
	}

	runRootCandidates := []string{}
	if base := os.Getenv("PICCOLO_PODMAN_RUNROOT_BASE"); strings.TrimSpace(base) != "" {
		base = filepath.Clean(base)
		if filepath.IsAbs(base) {
			runRootCandidates = append(runRootCandidates, base)
		}
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); strings.TrimSpace(xdg) != "" {
		runRootCandidates = append(runRootCandidates, filepath.Join(xdg, "piccolo", "podman"))
	}
	runRootCandidates = append(runRootCandidates, "/run/piccolo/podman")
	runRootCandidates = append(runRootCandidates, paths.Join("run", "podman"))

	var runRoot string
	for _, base := range runRootCandidates {
		candidate := filepath.Join(base, volID)
		if err := os.MkdirAll(candidate, 0o700); err != nil {
			continue
		}
		runRoot = candidate
		break
	}
	if runRoot == "" {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: unable to create a writable podman runroot")
	}

	if layout.PodmanRoot == "" {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: podman root missing for %s", appName)
	}

	imagestoreCandidates := []string{}
	if base := os.Getenv("PICCOLO_PODMAN_IMAGESTORE_BASE"); strings.TrimSpace(base) != "" {
		base = filepath.Clean(base)
		if filepath.IsAbs(base) {
			imagestoreCandidates = append(imagestoreCandidates, base)
		}
	}
	imagestoreCandidates = append(imagestoreCandidates, paths.Join("podman", "imagestore"))

	var imagestore string
	for _, candidate := range imagestoreCandidates {
		if err := os.MkdirAll(candidate, 0o700); err != nil {
			continue
		}
		imagestore = candidate
		break
	}
	if imagestore == "" {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: unable to create a writable podman imagestore")
	}

	return container.PodmanRuntime{
		Root:       layout.PodmanRoot,
		RunRoot:    runRoot,
		Imagestore: imagestore,
	}, nil
}
