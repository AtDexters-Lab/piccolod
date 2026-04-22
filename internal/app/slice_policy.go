package app

import (
	"fmt"
	"log"
	"sync"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/resources"
)

// sliceReconcileMu serializes the ReconcileAllSlicePolicies pass so
// concurrent install/uninstall/manifest-change events don't produce
// interleaved drop-in writes.
var sliceReconcileMu sync.Mutex

// computeSliceResourcePolicy derives the slice policy for a single app
// instance from its manifest + current system state.
//
// Returns an empty policy (IsEmpty==true) when the manifest has no
// resources declaration — callers translate empty-with-valid-UID into
// "clear any existing drop-in, reset live state to defaults."
func (m *AppManager) computeSliceResourcePolicy(appInst *AppInstance, activeElasticCount int) (container.SliceResourcePolicy, error) {
	if appInst == nil {
		return container.SliceResourcePolicy{}, fmt.Errorf("slice policy: appInst is nil")
	}

	user, err := container.ResolveRuntimeCredential(container.AppUsername(appInst.InstanceID))
	if err != nil {
		return container.SliceResourcePolicy{}, fmt.Errorf("slice policy: resolve runtime user for %s: %w", appInst.InstanceID, err)
	}
	policy := container.SliceResourcePolicy{UID: user.Credential.Uid}

	def := appInst.Definition
	if def == nil || def.Resources == nil {
		return policy, nil
	}

	// CPU priority → CPUWeight (always set when resources block is declared
	// so catalog authors don't need to know the default weight).
	policy.CPUWeight = resources.PriorityToCPUWeight(def.Resources.Priority)

	if def.Resources.Memory != nil {
		minReq, err := resources.ParseSize(def.Resources.Memory.MinRequired)
		if err != nil {
			return container.SliceResourcePolicy{}, fmt.Errorf("slice policy: parse min_required for %s: %w", appInst.InstanceID, err)
		}
		systemRAM := resources.SystemRAMBytes()
		knobs := resources.DeriveMemoryKnobs(minReq, def.Resources.Memory.Profile, systemRAM, activeElasticCount)
		policy.MemoryHighBytes = knobs.HighBytes
		policy.MemoryMaxBytes = knobs.MaxBytes
	}

	return policy, nil
}

// countActiveElasticApps returns the number of active elastic-profile
// apps, including the app being provisioned (counting self keeps the
// elastic-share denominator stable across Starting→Running transitions).
//
// Definition of "active":
//   - observed status in {Running, Starting} → counted
//   - observed status empty AND app.Enabled → counted (boot transient:
//     the background reconciler has not yet observed this app; treating
//     it as active avoids over-generous allocation on the first boot
//     reconcile cycle that would then tighten under load minutes later)
//   - otherwise (Stopped, Error, Disabled) → not counted
//
// IMPORTANT: reads observed status via getObservedStatus. The Status
// field on persisted AppInstance is marked "not persisted" and is only
// set at install-success; after a piccolod restart the cache Status is
// empty until the background reconciler re-runs. Reading app.Status
// directly would undercount active apps at boot.
func (m *AppManager) countActiveElasticApps() int {
	apps := m.snapshotApps(false)
	count := 0
	for _, app := range apps {
		if app.Definition == nil || app.Definition.Resources == nil || app.Definition.Resources.Memory == nil {
			continue
		}
		if app.Definition.Resources.Memory.Profile != api.ProfileElastic {
			continue
		}
		observed := m.getObservedStatus(app.InstanceID)
		switch observed {
		case StatusRunning, StatusStarting:
			count++
		case "":
			// Boot transient or never-observed. Count if enabled so
			// elastic shares don't briefly over-allocate and then tighten.
			if app.Enabled {
				count++
			}
		}
	}
	if count < 1 {
		count = 1
	}
	return count
}

// ReconcileAllSlicePolicies walks all installed apps, derives the current
// slice policy for each, and applies it. Safe to call from: install
// completion, uninstall cleanup, manifest-change handler, periodic timer,
// and piccolod startup. Serialized via sliceReconcileMu.
//
// Fail-policy per plan Q2:
//   - priority=normal + profile=bounded: fail-open (kernel defaults are
//     acceptably close to intended behaviour; periodic reconcile retries).
//   - priority=background|high OR profile=elastic: fail-closed at the app
//     level — mark the app's observed status message so the UI surfaces
//     "resource policy not enforced" and a follow-up reconcile tick retries.
//     Rationale: kernel defaults silently promote background→normal, leave
//     elastic unbounded, and drop high's priority boost — these are
//     user-visible divergences from the manifest's declared intent.
func (m *AppManager) ReconcileAllSlicePolicies() {
	sliceReconcileMu.Lock()
	defer sliceReconcileMu.Unlock()

	apps := m.snapshotApps(false)
	activeCount := m.countActiveElasticApps()

	for _, app := range apps {
		policy, err := m.computeSliceResourcePolicy(app, activeCount)
		if err != nil {
			log.Printf("WARN: slice policy compute %s: %v", app.InstanceID, err)
			continue
		}
		if policy.UID == 0 {
			// App provisioning hasn't completed — user doesn't exist yet.
			continue
		}
		if err := policy.Apply(); err != nil {
			m.reportSlicePolicyFailure(app, err)
		}
	}
}

