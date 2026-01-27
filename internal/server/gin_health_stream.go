package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/events"
	"piccolod/internal/services"
)

type healthMessage struct {
	Type    string `json:"type"`
	Payload any    `json:"payload,omitempty"`
}

// handleGinListenerHealthStream streams listener health events via WebSocket.
// Clients receive real-time updates when listener health status changes.
//
// Query parameters:
//   - app: (optional) filter by app name
//   - listener: (optional) filter by listener name (requires app)
//
// WebSocket message format:
//
//	{ "type": "listener_health", "payload": ListenerHealthEvent }
//	{ "type": "keepalive" }
func (s *GinServer) handleGinListenerHealthStream(c *gin.Context) {
	// Parse optional filters
	appFilter := strings.TrimSpace(c.Query("app"))
	listenerFilter := strings.TrimSpace(c.Query("listener"))

	// If listener filter is provided, app filter is required
	if listenerFilter != "" && appFilter == "" {
		writeGinError(c, http.StatusBadRequest, "app parameter required when filtering by listener")
		return
	}

	// Check access for standard users when filtering by app
	if appFilter != "" {
		if sess := s.getSessionFromContext(c); sess != nil && sess.Role != "admin" {
			if s.userManager != nil {
				allowed, err := s.userManager.IsAppAllowed(c.Request.Context(), sess.UserID, appFilter)
				if err != nil || !allowed {
					writeGinError(c, http.StatusForbidden, "Access denied")
					return
				}
			}
		}
	}

	conn, err := wsupgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	wsSendMu := sync.Mutex{}
	sendJSON := func(v any) error {
		wsSendMu.Lock()
		defer wsSendMu.Unlock()
		return conn.WriteJSON(v)
	}

	// Send initial health for all matching listeners
	if s.serviceManager != nil {
		// Build access check function for non-admin users without app filter
		var isAppAllowed func(app string) bool
		if appFilter == "" {
			if sess := s.getSessionFromContext(c); sess != nil && sess.Role != "admin" {
				if s.userManager != nil {
					isAppAllowed = func(app string) bool {
						allowed, _ := s.userManager.IsAppAllowed(c.Request.Context(), sess.UserID, app)
						return allowed
					}
				}
			}
		}
		s.sendInitialListenerHealth(appFilter, listenerFilter, isAppAllowed, sendJSON)
	}

	// Subscribe to health change events
	evtCh, unsubscribe := s.events.SubscribeWithCancel(events.TopicListenerHealthChanged, 256)
	defer unsubscribe()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				cancel()
				return
			}
		}
	}()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-keepalive.C:
			_ = sendJSON(healthMessage{Type: "keepalive"})
		case <-ctx.Done():
			_ = conn.Close()
			<-readDone
			return
		case evt, ok := <-evtCh:
			if !ok {
				_ = conn.Close()
				<-readDone
				return
			}
			payload, ok := evt.Payload.(events.ListenerHealthEvent)
			if !ok {
				continue
			}

			// Apply filters
			if appFilter != "" && payload.App != appFilter {
				continue
			}
			if listenerFilter != "" && payload.Listener != listenerFilter {
				continue
			}

			// Check access for unfiltered requests (standard users)
			if appFilter == "" {
				if sess := s.getSessionFromContext(c); sess != nil && sess.Role != "admin" {
					if s.userManager != nil {
						allowed, _ := s.userManager.IsAppAllowed(c.Request.Context(), sess.UserID, payload.App)
						if !allowed {
							continue
						}
					}
				}
			}

			if err := sendJSON(healthMessage{Type: "listener_health", Payload: payload}); err != nil {
				cancel()
				continue
			}
		}
	}
}

// sendInitialListenerHealth sends current health state for matching listeners.
// isAppAllowed is called for each endpoint when appFilter is empty and the user is non-admin;
// if nil, all endpoints are sent (admin or filtered request).
func (s *GinServer) sendInitialListenerHealth(appFilter, listenerFilter string, isAppAllowed func(string) bool, sendJSON func(any) error) {
	var endpoints []services.ServiceEndpoint

	// Get all relevant endpoints
	if appFilter != "" {
		eps, err := s.serviceManager.GetByApp(appFilter)
		if err != nil {
			return
		}
		for _, ep := range eps {
			if listenerFilter != "" && ep.Name != listenerFilter {
				continue
			}
			endpoints = append(endpoints, ep)
		}
	} else {
		// Get all endpoints from all apps
		endpoints = s.serviceManager.GetAll()
	}

	// Send initial health for each endpoint
	for _, ep := range endpoints {
		// Filter by access when no app filter and user is non-admin
		if isAppAllowed != nil && !isAppAllowed(ep.App) {
			continue
		}

		health := s.computeListenerHealth(ep)
		_ = sendJSON(healthMessage{
			Type: "listener_health",
			Payload: events.ListenerHealthEvent{
				App:       ep.App,
				Listener:  ep.Name,
				Health:    toEventsListenerHealth(health),
				Timestamp: time.Now(),
			},
		})
	}
}

// toEventsListenerHealth converts services.ListenerHealth to events.ListenerHealth.
func toEventsListenerHealth(h services.ListenerHealth) events.ListenerHealth {
	// Convert CertStatuses if present
	var certStatuses map[string]events.CertHealthStatus
	if len(h.CertStatuses) > 0 {
		certStatuses = make(map[string]events.CertHealthStatus, len(h.CertStatuses))
		for certID, cs := range h.CertStatuses {
			certStatuses[certID] = events.CertHealthStatus{
				Status:      string(cs.Status),
				ReasonCode:  cs.ReasonCode,
				RecoveryETA: cs.RecoveryETA,
			}
		}
	}

	return events.ListenerHealth{
		Status:         string(h.Status),
		ReasonCode:     h.ReasonCode,
		Reason:         h.Reason,
		Details:        h.Details,
		RecoveryETA:    h.RecoveryETA,
		Recoverable:    h.Recoverable,
		ActionRequired: h.ActionRequired,
		CertStatuses:   certStatuses,
		LastChecked:    h.LastChecked,
		LastOK:         h.LastOK,
	}
}
