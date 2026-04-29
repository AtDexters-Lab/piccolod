package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/auth"
	"piccolod/internal/persistence"
)

// UserResponse represents a user in API responses (without sensitive data).
type UserResponse struct {
	ID          string   `json:"id"`
	Username    string   `json:"username"`
	Email       string   `json:"email"`
	Role        string   `json:"role"`
	AllowedApps []string `json:"allowed_apps,omitempty"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

func userToResponse(u *auth.UserInfo) UserResponse {
	return UserResponse{
		ID:          u.ID,
		Username:    u.Username,
		Email:       u.Email,
		Role:        string(u.Role),
		AllowedApps: u.AllowedApps,
		CreatedAt:   u.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:   u.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// handleListUsers: GET /api/v1/users
func (s *GinServer) handleListUsers(c *gin.Context) {
	if s.userManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user management not available"})
		return
	}

	users, err := s.userManager.List(c.Request.Context())
	if err != nil {
		if errors.Is(err, persistence.ErrLocked) {
			c.JSON(http.StatusLocked, gin.H{"error": "storage locked"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list users"})
		return
	}

	resp := make([]UserResponse, len(users))
	for i, u := range users {
		resp[i] = UserResponse{
			ID:          u.ID,
			Username:    u.Username,
			Email:       u.Email,
			Role:        string(u.Role),
			AllowedApps: u.AllowedApps,
			CreatedAt:   u.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			UpdatedAt:   u.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		}
	}

	c.JSON(http.StatusOK, gin.H{"users": resp})
}

// handleGetUser: GET /api/v1/users/:id
func (s *GinServer) handleGetUser(c *gin.Context) {
	if s.userManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user management not available"})
		return
	}

	id := c.Param("id")
	user, err := s.userManager.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		if errors.Is(err, persistence.ErrLocked) {
			c.JSON(http.StatusLocked, gin.H{"error": "storage locked"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get user"})
		return
	}

	c.JSON(http.StatusOK, userToResponse(user))
}

// CreateUserRequest defines the request body for creating a user.
type CreateUserRequest struct {
	Username    string   `json:"username" binding:"required"`
	Email       string   `json:"email" binding:"required"`
	Password    string   `json:"password" binding:"required"`
	Role        string   `json:"role" binding:"required,oneof=admin standard"`
	AllowedApps []string `json:"allowed_apps,omitempty"`
}

// handleCreateUser: POST /api/v1/users
func (s *GinServer) handleCreateUser(c *gin.Context) {
	if s.userManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user management not available"})
		return
	}

	var req CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	input := auth.CreateUserInput{
		Username:    req.Username,
		Email:       req.Email,
		Password:    req.Password,
		Role:        persistence.UserRole(req.Role),
		AllowedApps: req.AllowedApps,
	}

	ctx, cancel := s.opContext(c, 30*time.Second)
	defer cancel()
	user, err := s.userManager.Create(ctx, input)
	if err != nil {
		if errors.Is(err, auth.ErrUsernameExists) {
			c.JSON(http.StatusConflict, gin.H{"error": "username already exists"})
			return
		}
		if errors.Is(err, auth.ErrEmailExists) {
			c.JSON(http.StatusConflict, gin.H{"error": "email already exists"})
			return
		}
		if errors.Is(err, persistence.ErrLocked) {
			c.JSON(http.StatusLocked, gin.H{"error": "storage locked"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, userToResponse(user))
}

// UpdateUserRequest defines the request body for updating a user.
type UpdateUserRequest struct {
	Username    *string   `json:"username,omitempty"`
	Email       *string   `json:"email,omitempty"`
	Role        *string   `json:"role,omitempty"`
	AllowedApps *[]string `json:"allowed_apps,omitempty"`
}

// handleUpdateUser: PUT /api/v1/users/:id
func (s *GinServer) handleUpdateUser(c *gin.Context) {
	if s.userManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user management not available"})
		return
	}

	id := c.Param("id")

	var req UpdateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	input := auth.UpdateUserInput{ID: id}
	if req.Username != nil {
		input.Username = req.Username
	}
	if req.Email != nil {
		input.Email = req.Email
	}
	if req.Role != nil {
		role := persistence.UserRole(*req.Role)
		input.Role = &role
	}
	if req.AllowedApps != nil {
		input.AllowedApps = req.AllowedApps
	}

	ctx, cancel := s.opContext(c, 30*time.Second)
	defer cancel()
	user, err := s.userManager.Update(ctx, input)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		if errors.Is(err, auth.ErrUsernameExists) {
			c.JSON(http.StatusConflict, gin.H{"error": "username already exists"})
			return
		}
		if errors.Is(err, auth.ErrEmailExists) {
			c.JSON(http.StatusConflict, gin.H{"error": "email already exists"})
			return
		}
		if errors.Is(err, persistence.ErrLocked) {
			c.JSON(http.StatusLocked, gin.H{"error": "storage locked"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, userToResponse(user))
}

// handleDeleteUser: DELETE /api/v1/users/:id
func (s *GinServer) handleDeleteUser(c *gin.Context) {
	if s.userManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user management not available"})
		return
	}

	id := c.Param("id")

	// Prevent self-deletion
	if sess := s.getSessionFromContext(c); sess != nil && sess.UserID == id {
		c.JSON(http.StatusForbidden, gin.H{"error": "cannot delete own account"})
		return
	}

	ctx, cancel := s.opContext(c, 30*time.Second)
	defer cancel()
	if err := s.userManager.Delete(ctx, id); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		if errors.Is(err, persistence.ErrLocked) {
			c.JSON(http.StatusLocked, gin.H{"error": "storage locked"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete user"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "user deleted"})
}

// SetPasswordRequest defines the request body for admin password reset.
type SetPasswordRequest struct {
	Password string `json:"password" binding:"required"`
}

// handleSetUserPassword: POST /api/v1/users/:id/password
func (s *GinServer) handleSetUserPassword(c *gin.Context) {
	if s.userManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user management not available"})
		return
	}

	id := c.Param("id")
	ctx, cancel := s.opContext(c, 30*time.Second)
	defer cancel()

	// First, get the user to check if it's the admin
	user, err := s.userManager.Get(ctx, id)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		if errors.Is(err, persistence.ErrLocked) {
			c.JSON(http.StatusLocked, gin.H{"error": "storage locked"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get user"})
		return
	}

	// Block admin password changes via this endpoint.
	// Admin password is tied to disk encryption (crypto SDEK) and requires the old
	// password for rewrap. Use POST /api/v1/auth/password instead.
	if user.Username == "admin" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "admin password must be changed via profile settings (requires current password for disk encryption sync)",
		})
		return
	}

	var req SetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if err := s.userManager.SetPassword(ctx, id, req.Password); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		if errors.Is(err, persistence.ErrLocked) {
			c.JSON(http.StatusLocked, gin.H{"error": "storage locked"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "password updated"})
}

// getSessionFromContext retrieves the validated portal session from the gin context.
// RFC 20260122 §6.2: Validates audience="portal" and origin binding.
//
// **MustRegisterPasskey gating is NOT performed here.** This helper is safe
// to use ONLY in handlers under the requireSession middleware (gin_server.go
// `authed` group), which centrally rejects gated sessions with 403 before
// they reach the handler. Most callsites in this package fall in that
// category — see gin_middleware.go:207 for the central enforcement.
//
// Most callsites are in handlers under requireSession; the middleware-level
// gate covers them. Two intentional exceptions exist on root-router routes
// that must work pre-login:
//   - handleOIDCAuthorize (gin_oidc_handlers.go:394): sessionGates at the
//     top of the function, then fast-paths via this helper post-gate.
//   - handleBoot (gin_boot_handler.go:145): performs its own
//     MustRegisterPasskey check at the next branch (line ~159).
//
// New handlers OUTSIDE requireSession that read a session must use
// sessionGate, not this helper — the type-level two-value return makes the
// gate impossible to drop accidentally. Same rule for recovery-key
// state-mutating handlers (admitted by the middleware's prefix allowlist
// for read paths).
func (s *GinServer) getSessionFromContext(c *gin.Context) *auth.Session {
	id, ok := s.getSession(c)
	if !ok {
		return nil
	}
	origin := s.computeCanonicalOrigin(c)
	sess, ok := s.sessions.ValidatePortalSession(id, origin)
	if !ok {
		return nil
	}
	return sess
}

// sessionGate returns the validated portal session and whether it is in the
// MustRegisterPasskey bootstrap window (D-10 forcing-gate). The two-value
// signature exists to make the gate state a type-level return: a caller that
// reads a session at all must also acknowledge the gate (even if to ignore
// it via `_`), which closes the bug class where a new handler reads the
// session and forgets the gate (closure for project_deferred_findings_passkey_cohesive_fix.md
// deferred 2 / "Cluster A root cause").
//
// **Use this helper only in routes that bypass the requireSession middleware
// umbrella** — the middleware (gin_middleware.go:207) already returns 403 for
// gated sessions on routes under the `authed` group. The two known bypass
// classes today:
//
//  1. Routes registered at the root router outside `authed` (e.g., handleOIDCAuthorize
//     reachable via oidc_passthrough — see gin_oidc_handlers.go:350 for the
//     attack surface this gate closes).
//  2. State-mutating handlers under the `/api/v1/crypto/recovery-key/` prefix,
//     which the middleware admits via its prefix allowlist so the read-only
//     bootstrap-screen view works (gin_middleware.go:222); the mutators must
//     self-deny here.
//
// Returns:
//   - (nil, false): no validated session — caller decides whether that's OK
//     (login flows, public endpoints).
//   - (sess, true): session valid and gated — caller MUST refuse with the
//     response shape appropriate to the route (403 passkey_registration_required
//     for API, 302 to portal root for OIDC authorize, etc.).
//   - (sess, false): session valid and not gated — proceed.
func (s *GinServer) sessionGate(c *gin.Context) (*auth.Session, bool) {
	sess := s.getSessionFromContext(c)
	if sess == nil {
		return nil, false
	}
	return sess, sess.MustRegisterPasskey.Load()
}
