package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"piccolod/internal/health"
)

func TestReadinessUsesOnlyRequiredComponents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		configure  func(*health.Tracker)
		wantStatus int
		wantReady  bool
	}{
		{
			name: "optional network error does not fail readiness",
			configure: func(tracker *health.Tracker) {
				setRequiredHealthOK(tracker)
				tracker.Setf("network", health.LevelError, "offline")
			},
			wantStatus: http.StatusOK,
			wantReady:  true,
		},
		{
			name: "missing required component fails readiness",
			configure: func(tracker *health.Tracker) {
				tracker.Setf("persistence", health.LevelOK, "ready")
				tracker.Setf("app-manager", health.LevelOK, "ready")
			},
			wantStatus: http.StatusServiceUnavailable,
			wantReady:  false,
		},
		{
			name: "required warning is boot healthy but not strictly ready",
			configure: func(tracker *health.Tracker) {
				setRequiredHealthOK(tracker)
				tracker.Setf("service-manager", health.LevelWarn, "initializing")
			},
			wantStatus: http.StatusOK,
			wantReady:  false,
		},
		{
			name: "required error fails readiness",
			configure: func(tracker *health.Tracker) {
				setRequiredHealthOK(tracker)
				tracker.Setf("persistence", health.LevelError, "unavailable")
			},
			wantStatus: http.StatusServiceUnavailable,
			wantReady:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := health.NewTracker()
			tt.configure(tracker)
			srv := &GinServer{healthTracker: tracker}

			w := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(w)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/health/ready", nil)
			srv.handleGinReadinessCheck(ctx)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tt.wantStatus, w.Body.String())
			}
			var payload struct {
				Ready bool `json:"ready"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if payload.Ready != tt.wantReady {
				t.Fatalf("ready = %v, want %v; body=%s", payload.Ready, tt.wantReady, w.Body.String())
			}
		})
	}
}

func TestReadinessWithoutTrackerFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/health/ready", nil)

	(&GinServer{}).handleGinReadinessCheck(ctx)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	var payload struct {
		Ready bool `json:"ready"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Ready {
		t.Fatalf("ready = true, want false; body=%s", w.Body.String())
	}
}

func setRequiredHealthOK(tracker *health.Tracker) {
	tracker.Setf("persistence", health.LevelOK, "ready")
	tracker.Setf("app-manager", health.LevelOK, "ready")
	tracker.Setf("service-manager", health.LevelOK, "ready")
}
