package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"slices"
	"strings"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/cluster"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/services"
)

// DiffKind classifies the structural difference between an installed app
// definition and a freshly-rendered one. The classifier picks the
// minimum-disruption apply path that still preserves correctness.
type DiffKind int

const (
	// DiffKindNone — definitions are byte-equal after canonicalization.
	DiffKindNone DiffKind = iota
	// DiffKindOIDCLibraryOnly — the only differences live under
	// services[*].oidc_client.{authorize_paths,redirect_uri_paths,
	// redirect_uris}. Apply via configureOIDCAuthorizePaths
	// (zero-downtime live path).
	DiffKindOIDCLibraryOnly
	// DiffKindStructuralNoImage — pure structural change that does not touch
	// service images. Apply via container recreate using existing rootfs LVs
	// (no image pull).
	DiffKindStructuralNoImage
	// DiffKindImageOnly — catalog changed an image tag with no other change.
	// Sync stores a manifest-review pending source; UpdateImage only refreshes
	// the currently committed source and must not consume catalog drift.
	DiffKindImageOnly
	// DiffKindStructuralWithImage — both image and structural fields changed.
	// Sync stores a manifest-review pending source when stageable, otherwise
	// fails closed with the service-app update policy reason.
	DiffKindStructuralWithImage
)

func (d DiffKind) String() string {
	switch d {
	case DiffKindNone:
		return "none"
	case DiffKindOIDCLibraryOnly:
		return "oidc_library_only"
	case DiffKindStructuralNoImage:
		return "structural_no_image"
	case DiffKindImageOnly:
		return "image_only"
	case DiffKindStructuralWithImage:
		return "structural_with_image"
	default:
		return "unknown"
	}
}

// classifyDiff compares two app definitions field-by-field and returns the
// minimum-disruption diff kind. Both definitions must have already been
// passed through SetDefaults so default-value application is consistent.
//
// The classifier walks every runtime-affecting top-level field. Anything
// not explicitly enumerated as runtime-irrelevant is treated as structural,
// because the cost of an unnecessary container recreate is much smaller
// than the cost of silently ignoring a runtime change.
//
// Inputs is deliberately excluded from the structural set: input schema
// changes (label, description, default) are install-form metadata and do
// not affect the rendered runtime spec for an existing install (which
// renders against persisted InstallInputs). If a new input default changes
// the rendered env, that change shows up in serviceStructuralDiff.
func classifyDiff(oldDef, newDef *api.AppDefinition) DiffKind {
	if oldDef == nil || newDef == nil {
		return DiffKindStructuralNoImage
	}

	imageChanged := serviceImageDiff(oldDef, newDef)
	oidcLibChanged, oidcLibExclusive := serviceOIDCLibraryDiff(oldDef, newDef)
	structuralBeyondOIDC := serviceStructuralDiffExcludingOIDCLibrary(oldDef, newDef)
	listenersChanged := !reflect.DeepEqual(oldDef.Listeners, newDef.Listeners)
	storageChanged := !reflect.DeepEqual(oldDef.Storage, newDef.Storage)
	permsChanged := !reflect.DeepEqual(oldDef.Permissions, newDef.Permissions)
	envChanged := !reflect.DeepEqual(oldDef.Environment, newDef.Environment)
	resourcesChanged := !reflect.DeepEqual(oldDef.Resources, newDef.Resources)
	healthCheckChanged := !reflect.DeepEqual(oldDef.HealthCheck, newDef.HealthCheck)
	appConfigChanged := !reflect.DeepEqual(oldDef.AppConfig, newDef.AppConfig)
	authChanged := !reflect.DeepEqual(oldDef.Auth, newDef.Auth)
	extensionsChanged := !reflect.DeepEqual(oldDef.Extensions, newDef.Extensions)
	primaryChanged := oldDef.PrimaryService != newDef.PrimaryService
	typeChanged := oldDef.Type != newDef.Type
	wsNameChanged := oldDef.WorkspaceName != newDef.WorkspaceName

	structural := structuralBeyondOIDC ||
		listenersChanged || storageChanged || permsChanged ||
		envChanged || resourcesChanged || healthCheckChanged ||
		appConfigChanged || authChanged || extensionsChanged ||
		primaryChanged || typeChanged || wsNameChanged

	switch {
	case !imageChanged && !structural && !oidcLibChanged:
		return DiffKindNone
	case !imageChanged && !structural && oidcLibChanged && oidcLibExclusive:
		return DiffKindOIDCLibraryOnly
	case imageChanged && !structural && !oidcLibChanged:
		return DiffKindImageOnly
	case imageChanged && (structural || oidcLibChanged):
		return DiffKindStructuralWithImage
	default:
		return DiffKindStructuralNoImage
	}
}

// serviceImageDiff returns true if any service present in BOTH defs has a
// different Image field, OR if the new def adds a service that wasn't in
// the old def. Sync's recreate path would have to pull a new image to
// materialize the added service, which violates the "sync never pulls
// images" invariant; routing additions to DiffKind*Image makes sync defer
// to a manual reinstall instead. Service REMOVALS are not image changes
// (the recreate path drops them safely without any image work) and are
// reported via serviceStructuralDiffExcludingOIDCLibrary.
func serviceImageDiff(oldDef, newDef *api.AppDefinition) bool {
	for name, oldSvc := range oldDef.Services {
		if newSvc, ok := newDef.Services[name]; ok && oldSvc.Image != newSvc.Image {
			return true
		}
	}
	for name := range newDef.Services {
		if _, ok := oldDef.Services[name]; !ok {
			return true
		}
	}
	return false
}

// serviceOIDCLibraryDiff reports whether OIDC library-only fields differ
// (authorize_paths, redirect_uri_paths, redirect_uris, scopes), and whether
// the diff is exclusive to those fields (i.e. all other oidc_client fields
// are unchanged).
func serviceOIDCLibraryDiff(oldDef, newDef *api.AppDefinition) (changed, exclusive bool) {
	if len(oldDef.Services) != len(newDef.Services) {
		return false, false
	}
	exclusive = true
	for name, oldSvc := range oldDef.Services {
		newSvc, ok := newDef.Services[name]
		if !ok {
			return false, false
		}
		oldOIDC := oldSvc.OIDCClient
		newOIDC := newSvc.OIDCClient
		switch {
		case oldOIDC == nil && newOIDC == nil:
			continue
		case oldOIDC == nil || newOIDC == nil:
			return true, false
		}
		if !equalStringSliceUnordered(oldOIDC.AuthorizePaths, newOIDC.AuthorizePaths) {
			changed = true
		}
		if !equalStringSliceUnordered(oldOIDC.RedirectURIPaths, newOIDC.RedirectURIPaths) {
			changed = true
		}
		if !equalStringSliceUnordered(oldOIDC.RedirectURIs, newOIDC.RedirectURIs) {
			changed = true
		}
		// Detect changes in any other oidc_client field that is NOT one of
		// the three library-only ones. Compare a stripped copy.
		stripped := func(c api.ServiceOIDCClient) api.ServiceOIDCClient {
			c.AuthorizePaths = nil
			c.RedirectURIPaths = nil
			c.RedirectURIs = nil
			return c
		}
		if !reflect.DeepEqual(stripped(*oldOIDC), stripped(*newOIDC)) {
			exclusive = false
		}
	}
	return changed, exclusive
}

