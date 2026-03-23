package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"piccolod/internal/fsutil"
)

// State captures the persisted authentication metadata.
type State struct {
	Initialized  bool
	PasswordHash string
}

// Storage abstracts the persistence backend for auth state.
type Storage interface {
	Load(ctx context.Context) (State, error)
	Save(ctx context.Context, state State) error
}

// Manager stores and verifies the admin credentials.
// For v1 we support a single local admin user: "admin".
type Manager struct {
	storage Storage
	mu      sync.RWMutex
	state   State
	loaded  bool
}

// NewManager constructs a manager that persists state to the given directory.
// This legacy constructor is retained for tests; production code should prefer
// NewManagerWithStorage with a persistence-backed storage layer.
func NewManager(stateDir string) (*Manager, error) {
	storage, err := newFilesystemStorage(stateDir)
	if err != nil {
		return nil, err
	}
	return NewManagerWithStorage(storage)
}

// NewManagerWithStorage constructs a manager with the provided storage backend.
func NewManagerWithStorage(storage Storage) (*Manager, error) {
	if storage == nil {
		return nil, errors.New("auth: storage required")
	}
	return &Manager{storage: storage}, nil
}

func (m *Manager) ensureLoaded(ctx context.Context) error {
	if m.loaded {
		return nil
	}
	state, err := m.storage.Load(ctx)
	if err != nil {
		return err
	}
	m.state = state
	m.loaded = true
	return nil
}

func (m *Manager) getState(ctx context.Context) (State, error) {
	m.mu.RLock()
	if m.loaded {
		st := m.state
		m.mu.RUnlock()
		return st, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureLoaded(ctx); err != nil {
		return State{}, err
	}
	return m.state, nil
}

func (m *Manager) updateState(ctx context.Context, fn func(*State) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureLoaded(ctx); err != nil {
		return err
	}
	if err := fn(&m.state); err != nil {
		return err
	}
	if err := m.storage.Save(ctx, m.state); err != nil {
		return err
	}
	return nil
}

// IsInitialized returns true if admin has been set up.
func (m *Manager) IsInitialized(ctx context.Context) (bool, error) {
	st, err := m.getState(ctx)
	if err != nil {
		return false, err
	}
	return st.Initialized, nil
}

// Setup initializes the admin password; allowed only once.
func (m *Manager) Setup(ctx context.Context, password string) error {
	if strings.TrimSpace(password) == "" {
		return errors.New("password required")
	}
	if err := validatePasswordStrength(password); err != nil {
		return err
	}
	return m.updateState(ctx, func(state *State) error {
		if state.Initialized {
			return errors.New("admin already set up")
		}
		ref, err := hashArgon2id(password)
		if err != nil {
			return err
		}
		state.Initialized = true
		state.PasswordHash = ref
		return nil
	})
}

// ChangePassword changes the admin password after verifying the old one.
func (m *Manager) ChangePassword(ctx context.Context, old, newp string) error {
	if strings.TrimSpace(old) == "" || strings.TrimSpace(newp) == "" {
		return errors.New("passwords required")
	}
	if err := validatePasswordStrength(newp); err != nil {
		return err
	}
	return m.updateState(ctx, func(state *State) error {
		if !state.Initialized {
			return errors.New("not initialized")
		}
		if !verifyArgon2id(state.PasswordHash, old) {
			return errors.New("invalid credentials")
		}
		ref, err := hashArgon2id(newp)
		if err != nil {
			return err
		}
		state.PasswordHash = ref
		return nil
	})
}

// ChangePasswordWithRecovery bypasses verification for recovery-key scenarios.
func (m *Manager) ChangePasswordWithRecovery(ctx context.Context, newp string) error {
	if strings.TrimSpace(newp) == "" {
		return errors.New("password required")
	}
	if err := validatePasswordStrength(newp); err != nil {
		return err
	}
	return m.updateState(ctx, func(state *State) error {
		if !state.Initialized {
			return errors.New("not initialized")
		}
		ref, err := hashArgon2id(newp)
		if err != nil {
			return err
		}
		state.PasswordHash = ref
		return nil
	})
}

// Verify returns true if (username=="admin" && password valid).
func (m *Manager) Verify(ctx context.Context, username, password string) (bool, error) {
	if username != "admin" {
		return false, nil
	}
	st, err := m.getState(ctx)
	if err != nil {
		return false, err
	}
	if !st.Initialized {
		return false, nil
	}
	return verifyArgon2id(st.PasswordHash, password), nil
}

// VerifyHash verifies a password against an Argon2id hash.
func (m *Manager) VerifyHash(hash, password string) bool {
	return verifyArgon2id(hash, password)
}

type fileState struct {
	Initialized bool   `json:"initialized"`
	Password    string `json:"password_hash"`
}

type filesystemStorage struct {
	path string
}

func newFilesystemStorage(stateDir string) (*filesystemStorage, error) {
	if stateDir == "" {
		stateDir = "/tmp/piccolo"
	}
	dir := filepath.Join(stateDir, "auth")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &filesystemStorage{path: filepath.Join(dir, "admin.json")}, nil
}

func (s *filesystemStorage) Load(ctx context.Context) (State, error) {
	_ = ctx
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, err
	}
	var fs fileState
	if err := json.Unmarshal(data, &fs); err != nil {
		return State{}, err
	}
	state := State{Initialized: fs.Initialized, PasswordHash: fs.Password}
	if !state.Initialized && state.PasswordHash != "" {
		state.Initialized = true
	}
	return state, nil
}

