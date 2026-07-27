package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/fsutil"
	"piccolod/internal/persistence"
)

const (
	defaultHuggingFaceEndpoint = "https://huggingface.co"
	maxHuggingFaceMetadata     = 32 << 20
	huggingFaceMetadataTimeout = 2 * time.Minute
	huggingFaceTransferStall   = 2 * time.Minute
	huggingFaceAttemptGrace    = 10 * time.Minute
	huggingFaceMinBytesPerSec  = 256 << 10
	huggingFaceMaxAttempt      = 48 * time.Hour
	artifactProgressHeartbeat  = 30 * time.Second
)

// ArtifactOperationTimeout is the outer lifecycle ceiling required to allow
// the longest supported materialization attempt plus bounded finalization.
const ArtifactOperationTimeout = huggingFaceMaxAttempt + time.Hour

// ociArtifactRuntime is an optional extension implemented by the production
// Podman adapter. Keeping it separate avoids widening every image-only
// ContainerManager test double.
type ociArtifactRuntime interface {
	PullArtifactWithProgress(
		ctx context.Context,
		runtime container.PodmanRuntime,
		reference string,
		callback container.ImagePullCallback,
	) error
	InspectArtifact(ctx context.Context, runtime container.PodmanRuntime, reference string) (*container.ArtifactInfo, error)
	ExtractArtifact(ctx context.Context, runtime container.PodmanRuntime, reference, targetDir string) error
}

type huggingFaceFile struct {
	Path string
	Size int64
}

type resolvedHuggingFaceSource struct {
	Commit       string
	Projection   string
	SelectedFile bool
	Files        []huggingFaceFile
	Size         int64
}

type artifactTransferProgressKey struct{}

type artifactTransferProgressFunc func(downloaded, total int64)

func reportArtifactTransferProgress(ctx context.Context, downloaded, total int64) {
	report, _ := ctx.Value(artifactTransferProgressKey{}).(artifactTransferProgressFunc)
	if report != nil {
		report(downloaded, total)
	}
}

func artifactTransferCallback(ctx context.Context) container.ImagePullCallback {
	return func(report container.ImagePullReport) {
		// Podman emits an initial report before it knows blob sizes. Keep the
		// lifecycle at its assigned minimum until byte progress is measurable.
		// Like an ordinary image pull, completed transport may reach the
		// content unit's range maximum; stage metadata distinguishes subsequent
		// materialization from the final Ready event.
		if report.TotalBytes <= 0 {
			return
		}
		reportArtifactTransferProgress(ctx, report.DownloadedBytes, report.TotalBytes)
	}
}

func artifactTransferPercent(downloaded, total int64) int {
	if total <= 0 {
		return -1
	}
	if downloaded <= 0 {
		return 0
	}
	if downloaded >= total {
		return 100
	}
	return int((float64(downloaded) / float64(total)) * 100)
}

func (m *AppManager) currentGoldenContentManager() persistence.GoldenContentManager {
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return nil
	}
	manager, _ := rootfs.(persistence.GoldenContentManager)
	return manager
}

type preparedArtifactAttachments struct {
	Handles       map[string]persistence.ArtifactHandle
	References    map[string]string
	CreatedByCall []string
}

func artifactReferenceID(instanceID, artifactName, goldenID string) string {
	sum := sha256.Sum256([]byte(
		"piccolo-artifact-reference-v1\x00" +
			instanceID + "\x00" +
			artifactName + "\x00" +
			goldenID,
	))
	return instanceID + "--artifact--" + hex.EncodeToString(sum[:])
}

func idMapForRuntime(instanceID string, runtime container.PodmanRuntime) persistence.IDMapConfig {
	var idmap persistence.IDMapConfig
	if runtime.Credential == nil {
		return idmap
	}
	idmap.AppUID = runtime.Credential.Uid
	idmap.AppGID = runtime.Credential.Gid
	username := container.AppUsername(instanceID)
	if subStart, subCount, err := container.LookupSubUIDRange(username); err == nil {
		idmap.SubUIDStart = subStart
		idmap.SubUIDCount = subCount
		idmap.SubGIDStart = subStart
		idmap.SubGIDCount = subCount
	}
	return idmap
}

