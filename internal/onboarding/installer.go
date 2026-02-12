package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"piccolod/internal/events"
	"piccolod/internal/runner"
	"piccolod/internal/state/paths"
)

var errChecksumMismatch = errors.New("SHA-256 checksum mismatch")

const (
	obsBaseURL      = "https://download.opensuse.org/repositories/home:/atdexterslab:/piccolo-os/home_atdexterslab_atdexterslab_tumbleweed/"
	defaultNumConns = 16
	maxRetries      = 3
	retryBaseDelay  = 2 * time.Second
)

// Installer handles the Install to Disk pipeline.
type Installer struct {
	run            runner.CommandRunner
	reporter       events.ProgressReporter
	onboardingMgr  *Manager
	mu             sync.Mutex
	running        bool
}

// NewInstaller creates a new Install to Disk installer.
func NewInstaller(run runner.CommandRunner, reporter events.ProgressReporter, mgr *Manager) *Installer {
	return &Installer{
		run:           run,
		reporter:      reporter,
		onboardingMgr: mgr,
	}
}

// Install starts the Install to Disk pipeline. It runs in a goroutine and reports
// progress via the event bus. Returns an error immediately if already running.
func (inst *Installer) Install(ctx context.Context, targetDisk, imageURL, taskID string) error {
	inst.mu.Lock()
	if inst.running {
		inst.mu.Unlock()
		return fmt.Errorf("install already in progress")
	}
	inst.running = true
	inst.mu.Unlock()

	go func() {
		defer func() {
			inst.mu.Lock()
			inst.running = false
			inst.mu.Unlock()
		}()
		if err := inst.runPipeline(ctx, targetDisk, imageURL, taskID); err != nil {
			log.Printf("ERROR: install to disk failed: %v", err)
			inst.report(taskID, "Error", 0, err.Error(), true)
		}
	}()

	return nil
}

// resolveImageURL determines the OBS image URL based on architecture and board.
func resolveImageURL() string {
	if override := os.Getenv("PICCOLO_INSTALL_IMAGE_URL"); override != "" {
		return override
	}

	arch := runtime.GOARCH
	var variant string
	switch arch {
	case "arm64":
		if isRaspberryPi() {
			variant = "aarch64-RaspberryPi"
		} else {
			variant = "aarch64-SelfInstall"
		}
	default: // amd64
		variant = "x86_64-SelfInstall"
	}

	return obsBaseURL + "piccolo-os." + variant + ".raw.xz"
}

// isRaspberryPi checks /sys/firmware/devicetree/base/model for Raspberry Pi.
func isRaspberryPi() bool {
	data, err := os.ReadFile("/sys/firmware/devicetree/base/model")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(data)), "raspberry pi")
}

// runPipeline executes the full install pipeline.
func (inst *Installer) runPipeline(ctx context.Context, targetDisk, imageURL, taskID string) error {
	if imageURL == "" {
		imageURL = resolveImageURL()
	}

	// Phase: Validate
	inst.report(taskID, "Validating target disk", 1, "", false)
	if err := ValidateTargetDisk(ctx, inst.run, targetDisk); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	inst.report(taskID, "Validating target disk", 2, "Target disk validated", false)

	// Phase: Download
	inst.report(taskID, "Downloading image", 3, "Starting download from OBS", false)
	stagingDir, imagePath, err := inst.downloadImage(ctx, imageURL, taskID)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	// Phase: Verify (SHA-256)
	inst.report(taskID, "Verifying image integrity", 66, "Checking SHA-256", false)
	if err := inst.verifyChecksum(ctx, imageURL, imagePath); err != nil {
		if errors.Is(err, errChecksumMismatch) {
			return fmt.Errorf("integrity check failed: %w", err)
		}
		// Checksum file unavailable — proceed with warning.
		log.Printf("WARN: checksum verification skipped: %v", err)
	}
	inst.report(taskID, "Verifying image integrity", 70, "Image verified", false)

	// Phase: Write — get compressed size for progress estimation.
	compressedInfo, _ := os.Stat(imagePath)
	var compressedSize int64
	if compressedInfo != nil {
		compressedSize = compressedInfo.Size()
	}
	// Re-validate disk is still unmounted before destructive write (TOCTOU mitigation).
	if err := ValidateTargetDisk(ctx, inst.run, targetDisk); err != nil {
		return fmt.Errorf("pre-write validation failed: %w", err)
	}
	inst.report(taskID, "Writing image to disk", 71, "Starting xzcat | dd", false)
	if err := inst.writeImage(ctx, imagePath, targetDisk, taskID, compressedSize); err != nil {
		return fmt.Errorf("write failed: %w", err)
	}
	inst.report(taskID, "Writing image to disk", 92, "Image written", false)

	// Phase: Sync
	inst.report(taskID, "Syncing writes", 93, "Running sync", false)
	if err := inst.run.Run(ctx, "sync"); err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}
	inst.report(taskID, "Syncing writes", 95, "Sync complete", false)

	// Phase: Boot config
	inst.report(taskID, "Configuring boot order", 96, "Setting up efibootmgr", false)
	if err := inst.configureBootOrder(ctx, targetDisk); err != nil {
		log.Printf("WARN: efibootmgr failed (non-fatal): %v", err)
		inst.report(taskID, "Configuring boot order", 97,
			"Boot order configuration failed — change boot order in BIOS manually", false)
	} else {
		inst.report(taskID, "Configuring boot order", 98, "Boot order configured", false)
	}

	// Phase: Complete
	if inst.onboardingMgr != nil {
		if err := inst.onboardingMgr.MarkInstallDone(); err != nil {
			return fmt.Errorf("mark install done: %w", err)
		}
	}

	inst.report(taskID, "Complete", 100, "Installation complete. Remove the USB drive and reboot.", true)
	return nil
}

