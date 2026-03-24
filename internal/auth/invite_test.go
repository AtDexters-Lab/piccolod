package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"piccolod/internal/persistence"
)

// --- Mock UserRepo ---

type mockUserRepo struct {
	mu    sync.Mutex
	users map[string]persistence.User
}

func newMockUserRepo() *mockUserRepo {
	return &mockUserRepo{users: make(map[string]persistence.User)}
}

func (r *mockUserRepo) Create(_ context.Context, u persistence.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[u.ID] = u
	return nil
}

func (r *mockUserRepo) Get(_ context.Context, id string) (persistence.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok {
		return persistence.User{}, persistence.ErrNotFound
	}
	return u, nil
}

func (r *mockUserRepo) GetByUsername(_ context.Context, username string) (persistence.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, u := range r.users {
		if u.Username == username {
			return u, nil
		}
	}
	return persistence.User{}, persistence.ErrNotFound
}

func (r *mockUserRepo) GetByEmail(_ context.Context, email string) (persistence.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, u := range r.users {
		if u.Email == email {
			return u, nil
		}
	}
	return persistence.User{}, persistence.ErrNotFound
}

func (r *mockUserRepo) List(_ context.Context) ([]persistence.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []persistence.User
	for _, u := range r.users {
		out = append(out, u)
	}
	return out, nil
}

func (r *mockUserRepo) Update(_ context.Context, u persistence.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.users[u.ID]; !ok {
		return persistence.ErrNotFound
	}
	r.users[u.ID] = u
	return nil
}

func (r *mockUserRepo) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.users[id]; !ok {
		return persistence.ErrNotFound
	}
	delete(r.users, id)
	return nil
}

func (r *mockUserRepo) Count(_ context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.users), nil
}

// --- Mock InviteTokenRepo ---

type mockInviteTokenRepo struct {
	mu     sync.Mutex
	tokens map[string]persistence.InviteToken
}

func newMockInviteTokenRepo() *mockInviteTokenRepo {
	return &mockInviteTokenRepo{tokens: make(map[string]persistence.InviteToken)}
}

func (r *mockInviteTokenRepo) Create(_ context.Context, token persistence.InviteToken) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens[token.Token] = token
	return nil
}

func (r *mockInviteTokenRepo) Get(_ context.Context, token string) (persistence.InviteToken, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tokens[token]
	if !ok {
		return persistence.InviteToken{}, persistence.ErrNotFound
	}
	return t, nil
}

func (r *mockInviteTokenRepo) Consume(_ context.Context, token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tokens[token]
	if !ok {
		return persistence.ErrNotFound
	}
	now := time.Now().UTC()
	t.ConsumedAt = &now
	r.tokens[token] = t
	return nil
}

func (r *mockInviteTokenRepo) DeleteByUser(_ context.Context, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, t := range r.tokens {
		if t.UserID == userID {
			delete(r.tokens, k)
		}
	}
	return nil
}

func (r *mockInviteTokenRepo) DeleteExpired(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for k, t := range r.tokens {
		if t.ExpiresAt.Before(now) {
			delete(r.tokens, k)
		}
	}
	return nil
}

func newTestInviteManager() (*InviteManager, *mockUserRepo, *mockInviteTokenRepo) {
	userRepo := newMockUserRepo()
	tokenRepo := newMockInviteTokenRepo()
	userMgr := NewUserManager(userRepo)
	inviteMgr := NewInviteManager(tokenRepo, userMgr)
	return inviteMgr, userRepo, tokenRepo
}

