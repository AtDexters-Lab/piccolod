package server

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"piccolod/internal/events"
	"piccolod/internal/network"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/services"
)

// Supported event stream topics
const (
	topicAppStatus        = "app_status"
	topicListenerHealth   = "listener_health"
	topicRemoteConfig     = "remote_config"
	topicCertificate      = "certificate"
	topicNetworkPeers     = "network_peers"
	topicIdentity         = "identity"
	topicNetworkStatus    = "network_status"
	topicStorageAlert     = "storage_alert"
	topicResourcePressure = "resource_pressure"
	topicSnapshotComplete = "snapshot_complete"
)

var supportedTopics = map[string][]events.Topic{
	topicAppStatus:        {events.TopicAppStatusChanged},
	topicListenerHealth:   {events.TopicListenerHealthChanged},
	topicRemoteConfig:     {events.TopicRemoteConfigChanged},
	topicCertificate:      {events.TopicCertificateChanged},
	topicNetworkPeers:     {events.TopicNetworkPeersChanged},
	topicIdentity:         {events.TopicIdentityChanged},
	topicNetworkStatus:    {events.TopicNetworkTransition, events.TopicWiFiSignalChanged},
	topicStorageAlert:     {events.TopicStorageAlert},
	topicResourcePressure: {events.TopicResourcePressure},
}

type streamMessage struct {
	Type    string `json:"type"`
	Payload any    `json:"payload,omitempty"`
}