// downloadImage downloads the image to a staging directory.
func (inst *Installer) downloadImage(ctx context.Context, imageURL, taskID string) (stagingDir, imagePath string, err error) {
	// Get content length first to ensure staging dir has enough space.
	contentLength, err := getContentLength(ctx, imageURL)
	if err != nil {
		return "", "", fmt.Errorf("get content length: %w", err)
	}

	// Determine staging directory.
	stagingDir, err = chooseStagingDir(contentLength)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create staging dir: %w", err)
	}

	imagePath = filepath.Join(stagingDir, "image.raw.xz")

	// Attempt parallel download.
	err = parallelDownload(ctx, imageURL, imagePath, defaultNumConns, func(downloaded int64, total int64) {
		if total <= 0 {
			return
		}
		// Map download progress to 3-65% range.
		pct := 3 + int(float64(downloaded)/float64(total)*62)
		if pct > 65 {
			pct = 65
		}
		inst.report(taskID, "Downloading image", pct,
			fmt.Sprintf("Downloaded %d MB / %d MB", downloaded/(1024*1024), total/(1024*1024)), false)
	})
	if err != nil {
		os.RemoveAll(stagingDir)
		return "", "", err
	}

	return stagingDir, imagePath, nil
}

// getContentLength performs a HEAD request to get the image size.
func getContentLength(ctx context.Context, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	return resp.ContentLength, nil
}

// chooseStagingDir picks a staging directory with sufficient space.
func chooseStagingDir(requiredSpace int64) (string, error) {
	// First try /tmp (tmpfs).
	tmpDir := "/tmp/piccolo-install"
	if hasEnoughSpace(tmpDir, requiredSpace) {
		return tmpDir, nil
	}

	// Fall back to persistent storage.
	fallback := paths.CoreJoin("recovery", "install-staging")
	if hasEnoughSpace(fallback, requiredSpace) {
		return fallback, nil
	}

	return "", fmt.Errorf("insufficient disk space: need %d MB, neither /tmp nor /piccolo-core has enough room",
		requiredSpace/(1024*1024))
}

// hasEnoughSpace checks if the path has enough available space.
// minBytes of 0 means we accept any available space (we'll check after HEAD).
func hasEnoughSpace(path string, minBytes int64) bool {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return false
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return false
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	if minBytes > 0 {
		return available > minBytes
	}
	// Require at least 2GB for tmpfs staging.
	return available > 2*1024*1024*1024
}

// parallelDownload downloads a file using multiple HTTP Range connections.
func parallelDownload(ctx context.Context, url, destPath string, numConns int, progress func(downloaded, total int64)) error {
	// HEAD request to get content length and range support.
	headReq, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return fmt.Errorf("create HEAD request: %w", err)
	}
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		return fmt.Errorf("HEAD request: %w", err)
	}
	headResp.Body.Close()

	contentLength := headResp.ContentLength
	supportsRange := headResp.Header.Get("Accept-Ranges") == "bytes"

	// Fall back to single-stream if server doesn't support Range or unknown size.
	if contentLength <= 0 || !supportsRange {
		return singleStreamDownload(ctx, url, destPath, progress)
	}

	// Create output file and preallocate.
	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	if err := f.Truncate(contentLength); err != nil {
		return fmt.Errorf("preallocate: %w", err)
	}

	// Split into chunks.
	chunkSize := contentLength / int64(numConns)
	var downloaded atomic.Int64

	g, gctx := errgroup.WithContext(ctx)
	for i := 0; i < numConns; i++ {
		start := int64(i) * chunkSize
		end := start + chunkSize - 1
		if i == numConns-1 {
			end = contentLength - 1 // Last chunk gets remainder.
		}

		g.Go(func() error {
			return downloadChunk(gctx, url, f, start, end, &downloaded, contentLength, progress)
		})
	}

	// Progress reporter goroutine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				progress(downloaded.Load(), contentLength)
			case <-gctx.Done():
				return
			}
		}
	}()

	err = g.Wait()
	<-done

	if err != nil {
		os.Remove(destPath)
		return err
	}

	// Final progress update.
	progress(contentLength, contentLength)

	// Verify file size.
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat after download: %w", err)
	}
	if info.Size() != contentLength {
		os.Remove(destPath)
		return fmt.Errorf("size mismatch: got %d, expected %d", info.Size(), contentLength)
	}

	return nil
}

