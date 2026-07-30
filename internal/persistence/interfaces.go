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
	// AttachAppLogs ensures+attaches the singleton app-logs store on demand
	// (used by first-setup, where the data pool is created after the initial
	// unlock so the unlock-time attach found no pool). Idempotent; no-op when
	// locked.
	AttachAppLogs(ctx context.Context)
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
	WebAuthnCredentials() WebAuthnCredentialRepo
	InviteTokens() InviteTokenRepo
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

	// AttachStateOf returns the kernel-state-derived attach state. Lock-free
	// advisory probe; transition callers serialize internally.
	//
	// IMPORTANT: this primitive returns the raw 7-state partition; the caller
	// must encode its own state→action policy. Different consumers want
	// different mappings (a "Foreign" mount is a hard-error for lock-teardown
	// but a normal "skip" for Shutdown). Misclassifying that mapping is the
	// failure mode that motivated the typed advisory probe below
	// (IsAttachedAdvisory) and the intra-package detach orchestration
	// policies in service.go (Module.detachVolumeStrict /
	// Module.detachVolumeBestEffort). External consumers of this interface
	// should prefer IsAttachedAdvisory for the common "is this volume
	// currently usable?" question; intra-package code that needs detach
	// semantics should use the typed orchestration wrappers rather than
	// hand-rolling a switch on AttachState.
	//
	// Use AttachStateOf directly only when none of the above fits — typically
	// a transition reconciler that genuinely needs per-state dispatch.
	AttachStateOf(ctx context.Context, volumeID string) (AttachState, error)

	// IsAttachedAdvisory reports whether the volume is currently in
	// AttachStateAttached. Lock-free; probe errors and ambiguous states
	// return false (the volume is not safely usable). Use for "is this
	// volume currently usable?" filter cases — auto-grow's volume
	// enumeration, scratch-flatten reuse check, etc.
	IsAttachedAdvisory(ctx context.Context, volumeID string) bool
}

// OrphanReconciler cleans up LVs that have no corresponding metadata.
// Used post-unlock when the LVM pool is active.
type OrphanReconciler interface {
	ReconcileOrphanLVs(ctx context.Context) error
}

// WorkspaceResizeMonitor manages automatic workspace volume resizing.
type WorkspaceResizeMonitor interface {
	StartWorkspaceResizeMonitor()
	StopWorkspaceResizeMonitor()
}

// RootfsImageIdentity is the image provenance recorded in rootfs volume
// metadata. Callers use this as proof for preserving an active rootfs.
type RootfsImageIdentity struct {
	VolumeID        string
	BaseImageRef    string
	BaseImageDigest string
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
	// When idmap is non-nil, it overrides the origin's IDMap in the clone metadata.
	CloneWorkspace(ctx context.Context, originID, cloneID string, idmap *IDMapConfig) (RootfsHandle, error)
	// ListClones returns volume IDs of clones created from the given origin volume.
	ListClones(ctx context.Context, originVolumeID string) ([]string, error)
	// AttachRootfs activates and mounts an existing rootfs volume.
	AttachRootfs(ctx context.Context, volumeID string) (RootfsHandle, error)
	// DetachRootfs unmounts and deactivates a rootfs volume.
	DetachRootfs(ctx context.Context, volumeID string) error
	// DestroyRootfs permanently removes a rootfs volume.
	DestroyRootfs(ctx context.Context, volumeID string) error
	// GarbageCollectGoldenLVs removes golden LVs with no remaining references.
	GarbageCollectGoldenLVs(ctx context.Context) error
	// HydrateGoldenMetadata loads only durable metadata and image config. It
	// must not inspect or mutate LVM, mounts, mappers, or snapshots.
	HydrateGoldenMetadata(ctx context.Context) error
	// RunPhysicalMaintenance performs one finite post-Ready pass using one
	// shared strict LV inventory. Generic orphan cleanup is not part of it.
	RunPhysicalMaintenance(ctx context.Context) error
	// ReadGoldenImageConfig returns the OCI image config stored alongside a golden LV.
	// The goldenID is the golden LV name (e.g., "golden-abc123").
	ReadGoldenImageConfig(ctx context.Context, goldenID string) (GoldenImageConfig, error)
	// RootfsVolumeID returns the rootfs volume ID for a given instance and mode.
	RootfsVolumeID(mode string, instanceID string) string
	// RootfsExists checks if rootfs volume metadata exists on disk for a given volume ID.
	// Used to distinguish apps installed with block-native rootfs from legacy apps.
	RootfsExists(volumeID string) bool
	// ReadRootfsImageIdentity returns the image provenance recorded for an
	// existing rootfs volume.
	ReadRootfsImageIdentity(volumeID string) (RootfsImageIdentity, error)
	// FindGoldenByImageRef checks the in-memory golden LV cache for a completed
	// golden LV matching the given image reference (e.g., "vaultwarden/server:latest").
	// When multiple golden LVs match (mutable tag pulled at different times),
	// returns the most recently created one. Returns the image digest and golden
	// LV ID if found. Used to skip redundant image pulls when a golden LV already
	// exists for the same image.
	FindGoldenByImageRef(imageRef string) (digest string, goldenID string, found bool)
	// ResizeWorkspace grows a workspace volume to the specified size.
	// Handles LV resize, LUKS resize (if mounted), and btrfs filesystem resize.
	ResizeWorkspace(ctx context.Context, volumeID string, newSizeBytes int64) error

	// ResizeApplication grows a per-app application volume (service-data)
	// to the specified size. Uses ext4 online resize (resize2fs) rather
	// than btrfs. Same thin-pool capacity and cooldown safeguards as
	// ResizeWorkspace.
	ResizeApplication(ctx context.Context, volumeID string, newSizeBytes int64) error
}