// serviceStructuralDiffExcludingOIDCLibrary returns true if any service field
// other than image and the four OIDC library-only fields differs.
func serviceStructuralDiffExcludingOIDCLibrary(oldDef, newDef *api.AppDefinition) bool {
	if len(oldDef.Services) != len(newDef.Services) {
		return true
	}
	stripOIDCLib := func(svc api.AppService) api.AppService {
		if svc.OIDCClient != nil {
			c := *svc.OIDCClient
			c.AuthorizePaths = nil
			c.RedirectURIPaths = nil
			c.RedirectURIs = nil
			svc.OIDCClient = &c
		}
		// Image diffs are reported separately by serviceImageDiff.
		svc.Image = ""
		return svc
	}
	for name, oldSvc := range oldDef.Services {
		newSvc, ok := newDef.Services[name]
		if !ok {
			return true
		}
		if !reflect.DeepEqual(stripOIDCLib(oldSvc), stripOIDCLib(newSvc)) {
			return true
		}
	}
	return false
}

// equalStringSliceUnordered reports whether a and b contain the same set of
// strings ignoring order. Used for OIDC library-only diff detection where
// the catalog spec treats authorize_paths/redirect lists as sets.
func equalStringSliceUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := slices.Clone(a)
	bc := slices.Clone(b)
	slices.Sort(ac)
	slices.Sort(bc)
	return slices.Equal(ac, bc)
}

// syncManifestIfDrifted is the per-app sync entry point. Implements the
// flow described in the plan (Phase 5):
//  1. follower / unlock / mode / disabled / catalog source guards
//  2. fetch + hash compare
//  3. retry throttle
//  4. install state load (modern vs legacy)
//  5. init script drift check
//  6. re-render via pipeline (modern) or allowlist patch (legacy)
//  7. canonical diff + classify
//  8. backup + persist new app.yaml
//  9. apply (live OIDC path or container recreate)
//  10. proxy OIDC client delta
//  11. failure → manifest-only rollback
//
// manual=true bypasses the per-app SyncDisabled flag. The auto-ticker
// always passes false; /sync/trigger and /sync/refresh-context pass true.
func (m *AppManager) syncManifestIfDrifted(ctx context.Context, instanceID string, manual bool, stagedSystemCtx *InstallSystemContext) error {
	host := m.currentSyncHost()
	if host == nil {
		return nil
	}

	if m.LastObservedRole(cluster.ResourceForApp(instanceID)) == cluster.RoleFollower {
		return nil
	}

	finishSync, err := m.beginSyncAttempt(instanceID)
	if err != nil {
		return err
	}
	defer finishSync()

	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	return m.syncManifestIfDriftedLocked(ctx, host, instanceID, manual, stagedSystemCtx)
}

func (m *AppManager) beginSyncAttempt(instanceID string) (func(), error) {
	m.syncStateMu.Lock()
	if m.syncInFlight[instanceID] {
		m.syncStateMu.Unlock()
		return nil, ErrSyncInProgress
	}
	m.syncInFlight[instanceID] = true
	m.syncStateMu.Unlock()

	return func() {
		m.syncStateMu.Lock()
		delete(m.syncInFlight, instanceID)
		m.syncStateMu.Unlock()
	}, nil
}

