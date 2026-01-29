package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"piccolod/internal/persistence"
)

// UserManager provides CRUD operations for users in the multi-user model.
type UserManager struct {
	repo persistence.UserRepo
}

// NewUserManager creates a new UserManager with the given repository.
func NewUserManager(repo persistence.UserRepo) *UserManager {
	if repo == nil {
		return nil
	}
	return &UserManager{repo: repo}
}

// CreateUserInput contains the input data for creating a new user.
type CreateUserInput struct {
	Username    string
	Email       string
	Password    string
	Role        persistence.UserRole
	AllowedApps []string // Only used for standard users
}

// UserInfo represents user information without sensitive data.
type UserInfo struct {
	ID          string
	Username    string
	Email       string
	Role        persistence.UserRole
	AllowedApps []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ErrUserNotFound is returned when a user is not found.
var ErrUserNotFound = errors.New("user not found")

// ErrInvalidCredentials is returned when authentication fails.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrUsernameExists is returned when the username is already taken.
var ErrUsernameExists = errors.New("username already exists")

// ErrEmailExists is returned when the email is already taken.
var ErrEmailExists = errors.New("email already exists")

// Create creates a new user with the given input.
func (m *UserManager) Create(ctx context.Context, input CreateUserInput) (*UserInfo, error) {
	if strings.TrimSpace(input.Username) == "" {
		return nil, errors.New("username required")
	}
	if strings.TrimSpace(input.Email) == "" {
		return nil, errors.New("email required")
	}
	if strings.TrimSpace(input.Password) == "" {
		return nil, errors.New("password required")
	}
	if err := validatePasswordStrength(input.Password); err != nil {
		return nil, err
	}
	if input.Role != persistence.UserRoleAdmin && input.Role != persistence.UserRoleStandard {
		return nil, errors.New("invalid role")
	}

	// Check if username exists
	if _, err := m.repo.GetByUsername(ctx, input.Username); err == nil {
		return nil, ErrUsernameExists
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, err
	}

	// Check if email exists
	if _, err := m.repo.GetByEmail(ctx, input.Email); err == nil {
		return nil, ErrEmailExists
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, err
	}

	// Hash the password
	passwordHash, err := hashArgon2id(input.Password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	// Generate user ID
	userID, err := generateUserID()
	if err != nil {
		return nil, fmt.Errorf("generate user ID: %w", err)
	}
	now := time.Now().UTC()

	user := persistence.User{
		ID:           userID,
		Username:     input.Username,
		Email:        input.Email,
		PasswordHash: passwordHash,
		Role:         input.Role,
		AllowedApps:  input.AllowedApps,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := m.repo.Create(ctx, user); err != nil {
		return nil, err
	}

	return &UserInfo{
		ID:          user.ID,
		Username:    user.Username,
		Email:       user.Email,
		Role:        user.Role,
		AllowedApps: user.AllowedApps,
		CreatedAt:   user.CreatedAt,
		UpdatedAt:   user.UpdatedAt,
	}, nil
}

// Verify verifies the user credentials and returns the user info.
func (m *UserManager) Verify(ctx context.Context, username, password string) (*UserInfo, error) {
	user, err := m.repo.GetByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	if !verifyArgon2id(user.PasswordHash, password) {
		return nil, ErrInvalidCredentials
	}

	return &UserInfo{
		ID:          user.ID,
		Username:    user.Username,
		Email:       user.Email,
		Role:        user.Role,
		AllowedApps: user.AllowedApps,
		CreatedAt:   user.CreatedAt,
		UpdatedAt:   user.UpdatedAt,
	}, nil
}

// Get returns the user info by ID.
func (m *UserManager) Get(ctx context.Context, id string) (*UserInfo, error) {
	user, err := m.repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}

	return &UserInfo{
		ID:          user.ID,
		Username:    user.Username,
		Email:       user.Email,
		Role:        user.Role,
		AllowedApps: user.AllowedApps,
		CreatedAt:   user.CreatedAt,
		UpdatedAt:   user.UpdatedAt,
	}, nil
}

// GetByUsername returns the user info by username.
func (m *UserManager) GetByUsername(ctx context.Context, username string) (*UserInfo, error) {
	user, err := m.repo.GetByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}

	return &UserInfo{
		ID:          user.ID,
		Username:    user.Username,
		Email:       user.Email,
		Role:        user.Role,
		AllowedApps: user.AllowedApps,
		CreatedAt:   user.CreatedAt,
		UpdatedAt:   user.UpdatedAt,
	}, nil
}

// List returns all users.
func (m *UserManager) List(ctx context.Context) ([]UserInfo, error) {
	users, err := m.repo.List(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]UserInfo, len(users))
	for i, user := range users {
		result[i] = UserInfo{
			ID:          user.ID,
			Username:    user.Username,
			Email:       user.Email,
			Role:        user.Role,
			AllowedApps: user.AllowedApps,
			CreatedAt:   user.CreatedAt,
			UpdatedAt:   user.UpdatedAt,
		}
	}
	return result, nil
}

