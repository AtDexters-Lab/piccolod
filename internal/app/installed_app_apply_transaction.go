package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"piccolod/internal/api"
)

type installedAppApplyTransactionSpec struct {
	OperationKind             string
	TaskType                  string
	RollbackPrefix            string
	InstanceID                string
	AppInst                   *AppInstance
	PreviousDefinition        *api.AppDefinition
	CandidateDefinition       *api.AppDefinition
	PreviousManifestHash      string
	CandidateManifestHash     string
	PreviousLedgerRevision    int64
	CandidateLedgerRevision   int64
	PreviousLedgerSourceHash  string
	CandidateLedgerSourceHash string
	DryRunToken               string
	RuntimeFingerprint        string
	MetadataOnly              bool
	ApplyPhase                string
	ApplyMessage              string
	FinalizingMessage         string
}

type installedAppApplyTransaction struct {
	manager *AppManager
	ctx     context.Context
	state   *FilesystemStateManager
	spec    installedAppApplyTransactionSpec
	txn     *ManifestUpdateTransaction
}

func (m *AppManager) beginInstalledAppApplyTransaction(ctx context.Context, state *FilesystemStateManager, spec installedAppApplyTransactionSpec) (*installedAppApplyTransaction, error) {
	operationID, err := randomManifestUpdateToken()
	if err != nil {
		return nil, err
	}
	backupPath, err := state.BackupCurrentAppDefinitionForManifestUpdate(spec.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("backup current manifest: %w", err)
	}
	backupInstallStatePath, err := state.BackupInstallStateForManifestUpdate(spec.InstanceID)
	if err != nil {
		_ = state.ClearManifestUpdateTransaction(spec.InstanceID, backupPath)
		return nil, fmt.Errorf("backup install state: %w", err)
	}
	txn := &ManifestUpdateTransaction{
		OperationID:               operationID,
		OperationKind:             spec.OperationKind,
		Phase:                     "prepared",
		PreviousManifestHash:      spec.PreviousManifestHash,
		CandidateManifestHash:     spec.CandidateManifestHash,
		PreviousLedgerRevision:    spec.PreviousLedgerRevision,
		CandidateLedgerRevision:   spec.CandidateLedgerRevision,
		PreviousLedgerSourceHash:  spec.PreviousLedgerSourceHash,
		CandidateLedgerSourceHash: spec.CandidateLedgerSourceHash,
		DryRunToken:               spec.DryRunToken,
		RuntimeFingerprint:        spec.RuntimeFingerprint,
		BackupPath:                backupPath,
		BackupInstallStatePath:    backupInstallStatePath,
		CreatedAt:                 time.Now().UTC(),
		UpdatedAt:                 time.Now().UTC(),
	}
	if err := state.StoreManifestUpdateTransaction(spec.InstanceID, txn); err != nil {
		_ = state.ClearManifestUpdateTransaction(spec.InstanceID, backupPath)
		_ = state.ClearInstallStateBackup(backupInstallStatePath)
		return nil, fmt.Errorf("store apply transaction: %w", err)
	}
	return &installedAppApplyTransaction{
		manager: m,
		ctx:     ctx,
		state:   state,
		spec:    spec,
		txn:     txn,
	}, nil
}

