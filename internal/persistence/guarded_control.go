package persistence

import (
	"context"
	"time"
)

// guardedControlStore wraps a ControlStore and enforces kernel-leader-only
// semantics for mutating operations. Reads are always allowed when unlocked.
type guardedControlStore struct {
	inner    ControlStore
	lockable lockableControlStore
	revision interface {
		Revision(context.Context) (uint64, string, error)
	}
	health interface {
		QuickCheck(context.Context) (ControlHealthReport, error)
	}
	leader   func() bool
	onCommit func(context.Context)
}

func newGuardedControlStore(inner ControlStore, leader func() bool, onCommit func(context.Context)) ControlStore {
	if inner == nil || leader == nil {
		return inner
	}
	var lockable lockableControlStore
	if l, ok := inner.(lockableControlStore); ok {
		lockable = l
	}
	var rev interface {
		Revision(context.Context) (uint64, string, error)
	}
	if r, ok := inner.(interface {
		Revision(context.Context) (uint64, string, error)
	}); ok {
		rev = r
	}
	var health interface {
		QuickCheck(context.Context) (ControlHealthReport, error)
	}
	if h, ok := inner.(interface {
		QuickCheck(context.Context) (ControlHealthReport, error)
	}); ok {
		health = h
	}
	return &guardedControlStore{inner: inner, lockable: lockable, revision: rev, health: health, leader: leader, onCommit: onCommit}
}

func (g *guardedControlStore) Auth() AuthRepo {
	return &guardedAuthRepo{store: g, repo: g.inner.Auth()}
}
func (g *guardedControlStore) Remote() RemoteRepo {
	return &guardedRemoteRepo{store: g, repo: g.inner.Remote()}
}
func (g *guardedControlStore) AppState() AppStateRepo {
	return &guardedAppStateRepo{store: g, repo: g.inner.AppState()}
}
func (g *guardedControlStore) Users() UserRepo {
	return &guardedUserRepo{store: g, repo: g.inner.Users()}
}
func (g *guardedControlStore) OIDCClients() OIDCClientRepo {
	return &guardedOIDCClientRepo{store: g, repo: g.inner.OIDCClients()}
}
func (g *guardedControlStore) OIDCKeys() OIDCKeyRepo {
	return &guardedOIDCKeyRepo{store: g, repo: g.inner.OIDCKeys()}
}
func (g *guardedControlStore) OIDCAuthCodes() OIDCAuthCodeRepo {
	return &guardedOIDCAuthCodeRepo{store: g, repo: g.inner.OIDCAuthCodes()}
}
func (g *guardedControlStore) OIDCRefreshTokens() OIDCRefreshTokenRepo {
	return &guardedOIDCRefreshTokenRepo{store: g, repo: g.inner.OIDCRefreshTokens()}
}
func (g *guardedControlStore) OIDCConfig() OIDCConfigRepo {
	return &guardedOIDCConfigRepo{store: g, repo: g.inner.OIDCConfig()}
}
func (g *guardedControlStore) WebAuthnCredentials() WebAuthnCredentialRepo {
	return &guardedWebAuthnCredRepo{store: g, repo: g.inner.WebAuthnCredentials()}
}
func (g *guardedControlStore) InviteTokens() InviteTokenRepo {
	return &guardedInviteTokenRepo{store: g, repo: g.inner.InviteTokens()}
}
func (g *guardedControlStore) Close(ctx context.Context) error { return g.inner.Close(ctx) }

func (g *guardedControlStore) Lock() {
	if g.lockable != nil {
		g.lockable.Lock()
	}
}

func (g *guardedControlStore) Unlock(ctx context.Context) error {
	if g.lockable != nil {
		return g.lockable.Unlock(ctx)
	}
	return ErrNotImplemented
}

func (g *guardedControlStore) Revision(ctx context.Context) (uint64, string, error) {
	if g.revision == nil {
		return 0, "", ErrNotImplemented
	}
	return g.revision.Revision(ctx)
}

