package persistence

import (
	"context"
	"time"

	"piccolod/internal/cluster"
	"piccolod/internal/fsutil"
)

// Service defines the entry point for persistence-related capabilities.
type Service interface {
	Control() ControlStore
	Volumes() VolumeManager
	Rootfs() RootfsVolumeManager
	Devices() DeviceManager
	StorageAdapter() StorageAdapter
	Consensus() ConsensusManager
	ControlVolume() VolumeHandle
	// Shutdown terminates background tasks and detaches mounted volumes.
	Shutdown(ctx context.Context) error
}

// ControlStore exposes repositories backed by the control-plane dataset.
type ControlStore interface {
	Auth() AuthRepo
	Remote() RemoteRepo
	AppState() AppStateRepo
	Users() UserRepo
	OIDCClients() OIDCClientRepo
	OIDCKeys() OIDCKeyRepo
	OIDCAuthCodes() OIDCAuthCodeRepo
	OIDCRefreshTokens() OIDCRefreshTokenRepo
	OIDCConfig() OIDCConfigRepo
	Close(ctx context.Context) error
	Revision(ctx context.Context) (uint64, string, error)
	QuickCheck(ctx context.Context) (ControlHealthReport, error)
}

// VolumeManager orchestrates encrypted volumes via the storage backend.
type VolumeManager interface {
	EnsureVolume(ctx context.Context, req VolumeRequest) (VolumeHandle, error)
	Attach(ctx context.Context, handle VolumeHandle, opts AttachOptions) error
	Detach(ctx context.Context, handle VolumeHandle) error
	DestroyVolume(ctx context.Context, id string) error
	RoleStream(volumeID string) (<-chan VolumeRole, error)
}

// RootfsVolumeManager manages block-native rootfs volumes — golden LVs,
// workspace snapshots, and service rootfs instances with idmapped mounts.
type RootfsVolumeManager interface {
	// EnsureGoldenLV creates or reuses a golden LV for the given image.
	EnsureGoldenLV(ctx context.Context, req GoldenLVRequest) (string, error)
	// CreateWorkspaceFromGolden creates a workspace rootfs from a golden LV snapshot.
	CreateWorkspaceFromGolden(ctx context.Context, req WorkspaceRootfsRequest) (RootfsHandle, error)
	// CreateServiceRootfs creates a read-only service rootfs from a golden LV snapshot.
	CreateServiceRootfs(ctx context.Context, req ServiceRootfsRequest) (RootfsHandle, error)
	// CloneWorkspace creates a clone of an existing workspace.
	CloneWorkspace(ctx context.Context, originID, cloneID string) (RootfsHandle, error)
	// AttachRootfs activates and mounts an existing rootfs volume.
	AttachRootfs(ctx context.Context, volumeID string) (RootfsHandle, error)
	// DetachRootfs unmounts and deactivates a rootfs volume.
	DetachRootfs(ctx context.Context, volumeID string) error
	// DestroyRootfs permanently removes a rootfs volume.
	DestroyRootfs(ctx context.Context, volumeID string) error
	// GarbageCollectGoldenLVs removes golden LVs with no remaining references.
	GarbageCollectGoldenLVs(ctx context.Context) error
	// ReconcileRootfsStates validates rootfs volumes on startup.
	ReconcileRootfsStates(ctx context.Context) error
	// ReadGoldenImageConfig returns the OCI image config stored alongside a golden LV.
	// The goldenID is the golden LV name (e.g., "golden-abc123").
	ReadGoldenImageConfig(ctx context.Context, goldenID string) (GoldenImageConfig, error)
	// RootfsVolumeID returns the rootfs volume ID for a given instance and mode.
	RootfsVolumeID(mode string, instanceID string) string
	// RootfsExists checks if rootfs volume metadata exists on disk for a given volume ID.
	// Used to distinguish apps installed with block-native rootfs from legacy apps.
	RootfsExists(volumeID string) bool
}

// GoldenLVRequest describes the image for a golden LV.
type GoldenLVRequest struct {
	ImageDigest string
	ImageRef    string
}

// WorkspaceRootfsRequest describes a workspace rootfs creation request.
type WorkspaceRootfsRequest struct {
	InstanceID  string
	ImageDigest string
	ImageRef    string
	IDMap       IDMapConfig
}

// ServiceRootfsRequest describes a service rootfs creation request.
type ServiceRootfsRequest struct {
	InstanceID  string
	ImageDigest string
	ImageRef    string
	IDMap       IDMapConfig
}