func (t *installedAppApplyTransaction) persistCandidateManifest() error {
	if t.spec.ApplyPhase != "" || t.spec.ApplyMessage != "" {
		t.manager.emitProgress(t.ctx, t.spec.TaskType, t.spec.InstanceID, t.spec.ApplyPhase, 20, t.spec.ApplyMessage, false, nil)
	}
	var previous *api.AppDefinition
	if t.spec.AppInst.Definition != nil {
		copy := *t.spec.AppInst.Definition
		previous = &copy
	}
	t.spec.AppInst.Definition = t.spec.CandidateDefinition
	t.spec.AppInst.UpdatedAt = time.Now()
	if err := t.state.StoreApp(t.spec.AppInst); err != nil {
		t.spec.AppInst.Definition = previous
		cause := fmt.Errorf("persist candidate manifest: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return cause
	}
	if err := t.storePhase("candidate_persisted"); err != nil {
		cause := fmt.Errorf("persist candidate transaction marker: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return cause
	}
	return nil
}

func (t *installedAppApplyTransaction) recreateRuntimeIfNeeded() error {
	if t.spec.MetadataOnly {
		return nil
	}
	if err := markManifestTransactionRuntimeTouched(t.state, t.spec.InstanceID, t.txn); err != nil {
		cause := fmt.Errorf("persist runtime transaction marker: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	t.manager.emitProgress(t.ctx, t.spec.TaskType, t.spec.InstanceID, taskPhaseRecreatingContainer, 50, "Recreating containers", false, nil)
	if err := t.manager.recreateContainersInPlace(t.ctx, t.spec.InstanceID, t.spec.CandidateDefinition, t.spec.PreviousDefinition, t.spec.AppInst); err != nil {
		if restoreErr := t.rollback(err); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, err)
	}
	return nil
}

func (t *installedAppApplyTransaction) markCreatedOIDCClient(clientID string) error {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return nil
	}
	prevClientID := t.txn.CreatedOIDCClientID
	t.txn.CreatedOIDCClientID = clientID
	if err := t.state.StoreManifestUpdateTransaction(t.spec.InstanceID, t.txn); err != nil {
		t.txn.CreatedOIDCClientID = prevClientID
		cause := fmt.Errorf("persist oidc client transaction marker: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	return nil
}

func (t *installedAppApplyTransaction) markProxyOIDCDeltaApplied() error {
	if t.txn.ProxyOIDCDeltaApplied {
		return nil
	}
	prevApplied := t.txn.ProxyOIDCDeltaApplied
	t.txn.ProxyOIDCDeltaApplied = true
	if err := t.state.StoreManifestUpdateTransaction(t.spec.InstanceID, t.txn); err != nil {
		t.txn.ProxyOIDCDeltaApplied = prevApplied
		cause := fmt.Errorf("persist proxy oidc transaction marker: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	return nil
}

func (t *installedAppApplyTransaction) commitLedger(nextState *InstallState) error {
	if nextState == nil {
		return nil
	}
	t.txn.CreatedInstallState = t.txn.BackupInstallStatePath == ""
	t.txn.CandidateLedgerRevision = nextState.Revision
	t.txn.CandidateLedgerSourceHash = nextState.RawTemplateHash
	if err := t.storePhase("ledger_committing"); err != nil {
		cause := fmt.Errorf("persist ledger transaction marker: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	t.manager.emitProgress(t.ctx, t.spec.TaskType, t.spec.InstanceID, taskPhaseFinalizing, 85, t.spec.FinalizingMessage, false, nil)
	if err := t.state.StoreInstallState(t.spec.InstanceID, nextState); err != nil {
		cause := fmt.Errorf("persist config ledger: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	return nil
}

func (t *installedAppApplyTransaction) complete() {
	if err := t.storePhase("committed"); err != nil {
		log.Printf("WARN: %s %s: mark committed: %v", t.spec.OperationKind, t.spec.InstanceID, err)
	}
	if err := t.state.ClearManifestUpdateTransaction(t.spec.InstanceID, t.txn.BackupPath); err != nil {
		log.Printf("WARN: %s %s: cleanup transaction: %v", t.spec.OperationKind, t.spec.InstanceID, err)
	}
}

func (t *installedAppApplyTransaction) storePhase(phase string) error {
	prevPhase := t.txn.Phase
	prevUpdatedAt := t.txn.UpdatedAt
	t.txn.Phase = phase
	t.txn.UpdatedAt = time.Now().UTC()
	if err := t.state.StoreManifestUpdateTransaction(t.spec.InstanceID, t.txn); err != nil {
		t.txn.Phase = prevPhase
		t.txn.UpdatedAt = prevUpdatedAt
		return err
	}
	return nil
}

func (t *installedAppApplyTransaction) rollback(cause error) error {
	return t.manager.restoreInstalledAppApplyFailure(
		t.ctx,
		t.state,
		t.spec.AppInst,
		t.spec.PreviousDefinition,
		t.spec.CandidateDefinition,
		t.txn,
		t.spec.TaskType,
		t.spec.OperationKind,
		cause,
	)
}