func TestCreateInvite(t *testing.T) {
	t.Run("creates_user_and_token", func(t *testing.T) {
		mgr, userRepo, tokenRepo := newTestInviteManager()
		ctx := context.Background()

		token, userInfo, err := mgr.CreateInvite(ctx, CreateInviteInput{
			Username:    "alice",
			Email:       "alice@example.com",
			AllowedApps: []string{"app1"},
		}, "admin-id")
		if err != nil {
			t.Fatalf("CreateInvite: %v", err)
		}
		if token == "" {
			t.Fatal("expected non-empty token")
		}
		if userInfo == nil || userInfo.Username != "alice" {
			t.Fatalf("unexpected userInfo: %+v", userInfo)
		}
		if userInfo.Role != persistence.UserRoleStandard {
			t.Fatalf("expected standard role, got %s", userInfo.Role)
		}
		if userInfo.HasPassword {
			t.Fatal("expected passwordless user")
		}

		// Verify user was created in the repo
		if _, err := userRepo.Get(ctx, userInfo.ID); err != nil {
			t.Fatalf("user not found in repo: %v", err)
		}

		// Verify token was stored
		stored, err := tokenRepo.Get(ctx, token)
		if err != nil {
			t.Fatalf("token not found in repo: %v", err)
		}
		if stored.UserID != userInfo.ID {
			t.Fatalf("expected token userID %s, got %s", userInfo.ID, stored.UserID)
		}
		if stored.CreatedBy != "admin-id" {
			t.Fatalf("expected createdBy admin-id, got %s", stored.CreatedBy)
		}
		if stored.ConsumedAt != nil {
			t.Fatal("expected unconsumed token")
		}
	})
}

