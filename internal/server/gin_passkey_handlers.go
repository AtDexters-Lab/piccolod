package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"

	authpkg "piccolod/internal/auth"
	"piccolod/internal/events"
	"piccolod/internal/persistence"
)

const rpDisplayName = "Piccolo OS"

// respondInviteError maps invite validation errors to HTTP responses.
// Returns true if an error was handled, false if the caller should handle it.
func respondInviteError(c *gin.Context, err error) bool {
	if errors.Is(err, authpkg.ErrInviteNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "invite not found"})
		return true
	}
	if errors.Is(err, authpkg.ErrInviteExpired) {
		c.JSON(http.StatusGone, gin.H{"error": "invite expired"})
		return true
	}
	if errors.Is(err, authpkg.ErrInviteConsumed) {
		c.JSON(http.StatusGone, gin.H{"error": "invite already used"})
		return true
	}
	return false
}

// getAuthorizedCredential fetches a credential by path param and checks ownership or admin access.
// Returns the credential and true on success, or responds with an error and returns false.
func (s *GinServer) getAuthorizedCredential(c *gin.Context, sess *authpkg.Session) (*persistence.WebAuthnCredential, bool) {
	credID := c.Param("id")
	if credID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "credential id required"})
		return nil, false
	}
	cred, err := s.webauthnMgr.GetCredential(c.Request.Context(), credID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "passkey not found"})
			return nil, false
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to look up passkey"})
		return nil, false
	}
	if cred.UserID != sess.UserID && sess.Role != string(persistence.UserRoleAdmin) {
		c.JSON(http.StatusForbidden, gin.H{"error": "not authorized"})
		return nil, false
	}
	return &cred, true
}

// --- Audit ---

// publishPasskeyAudit publishes an audit event for passkey/invite operations.
func (s *GinServer) publishPasskeyAudit(c *gin.Context, kind string, metadata map[string]any) {
	if s.events == nil {
		return
	}
	s.events.Publish(events.Event{
		Topic: events.TopicAudit,
		Payload: events.AuditEvent{
			Kind:     kind,
			Time:     time.Now(),
			Source:   c.ClientIP(),
			Metadata: metadata,
		},
	})
}

// --- Helpers ---

// getBaseDomain returns the identity service's base domain, or empty if unavailable.
func (s *GinServer) getBaseDomain() string {
	if s.identityService == nil {
		return ""
	}
	return s.identityService.DeviceConfig().BaseDomain
}

// getAccessContext returns the access context for the request.
func (s *GinServer) getAccessContext(c *gin.Context) authpkg.AccessContext {
	if s.isRemoteSecureRequest(c.Request) {
		return authpkg.AccessContextRemote
	}
	return authpkg.AccessContextLAN
}

// getRPID derives the WebAuthn RP ID for the request.
func (s *GinServer) getRPID(c *gin.Context) string {
	return authpkg.DetermineRPID(c.Request.Host, s.getBaseDomain())
}

// getUserVerification returns the appropriate user verification level.
func (s *GinServer) getUserVerification(c *gin.Context) protocol.UserVerificationRequirement {
	if s.isRemoteSecureRequest(c.Request) {
		return protocol.VerificationRequired
	}
	return protocol.VerificationPreferred
}

// getValidatedSession returns the session for the request, or aborts with 401.
func (s *GinServer) getValidatedSession(c *gin.Context) (*authpkg.Session, bool) {
	id, ok := s.getSession(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return nil, false
	}
	origin := s.computeCanonicalOrigin(c)
	sess, ok := s.sessions.ValidatePortalSession(id, origin)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return nil, false
	}
	return sess, true
}

// --- Login Options ---

// handleLoginOptions: GET /api/v1/auth/login-options
func (s *GinServer) handleLoginOptions(c *gin.Context) {
	accessCtx := s.getAccessContext(c)
	secure := s.isSecureRequest(c.Request)
	rpID := s.getRPID(c)

	// Compute the union of allowed methods across all users who could
	// plausibly use each method. We skip passwordless users because they
	// can never authenticate with "password" — including them would keep
	// the password form visible indefinitely.
	methods := s.computeLoginOptionsUnion(c.Request.Context(), accessCtx, secure, rpID)

	// When passkeys are disabled, strip "passkey" from methods
	if s.webauthnMgr == nil {
		filtered := methods[:0]
		for _, m := range methods {
			if m != "passkey" {
				filtered = append(filtered, m)
			}
		}
		methods = filtered
	}

	c.JSON(http.StatusOK, gin.H{
		"methods": methods,
		"context": accessCtx.String(),
		"rp_id":   rpID,
	})
}