func (m *AppManager) beginArtifactProgress(
	ctx context.Context,
	instanceID, artifactName, sourceKind string,
	index, total int,
	heartbeat time.Duration,
	progressRange transferProgressRange,
) (context.Context, func(error)) {
	if TaskIDFromContext(ctx) == "" || m.currentProgressReporter() == nil {
		return ctx, func(error) {}
	}
	taskType, inheritedProgress := m.inheritedTaskProgress(
		ctx,
		taskTypeInstallApp,
		progressRange.Min,
	)
	if inheritedProgress > progressRange.Min {
		progressRange.Min = inheritedProgress
	}
	if progressRange.Max < progressRange.Min {
		progressRange.Max = progressRange.Min
	}
	started := time.Now()

	var byteHighWater transferByteHighWater
	var lastPercent atomic.Int64
	var lifecycleProgress atomic.Int64
	var emitMu sync.Mutex
	lastPercent.Store(-2)
	lifecycleProgress.Store(int64(progressRange.Min))

	emitLocked := func(stage, message string, opErr error) {
		downloaded, totalSize := byteHighWater.snapshot()
		overallPercent := artifactTransferPercent(downloaded, totalSize)
		m.emitProgressWithMetadata(
			ctx,
			taskType,
			instanceID,
			taskPhaseMaterializingArtifact,
			int(lifecycleProgress.Load()),
			message,
			false,
			map[string]any{
				"artifact":         artifactName,
				"artifact_index":   index,
				"artifact_total":   total,
				"source_type":      sourceKind,
				"stage":            stage,
				"elapsed_seconds":  int64(time.Since(started) / time.Second),
				"overall_percent":  overallPercent,
				"total_bytes":      totalSize,
				"downloaded_bytes": downloaded,
			},
			opErr,
		)
	}
	emit := func(stage, message string, opErr error) {
		emitMu.Lock()
		defer emitMu.Unlock()
		emitLocked(stage, message, opErr)
	}
	emit(
		"preparing",
		fmt.Sprintf("Preparing artifact %s (%d/%d)", artifactName, index, total),
		nil,
	)
	downloadMessage := func(downloaded, totalSize int64, heartbeat bool) string {
		elapsed := int64(time.Since(started) / time.Second)
		if totalSize <= 0 {
			if heartbeat {
				if downloaded > 0 {
					return fmt.Sprintf(
						"Still processing artifact %s: %s downloaded (%d/%d, %ds elapsed)",
						artifactName,
						formatBytes(downloaded),
						index,
						total,
						elapsed,
					)
				}
				return fmt.Sprintf(
					"Still processing artifact %s (%d/%d, %ds elapsed)",
					artifactName,
					index,
					total,
					elapsed,
				)
			}
			return fmt.Sprintf(
				"Pulling artifact %s (%d/%d)",
				artifactName,
				index,
				total,
			)
		}
		verb := "Pulling"
		if heartbeat {
			verb = "Still pulling"
		}
		return fmt.Sprintf(
			"%s artifact %s: %s / %s (%d/%d, %ds elapsed)",
			verb,
			artifactName,
			formatBytes(downloaded),
			formatBytes(totalSize),
			index,
			total,
			elapsed,
		)
	}

	transferProgress := artifactTransferProgressFunc(func(downloaded, totalSize int64) {
		emitMu.Lock()
		defer emitMu.Unlock()

		if downloaded < 0 {
			downloaded = 0
		}
		if totalSize < 0 {
			totalSize = 0
		}
		if totalSize > 0 && downloaded > totalSize {
			downloaded = totalSize
		}
		downloaded, totalSize = byteHighWater.observe(downloaded, totalSize)
		percent := artifactTransferPercent(downloaded, totalSize)
		if lastPercent.Swap(int64(percent)) == int64(percent) {
			return
		}
		mapped := int64(mapTransferProgress(progressRange, percent))
		for {
			current := lifecycleProgress.Load()
			if mapped <= current || lifecycleProgress.CompareAndSwap(current, mapped) {
				break
			}
		}

		emitLocked("downloading", downloadMessage(downloaded, totalSize, false), nil)
	})
	progressCtx := context.WithValue(ctx, artifactTransferProgressKey{}, transferProgress)

	if heartbeat <= 0 {
		heartbeat = artifactProgressHeartbeat
	}
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				func() {
					emitMu.Lock()
					defer emitMu.Unlock()

					downloaded, totalSize := byteHighWater.snapshot()
					if totalSize > 0 && downloaded < totalSize {
						emitLocked("downloading", downloadMessage(downloaded, totalSize, true), nil)
						return
					}
					if totalSize <= 0 {
						emitLocked("processing", downloadMessage(downloaded, totalSize, true), nil)
						return
					}
					emitLocked(
						"materializing",
						fmt.Sprintf(
							"Materializing artifact %s (%d/%d, %ds elapsed)",
							artifactName,
							index,
							total,
							int64(time.Since(started)/time.Second),
						),
						nil,
					)
				}()
			case <-stop:
				return
			}
		}
	}()

	var once sync.Once
	return progressCtx, func(opErr error) {
		once.Do(func() {
			close(stop)
			<-stopped
			if opErr != nil {
				emit(
					"failed",
					fmt.Sprintf("Artifact %s failed (%d/%d)", artifactName, index, total),
					opErr,
				)
				return
			}
			lifecycleProgress.Store(int64(progressRange.Max))
			emit(
				"ready",
				fmt.Sprintf("Artifact %s ready (%d/%d)", artifactName, index, total),
				nil,
			)
		})
	}
}