func TestValidateInvite(t *testing.T) {
	t.Run("valid_token", func(t *testing.T) {
		mgr, _, _ := newTestInviteManager()
		ctx := context.Background()

		token, userInfo, err := mgr.CreateInvite(ctx, CreateInviteInput{
			Username: "bob",
			Email:    "bob@example.com",
		}, "admin-id")
		if err != nil {
			t.Fatalf("CreateInvite: %v", err)
		}

		username, userID, err := mgr.ValidateInvite(ctx, token)
		if err != nil {
			t.Fatalf("ValidateInvite: %v", err)
		}
		if username != "bob" {
			t.Fatalf("expected username bob, got %s", username)
		}
		if userID != userInfo.ID {
			t.Fatalf("expected userID %s, got %s", userInfo.ID, userID)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		mgr, _, _ := newTestInviteManager()
		ctx := context.Background()

		_, _, err := mgr.ValidateInvite(ctx, "nonexistent-token")
		if !errors.Is(err, ErrInviteNotFound) {
			t.Fatalf("expected ErrInviteNotFound, got %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		mgr, _, tokenRepo := newTestInviteManager()
		ctx := context.Background()

		token, _, err := mgr.CreateInvite(ctx, CreateInviteInput{
			Username: "charlie",
			Email:    "charlie@example.com",
		}, "admin-id")
		if err != nil {
			t.Fatalf("CreateInvite: %v", err)
		}

		// Manually expire the token
		tokenRepo.mu.Lock()
		stored := tokenRepo.tokens[token]
		stored.ExpiresAt = time.Now().Add(-1 * time.Hour)
		tokenRepo.tokens[token] = stored
		tokenRepo.mu.Unlock()

		_, _, err = mgr.ValidateInvite(ctx, token)
		if !errors.Is(err, ErrInviteExpired) {
			t.Fatalf("expected ErrInviteExpired, got %v", err)
		}
	})

	t.Run("consumed", func(t *testing.T) {
		mgr, _, _ := newTestInviteManager()
		ctx := context.Background()

		token, _, err := mgr.CreateInvite(ctx, CreateInviteInput{
			Username: "dave",
			Email:    "dave@example.com",
		}, "admin-id")
		if err != nil {
			t.Fatalf("CreateInvite: %v", err)
		}

		// Consume first
		if _, err := mgr.ConsumeInvite(ctx, token); err != nil {
			t.Fatalf("ConsumeInvite: %v", err)
		}

		// Validate should fail
		_, _, err = mgr.ValidateInvite(ctx, token)
		if !errors.Is(err, ErrInviteConsumed) {
			t.Fatalf("expected ErrInviteConsumed, got %v", err)
		}
	})
}

func TestConsumeInvite(t *testing.T) {
	t.Run("consumes_and_returns_user_id", func(t *testing.T) {
		mgr, _, _ := newTestInviteManager()
		ctx := context.Background()

		token, userInfo, err := mgr.CreateInvite(ctx, CreateInviteInput{
			Username: "eve",
			Email:    "eve@example.com",
		}, "admin-id")
		if err != nil {
			t.Fatalf("CreateInvite: %v", err)
		}

		userID, err := mgr.ConsumeInvite(ctx, token)
		if err != nil {
			t.Fatalf("ConsumeInvite: %v", err)
		}
		if userID != userInfo.ID {
			t.Fatalf("expected userID %s, got %s", userInfo.ID, userID)
		}
	})

	t.Run("double_consume_fails", func(t *testing.T) {
		mgr, _, _ := newTestInviteManager()
		ctx := context.Background()

		token, _, err := mgr.CreateInvite(ctx, CreateInviteInput{
			Username: "frank",
			Email:    "frank@example.com",
		}, "admin-id")
		if err != nil {
			t.Fatalf("CreateInvite: %v", err)
		}

		if _, err := mgr.ConsumeInvite(ctx, token); err != nil {
			t.Fatalf("first ConsumeInvite: %v", err)
		}

		_, err = mgr.ConsumeInvite(ctx, token)
		if !errors.Is(err, ErrInviteConsumed) {
			t.Fatalf("expected ErrInviteConsumed, got %v", err)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		mgr, _, _ := newTestInviteManager()
		ctx := context.Background()

		_, err := mgr.ConsumeInvite(ctx, "nonexistent")
		if !errors.Is(err, ErrInviteNotFound) {
			t.Fatalf("expected ErrInviteNotFound, got %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		mgr, _, tokenRepo := newTestInviteManager()
		ctx := context.Background()

		token, _, err := mgr.CreateInvite(ctx, CreateInviteInput{
			Username: "grace",
			Email:    "grace@example.com",
		}, "admin-id")
		if err != nil {
			t.Fatalf("CreateInvite: %v", err)
		}

		// Expire it
		tokenRepo.mu.Lock()
		stored := tokenRepo.tokens[token]
		stored.ExpiresAt = time.Now().Add(-1 * time.Hour)
		tokenRepo.tokens[token] = stored
		tokenRepo.mu.Unlock()

		_, err = mgr.ConsumeInvite(ctx, token)
		if !errors.Is(err, ErrInviteExpired) {
			t.Fatalf("expected ErrInviteExpired, got %v", err)
		}
	})
}

func TestReinviteUser(t *testing.T) {
	t.Run("generates_new_token_for_standard_user", func(t *testing.T) {
		mgr, _, tokenRepo := newTestInviteManager()
		ctx := context.Background()

		_, userInfo, err := mgr.CreateInvite(ctx, CreateInviteInput{
			Username: "hank",
			Email:    "hank@example.com",
		}, "admin-id")
		if err != nil {
			t.Fatalf("CreateInvite: %v", err)
		}

		newToken, err := mgr.ReinviteUser(ctx, userInfo.ID, "admin-id")
		if err != nil {
			t.Fatalf("ReinviteUser: %v", err)
		}
		if newToken == "" {
			t.Fatal("expected non-empty token")
		}

		// Old tokens for this user should be deleted, new one should exist
		tokenRepo.mu.Lock()
		count := 0
		for _, tok := range tokenRepo.tokens {
			if tok.UserID == userInfo.ID {
				count++
			}
		}
		tokenRepo.mu.Unlock()
		if count != 1 {
			t.Fatalf("expected exactly 1 token for user, got %d", count)
		}

		// Validate the new token works
		username, userID, err := mgr.ValidateInvite(ctx, newToken)
		if err != nil {
			t.Fatalf("ValidateInvite with new token: %v", err)
		}
		if username != "hank" {
			t.Fatalf("expected username hank, got %s", username)
		}
		if userID != userInfo.ID {
			t.Fatalf("expected userID %s, got %s", userInfo.ID, userID)
		}
	})

	t.Run("rejects_admin_user", func(t *testing.T) {
		mgr, userRepo, _ := newTestInviteManager()
		ctx := context.Background()

		// Create an admin user directly in the repo
		adminUser := persistence.User{
			ID:       "admin-user-1",
			Username: "adminuser",
			Email:    "admin@example.com",
			Role:     persistence.UserRoleAdmin,
		}
		if err := userRepo.Create(ctx, adminUser); err != nil {
			t.Fatalf("create admin: %v", err)
		}

		_, err := mgr.ReinviteUser(ctx, "admin-user-1", "other-admin")
		if err == nil {
			t.Fatal("expected error for admin user reinvite")
		}
		if err.Error() != "can only reinvite standard users" {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("user_not_found", func(t *testing.T) {
		mgr, _, _ := newTestInviteManager()
		ctx := context.Background()

		_, err := mgr.ReinviteUser(ctx, "nonexistent-user", "admin-id")
		if !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("expected ErrUserNotFound, got %v", err)
		}
	})
}