// computeLoginOptionsUnion evaluates AllowedMethods for each user who has a
// password and returns the union of results. Users without passwords are
// excluded because they can never use the "password" method — including them
// would keep the password form visible even after all password-capable users
// have registered passkeys.
func (s *GinServer) computeLoginOptionsUnion(ctx context.Context, accessCtx authpkg.AccessContext, secure bool, rpID string) []string {
	// If we can't query users, fall back to the most permissive set.
	if s.userManager == nil {
		return authpkg.AllowedMethods(accessCtx, secure, "", false)
	}

	users, err := s.userManager.List(ctx)
	if err != nil {
		return authpkg.AllowedMethods(accessCtx, secure, "", false)
	}

	// Bulk-fetch which users have passkeys for this RP (single query).
	passkeyUsers := map[string]struct{}{}
	if s.webauthnMgr != nil {
		if set, err := s.webauthnMgr.UserIDsWithPasskeyForRP(ctx, rpID); err == nil {
			passkeyUsers = set
		}
	}

	methodSet := make(map[string]struct{})
	evaluated := false
	for _, u := range users {
		if !u.HasPassword {
			continue
		}
		evaluated = true
		_, hasPasskey := passkeyUsers[u.ID]
		for _, m := range authpkg.AllowedMethods(accessCtx, secure, string(u.Role), hasPasskey) {
			methodSet[m] = struct{}{}
		}
		// Max possible set is {"passkey","password"} — no point continuing.
		if len(methodSet) >= 2 {
			break
		}
	}

	if !evaluated {
		// No password-capable users found (e.g., only invite-based users).
		// Return passkey-only since password is unusable by anyone.
		return []string{"passkey"}
	}

	methods := make([]string, 0, len(methodSet))
	// Stable order: known methods first, then any future additions.
	for _, m := range []string{"passkey", "password"} {
		if _, ok := methodSet[m]; ok {
			methods = append(methods, m)
			delete(methodSet, m)
		}
	}
	for m := range methodSet {
		methods = append(methods, m)
	}
	return methods
}

// --- Passkey Registration (authenticated) ---

// handlePasskeyRegisterBegin: POST /api/v1/auth/passkey/register/begin
func (s *GinServer) handlePasskeyRegisterBegin(c *gin.Context) {
	if s.webauthnMgr == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "passkey support not available"})
		return
	}

	sess, ok := s.getValidatedSession(c)
	if !ok {
		return
	}

	rpID := s.getRPID(c)
	rpOrigin := s.computeCanonicalOrigin(c)
	uv := s.getUserVerification(c)

	options, sessionID, err := s.webauthnMgr.BeginRegistration(
		c.Request.Context(), sess.UserID, sess.User,
		rpID, rpDisplayName, rpOrigin, uv,
	)
	if err != nil {
		log.Printf("WARN: passkey register begin: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to begin registration"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"publicKey":  options.Response,
		"session_id": sessionID,
	})
}

// handlePasskeyRegisterFinish: POST /api/v1/auth/passkey/register/finish
func (s *GinServer) handlePasskeyRegisterFinish(c *gin.Context) {
	if s.webauthnMgr == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "passkey support not available"})
		return
	}

	sess, ok := s.getValidatedSession(c)
	if !ok {
		return
	}

	// session_id passed as query param; body is the raw WebAuthn attestation response.
	sessionID := c.Query("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id required"})
		return
	}

	parsedResponse, err := protocol.ParseCredentialCreationResponseBody(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid attestation response"})
		return
	}

	cred, err := s.webauthnMgr.FinishRegistration(
		c.Request.Context(), sessionID, rpDisplayName, parsedResponse,
	)
	if err != nil {
		if errors.Is(err, authpkg.ErrCeremonyNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ceremony expired or not found"})
			return
		}
		log.Printf("WARN: passkey register finish: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "registration failed"})
		return
	}

	sess.MustRegisterPasskey = false

	s.publishPasskeyAudit(c, "passkey.registered", map[string]any{
		"user_id":       sess.UserID,
		"credential_id": cred.ID,
		"rp_id":         cred.RPID,
	})

	c.JSON(http.StatusOK, gin.H{
		"credential_id": cred.ID,
		"created_at":    cred.CreatedAt.Format(time.RFC3339),
	})
}