// prepareArtifactAttachments either reattaches the exact references recorded
// by the currently committed, semantically identical artifact declaration or
// resolves/materializes a new candidate set. New references remain candidate
// owned until the caller persists the returned map in AppInstance.
func (m *AppManager) prepareArtifactAttachments(
	ctx context.Context,
	def *api.AppDefinition,
	instanceID string,
	idmap persistence.IDMapConfig,
	reuseRecorded bool,
	progressRange transferProgressRange,
) (preparedArtifactAttachments, error) {
	result := preparedArtifactAttachments{
		Handles:    make(map[string]persistence.ArtifactHandle),
		References: make(map[string]string),
	}
	if def == nil || len(def.Artifacts) == 0 {
		return result, nil
	}
	manager := m.currentGoldenContentManager()
	if manager == nil {
		return result, fmt.Errorf("artifact bindings require generic golden-content support")
	}

	var recorded map[string]string
	if reuseRecorded {
		state, err := m.ensureStateManager()
		if err != nil {
			return preparedArtifactAttachments{}, err
		}
		if current, ok := state.GetApp(instanceID); ok &&
			current.Definition != nil &&
			reflect.DeepEqual(current.Definition.Artifacts, def.Artifacts) {
			recorded = current.ArtifactReferences
		}
	}

	names := make([]string, 0, len(def.Artifacts))
	for name := range def.Artifacts {
		names = append(names, name)
	}
	sort.Strings(names)
	cleanupCreated := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupBudget)
		defer cancel()
		for i := len(result.CreatedByCall) - 1; i >= 0; i-- {
			_ = manager.DestroyArtifactReference(cleanupCtx, result.CreatedByCall[i])
		}
	}
	for index, name := range names {
		source := def.Artifacts[name].Source
		unitProgressRange := progressRangeForUnit(
			progressRange.Min,
			progressRange.Max,
			index,
			len(names),
		)
		progressCtx, finishProgress := m.beginArtifactProgress(
			ctx,
			instanceID,
			name,
			source.Type,
			index+1,
			len(names),
			artifactProgressHeartbeat,
			unitProgressRange,
		)
		prepareErr := func() error {
			if referenceID := recorded[name]; referenceID != "" {
				handle, err := manager.AttachArtifactReference(progressCtx, referenceID)
				var missing *persistence.ArtifactContentMissingError
				if errors.As(err, &missing) {
					if rebuildErr := m.reconstructRecordedArtifactContent(
						progressCtx,
						source,
						missing,
					); rebuildErr != nil {
						return fmt.Errorf(
							"reconstruct recorded artifact %q: %w",
							name,
							rebuildErr,
						)
					}
					handle, err = manager.AttachArtifactReference(progressCtx, referenceID)
				}
				if err != nil {
					return fmt.Errorf("attach recorded artifact %q: %w", name, err)
				}
				result.Handles[name] = handle
				result.References[name] = referenceID
				return nil
			}

			content, err := m.ensureArtifactContent(progressCtx, source)
			if err != nil {
				return fmt.Errorf("materialize artifact %q: %w", name, err)
			}
			referenceID := artifactReferenceID(instanceID, name, content.GoldenID)
			handle, err := manager.CreateArtifactReference(progressCtx, persistence.ArtifactReferenceRequest{
				ReferenceID: referenceID,
				GoldenID:    content.GoldenID,
				IDMap:       idmap,
			})
			if err != nil {
				return fmt.Errorf("create artifact reference %q: %w", name, err)
			}
			result.Handles[name] = handle
			result.References[name] = referenceID
			if handle.Created {
				result.CreatedByCall = append(result.CreatedByCall, referenceID)
			}
			return nil
		}()
		finishProgress(prepareErr)
		if prepareErr != nil {
			cleanupCreated()
			return preparedArtifactAttachments{}, prepareErr
		}
	}
	return result, nil
}

func (m *AppManager) reconstructRecordedArtifactContent(
	ctx context.Context,
	source api.ArtifactSource,
	missing *persistence.ArtifactContentMissingError,
) error {
	if missing == nil {
		return fmt.Errorf("missing artifact identity is required")
	}
	exact, err := exactSourceForRecordedArtifact(source, missing.Identity)
	if err != nil {
		return err
	}

	content, err := m.ensureArtifactContentAtGoldenID(ctx, exact, missing.GoldenID)
	if err != nil {
		return err
	}
	if content.GoldenID != missing.GoldenID || content.Identity != missing.Identity {
		return fmt.Errorf(
			"recorded artifact reconstructed as %s (%+v), expected %s (%+v)",
			content.GoldenID,
			content.Identity,
			missing.GoldenID,
			missing.Identity,
		)
	}
	return nil
}

func exactSourceForRecordedArtifact(
	source api.ArtifactSource,
	identity persistence.GoldenContentIdentity,
) (api.ArtifactSource, error) {
	exact := source
	switch identity.SourceKind {
	case persistence.GoldenSourceOCI:
		reference := exact.Reference
		if at := strings.LastIndex(reference, "@"); at >= 0 {
			reference = reference[:at]
		}
		exact.Reference = reference + "@" + identity.ResolvedIdentity
		exact.Digest = identity.ResolvedIdentity
	case persistence.GoldenSourceHuggingFace:
		exact.Revision = identity.ResolvedIdentity
	default:
		return api.ArtifactSource{}, fmt.Errorf("unsupported recorded source kind %q", identity.SourceKind)
	}
	return exact, nil
}