// syncManifestIfDriftedLocked performs sync while the caller owns reconcileMu
// and has registered the per-app sync attempt with beginSyncAttempt.
func (m *AppManager) syncManifestIfDriftedLocked(ctx context.Context, host SyncHost, instanceID string, manual bool, stagedSystemCtx *InstallSystemContext) error {
	if err := m.ensureUnlocked(); err != nil {
		return nil
	}

	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return nil
	}
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceCatalogSyncApply); err != nil {
		if errors.Is(err, ErrTransitionInProgress) && !manual {
			return nil
		}
		return err
	}
	if appInst.Definition == nil {
		return nil
	}
	curDef := appInst.Definition

	if appInst.Mode() != ModeService || appInst.CatalogSource == "" {
		return nil
	}
	if appInst.SyncDisabled && !manual {
		return nil
	}
	// Grace period: skip apps whose install completed less than installGracePeriod
	// ago. The install handler writes install_state.json AFTER releasing
	// reconcileMu, so a sync tick firing in that window would observe the
	// app without install_state.json and misclassify it as a legacy backfill.
	// The handler's RecordInstallState always finishes within milliseconds in
	// the normal case; the grace period is generous to cover any transient
	// I/O delay. Manual triggers (manual=true) bypass the grace period.
	if !manual && time.Since(appInst.CreatedAt) < installGracePeriod {
		return nil
	}
	// Disabled apps are intentionally stopped and may have stale container
	// IDs from a previous spec. Sync would either skip the recreate (and
	// leave the new app.yaml diverged from running state) or recreate the
	// containers (overriding the user's stopped intent). Either is wrong;
	// the safer model is to skip sync entirely for disabled apps. The
	// operator must re-enable to pick up catalog updates, at which point
	// the next sync tick processes them normally.
	if !appInst.Enabled {
		return nil
	}

	// Manual triggers clear throttle state under reconcileMu so a concurrent
	// auto-sync pass can't race on the cached AppInstance pointer's
	// LastSyncAttemptHash / LastSyncError fields. The clear is the only
	// way /sync/trigger can bypass a sticky failed-hash gate.
	if manual && (appInst.LastSyncAttemptHash != "" || appInst.LastSyncError != "") {
		appInst.LastSyncAttemptHash = ""
		appInst.LastSyncError = ""
		if err := state.StoreAppMetadata(appInst); err != nil {
			return fmt.Errorf("clear sync throttle: %w", err)
		}
	}

	// Fetch the catalog template.
	rawBytes, err := host.FetchCatalogTemplate(ctx, appInst.CatalogSource)
	if err != nil {
		return m.recordSyncFailure(state, appInst, "", fmt.Errorf("fetch catalog: %w", err))
	}
	newHash := Sha256Hex(rawBytes)

	// Auto-tick fast path: if the raw template hash matches and the caller
	// isn't a manual trigger, exit early. Manual triggers bypass this so
	// /sync/trigger and /sync/refresh-context (which mutates install_state
	// without changing the catalog hash) always reach the re-render path.
	if !manual && newHash == appInst.CatalogManifestHash {
		return nil
	}

	// Retry throttle: same hash already failed; wait for catalog to publish a
	// distinct hash or for an explicit /sync/trigger to clear the throttle.
	if newHash == appInst.LastSyncAttemptHash && appInst.LastSyncError != "" {
		return nil
	}

	// Track the in-flight attempt hash on the cached pointer so subsequent
	// failure paths persist it via recordSyncFailure. We deliberately do NOT
	// fsync the throttle hash here — the only reason to persist before
	// validation would be to throttle a process-crash retry loop, but the
	// drift detection itself is idempotent and a fresh pass after restart
	// will simply re-check the catalog hash. Avoiding the early write saves
	// one metadata.json write per drifted tick on the success path.
	appInst.LastSyncAttemptHash = newHash
	appInst.LastSyncError = ""

	// Load (or backfill) the install state.
	installSt, ierr := state.LoadInstallState(instanceID)
	switch {
	case errors.Is(ierr, ErrInstallStateNotFound):
		installSt = &InstallState{InstanceID: instanceID, IsLegacyBackfill: true}
		if err := state.StoreInstallState(instanceID, installSt); err != nil {
			log.Printf("WARN: catalog sync %s: backfill stub install_state.json: %v", instanceID, err)
		}
	case ierr != nil:
		return m.recordSyncFailure(state, appInst, newHash, fmt.Errorf("install_state.json malformed: %w", ierr))
	}

	renderInstallSt := installSt
	if stagedSystemCtx != nil && !installSt.IsLegacyBackfill && installSt.SchemaVersion >= installStateSchemaVersionConfig {
		cloned, err := installSt.Clone()
		if err != nil {
			return m.recordSyncFailure(state, appInst, newHash, err)
		}
		fresh := *stagedSystemCtx
		cloned.InstallSystemCtx = &fresh
		renderInstallSt = cloned
	}

	var newDef *api.AppDefinition
	var newCanonical []byte
	// Track newly-generated OIDC credentials so we can persist them via
	// CreateClient AFTER the rest of the sync flow commits. Only set when
	// the catalog adds oidc_client to a service that didn't previously
	// declare one.
	var freshOIDCCreds *OIDCCredentials
	if installSt.IsLegacyBackfill {
		patched, perr := m.legacyAllowlistPatch(curDef, rawBytes)
		if perr != nil {
			return m.recordSyncFailure(state, appInst, newHash, perr)
		}
		newDef = patched
		canon, err := SerializeAppDefinition(newDef)
		if err != nil {
			return m.recordSyncFailure(state, appInst, newHash, fmt.Errorf("canonicalize patched: %w", err))
		}
		newCanonical = canon
	} else {
		// Modern path: full re-render via the install pipeline.
		if renderInstallSt.InstallSystemCtx == nil {
			return m.recordSyncFailure(state, appInst, newHash, errors.New("modern install missing install_system_context"))
		}
		renderInputs := renderInstallSt.InstallInputs
		if effectiveInputs, ok := catalogSyncRenderInputsForRawTemplate(instanceID, rawBytes, renderInstallSt.InstallInputs); ok {
			renderInputs = effectiveInputs
		}
		res, perr := RunInstallPipeline(ctx, InstallPipelineInput{
			RawTemplate:   rawBytes,
			UserInputs:    renderInputs,
			SystemContext: *renderInstallSt.InstallSystemCtx,
			InstanceID:    instanceID,
			ExistingOIDC:  renderInstallSt.OIDCCredentials,
		}, host.OIDCClientGenerator(), m.syncSelfSkippingLister(instanceID))
		if perr != nil {
			if storeErr := m.storePendingCatalogConfigSource(state, instanceID, installSt, rawBytes, perr); storeErr != nil {
				log.Printf("WARN: catalog sync %s: store pending config source: %v", instanceID, storeErr)
			}
			return m.recordSyncFailure(state, appInst, newHash, perr)
		}
		// S1' invariant evaluation. Re-evaluate every sync — a previously
		// blocked install can become unblocked if the catalog moves the
		// secret back into env scope. Persist the fresh verdict so
		// /sync/status reflects current state.
		blockedNow := res.UsedSecretOnlyInInitScript
		blockedBefore := installSt.SyncBlocked
		if blockedNow != blockedBefore {
			blockedState, err := installSt.Clone()
			if err != nil {
				return m.recordSyncFailure(state, appInst, newHash, err)
			}
			blockedState.SyncBlocked = blockedNow
			if blockedNow {
				blockedState.SyncBlockedReason = "catalog uses oidc client_secret only in init_script scope; sync disabled to prevent silent rotation on container recreate"
			} else {
				blockedState.SyncBlockedReason = ""
			}
			if err := state.StoreInstallState(instanceID, blockedState); err != nil {
				log.Printf("WARN: catalog sync %s: persist sync_blocked verdict: %v", instanceID, err)
			}
			renderInstallSt.SyncBlocked = blockedState.SyncBlocked
			renderInstallSt.SyncBlockedReason = blockedState.SyncBlockedReason
		}
		if blockedNow {
			return m.recordSyncFailure(state, appInst, newHash, errors.New(renderInstallSt.SyncBlockedReason))
		}
		newDef = res.Definition
		newCanonical = res.CanonicalBytes
		// If the pipeline generated fresh credentials (catalog added an
		// oidc_client to a service that didn't have one before), capture
		// them for post-apply persistence.
		if renderInstallSt.OIDCCredentials == nil && res.OIDCCredentials != nil {
			freshOIDCCreds = res.OIDCCredentials
		}

		// Init script drift check (modern path). Runs against the RENDERED
		// definition so templated init_script.file paths resolve correctly.
		//
		// Three failure modes block sync:
		//   1. Catalog ADDED an init_script to a service that didn't have one
		//      at install time. installContainerGroup only runs init scripts
		//      whose FileContent was fetched at install; sync's
		//      recreateContainersInPlace would commit the new manifest
		//      without ever executing the new initialization. → reinstall.
		//   2. Init script content (file bytes) drifted. Same reason.
		//   3. Init script config (env/shell/timeout/ready_timeout) drifted.
		//      Those fields shape the one-shot run; recreate would skip
		//      them, leaving the app in the old initialized state.
		//
		// Any error (fetch failure, hash mismatch, etc.) blocks sync — we
		// cannot safely recreate containers without proving the one-shot
		// init scripts haven't drifted.
		drifted, dreason, derr := m.initScriptDrift(ctx, host, appInst.CatalogSource, curDef, newDef, appInst.InitScriptHashes)
		if derr != nil {
			return m.recordSyncFailure(state, appInst, newHash, fmt.Errorf("init script verification failed: %w", derr))
		}
		if drifted {
			return m.recordSyncFailure(state, appInst, newHash, fmt.Errorf("init script %s; manual reinstall required", dreason))
		}
	}

	// Compute canonical bytes for the existing definition (post-defaults).
	SetDefaults(curDef)
	oldCanonical, err := SerializeAppDefinition(curDef)
	if err != nil {
		return m.recordSyncFailure(state, appInst, newHash, fmt.Errorf("canonicalize current: %w", err))
	}
	if bytes.Equal(oldCanonical, newCanonical) {
		// Modern path: a true comment/whitespace-only catalog change. Bump
		// the hash so we don't re-evaluate this version on every tick.
		// Legacy path: the patched output equalling curDef does NOT mean the
		// catalog change was a no-op — it means the catalog changed
		// non-allowlisted fields (like x-piccolo or env) that the legacy
		// patcher intentionally ignores. Recording the new hash here would
		// silently mark the manifest "applied" when nothing was actually
		// applied; the operator must reinstall to pick up the change.
		if installSt.IsLegacyBackfill {
			return m.recordSyncFailure(state, appInst, newHash, errors.New("legacy install: catalog changes are not allowlisted, manual reinstall required"))
		}
		renderInstallSt.markCatalogSourceCommitted(instanceID, appInst.CatalogSource, rawBytes)
		if err := state.StoreInstallState(instanceID, renderInstallSt); err != nil {
			return m.recordSyncFailure(state, appInst, newHash, fmt.Errorf("persist config ledger source: %w", err))
		}
		prevHash := appInst.CatalogManifestHash
		appInst.CatalogManifestHash = newHash
		appInst.LastSyncError = ""
		if err := state.StoreAppMetadata(appInst); err != nil {
			appInst.CatalogManifestHash = prevHash
			return fmt.Errorf("persist hash bump: %w", err)
		}
		return nil
	}

	diffKind := classifyDiff(curDef, newDef)
	requiresPrecommitSnapshot := false
	if diffKind != DiffKindOIDCLibraryOnly {
		updatePolicy, _ := evaluateCustomManifestUpdatePolicy(curDef, newDef)
		requiresPrecommitSnapshot = updatePolicy.Classification.DataSafety != nil && updatePolicy.Classification.DataSafety.SnapshotRequired
		if !updatePolicy.Stageable {
			reason := strings.TrimSpace(updatePolicy.Reason)
			if reason == "" {
				reason = "update rejected by service app update policy"
			} else {
				reason = "update rejected by service app update policy: " + reason
			}
			return m.recordSyncFailure(state, appInst, newHash, errors.New(reason))
		}
		if !updatePolicy.Allowed {
			reason := strings.TrimSpace(updatePolicy.Reason)
			if reason == "" {
				reason = "update requires operator review"
			} else {
				reason = "update requires operator review: " + reason
			}
			if err := m.storePendingRenderedCatalogManifestReviewSource(state, instanceID, installSt, rawBytes, errors.New(reason)); err != nil {
				log.Printf("WARN: catalog sync %s: store pending service-app update source: %v", instanceID, err)
			}
			return m.recordSyncFailure(state, appInst, newHash, errors.New(reason))
		}
	}

	switch diffKind {
	case DiffKindNone:
		renderInstallSt.markCatalogSourceCommitted(instanceID, appInst.CatalogSource, rawBytes)
		if err := state.StoreInstallState(instanceID, renderInstallSt); err != nil {
			return m.recordSyncFailure(state, appInst, newHash, fmt.Errorf("persist config ledger source: %w", err))
		}
		prevHash := appInst.CatalogManifestHash
		appInst.CatalogManifestHash = newHash
		appInst.LastSyncError = ""
		if err := state.StoreAppMetadata(appInst); err != nil {
			appInst.CatalogManifestHash = prevHash
			return err
		}
		return nil
	case DiffKindImageOnly, DiffKindStructuralWithImage:
		reason := fmt.Errorf("update requires operator review: catalog %s includes image changes", diffKind)
		if err := m.storePendingRenderedCatalogManifestReviewSource(state, instanceID, installSt, rawBytes, reason); err != nil {
			log.Printf("WARN: catalog sync %s: store pending catalog image update source: %v", instanceID, err)
		}
		return m.recordSyncFailure(state, appInst, newHash, reason)
	}

	prevDef := curDef
	prevManifestHash := Sha256Hex(oldCanonical)
	candidateDigest := Sha256Hex(newCanonical)
	runtimeFingerprint, err := manifestRuntimeFingerprint(appInst)
	if err != nil {
		return m.recordSyncFailure(state, appInst, newHash, fmt.Errorf("fingerprint runtime: %w", err))
	}

	nextInstallSt, err := renderInstallSt.Clone()
	if err != nil {
		return m.recordSyncFailure(state, appInst, newHash, err)
	}
	nextInstallSt.markCatalogSourceCommitted(instanceID, appInst.CatalogSource, rawBytes)
	if freshOIDCCreds != nil {
		nextInstallSt.OIDCCredentials = freshOIDCCreds
	}
	metadataOnly := diffKind == DiffKindOIDCLibraryOnly
	legacyTransitionActive, err := transitionLegacyJournalExists(state, instanceID)
	if err != nil {
		return m.recordSyncFailure(state, appInst, newHash, err)
	}
	transitionPlan, err := PlanInstalledAppTransition(TransitionPlanInput{
		OperationKind:           TransitionOperationCatalogAutoApply,
		SourceKind:              TransitionSourceCatalogRendered,
		Mode:                    appInst.Mode(),
		Enabled:                 appInst.Enabled,
		RuntimeChanging:         !metadataOnly,
		LegacyTransactionActive: legacyTransitionActive,
		BaseManifestHash:        prevManifestHash,
		CandidateManifestHash:   candidateDigest,
		LedgerRevision:          installSt.Revision,
		SourceHash:              newHash,
		Data: TransitionDataPolicy{
			SnapshotRequired:      requiresPrecommitSnapshot,
			CandidateMayTouchData: !metadataOnly,
		},
		Runtime: TransitionRuntimePolicy{
			RuntimeFingerprint:   runtimeFingerprint,
			PreviousActiveRootfs: cloneStringMap(appInst.ActiveRootfs),
			PrimaryService:       primaryServiceFor(newDef, appInst),
		},
		Access: TransitionAccessPolicy{
			PrepareRequired: !metadataOnly,
		},
	})
	if err != nil {
		return m.recordSyncFailure(state, appInst, newHash, err)
	}
	transitionPlanHash, err := transitionPlan.Hash()
	if err != nil {
		return m.recordSyncFailure(state, appInst, newHash, err)
	}
	applyTxn, err := m.beginInstalledAppApplyTransaction(ctx, state, installedAppApplyTransactionSpec{
		OperationKind:             "catalog_sync",
		TaskType:                  taskTypeUpdateManifest,
		RollbackPrefix:            "catalog sync rolled back",
		InstanceID:                instanceID,
		AppInst:                   appInst,
		PreviousDefinition:        prevDef,
		CandidateDefinition:       newDef,
		PreviousManifestHash:      prevManifestHash,
		CandidateManifestHash:     candidateDigest,
		PreviousLedgerRevision:    installSt.Revision,
		CandidateLedgerRevision:   nextInstallSt.Revision,
		PreviousLedgerSourceHash:  installSt.RawTemplateHash,
		CandidateLedgerSourceHash: nextInstallSt.RawTemplateHash,
		RuntimeFingerprint:        runtimeFingerprint,
		TransitionPlan:            *transitionPlan,
		TransitionPlanHash:        transitionPlanHash,
		MetadataOnly:              metadataOnly,
		RequiresPrecommitSnapshot: requiresPrecommitSnapshot,
		ApplyPhase:                taskPhaseApplyingManifest,
		ApplyMessage:              "Persisting catalog manifest",
		FinalizingMessage:         "Saving catalog config ledger",
	})
	if err != nil {
		return m.recordSyncFailure(state, appInst, newHash, err)
	}
	failApply := func(cause error) error {
		if rollbackErr := applyTxn.rollback(cause); rollbackErr != nil {
			m.setObservedStatus(instanceID, StatusError)
			return m.recordSyncFailure(state, appInst, newHash, rollbackErr)
		}
		return m.recordSyncFailure(state, appInst, newHash, fmt.Errorf("catalog sync rolled back: %w", cause))
	}
	// commitHash bumps CatalogManifestHash to newHash and persists it. Called
	// only on success paths after the apply (or no-op-write paths) commits.
	commitHash := func() error {
		return storeCommittedCatalogMetadata(state, appInst, newHash)
	}

	if err := applyTxn.persistCandidateManifest(); err != nil {
		return m.recordSyncFailure(state, appInst, newHash, err)
	}

	// If the catalog added oidc_client to a service that didn't have one,
	// register the freshly-generated credentials with the OIDC client manager
	// BEFORE recreating containers, so the new container's OIDC env vars
	// reference a real registered client. Failure to register here means
	// containers would come up with an unregistered client; abort using the
	// pre-apply rollback path (no containers have been touched yet).
	if freshOIDCCreds != nil {
		if err := applyTxn.markCreatedOIDCClient(freshOIDCCreds.ClientID); err != nil {
			return m.recordSyncFailure(state, appInst, newHash, err)
		}
		if err := host.PersistOIDCClient(ctx, freshOIDCCreds.ClientID, freshOIDCCreds.ClientSecret, instanceID); err != nil {
			return failApply(fmt.Errorf("persist new oidc client: %w", err))
		}
	}

	// Resource stewardship (D-9 ordering): apply slice policy updates BEFORE
	// container recreate. appInst.Definition is already newDef at this point,
	// so ReconcileAllSlicePolicies derives against the new shape. Runs across
	// all apps because a manifest change to this app may alter sibling elastic
	// shares; the reconcile mutex serializes against concurrent changes.
	m.ReconcileAllSlicePolicies()

	switch diffKind {
	case DiffKindOIDCLibraryOnly:
		m.configureOIDCAuthorizePaths(instanceID, newDef)
		if proxyOIDCDeltaRequired(host, prevDef, newDef) {
			if err := applyTxn.markProxyOIDCDeltaApplied(); err != nil {
				return m.recordSyncFailure(state, appInst, newHash, err)
			}
		}
		if err := m.applyProxyOIDCDelta(ctx, host, instanceID, prevDef, newDef); err != nil {
			return failApply(err)
		}
	case DiffKindStructuralNoImage:
		if err := applyTxn.recreateRuntimeIfNeeded(); err != nil {
			return m.recordSyncFailure(state, appInst, newHash, err)
		}
		if proxyOIDCDeltaRequired(host, prevDef, newDef) {
			if err := applyTxn.markProxyOIDCDeltaApplied(); err != nil {
				return m.recordSyncFailure(state, appInst, newHash, err)
			}
		}
		if err := m.applyProxyOIDCDelta(ctx, host, instanceID, prevDef, newDef); err != nil {
			return failApply(err)
		}
	default:
		return failApply(fmt.Errorf("unsupported sync diff kind %s", diffKind))
	}
	if err := applyTxn.commitLedger(nextInstallSt); err != nil {
		return m.recordSyncFailure(state, appInst, newHash, err)
	}
	var catalogMetadataErr error
	if err := commitHash(); err != nil {
		catalogMetadataErr = err
		log.Printf("WARN: catalog sync %s: committed catalog metadata pending retry: %v", instanceID, err)
	}
	if err := applyTxn.publishAccess(); err != nil {
		if catalogMetadataErr != nil {
			err = errors.Join(err, catalogMetadataErr)
		}
		return errors.New(applyTxn.markAccessRepairPending(err))
	}
	if catalogMetadataErr != nil {
		applyTxn.markCatalogMetadataPending(catalogMetadataErr)
		return nil
	}
	applyTxn.complete()
	log.Printf("INFO: catalog sync %s: applied %s", instanceID, diffKind)
	return nil
}