// RootfsHandle is a reference to a mounted rootfs volume.
type RootfsHandle struct {
	VolumeID  string
	MountPath string
	ReadOnly  bool
}

// IDMapConfig re-exports the fsutil IDMapConfig for use by callers.
type IDMapConfig = fsutil.IDMapConfig

// GoldenImageConfig holds OCI image config extracted during golden LV creation.
// Used to populate ContainerCreateSpec when --rootfs bypasses podman's image layer.
type GoldenImageConfig struct {
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        []string          `json:"env,omitempty"`
	User       string            `json:"user,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// DeviceManager discovers and manages physical storage devices.
type DeviceManager interface {
	List(ctx context.Context) ([]PhysicalDevice, error)
	Observe() (<-chan DeviceEvent, error)
}

// StorageAdapter provides low-level access to the storage backend (e.g., AionFS).
type StorageAdapter interface {
	CreateVolume(ctx context.Context, req VolumeRequest) (VolumeHandle, error)
	RemoveVolume(ctx context.Context, id string) error
}

// ConsensusManager coordinates leader election and role dissemination.
type ConsensusManager interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Repository interfaces ------------------------------------------------------

type AuthRepo interface {
	IsInitialized(ctx context.Context) (bool, error)
	SetInitialized(ctx context.Context) error
	PasswordHash(ctx context.Context) (string, error)
	SavePasswordHash(ctx context.Context, hash string) error
	Staleness(ctx context.Context) (AuthStaleness, error)
	UpdateStaleness(ctx context.Context, update AuthStalenessUpdate) error
}

type RemoteRepo interface {
	CurrentConfig(ctx context.Context) (RemoteConfig, error)
	SaveConfig(ctx context.Context, cfg RemoteConfig) error
}

type AppStateRepo interface {
	ListApps(ctx context.Context) ([]AppRecord, error)
	UpsertApp(ctx context.Context, record AppRecord) error
}

// UserRepo manages user accounts for the family user model.
type UserRepo interface {
	Create(ctx context.Context, user User) error
	Get(ctx context.Context, id string) (User, error)
	GetByUsername(ctx context.Context, username string) (User, error)
	GetByEmail(ctx context.Context, email string) (User, error)
	List(ctx context.Context) ([]User, error)
	Update(ctx context.Context, user User) error
	Delete(ctx context.Context, id string) error
	Count(ctx context.Context) (int, error)
}

// OIDCClientRepo manages OIDC client registrations for apps.
type OIDCClientRepo interface {
	Create(ctx context.Context, client OIDCClient) error
	Get(ctx context.Context, clientID string) (OIDCClient, error)
	GetByAppID(ctx context.Context, appID string) (OIDCClient, error)
	Delete(ctx context.Context, clientID string) error
	DeleteByAppID(ctx context.Context, appID string) error
	List(ctx context.Context) ([]OIDCClient, error)
}

// OIDCKeyRepo manages JWT signing keys for the OIDC provider.
type OIDCKeyRepo interface {
	Create(ctx context.Context, key OIDCKey) error
	GetActive(ctx context.Context) ([]OIDCKey, error)
	Get(ctx context.Context, kid string) (OIDCKey, error)
	Retire(ctx context.Context, kid string) error
}

// OIDCAuthCodeRepo manages short-lived authorization codes.
type OIDCAuthCodeRepo interface {
	Store(ctx context.Context, code OIDCAuthCode) error
	Consume(ctx context.Context, code string) (OIDCAuthCode, error)
	Delete(ctx context.Context, code string) error
	Cleanup(ctx context.Context) error
}

// OIDCRefreshTokenRepo manages refresh tokens for token rotation.
type OIDCRefreshTokenRepo interface {
	Store(ctx context.Context, token OIDCRefreshToken) error
	Get(ctx context.Context, token string) (OIDCRefreshToken, error)
	Revoke(ctx context.Context, token string) error
	RevokeByUserAndClient(ctx context.Context, userID, clientID string) error
	Cleanup(ctx context.Context) error
}

// Data structures -----------------------------------------------------------

type VolumeRequest struct {
	ID          string
	Class       VolumeClass
	ClusterMode ClusterMode
}

type VolumeHandle struct {
	ID       string
	MountDir string
}

type AttachOptions struct {
	Role VolumeRole
}

// VolumeClass captures high-level intent for replication/encryption policy.
type VolumeClass string

const (
	VolumeClassControl     VolumeClass = "control"
	VolumeClassApplication VolumeClass = "application"
)

type ClusterMode string

const (
	ClusterModeStateful          ClusterMode = "stateful"
	ClusterModeStatelessReadOnly ClusterMode = "stateless_read_only"
)

type VolumeRole = cluster.Role

const (
	VolumeRoleUnknown  VolumeRole = cluster.RoleUnknown
	VolumeRoleLeader   VolumeRole = cluster.RoleLeader
	VolumeRoleFollower VolumeRole = cluster.RoleFollower
)

type PhysicalDevice struct {
	ID        string
	Model     string
	SizeBytes uint64
}

type DeviceEvent struct {
	Device PhysicalDevice
	Type   DeviceEventType
}

type DeviceEventType string

const (
	DeviceEventAdded   DeviceEventType = "added"
	DeviceEventRemoved DeviceEventType = "removed"
	DeviceEventUpdated DeviceEventType = "updated"
)

type RemoteConfig struct {
	Payload []byte
}

type AppRecord struct {
	Name string
}

type ControlHealthStatus string

const (
	ControlHealthStatusOK       ControlHealthStatus = "ok"
	ControlHealthStatusDegraded ControlHealthStatus = "degraded"
	ControlHealthStatusError    ControlHealthStatus = "error"
	ControlHealthStatusUnknown  ControlHealthStatus = "unknown"
)

type ControlHealthReport struct {
	Status    ControlHealthStatus
	Message   string
	CheckedAt time.Time
}

// AuthStaleness captures credential health flags and audit timestamps.
type AuthStaleness struct {
	PasswordStale   bool
	PasswordStaleAt time.Time
	PasswordAckAt   time.Time
	RecoveryStale   bool
	RecoveryStaleAt time.Time
	RecoveryAckAt   time.Time
}

// AuthStalenessUpdate describes partial updates applied atomically.
type AuthStalenessUpdate struct {
	PasswordStale   *bool
	PasswordStaleAt *time.Time
	PasswordAckAt   *time.Time
	RecoveryStale   *bool
	RecoveryStaleAt *time.Time
	RecoveryAckAt   *time.Time
}

// User data structures -------------------------------------------------------

// UserRole defines the role of a user in the system.
type UserRole string

const (
	UserRoleAdmin    UserRole = "admin"
	UserRoleStandard UserRole = "standard"
)

// User represents a user account in the family user model.
type User struct {
	ID           string
	Username     string
	Email        string
	PasswordHash string
	Role         UserRole
	AllowedApps  []string  // JSON-encoded list of allowed app instance IDs (nil for admin)
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// OIDC data structures -------------------------------------------------------

// OIDCClientType distinguishes between app-declared and proxy OIDC clients.
type OIDCClientType string

const (
	// OIDCClientTypeApp is an app-declared OIDC client (oidc_passthrough strategy).
	OIDCClientTypeApp OIDCClientType = "app"
	// OIDCClientTypeProxy is an auto-generated proxy OIDC client (headers/protected strategies).
	OIDCClientTypeProxy OIDCClientType = "proxy"
)

// OIDCClient represents a registered OIDC client for an app.
type OIDCClient struct {
	ID        string         // client_id
	Secret    string         // hashed client_secret
	AppID     string         // app instance ID
	Type      OIDCClientType // RFC 20260122: "app" (oidc_passthrough) or "proxy" (headers/protected)
	CreatedAt time.Time
}

// OIDCKey represents a JWT signing key for the OIDC provider.
type OIDCKey struct {
	KID        string // Key ID
	Alg        string // Algorithm (e.g., "RS256")
	PrivateKey []byte // PEM-encoded private key
	CreatedAt  time.Time
	RetiredAt  *time.Time // nil = active
}

// OIDCAuthCode represents an authorization code in the OIDC flow.
type OIDCAuthCode struct {
	Code                string
	ClientID            string
	UserID              string
	RedirectURI         string
	Scope               string
	Nonce               string
	CodeChallenge       string
	CodeChallengeMethod string
	PortalSessionID     string // RFC 20260122 §6.3: Links to portal session for logout propagation
	ExpiresAt           time.Time
	CreatedAt           time.Time
}

// OIDCRefreshToken represents a refresh token for token rotation.
type OIDCRefreshToken struct {
	Token     string
	ClientID  string
	UserID    string
	Scope     string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// OIDCConfigRepo manages OIDC configuration like encryption keys.
type OIDCConfigRepo interface {
	// GetEncryptionKey returns the OIDC encryption key, or nil if not set.
	GetEncryptionKey(ctx context.Context) ([]byte, error)
	// SetEncryptionKey stores the OIDC encryption key.
	SetEncryptionKey(ctx context.Context, key []byte) error
}