func (g *guardedControlStore) QuickCheck(ctx context.Context) (ControlHealthReport, error) {
	if g.health == nil {
		return ControlHealthReport{Status: ControlHealthStatusUnknown, Message: "quick check unavailable", CheckedAt: time.Now().UTC()}, nil
	}
	return g.health.QuickCheck(ctx)
}

func (g *guardedControlStore) notifyCommit(ctx context.Context, err error) error {
	if err == nil && g.onCommit != nil {
		g.onCommit(ctx)
	}
	return err
}

type guardedAuthRepo struct {
	store *guardedControlStore
	repo  AuthRepo
}

type guardedRemoteRepo struct {
	store *guardedControlStore
	repo  RemoteRepo
}

type guardedAppStateRepo struct {
	store *guardedControlStore
	repo  AppStateRepo
}

func (r *guardedAuthRepo) IsInitialized(ctx context.Context) (bool, error) {
	return r.repo.IsInitialized(ctx)
}
func (r *guardedAuthRepo) PasswordHash(ctx context.Context) (string, error) {
	return r.repo.PasswordHash(ctx)
}
func (r *guardedAuthRepo) Staleness(ctx context.Context) (AuthStaleness, error) {
	return r.repo.Staleness(ctx)
}

func (r *guardedAuthRepo) SetInitialized(ctx context.Context) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.SetInitialized(ctx))
}

func (r *guardedAuthRepo) SavePasswordHash(ctx context.Context, hash string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.SavePasswordHash(ctx, hash))
}

func (r *guardedAuthRepo) UpdateStaleness(ctx context.Context, update AuthStalenessUpdate) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.UpdateStaleness(ctx, update))
}

func (r *guardedRemoteRepo) CurrentConfig(ctx context.Context) (RemoteConfig, error) {
	return r.repo.CurrentConfig(ctx)
}

func (r *guardedRemoteRepo) SaveConfig(ctx context.Context, cfg RemoteConfig) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.SaveConfig(ctx, cfg))
}

func (r *guardedAppStateRepo) ListApps(ctx context.Context) ([]AppRecord, error) {
	return r.repo.ListApps(ctx)
}

func (r *guardedAppStateRepo) UpsertApp(ctx context.Context, record AppRecord) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.UpsertApp(ctx, record))
}

// -----------------------------------------------------------------------------
// Guarded User Repository
// -----------------------------------------------------------------------------

type guardedUserRepo struct {
	store *guardedControlStore
	repo  UserRepo
}

func (r *guardedUserRepo) Get(ctx context.Context, id string) (User, error) {
	return r.repo.Get(ctx, id)
}
func (r *guardedUserRepo) GetByUsername(ctx context.Context, username string) (User, error) {
	return r.repo.GetByUsername(ctx, username)
}
func (r *guardedUserRepo) GetByEmail(ctx context.Context, email string) (User, error) {
	return r.repo.GetByEmail(ctx, email)
}
func (r *guardedUserRepo) List(ctx context.Context) ([]User, error) {
	return r.repo.List(ctx)
}
func (r *guardedUserRepo) Count(ctx context.Context) (int, error) {
	return r.repo.Count(ctx)
}
func (r *guardedUserRepo) Create(ctx context.Context, user User) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Create(ctx, user))
}
func (r *guardedUserRepo) Update(ctx context.Context, user User) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Update(ctx, user))
}
func (r *guardedUserRepo) Delete(ctx context.Context, id string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Delete(ctx, id))
}

// -----------------------------------------------------------------------------
// Guarded OIDC Client Repository
// -----------------------------------------------------------------------------

type guardedOIDCClientRepo struct {
	store *guardedControlStore
	repo  OIDCClientRepo
}