// recordSyncFailure persists LastSyncError + LastSyncAttemptHash on the app
// and returns the originating error wrapped for caller logging.
func (m *AppManager) recordSyncFailure(state *FilesystemStateManager, appInst *AppInstance, attemptHash string, cause error) error {
	if attemptHash != "" {
		appInst.LastSyncAttemptHash = attemptHash
	}
	appInst.LastSyncError = cause.Error()
	if err := state.StoreAppMetadata(appInst); err != nil {
		return fmt.Errorf("persist sync failure (%v): %w", cause, err)
	}
	return cause
}

func (m *AppManager) storePendingCatalogConfigSource(state *FilesystemStateManager, instanceID string, st *InstallState, raw []byte, cause error) error {
	if state == nil || st == nil || st.IsLegacyBackfill || st.SchemaVersion < installStateSchemaVersionConfig {
		return nil
	}
	if _, err := ParseAppSchema(raw); err != nil {
		return nil
	}
	return m.storePendingCatalogSourceForFlow(state, instanceID, st, raw, cause, pendingCatalogReviewFlowConfig)
}

// storePendingRenderedCatalogManifestReviewSource is for sync paths that already
// rendered and validated the candidate manifest. The raw template may include
// block directives that only become valid YAML after render.
func (m *AppManager) storePendingRenderedCatalogManifestReviewSource(state *FilesystemStateManager, instanceID string, st *InstallState, raw []byte, cause error) error {
	if state == nil || st == nil || st.IsLegacyBackfill || st.SchemaVersion < installStateSchemaVersionConfig {
		return nil
	}
	return m.storePendingCatalogSourceForFlow(state, instanceID, st, raw, cause, pendingCatalogReviewFlowManifest)
}

