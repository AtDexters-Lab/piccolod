package terminal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/resources/pressure"
)

const (
	DefaultIdleTimeout    = 5 * time.Minute
	DefaultScrollbackSize = 64 * 1024 // 64KB
	DefaultMaxSessions    = 16
	reaperInterval        = 30 * time.Second
)

// SessionInfo is the JSON-serializable representation of a session.
type SessionInfo struct {
	ID        string      `json:"id"`
	Kind      SessionKind `json:"kind"`
	AppName   string      `json:"app_name,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	Attached  bool        `json:"attached"`
}

// CmdFactory creates an *exec.Cmd for a new terminal session.
// The returned cmd must NOT be pre-started.
type CmdFactory func() (*exec.Cmd, error)

// Manager is a session registry that manages persistent terminal sessions.
// It implements supervisor.Component.
type Manager struct {
	mu             sync.RWMutex
	sessions       map[string]*Session
	idleTimeout    time.Duration
	scrollbackSize int
	maxSessions    int
	stopCh         chan struct{}
	wg             sync.WaitGroup
	eventBus       *events.Bus
	eventCancel    func()
	admission      *pressure.AdmissionGate
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithIdleTimeout sets the idle timeout for detached sessions.
func WithIdleTimeout(d time.Duration) ManagerOption {
	return func(m *Manager) { m.idleTimeout = d }
}

// WithScrollbackSize sets the scrollback buffer size in bytes.
func WithScrollbackSize(size int) ManagerOption {
	return func(m *Manager) { m.scrollbackSize = size }
}

// WithMaxSessions sets the maximum number of concurrent sessions.
func WithMaxSessions(max int) ManagerOption {
	return func(m *Manager) { m.maxSessions = max }
}

func WithAdmissionGate(gate *pressure.AdmissionGate) ManagerOption {
	return func(m *Manager) { m.admission = gate }
}

// NewManager creates a new session manager.
func NewManager(opts ...ManagerOption) *Manager {
	m := &Manager{
		sessions:       make(map[string]*Session),
		idleTimeout:    DefaultIdleTimeout,
		scrollbackSize: DefaultScrollbackSize,
		maxSessions:    DefaultMaxSessions,
		stopCh:         make(chan struct{}),
		admission:      pressure.DefaultAdmission,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name returns the supervisor component name.
func (m *Manager) Name() string { return "terminal-sessions" }

// Start begins the idle reaper goroutine.
func (m *Manager) Start(ctx context.Context) error {
	m.wg.Add(1)
	go m.reaperLoop()
	log.Println("terminal: session manager started")
	return nil
}

// Stop closes all sessions and waits for goroutines to exit.
func (m *Manager) Stop(ctx context.Context) error {
	if m.eventCancel != nil {
		m.eventCancel()
	}
	close(m.stopCh)

	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	for _, s := range sessions {
		if s != nil {
			s.Close("shutdown")
		}
	}

	m.wg.Wait()
	log.Println("terminal: session manager stopped")
	return nil
}

// SetEventBus subscribes to app status changes for automatic cleanup.
func (m *Manager) SetEventBus(bus *events.Bus) {
	m.eventBus = bus
	ch, cancel := bus.SubscribeWithCancel(events.TopicAppStatusChanged, 16)
	m.eventCancel = cancel
	m.wg.Add(1)
	go m.watchAppStatus(ch)
}

// Create creates a new persistent terminal session.
// Uses slot reservation to prevent TOCTOU races on maxSessions.
func (m *Manager) Create(kind SessionKind, appName string, cmdFactory CmdFactory) (*Session, error) {
	return m.CreateContext(context.Background(), kind, appName, cmdFactory)
}

func (m *Manager) CreateContext(ctx context.Context, kind SessionKind, appName string, cmdFactory CmdFactory) (*Session, error) {
	if err := m.admission.Check(ctx, pressure.WorkTerminal); err != nil {
		return nil, err
	}
	id, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("generate session ID: %w", err)
	}

	// Reserve a slot (nil placeholder) while holding the lock
	m.mu.Lock()
	if len(m.sessions) >= m.maxSessions {
		m.mu.Unlock()
		return nil, fmt.Errorf("max sessions (%d) reached", m.maxSessions)
	}
	m.sessions[id] = nil // placeholder
	m.mu.Unlock()

	// Ensure placeholder is cleaned up on any failure (including panics)
	success := false
	defer func() {
		if !success {
			m.mu.Lock()
			delete(m.sessions, id)
			m.mu.Unlock()
		}
	}()

	cmd, err := cmdFactory()
	if err != nil {
		return nil, fmt.Errorf("create command: %w", err)
	}
	// The factory may perform observation before returning the command. Recheck
	// immediately at the actual PTY child boundary so a concurrent fence cannot
	// admit a new shell from a stale decision.
	if err := m.admission.Check(ctx, pressure.WorkTerminal); err != nil {
		return nil, err
	}

	sess, err := NewSession(id, kind, appName, cmd, m.scrollbackSize)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()
	success = true

	// Auto-remove when the shell exits
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		<-sess.Done()
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
	}()

	return sess, nil
}

// CloseDetached sheds only sessions without an attached client. Active
// sessions retain their existing owner until Critical or normal completion.
func (m *Manager) CloseDetached() {
	m.mu.Lock()
	for id, session := range m.sessions {
		if session != nil && session.closeDetachedWithoutWait("task-pressure") {
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
}

func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, session := range m.sessions {
		if session != nil {
			count++
		}
	}
	return count
}

// Get retrieves a session by ID. Returns false for nil placeholders.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok || s == nil {
		return nil, false
	}
	return s, true
}

// List returns session info filtered by kind and optional app name.
func (m *Manager) List(kind SessionKind, appName string) []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []SessionInfo
	for _, s := range m.sessions {
		if s == nil {
			continue // skip reserved placeholder slots
		}
		if s.Kind != kind {
			continue
		}
		if kind == SessionKindContainer && s.AppName != appName {
			continue
		}
		result = append(result, SessionInfo{
			ID:        s.ID,
			Kind:      s.Kind,
			AppName:   s.AppName,
			CreatedAt: s.CreatedAt,
			Attached:  s.IsAttached(),
		})
	}
	return result
}

// Delete closes and removes a session. Returns true if the session existed.
func (m *Manager) Delete(id string) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok || s == nil {
		m.mu.Unlock()
		return false
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	s.Close("deleted")
	return true
}

// CleanupApp closes all sessions for a given app.
func (m *Manager) CleanupApp(appName string) {
	m.mu.Lock()
	var toClose []*Session
	for id, s := range m.sessions {
		if s != nil && s.Kind == SessionKindContainer && s.AppName == appName {
			toClose = append(toClose, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()

	for _, s := range toClose {
		s.Close("app-stopped")
	}
}

// reaperLoop periodically closes idle detached sessions.
func (m *Manager) reaperLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.reapIdle()
		case <-m.stopCh:
			return
		}
	}
}

func (m *Manager) reapIdle() {
	m.mu.Lock()
	var toClose []*Session
	for id, s := range m.sessions {
		if s != nil && s.IsIdle(m.idleTimeout) {
			toClose = append(toClose, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()

	for _, s := range toClose {
		s.Close("idle-reap")
	}
}

// watchAppStatus listens for app status changes and cleans up sessions.
func (m *Manager) watchAppStatus(ch <-chan events.Event) {
	defer m.wg.Done()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			e, ok := ev.Payload.(events.AppStatusChangedEvent)
			if !ok {
				continue
			}
			switch e.Status {
			case "stopped", "error", "uninstalled":
				m.CleanupApp(e.App)
			}
		case <-m.stopCh:
			return
		}
	}
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