// UpdateUserInput contains the input data for updating a user.
type UpdateUserInput struct {
	ID          string
	Username    *string
	Email       *string
	Role        *persistence.UserRole
	AllowedApps *[]string
}

// Update updates an existing user.
func (m *UserManager) Update(ctx context.Context, input UpdateUserInput) (*UserInfo, error) {
	user, err := m.repo.Get(ctx, input.ID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}

	if input.Username != nil {
		if *input.Username != user.Username {
			// Check if new username is taken
			if existing, err := m.repo.GetByUsername(ctx, *input.Username); err == nil && existing.ID != input.ID {
				return nil, ErrUsernameExists
			} else if err != nil && !errors.Is(err, persistence.ErrNotFound) {
				return nil, err
			}
		}
		user.Username = *input.Username
	}

	if input.Email != nil {
		if *input.Email != user.Email {
			// Check if new email is taken
			if existing, err := m.repo.GetByEmail(ctx, *input.Email); err == nil && existing.ID != input.ID {
				return nil, ErrEmailExists
			} else if err != nil && !errors.Is(err, persistence.ErrNotFound) {
				return nil, err
			}
		}
		user.Email = *input.Email
	}

	if input.Role != nil {
		user.Role = *input.Role
	}

	if input.AllowedApps != nil {
		user.AllowedApps = *input.AllowedApps
	}

	user.UpdatedAt = time.Now().UTC()

	if err := m.repo.Update(ctx, user); err != nil {
		return nil, err
	}

	return &UserInfo{
		ID:          user.ID,
		Username:    user.Username,
		Email:       user.Email,
		Role:        user.Role,
		AllowedApps: user.AllowedApps,
		CreatedAt:   user.CreatedAt,
		UpdatedAt:   user.UpdatedAt,
	}, nil
}

// ChangePassword changes the user's password after verifying the old one.
func (m *UserManager) ChangePassword(ctx context.Context, userID, oldPassword, newPassword string) error {
	user, err := m.repo.Get(ctx, userID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return ErrUserNotFound
		}
		return err
	}

	if !verifyArgon2id(user.PasswordHash, oldPassword) {
		return ErrInvalidCredentials
	}
	if err := validatePasswordStrength(newPassword); err != nil {
		return err
	}

	passwordHash, err := hashArgon2id(newPassword)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	user.PasswordHash = passwordHash
	user.UpdatedAt = time.Now().UTC()

	return m.repo.Update(ctx, user)
}

// SetPassword sets a new password for the user without verifying the old one.
// Use with caution - typically for admin reset or recovery.
func (m *UserManager) SetPassword(ctx context.Context, userID, newPassword string) error {
	user, err := m.repo.Get(ctx, userID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return ErrUserNotFound
		}
		return err
	}

	if err := validatePasswordStrength(newPassword); err != nil {
		return err
	}

	passwordHash, err := hashArgon2id(newPassword)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	user.PasswordHash = passwordHash
	user.UpdatedAt = time.Now().UTC()

	return m.repo.Update(ctx, user)
}

// Delete deletes a user by ID.
func (m *UserManager) Delete(ctx context.Context, id string) error {
	if err := m.repo.Delete(ctx, id); err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return ErrUserNotFound
		}
		return err
	}
	return nil
}

// IsAppAllowed checks if a user is allowed to access an app.
func (m *UserManager) IsAppAllowed(ctx context.Context, userID, appID string) (bool, error) {
	user, err := m.repo.Get(ctx, userID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return false, ErrUserNotFound
		}
		return false, err
	}

	// Admin has access to all apps
	if user.Role == persistence.UserRoleAdmin {
		return true, nil
	}

	// Standard user: check allowed_apps
	if user.AllowedApps == nil {
		return false, nil
	}
	for _, allowed := range user.AllowedApps {
		if allowed == appID {
			return true, nil
		}
	}
	return false, nil
}

// Count returns the total number of users.
func (m *UserManager) Count(ctx context.Context) (int, error) {
	return m.repo.Count(ctx)
}

// generateUserID generates a random UUID v4 string.
func generateUserID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