func (s *filesystemStorage) Save(ctx context.Context, state State) error {
	_ = ctx
	fs := fileState{Initialized: state.Initialized, Password: state.PasswordHash}
	data, err := json.MarshalIndent(&fs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(s.path, data, 0o600)
}

// Argon2id helpers (simple encoded format: argon2id$v=19$m=...,t=...,p=...$saltB64$hashB64)
func hashArgon2id(password string) (string, error) {
	// Soft profile defaults
	var (
		time    uint32 = 3
		memory  uint32 = 64 * 1024 // 64MB
		keyLen  uint32 = 32
		saltLen        = 16
	)
	threads := uint8(selectAuthParallelism())
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, time, memory, threads, keyLen)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memory, time, threads, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func selectAuthParallelism() int {
	cores := runtime.NumCPU()
	if cores <= 1 {
		return 1
	}
	p := cores / 2
	if p < 1 {
		p = 1
	}
	if p > 4 {
		p = 4
	}
	return p
}

func verifyArgon2id(encoded, password string) bool {
	// Expected tokens: ["argon2id", "v=19", "m=...,t=...,p=...", "saltB64", "hashB64"]
	toks := strings.Split(encoded, "$")
	if len(toks) < 5 || toks[0] != "argon2id" {
		return false
	}
	// Parse parameters
	var memory, timeIters, threads uint64
	params := strings.Split(toks[2], ",")
	for _, p := range params {
		if !strings.Contains(p, "=") {
			continue
		}
		kv := strings.SplitN(p, "=", 2)
		switch kv[0] {
		case "m":
			v, _ := strconv.ParseUint(kv[1], 10, 32)
			memory = v
		case "t":
			v, _ := strconv.ParseUint(kv[1], 10, 32)
			timeIters = v
		case "p":
			v, _ := strconv.ParseUint(kv[1], 10, 8)
			threads = v
		}
	}
	salt, err := base64.RawStdEncoding.DecodeString(toks[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(toks[4])
	if err != nil {
		return false
	}
	calc := argon2.IDKey([]byte(password), salt, uint32(timeIters), uint32(memory), uint8(threads), uint32(len(want)))
	if len(calc) != len(want) {
		return false
	}
	var v byte
	for i := range calc {
		v |= calc[i] ^ want[i]
	}
	return v == 0
}

// Session store (in-memory)
// RFC 20260122 §6.2: Each session is bound to an audience and origin
type Session struct {
	ID              string
	UserID          string // User UUID from users table
	User            string // Username for display
	Role            string // "admin" or "standard"
	CSRF            string
	ExpiresAt       int64  // unix seconds
	Audience        string // RFC 20260122 §6.2: "portal" | "app:<appname>" - binds session to specific context
	BoundOrigin         string // RFC 20260122 §6.2: Canonical origin (scheme://host[:port]) - prevents cross-origin replay
	ParentSessionID     string // RFC 20260122 §6.3: For app sessions, ID of portal session that authorized this session
	MustRegisterPasskey bool   // Set during bootstrap login over remote — user must register a passkey
}

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: make(map[string]*Session)}
}

func randString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// CreateWithUserInfo creates a new portal session with full user information.
// This method creates a session with audience="portal" for backwards compatibility.
func (s *SessionStore) CreateWithUserInfo(userID, username, role string, ttlSeconds int64) *Session {
	id := randString(32)
	csrf := randString(16)
	sess := &Session{
		ID:        id,
		UserID:    userID,
		User:      username,
		Role:      role,
		CSRF:      csrf,
		ExpiresAt: timeNow().Unix() + ttlSeconds,
		Audience:  "portal", // Default to portal audience for backwards compatibility
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	return sess
}

// CreatePortalSession creates a new portal session with origin binding per RFC 20260122 §6.2.
func (s *SessionStore) CreatePortalSession(userID, username, role, boundOrigin string, ttlSeconds int64) *Session {
	id := randString(32)
	csrf := randString(16)
	sess := &Session{
		ID:          id,
		UserID:      userID,
		User:        username,
		Role:        role,
		CSRF:        csrf,
		ExpiresAt:   timeNow().Unix() + ttlSeconds,
		Audience:    "portal",
		BoundOrigin: boundOrigin,
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	return sess
}

// AppSessionParams contains parameters for creating an app session.
type AppSessionParams struct {
	UserID          string
	Username        string
	Role            string
	AppName         string // App name for audience binding
	BoundOrigin     string // Canonical origin (scheme://host[:port])
	ParentSessionID string // Portal session that authorized this session
	TTLSeconds      int64
}

// CreateAppSession creates a new app-scoped session per RFC 20260122 §5.8.
// The session is bound to a specific app and origin for security.
func (s *SessionStore) CreateAppSession(params AppSessionParams) *Session {
	id := randString(32)
	csrf := randString(16)
	sess := &Session{
		ID:              id,
		UserID:          params.UserID,
		User:            params.Username,
		Role:            params.Role,
		CSRF:            csrf,
		ExpiresAt:       timeNow().Unix() + params.TTLSeconds,
		Audience:        "app:" + params.AppName,
		BoundOrigin:     params.BoundOrigin,
		ParentSessionID: params.ParentSessionID,
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	return sess
}

func (s *SessionStore) Get(id string) (*Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if timeNow().Unix() > sess.ExpiresAt {
		s.Delete(id)
		return nil, false
	}
	return sess, true
}

func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *SessionStore) RotateCSRF(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return "", false
	}
	sess.CSRF = randString(16)
	return sess.CSRF, true
}

// ValidatePortalSession validates a session for portal access per RFC 20260122 §6.2.
// Checks that the session has audience="portal" and validates origin binding.
func (s *SessionStore) ValidatePortalSession(id, requestOrigin string) (*Session, bool) {
	sess, ok := s.Get(id)
	if !ok {
		return nil, false
	}

	// Check audience is "portal"
	if sess.Audience != "portal" && sess.Audience != "" {
		// Empty audience for backwards compatibility with existing sessions
		return nil, false
	}

	// Check origin binding: fail closed if session has BoundOrigin but request origin is empty
	// (indicates an origin computation failure — must not bypass validation).
	if sess.BoundOrigin != "" {
		if requestOrigin == "" {
			return nil, false
		}
		if sess.BoundOrigin != requestOrigin && !originsMatchIgnoringScheme(sess.BoundOrigin, requestOrigin) {
			return nil, false
		}
	}

	return sess, true
}

// ValidateAppSession validates a session for app access per RFC 20260122 §6.2.
// Checks that the session has audience="app:<appName>" and validates origin binding.
func (s *SessionStore) ValidateAppSession(id, appName, requestOrigin string) (*Session, bool) {
	sess, ok := s.Get(id)
	if !ok {
		return nil, false
	}

	// Check audience matches the app
	expectedAudience := "app:" + appName
	if sess.Audience != expectedAudience {
		return nil, false
	}

	// Check origin binding: fail closed if session has BoundOrigin but request origin is empty
	// (indicates an origin computation failure — must not bypass validation).
	if sess.BoundOrigin != "" {
		if requestOrigin == "" {
			return nil, false
		}
		if sess.BoundOrigin != requestOrigin && !originsMatchIgnoringScheme(sess.BoundOrigin, requestOrigin) {
			return nil, false
		}
	}

	return sess, true
}

// originsMatchIgnoringScheme returns true if two origins differ only in scheme (http vs https).
// This allows sessions created on HTTP to remain valid when the user switches to HTTPS
// on the same host, which is the expected behavior for LAN HTTPS with self-signed certificates.
// Both origins are served by the same piccolod instance, so this is safe.
func originsMatchIgnoringScheme(a, b string) bool {
	normalize := func(o string) string {
		// Strip scheme
		if after, ok := strings.CutPrefix(o, "https://"); ok {
			o = after
		} else if after, ok := strings.CutPrefix(o, "http://"); ok {
			o = after
		}
		// Only strip default HTTP/HTTPS ports so http://h:80 matches https://h:443.
		// Non-default ports are preserved to maintain origin binding strength.
		o = strings.TrimSuffix(o, ":80")
		o = strings.TrimSuffix(o, ":443")
		return o
	}
	hostA := normalize(a)
	hostB := normalize(b)
	return hostA != "" && hostA == hostB
}

// DeleteWithPropagation deletes a portal session and all derived app sessions per RFC 20260122 §6.3.
// This implements logout propagation: portal logout invalidates all app sessions.
func (s *SessionStore) DeleteWithPropagation(portalSessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// First, collect all sessions with this parent (while holding the lock)
	toDelete := []string{portalSessionID}
	for id, sess := range s.sessions {
		if sess.ParentSessionID == portalSessionID {
			toDelete = append(toDelete, id)
		}
	}

	// Delete all collected sessions
	for _, id := range toDelete {
		delete(s.sessions, id)
	}
}

// FindByParent returns all sessions with the given parent session ID.
// Used for logout propagation per RFC 20260122 §6.3.
func (s *SessionStore) FindByParent(parentID string) []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Session
	for _, sess := range s.sessions {
		if sess.ParentSessionID == parentID {
			result = append(result, sess)
		}
	}
	return result
}

// ExtendTTL extends a non-expired session's lifetime by the given number of seconds.
// Returns true if the session was found and extended, false otherwise (missing or expired).
// ExtendTTLIfNeeded atomically extends the session TTL only when the remaining
// lifetime drops below threshold seconds. Returns true if the TTL was extended
// (caller should refresh the cookie). All reads/writes happen under a single
// lock acquisition to avoid data races on sess.ExpiresAt.
func (s *SessionStore) ExtendTTLIfNeeded(id string, ttlSeconds, threshold int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return false
	}
	now := timeNow().Unix()
	if now > sess.ExpiresAt {
		delete(s.sessions, id)
		return false
	}
	if sess.ExpiresAt-now >= threshold {
		return false // Still within fresh window, no extension needed.
	}
	sess.ExpiresAt = now + ttlSeconds
	return true
}

// timeNow is a small indirection for tests
var timeNow = func() time.Time { return time.Now() }

