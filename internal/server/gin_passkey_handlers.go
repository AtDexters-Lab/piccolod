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

// mapRegistrationError translates the typed errors returned by
// WebAuthnManager.FinishRegistration into HTTP responses. Returns true if a
// response was written (caller must return without further work).
func (s *GinServer) mapRegistrationError(c *gin.Context, err error) bool {
	if errors.Is(err, authpkg.ErrCeremonyNotFound) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ceremony expired or not found"})
		return true
	}
	var dup *authpkg.AlreadyRegisteredError
	if errors.As(err, &dup) {
		c.JSON(http.StatusConflict, gin.H{
			"error":         "passkey_already_registered",
			"credential_id": dup.CredentialID,
			"rp_id":         dup.RPID,
		})
		return true
	}
	return false
}

// passkeyPresence captures the per-RP credential summary used by both the
// session and boot handlers.
type passkeyPresence struct {
	HasPasskey bool
	TotalCount int
}

// computePasskeyPresence derives the passkey summary for the given user/RP.
// Falls back to a zero value when webauthnMgr is nil or the query fails.
func (s *GinServer) computePasskeyPresence(c *gin.Context, userID, rpID string) passkeyPresence {
	if s.webauthnMgr == nil || userID == "" {
		return passkeyPresence{}
	}
	total, err := s.webauthnMgr.CountCredentials(c.Request.Context(), userID)
	if err != nil {
		return passkeyPresence{}
	}
	rpCount, err := s.webauthnMgr.CountByUserAndRP(c.Request.Context(), userID, rpID)
	if err != nil {
		return passkeyPresence{}
	}
	return passkeyPresence{
		HasPasskey: rpCount > 0,
		TotalCount: total,
	}
}

// getAuthorizedCredential fetches a credential by path param and checks ownership or admin access.
// Returns the credential and true on success, or responds with an error and returns false.
//
// Under MustRegisterPasskey=true (D-10 forcing-gate), admin-cross-user access
// is suppressed: an attacker who reaches the bootscreen with a stolen admin
// password must not be able to enumerate/destroy other users' credentials
// before completing a passkey ceremony bound to an authenticator they control.
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
	adminCrossUser := sess.Role == string(persistence.UserRoleAdmin) && !sess.MustRegisterPasskey.Load()
	if cred.UserID != sess.UserID && !adminCrossUser {
		c.JSON(http.StatusForbidden, gin.H{"error": "not authorized"})
		return nil, false
	}
	return &cred, true
}

// --- Audit ---

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
	methods, derived := s.computeLoginOptionsUnion(c.Request.Context(), accessCtx, secure, rpID)

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

	// Suppress the passkey button when no user has any credential for this RP.
	if derived.NoPasskeyAnywhere {
		filtered := methods[:0]
		for _, m := range methods {
			if m != "passkey" {
				filtered = append(filtered, m)
			}
		}
		methods = filtered
	}

	c.JSON(http.StatusOK, gin.H{
		"methods":             methods,
		"context":             accessCtx.String(),
		"rp_id":               rpID,
		"no_passkey_anywhere": derived.NoPasskeyAnywhere,
	})
}

// loginOptionsDerived carries flags used to shape the login UI.
type loginOptionsDerived struct {
	// NoPasskeyAnywhere is true when NO user has any credential for this RP.
	// Drives the "suppress passkey button" rule at the login screen.
	NoPasskeyAnywhere bool
}

