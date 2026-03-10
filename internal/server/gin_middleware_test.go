package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"piccolod/internal/health"
)

func TestRequireUnhealthy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		tracker    *health.Tracker
		setLevel   *health.Level // nil = don't set anything (fresh tracker = LevelOK)
		wantStatus int
	}{
		{
			name:       "nil_tracker_returns_403",
			tracker:    nil,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "level_ok_returns_403",
			tracker:    health.NewTracker(),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "level_warn_returns_403",
			tracker:    health.NewTracker(),
			setLevel:   levelPtr(health.LevelWarn),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "level_error_passes_through",
			tracker:    health.NewTracker(),
			setLevel:   levelPtr(health.LevelError),
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.tracker != nil && tt.setLevel != nil {
				tt.tracker.Setf("test", *tt.setLevel, "test")
			}

			s := &GinServer{healthTracker: tt.tracker}

			w := httptest.NewRecorder()
			c, r := gin.CreateTestContext(w)

			r.Use(s.requireUnhealthy())
			r.GET("/test", func(c *gin.Context) {
				c.Status(http.StatusOK)
			})

			c.Request = httptest.NewRequest(http.MethodGet, "/test", nil)
			r.ServeHTTP(w, c.Request)

			if w.Code != tt.wantStatus {
				t.Errorf("got status %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}

func levelPtr(l health.Level) *health.Level { return &l }