func (m *AppManager) ensureAppArtifactAttachments(
	ctx context.Context,
	state *FilesystemStateManager,
	app *AppInstance,
	def *api.AppDefinition,
	runtime container.PodmanRuntime,
) (map[string]persistence.ArtifactHandle, error) {
	if app == nil {
		return nil, fmt.Errorf("app state is required to attach artifacts")
	}
	_, progress := m.inheritedTaskProgress(ctx, taskTypeStartApp, 60)
	progressMax := 90
	if progress > progressMax {
		progressMax = progress
	}
	prepared, err := m.prepareArtifactAttachments(
		ctx,
		def,
		app.InstanceID,
		idMapForRuntime(app.InstanceID, runtime),
		true,
		transferProgressRange{Min: progress, Max: progressMax},
	)
	if err != nil {
		return nil, err
	}
	if artifactReferencesEqual(app.ArtifactReferences, prepared.References) {
		return prepared.Handles, nil
	}
	candidate, err := detachedAppCandidate(app)
	if err != nil {
		return nil, err
	}
	candidate.ArtifactReferences = cloneStringMap(prepared.References)
	if err := commitDetachedAppMetadata(state, app, candidate); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupBudget)
		defer cancel()
		created := make(map[string]string, len(prepared.CreatedByCall))
		for _, referenceID := range prepared.CreatedByCall {
			created[referenceID] = referenceID
		}
		_ = m.destroyArtifactReferences(cleanupCtx, created)
		return nil, fmt.Errorf("persist artifact references: %w", err)
	}
	return prepared.Handles, nil
}

