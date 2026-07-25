package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"piccolod/internal/api"
	"piccolod/internal/fsutil"
	"piccolod/internal/resources/pressure"
)

const capabilityStateVersion = 1

const CapabilityProviderChangeDisclosure = "Switching providers or removing the selected provider may interrupt running requests. Provider-owned configuration, models, indexes, history, and other state are not migrated."

type capabilityState struct {
	Version          int                       `json:"version"`
	Defaults         map[string]string         `json:"defaults"`
	IngressPorts     map[string]map[string]int `json:"ingress_ports,omitempty"`
	AcceleratorGrant *acceleratorGrantRecord   `json:"accelerator_grant,omitempty"`
}

type acceleratorGrantRecord struct {
	Owner   string   `json:"owner"`
	UIDs    []uint32 `json:"uids"`
	Devices []string `json:"devices"`
}

type CapabilitySelectionReconcilePendingError struct {
	Cause error
}

func (e *CapabilitySelectionReconcilePendingError) Error() string {
	return "capability selection persisted but runtime reconciliation failed: " + e.Cause.Error()
}

func (e *CapabilitySelectionReconcilePendingError) Unwrap() error {
	return e.Cause
}

type CapabilityProviderChangeConfirmationRequiredError struct {
	Capability string
	Current    string
	Candidate  string
}

func (e *CapabilityProviderChangeConfirmationRequiredError) Error() string {
	return CapabilityProviderChangeDisclosure
}

type CapabilityProviderStatus struct {
	AppInstance string `json:"app_instance"`
	Enabled     bool   `json:"enabled"`
}

type CapabilityStatus struct {
	Capability               string                     `json:"capability"`
	Default                  string                     `json:"default,omitempty"`
	Providers                []CapabilityProviderStatus `json:"providers"`
	ProviderChangeDisclosure string                     `json:"provider_change_disclosure"`
}

func newCapabilityState() *capabilityState {
	return &capabilityState{
		Version:      capabilityStateVersion,
		Defaults:     make(map[string]string),
		IngressPorts: make(map[string]map[string]int),
	}
}

func (fsm *FilesystemStateManager) capabilityStatePath() string {
	return filepath.Join(fsm.stateDir, "capabilities.json")
}

func (fsm *FilesystemStateManager) loadCapabilityState() (*capabilityState, error) {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()
	return fsm.loadCapabilityStateLocked()
}

func (fsm *FilesystemStateManager) loadCapabilityStateLocked() (*capabilityState, error) {
	data, err := os.ReadFile(fsm.capabilityStatePath())
	if os.IsNotExist(err) {
		return newCapabilityState(), nil
	}
	if err != nil {
		return nil, err
	}
	var state capabilityState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse capability state: %w", err)
	}
	if state.Version != capabilityStateVersion {
		return nil, fmt.Errorf("unsupported capability state version %d", state.Version)
	}
	if state.Defaults == nil {
		state.Defaults = make(map[string]string)
	}
	if state.IngressPorts == nil {
		state.IngressPorts = make(map[string]map[string]int)
	}
	if grant := state.AcceleratorGrant; grant != nil {
		grant.Owner = strings.TrimSpace(grant.Owner)
		grant.UIDs = sortedUniqueUint32s(grant.UIDs)
		grant.Devices = sortedUniqueStrings(grant.Devices)
		if grant.Owner == "" || len(grant.UIDs) == 0 || len(grant.Devices) == 0 {
			return nil, fmt.Errorf("invalid persisted accelerator grant")
		}
		for _, uid := range grant.UIDs {
			if uid == 0 {
				return nil, fmt.Errorf("invalid persisted accelerator grant")
			}
		}
	}
	return &state, nil
}

