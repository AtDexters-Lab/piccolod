package pcv

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/fsutil"
	"piccolod/internal/runner"
	"piccolod/internal/state/paths"
)

const (
	debounceInterval  = 30 * time.Second
	periodicInterval  = 6 * time.Hour
	maxPCVSize        = 100 * 1024 * 1024 // 100 MiB
	historyLimit      = 5
	publishTimeout    = 2 * time.Minute
	manifestVersion   = 1
	eventBufferSize = 16
)

// Manifest describes a published PCV archive.
type Manifest struct {
	Version         int    `json:"version"`
	Gen             string `json:"gen"`
	CreatedAt       string `json:"created_at"`
	SHA256          string `json:"sha256"`
	SizeBytes       int64  `json:"size_bytes"`
	SourceNodeID    string `json:"source_node_id"`
	SourceModTimeNs int64  `json:"source_mod_time_ns"`
}

// Publisher manages PCV export lifecycle. It implements the supervisor.Component
// interface with a dormant-start model: Start() enters dormant state, Activate()
// transitions to operational.
type Publisher struct {
	bus    *events.Bus
	run    runner.CommandRunner
	nodeID string

	mu        sync.Mutex
	active    bool
	dirty     bool
	counter     uint64
	lastGen     string
	stopCh      chan struct{}
	publishMu   sync.Mutex // serializes doPublish calls
	publishWg   sync.WaitGroup
	devFallback bool // override for testing; bypasses sentinel check

	// cancelMutation cancels the mutation subscriber goroutine.
	cancelMutation func()
	// cancelPeriodic cancels the periodic timer goroutine.
	cancelPeriodic func()
}

// NewPublisher creates a dormant PCV publisher.
func NewPublisher(bus *events.Bus, run runner.CommandRunner) *Publisher {
	return &Publisher{
		bus:    bus,
		run:    run,
		stopCh: make(chan struct{}),
	}
}

// Name implements supervisor.Component.
func (p *Publisher) Name() string { return "pcv-publisher" }

// Start implements supervisor.Component. Enters dormant state.
func (p *Publisher) Start(ctx context.Context) error {
	// Derive node ID eagerly (inputs available before unlock).
	nodeID, err := DeriveNodeID()
	if err != nil {
		log.Printf("WARN: pcv publisher: node ID derivation failed: %v", err)
		nodeID = "unknown"
	}
	p.mu.Lock()
	p.nodeID = nodeID
	p.mu.Unlock()
	return nil
}

// Stop implements supervisor.Component. Shuts down without flushing — the daemon
// is going away so no consumer benefits from a final publish. Recovery archives
// are produced by the mutation debounce (30s) and periodic timer (6h).
func (p *Publisher) Stop(ctx context.Context) error {
	p.mu.Lock()
	select {
	case <-p.stopCh:
		// already closed
	default:
		close(p.stopCh)
	}
	cancelMut := p.cancelMutation
	cancelPer := p.cancelPeriodic
	p.mu.Unlock()

	if cancelMut != nil {
		cancelMut()
	}
	if cancelPer != nil {
		cancelPer()
	}

	// Prevent new PublishNow calls and wait for any in-flight publish.
	p.mu.Lock()
	p.active = false
	p.mu.Unlock()
	p.publishWg.Wait()
	return nil
}

// Activate transitions the publisher from dormant to operational.
// Called after confirming the control plane volume is available.
func (p *Publisher) Activate() {
	p.mu.Lock()
	if p.active {
		p.mu.Unlock()
		return
	}
	p.active = true
	p.mu.Unlock()

	// Clean up staging leftovers from previous crashes.
	p.cleanupStaging()

	// Load previous manifest for counter continuity.
	p.loadPreviousManifest()

	// Start subscribers and timers.
	p.startMutationSubscriber()
	p.startPeriodicTimer()

	// Startup dirty-check with cancellation support.
	go func() {
		select {
		case <-p.stopCh:
			return
		default:
		}
		if p.needsStartupPublish() {
			p.mu.Lock()
			p.dirty = true
			p.mu.Unlock()
			if err := p.doPublish(context.Background()); err != nil {
				log.Printf("WARN: pcv publisher: startup publish failed: %v", err)
			}
		}
	}()

	log.Printf("pcv publisher activated")
}

