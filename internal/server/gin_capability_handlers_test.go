package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/app"
)

func capabilityProviderDefinitionForHandlerTest(name string) *api.AppDefinition {
	return &api.AppDefinition{
		Type: "user",
		Listeners: []api.AppListener{{
			Name:      name,
			GuestPort: 8080,
			Flow:      api.FlowTCP,
			Protocol:  api.ListenerProtocolHTTP,
			Primary:   true,
			Provides: []api.CapabilityProvider{{
				Capability: api.CapabilityAIInferenceOpenAIV1,
				BasePath:   "/v1",
			}},
		}},
		Services: map[string]api.AppService{
			"main": {
				Image:     "alpine:latest",
				BindPorts: []int{8080},
			},
		},
		Extensions: map[string]interface{}{
			"mode":              "service",
			"requires_features": []string{api.FeatureCapabilityBindingsV1},
		},
	}
}

func TestGinInstallCapabilityReconcilePendingKeepsCommittedBookkeeping(t *testing.T) {
	server, mockContainer := createGinTestServerWithContainerManager(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, server)
	mockContainer.removeError = errors.New("injected capability recreation failure")

	manifest := `type: user
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: /
          type: prefix
          strategy: public
    provides:
      - capability: ai.inference.openai.v1
        base_path: /v1
services:
  main:
    image: alpine:latest
    bind_ports: [8080]
x-piccolo:
  mode: service
  requires_features:
    - capability_bindings_v1
`
	body, err := json.Marshal(map[string]any{
		"app_definition": manifest,
		"inputs": map[string]any{
			"__app_address__": "provider",
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/apps", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	attachAuth(request, sessionCookie, csrfToken)
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("install status=%d body=%s, want reconciliation-pending 202", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "capability reconciliation pending") {
		t.Fatalf("install response omitted pending result: %s", recorder.Body.String())
	}
	if _, err := server.appManager.Get(context.Background(), "provider"); err != nil {
		t.Fatalf("pending install was not committed: %v", err)
	}
	statuses, err := server.appManager.ListCapabilities(context.Background())
	if err != nil {
		t.Fatalf("ListCapabilities: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Default != "provider" {
		t.Fatalf("capability status after pending install = %+v", statuses)
	}
	if _, err := server.appManager.ReadInstalledConfig(context.Background(), "provider"); err != nil {
		t.Fatalf("pending install skipped install-state bookkeeping: %v", err)
	}
}

func TestGinCapabilityProviderChangeDisclosureAndAcknowledgement(t *testing.T) {
	server := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, server)

	first, err := server.appManager.Install(
		context.Background(),
		capabilityProviderDefinitionForHandlerTest("providerone"),
	)
	if err != nil {
		t.Fatalf("install first provider: %v", err)
	}
	second, err := server.appManager.Install(
		context.Background(),
		capabilityProviderDefinitionForHandlerTest("providertwo"),
	)
	if err != nil {
		t.Fatalf("install second provider: %v", err)
	}
	if err := server.appManager.SelectCapabilityProvider(
		context.Background(),
		api.CapabilityAIInferenceOpenAIV1,
		first.InstanceID,
	); err != nil {
		var pending *app.CapabilitySelectionReconcilePendingError
		if !errors.As(err, &pending) {
			t.Fatalf("seed first provider default: %v", err)
		}
	}

	getStatus := func() struct {
		Capabilities []app.CapabilityStatus `json:"capabilities"`
	} {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
		attachAuth(request, sessionCookie, csrfToken)
		server.router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("list capabilities status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Capabilities []app.CapabilityStatus `json:"capabilities"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("parse capabilities response: %v", err)
		}
		return response
	}

	status := getStatus()
	if len(status.Capabilities) != 1 ||
		status.Capabilities[0].Default != first.InstanceID ||
		status.Capabilities[0].ProviderChangeDisclosure != app.CapabilityProviderChangeDisclosure {
		t.Fatalf("capability status = %+v", status.Capabilities)
	}

	body := []byte(`{"app_instance":"` + second.InstanceID + `"}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/capabilities/"+api.CapabilityAIInferenceOpenAIV1+"/default",
		bytes.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	attachAuth(request, sessionCookie, csrfToken)
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict ||
		!strings.Contains(recorder.Body.String(), "not migrated") {
		t.Fatalf("unacknowledged switch status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := getStatus().Capabilities[0].Default; got != first.InstanceID {
		t.Fatalf("default changed without acknowledgement: %q", got)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodDelete, "/api/v1/apps/"+first.InstanceID, nil)
	attachAuth(request, sessionCookie, csrfToken)
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict ||
		!strings.Contains(recorder.Body.String(), "not migrated") {
		t.Fatalf("unacknowledged selected-provider removal status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	body = []byte(`{"app_instance":"` + second.InstanceID + `","acknowledge_provider_change":true}`)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodPut,
		"/api/v1/capabilities/"+api.CapabilityAIInferenceOpenAIV1+"/default",
		bytes.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	attachAuth(request, sessionCookie, csrfToken)
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent && recorder.Code != http.StatusAccepted {
		t.Fatalf("acknowledged switch status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := getStatus().Capabilities[0].Default; got != second.InstanceID {
		t.Fatalf("acknowledged switch default = %q, want %q", got, second.InstanceID)
	}

	body = []byte(`{"app_instance":"` + second.InstanceID + `"}`)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodPut,
		"/api/v1/capabilities/"+api.CapabilityAIInferenceOpenAIV1+"/default",
		bytes.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	attachAuth(request, sessionCookie, csrfToken)
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent && recorder.Code != http.StatusAccepted {
		t.Fatalf("same-provider repair status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodDelete, "/api/v1/apps/"+second.InstanceID, nil)
	attachAuth(request, sessionCookie, csrfToken)
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict ||
		!strings.Contains(recorder.Body.String(), "not migrated") {
		t.Fatalf("unacknowledged replacement removal status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodDelete,
		"/api/v1/apps/"+second.InstanceID+"?acknowledge_provider_change=true",
		nil,
	)
	attachAuth(request, sessionCookie, csrfToken)
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
		t.Fatalf("acknowledged selected-provider removal status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := server.appManager.GetAppDefinition(context.Background(), second.InstanceID); err == nil {
		t.Fatalf("acknowledged selected-provider removal left app %q installed", second.InstanceID)
	}
	if got := getStatus().Capabilities[0].Default; got != first.InstanceID {
		t.Fatalf("acknowledged removal default = %q, want replacement %q", got, first.InstanceID)
	}
}
