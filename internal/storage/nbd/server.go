package nbd

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"piccolod/internal/runner"
)

// Server manages NBD sessions for volumes. Each volume gets a dedicated
// Unix socket and kernel nbd client connection.
type Server struct {
	run runner.CommandRunner
	cfg ServerConfig

	mu       sync.Mutex
	sessions map[string]*session // volumeID → session
}

// session tracks a single volume's NBD server state.
type session struct {
	volumeID   string
	devicePath string // backing block device (thin LV)
	socketPath string // Unix socket path
	nbdDevice  string // /dev/nbdN (set after allocation, before ready)
	sizeBytes  int64
	hooks      Hooks
	cancel     context.CancelFunc // stops the server goroutine; nil during setup
	ready      bool               // true once fully initialized
}

// NewServer creates an NBD server manager.
func NewServer(run runner.CommandRunner, cfg ServerConfig) *Server {
	if cfg.SocketDir == "" {
		cfg.SocketDir = DefaultServerConfig().SocketDir
	}
	return &Server{
		run:      run,
		cfg:      cfg,
		sessions: make(map[string]*session),
	}
}

// socketPath returns the Unix socket path for a volume.
func (s *Server) socketPath(volumeID string) string {
	return filepath.Join(s.cfg.SocketDir, volumeID+".sock")
}

// StartSession creates an NBD export for the given volume.
// The server goroutine provides block I/O passthrough from the backing device
// to the kernel nbd client via a Unix socket.
//
// The NBD layer always exists in the stack, even in single-node mode.
// In single-node mode, hooks are no-ops so the server is pure passthrough
// with minimal overhead (one extra memcpy per I/O).
func (s *Server) StartSession(ctx context.Context, volumeID, devicePath string, sizeBytes int64, hooks Hooks) (*VolumeSession, error) {
	// Reserve the session slot under the lock to prevent TOCTOU races.
	// A partial session (ready=false) acts as a reservation — concurrent
	// StartSession calls for the same volume will see it and bail out.
	s.mu.Lock()
	if _, exists := s.sessions[volumeID]; exists {
		s.mu.Unlock()
		return nil, fmt.Errorf("nbd session already exists for volume %s", volumeID)
	}
	s.sessions[volumeID] = &session{volumeID: volumeID} // reservation
	s.mu.Unlock()

	// On failure, release the reservation.
	cleanup := func() {
		s.mu.Lock()
		delete(s.sessions, volumeID)
		s.mu.Unlock()
	}

	// Ensure socket directory exists.
	if err := os.MkdirAll(s.cfg.SocketDir, 0o700); err != nil {
		cleanup()
		return nil, fmt.Errorf("create nbd socket dir: %w", err)
	}

	sockPath := s.socketPath(volumeID)

	// Clean up stale socket if present.
	_ = os.Remove(sockPath)

	// Allocate an nbd device for this volume.
	nbdDev, err := s.allocateNBDDevice(ctx)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("allocate nbd device for %s: %w", volumeID, err)
	}

	// Claim the device and populate the full session atomically.
	// Check the reservation still exists — StopSession may have removed it.
	sessCtx, cancel := context.WithCancel(context.Background())

	s.mu.Lock()
	sess, ok := s.sessions[volumeID]
	if !ok || sess == nil {
		// Reservation was removed (StopSession cancelled us). Abort.
		s.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("nbd session setup for %s was cancelled", volumeID)
	}
	sess.nbdDevice = nbdDev
	sess.devicePath = devicePath
	sess.socketPath = sockPath
	sess.sizeBytes = sizeBytes
	sess.hooks = hooks
	sess.cancel = cancel
	sess.ready = true
	s.mu.Unlock()

	// Start the userspace NBD server goroutine.
	// It listens on the Unix socket and serves block I/O from the backing device.
	go s.serveSession(sessCtx, sess)

	// Wait for the socket to appear before connecting the client.
	// The nbd-server goroutine needs a moment to bind the Unix socket.
	if err := s.waitForSocket(ctx, sockPath); err != nil {
		cancel()
		cleanup()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("wait for nbd socket %s: %w", volumeID, err)
	}

	// Connect the kernel nbd client to our Unix socket.
	if err := s.connectClient(ctx, sess); err != nil {
		cancel()
		cleanup()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("connect nbd client for %s: %w", volumeID, err)
	}

	log.Printf("nbd: session started for %s (device=%s, socket=%s, nbd=%s)",
		volumeID, devicePath, sockPath, nbdDev)

	return &VolumeSession{
		VolumeID:   volumeID,
		DevicePath: devicePath,
		SocketPath: sockPath,
		NBDDevice:  nbdDev,
		SizeBytes:  sizeBytes,
	}, nil
}