// PublishNow triggers an immediate on-demand publish. Returns the resulting
// manifest or an error.
func (p *Publisher) PublishNow(ctx context.Context) (*Manifest, error) {
	p.mu.Lock()
	if !p.active {
		p.mu.Unlock()
		return nil, fmt.Errorf("pcv publisher not active")
	}
	p.dirty = true
	p.mu.Unlock()

	if err := p.doPublish(ctx); err != nil {
		return nil, err
	}
	return p.readManifest()
}

// --- Internal ---

func (p *Publisher) startMutationSubscriber() {
	if p.bus == nil {
		return
	}
	commitCh, cancelCommit := p.bus.SubscribeWithCancel(events.TopicControlStoreCommit, eventBufferSize)
	lockCh, cancelLock := p.bus.SubscribeWithCancel(events.TopicLockStateChanged, eventBufferSize)

	cancel := func() {
		cancelCommit()
		cancelLock()
	}

	p.mu.Lock()
	p.cancelMutation = cancel
	p.mu.Unlock()

	go p.mutationLoop(commitCh, lockCh)
}

func (p *Publisher) mutationLoop(commitCh <-chan events.Event, lockCh <-chan events.Event) {
	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	for {
		select {
		case <-p.stopCh:
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return

		case _, ok := <-commitCh:
			if !ok {
				return
			}
			p.mu.Lock()
			p.dirty = true
			p.mu.Unlock()

			// Reset trailing-edge debounce timer.
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(debounceInterval)
				debounceCh = debounceTimer.C
			} else {
				debounceTimer.Stop()
				debounceTimer.Reset(debounceInterval)
			}

		case evt, ok := <-lockCh:
			if !ok {
				return
			}
			payload, ok := evt.Payload.(events.LockStateChanged)
			if !ok || !payload.Locked {
				continue
			}
			// On lock: immediate flush if dirty.
			p.mu.Lock()
			dirty := p.dirty
			p.mu.Unlock()
			if dirty {
				if debounceTimer != nil {
					debounceTimer.Stop()
					debounceCh = nil
				}
				if err := p.doPublish(context.Background()); err != nil {
					log.Printf("WARN: pcv publisher: flush-on-lock failed: %v", err)
				}
			}

		case <-debounceCh:
			debounceCh = nil
			p.mu.Lock()
			dirty := p.dirty
			p.mu.Unlock()
			if dirty {
				if err := p.doPublish(context.Background()); err != nil {
					log.Printf("WARN: pcv publisher: debounce publish failed: %v", err)
				}
			}
		}
	}
}

func (p *Publisher) startPeriodicTimer() {
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancelPeriodic = cancel
	p.mu.Unlock()

	go func() {
		ticker := time.NewTicker(periodicInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.stopCh:
				return
			case <-ticker.C:
				// Skip if generation unchanged (no mutations while locked).
				if !p.needsStartupPublish() {
					continue
				}
				p.mu.Lock()
				p.dirty = true
				p.mu.Unlock()
				if err := p.doPublish(context.Background()); err != nil {
					log.Printf("WARN: pcv publisher: periodic publish failed: %v", err)
				}
			}
		}
	}()
}