func (m *AppManager) storePendingCatalogSourceForFlow(state *FilesystemStateManager, instanceID string, st *InstallState, raw []byte, cause error, flow string) error {
	next, err := st.Clone()
	if err != nil {
		return err
	}
	reason := strings.TrimSpace(cause.Error())
	if reason == "" {
		reason = "catalog sync render failed"
	}
	if !next.markPendingCatalogSourceForFlow(instanceID, raw, reason, flow) {
		return nil
	}
	return state.StoreInstallState(instanceID, next)
}

// applySyncedDefinition routes a non-no-op diff to the right apply path:
// live OIDC update for library-only diffs, full container recreate for
// structural diffs.
func (m *AppManager) applySyncedDefinition(
	ctx context.Context,
	host SyncHost,
	instanceID string,
	appInst *AppInstance,
	newDef, prevDef *api.AppDefinition,
	diffKind DiffKind,
) error {
	switch diffKind {
	case DiffKindOIDCLibraryOnly:
		m.configureOIDCAuthorizePaths(instanceID, newDef)
		// Proxy OIDC client delta — only relevant if listener auth changed,
		// which it cannot in this branch (library-only excludes auth changes).
		// Still call the delta function so semantics stay consistent if the
		// classifier ever permits a borderline case.
		return m.applyProxyOIDCDelta(ctx, host, instanceID, prevDef, newDef)
	case DiffKindStructuralNoImage:
		if err := m.recreateContainersInPlace(ctx, instanceID, newDef, prevDef, appInst); err != nil {
			return err
		}
		return m.applyProxyOIDCDelta(ctx, host, instanceID, prevDef, newDef)
	default:
		return fmt.Errorf("applySyncedDefinition: unsupported diff kind %s", diffKind)
	}
}

