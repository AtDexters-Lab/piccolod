package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"piccolod/internal/persistence"
)

var (
	ErrInviteNotFound = errors.New("invite not found")
	ErrInviteExpired  = errors.New("invite expired")
	ErrInviteConsumed = errors.New("invite already consumed")
)

const inviteTTL = 7 * 24 * time.Hour // 7 days

// InviteManager manages magic link invite tokens for passwordless user onboarding.
type InviteManager struct {
	tokenRepo persistence.InviteTokenRepo
	userMgr   *UserManager
}

// NewInviteManager creates a new InviteManager.
func NewInviteManager(tokenRepo persistence.InviteTokenRepo, userMgr *UserManager) *InviteManager {
	return &InviteManager{
		tokenRepo: tokenRepo,
		userMgr:   userMgr,
	}
}

// CreateInviteInput contains the input for creating an invite.
type CreateInviteInput struct {
	Username    string
	Email       string
	AllowedApps []string
}

// CreateInvite creates a passwordless standard user and returns an invite token.
func (m *InviteManager) CreateInvite(ctx context.Context, input CreateInviteInput, createdBy string) (string, *UserInfo, error) {
	// Create the passwordless user
	userInfo, err := m.userMgr.Create(ctx, CreateUserInput{
		Username:     input.Username,
		Email:        input.Email,
		Role:         persistence.UserRoleStandard,
		AllowedApps:  input.AllowedApps,
		Passwordless: true,
	})
	if err != nil {
		return "", nil, fmt.Errorf("create user: %w", err)
	}

	token, err := generateSecureToken()
	if err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}

	now := time.Now().UTC()
	inviteToken := persistence.InviteToken{
		Token:     token,
		UserID:    userInfo.ID,
		CreatedBy: createdBy,
		ExpiresAt: now.Add(inviteTTL),
		CreatedAt: now,
	}

	if err := m.tokenRepo.Create(ctx, inviteToken); err != nil {
		return "", nil, fmt.Errorf("store invite: %w", err)
	}

	return token, userInfo, nil
}

// ValidateInvite checks if a token is valid and returns the username and user ID.
func (m *InviteManager) ValidateInvite(ctx context.Context, token string) (string, string, error) {
	invite, err := m.tokenRepo.Get(ctx, token)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return "", "", ErrInviteNotFound
		}
		return "", "", err
	}

	if invite.ConsumedAt != nil {
		return "", "", ErrInviteConsumed
	}
	if time.Now().After(invite.ExpiresAt) {
		return "", "", ErrInviteExpired
	}

	user, err := m.userMgr.Get(ctx, invite.UserID)
	if err != nil {
		return "", "", fmt.Errorf("lookup user: %w", err)
	}

	return user.Username, invite.UserID, nil
}

// ConsumeInvite atomically marks the token as consumed and returns the user ID.
func (m *InviteManager) ConsumeInvite(ctx context.Context, token string) (string, error) {
	invite, err := m.tokenRepo.Get(ctx, token)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return "", ErrInviteNotFound
		}
		return "", err
	}

	if invite.ConsumedAt != nil {
		return "", ErrInviteConsumed
	}
	if time.Now().After(invite.ExpiresAt) {
		return "", ErrInviteExpired
	}

	// Atomic consumption (UPDATE WHERE consumed_at IS NULL)
	if err := m.tokenRepo.Consume(ctx, token); err != nil {
		return "", fmt.Errorf("consume invite: %w", err)
	}

	return invite.UserID, nil
}

// ReinviteUser generates a new invite token for an existing user.
func (m *InviteManager) ReinviteUser(ctx context.Context, userID, createdBy string) (string, error) {
	// Verify user exists
	user, err := m.userMgr.Get(ctx, userID)
	if err != nil {
		return "", err
	}
	if user.Role != persistence.UserRoleStandard {
		return "", errors.New("can only reinvite standard users")
	}

	// Revoke any existing unconsumed invites for this user
	_ = m.tokenRepo.DeleteByUser(ctx, userID)

	token, err := generateSecureToken()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}

	now := time.Now().UTC()
	inviteToken := persistence.InviteToken{
		Token:     token,
		UserID:    userID,
		CreatedBy: createdBy,
		ExpiresAt: now.Add(inviteTTL),
		CreatedAt: now,
	}

	if err := m.tokenRepo.Create(ctx, inviteToken); err != nil {
		return "", fmt.Errorf("store invite: %w", err)
	}

	return token, nil
}