// computeLoginOptionsUnion evaluates AllowedMethods for each password-capable
// user and returns the union of results. Users without passwords are excluded
// from method evaluation because they can never use "password" — including
// them would keep the password form visible indefinitely.
func (s *GinServer) computeLoginOptionsUnion(ctx context.Context, accessCtx authpkg.AccessContext, secure bool, rpID string) ([]string, loginOptionsDerived) {
	derived := loginOptionsDerived{NoPasskeyAnywhere: true}
	if s.userManager == nil {
		return authpkg.AllowedMethods(accessCtx, secure, "", false), derived
	}

	users, err := s.userManager.List(ctx)
	if err != nil {
		return authpkg.AllowedMethods(accessCtx, secure, "", false), derived
	}

	passkeyUsers := map[string]struct{}{}
	if s.webauthnMgr != nil {
		if set, err := s.webauthnMgr.UserIDsWithPasskeyForRP(ctx, rpID); err == nil {
			passkeyUsers = set
		}
	}
	if len(passkeyUsers) > 0 {
		derived.NoPasskeyAnywhere = false
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
	}

	if !evaluated {
		return []string{"passkey"}, derived
	}

	methods := make([]string, 0, len(methodSet))
	for _, m := range []string{"passkey", "password"} {
		if _, ok := methodSet[m]; ok {
			methods = append(methods, m)
			delete(methodSet, m)
		}
	}
	for m := range methodSet {
		methods = append(methods, m)
	}
	return methods, derived
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
	regHost := authpkg.NormalizeHost(c.Request.Host)

	options, sessionID, err := s.webauthnMgr.BeginRegistration(
		c.Request.Context(), sess.UserID, sess.User,
		rpID, rpDisplayName, rpOrigin, regHost, uv,
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
		// Benign-duplicate (same-user, same-credential): the authenticator
		// ignored excludeCredentials and returned a credential we already
		// have. The user demonstrably owns a passkey for this RP, so clear
		// the forcing-gate before returning — otherwise they'd be stuck in
		// bootscreen with a valid passkey they can't register again. Scope
		// the clear to the credential's RP so cross-RP sessions (e.g. a
		// remote-domain bootstrap) aren't silently unlocked by a LAN-only
		// registration.
		var dup *authpkg.AlreadyRegisteredError
		if errors.As(err, &dup) {
			s.sessions.ClearMustRegisterPasskey(sess.UserID, dup.RPID, s.getBaseDomain())
		}
		if s.mapRegistrationError(c, err) {
			return
		}
		log.Printf("WARN: passkey register finish: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "registration failed"})
		return
	}

	// Clear the forcing gate across all of the user's sessions bound to the
	// registered credential's RP (includes the caller's). A passkey
	// registered on one device releases the gate on the user's other open
	// sessions for the same RP — but NOT on sessions for a different RP (a
	// LAN passkey must not unlock a remote-domain bootstrap). Residual race
	// (self-healing): a login racing with this finish can re-set the flag
	// on the new session because the login's hasPasskeyForRP check is
	// TOCTOU against credential persistence; the user hits the gate once
	// more and the next register-finish clears it idempotently.
	if n := s.sessions.ClearMustRegisterPasskey(sess.UserID, cred.RPID, s.getBaseDomain()); n > 1 {
		log.Printf("INFO: passkey register cleared forcing flag on %d sessions for user=%s (id=%s) rp=%s",
			n, sess.User, sess.UserID, cred.RPID)
	}

	s.publishAuditEvent(c, "passkey.registered", map[string]any{
		"user_id":       sess.UserID,
		"credential_id": cred.ID,
		"rp_id":         cred.RPID,
	})

	// Surface the recovery-key gate so the post-registration flow (forced or
	// voluntary) can route to the recovery screen on unack'd keys. The
	// MustRegisterPasskey forcing path lands here and historically went
	// straight to the dashboard without consulting the gate.
	c.JSON(http.StatusOK, gin.H{
		"credential_id":        cred.ID,
		"created_at":           cred.CreatedAt.Format(time.RFC3339),
		"recovery_key_pending": s.computeRecoveryKeyPending(c.Request.Context()),
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

	authResult, err := s.webauthnMgr.FinishAuthentication(
		c.Request.Context(), sessionID, rpDisplayName, parsedResponse,
	)
	if err != nil {
		if errors.Is(err, authpkg.ErrCeremonyNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ceremony expired or not found"})
			return
		}
		s.passkeyRateLimiter.RecordFailure(c.ClientIP())
		s.publishAuditEvent(c, "passkey.auth.failure", map[string]any{
			"rp_id":  rpID,
			"reason": err.Error(),
		})
		// Genuine DB miss: include a Signal API hint so the frontend can prune
		// the stale picker entry. Other failures (signature/origin/etc.) get
		// the generic 401 with no leak.
		var notFound *authpkg.CredentialNotFoundError
		if errors.As(err, &notFound) {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "authentication failed",
				"signal_unknown_credential": gin.H{
					"rp_id":         notFound.RPID,
					"credential_id": notFound.CredentialID,
				},
			})
			return
		}
		log.Printf("WARN: passkey login finish: %v", err)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication failed"})
		return
	}

	// Look up user info
	if s.userManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "user management unavailable"})
		return
	}
	userInfo, err := s.userManager.Get(c.Request.Context(), authResult.UserID)
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

	s.publishAuditEvent(c, "passkey.auth.success", map[string]any{
		"user_id": userInfo.ID,
		"rp_id":   rpID,
	})

	// Surface the recovery-key gate so the passkey-login flow can route to
	// the recovery screen on unack'd keys, matching /auth/login's behavior.
	c.JSON(http.StatusOK, gin.H{
		"message":              "ok",
		"recovery_key_pending": s.computeRecoveryKeyPending(c.Request.Context()),
	})
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

	// Admin can list passkeys for another user via ?user_id=, but not while
	// MustRegisterPasskey is set — that gate exists to contain a stolen-admin-
	// password attacker; cross-user enumeration would expand their reach.
	targetUserID := sess.UserID
	if qUserID := c.Query("user_id"); qUserID != "" &&
		sess.Role == string(persistence.UserRoleAdmin) && !sess.MustRegisterPasskey.Load() {
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
		entry := gin.H{
			"id":            cred.ID,
			"friendly_name": cred.FriendlyName,
			"rp_id":         cred.RPID,
			"created_at":    cred.CreatedAt.Format(time.RFC3339),
			"transports":    cred.Transports,
		}
		if !cred.LastUsedAt.IsZero() {
			entry["last_used_at"] = cred.LastUsedAt.Format(time.RFC3339)
		} else {
			entry["last_used_at"] = nil
		}
		result = append(result, entry)
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

	// Under the MustRegisterPasskey forcing-gate, DELETE is admitted for the
	// "inline legacy removal" bootstrap flow (delete stale cred then register
	// a new one). Restrict that to the session's current RP — otherwise a
	// remote-domain password-bootstrap session could delete the user's LAN
	// passkeys before registering the required new one, destroying recovery
	// paths the user had on a different network.
	if sess.MustRegisterPasskey.Load() {
		sessRPID := s.getRPID(c)
		if sessRPID != "" && cred.RPID != sessRPID {
			c.JSON(http.StatusForbidden, gin.H{
				"error":   "passkey_registration_required",
				"message": "Passkeys for other networks cannot be deleted before completing passkey registration here.",
			})
			return
		}
	}

	ctx, cancel := s.opContext(c, 30*time.Second)
	defer cancel()

	// Safety check: a passwordless user must retain at least one passkey,
	// otherwise they'd have no way back in.
	if s.userManager != nil {
		user, userErr := s.userManager.Get(ctx, cred.UserID)
		if userErr == nil && !user.HasPassword {
			count, cErr := s.webauthnMgr.CountCredentials(ctx, cred.UserID)
			if cErr == nil && count <= 1 {
				c.JSON(http.StatusConflict, gin.H{"error": "cannot delete last passkey for passwordless user"})
				return
			}
		}
	}

	if err := s.webauthnMgr.DeleteCredential(ctx, cred.ID); err != nil {
		log.Printf("WARN: delete passkey: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete passkey"})
		return
	}

	s.publishAuditEvent(c, "passkey.deleted", map[string]any{
		"user_id":       cred.UserID,
		"credential_id": cred.ID,
		"deleted_by":    sess.UserID,
	})

	c.JSON(http.StatusOK, gin.H{
		"message":       "ok",
		"rp_id":         cred.RPID,
		"credential_id": cred.ID,
	})
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

	ctx, cancel := s.opContext(c, 30*time.Second)
	defer cancel()
	if err := s.webauthnMgr.RenameCredential(ctx, cred.ID, name); err != nil {
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

	ctx, cancel := s.opContext(c, 30*time.Second)
	defer cancel()
	token, userInfo, err := s.inviteMgr.CreateInvite(ctx, authpkg.CreateInviteInput{
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

	s.publishAuditEvent(c, "invite.created", map[string]any{
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
	regHost := authpkg.NormalizeHost(c.Request.Host)

	options, sessionID, err := s.webauthnMgr.BeginRegistration(
		ctx, userID, username,
		rpID, rpDisplayName, rpOrigin, regHost, uv,
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
		if s.mapRegistrationError(c, err) {
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

	s.publishAuditEvent(c, "passkey.registered", map[string]any{
		"user_id":       cred.UserID,
		"credential_id": cred.ID,
		"rp_id":         cred.RPID,
	})
	s.publishAuditEvent(c, "invite.consumed", map[string]any{
		"user_id": userID,
	})

	// Create session
	boundOrigin := s.computeCanonicalOrigin(c)
	sess := s.sessions.CreatePortalSession(
		userInfo.ID, userInfo.Username, string(userInfo.Role),
		boundOrigin, portalSessionTTL,
	)
	s.setSessionCookie(c, sess.ID, portalSessionCookieTTL)

	c.JSON(http.StatusOK, gin.H{
		"message":       "ok",
		"credential_id": cred.ID,
		"created_at":    cred.CreatedAt.Format(time.RFC3339),
	})
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

	ctx, cancel := s.opContext(c, 30*time.Second)
	defer cancel()
	token, err := s.inviteMgr.ReinviteUser(ctx, userID, sess.UserID)
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