// --- Passkey Login (public) ---

// handlePasskeyLoginBegin: POST /api/v1/auth/passkey/login/begin
func (s *GinServer) handlePasskeyLoginBegin(c *gin.Context) {
	if s.webauthnMgr == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "passkey support not available"})
		return
	}

	if !s.passkeyRateLimiter.Allow(c.ClientIP()) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
		return
	}

	rpID := s.getRPID(c)
	rpOrigin := s.computeCanonicalOrigin(c)
	uv := s.getUserVerification(c)

	options, sessionID, err := s.webauthnMgr.BeginAuthentication(
		c.Request.Context(), rpID, rpDisplayName, rpOrigin, uv,
	)
	if err != nil {
		log.Printf("WARN: passkey login begin: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to begin authentication"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"publicKey":  options.Response,
		"session_id": sessionID,
	})
}

// handlePasskeyLoginFinish: POST /api/v1/auth/passkey/login/finish
func (s *GinServer) handlePasskeyLoginFinish(c *gin.Context) {
	if s.webauthnMgr == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "passkey support not available"})
		return
	}

	if !s.passkeyRateLimiter.Allow(c.ClientIP()) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
		return
	}

	sessionID := c.Query("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id required"})
		return
	}

	parsedResponse, err := protocol.ParseCredentialRequestResponseBody(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid assertion response"})
		return
	}

	rpID := s.getRPID(c)

	userID, err := s.webauthnMgr.FinishAuthentication(
		c.Request.Context(), sessionID, rpDisplayName, parsedResponse,
	)
	if err != nil {
		if errors.Is(err, authpkg.ErrCeremonyNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ceremony expired or not found"})
			return
		}
		s.passkeyRateLimiter.RecordFailure(c.ClientIP())
		s.publishPasskeyAudit(c, "passkey.auth.failure", map[string]any{
			"rp_id":  rpID,
			"reason": err.Error(),
		})
		log.Printf("WARN: passkey login finish: %v", err)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication failed"})
		return
	}

	// Look up user info
	if s.userManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "user management unavailable"})
		return
	}
	userInfo, err := s.userManager.Get(c.Request.Context(), userID)
	if err != nil {
		log.Printf("WARN: passkey login user lookup: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "user lookup failed"})
		return
	}

	// Create session (same flow as handleAuthLogin)
	boundOrigin := s.computeCanonicalOrigin(c)
	sess := s.sessions.CreatePortalSession(
		userInfo.ID, userInfo.Username, string(userInfo.Role),
		boundOrigin, portalSessionTTL,
	)
	s.setSessionCookie(c, sess.ID, portalSessionCookieTTL)
	s.resetLoginFailures()

	s.publishPasskeyAudit(c, "passkey.auth.success", map[string]any{
		"user_id": userInfo.ID,
		"rp_id":   rpID,
	})

	c.JSON(http.StatusOK, gin.H{"message": "ok"})
}

// --- Passkey Management (authenticated) ---

// handleListPasskeys: GET /api/v1/auth/passkeys
func (s *GinServer) handleListPasskeys(c *gin.Context) {
	if s.webauthnMgr == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "passkey support not available"})
		return
	}

	sess, ok := s.getValidatedSession(c)
	if !ok {
		return
	}

	// Admin can list passkeys for another user via ?user_id=
	targetUserID := sess.UserID
	if qUserID := c.Query("user_id"); qUserID != "" && sess.Role == string(persistence.UserRoleAdmin) {
		targetUserID = qUserID
	}

	creds, err := s.webauthnMgr.ListCredentials(c.Request.Context(), targetUserID)
	if err != nil {
		log.Printf("WARN: list passkeys: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list passkeys"})
		return
	}

	result := make([]gin.H, 0, len(creds))
	for _, cred := range creds {
		result = append(result, gin.H{
			"id":            cred.ID,
			"friendly_name": cred.FriendlyName,
			"rp_id":         cred.RPID,
			"created_at":    cred.CreatedAt.Format(time.RFC3339),
			"last_used_at":  cred.LastUsedAt.Format(time.RFC3339),
			"transports":    cred.Transports,
		})
	}

	c.JSON(http.StatusOK, gin.H{"passkeys": result})
}