func sortedUniqueUint32s(values []uint32) []uint32 {
	if len(values) == 0 {
		return nil
	}
	result := append([]uint32(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return slices.Compact(result)
}

func (fsm *FilesystemStateManager) storeCapabilityState(state *capabilityState) error {
	if state == nil {
		return fmt.Errorf("capability state is required")
	}
	if fsm.storeCapabilityStateHook != nil {
		if err := fsm.storeCapabilityStateHook(state); err != nil {
			return err
		}
	}
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()
	state.Version = capabilityStateVersion
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(fsm.capabilityStatePath(), data, 0o600)
}

func registeredCapabilities() []string {
	return []string{api.CapabilityAIInferenceOpenAIV1}
}

func providedCapability(def *api.AppDefinition, capability string) (listener, basePath string, ok bool) {
	if def == nil {
		return "", "", false
	}
	for _, candidate := range def.Listeners {
		for _, provider := range candidate.Provides {
			if provider.Capability == capability {
				return candidate.Name, provider.BasePath, true
			}
		}
	}
	return "", "", false
}

func consumedCapabilities(def *api.AppDefinition) map[string]struct{} {
	result := make(map[string]struct{})
	if def == nil {
		return result
	}
	for _, service := range def.Services {
		for _, consumer := range service.Consumes {
			result[consumer.Capability] = struct{}{}
		}
	}
	return result
}

func sortedCapabilityProviders(state *FilesystemStateManager, capability string, excluded map[string]struct{}) []*AppInstance {
	apps := state.ListApps()
	filtered := make([]*AppInstance, 0, len(apps))
	for _, app := range apps {
		if app == nil || !app.Enabled {
			continue
		}
		if _, skip := excluded[app.InstanceID]; skip {
			continue
		}
		if _, _, ok := providedCapability(app.Definition, capability); ok {
			filtered = append(filtered, app)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].CreatedAt.Equal(filtered[j].CreatedAt) {
			return filtered[i].InstanceID < filtered[j].InstanceID
		}
		return filtered[i].CreatedAt.Before(filtered[j].CreatedAt)
	})
	return filtered
}

func selectedProviderStillValid(state *FilesystemStateManager, capability, instanceID string, excluded map[string]struct{}) bool {
	if instanceID == "" {
		return false
	}
	if _, skip := excluded[instanceID]; skip {
		return false
	}
	app, ok := state.GetApp(instanceID)
	if !ok || app == nil {
		return false
	}
	_, _, provided := providedCapability(app.Definition, capability)
	return provided
}

func capabilityBindingSignature(state *FilesystemStateManager, capability, instanceID string) string {
	app, ok := state.GetApp(instanceID)
	if !ok || app == nil {
		return ""
	}
	_, basePath, ok := providedCapability(app.Definition, capability)
	if !ok {
		return ""
	}
	return basePath
}

func desiredCapabilityBindings(state *FilesystemStateManager, consumer string, def *api.AppDefinition) (map[string]string, error) {
	result := make(map[string]string)
	consumed := consumedCapabilities(def)
	if len(consumed) == 0 {
		return result, nil
	}
	durable, err := state.loadCapabilityState()
	if err != nil {
		return nil, err
	}
	for capability := range consumed {
		providerSignature := capabilityBindingSignature(state, capability, durable.Defaults[capability])
		port := durable.IngressPorts[consumer][capability]
		result[capability] = fmt.Sprintf("%s\x00%d", providerSignature, port)
	}
	return result, nil
}

func capabilityGenerationMatches(app *AppInstance, desired map[string]string) bool {
	if app == nil {
		return len(desired) == 0
	}
	return maps.Equal(app.CapabilityBindings, desired)
}

// capabilityConsumerBindingCurrent proves that this one private ingress still
// belongs to the binding generation committed for its consumer. Other
// capabilities do not fence this route independently.
func capabilityConsumerBindingCurrent(state *FilesystemStateManager, consumer, capability string) bool {
	app, ok := state.GetApp(consumer)
	if !ok || app == nil {
		return false
	}
	if _, consumes := consumedCapabilities(app.Definition)[capability]; !consumes {
		return false
	}
	desired, err := desiredCapabilityBindings(state, consumer, app.Definition)
	if err != nil {
		return false
	}
	committed, committedOK := app.CapabilityBindings[capability]
	expected, expectedOK := desired[capability]
	return committedOK && expectedOK && committed == expected
}

// reconcileCapabilityDefaults preserves valid selections (including manually
// stopped providers) and deterministically fills only absent/invalid defaults.
func (m *AppManager) reconcileCapabilityDefaults(state *FilesystemStateManager, excluded ...string) (map[string][2]string, error) {
	excludedSet := make(map[string]struct{}, len(excluded))
	for _, instanceID := range excluded {
		excludedSet[instanceID] = struct{}{}
	}
	durable, err := state.loadCapabilityState()
	if err != nil {
		return nil, err
	}
	changes := make(map[string][2]string)
	for _, capability := range registeredCapabilities() {
		old := durable.Defaults[capability]
		if selectedProviderStillValid(state, capability, old, excludedSet) {
			continue
		}
		replacement := ""
		if providers := sortedCapabilityProviders(state, capability, excludedSet); len(providers) > 0 {
			replacement = providers[0].InstanceID
		}
		if old != replacement {
			changes[capability] = [2]string{old, replacement}
			if replacement == "" {
				delete(durable.Defaults, capability)
			} else {
				durable.Defaults[capability] = replacement
			}
		}
	}
	if len(changes) > 0 {
		var restoreFences []func()
		fenced := make(map[string]struct{})
		for _, capability := range registeredCapabilities() {
			replacement := changes[capability][1]
			if replacement == "" {
				continue
			}
			if _, exists := fenced[replacement]; exists {
				continue
			}
			fenced[replacement] = struct{}{}
			restoreFences = append(restoreFences, m.fenceCapabilityProviderStarting(replacement))
		}
		if err := state.storeCapabilityState(durable); err != nil {
			for i := len(restoreFences) - 1; i >= 0; i-- {
				restoreFences[i]()
			}
			return nil, err
		}
	}
	return changes, nil
}

func (m *AppManager) ListCapabilities(ctx context.Context) ([]CapabilityStatus, error) {
	if err := m.ensureUnlocked(); err != nil {
		return nil, err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	durable, err := state.loadCapabilityState()
	if err != nil {
		return nil, err
	}
	statuses := make([]CapabilityStatus, 0, len(registeredCapabilities()))
	for _, capability := range registeredCapabilities() {
		status := CapabilityStatus{
			Capability:               capability,
			Default:                  durable.Defaults[capability],
			ProviderChangeDisclosure: CapabilityProviderChangeDisclosure,
		}
		apps := state.ListApps()
		sort.Slice(apps, func(i, j int) bool { return apps[i].InstanceID < apps[j].InstanceID })
		for _, app := range apps {
			_, _, ok := providedCapability(app.Definition, capability)
			if !ok {
				continue
			}
			status.Providers = append(status.Providers, CapabilityProviderStatus{
				AppInstance: app.InstanceID,
				Enabled:     app.Enabled,
			})
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func (m *AppManager) SelectCapabilityProvider(ctx context.Context, capability, instanceID string) (err error) {
	return m.selectCapabilityProvider(ctx, capability, instanceID, true)
}

func (m *AppManager) SelectCapabilityProviderAcknowledged(
	ctx context.Context,
	capability, instanceID string,
	acknowledged bool,
) error {
	return m.selectCapabilityProvider(ctx, capability, instanceID, acknowledged)
}

func (m *AppManager) selectCapabilityProvider(
	ctx context.Context,
	capability, instanceID string,
	acknowledged bool,
) (err error) {
	instanceID = strings.TrimSpace(instanceID)
	m.emitProgress(ctx, taskTypeSelectCapability, instanceID, taskPhaseValidating, 0, "Validating capability provider", false, nil)
	defer func() {
		if err == nil {
			m.emitProgress(ctx, taskTypeSelectCapability, instanceID, taskPhaseComplete, 100, "Capability provider selected", true, nil)
			return
		}
		var pending *CapabilitySelectionReconcilePendingError
		if errors.As(err, &pending) {
			m.emitProgress(ctx, taskTypeSelectCapability, instanceID, taskPhaseComplete, 100, "Capability provider selected; reconciliation pending", true, nil)
			return
		}
		m.emitProgress(ctx, taskTypeSelectCapability, instanceID, taskPhaseComplete, 100, "Capability provider selection failed", true, err)
	}()
	defer pressure.BeginLifecycleOwner("capability:select")()
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return err
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	if capability != api.CapabilityAIInferenceOpenAIV1 {
		return fmt.Errorf("unknown capability %q", capability)
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	app, ok := state.GetApp(instanceID)
	if !ok || app == nil || !app.Enabled {
		return fmt.Errorf("capability provider %q is not an enabled installed app", instanceID)
	}
	if _, _, ok := providedCapability(app.Definition, capability); !ok {
		return fmt.Errorf("app %q does not provide %s", instanceID, capability)
	}
	durable, err := state.loadCapabilityState()
	if err != nil {
		return err
	}
	old := durable.Defaults[capability]
	if old != "" && old != instanceID && !acknowledged {
		return &CapabilityProviderChangeConfirmationRequiredError{
			Capability: capability,
			Current:    old,
			Candidate:  instanceID,
		}
	}
	if old == instanceID {
		if err := m.reconcilePersistedAcceleratorGrant(ctx, state, nil); err != nil {
			return &CapabilitySelectionReconcilePendingError{Cause: err}
		}
		if err := m.finalizeCommittedCapabilityRuntime(ctx, state, instanceID); err != nil {
			return &CapabilitySelectionReconcilePendingError{Cause: err}
		}
		return nil
	}
	durable.Defaults[capability] = instanceID
	restoreFence := m.fenceCapabilityProviderStarting(instanceID)
	if err := state.storeCapabilityState(durable); err != nil {
		restoreFence()
		return err
	}
	if err := m.applyCapabilityDefaultChange(ctx, state, capability, old, instanceID, nil); err != nil {
		// The durable selection remains authoritative. Reconciliation will retry
		// its effects; do not fabricate a rollback after a one-sided runtime
		// failure.
		return &CapabilitySelectionReconcilePendingError{Cause: err}
	}
	return nil
}

func capabilitySelectedByProvider(state *FilesystemStateManager, instanceID string) (string, bool, error) {
	durable, err := state.loadCapabilityState()
	if err != nil {
		return "", false, err
	}
	for _, capability := range registeredCapabilities() {
		if durable.Defaults[capability] == instanceID {
			return capability, true, nil
		}
	}
	return "", false, nil
}

func (m *AppManager) recreateAppForCapabilityEffects(
	ctx context.Context,
	state *FilesystemStateManager,
	instanceID string,
	afterRemoval func() error,
) error {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		if afterRemoval != nil {
			return afterRemoval()
		}
		return nil
	}
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceNormalReconcile); err != nil {
		return err
	}
	legacyTransitionActive, err := transitionLegacyJournalExists(state, instanceID)
	if err != nil {
		return fmt.Errorf("read legacy transition journals before capability reconciliation: %w", err)
	}
	if legacyTransitionActive {
		return fmt.Errorf(
			"%w: %s has a legacy transition journal entry=%s",
			ErrTransitionInProgress,
			instanceID,
			TransitionFenceNormalReconcile,
		)
	}
	app, ok := state.GetApp(instanceID)
	if !ok || app == nil {
		if afterRemoval != nil {
			return afterRemoval()
		}
		return nil
	}
	def, err := state.GetAppDefinition(instanceID)
	if err != nil {
		return err
	}
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return err
	}
	runtimeConfig, err := m.podmanRuntimeForApp(ctx, instanceID, layout, piccoloModeFromExtensions(def.Extensions), appRuntimeEnsureReady)
	if err != nil {
		return err
	}
	rootfs, err := m.ensureAllServiceRootfsAttached(ctx, instanceID, piccoloModeFromExtensions(def.Extensions), def, app)
	if err != nil {
		return err
	}
	if m.serviceManager != nil {
		m.serviceManager.DeactivateApp(instanceID)
	}
	if err := m.stopContainersForMultiApp(ctx, app, def, runtimeConfig); err != nil {
		return fmt.Errorf("stop %s for capability reconciliation: %w", instanceID, err)
	}
	if err := m.removeContainersForMultiApp(ctx, app, def, runtimeConfig); err != nil {
		return fmt.Errorf("remove %s for capability reconciliation: %w", instanceID, err)
	}
	if m.serviceManager != nil {
		m.serviceManager.RemoveApp(instanceID)
	}
	m.removeCapabilityIngresses(instanceID)
	if afterRemoval != nil {
		if err := afterRemoval(); err != nil {
			return err
		}
	}
	if err := m.commitRemovedContainerGroup(state, app); err != nil {
		return err
	}
	if !app.Enabled {
		return nil
	}
	return m.recreateMissingMultiContainer(ctx, state, app, def, layout, runtimeConfig, rootfs)
}

func (m *AppManager) fenceCapabilityProviderStarting(instanceID string) func() {
	previousStatus, previousMessage := m.getObservedStatusAndMessage(instanceID)
	m.updateStatusWithEvent(instanceID, StatusStarting)
	return func() {
		m.updateStatusAndMessageWithEvent(instanceID, previousStatus, previousMessage)
	}
}

func (m *AppManager) applyCapabilityDefaultChange(
	ctx context.Context,
	state *FilesystemStateManager,
	capability, oldProvider, newProvider string,
	alreadyRemoved map[string]struct{},
) error {
	if capability != api.CapabilityAIInferenceOpenAIV1 {
		return nil
	}
	handled := make(map[string]struct{})
	if oldProvider != "" && oldProvider != newProvider {
		if _, removed := alreadyRemoved[oldProvider]; removed {
			if err := m.revokeAcceleratorAccess(ctx, state, oldProvider); err != nil {
				return err
			}
			if old, ok := state.GetApp(oldProvider); ok && old != nil && len(old.AcceleratorDevices) > 0 {
				old.AcceleratorDevices = nil
				if err := state.StoreAppMetadata(old); err != nil {
					return fmt.Errorf("persist withdrawn accelerator ownership for %s: %w", oldProvider, err)
				}
			}
		} else {
			if err := m.recreateAppForCapabilityEffects(ctx, state, oldProvider, func() error {
				return m.revokeAcceleratorAccess(ctx, state, oldProvider)
			}); err != nil {
				return err
			}
		}
		handled[oldProvider] = struct{}{}
	}
	if newProvider != "" && oldProvider != newProvider {
		if err := m.recreateAppForCapabilityEffects(ctx, state, newProvider, nil); err != nil {
			return err
		}
		handled[newProvider] = struct{}{}
	}
	for _, consumer := range state.ListApps() {
		if consumer == nil || !consumer.Enabled {
			continue
		}
		if _, already := handled[consumer.InstanceID]; already {
			continue
		}
		if _, consumes := consumedCapabilities(consumer.Definition)[capability]; !consumes {
			continue
		}
		desired, err := desiredCapabilityBindings(state, consumer.InstanceID, consumer.Definition)
		if err != nil {
			return err
		}
		if capabilityGenerationMatches(consumer, desired) {
			continue
		}
		if err := m.recreateAppForCapabilityEffects(ctx, state, consumer.InstanceID, nil); err != nil {
			return err
		}
	}
	return nil
}

func (m *AppManager) reconcileCapabilityDefaultsAndEffects(
	ctx context.Context,
	state *FilesystemStateManager,
	excluded ...string,
) error {
	changes, err := m.reconcileCapabilityDefaults(state, excluded...)
	if err != nil {
		return err
	}
	capabilities := make([]string, 0, len(changes))
	for capability := range changes {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)
	alreadyRemoved := make(map[string]struct{}, len(excluded))
	for _, instanceID := range excluded {
		alreadyRemoved[instanceID] = struct{}{}
	}
	for _, capability := range capabilities {
		change := changes[capability]
		if err := m.applyCapabilityDefaultChange(ctx, state, capability, change[0], change[1], alreadyRemoved); err != nil {
			return err
		}
	}
	return m.reconcilePersistedAcceleratorGrant(ctx, state, alreadyRemoved)
}

func (m *AppManager) reconcileConsumerCapabilityBindings(
	ctx context.Context,
	state *FilesystemStateManager,
) error {
	apps := state.ListApps()
	sort.Slice(apps, func(i, j int) bool {
		return apps[i].InstanceID < apps[j].InstanceID
	})
	for _, app := range apps {
		if app == nil || !app.Enabled || len(consumedCapabilities(app.Definition)) == 0 {
			continue
		}
		desired, err := desiredCapabilityBindings(state, app.InstanceID, app.Definition)
		if err != nil {
			return err
		}
		if capabilityGenerationMatches(app, desired) {
			continue
		}
		if err := m.recreateAppForCapabilityEffects(ctx, state, app.InstanceID, nil); err != nil {
			return err
		}
	}
	return nil
}

// finalizeCommittedCapabilityRuntime converges effects created by an
// already-committed ordinary app replacement. The selected app exercises its
// existing accelerator entitlement during replacement; this helper applies
// binding/default changes and then uses ordinary app reconciliation to prove
// the selected provider's runtime and publication are usable.
func (m *AppManager) finalizeCommittedCapabilityRuntime(
	ctx context.Context,
	state *FilesystemStateManager,
	instanceID string,
) error {
	changes, err := m.reconcileCapabilityDefaults(state)
	if err != nil {
		return err
	}
	capabilities := make([]string, 0, len(changes))
	for capability := range changes {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)

	convergeSelectedRuntime := func() error {
		app, ok := state.GetApp(instanceID)
		if !ok || app == nil || !app.Enabled {
			return nil
		}
		desired, err := m.desiredAcceleratorDevices(state, instanceID, app.Definition)
		if err != nil {
			return err
		}
		if !acceleratorGenerationMatches(app, desired) {
			return m.recreateAppForCapabilityEffects(ctx, state, instanceID, nil)
		}
		if err := m.reconcileApp(ctx, state, app); err != nil {
			return err
		}
		hasRuntimeProjection := strings.TrimSpace(app.NetworkAnchorID) != "" ||
			len(app.Containers) > 0 ||
			len(app.ActiveRootfs) > 0
		if hasRuntimeProjection {
			if m.getObservedStatus(instanceID) != StatusRunning {
				return fmt.Errorf("capability provider %s runtime reconciliation remains pending", instanceID)
			}
			if m.serviceManager != nil &&
				len(app.Definition.Listeners) > 0 &&
				!m.serviceManager.AppPublicationActive(instanceID) {
				return fmt.Errorf("capability provider %s runtime reconciled without active publication", instanceID)
			}
		}
		return nil
	}

	alreadyRemoved := map[string]struct{}{instanceID: {}}
	for _, capability := range capabilities {
		change := changes[capability]
		if err := m.applyCapabilityDefaultChange(ctx, state, capability, change[0], change[1], alreadyRemoved); err != nil {
			return err
		}
	}
	if err := convergeSelectedRuntime(); err != nil {
		return err
	}
	return m.reconcileConsumerCapabilityBindings(ctx, state)
}