// GoldenContentManager extends the block-native rootfs owner with generic
// reconstructible content and direct read-only artifact references. Keeping it
// interface-segregated avoids forcing non-artifact test doubles to implement
// behavior they never exercise.
type GoldenContentManager interface {
	EnsureGoldenContent(ctx context.Context, req GoldenContentRequest) (GoldenContentHandle, error)
	CreateArtifactReference(ctx context.Context, req ArtifactReferenceRequest) (ArtifactHandle, error)
	AttachArtifactReference(ctx context.Context, referenceID string) (ArtifactHandle, error)
	DetachArtifactReference(ctx context.Context, referenceID string) error
	DestroyArtifactReference(ctx context.Context, referenceID string) error
	GarbageCollectArtifactReferences(ctx context.Context, retained map[string]struct{}) error
}

// GoldenContentIdentity is the complete durable identity used for Ready reuse.
// Consumption mode is intentionally absent: the same OCI image projection can
// back a rootfs snapshot or a read-only artifact attachment.
type GoldenContentIdentity struct {
	SourceKind       string `json:"source_kind"`
	ResolvedIdentity string `json:"resolved_identity"`
	Projection       string `json:"projection"`
}

const (
	GoldenSourceOCI         = "oci"
	GoldenSourceHuggingFace = "huggingface"

	GoldenProjectionOCIImageRootfs = "oci-image-rootfs"
	GoldenProjectionOCIArtifact    = "oci-artifact-root"
	GoldenProjectionHuggingFace    = "huggingface-path"
)

// GoldenMaterializationResult carries optional OCI image configuration. Raw
// artifacts do not populate ImageConfig.
type GoldenMaterializationResult struct {
	ImageConfig *GoldenImageConfig
}

// GoldenContentRequest describes one source-specific adapter invocation. The
// caller resolves and verifies source semantics; persistence owns staging,
// publication, reuse, references, and GC.
type GoldenContentRequest struct {
	Identity          GoldenContentIdentity
	SourceRef         string
	SizeHint          int64
	PreferredGoldenID string
	Materialize       func(ctx context.Context, targetDir string) (GoldenMaterializationResult, error)
}

// GoldenContentHandle identifies verified Ready content.
type GoldenContentHandle struct {
	GoldenID string
	Identity GoldenContentIdentity
}

// ArtifactReferenceRequest creates a durable app-owned reference before its
// live attachment. ReferenceID must be stable for the app/artifact generation.
type ArtifactReferenceRequest struct {
	ReferenceID string
	GoldenID    string
	Identity    GoldenContentIdentity
	IDMap       IDMapConfig
}

// ArtifactHandle identifies one app-private read-only view of a golden LV.
type ArtifactHandle struct {
	MountPath string
	Created   bool
}

// ArtifactContentMissingError preserves the exact recorded identity when a
// durable reference survives loss of its local golden LV. App lifecycle code
// can reconstruct that identity without following a mutable source.
type ArtifactContentMissingError struct {
	ReferenceID string
	GoldenID    string
	Identity    GoldenContentIdentity
}

