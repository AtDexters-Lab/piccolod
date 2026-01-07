package server

import (
	"errors"
	"net/http"

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

	user, err := s.userManager.Create(c.Request.Context(), input)
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

	user, err := s.userManager.Update(c.Request.Context(), input)
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

	if err := s.userManager.Delete(c.Request.Context(), id); err != nil {
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

	var req SetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if err := s.userManager.SetPassword(c.Request.Context(), id, req.Password); err != nil {
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

// getSessionFromContext retrieves the session from the gin context.
func (s *GinServer) getSessionFromContext(c *gin.Context) *auth.Session {
	id, ok := s.getSession(c)
	if !ok {
		return nil
	}
	sess, ok := s.sessions.Get(id)
	if !ok {
		return nil
	}
	return sess
}
