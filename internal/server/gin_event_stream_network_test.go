package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"

	"piccolod/internal/events"
)

func TestNetworkStatusStreamSubscribesToTopologyAndSignalChanges(t *testing.T) {
	topics := supportedTopics[topicNetworkStatus]
	if !slices.Contains(topics, events.TopicNetworkTransition) {
		t.Fatal("network_status does not subscribe to topology transitions")
	}
	if !slices.Contains(topics, events.TopicWiFiSignalChanged) {
		t.Fatal("network_status does not subscribe to WiFi signal changes")
	}
}

func TestHandleNetworkStatusWithoutManagerReturnsTypedUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/network/status", nil)

	(&GinServer{}).handleNetworkStatus(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["active_uplink"] != "none" || payload["connectivity"] != "unknown" {
		t.Fatalf("fallback status = %v, want none/unknown", payload)
	}
	if _, exists := payload["state"]; exists {
		t.Fatalf("fallback status restored removed compatibility state: %v", payload)
	}
	if _, ok := payload["interfaces"].([]any); !ok {
		t.Fatalf("interfaces = %T, want array", payload["interfaces"])
	}
}

func TestFilterNetworkStatusTopicEnforcesRESTAccessBoundary(t *testing.T) {
	tests := []struct {
		name       string
		isAdmin    bool
		hasAccess  bool
		wantDenied bool
		wantTopic  bool
	}{
		{name: "local admin", isAdmin: true, hasAccess: true, wantTopic: true},
		{name: "remote admin", isAdmin: true, wantDenied: true},
		{name: "local non-admin", hasAccess: true, wantDenied: true},
		{name: "remote non-admin", wantDenied: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			topics := map[string]bool{topicNetworkStatus: true, topicAppStatus: true}
			denied := filterNetworkStatusTopic(topics, tt.isAdmin, tt.hasAccess)
			if denied != tt.wantDenied {
				t.Fatalf("denied = %v, want %v", denied, tt.wantDenied)
			}
			if got := topics[topicNetworkStatus]; got != tt.wantTopic {
				t.Fatalf("network_status retained = %v, want %v", got, tt.wantTopic)
			}
			if !topics[topicAppStatus] {
				t.Fatal("filter removed unrelated topic")
			}
		})
	}
}