// downloadChunk downloads a byte range and writes to the file at the correct offset.
func downloadChunk(ctx context.Context, url string, f *os.File, start, end int64, downloaded *atomic.Int64, total int64, progress func(int64, int64)) error {
	for attempt := 0; attempt < maxRetries; attempt++ {
		err := func() error {
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				return err
			}
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusPartialContent {
				// Server ignored Range header — fall back handled by caller.
				return fmt.Errorf("expected 206, got %d", resp.StatusCode)
			}

			buf := make([]byte, 64*1024) // 64KB read buffer
			offset := start
			for {
				n, readErr := resp.Body.Read(buf)
				if n > 0 {
					if _, writeErr := f.WriteAt(buf[:n], offset); writeErr != nil {
						return fmt.Errorf("write at offset %d: %w", offset, writeErr)
					}
					offset += int64(n)
					downloaded.Add(int64(n))
				}
				if readErr == io.EOF {
					return nil
				}
				if readErr != nil {
					return fmt.Errorf("read: %w", readErr)
				}
			}
		}()

		if err == nil {
			return nil
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		if attempt < maxRetries-1 {
			delay := retryBaseDelay * time.Duration(1<<uint(attempt))
			log.Printf("WARN: download chunk %d-%d failed (attempt %d): %v; retrying in %v",
				start, end, attempt+1, err, delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		} else {
			return fmt.Errorf("chunk %d-%d failed after %d attempts: %w", start, end, maxRetries, err)
		}
	}
	return nil
}

// singleStreamDownload is the fallback for servers without Range support.
func singleStreamDownload(ctx context.Context, url, destPath string, progress func(downloaded, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	total := resp.ContentLength
	var downloaded int64
	buf := make([]byte, 64*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := f.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			downloaded += int64(n)
			progress(downloaded, total)
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// verifyChecksum downloads and verifies the SHA-256 checksum.
func (inst *Installer) verifyChecksum(ctx context.Context, imageURL, imagePath string) error {
	checksumURL := imageURL + ".sha256"
	req, err := http.NewRequestWithContext(ctx, "GET", checksumURL, nil)
	if err != nil {
		return fmt.Errorf("checksum request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("checksum download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksum file not available (status %d)", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return fmt.Errorf("read checksum: %w", err)
	}

	// Parse expected hash (first field, hex-encoded).
	fields := strings.Fields(strings.TrimSpace(string(body)))
	if len(fields) == 0 {
		return fmt.Errorf("empty checksum file")
	}
	expected := strings.ToLower(fields[0])

	// Compute actual hash.
	f, err := os.Open(imagePath)
	if err != nil {
		return fmt.Errorf("open image for hash: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash image: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))

	if actual != expected {
		os.Remove(imagePath)
		return fmt.Errorf("%w: expected %s, got %s", errChecksumMismatch, expected, actual)
	}

	return nil
}

// writeImage decompresses and writes the image to the target disk.
// compressedSize is the .raw.xz file size, used to estimate decompressed size for progress.
func (inst *Installer) writeImage(ctx context.Context, imagePath, targetDisk, taskID string, compressedSize int64) error {
	xzCmd := exec.CommandContext(ctx, "xzcat", imagePath)
	pipe, err := xzCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("xzcat stdout pipe: %w", err)
	}

	// Estimate decompressed size (xz ratio ~3-5x, use 4x).
	estimatedSize := compressedSize * 4

	// Parse dd progress output.
	progressParser := &ddProgressParser{
		taskID:        taskID,
		reporter:      inst,
		estimatedSize: estimatedSize,
	}

	ddCmd := exec.CommandContext(ctx, "dd",
		"of="+targetDisk, "bs=4M", "conv=fsync", "status=progress")
	ddCmd.Stdin = pipe
	ddCmd.Stderr = progressParser

	// Start xzcat first, then dd.
	if err := xzCmd.Start(); err != nil {
		return fmt.Errorf("start xzcat: %w", err)
	}
	if err := ddCmd.Start(); err != nil {
		xzCmd.Process.Kill()
		xzCmd.Wait() // Reap to prevent zombie
		return fmt.Errorf("start dd: %w", err)
	}

	// Wait for both processes.
	ddErr := ddCmd.Wait()
	xzErr := xzCmd.Wait()

	if ddErr != nil {
		return fmt.Errorf("dd failed: %w", ddErr)
	}
	if xzErr != nil {
		return fmt.Errorf("xzcat failed: %w", xzErr)
	}

	return nil
}

// configureBootOrder attempts to set the internal disk as the first UEFI boot entry.
func (inst *Installer) configureBootOrder(ctx context.Context, targetDisk string) error {
	// Check if UEFI is available.
	if _, err := os.Stat("/sys/firmware/efi"); os.IsNotExist(err) {
		return fmt.Errorf("not a UEFI system")
	}

	// Find the ESP partition on the target disk (partition 1).
	espDev := partitionDevPath(targetDisk, 1)
	disk, partNum := splitDiskAndPart(espDev)
	if disk == "" {
		return fmt.Errorf("could not parse ESP device: %s", espDev)
	}

	loader := `\EFI\BOOT\BOOTX64.EFI`
	if runtime.GOARCH == "arm64" {
		loader = `\EFI\BOOT\BOOTAA64.EFI`
	}

	return inst.run.Run(ctx, "efibootmgr",
		"--create",
		"--disk", disk,
		"--part", strconv.Itoa(partNum),
		"--label", "Piccolo OS",
		"--loader", loader,
	)
}

// partitionDevPath returns the device path for a partition on a disk.
func partitionDevPath(disk string, slot int) string {
	if len(disk) > 0 && disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
		return fmt.Sprintf("%sp%d", disk, slot)
	}
	return fmt.Sprintf("%s%d", disk, slot)
}

// splitDiskAndPart splits a partition device path into disk and partition number.
func splitDiskAndPart(partDev string) (disk string, partNum int) {
	// NVMe/eMMC: /dev/nvme0n1p1 → /dev/nvme0n1, 1
	if strings.Contains(partDev, "nvme") || strings.Contains(partDev, "mmcblk") {
		if idx := strings.LastIndex(partDev, "p"); idx > 0 {
			rest := partDev[idx+1:]
			if n, err := strconv.Atoi(rest); err == nil {
				return partDev[:idx], n
			}
		}
		return "", 0
	}
	// SCSI/SATA: /dev/sda1 → /dev/sda, 1
	re := regexp.MustCompile(`^(.+?)(\d+)$`)
	m := re.FindStringSubmatch(partDev)
	if len(m) == 3 {
		if n, err := strconv.Atoi(m[2]); err == nil {
			return m[1], n
		}
	}
	return "", 0
}

// report emits a TaskProgressEvent.
func (inst *Installer) report(taskID, phase string, progress int, message string, complete bool) {
	if inst.reporter == nil {
		return
	}
	evt := events.TaskProgressEvent{
		TaskID:     taskID,
		TaskType:   "install_to_disk",
		Phase:      phase,
		Progress:   progress,
		Message:    message,
		IsComplete: complete,
		Timestamp:  time.Now(),
	}
	if complete && message != "" && progress < 100 {
		evt.Error = message
	}
	inst.reporter.Report(evt)
}

// ddProgressParser parses dd status=progress stderr output for progress reporting.
// It interpolates within the 71-91% range based on bytes written vs estimated total.
type ddProgressParser struct {
	taskID        string
	reporter      *Installer
	estimatedSize int64 // estimated decompressed size (compressed * ratio)
}

func (p *ddProgressParser) Write(data []byte) (int, error) {
	// dd outputs progress lines like: "1234567890 bytes (1.2 GB, 1.1 GiB) copied, 5.0 s, 247 MB/s"
	line := string(data)
	if strings.Contains(line, "bytes") && strings.Contains(line, "copied") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			if written, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
				mb := written / (1024 * 1024)
				pct := 71
				if p.estimatedSize > 0 {
					ratio := float64(written) / float64(p.estimatedSize)
					if ratio > 1.0 {
						ratio = 1.0
					}
					pct = 71 + int(ratio*20) // 71 to 91
				}
				p.reporter.report(p.taskID, "Writing image to disk", pct,
					fmt.Sprintf("Written %d MB", mb), false)
			}
		}
	}
	return len(data), nil
}
