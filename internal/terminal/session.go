package terminal

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
)

// SessionKind distinguishes host vs container terminal sessions.
type SessionKind string

const (
	SessionKindHost      SessionKind = "host"
	SessionKindContainer SessionKind = "container"
)

// Session is a persistent PTY session whose lifetime is decoupled from any
// single WebSocket connection. Clients attach and detach without killing the
// underlying shell process.
type Session struct {
	ID        string
	Kind      SessionKind
	AppName   string // empty for host sessions
	CreatedAt time.Time

	mu         sync.Mutex
	lastAttach time.Time
	ptmx       *os.File
	cmd        *exec.Cmd
	cancel     context.CancelFunc // defense-in-depth: cancels cmd context
	scrollback *RingBuffer
	client     *Client // nil when detached
	closed     bool
	doneCh     chan struct{}  // closed when PTY process exits
	outputWg   sync.WaitGroup // tracks outputLoop goroutine
}

// NewSession starts a PTY for cmd and returns a persistent session.
// The cmd must NOT be pre-started. scrollbackSize is in bytes.
func NewSession(id string, kind SessionKind, appName string, cmd *exec.Cmd, scrollbackSize int) (*Session, error) {
	// Create a cancellable context for defense-in-depth cleanup (e.g. killing
	// podman exec processes even if ptmx close alone doesn't propagate).
	ctx, cancel := context.WithCancel(context.Background())
	cmd2 := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	cmd2.Env = cmd.Env
	cmd2.Dir = cmd.Dir
	cmd2.WaitDelay = cmd.WaitDelay
	cmd2.SysProcAttr = cmd.SysProcAttr
	cmd2.ExtraFiles = cmd.ExtraFiles

	ptmx, err := pty.Start(cmd2)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("pty start: %w", err)
	}

	s := &Session{
		ID:         id,
		Kind:       kind,
		AppName:    appName,
		CreatedAt:  time.Now(),
		lastAttach: time.Now(), // prevents immediate reaping
		ptmx:       ptmx,
		cmd:        cmd2,
		cancel:     cancel,
		scrollback: NewRingBuffer(scrollbackSize),
		doneCh:     make(chan struct{}),
	}
	s.outputWg.Add(1)
	go s.outputLoop()

	log.Printf("terminal: session %s created (kind=%s app=%s)", id, kind, appName)
	return s, nil
}

// outputLoop reads PTY output, writes to scrollback, and forwards to any
// attached client. Exits when the PTY read returns an error (shell exit).
func (s *Session) outputLoop() {
	defer s.outputWg.Done()
	defer close(s.doneCh)

	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])

			s.mu.Lock()
			s.scrollback.Write(data)
			c := s.client
			s.mu.Unlock()

			if c != nil {
				c.SendOutput(data)
			}
		}
		if err != nil {
			s.mu.Lock()
			s.closed = true
			c := s.client
			s.mu.Unlock()

			// Close PTY fd, cancel context, and reap child process
			_ = s.ptmx.Close()
			s.cancel()
			_ = s.cmd.Wait()

			if c != nil {
				c.SendSessionEnded()
			}
			log.Printf("terminal: session %s closed (reason: shell-exit)", s.ID)
			return
		}
	}
}

// Attach connects a WebSocket client to this session, replaying the scrollback.
// If another client is already attached, it is detached (last-writer-wins).
func (s *Session) Attach(c *Client) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("session %s is closed", s.ID)
	}
	// Capture old client for displacement outside the lock to avoid
	// holding s.mu during network I/O (Detach sends a close frame).
	old := s.client
	snapshot := s.scrollback.Snapshot()
	s.client = c
	s.lastAttach = time.Now()
	s.mu.Unlock()

	// Displace existing client outside the lock
	if old != nil {
		old.Detach()
	}

	// Replay scrollback (SendOutput has its own wsMu)
	if len(snapshot) > 0 {
		c.SendOutput(snapshot)
	}

	log.Printf("terminal: session %s attached", s.ID)
	return nil
}

// DetachClient disconnects the given client without killing the PTY.
// Only the specified client is detached; if a different client is now
// attached (last-writer-wins race), this is a no-op.
func (s *Session) DetachClient(c *Client) {
	s.mu.Lock()
	if s.client != c {
		s.mu.Unlock()
		return
	}
	s.client = nil
	s.lastAttach = time.Now()
	s.mu.Unlock()
	log.Printf("terminal: session %s detached", s.ID)
}

// Close kills the PTY process and waits for the output loop to exit. Idempotent.
func (s *Session) Close(reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		// outputLoop is the sole cmd.Wait owner. A concurrent close may observe
		// closed after that loop has claimed shutdown but before it has reaped
		// the child, so still join it here.
		s.outputWg.Wait()
		return
	}
	s.closed = true
	_ = s.ptmx.Close()
	s.cancel()
	c := s.client
	s.client = nil
	s.mu.Unlock()

	// Wait for outputLoop to exit. It closes the PTY, cancels the command
	// context, and is the single owner of cmd.Wait.
	s.outputWg.Wait()

	if c != nil {
		c.SendSessionEnded()
	}

	if reason != "shell-exit" {
		log.Printf("terminal: session %s closed (reason: %s)", s.ID, reason)
	}
}

// closeDetachedWithoutWait initiates shutdown only when no client is attached
// and returns without joining outputLoop/cmd.Wait. The existing output loop
// remains the sole child reaper. This is the task-pressure shedding path; an
// explicit manager shutdown still uses Close and joins every session.
func (s *Session) closeDetachedWithoutWait(reason string) bool {
	s.mu.Lock()
	if s.closed || s.client != nil {
		s.mu.Unlock()
		return false
	}
	s.closed = true
	_ = s.ptmx.Close()
	s.cancel()
	s.mu.Unlock()

	if reason != "shell-exit" {
		log.Printf("terminal: session %s close requested (reason: %s)", s.ID, reason)
	}
	return true
}

// Done returns a channel that is closed when the PTY process exits.
func (s *Session) Done() <-chan struct{} {
	return s.doneCh
}

// IsAttached reports whether a client is currently connected.
func (s *Session) IsAttached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client != nil
}

// IsClosed reports whether the session has been closed.
func (s *Session) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// IsIdle reports whether the session is detached and idle for longer than d.
// All fields are read under s.mu to avoid data races.
func (s *Session) IsIdle(d time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client == nil && !s.closed && time.Since(s.lastAttach) > d
}

// WriteToPTY sends data to the PTY's stdin.
func (s *Session) WriteToPTY(data []byte) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("session closed")
	}
	s.mu.Unlock()
	_, err := s.ptmx.Write(data)
	return err
}

// Resize changes the PTY window size.
func (s *Session) Resize(cols, rows int) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("session closed")
	}
	s.mu.Unlock()
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}