// handleGinEventStream streams multiple event types via a single WebSocket connection.
// Clients can subscribe to specific topics or receive all events.
//
// Query parameters:
//   - topics: (optional) comma-separated list of topics to subscribe to.
//     Supported: app_status, listener_health, remote_config, certificate,
//     network_peers, identity, network_status, storage_alert, resource_pressure
//     If omitted, subscribes to all topics.
//     network_peers is automatically stripped for remote clients (via Nexus proxy).
//     network_status requires admin plus LAN or same-public-IP access.
//
// WebSocket message format:
//
//	{ "type": "app_status", "payload": AppStatusChangedEvent }
//	{ "type": "listener_health", "payload": ListenerHealthEvent }
//	{ "type": "remote_config", "payload": remote.Status }
//	{ "type": "certificate", "payload": CertificateChangedEvent }
//	{ "type": "network_peers", "payload": NetworkPeersChangedEvent }
//	{ "type": "identity", "payload": IdentityStatusPayload }
//	{ "type": "network_status", "payload": network.NetworkStatus }
//	{ "type": "storage_alert", "payload": events.StorageAlertEvent }
//	{ "type": "resource_pressure", "payload": events.ResourcePressureEvent }
//
// Keep-alive uses WebSocket Ping frames (not application-level messages).
//
// On connect, sends initial snapshot for subscribed topics.
// Access control: standard users only receive events for their allowed apps.
func (s *GinServer) handleGinEventStream(c *gin.Context) {
	// Parse topics query param
	topicsParam := strings.TrimSpace(c.Query("topics"))
	requestedTopics := make(map[string]bool)

	if topicsParam == "" {
		// Subscribe to all supported topics
		for topic := range supportedTopics {
			requestedTopics[topic] = true
		}
	} else {
		// Parse comma-separated topics
		for _, t := range strings.Split(topicsParam, ",") {
			t = strings.TrimSpace(t)
			if _, ok := supportedTopics[t]; ok {
				requestedTopics[t] = true
			}
			// Ignore unknown topics (per reviewer suggestion: log warning)
		}
	}

	// LAN-only guard: remote clients arrive via the Nexus reverse proxy which
	// binds to loopback, so loopback RemoteAddr implies remote access.
	// Strip network_peers since peer data is LAN-only.
	// Fail-closed: if we can't parse RemoteAddr, assume remote and strip.
	if host, _, err := net.SplitHostPort(c.Request.RemoteAddr); err != nil {
		delete(requestedTopics, topicNetworkPeers)
	} else if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		delete(requestedTopics, topicNetworkPeers)
	}

	// Validate session BEFORE WebSocket upgrade (so we can return proper HTTP errors)
	sess := s.getSessionFromContext(c)
	if sess == nil {
		// Session should always exist after requireSession middleware.
		// If nil, abort as defense-in-depth (don't default to privileged access).
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session required"})
		return
	}
	isAdmin := sess.Role == "admin"
	networkStatusDenied := false
	if requestedTopics[topicNetworkStatus] {
		networkAccess := isAdmin && s.hasLANOrSamePublicIPAccess(c)
		networkStatusDenied = filterNetworkStatusTopic(requestedTopics, isAdmin, networkAccess)
	}
	if len(requestedTopics) == 0 {
		if networkStatusDenied {
			writeGinError(c, http.StatusForbidden, "Network status requires local admin access")
		} else {
			writeGinError(c, http.StatusBadRequest, "No valid topics specified")
		}
		return
	}

	conn, err := wsupgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Set up WebSocket keep-alive: Pong responses extend the read deadline;
	// if no Pong arrives within wsPongWait the read pump exits and tears down.
	// The initial read deadline is deferred until after snapshots are sent (below)
	// so that slow snapshot delivery doesn't eat into the pong window.
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	wsSendMu := sync.Mutex{}
	sendJSON := func(v any) error {
		wsSendMu.Lock()
		defer wsSendMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
		return conn.WriteJSON(v)
	}
	sendPing := func() error {
		wsSendMu.Lock()
		defer wsSendMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
		return conn.WriteMessage(websocket.PingMessage, nil)
	}

	// Build access control helper for non-admin users
	isAppAllowed := func(appID string) bool {
		if isAdmin {
			return true
		}
		if s.userManager == nil {
			return false
		}
		allowed, _ := s.userManager.IsAppAllowed(c.Request.Context(), sess.UserID, appID)
		return allowed
	}

	// Subscribe to event bus topics BEFORE sending initial snapshot
	// This ensures no events are missed during snapshot collection
	type subscription struct {
		topic  string
		ch     <-chan events.Event
		cancel func()
	}
	var subscriptions []subscription

	for topicName := range requestedTopics {
		for _, busTopic := range supportedTopics[topicName] {
			ch, cancelFn := s.events.SubscribeWithCancel(busTopic, 256)
			subscriptions = append(subscriptions, subscription{
				topic:  topicName,
				ch:     ch,
				cancel: cancelFn,
			})
		}
	}
	defer func() {
		for _, sub := range subscriptions {
			sub.cancel()
		}
	}()

	// Start read pump FIRST to handle control frames/disconnects during snapshot transmission
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

	// Send initial snapshots (read pump is running, so disconnects are detected)
	if requestedTopics[topicAppStatus] {
		s.sendInitialAppStatus(isAppAllowed, sendJSON)
	}
	if requestedTopics[topicListenerHealth] {
		s.sendInitialListenerHealthUnified(isAppAllowed, sendJSON)
	}
	if requestedTopics[topicRemoteConfig] && isAdmin {
		s.sendInitialRemoteConfig(sendJSON)
	}
	if requestedTopics[topicIdentity] && isAdmin {
		s.sendInitialIdentityStatus(sendJSON)
	}
	if requestedTopics[topicNetworkPeers] {
		s.sendInitialNetworkPeers(sendJSON)
	}
	if requestedTopics[topicNetworkStatus] {
		s.sendInitialNetworkStatus(sendJSON)
	}
	if requestedTopics[topicResourcePressure] {
		s.sendInitialResourcePressure(isAppAllowed, sendJSON)
	}
	// Certificate topic doesn't need initial snapshot (included in remote_config)
	for topicName := range requestedTopics {
		_ = sendJSON(streamMessage{
			Type:    topicSnapshotComplete,
			Payload: map[string]string{"topic": topicName},
		})
	}

	// Merge all subscription channels
	eventCh := make(chan struct {
		topic string
		evt   events.Event
	}, 256)

	for _, sub := range subscriptions {
		go func(topicName string, ch <-chan events.Event) {
			for evt := range ch {
				select {
				case eventCh <- struct {
					topic string
					evt   events.Event
				}{topicName, evt}:
				case <-ctx.Done():
					return
				}
			}
		}(sub.topic, sub.ch)
	}

	// Start the read deadline now that snapshots are sent and pings will begin.
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))

	keepalive := time.NewTicker(wsPingInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-keepalive.C:
			if err := sendPing(); err != nil {
				cancel()
			}

		case <-ctx.Done():
			_ = conn.Close()
			<-readDone
			return

		case item := <-eventCh:
			msg := s.processEvent(item.topic, item.evt, isAppAllowed, isAdmin)
			if msg != nil {
				if err := sendJSON(msg); err != nil {
					cancel()
				}
			}
		}
	}
}