// rollbackManifestOnly restores the previous app definition (prev.yaml on
// disk via state.GetPreviousAppDefinition) and recreates containers in
// place. Used by sync failure paths to undo a manifest-only change without
// touching LV state or data volumes.
//
// On `recreateContainersInPlace` failure, the in-memory cache is LEFT
// pointing at prevDef (matching the on-disk app.yaml after restore). The
// reconciler will then retry container creation with prevDef, which is the
// canonical truth. Restoring the cache to curDef (the failed-apply state)
// would cause cache/disk divergence and confuse subsequent reconcile passes.
func (m *AppManager) rollbackManifestOnly(
	ctx context.Context,
	host SyncHost,
	instanceID string,
	appInst *AppInstance,
	prevDef *api.AppDefinition,
) error {
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	if prevDef == nil {
		var perr error
		prevDef, perr = state.GetPreviousAppDefinition(instanceID)
		if perr != nil {
			return fmt.Errorf("load prev definition: %w", perr)
		}
	}
	curDef := appInst.Definition
	appInst.Definition = prevDef
	if err := state.StoreApp(appInst); err != nil {
		// Disk write failed — restore cache to curDef so cache and disk
		// stay aligned (disk still has the failed-apply newDef).
		appInst.Definition = curDef
		return fmt.Errorf("restore previous app.yaml: %w", err)
	}
	// removeDef is the failed-apply def (curDef) — that's what is currently
	// materialized as containers, not prevDef.
	if err := m.recreateContainersInPlace(ctx, instanceID, prevDef, curDef, appInst); err != nil {
		// Container recreate failed for the previous definition too. The
		// disk and cache both already hold prevDef; leave them that way so
		// the reconciler retries against the correct (rolled-back) state.
		return fmt.Errorf("recreate previous containers: %w", err)
	}
	if err := m.applyProxyOIDCDelta(ctx, host, instanceID, curDef, prevDef); err != nil {
		// Best-effort during rollback: containers are restored, just log
		// the proxy delta failure. The reconciler / next sync tick will
		// retry the proxy registration.
		log.Printf("WARN: catalog sync %s: rollback proxy oidc delta: %v", instanceID, err)
	}
	return nil
}

// recreateContainersInPlace removes the existing container group and
// recreates it from newDef, reusing the existing per-service ActiveRootfs LVs
// (no image pull). Shared by sync apply and sync rollback to keep their
// container manipulation logic in lockstep.
//
// removeDef is the definition currently materialized as containers — this is
// what removeContainersForMultiApp iterates to find which containers to stop
// and remove. For sync apply, removeDef is the previous (pre-sync) def; for
// rollback, removeDef is the failed-apply def. Passing newDef here would
// orphan any service that the catalog removed.
//
// Listener reconciliation is prepared before destructive runtime work so
// container port bindings are stable, but proxy/firewall/registry publication
// is deferred until the replacement container IDs have been persisted.
func (m *AppManager) recreateContainersInPlace(
	ctx context.Context,
	instanceID string,
	newDef *api.AppDefinition,
	removeDef *api.AppDefinition,
	appInst *AppInstance,
) error {
	return m.recreateContainersInPlaceWithHookAndPublicationResumeToken(ctx, instanceID, newDef, removeDef, appInst, nil, services.PublicationResumeToken{})
}

func (m *AppManager) recreateContainersInPlaceWithHook(
	ctx context.Context,
	instanceID string,
	newDef *api.AppDefinition,
	removeDef *api.AppDefinition,
	appInst *AppInstance,
	beforeInstall func() error,
) error {
	return m.recreateContainersInPlaceWithHookAndPublicationResumeToken(ctx, instanceID, newDef, removeDef, appInst, beforeInstall, services.PublicationResumeToken{})
}

