package onboarding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/runner"
)

const (
	obsBaseURL = "https://download.opensuse.org/repositories/home:/atdexterslab:/piccolo-os/home_atdexterslab_atdexterslab_tumbleweed/"
)

// Installer handles the Install to Disk pipeline.
type Installer struct {
	run           runner.CommandRunner
	reporter      events.ProgressReporter
	onboardingMgr *Manager
	mu            sync.Mutex
	running       bool
	activeTaskID  string
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
	inst.activeTaskID = taskID
	inst.mu.Unlock()

	go func() {
		defer func() {
			inst.mu.Lock()
			inst.running = false
			inst.activeTaskID = ""
			inst.mu.Unlock()
		}()
		if err := inst.runPipeline(ctx, targetDisk, imageURL, taskID); err != nil {
			log.Printf("ERROR: install to disk failed: %v", err)
			inst.report(taskID, "Error", 0, err.Error(), true)
		}
	}()

	return nil
}

// ActiveTaskID returns the task ID of the currently running install, or "" if idle.
func (inst *Installer) ActiveTaskID() string {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.activeTaskID
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

// runPipeline executes the full install pipeline using a zero-staging streaming
// architecture: HTTP GET → tee(sha256) → xzcat → dd → sync.
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

	// Phase: Download + Write (streaming)
	inst.report(taskID, "Downloading and writing image", 3, "Starting streaming install", false)
	if err := inst.streamInstall(ctx, imageURL, targetDisk, taskID); err != nil {
		return fmt.Errorf("streaming install failed: %w", err)
	}
	inst.report(taskID, "Downloading and writing image", 90, "Image written and verified", false)

	// Phase: Sync (belt-and-suspenders for USB-connected drives with write-back caching)
	inst.report(taskID, "Syncing writes", 91, "Running sync", false)
	if err := inst.run.Run(ctx, "sync"); err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}
	inst.report(taskID, "Syncing writes", 93, "Sync complete", false)

	// Phase: Boot config
	inst.report(taskID, "Configuring boot order", 96, "Setting up efibootmgr", false)
	bootOrderOK := false
	if err := inst.configureBootOrder(ctx, targetDisk); err != nil {
		log.Printf("WARN: efibootmgr failed (non-fatal): %v", err)
		inst.report(taskID, "Configuring boot order", 97,
			"Boot order configuration failed — you may need to change boot order in BIOS", false)
	} else {
		bootOrderOK = true
		if inst.onboardingMgr != nil {
			inst.onboardingMgr.MarkBootOrderConfigured()
		}
		inst.report(taskID, "Configuring boot order", 98, "Boot order configured", false)
	}

	// Phase: Complete
	if inst.onboardingMgr != nil {
		if err := inst.onboardingMgr.MarkInstallDone(); err != nil {
			return fmt.Errorf("mark install done: %w", err)
		}
	}

	completeMsg := "Installation complete — reboot to start using Piccolo from the internal disk."
	if !bootOrderOK {
		completeMsg = "Installation complete — power off the device, remove the USB drive, then turn it back on."
	}
	inst.report(taskID, "Complete", 100, completeMsg, true)
	return nil
}