// StopSession tears down the NBD session for a volume.
// If the session is still being set up (not ready), it is cancelled and removed.
func (s *Server) StopSession(ctx context.Context, volumeID string) error {
	s.mu.Lock()
	sess, exists := s.sessions[volumeID]
	if !exists {
		s.mu.Unlock()
		return nil // already stopped
	}
	if !sess.ready {
		// Session is still being set up. Remove the reservation so
		// StartSession's cleanup path sees it gone and aborts.
		delete(s.sessions, volumeID)
		s.mu.Unlock()
		log.Printf("nbd: cancelled in-progress session setup for %s", volumeID)
		return nil
	}
	s.mu.Unlock()

	// Disconnect the kernel nbd client first.
	if err := s.disconnectClient(ctx, sess); err != nil {
		// Keep session in map so it can be retried.
		return fmt.Errorf("disconnect nbd client for %s: %w", volumeID, err)
	}

	// Disconnect succeeded — remove session and clean up.
	s.mu.Lock()
	delete(s.sessions, volumeID)
	s.mu.Unlock()

	sess.cancel()
	_ = os.Remove(sess.socketPath)

	log.Printf("nbd: session stopped for %s", volumeID)
	return nil
}

// SessionInfo returns the active session for a volume, if any.
// Only returns sessions that are fully ready (not being set up).
func (s *Server) SessionInfo(volumeID string) (*VolumeSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[volumeID]
	if !ok || !sess.ready {
		return nil, false
	}
	return &VolumeSession{
		VolumeID:   sess.volumeID,
		DevicePath: sess.devicePath,
		SocketPath: sess.socketPath,
		NBDDevice:  sess.nbdDevice,
		SizeBytes:  sess.sizeBytes,
	}, true
}

// StopAll tears down all active sessions.
func (s *Server) StopAll(ctx context.Context) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.mu.Unlock()

	for _, id := range ids {
		if err := s.StopSession(ctx, id); err != nil {
			log.Printf("WARN: nbd: stop session %s: %v", id, err)
		}
	}
}

// allocateNBDDevice finds an available /dev/nbdN device.
// Uses nbd-client -c to check if a device is in use, and cross-references
// our sessions map to avoid allocating a device we've already claimed
// (including reservations that have claimed a device but aren't ready yet).
func (s *Server) allocateNBDDevice(ctx context.Context) (string, error) {
	// Build set of devices already claimed by our sessions.
	s.mu.Lock()
	claimed := make(map[string]bool, len(s.sessions))
	for _, sess := range s.sessions {
		if sess != nil && sess.nbdDevice != "" {
			claimed[sess.nbdDevice] = true
		}
	}
	s.mu.Unlock()

	// Scan /dev/nbd0 through /dev/nbd15 for an unused device.
	for i := 0; i < 16; i++ {
		dev := fmt.Sprintf("/dev/nbd%d", i)
		if claimed[dev] {
			continue // already used by one of our sessions
		}
		// nbd-client -c returns the PID if connected, exits non-zero if free.
		if err := s.run.Run(ctx, "nbd-client", "-c", dev); err != nil {
			// Not connected — this device is available.
			return dev, nil
		}
	}
	return "", fmt.Errorf("no free nbd devices (checked /dev/nbd0 through /dev/nbd15)")
}

// serveSession runs the userspace NBD server for a single volume.
// Block I/O is served via nbd-server listening on the Unix socket.
func (s *Server) serveSession(ctx context.Context, sess *session) {
	// Use nbd-server in Unix socket mode.
	// nbd-server --unix <socket> <backing-device> --readonly (if configured)
	//
	// The actual block I/O passthrough (pread/pwrite on the backing device fd)
	// is handled by nbd-server. TRIM (NBD_CMD_TRIM) maps to BLKDISCARD on the
	// thin LV, and FLUSH (NBD_CMD_FLUSH) maps to fdatasync.
	//
	// In a future iteration, this will be replaced with a pure Go NBD server
	// (using go-nbd or custom implementation) to intercept I/O for PSFN hooks.
	// For now, we use the system nbd-server binary for reliability.

	args := []string{
		"--unix", sess.socketPath,
		sess.devicePath,
	}
	if s.cfg.ReadOnly {
		args = append(args, "--readonly")
	}

	// nbd-server runs until the context is cancelled.
	err := s.run.Run(ctx, "nbd-server", args...)
	if err != nil && ctx.Err() == nil {
		log.Printf("ERROR: nbd server for %s exited unexpectedly: %v", sess.volumeID, err)
	}
}

// connectClient connects the kernel nbd client to the Unix socket.
func (s *Server) connectClient(ctx context.Context, sess *session) error {
	// nbd-client -unix <socket> <nbd-device> -block-size 4096
	return s.run.Run(ctx, "nbd-client",
		"-unix", sess.socketPath,
		sess.nbdDevice,
		"-block-size", "4096",
	)
}

// disconnectClient disconnects the kernel nbd client.
func (s *Server) disconnectClient(ctx context.Context, sess *session) error {
	return s.run.Run(ctx, "nbd-client", "-d", sess.nbdDevice)
}

// waitForSocket polls until the Unix socket file appears or the context expires.
func (s *Server) waitForSocket(ctx context.Context, sockPath string) error {
	const (
		pollInterval = 10 * time.Millisecond
		maxWait      = 5 * time.Second
	)
	deadline := time.Now().Add(maxWait)
	for {
		if _, err := os.Stat(sockPath); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socket %s did not appear within %v", sockPath, maxWait)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(pollInterval)
	}
}