func (m *AppManager) recreateContainersInPlaceWithHookAndPublicationResumeToken(
	ctx context.Context,
	instanceID string,
	newDef *api.AppDefinition,
	removeDef *api.AppDefinition,
	appInst *AppInstance,
	beforeInstall func() error,
	publicationResumeToken services.PublicationResumeToken,
) error {
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("ensure layout: %w", err)
	}
	mode := piccoloModeFromExtensions(newDef.Extensions)
	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeEnsureReady)
	if err != nil {
		return fmt.Errorf("podman runtime: %w", err)
	}

	// Reserve the complete replacement endpoint set before removing the
	// current runtime. Preparation does not alter registry/proxy/firewall state,
	// so a suspended app remains suspended until the replacement runtime and
	// its durable container IDs are ready.
	listenerPlan, err := m.serviceManager.PrepareReconcile(instanceID, newDef.Listeners)
	if err != nil {
		return fmt.Errorf("prepare listeners: %w", err)
	}
	defer listenerPlan.Release()
	endpoints := listenerPlan.Endpoints()
	if len(endpoints) == 0 && len(newDef.Listeners) > 0 && !allowMissingListenerEndpointsForTest() {
		return fmt.Errorf("prepare listeners: no endpoints for %d listeners", len(newDef.Listeners))
	}

	if err := m.removeContainersForMultiApp(ctx, appInst, removeDef, runtime); err != nil {
		return fmt.Errorf("remove previous containers: %w", err)
	}
	m.configureOIDCAuthorizePaths(instanceID, newDef)
	if beforeInstall != nil {
		if err := beforeInstall(); err != nil {
			return err
		}
	}

	var prebuiltRootfs map[string]*rootfsMountInfo
	if rootfs := m.currentRootfsManager(); rootfs != nil {
		prebuiltRootfs, err = m.ensureAllServiceRootfsAttached(ctx, instanceID, mode, newDef, appInst)
		if err != nil {
			return fmt.Errorf("attach existing rootfs: %w", err)
		}
	}
	result, err := m.installContainerGroup(ctx, newDef, instanceID, layout, runtime, endpoints, prebuiltRootfs)
	if err != nil {
		return fmt.Errorf("install container group: %w", err)
	}
	appInst.NetworkAnchorID = result.NetworkAnchorID
	appInst.Containers = result.Containers
	appInst.PrimaryService = result.PrimaryService
	appInst.ActiveRootfs = activeRootfsForDefinition(appInst.ActiveRootfs, newDef)
	if err := state.StoreApp(appInst); err != nil {
		return fmt.Errorf("persist container ids: %w", err)
	}
	publicationCtx := pressure.WithTransitionContinuation(ctx)
	if _, _, err := listenerPlan.PublishWithResumeTokenContext(publicationCtx, publicationResumeToken); err != nil {
		return fmt.Errorf("publish listeners: %w", err)
	}
	if appInst.Enabled {
		m.setObservedStatus(instanceID, StatusRunning)
	}
	return nil
}

// applyProxyOIDCDelta is the B6'/RISK-4 fix: directly call register or delete
// based on whether the proxy client requirement flipped, mirroring the
// UpdateListeners pattern at gin_app_handlers.go:893–903.
//
// Returns an error if proxy client registration fails — the caller should
// treat this as a sync apply failure and trigger rollback. Silently logging
// the error would leave the listener requiring auth without a proxy client
// to issue tokens, breaking auth in a way that's invisible to the operator.
func (m *AppManager) applyProxyOIDCDelta(ctx context.Context, host SyncHost, instanceID string, oldDef, newDef *api.AppDefinition) error {
	if host == nil {
		return nil
	}
	wasRequired := host.RequiresProxyOIDCClient(oldDef)
	isRequired := host.RequiresProxyOIDCClient(newDef)
	switch {
	case isRequired && !wasRequired:
		if err := host.RegisterProxyOIDCClient(ctx, instanceID); err != nil {
			return fmt.Errorf("register proxy oidc client: %w", err)
		}
	case !isRequired && wasRequired:
		host.DeleteProxyOIDCClient(ctx, instanceID)
	}
	return nil
}

func proxyOIDCDeltaRequired(host SyncHost, oldDef, newDef *api.AppDefinition) bool {
	if host == nil {
		return false
	}
	return host.RequiresProxyOIDCClient(oldDef) != host.RequiresProxyOIDCClient(newDef)
}

// initScriptDrift compares init_script declarations between the current
// installed definition and the new rendered definition, returning (drifted,
// reason, fetchErr). Drift is reported when:
//
//   - The new def adds an init_script to a service that previously had none
//     (sync's recreate path can't fetch+execute it).
//   - The new def removes an init_script that previously ran (functionally
//     fine but operator should know).
//   - Any init_script config field (Env, Shell, Timeout, ReadyTimeout)
//     differs from the previous def (those fields shape the one-shot run
//     and aren't replayed on container recreate).
//   - The fetched script bytes hash differs from the persisted install hash.
//
// fetchErr propagates network failures so the caller can fail closed (any
// transient error blocks sync rather than silently allowing drift).
func (m *AppManager) initScriptDrift(
	ctx context.Context,
	host SyncHost,
	catalogSource string,
	curDef *api.AppDefinition,
	renderedDef *api.AppDefinition,
	persistedHashes map[string]string,
) (drifted bool, reason string, err error) {
	curScripts := map[string]*api.ServiceInitScript{}
	if curDef != nil {
		for name, svc := range curDef.Services {
			if svc.InitScript != nil {
				curScripts[name] = svc.InitScript
			}
		}
	}
	newScripts := map[string]*api.ServiceInitScript{}
	if renderedDef != nil {
		for name, svc := range renderedDef.Services {
			if svc.InitScript != nil {
				newScripts[name] = svc.InitScript
			}
		}
	}

	for svcName, newScript := range newScripts {
		curScript, hadBefore := curScripts[svcName]
		if !hadBefore {
			return true, fmt.Sprintf("added to service '%s' (sync cannot replay newly-added init scripts)", svcName), nil
		}
		// Compare config fields. Use reflect.DeepEqual on the env map, plus
		// per-field comparison for the scalars. FileContent is install-time
		// only and never set on tmplDef parses, so we ignore it here.
		if !reflect.DeepEqual(curScript.Env, newScript.Env) ||
			curScript.Shell != newScript.Shell ||
			curScript.Timeout != newScript.Timeout ||
			curScript.ReadyTimeout != newScript.ReadyTimeout ||
			curScript.File != newScript.File {
			return true, fmt.Sprintf("config changed for service '%s'", svcName), nil
		}
	}
	for svcName := range curScripts {
		if _, stillThere := newScripts[svcName]; !stillThere {
			return true, fmt.Sprintf("removed from service '%s'", svcName), nil
		}
	}

	// Hash compare against persisted install-time hashes.
	for svcName, newScript := range newScripts {
		content, ferr := host.FetchInitScript(ctx, catalogSource, newScript.File)
		if ferr != nil {
			return false, "", fmt.Errorf("fetch init script %s: %w", svcName, ferr)
		}
		newHash := Sha256Hex(content)
		// If the install never persisted a hash for this script (legacy
		// install or pre-sync upgrade), we cannot prove non-drift; treat
		// as drifted to be safe.
		persisted, ok := persistedHashes[svcName]
		if !ok {
			return true, fmt.Sprintf("no install-time hash recorded for service '%s'", svcName), nil
		}
		if persisted != newHash {
			return true, fmt.Sprintf("content changed for service '%s'", svcName), nil
		}
	}

	return false, "", nil
}