func (r *guardedOIDCClientRepo) Get(ctx context.Context, clientID string) (OIDCClient, error) {
	return r.repo.Get(ctx, clientID)
}
func (r *guardedOIDCClientRepo) GetByAppID(ctx context.Context, appID string) (OIDCClient, error) {
	return r.repo.GetByAppID(ctx, appID)
}
func (r *guardedOIDCClientRepo) List(ctx context.Context) ([]OIDCClient, error) {
	return r.repo.List(ctx)
}
func (r *guardedOIDCClientRepo) Create(ctx context.Context, client OIDCClient) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Create(ctx, client))
}
func (r *guardedOIDCClientRepo) Delete(ctx context.Context, clientID string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Delete(ctx, clientID))
}
func (r *guardedOIDCClientRepo) DeleteByAppID(ctx context.Context, appID string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.DeleteByAppID(ctx, appID))
}

// -----------------------------------------------------------------------------
// Guarded OIDC Key Repository
// -----------------------------------------------------------------------------

type guardedOIDCKeyRepo struct {
	store *guardedControlStore
	repo  OIDCKeyRepo
}

func (r *guardedOIDCKeyRepo) GetActive(ctx context.Context) ([]OIDCKey, error) {
	return r.repo.GetActive(ctx)
}
func (r *guardedOIDCKeyRepo) Get(ctx context.Context, kid string) (OIDCKey, error) {
	return r.repo.Get(ctx, kid)
}
func (r *guardedOIDCKeyRepo) Create(ctx context.Context, key OIDCKey) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Create(ctx, key))
}
func (r *guardedOIDCKeyRepo) Retire(ctx context.Context, kid string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Retire(ctx, kid))
}

// -----------------------------------------------------------------------------
// Guarded OIDC Auth Code Repository
// -----------------------------------------------------------------------------

type guardedOIDCAuthCodeRepo struct {
	store *guardedControlStore
	repo  OIDCAuthCodeRepo
}

func (r *guardedOIDCAuthCodeRepo) Store(ctx context.Context, code OIDCAuthCode) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Store(ctx, code))
}
func (r *guardedOIDCAuthCodeRepo) Consume(ctx context.Context, code string) (OIDCAuthCode, error) {
	if r.store.leader != nil && !r.store.leader() {
		return OIDCAuthCode{}, ErrNotLeader
	}
	authCode, err := r.repo.Consume(ctx, code)
	if err == nil && r.store.onCommit != nil {
		r.store.onCommit(ctx)
	}
	return authCode, err
}
func (r *guardedOIDCAuthCodeRepo) Delete(ctx context.Context, code string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Delete(ctx, code))
}
func (r *guardedOIDCAuthCodeRepo) Cleanup(ctx context.Context) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Cleanup(ctx))
}

// -----------------------------------------------------------------------------
// Guarded OIDC Refresh Token Repository
// -----------------------------------------------------------------------------

type guardedOIDCRefreshTokenRepo struct {
	store *guardedControlStore
	repo  OIDCRefreshTokenRepo
}

func (r *guardedOIDCRefreshTokenRepo) Get(ctx context.Context, token string) (OIDCRefreshToken, error) {
	return r.repo.Get(ctx, token)
}
func (r *guardedOIDCRefreshTokenRepo) Store(ctx context.Context, token OIDCRefreshToken) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Store(ctx, token))
}
func (r *guardedOIDCRefreshTokenRepo) Revoke(ctx context.Context, token string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Revoke(ctx, token))
}
func (r *guardedOIDCRefreshTokenRepo) RevokeByUserAndClient(ctx context.Context, userID, clientID string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.RevokeByUserAndClient(ctx, userID, clientID))
}
func (r *guardedOIDCRefreshTokenRepo) Cleanup(ctx context.Context) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Cleanup(ctx))
}

// -----------------------------------------------------------------------------
// Guarded OIDC Config Repository
// -----------------------------------------------------------------------------