func (e *ArtifactContentMissingError) Error() string {
	return "artifact reference " + e.ReferenceID + " is missing local golden content " + e.GoldenID
}

// GoldenContentMissingError tells an OCI image caller that metadata identified
// an exact cached digest but the local golden LV was proven absent. The caller
// must run its verified pull path before asking persistence to reconstruct it.
type GoldenContentMissingError struct {
	GoldenID string
}

func (e *GoldenContentMissingError) Error() string {
	return "cached golden content " + e.GoldenID + " is physically absent"
}

// GoldenLVRequest describes the image for a golden LV.
type GoldenLVRequest struct {
	ImageDigest   string
	ImageRef      string
	ImageSizeHint int64  // uncompressed image size; when > 0, skips imageSizeFn pull
	PrePulledDir  string // when non-empty, podman root dir with the image already pulled — flattenFn reuses it

	// Generic fields are populated by EnsureGoldenContent. Existing image
	// callers leave them empty and receive the canonical OCI image identity.
	Identity          *GoldenContentIdentity
	SourceRef         string
	ContentSizeHint   int64
	PreferredGoldenID string
	Materialize       func(ctx context.Context, targetDir string) (GoldenMaterializationResult, error)
}

// WorkspaceRootfsRequest describes a workspace rootfs creation request.
type WorkspaceRootfsRequest struct {
	InstanceID        string
	ImageDigest       string
	ImageRef          string
	PreferredGoldenID string
	IDMap             IDMapConfig
	ImageSizeHint     int64  // uncompressed image size; when > 0, skips imageSizeFn pull
	PrePulledDir      string // podman root dir with image already pulled
}

// ServiceRootfsRequest describes a service rootfs creation request.
type ServiceRootfsRequest struct {
	InstanceID        string
	ServiceName       string // per-service rootfs; empty = legacy single-rootfs
	ImageDigest       string
	ImageRef          string
	PreferredGoldenID string
	IDMap             IDMapConfig
	VolumeID          string // optional: override derived volume ID (for versioned updates, RFC 20260302)
	ImageSizeHint     int64  // uncompressed image size; when > 0, skips imageSizeFn pull
	PrePulledDir      string // podman root dir with image already pulled
}