// doPublish performs the actual PCV archive build and atomic publish.
// Uses publishMu to serialize concurrent publish attempts.
func (p *Publisher) doPublish(ctx context.Context) error {
	p.publishMu.Lock()
	defer p.publishMu.Unlock()

	// Check active and increment WaitGroup atomically under p.mu.
	// This prevents a race where Stop() sets active=false and calls
	// publishWg.Wait() while a goroutine concurrently calls Add(1).
	p.mu.Lock()
	if !p.active {
		p.mu.Unlock()
		return fmt.Errorf("publisher not active")
	}
	p.publishWg.Add(1)
	p.mu.Unlock()
	defer p.publishWg.Done()

	pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	coreRoot := paths.CoreRoot()
	loopFile := filepath.Join(coreRoot, "control-plane.luks")
	mountDir := paths.MountDir("control-plane")
	recoveryDir := filepath.Join(coreRoot, "recovery")
	stagingDir := filepath.Join(recoveryDir, "staging")
	historyDir := filepath.Join(recoveryDir, "history")

	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return p.publishFailed(fmt.Errorf("create staging dir: %w", err))
	}
	if err := os.MkdirAll(historyDir, 0o700); err != nil {
		return p.publishFailed(fmt.Errorf("create history dir: %w", err))
	}

	// Step 1: Sync the control plane mount to flush pending writes.
	_ = fsutil.SyncDir(mountDir)

	// Step 2: Copy loop file under fsfreeze for crash consistency.
	snapshotFile := filepath.Join(stagingDir, fmt.Sprintf("pcv-%d.luks", time.Now().UnixNano()))
	defer os.Remove(snapshotFile)

	if err := p.createLoopSnapshot(pubCtx, loopFile, snapshotFile, mountDir); err != nil {
		return p.publishFailed(fmt.Errorf("create loop snapshot: %w", err))
	}

	// Step 3: Get loop file provenance (modification time).
	sourceModTime := p.getLoopFileModTime(loopFile)

	// Step 4: Build tar.gz archive.
	tmpArchive, archiveSize, archiveHash, err := p.buildArchive(pubCtx, coreRoot, snapshotFile)
	if err != nil {
		return p.publishFailed(fmt.Errorf("build archive: %w", err))
	}
	defer os.Remove(tmpArchive)

	// Size guard.
	if archiveSize > maxPCVSize {
		return p.publishFailed(fmt.Errorf("PCV archive too large: %d bytes (max %d)", archiveSize, maxPCVSize))
	}

	// Step 5: Generate generation ID.
	gen := p.nextGeneration()

	// Step 6: Build manifest.
	p.mu.Lock()
	nodeID := p.nodeID
	p.mu.Unlock()

	manifest := Manifest{
		Version:         manifestVersion,
		Gen:             gen,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		SHA256:          archiveHash,
		SizeBytes:       archiveSize,
		SourceNodeID:    nodeID,
		SourceModTimeNs: sourceModTime,
	}

	// Step 7: Atomic publish archive.
	destArchive := filepath.Join(recoveryDir, "current.enc")
	if err := os.Rename(tmpArchive, destArchive); err != nil {
		return p.publishFailed(fmt.Errorf("publish archive: %w", err))
	}
	_ = fsutil.SyncDir(recoveryDir)

	// Step 8: Atomic publish manifest.
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	destManifest := filepath.Join(recoveryDir, "current.json")
	if err := fsutil.AtomicWriteFile(destManifest, manifestData, 0o600); err != nil {
		return p.publishFailed(fmt.Errorf("publish manifest: %w", err))
	}

	// Step 9: Archive to history with bounded retention.
	historyFile := filepath.Join(historyDir, gen+".enc")
	copyFile(destArchive, historyFile)
	historyManifest := filepath.Join(historyDir, gen+".json")
	_ = fsutil.AtomicWriteFile(historyManifest, manifestData, 0o600)
	p.pruneHistory(historyDir)

	// Clear dirty latch.
	p.mu.Lock()
	p.dirty = false
	p.lastGen = gen
	p.mu.Unlock()

	// Emit success event.
	if p.bus != nil {
		p.bus.Publish(events.Event{
			Topic: events.TopicPCVExportPublished,
			Payload: events.PCVExportPublished{
				Generation: gen,
				SHA256:     archiveHash,
				SizeBytes:  archiveSize,
			},
		})
	}

	log.Printf("pcv: published generation %s (%d bytes)", gen, archiveSize)
	return nil
}

func (p *Publisher) publishFailed(err error) error {
	log.Printf("ERROR: pcv publish: %v", err)
	if p.bus != nil {
		p.bus.Publish(events.Event{
			Topic:   events.TopicPCVExportFailed,
			Payload: events.PCVExportFailed{Error: err.Error()},
		})
	}
	return err
}