func (m *AppManager) destroyArtifactReferences(ctx context.Context, references map[string]string) error {
	if len(references) == 0 {
		return nil
	}
	manager := m.currentGoldenContentManager()
	if manager == nil {
		return fmt.Errorf("generic golden-content support unavailable")
	}
	names := make([]string, 0, len(references))
	for name := range references {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		if referenceID := references[name]; referenceID != "" {
			if err := manager.DestroyArtifactReference(ctx, referenceID); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (m *AppManager) detachArtifactReferences(ctx context.Context, references map[string]string) error {
	if len(references) == 0 {
		return nil
	}
	manager := m.currentGoldenContentManager()
	if manager == nil {
		return fmt.Errorf("generic golden-content support unavailable")
	}
	names := make([]string, 0, len(references))
	for name := range references {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		if referenceID := references[name]; referenceID != "" {
			if err := manager.DetachArtifactReference(ctx, referenceID); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func artifactReferencesEqual(a, b map[string]string) bool {
	return reflect.DeepEqual(a, b)
}

func subtractArtifactReferences(oldReferences, retained map[string]string) map[string]string {
	superseded := make(map[string]string)
	for name, referenceID := range oldReferences {
		if referenceID == "" {
			continue
		}
		if retained[name] != referenceID {
			superseded[name] = referenceID
		}
	}
	return superseded
}

func (m *AppManager) releaseSupersededArtifactReferences(ctx context.Context, oldReferences, retained map[string]string) {
	superseded := subtractArtifactReferences(oldReferences, retained)
	if len(superseded) == 0 {
		return
	}
	if err := m.destroyArtifactReferences(ctx, superseded); err != nil {
		// The committed references remain safe; this is bounded cleanup debt and
		// must never roll back a runtime that has already been published.
		log.Printf("WARN: release superseded artifact references: %v", err)
	}
}

func (m *AppManager) discardUncommittedArtifactReferences(ctx context.Context, candidate, committed map[string]string) error {
	return m.destroyArtifactReferences(ctx, subtractArtifactReferences(candidate, committed))
}

func (m *AppManager) reconcileArtifactReferences(
	ctx context.Context,
	state *FilesystemStateManager,
) error {
	manager := m.currentGoldenContentManager()
	if manager == nil {
		return nil
	}
	retained, err := retainedArtifactReferenceIDs(state)
	if err != nil {
		return err
	}
	if err := manager.GarbageCollectArtifactReferences(ctx, retained); err != nil {
		return fmt.Errorf("garbage collect artifact references: %w", err)
	}
	return nil
}

func retainedArtifactReferenceIDs(state *FilesystemStateManager) (map[string]struct{}, error) {
	retained := make(map[string]struct{})
	cachedApps := make(map[string]struct{})
	for _, app := range state.ListApps() {
		if app == nil {
			continue
		}
		cachedApps[app.InstanceID] = struct{}{}
		retainArtifactReferenceValues(retained, app.ArtifactReferences)
	}

	entries, err := os.ReadDir(state.appsDir)
	if os.IsNotExist(err) {
		return retained, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list artifact reference owners: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		instanceID := entry.Name()
		if _, cached := cachedApps[instanceID]; !cached {
			hasInstalledState, err := directoryHasInstalledAppState(
				filepath.Join(state.appsDir, instanceID),
			)
			if err != nil {
				return nil, fmt.Errorf("inspect installed artifact owner %s: %w", instanceID, err)
			}
			if hasInstalledState {
				app, err := state.loadAppFromDisk(instanceID)
				if err != nil {
					return nil, fmt.Errorf("read installed artifact owner %s: %w", instanceID, err)
				}
				retainArtifactReferenceValues(retained, app.ArtifactReferences)
			}
		}
		txn, err := state.LoadManifestUpdateTransaction(instanceID)
		if err == nil && txn != nil {
			retainArtifactReferenceValues(retained, txn.PreviousArtifactRefs)
			retainArtifactReferenceValues(retained, txn.CandidateArtifactRefs)
		} else if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read artifact owner manifest transaction for %s: %w", instanceID, err)
		}
		record, err := state.LoadTransitionRecord(instanceID)
		if err == nil && record != nil {
			retainArtifactReferenceValues(retained, record.Resources.PreviousArtifactRefs)
			retainArtifactReferenceValues(retained, record.Resources.CandidateArtifactRefs)
		} else if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read artifact owner transition for %s: %w", instanceID, err)
		}
	}
	return retained, nil
}

func directoryHasInstalledAppState(appDir string) (bool, error) {
	publication, err := inspectAppPublication(appDir)
	return publication == appPublicationComplete, err
}

func retainArtifactReferenceValues(retained map[string]struct{}, references map[string]string) {
	for _, referenceID := range references {
		if referenceID != "" {
			retained[referenceID] = struct{}{}
		}
	}
}

// ensureArtifactContent resolves one manifest source and routes its projection
// through the generalized golden-LV lifecycle.
func (m *AppManager) ensureArtifactContent(ctx context.Context, source api.ArtifactSource) (persistence.GoldenContentHandle, error) {
	return m.ensureArtifactContentAtGoldenID(ctx, source, "")
}

func (m *AppManager) ensureArtifactContentAtGoldenID(
	ctx context.Context,
	source api.ArtifactSource,
	preferredGoldenID string,
) (persistence.GoldenContentHandle, error) {
	manager := m.currentGoldenContentManager()
	if manager == nil {
		return persistence.GoldenContentHandle{}, fmt.Errorf("artifact bindings require generic golden-content support")
	}
	switch source.Type {
	case persistence.GoldenSourceOCI:
		return m.ensureOCIArtifactContent(ctx, manager, source, preferredGoldenID)
	case persistence.GoldenSourceHuggingFace:
		return m.ensureHuggingFaceContent(ctx, manager, source, preferredGoldenID)
	default:
		return persistence.GoldenContentHandle{}, fmt.Errorf("unsupported artifact source type %q", source.Type)
	}
}

func (m *AppManager) ensureOCIArtifactContent(
	ctx context.Context,
	manager persistence.GoldenContentManager,
	source api.ArtifactSource,
	preferredGoldenID string,
) (persistence.GoldenContentHandle, error) {
	runtime, cleanup, err := m.newFlattenRuntime(ctx)
	if err != nil {
		return persistence.GoldenContentHandle{}, fmt.Errorf("create OCI materialization runtime: %w", err)
	}
	defer cleanup()

	pullErr := m.containerManager.PullImageWithProgress(
		ctx,
		runtime,
		source.Reference,
		artifactTransferCallback(ctx),
	)
	if pullErr == nil {
		info, err := m.containerManager.InspectImage(ctx, runtime, source.Reference)
		if err != nil {
			return persistence.GoldenContentHandle{}, fmt.Errorf("inspect OCI image %s: %w", source.Reference, err)
		}
		digest := canonicalImageDigestKey(info.Digest)
		if digest == "" && len(info.RepoDigests) > 0 {
			digest = canonicalImageDigestKey(info.RepoDigests[0])
		}
		if digest == "" {
			return persistence.GoldenContentHandle{}, fmt.Errorf("OCI image %s has no canonical digest", source.Reference)
		}
		if err := verifyExpectedDigest(source.Digest, digest); err != nil {
			return persistence.GoldenContentHandle{}, err
		}
		flatten := m.MakeFlattenFn()
		prePulledDir := filepath.Dir(runtime.Root)
		return manager.EnsureGoldenContent(ctx, persistence.GoldenContentRequest{
			Identity: persistence.GoldenContentIdentity{
				SourceKind:       persistence.GoldenSourceOCI,
				ResolvedIdentity: digest,
				Projection:       persistence.GoldenProjectionOCIImageRootfs,
			},
			SourceRef:         source.Reference,
			SizeHint:          info.Size,
			PreferredGoldenID: preferredGoldenID,
			Materialize: func(ctx context.Context, targetDir string) (persistence.GoldenMaterializationResult, error) {
				cfg, err := flatten(ctx, source.Reference, targetDir, prePulledDir)
				if err != nil {
					return persistence.GoldenMaterializationResult{}, err
				}
				return persistence.GoldenMaterializationResult{ImageConfig: &cfg}, nil
			},
		})
	}

	var nonImage *container.NotContainerImageError
	if !errors.As(pullErr, &nonImage) {
		return persistence.GoldenContentHandle{}, fmt.Errorf("pull OCI source %s as image: %w", source.Reference, pullErr)
	}
	artifactRuntime, ok := m.containerManager.(ociArtifactRuntime)
	if !ok {
		return persistence.GoldenContentHandle{}, fmt.Errorf("OCI source %s is a non-image artifact but this Podman runtime has no artifact support", source.Reference)
	}
	if err := artifactRuntime.PullArtifactWithProgress(
		ctx,
		runtime,
		source.Reference,
		artifactTransferCallback(ctx),
	); err != nil {
		return persistence.GoldenContentHandle{}, fmt.Errorf("pull OCI artifact %s: %w", source.Reference, err)
	}
	info, err := artifactRuntime.InspectArtifact(ctx, runtime, source.Reference)
	if err != nil {
		return persistence.GoldenContentHandle{}, fmt.Errorf("inspect OCI artifact %s: %w", source.Reference, err)
	}
	digest := canonicalImageDigestKey(info.Digest)
	if digest == "" {
		return persistence.GoldenContentHandle{}, fmt.Errorf("OCI artifact %s has no canonical descriptor digest", source.Reference)
	}
	if err := verifyExpectedDigest(source.Digest, digest); err != nil {
		return persistence.GoldenContentHandle{}, err
	}
	return manager.EnsureGoldenContent(ctx, persistence.GoldenContentRequest{
		Identity: persistence.GoldenContentIdentity{
			SourceKind:       persistence.GoldenSourceOCI,
			ResolvedIdentity: digest,
			Projection:       persistence.GoldenProjectionOCIArtifact,
		},
		SourceRef:         source.Reference,
		SizeHint:          info.TotalSizeBytes,
		PreferredGoldenID: preferredGoldenID,
		Materialize: func(ctx context.Context, targetDir string) (persistence.GoldenMaterializationResult, error) {
			if err := artifactRuntime.ExtractArtifact(ctx, runtime, source.Reference, targetDir); err != nil {
				return persistence.GoldenMaterializationResult{}, err
			}
			if err := fsutil.ValidateArtifactTree(targetDir); err != nil {
				return persistence.GoldenMaterializationResult{}, err
			}
			return persistence.GoldenMaterializationResult{}, nil
		},
	})
}

func verifyExpectedDigest(expected, actual string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	if expected != canonicalImageDigestKey(actual) {
		return fmt.Errorf("digest mismatch: expected %s, resolved %s", expected, actual)
	}
	return nil
}

func (m *AppManager) ensureHuggingFaceContent(
	ctx context.Context,
	manager persistence.GoldenContentManager,
	source api.ArtifactSource,
	preferredGoldenID string,
) (persistence.GoldenContentHandle, error) {
	client := newHuggingFaceHTTPClient()
	resolved, err := resolveHuggingFaceSource(ctx, client, defaultHuggingFaceEndpoint, source)
	if err != nil {
		return persistence.GoldenContentHandle{}, err
	}
	if source.Digest != "" && !resolved.SelectedFile {
		return persistence.GoldenContentHandle{}, fmt.Errorf("Hugging Face digest requires a selected file")
	}
	return manager.EnsureGoldenContent(ctx, persistence.GoldenContentRequest{
		Identity: persistence.GoldenContentIdentity{
			SourceKind:       persistence.GoldenSourceHuggingFace,
			ResolvedIdentity: resolved.Commit,
			Projection:       huggingFaceGoldenProjection(resolved.Projection, source.Digest),
		},
		SourceRef:         source.Repository + "@" + source.Revision,
		SizeHint:          resolved.Size,
		PreferredGoldenID: preferredGoldenID,
		Materialize: func(ctx context.Context, targetDir string) (persistence.GoldenMaterializationResult, error) {
			projectionCtx, cancel := context.WithTimeout(ctx, huggingFaceMaxAttempt)
			defer cancel()
			if err := downloadHuggingFaceProjection(
				projectionCtx,
				client,
				defaultHuggingFaceEndpoint,
				source,
				resolved,
				targetDir,
				func(downloaded, total int64) {
					reportArtifactTransferProgress(ctx, downloaded, total)
				},
			); err != nil {
				return persistence.GoldenMaterializationResult{}, err
			}
			return persistence.GoldenMaterializationResult{}, nil
		},
	})
}

func huggingFaceGoldenProjection(projection, expectedDigest string) string {
	result := persistence.GoldenProjectionHuggingFace + ":" + projection
	if expectedDigest != "" {
		// A pin is verification input, not mutable-source identity. Including
		// it in the storage key prevents an older unpinned Ready object from
		// bypassing verification without adding a second metadata protocol.
		result += "@" + expectedDigest
	}
	return result
}

func newHuggingFaceHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many Hugging Face redirects")
			}
			if req.URL.Scheme != "https" || unsafeRedirectHost(req.URL.Hostname()) {
				return fmt.Errorf("unsafe Hugging Face redirect target")
			}
			return nil
		},
	}
}

func unsafeRedirectHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast())
}

func resolveHuggingFaceSource(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	source api.ArtifactSource,
) (resolvedHuggingFaceSource, error) {
	ctx, cancel := context.WithTimeout(ctx, huggingFaceMetadataTimeout)
	defer cancel()

	base, err := url.Parse(endpoint)
	if err != nil {
		return resolvedHuggingFaceSource{}, err
	}
	base.Path = path.Join(base.Path, "api", "models", source.Repository, "revision", source.Revision)
	query := base.Query()
	query.Set("blobs", "true")
	base.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return resolvedHuggingFaceSource{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return resolvedHuggingFaceSource{}, fmt.Errorf("resolve Hugging Face source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resolvedHuggingFaceSource{}, fmt.Errorf("resolve Hugging Face source: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		SHA      string `json:"sha"`
		Siblings []struct {
			Filename string `json:"rfilename"`
			Size     *int64 `json:"size"`
		} `json:"siblings"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxHuggingFaceMetadata))
	if err := decoder.Decode(&payload); err != nil {
		return resolvedHuggingFaceSource{}, fmt.Errorf("decode Hugging Face metadata: %w", err)
	}
	if !isCommitIdentity(payload.SHA) {
		return resolvedHuggingFaceSource{}, fmt.Errorf("Hugging Face returned an invalid concrete revision")
	}

	selection := path.Clean(source.Path)
	var exact *huggingFaceFile
	files := make([]huggingFaceFile, 0)
	prefix := ""
	if selection != "." {
		prefix = selection + "/"
	}
	for _, sibling := range payload.Siblings {
		name := path.Clean(sibling.Filename)
		if name != sibling.Filename || !safeRepositoryPath(name) {
			return resolvedHuggingFaceSource{}, fmt.Errorf("Hugging Face returned unsafe repository path %q", sibling.Filename)
		}
		if sibling.Size == nil || *sibling.Size < 0 {
			return resolvedHuggingFaceSource{}, fmt.Errorf("Hugging Face returned no valid size for %q", sibling.Filename)
		}
		file := huggingFaceFile{Path: name, Size: *sibling.Size}
		if name == selection {
			copy := file
			exact = &copy
		}
		if selection == "." || strings.HasPrefix(name, prefix) {
			files = append(files, file)
		}
	}
	selectedFile := exact != nil
	if selectedFile {
		files = []huggingFaceFile{*exact}
	}
	if len(files) == 0 {
		return resolvedHuggingFaceSource{}, fmt.Errorf("Hugging Face path %q does not select a file or directory", source.Path)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	var size int64
	for _, file := range files {
		if file.Size > 0 {
			if file.Size > int64(^uint64(0)>>1)-size {
				return resolvedHuggingFaceSource{}, fmt.Errorf("Hugging Face projection size overflows")
			}
			size += file.Size
		}
	}
	return resolvedHuggingFaceSource{
		Commit:       payload.SHA,
		Projection:   selection,
		SelectedFile: selectedFile,
		Files:        files,
		Size:         size,
	}, nil
}

func downloadHuggingFaceProjection(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	source api.ArtifactSource,
	resolved resolvedHuggingFaceSource,
	targetDir string,
	progress func(downloaded, total int64),
) error {
	var completed int64
	if progress != nil {
		progress(0, resolved.Size)
	}
	for _, file := range resolved.Files {
		relative := file.Path
		if resolved.SelectedFile {
			relative = path.Base(file.Path)
		} else if resolved.Projection != "." {
			relative = strings.TrimPrefix(file.Path, resolved.Projection+"/")
		}
		if !safeRepositoryPath(relative) {
			return fmt.Errorf("unsafe projected Hugging Face path %q", relative)
		}
		target := filepath.Join(targetDir, filepath.FromSlash(relative))
		if !pathWithinRoot(targetDir, target) {
			return fmt.Errorf("projected Hugging Face path escapes staging root")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := downloadHuggingFaceFile(
			ctx,
			client,
			endpoint,
			source.Repository,
			resolved.Commit,
			file.Path,
			target,
			file.Size,
			source.Digest,
			func(downloaded int64) {
				if progress != nil {
					progress(completed+downloaded, resolved.Size)
				}
			},
		); err != nil {
			return err
		}
		completed += file.Size
		if progress != nil {
			progress(completed, resolved.Size)
		}
	}
	return fsutil.ValidateArtifactTree(targetDir)
}

func downloadHuggingFaceFile(
	ctx context.Context,
	client *http.Client,
	endpoint, repository, commit, repositoryPath, target string,
	expectedSize int64,
	expectedDigest string,
	progress func(downloaded int64),
) error {
	return downloadHuggingFaceFileWithStallTimeout(
		ctx,
		client,
		endpoint,
		repository,
		commit,
		repositoryPath,
		target,
		expectedSize,
		expectedDigest,
		huggingFaceTransferStall,
		huggingFaceDownloadAttemptTimeout(expectedSize),
		progress,
	)
}

func huggingFaceDownloadAttemptTimeout(expectedSize int64) time.Duration {
	if expectedSize <= 0 {
		return huggingFaceAttemptGrace
	}
	seconds := expectedSize / huggingFaceMinBytesPerSec
	if expectedSize%huggingFaceMinBytesPerSec != 0 {
		seconds++
	}
	maxTransferSeconds := int64((huggingFaceMaxAttempt - huggingFaceAttemptGrace) / time.Second)
	if seconds >= maxTransferSeconds {
		return huggingFaceMaxAttempt
	}
	return huggingFaceAttemptGrace + time.Duration(seconds)*time.Second
}

func downloadHuggingFaceFileWithStallTimeout(
	ctx context.Context,
	client *http.Client,
	endpoint, repository, commit, repositoryPath, target string,
	expectedSize int64,
	expectedDigest string,
	stallTimeout time.Duration,
	attemptTimeout time.Duration,
	progress func(downloaded int64),
) error {
	if expectedSize < 0 {
		return fmt.Errorf("download Hugging Face file %s has invalid resolved size %d", repositoryPath, expectedSize)
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	base.Path = path.Join(base.Path, repository, "resolve", commit, repositoryPath)
	query := base.Query()
	query.Set("download", "true")
	base.RawQuery = query.Encode()
	downloadCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, base.String(), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(downloadCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf(
				"download Hugging Face file %s exceeded attempt deadline %s",
				repositoryPath,
				attemptTimeout,
			)
		}
		return fmt.Errorf("download Hugging Face file %s: %w", repositoryPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download Hugging Face file %s: HTTP %d", repositoryPath, resp.StatusCode)
	}

	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	hash := sha256.New()
	var stalled atomic.Bool
	var attemptExpired atomic.Bool
	stopDeadlineClose := context.AfterFunc(downloadCtx, func() {
		if errors.Is(downloadCtx.Err(), context.DeadlineExceeded) {
			attemptExpired.Store(true)
		}
		_ = resp.Body.Close()
	})
	timer := time.AfterFunc(stallTimeout, func() {
		stalled.Store(true)
		cancel()
		_ = resp.Body.Close()
	})
	var downloaded int64
	progressBody := &progressResetReader{
		reader: resp.Body,
		progress: func(read int) {
			timer.Reset(stallTimeout)
			downloaded += int64(read)
			if progress != nil {
				progress(downloaded)
			}
		},
	}
	limited := &io.LimitedReader{R: progressBody, N: expectedSize}
	written, copyErr := io.Copy(io.MultiWriter(file, hash), limited)
	if copyErr == nil && written == expectedSize {
		var extra [1]byte
		if _, extraErr := io.ReadFull(progressBody, extra[:]); extraErr == nil {
			copyErr = fmt.Errorf("response exceeds resolved size %d", expectedSize)
		} else if extraErr != io.EOF && extraErr != io.ErrUnexpectedEOF {
			copyErr = extraErr
		}
	}
	timer.Stop()
	stopDeadlineClose()
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(target)
		if stalled.Load() {
			return fmt.Errorf("download Hugging Face file %s stalled for %s", repositoryPath, stallTimeout)
		}
		if attemptExpired.Load() {
			return fmt.Errorf(
				"download Hugging Face file %s exceeded attempt deadline %s",
				repositoryPath,
				attemptTimeout,
			)
		}
		return fmt.Errorf("download Hugging Face file %s: %w", repositoryPath, copyErr)
	}
	if written != expectedSize {
		_ = os.Remove(target)
		return fmt.Errorf(
			"download Hugging Face file %s ended after %d bytes, expected %d",
			repositoryPath,
			written,
			expectedSize,
		)
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return closeErr
	}
	if expectedDigest != "" {
		actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
		if actual != expectedDigest {
			_ = os.Remove(target)
			return fmt.Errorf("digest mismatch for Hugging Face file %s: expected %s, got %s", repositoryPath, expectedDigest, actual)
		}
	}
	return nil
}

type progressResetReader struct {
	reader   io.Reader
	progress func(read int)
}

func (r *progressResetReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	if read > 0 {
		r.progress(read)
	}
	return read, err
}

func isCommitIdentity(value string) bool {
	if len(value) < 40 || len(value) > 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func safeRepositoryPath(value string) bool {
	return value != "" &&
		value != "." &&
		!strings.HasPrefix(value, "/") &&
		!strings.Contains(value, "\\") &&
		!strings.ContainsRune(value, '\x00') &&
		path.Clean(value) == value &&
		!strings.HasPrefix(value, "../") &&
		!strings.Contains(value, "/../") &&
		!strings.Contains(value, "//")
}

func pathWithinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