func filterNetworkStatusTopic(requestedTopics map[string]bool, isAdmin, hasNetworkAccess bool) bool {
	if !requestedTopics[topicNetworkStatus] || (isAdmin && hasNetworkAccess) {
		return false
	}
	delete(requestedTopics, topicNetworkStatus)
	return true
}

// processEvent converts an event bus event to a stream message, applying access control.
// Returns nil if the event should be filtered out.
func (s *GinServer) processEvent(topic string, evt events.Event, isAppAllowed func(string) bool, isAdmin bool) *streamMessage {
	switch topic {
	case topicAppStatus:
		payload, ok := evt.Payload.(events.AppStatusChangedEvent)
		if !ok {
			return nil
		}
		if !isAppAllowed(payload.App) {
			return nil
		}
		return &streamMessage{Type: topicAppStatus, Payload: payload}

	case topicListenerHealth:
		payload, ok := evt.Payload.(events.ListenerHealthEvent)
		if !ok {
			return nil
		}
		if !isAppAllowed(payload.App) {
			return nil
		}
		return &streamMessage{Type: topicListenerHealth, Payload: payload}

	case topicRemoteConfig:
		// Remote config is admin-only
		if !isAdmin {
			return nil
		}
		return &streamMessage{Type: topicRemoteConfig, Payload: evt.Payload}

	case topicCertificate:
		// Certificate events are admin-only
		if !isAdmin {
			return nil
		}
		payload, ok := evt.Payload.(events.CertificateChangedEvent)
		if !ok {
			return nil
		}
		return &streamMessage{Type: topicCertificate, Payload: payload}

	case topicNetworkPeers:
		payload, ok := evt.Payload.(events.NetworkPeersChangedEvent)
		if !ok {
			return nil
		}
		return &streamMessage{Type: topicNetworkPeers, Payload: payload}

	case topicIdentity:
		if !isAdmin {
			return nil
		}
		return s.buildIdentitySnapshot()

	case topicNetworkStatus:
		switch evt.Payload.(type) {
		case network.NetworkTransitionEvent, network.WiFiSignalChangedEvent:
		default:
			return nil
		}
		if s.networkManager == nil {
			return nil
		}
		// The transport emits one complete status shape even though topology
		// and signal changes remain distinct internal facts.
		return &streamMessage{Type: topicNetworkStatus, Payload: s.networkManager.Status()}

	case topicStorageAlert:
		payload, ok := evt.Payload.(events.StorageAlertEvent)
		if !ok {
			return nil
		}
		if !isAdmin {
			// Strip instance IDs for non-admin users to avoid leaking
			// workspace names belonging to other users.
			payload.AffectedApps = nil
		}
		return &streamMessage{Type: topicStorageAlert, Payload: payload}

	case topicResourcePressure:
		payload, ok := evt.Payload.(events.ResourcePressureEvent)
		if !ok {
			return nil
		}
		if payload.AppInstanceID != "" && !isAppAllowed(payload.AppInstanceID) {
			return nil
		}
		return &streamMessage{Type: topicResourcePressure, Payload: payload}
	}
	return nil
}