// buildArchive creates a gzip tar archive containing the LUKS loop file
// snapshot and essential crypto files. Returns the temp path, size, and
// hex SHA-256.
func (p *Publisher) buildArchive(ctx context.Context, coreRoot, snapshotFile string) (tmpPath string, size int64, hash string, err error) {
	tmpFile, err := os.CreateTemp(filepath.Join(coreRoot, "recovery", "staging"), "pcv-archive-*.tmp")
	if err != nil {
		return "", 0, "", err
	}
	tmpPath = tmpFile.Name()

	success := false
	defer func() {
		tmpFile.Close()
		if !success {
			os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	mw := io.MultiWriter(tmpFile, hasher)
	gw := gzip.NewWriter(mw)
	tw := tar.NewWriter(gw)

	modTime := time.Now().UTC().Round(time.Second)

	// Add the LUKS loop file from the frozen snapshot.
	if err := p.addFileToTar(tw, snapshotFile, "control-plane.luks", modTime); err != nil {
		tw.Close()
		gw.Close()
		return "", 0, "", fmt.Errorf("add loop file: %w", err)
	}

	// Add essential crypto and volume metadata files.
	essentialFiles := []struct {
		srcRel string // relative to coreRoot
		tarName string
	}{
		{"crypto/keyset.json", "crypto/keyset.json"},
		{"crypto/piccolo_data_pool_key.enc", "crypto/piccolo_data_pool_key.enc"},
		{"volumes/control-plane/piccolo.volume.json", "volumes/control-plane/piccolo.volume.json"},
	}
	for _, ef := range essentialFiles {
		src := filepath.Join(coreRoot, ef.srcRel)
		if err := p.addFileToTar(tw, src, ef.tarName, modTime); err != nil {
			if os.IsNotExist(err) {
				continue // optional files may not exist yet
			}
			tw.Close()
			gw.Close()
			return "", 0, "", fmt.Errorf("add %s: %w", ef.srcRel, err)
		}
	}

	// Add LUKS KDF params directory.
	kdfDir := filepath.Join(coreRoot, "crypto", "luks-kdf-params")
	if info, err := os.Stat(kdfDir); err == nil && info.IsDir() {
		if err := p.addDirToTar(ctx, tw, kdfDir, "crypto/luks-kdf-params", modTime); err != nil {
			tw.Close()
			gw.Close()
			return "", 0, "", fmt.Errorf("add kdf params: %w", err)
		}
	}

	if err := tw.Close(); err != nil {
		gw.Close()
		return "", 0, "", err
	}
	if err := gw.Close(); err != nil {
		return "", 0, "", err
	}
	if err := tmpFile.Sync(); err != nil {
		return "", 0, "", err
	}

	info, err := tmpFile.Stat()
	if err != nil {
		return "", 0, "", err
	}

	success = true
	return tmpPath, info.Size(), fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

func (p *Publisher) addDirToTar(ctx context.Context, tw *tar.Writer, srcDir, tarPrefix string, modTime time.Time) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		tarName := filepath.ToSlash(filepath.Join(tarPrefix, rel))

		if d.IsDir() {
			if rel == "." {
				tarName = tarPrefix
			}
			return tw.WriteHeader(&tar.Header{
				Name:     tarName + "/",
				Typeflag: tar.TypeDir,
				Mode:     0o700,
				ModTime:  modTime,
			})
		}

		// Skip symlinks for security.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		return p.addFileToTar(tw, path, tarName, modTime)
	})
}

func (p *Publisher) addFileToTar(tw *tar.Writer, srcPath, tarName string, modTime time.Time) error {
	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = tarName
	hdr.ModTime = modTime
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

// nextGeneration produces a monotonically increasing generation ID.
func (p *Publisher) nextGeneration() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now().UTC()
	ts := now.Format("20060102T150405Z")

	p.counter++
	candidate := fmt.Sprintf("%s-%06d", ts, p.counter)

	// Clock-skew safety: if new gen would not be greater than last,
	// reuse the timestamp portion from the last gen and increment counter.
	if p.lastGen != "" && candidate <= p.lastGen {
		parts := strings.SplitN(p.lastGen, "-", 2)
		if len(parts) == 2 {
			ts = parts[0]
		}
		candidate = fmt.Sprintf("%s-%06d", ts, p.counter)
	}

	return candidate
}