type guardedOIDCConfigRepo struct {
	store *guardedControlStore
	repo  OIDCConfigRepo
}

func (r *guardedOIDCConfigRepo) GetEncryptionKey(ctx context.Context) ([]byte, error) {
	return r.repo.GetEncryptionKey(ctx)
}
func (r *guardedOIDCConfigRepo) SetEncryptionKey(ctx context.Context, key []byte) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.SetEncryptionKey(ctx, key))
}

// -----------------------------------------------------------------------------
// Guarded WebAuthn Credential Repository
// -----------------------------------------------------------------------------

type guardedWebAuthnCredRepo struct {
	store *guardedControlStore
	repo  WebAuthnCredentialRepo
}

func (r *guardedWebAuthnCredRepo) Get(ctx context.Context, credID string) (WebAuthnCredential, error) {
	return r.repo.Get(ctx, credID)
}
func (r *guardedWebAuthnCredRepo) ListByUser(ctx context.Context, userID string) ([]WebAuthnCredential, error) {
	return r.repo.ListByUser(ctx, userID)
}
func (r *guardedWebAuthnCredRepo) ListByUserAndRP(ctx context.Context, userID, rpID string) ([]WebAuthnCredential, error) {
	return r.repo.ListByUserAndRP(ctx, userID, rpID)
}
func (r *guardedWebAuthnCredRepo) ListByRP(ctx context.Context, rpID string) ([]WebAuthnCredential, error) {
	return r.repo.ListByRP(ctx, rpID)
}
func (r *guardedWebAuthnCredRepo) CountByUser(ctx context.Context, userID string) (int, error) {
	return r.repo.CountByUser(ctx, userID)
}
func (r *guardedWebAuthnCredRepo) CountByUserAndRP(ctx context.Context, userID, rpID string) (int, error) {
	return r.repo.CountByUserAndRP(ctx, userID, rpID)
}
func (r *guardedWebAuthnCredRepo) ListUserIDsByRP(ctx context.Context, rpID string) ([]string, error) {
	return r.repo.ListUserIDsByRP(ctx, rpID)
}
func (r *guardedWebAuthnCredRepo) Create(ctx context.Context, cred WebAuthnCredential) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Create(ctx, cred))
}
func (r *guardedWebAuthnCredRepo) UpdateAfterAuth(ctx context.Context, credID string, signCount uint32, lastUsed time.Time) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.UpdateAfterAuth(ctx, credID, signCount, lastUsed))
}
func (r *guardedWebAuthnCredRepo) UpdateFriendlyName(ctx context.Context, credID, name string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.UpdateFriendlyName(ctx, credID, name))
}
func (r *guardedWebAuthnCredRepo) Delete(ctx context.Context, credID string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Delete(ctx, credID))
}
func (r *guardedWebAuthnCredRepo) DeleteByUser(ctx context.Context, userID string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.DeleteByUser(ctx, userID))
}

// -----------------------------------------------------------------------------
// Guarded Invite Token Repository
// -----------------------------------------------------------------------------

type guardedInviteTokenRepo struct {
	store *guardedControlStore
	repo  InviteTokenRepo
}

func (r *guardedInviteTokenRepo) Get(ctx context.Context, token string) (InviteToken, error) {
	return r.repo.Get(ctx, token)
}
func (r *guardedInviteTokenRepo) Create(ctx context.Context, token InviteToken) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Create(ctx, token))
}
func (r *guardedInviteTokenRepo) Consume(ctx context.Context, token string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.Consume(ctx, token))
}
func (r *guardedInviteTokenRepo) DeleteByUser(ctx context.Context, userID string) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.DeleteByUser(ctx, userID))
}
func (r *guardedInviteTokenRepo) DeleteExpired(ctx context.Context) error {
	if r.store.leader != nil && !r.store.leader() {
		return ErrNotLeader
	}
	return r.store.notifyCommit(ctx, r.repo.DeleteExpired(ctx))
}