// streamInstall streams the compressed image directly from HTTP to disk:
// HTTP GET → tee(sha256) → xzcat → dd. Zero staging space required.
func (inst *Installer) streamInstall(ctx context.Context, imageURL, targetDisk, taskID string) error {
	// 1. Download .sha256 checksum file (small, quick).
	expectedHash, checksumErr := inst.fetchChecksum(ctx, imageURL)

	// 2. Start HTTP GET for the image.
	client := &http.Client{Timeout: 0} // no total timeout; governed by ctx
	req, err := http.NewRequestWithContext(ctx, "GET", imageURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from image URL", resp.StatusCode)
	}
	contentLength := resp.ContentLength

	// 3. Tee the response body through SHA-256 hasher.
	hasher := sha256.New()
	teeReader := io.TeeReader(resp.Body, hasher)

	// 4. Wrap with progress tracking reader.
	progressReader := &progressTrackingReader{
		reader:        teeReader,
		contentLength: contentLength,
		taskID:        taskID,
		inst:          inst,
	}

	// 5. Re-validate disk is still unmounted before destructive write (TOCTOU mitigation).
	if err := ValidateTargetDisk(ctx, inst.run, targetDisk); err != nil {
		return fmt.Errorf("pre-write validation failed: %w", err)
	}

	// 6. Set up xzcat | dd pipeline.
	var xzStderr bytes.Buffer
	xzCmd := exec.CommandContext(ctx, "xzcat")
	xzCmd.Stdin = progressReader
	xzCmd.Stderr = &xzStderr

	pipe, err := xzCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("xzcat stdout pipe: %w", err)
	}

	ddCmd := exec.CommandContext(ctx, "dd",
		"of="+targetDisk, "bs=4M", "conv=fsync", "status=progress")
	ddCmd.Stdin = pipe
	ddCmd.Stderr = ddStderrSink{}

	// 7. Start both processes.
	if err := xzCmd.Start(); err != nil {
		return fmt.Errorf("start xzcat: %w", err)
	}
	if err := ddCmd.Start(); err != nil {
		resp.Body.Close()
		xzCmd.Process.Kill()
		xzCmd.Wait()
		return fmt.Errorf("start dd: %w", err)
	}

	// 8. Wait for dd (downstream consumer) first, then xzcat (upstream producer).
	ddErr := ddCmd.Wait()
	// Close HTTP body to unblock any pending teeReader.Read() — prevents
	// xzCmd.Wait() from hanging if dd exits before all bytes are consumed.
	resp.Body.Close()
	xzErr := xzCmd.Wait()

	if ddErr != nil {
		return fmt.Errorf("dd failed: %w", ddErr)
	}
	if xzErr != nil {
		return fmt.Errorf("xzcat failed: %w (stderr: %s)", xzErr, xzStderr.String())
	}

	// 9. Post-write checksum verification.
	if checksumErr == nil {
		actual := hex.EncodeToString(hasher.Sum(nil))
		if actual != expectedHash {
			return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actual)
		}
		log.Printf("INFO: SHA-256 checksum verified: %s", expectedHash)
	} else {
		log.Printf("WARN: checksum verification skipped: %v", checksumErr)
	}

	return nil
}

// fetchChecksum downloads and parses the .sha256 checksum file for the image.
func (inst *Installer) fetchChecksum(ctx context.Context, imageURL string) (string, error) {
	checksumURL := imageURL + ".sha256"
	req, err := http.NewRequestWithContext(ctx, "GET", checksumURL, nil)
	if err != nil {
		return "", fmt.Errorf("checksum request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("checksum download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksum file not available (status %d)", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", fmt.Errorf("read checksum: %w", err)
	}

	fields := strings.Fields(strings.TrimSpace(string(body)))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty checksum file")
	}
	return strings.ToLower(fields[0]), nil
}

// progressTrackingReader wraps a reader and reports download progress.
type progressTrackingReader struct {
	reader        io.Reader
	contentLength int64
	downloaded    int64
	taskID        string
	inst          *Installer
	lastReport    time.Time
}

func (r *progressTrackingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.downloaded += int64(n)
		// Throttle progress reports to avoid flooding.
		if time.Since(r.lastReport) > 500*time.Millisecond {
			r.lastReport = time.Now()
			pct := 3
			if r.contentLength > 0 {
				ratio := float64(r.downloaded) / float64(r.contentLength)
				if ratio > 1.0 {
					ratio = 1.0
				}
				pct = 3 + int(ratio*87) // 3 to 90
			}
			mb := r.downloaded / (1024 * 1024)
			var msg string
			if r.contentLength > 0 {
				totalMB := r.contentLength / (1024 * 1024)
				msg = fmt.Sprintf("Downloaded %d MB / %d MB", mb, totalMB)
			} else {
				msg = fmt.Sprintf("Downloaded %d MB", mb)
			}
			r.inst.report(r.taskID, "Downloading and writing image", pct, msg, false)
		}
	}
	return n, err
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

// ddStderrSink consumes dd stderr output. Progress lines (status=progress) are
// suppressed to avoid backward progress jumps with the download reader (3-90%).
// Non-progress lines (errors like "No space left on device") are logged.
type ddStderrSink struct{}

func (ddStderrSink) Write(data []byte) (int, error) {
	s := string(data)
	if !strings.Contains(s, "bytes") || !strings.Contains(s, "copied") {
		log.Printf("dd: %s", strings.TrimRight(s, "\r\n"))
	}
	return len(data), nil
}