// handleDeletePasskey: DELETE /api/v1/auth/passkeys/:id
func (s *GinServer) handleDeletePasskey(c *gin.Context) {
	if s.webauthnMgr == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "passkey support not available"})
		return
	}

	sess, ok := s.getValidatedSession(c)
	if !ok {
		return
	}

	cred, ok := s.getAuthorizedCredential(c, sess)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	credID := cred.ID

	// Safety check: don't delete last credential for a passwordless user
	count, err := s.webauthnMgr.CountCredentials(ctx, cred.UserID)
	if err == nil && count <= 1 {
		if s.userManager != nil {
			user, userErr := s.userManager.Get(ctx, cred.UserID)
			if userErr == nil && !user.HasPassword {
				c.JSON(http.StatusConflict, gin.H{"error": "cannot delete last passkey for passwordless user"})
				return
			}
		}
	}

	if err := s.webauthnMgr.DeleteCredential(ctx, credID); err != nil {
		log.Printf("WARN: delete passkey: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete passkey"})
		return
	}

	s.publishPasskeyAudit(c, "passkey.deleted", map[string]any{
		"user_id":       cred.UserID,
		"credential_id": credID,
		"deleted_by":    sess.UserID,
	})

	c.JSON(http.StatusOK, gin.H{"message": "ok"})
}

// handleRenamePasskey: PATCH /api/v1/auth/passkeys/:id
func (s *GinServer) handleRenamePasskey(c *gin.Context) {
	if s.webauthnMgr == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "passkey support not available"})
		return
	}

	sess, ok := s.getValidatedSession(c)
	if !ok {
		return
	}

	var body struct {
		FriendlyName string `json:"friendly_name"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	name := strings.TrimSpace(body.FriendlyName)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "friendly_name required"})
		return
	}
	if len(name) > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "friendly_name too long (max 100 chars)"})
		return
	}

	cred, ok := s.getAuthorizedCredential(c, sess)
	if !ok {
		return
	}

	if err := s.webauthnMgr.RenameCredential(c.Request.Context(), cred.ID, name); err != nil {
		log.Printf("WARN: rename passkey: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rename passkey"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "ok"})
}

// --- Invite Management ---

// handleCreateInvite: POST /api/v1/users/invite
func (s *GinServer) handleCreateInvite(c *gin.Context) {
	if s.inviteMgr == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "invite support not available"})
		return
	}

	sess, ok := s.getValidatedSession(c)
	if !ok {
		return
	}

	var body struct {
		Username    string   `json:"username"`
		Email       string   `json:"email"`
		AllowedApps []string `json:"allowed_apps"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if strings.TrimSpace(body.Username) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username required"})
		return
	}

	token, userInfo, err := s.inviteMgr.CreateInvite(c.Request.Context(), authpkg.CreateInviteInput{
		Username:    body.Username,
		Email:       body.Email,
		AllowedApps: body.AllowedApps,
	}, sess.UserID)
	if err != nil {
		if errors.Is(err, authpkg.ErrUsernameExists) || errors.Is(err, authpkg.ErrEmailExists) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		// Validation errors from UserManager (e.g., "email required", "invalid role")
		if !errors.Is(err, persistence.ErrLocked) && !errors.Is(err, persistence.ErrNotLeader) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		log.Printf("WARN: create invite: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create invite"})
		return
	}

	s.publishPasskeyAudit(c, "invite.created", map[string]any{
		"user_id":    userInfo.ID,
		"created_by": sess.UserID,
	})

	c.JSON(http.StatusOK, gin.H{
		"token":    token,
		"user_id":  userInfo.ID,
		"username": userInfo.Username,
	})
}

// handleValidateInvite: GET /api/v1/auth/invite/:token
func (s *GinServer) handleValidateInvite(c *gin.Context) {
	if s.inviteMgr == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "invite support not available"})
		return
	}

	if !s.passkeyRateLimiter.Allow(c.ClientIP()) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
		return
	}

	token := c.Param("token")
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token required"})
		return
	}

	username, _, err := s.inviteMgr.ValidateInvite(c.Request.Context(), token)
	if err != nil {
		s.passkeyRateLimiter.RecordFailure(c.ClientIP())
		if respondInviteError(c, err) {
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate invite"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"valid":    true,
		"username": username,
	})
}