// legacyAllowlistPatch is the Phase 7 fallback for legacy installs (no
// install_state.json). It cannot re-render the catalog template because the
// install inputs and OIDC credentials were never persisted. Instead it walks
// ONLY allowlisted fields in the (literal, unrendered) tmplDef and copies
// them onto a clone of curDef.
//
// Critical: the comparison must NOT walk the full struct because tmplDef
// fields like services[*].environment.X = "{{ .System.Auth.ClientID }}"
// are template literals while curDef contains the rendered runtime value.
// A full-struct compare would always reject the patch and break the
// motivating use case (legacy Immich picks up authorize_paths).
//
// Service-set changes (add/remove/rename) still short-circuit to skip
// because we cannot safely allocate ports / OIDC clients without
// install_state.json. Listener-set changes likewise skip.
//
// Allowlisted fields:
//   - services[*].oidc_client.{authorize_paths, redirect_uri_paths, redirect_uris}
//   - listeners[*].protocol_middleware (matched by listener name, not index)
//   - extensions.* (top-level catalog metadata)
func (m *AppManager) legacyAllowlistPatch(curDef *api.AppDefinition, rawBytes []byte) (*api.AppDefinition, error) {
	tmplDef, err := ParseAppSchema(rawBytes)
	if err != nil {
		return nil, fmt.Errorf("legacy: parse template: %w", err)
	}

	// Service-set rule: any add/remove/rename → skip.
	if len(curDef.Services) != len(tmplDef.Services) {
		return nil, errors.New("legacy install: catalog changed service set, manual reinstall required")
	}
	for name := range curDef.Services {
		if _, ok := tmplDef.Services[name]; !ok {
			return nil, errors.New("legacy install: catalog renamed or removed a service, manual reinstall required")
		}
	}
	// Listener-set rule: name-based matching, any add/remove/rename → skip.
	if len(curDef.Listeners) != len(tmplDef.Listeners) {
		return nil, errors.New("legacy install: catalog changed listener count, manual reinstall required")
	}
	tmplListenerByName := make(map[string]api.AppListener, len(tmplDef.Listeners))
	for _, l := range tmplDef.Listeners {
		tmplListenerByName[l.Name] = l
	}
	// Note: tmplDef listeners may carry the __primary marker name while
	// curDef listeners carry the substituted name. The plan accepts that
	// legacy installs cannot detect listener-name drift. Match by index
	// when the only listener-name diff is __primary substitution.
	for _, l := range curDef.Listeners {
		if _, ok := tmplListenerByName[l.Name]; ok {
			continue
		}
		// Allow primary substitution: tmplDef may declare __primary while
		// curDef has the substituted name. If tmplDef has any __primary
		// listener, treat that as the match for curDef's primary listener.
		if l.Primary {
			continue
		}
		return nil, fmt.Errorf("legacy install: listener '%s' not present in catalog, manual reinstall required", l.Name)
	}

	patched := *curDef
	patched.Services = make(map[string]api.AppService, len(curDef.Services))
	for name, oldSvc := range curDef.Services {
		tmplSvc := tmplDef.Services[name]
		merged := oldSvc

		// Apply allowlisted OIDC field deltas.
		switch {
		case tmplSvc.OIDCClient == nil && oldSvc.OIDCClient == nil:
			// no-op
		case tmplSvc.OIDCClient == nil && oldSvc.OIDCClient != nil:
			// Catalog dropped oidc_client entirely. Don't drop on the legacy
			// path — leave existing OIDC settings intact and skip this app
			// to avoid surprising auth removal.
			return nil, fmt.Errorf("legacy install: service '%s' lost oidc_client in catalog, manual reinstall required", name)
		case tmplSvc.OIDCClient != nil && oldSvc.OIDCClient == nil:
			// Catalog added oidc_client to a service that didn't have one.
			// Cannot safely seed credentials on the legacy path.
			return nil, fmt.Errorf("legacy install: service '%s' gained oidc_client in catalog, manual reinstall required", name)
		default:
			c := *oldSvc.OIDCClient
			c.AuthorizePaths = append([]string(nil), tmplSvc.OIDCClient.AuthorizePaths...)
			c.RedirectURIPaths = append([]string(nil), tmplSvc.OIDCClient.RedirectURIPaths...)
			c.RedirectURIs = append([]string(nil), tmplSvc.OIDCClient.RedirectURIs...)
			merged.OIDCClient = &c
		}

		patched.Services[name] = merged
	}

	// Patch listener middleware by name (or by primary marker).
	patched.Listeners = make([]api.AppListener, len(curDef.Listeners))
	for i, oldL := range curDef.Listeners {
		patched.Listeners[i] = oldL
		var tmplL api.AppListener
		if t, ok := tmplListenerByName[oldL.Name]; ok {
			tmplL = t
		} else if oldL.Primary {
			// Find the __primary marker in tmplDef.
			for _, t := range tmplDef.Listeners {
				if t.Name == "__primary" {
					tmplL = t
					break
				}
			}
		} else {
			continue
		}
		patched.Listeners[i].Middleware = append([]api.AppProtocolMiddleware(nil), tmplL.Middleware...)
	}

	// Extensions (the x-piccolo block) is NOT allowlisted on the legacy
	// path. Keys like mode, tmpfs, app_config affect runtime layout and
	// changing them automatically would defeat the legacy-patch safety
	// guarantee. Leave the existing Extensions in place; if the catalog
	// changed an x-piccolo key, the operator must reinstall.

	// Re-apply defaults so the patched def is canonicalized.
	SetDefaults(&patched)
	if err := ValidateAppDefinition(&patched); err != nil {
		return nil, fmt.Errorf("legacy: validate patched: %w", err)
	}
	return &patched, nil
}

// appListerFunc adapts a function to the AppLister interface.
type appListerFunc func(context.Context) ([]*AppInstance, error)

func (f appListerFunc) List(ctx context.Context) ([]*AppInstance, error) { return f(ctx) }

// syncSelfSkippingLister returns an AppLister that filters out the app being
// synced from the install pipeline's primary-name uniqueness check.
func (m *AppManager) syncSelfSkippingLister(skipInstanceID string) AppLister {
	return appListerFunc(func(ctx context.Context) ([]*AppInstance, error) {
		apps, err := m.List(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]*AppInstance, 0, len(apps))
		for _, a := range apps {
			if a.InstanceID == skipInstanceID {
				continue
			}
			out = append(out, a)
		}
		return out, nil
	})
}