// RootfsHandle is a reference to a mounted rootfs volume.
type RootfsHandle struct {
	VolumeID  string
	MountPath string
	ReadOnly  bool
	GoldenLV  string // golden LV this rootfs was snapshotted from (populated during attach)
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

// KeyslotProvisioner provisions LUKS keyslots on all v3 volumes via the
// async reconciler (RFC 20260510). Implemented by Module when backed by
// luksVolumeManager.
type KeyslotProvisioner interface {
	// ProvisionLUKSKeyslotSync rotates a slot synchronously across all
	// v3 volumes — the legacy N×Argon2id path. Reserved for callers
	// that need the rotation completed BEFORE returning (e.g., the
	// locked /reset-password handler that must drain slot-1 before
	// relocking the SDEK). Stamps per-volume kskey_id = stampKeyID on
	// success so the async reconciler does not redundantly re-provision
	// later. stampKeyID == "" disables stamping (test-only).
	// Normal callers should use WriteKeyslotBlob + the async reconciler.
	ProvisionLUKSKeyslotSync(ctx context.Context, slot int, passphrase []byte, stampKeyID string) error

	// WriteKeyslotBlob persists a pending-passphrase blob for the keyslot
	// reconciler to drain (RFC 20260510 §Architecture). Called by the
	// synchronous handler BEFORE committing the persistent state change
	// (keyset.json for slot 2, userManager.ChangePassword for slot 1) so
	// a blob-write failure leaves the operator's prior state authoritative.
	//
	// slot=1 (admin password) | slot=2 (recovery mnemonic).
	// keyID is the generation fingerprint — crypt.FingerprintPasswordHash
	// for slot 1, crypt.Manager.RecoveryKeyID() for slot 2.
	WriteKeyslotBlob(ctx context.Context, slot KeyslotSlot, keyID string, passphrase []byte) error

	// WriteKeyslotBlobWithKey is the D11 prepare-hook variant — caller is
	// inside crypt.Manager's write lock and would deadlock if we called
	// back through EncryptWithAAD's RLock. Uses the supplied SDEK directly.
	WriteKeyslotBlobWithKey(ctx context.Context, sdek []byte, slot KeyslotSlot, keyID string, passphrase []byte) error

	// CountKeyslotUnprovisioned returns the slot 1 + slot 2 counts of
	// v3 LUKS volumes stamped "unprovisioned" (RFC 20260510 S7) in a
	// single metadata walk. Counted from on-disk metadata so the boot
	// surface reflects current truth, not stale reconciler-pass state.
	CountKeyslotUnprovisioned() (slot1, slot2 int, err error)
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
	VolumeClassEphemeral   VolumeClass = "ephemeral"
	// VolumeClassAppLogs is the singleton store holding all apps' persistent
	// service logs. Backed like an application volume (v3
	// service-data thin LV on the data pool, per-volume LUKS2), but it is one
	// shared volume keyed by AppLogsVolumeID, mounted for the whole unlocked
	// session and disjoint from any app/container lifecycle.
	VolumeClassAppLogs VolumeClass = "applogs"
)

// AppLogsVolumeID is the volume ID of the singleton app-logs store. Its LV
// name is appLVPrefix+ID = "vol-logstore", deliberately outside the
// "vol-app-{instanceID}" namespace that per-app volumes and RollbackDataVolume
// operate on, so a per-app rollback can never rename it.
const AppLogsVolumeID = "logstore"

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
	AllowedApps  []string // JSON-encoded list of allowed app instance IDs (nil for admin)
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

// WebAuthn data structures ---------------------------------------------------

// WebAuthnCredential represents a stored WebAuthn credential for a user.
type WebAuthnCredential struct {
	ID              string   // base64url-encoded credential ID
	UserID          string   // FK to users.id
	PublicKey       []byte   // CBOR-encoded public key
	AttestationType string   // e.g., "none", "packed"
	Transports      []string // e.g., ["internal", "usb"]
	SignCount       uint32
	RPID            string // Relying Party ID this credential is bound to
	AAGUID          []byte // Authenticator AAGUID
	FriendlyName    string // User-assigned label
	BackupEligible  bool   // WebAuthn BE flag — must not change after registration
	BackupState     bool   // WebAuthn BS flag — may change (credential synced/backed up)
	CreatedAt       time.Time
	LastUsedAt      time.Time
}

// InviteToken represents a magic link invite for passwordless user onboarding.
type InviteToken struct {
	Token      string
	UserID     string // FK to users.id (pre-created passwordless user)
	CreatedBy  string // Admin user_id who created the invite
	ExpiresAt  time.Time
	ConsumedAt *time.Time // nil until used
	CreatedAt  time.Time
}

// OIDCConfigRepo manages OIDC configuration like encryption keys.
type OIDCConfigRepo interface {
	// GetEncryptionKey returns the OIDC encryption key, or nil if not set.
	GetEncryptionKey(ctx context.Context) ([]byte, error)
	// SetEncryptionKey stores the OIDC encryption key.
	SetEncryptionKey(ctx context.Context, key []byte) error
}

// WebAuthnCredentialRepo manages WebAuthn/Passkey credentials.
type WebAuthnCredentialRepo interface {
	Create(ctx context.Context, cred WebAuthnCredential) error
	Get(ctx context.Context, credID string) (WebAuthnCredential, error)
	ListByUser(ctx context.Context, userID string) ([]WebAuthnCredential, error)
	ListByUserAndRP(ctx context.Context, userID, rpID string) ([]WebAuthnCredential, error)
	ListByRP(ctx context.Context, rpID string) ([]WebAuthnCredential, error)
	UpdateAfterAuth(ctx context.Context, credID string, signCount uint32, lastUsed time.Time) error
	UpdateFriendlyName(ctx context.Context, credID, name string) error
	Delete(ctx context.Context, credID string) error
	DeleteByUser(ctx context.Context, userID string) error
	CountByUser(ctx context.Context, userID string) (int, error)
	CountByUserAndRP(ctx context.Context, userID, rpID string) (int, error)
	// ListUserIDsByRP returns the distinct set of user IDs with at least one
	// credential for rpID. Cheaper than ListByRP (no blob columns).
	ListUserIDsByRP(ctx context.Context, rpID string) ([]string, error)
}

// InviteTokenRepo manages magic link invite tokens.
type InviteTokenRepo interface {
	Create(ctx context.Context, token InviteToken) error
	Get(ctx context.Context, token string) (InviteToken, error)
	Consume(ctx context.Context, token string) error
	DeleteByUser(ctx context.Context, userID string) error
	DeleteExpired(ctx context.Context) error
}