// handleInvitePasskeyBegin: POST /api/v1/auth/invite/:token/passkey/begin
func (s *GinServer) handleInvitePasskeyBegin(c *gin.Context) {
	if s.webauthnMgr == nil || s.inviteMgr == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "passkey support not available"})
		return
	}

	if !s.passkeyRateLimiter.Allow(c.ClientIP()) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
		return
	}

	if !s.isSecureRequest(c.Request) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "passkey registration requires HTTPS"})
		return
	}

	token := c.Param("token")
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token required"})
		return
	}

	ctx := c.Request.Context()

	// Validate the invite first
	username, userID, err := s.inviteMgr.ValidateInvite(ctx, token)
	if err != nil {
		s.passkeyRateLimiter.RecordFailure(c.ClientIP())
		if respondInviteError(c, err) {
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invite validation failed"})
		return
	}

	rpID := s.getRPID(c)
	rpOrigin := s.computeCanonicalOrigin(c)
	uv := s.getUserVerification(c)

	options, sessionID, err := s.webauthnMgr.BeginRegistration(
		ctx, userID, username,
		rpID, rpDisplayName, rpOrigin, uv,
	)
	if err != nil {
		log.Printf("WARN: invite passkey begin: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to begin registration"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"publicKey":  options.Response,
		"session_id": sessionID,
	})
}

// handleInvitePasskeyFinish: POST /api/v1/auth/invite/:token/passkey/finish
func (s *GinServer) handleInvitePasskeyFinish(c *gin.Context) {
	if s.webauthnMgr == nil || s.inviteMgr == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "passkey support not available"})
		return
	}

	if !s.passkeyRateLimiter.Allow(c.ClientIP()) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
		return
	}

	token := c.Param("token")
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token required"})
		return
	}

	sessionID := c.Query("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id required"})
		return
	}

	parsedResponse, err := protocol.ParseCredentialCreationResponseBody(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid attestation response"})
		return
	}

	ctx := c.Request.Context()

	// Validate the invite and check user match BEFORE any state mutations.
	_, inviteUserID, err := s.inviteMgr.ValidateInvite(ctx, token)
	if err != nil {
		if respondInviteError(c, err) {
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invite validation failed"})
		return
	}

	// Register the credential. If this fails (ceremony expired, user
	// cancelled, etc.), the invite remains valid and can be retried.
	cred, err := s.webauthnMgr.FinishRegistration(ctx, sessionID, rpDisplayName, parsedResponse)
	if err != nil {
		if errors.Is(err, authpkg.ErrCeremonyNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ceremony expired or not found"})
			return
		}
		log.Printf("WARN: invite passkey finish registration: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "registration failed"})
		return
	}

	// Verify the credential was registered for the same user as the invite.
	if cred.UserID != inviteUserID {
		_ = s.webauthnMgr.DeleteCredential(ctx, cred.ID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invite token does not match ceremony"})
		return
	}

	// Consume the invite after successful registration + validation.
	userID, err := s.inviteMgr.ConsumeInvite(ctx, token)
	if err != nil {
		_ = s.webauthnMgr.DeleteCredential(ctx, cred.ID)
		if respondInviteError(c, err) {
			return
		}
		log.Printf("WARN: invite consume: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to consume invite"})
		return
	}

	userInfo, err := s.userManager.Get(ctx, userID)
	if err != nil {
		log.Printf("WARN: invite passkey finish user lookup: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "user lookup failed"})
		return
	}

	s.publishPasskeyAudit(c, "passkey.registered", map[string]any{
		"user_id":       cred.UserID,
		"credential_id": cred.ID,
		"rp_id":         cred.RPID,
	})
	s.publishPasskeyAudit(c, "invite.consumed", map[string]any{
		"user_id": userID,
	})

	// Create session
	boundOrigin := s.computeCanonicalOrigin(c)
	sess := s.sessions.CreatePortalSession(
		userInfo.ID, userInfo.Username, string(userInfo.Role),
		boundOrigin, portalSessionTTL,
	)
	s.setSessionCookie(c, sess.ID, portalSessionCookieTTL)

	c.JSON(http.StatusOK, gin.H{"message": "ok"})
}

// handleReinviteUser: POST /api/v1/users/:id/reinvite
func (s *GinServer) handleReinviteUser(c *gin.Context) {
	if s.inviteMgr == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "invite support not available"})
		return
	}

	sess, ok := s.getValidatedSession(c)
	if !ok {
		return
	}

	userID := c.Param("id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user ID required"})
		return
	}

	token, err := s.inviteMgr.ReinviteUser(c.Request.Context(), userID, sess.UserID)
	if err != nil {
		log.Printf("WARN: reinvite user: %v", err)
		if errors.Is(err, authpkg.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create invite"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"token": token})
}
