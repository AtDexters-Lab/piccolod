package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"piccolod/internal/fsutil"
)

const installStateFilename = "install_state.json"

// InstallState carries the secrets-bearing install context that catalog
// manifest sync needs to deterministically re-render an installed app's
// manifest. It lives alongside metadata.json on the encrypted control volume,
// in a separate file so accidental serialization of AppInstance never
// surfaces plaintext secrets through generic API responses.
//
// IsLegacyBackfill is the load-bearing discriminator for which sync path to
// take. Pre-sync installs (legacy) write a stub with IsLegacyBackfill=true on
// first sync contact; modern installs write the full struct at install time.
// A malformed install_state.json on disk is treated as IsLegacyBackfill=true
// so a single corrupted file does not hard-fail sync for an app.
type InstallState struct {
	InstanceID        string                `json:"instance_id"`
	IsLegacyBackfill  bool                  `json:"is_legacy_backfill,omitempty"`
	InstallInputs     map[string]any        `json:"install_inputs,omitempty"`
	InstallSystemCtx  *InstallSystemContext `json:"install_system_context,omitempty"`
	OIDCCredentials   *OIDCCredentials      `json:"oidc_credentials,omitempty"`
	SyncBlocked       bool                  `json:"sync_blocked,omitempty"`
	SyncBlockedReason string                `json:"sync_blocked_reason,omitempty"`
}

// ErrInstallStateNotFound is returned when no install_state.json exists for an
// app instance. Sync treats this as the legacy-backfill path entry point.
var ErrInstallStateNotFound = errors.New("install_state.json not found")

// installStatePath returns the absolute path of an app's install_state.json.
func (fsm *FilesystemStateManager) installStatePath(instanceID string) string {
	return filepath.Join(fsm.appsDir, instanceID, installStateFilename)
}

// StoreInstallState atomically writes the install state for an app instance.
// The file lives on the encrypted control volume next to metadata.json.
func (fsm *FilesystemStateManager) StoreInstallState(instanceID string, st *InstallState) error {
	if st == nil {
		return fmt.Errorf("nil install state")
	}
	if st.InstanceID == "" {
		st.InstanceID = instanceID
	}
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	appDir := filepath.Join(fsm.appsDir, instanceID)
	if err := os.MkdirAll(appDir, 0700); err != nil {
		return fmt.Errorf("create app directory: %w", err)
	}
	// MkdirAll is a no-op when the directory already exists with a wider
	// mode (other writers — StoreApp, StoreAppMetadata — use 0755). Force
	// the dir to 0700 here so install_state.json's secrets-bearing payload
	// is defended at the directory level too, not just the file mode.
	if err := os.Chmod(appDir, 0700); err != nil {
		return fmt.Errorf("chmod app directory: %w", err)
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize install state: %w", err)
	}

	// 0600 is intentional: install_state.json carries plaintext secrets
	// (OIDC ClientSecret, user-provided passwords). Even though the control
	// volume is encrypted at rest, narrow file mode prevents accidental
	// disclosure to other accounts on the host.
	if err := fsutil.AtomicWriteFile(fsm.installStatePath(instanceID), data, 0600); err != nil {
		return fmt.Errorf("write install state: %w", err)
	}
	return nil
}

// LoadInstallState reads an app's install state. Returns ErrInstallStateNotFound
// if the file does not exist (legacy install signal); other errors propagate.
// A successfully-loaded but malformed file is reported via the returned error;
// callers should treat parse errors as legacy-backfill.
func (fsm *FilesystemStateManager) LoadInstallState(instanceID string) (*InstallState, error) {
	data, err := os.ReadFile(fsm.installStatePath(instanceID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrInstallStateNotFound
		}
		return nil, fmt.Errorf("read install state: %w", err)
	}
	var st InstallState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse install state: %w", err)
	}
	if st.InstanceID == "" {
		st.InstanceID = instanceID
	}
	return &st, nil
}

// DeleteInstallState removes an app's install state file. Best-effort: a
// missing file is not an error.
func (fsm *FilesystemStateManager) DeleteInstallState(instanceID string) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()
	if err := os.Remove(fsm.installStatePath(instanceID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete install state: %w", err)
	}
	return nil
}