// needsStartupPublish checks whether the loop file has been modified since
// the last publish, indicating unpublished mutations. Uses file mtime which
// may have coarser resolution on some filesystems; worst case is a redundant
// publish (never data loss).
func (p *Publisher) needsStartupPublish() bool {
	manifest, err := p.readManifest()
	if err != nil {
		return true // no manifest = needs publish
	}
	loopFile := filepath.Join(paths.CoreRoot(), "control-plane.luks")
	currentModTime := p.getLoopFileModTime(loopFile)
	return currentModTime != manifest.SourceModTimeNs
}

func (p *Publisher) readManifest() (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(paths.CoreRoot(), "recovery", "current.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (p *Publisher) loadPreviousManifest() {
	m, err := p.readManifest()
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastGen = m.Gen
	// Extract counter from generation ID.
	if parts := strings.SplitN(m.Gen, "-", 2); len(parts) == 2 {
		var c uint64
		fmt.Sscanf(parts[1], "%d", &c)
		p.counter = c
	}
}

// createLoopSnapshot copies the LUKS loop file to a staging location for
// archiving. In production, it freezes the mounted ext4 filesystem first
// (via fsfreeze) to ensure crash consistency. In dev/test mode, it skips
// the freeze since there is no real mount.
func (p *Publisher) createLoopSnapshot(ctx context.Context, loopFile, snapshotFile, mountDir string) error {
	if !p.devFallback {
		// Only freeze if the filesystem is currently mounted. During flush-on-lock,
		// the control-plane may already be unmounted — the loop file is quiescent
		// and safe to copy without a freeze.
		if err := p.run.Run(ctx, "mountpoint", "-q", mountDir); err == nil {
			if err := p.run.Run(ctx, "fsfreeze", "--freeze", mountDir); err != nil {
				return fmt.Errorf("fsfreeze --freeze: %w", err)
			}
			// Use context.Background for unfreeze — if the parent context is
			// cancelled mid-copy, the filesystem must still be unfrozen.
			defer p.run.Run(context.Background(), "fsfreeze", "--unfreeze", mountDir)
		}
	}

	in, err := os.Open(loopFile)
	if err != nil {
		return fmt.Errorf("open loop file: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(snapshotFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create snapshot file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy loop file: %w", err)
	}
	return out.Sync()
}

// getLoopFileModTime returns the modification time of the loop file in
// nanoseconds since epoch. Used for dirty-checking between publishes.
func (p *Publisher) getLoopFileModTime(loopFile string) int64 {
	info, err := os.Stat(loopFile)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

func (p *Publisher) cleanupStaging() {
	stagingDir := filepath.Join(paths.CoreRoot(), "recovery", "staging")
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pcv-") {
			path := filepath.Join(stagingDir, e.Name())
			log.Printf("pcv publisher: cleaning up stale staging entry %s", e.Name())
			os.Remove(path)
		}
	}
}

func (p *Publisher) pruneHistory(historyDir string) {
	entries, err := os.ReadDir(historyDir)
	if err != nil {
		return
	}
	// Collect .enc files, sorted lexicographically (oldest first).
	var archives []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".enc") {
			archives = append(archives, e.Name())
		}
	}
	sort.Strings(archives)

	// Remove oldest beyond limit.
	for len(archives) > historyLimit {
		oldest := archives[0]
		archives = archives[1:]
		base := strings.TrimSuffix(oldest, ".enc")
		os.Remove(filepath.Join(historyDir, oldest))
		os.Remove(filepath.Join(historyDir, base+".json"))
	}
}

func copyFile(src, dst string) {
	in, err := os.Open(src)
	if err != nil {
		log.Printf("WARN: pcv: copy history archive: open %s: %v", src, err)
		return
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Printf("WARN: pcv: copy history archive: create %s: %v", dst, err)
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		log.Printf("WARN: pcv: copy history archive: %v", err)
	}
	out.Sync()
}