func (s *GinServer) sendInitialResourcePressure(isAppAllowed func(string) bool, sendJSON func(any) error) {
	snapshot := s.TaskPressureSnapshot()
	payload := events.ResourcePressureEvent{
		Resource:    events.PressureResourceTasks,
		Severity:    events.PressureSeverityOK,
		ReasonCode:  snapshot.ReasonCode,
		ActionTaken: snapshot.ActionTaken,
	}
	if snapshot.CurrentKnown {
		current := snapshot.Current
		payload.TaskCurrent = &current
	}
	if snapshot.LimitKnown {
		limit := snapshot.Limit
		payload.TaskLimit = &limit
	}
	switch snapshot.State {
	case pressure.TaskPressureWarning, pressure.TaskPressureUnavailable:
		payload.Severity = events.PressureSeverityWarn
	case pressure.TaskPressureCritical:
		payload.Severity = events.PressureSeverityUrgent
	}
	_ = sendJSON(streamMessage{Type: topicResourcePressure, Payload: payload})
	if recoveryPayload := s.taskRecoveryGlobalPressureSnapshot(); recoveryPayload != nil {
		_ = sendJSON(streamMessage{Type: topicResourcePressure, Payload: *recoveryPayload})
	}
	if s.appManager == nil {
		return
	}
	for _, runtimePayload := range s.appManager.RuntimeObservationPressureSnapshot() {
		if runtimePayload.AppInstanceID != "" && !isAppAllowed(runtimePayload.AppInstanceID) {
			continue
		}
		_ = sendJSON(streamMessage{Type: topicResourcePressure, Payload: runtimePayload})
	}
}

func (s *GinServer) sendInitialNetworkStatus(sendJSON func(any) error) {
	if s.networkManager == nil {
		return
	}
	_ = sendJSON(streamMessage{
		Type:    topicNetworkStatus,
		Payload: s.networkManager.Status(),
	})
}

// sendInitialAppStatus sends current status for all accessible apps.
func (s *GinServer) sendInitialAppStatus(isAppAllowed func(string) bool, sendJSON func(any) error) {
	if s.appManager == nil {
		return
	}

	apps, err := s.appManager.List(context.Background())
	if err != nil {
		return
	}

	for _, app := range apps {
		if !isAppAllowed(app.InstanceID) {
			continue
		}
		_ = sendJSON(streamMessage{
			Type: topicAppStatus,
			Payload: events.AppStatusChangedEvent{
				App:       app.InstanceID,
				Status:    app.Status,
				Message:   app.StatusMessage,
				Timestamp: time.Now(),
			},
		})
	}
}

// sendInitialListenerHealthUnified sends current health for all accessible listeners.
func (s *GinServer) sendInitialListenerHealthUnified(isAppAllowed func(string) bool, sendJSON func(any) error) {
	if s.serviceManager == nil {
		return
	}

	endpoints := s.serviceManager.GetAll()
	for _, ep := range endpoints {
		if !isAppAllowed(ep.App) {
			continue
		}
		health := s.computeListenerHealth(ep)
		_ = sendJSON(streamMessage{
			Type: topicListenerHealth,
			Payload: events.ListenerHealthEvent{
				App:       ep.App,
				Listener:  ep.Name,
				Health:    toEventsListenerHealth(health),
				Timestamp: time.Now(),
			},
		})
	}
}

// sendInitialRemoteConfig sends current remote access configuration.
func (s *GinServer) sendInitialRemoteConfig(sendJSON func(any) error) {
	if s.remoteManager == nil {
		return
	}
	status := s.remoteManager.Status()
	_ = sendJSON(streamMessage{
		Type:    topicRemoteConfig,
		Payload: status,
	})
}

// sendInitialNetworkPeers sends the current peer list snapshot.
func (s *GinServer) sendInitialNetworkPeers(sendJSON func(any) error) {
	if s.mdnsManager == nil {
		return
	}
	_ = sendJSON(streamMessage{
		Type:    topicNetworkPeers,
		Payload: s.mdnsManager.SnapshotPeersForEvent(),
	})
}

// buildIdentitySnapshot constructs a stream message matching the REST handleIdentityStatus shape.
func (s *GinServer) buildIdentitySnapshot() *streamMessage {
	svc := s.identityService
	if svc == nil {
		return nil
	}
	return &streamMessage{
		Type:    topicIdentity,
		Payload: buildIdentityPayload(svc),
	}
}

// sendInitialIdentityStatus sends the current identity status snapshot.
func (s *GinServer) sendInitialIdentityStatus(sendJSON func(any) error) {
	msg := s.buildIdentitySnapshot()
	if msg != nil {
		_ = sendJSON(*msg)
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