// reportSlicePolicyFailure logs a drop-in/live-apply failure and, for
// profiles where kernel defaults produce user-visible wrong behaviour
// (elastic / background / high), surfaces the condition via the app's
// observed-status message so the UI can show "policy not enforced".
// The periodic reconciler (P1.7) will retry on its next tick.
func (m *AppManager) reportSlicePolicyFailure(app *AppInstance, applyErr error) {
	log.Printf("WARN: slice policy apply %s: %v", app.InstanceID, applyErr)

	if app.Definition == nil || app.Definition.Resources == nil {
		return
	}
	r := app.Definition.Resources
	strictProfile := r.Priority == api.PriorityBackground ||
		r.Priority == api.PriorityHigh ||
		(r.Memory != nil && r.Memory.Profile == api.ProfileElastic)
	if !strictProfile {
		return
	}
	msg := fmt.Sprintf("Resource policy not enforced (%v); kernel defaults in effect until next reconcile.", applyErr)
	m.setObservedStatusMessage(app.InstanceID, msg)
}

// CheckInstallMemoryGate enforces D-11's two-tier install-time gate
// against the candidate app's declared min_required.
//
// Tier 2 (hard block): the app alone requires > system_RAM. Slice
// enforcement would be toothless because the cgroup limit would be
// unreachable. Any runtime state of the app will force host-scope OOM.
// Returns an error; install is refused.
//
// Tier 1 (soft warn): the candidate + already-installed apps'
// min_required collectively exceed usable_RAM (80% of system RAM). The
// slice clamp in D-1 keeps failure contained to the offending slice,
// but the user's machine may struggle under load. Currently logs a WARN;
// future work: escalate to a UI confirmation response (Tier-1 today is
// server-log only because the install path has no synchronous
// confirm-with-user mechanism).
//
// Returns nil if the candidate has no memory declaration (kernel
// defaults will apply) or passes both tiers.
func (m *AppManager) CheckInstallMemoryGate(appDef *api.AppDefinition) error {
	if appDef == nil || appDef.Resources == nil || appDef.Resources.Memory == nil {
		return nil
	}
	candidate, err := resources.ParseSize(appDef.Resources.Memory.MinRequired)
	if err != nil {
		return fmt.Errorf("install gate: parse candidate min_required: %w", err)
	}
	if candidate <= 0 {
		return nil
	}

	systemRAM := resources.SystemRAMBytes()
	if systemRAM <= 0 {
		// Sysinfo failed — Tier-2 is the "cannot be contained" case and
		// is the load-bearing host-safety invariant; without a known
		// system_RAM we cannot verify it. Fail-closed: refuse install
		// rather than silently skipping the only hard-block tier.
		return fmt.Errorf("install refused: cannot verify system memory (sysinfo unavailable); retry later or contact support")
	}
	usableRAM := int64(float64(systemRAM) * resources.UsableRAMRatio)

	// Tier 2: single-app-over-host.
	if candidate > systemRAM {
		return fmt.Errorf(
			"install refused: app declares min_required=%d bytes (%.1f GiB) but system has only %d bytes (%.1f GiB). "+
				"The app cannot be contained within available memory and would destabilize the device. "+
				"Uninstall a sibling app or upgrade hardware.",
			candidate, bytesToGiB(candidate),
			systemRAM, bytesToGiB(systemRAM),
		)
	}

	// Tier 1: sum across installed + candidate.
	apps := m.snapshotApps(false)
	var sumInstalled int64
	for _, app := range apps {
		if app.Definition == nil || app.Definition.Resources == nil || app.Definition.Resources.Memory == nil {
			continue
		}
		n, err := resources.ParseSize(app.Definition.Resources.Memory.MinRequired)
		if err != nil {
			continue
		}
		sumInstalled += n
	}
	total := sumInstalled + candidate
	if total > usableRAM {
		log.Printf("WARN: install gate (Tier-1): total min_required %.1f GiB exceeds usable_RAM %.1f GiB on this device. App may run slowly or restart under load; the app itself will be the failure victim thanks to slice clamp. Proceeding with install.",
			bytesToGiB(total), bytesToGiB(usableRAM))
	}
	return nil
}

func bytesToGiB(b int64) float64 {
	return float64(b) / float64(1<<30)
}

// RemoveSlicePolicyForApp clears the slice drop-in for an uninstalled app.
// Called from the uninstall path before user deletion. Does not live-reset
// the slice — the slice itself is about to be torn down.
func (m *AppManager) RemoveSlicePolicyForApp(instanceID string) {
	user, err := container.ResolveRuntimeCredential(container.AppUsername(instanceID))
	if err != nil {
		// User may already be gone; nothing to do.
		return
	}
	policy := container.SliceResourcePolicy{UID: user.Credential.Uid}
	if err := policy.Remove(); err != nil {
		log.Printf("WARN: slice policy remove %s: %v", instanceID, err)
	}
}
