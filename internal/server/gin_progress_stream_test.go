package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"piccolod/internal/events"
)

func TestTaskProgressReconnectReplaysTerminalOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "rollback", "access_repair"} {
		t.Run(outcome, func(t *testing.T) {
			bus := events.NewBus()
			reporter := events.NewBusProgressReporter(bus)
			srv := &GinServer{events: bus, progress: reporter}
			router := gin.New()
			router.GET("/progress", srv.handleGinTaskProgressStream)
			httpServer := httptest.NewServer(router)
			defer httpServer.Close()
			url := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/progress?task_id=original-task"
			dial := func() *websocket.Conn {
				t.Helper()
				conn, _, err := websocket.DefaultDialer.Dial(url, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
					t.Fatal(err)
				}
				return conn
			}
			type message struct {
				Type    string                   `json:"type"`
				Payload events.TaskProgressEvent `json:"payload"`
			}
			reporter.Report(events.TaskProgressEvent{TaskID: "original-task", Phase: "stopping_containers", Timestamp: time.Now()})
			conn := dial()
			var initial message
			if err := conn.ReadJSON(&initial); err != nil {
				t.Fatal(err)
			}
			if initial.Payload.IsComplete || initial.Payload.TaskID != "original-task" {
				t.Fatalf("initial progress = %+v", initial)
			}
			_ = conn.Close() // Simulate the portal disconnecting during Apply.
			terminal := events.TaskProgressEvent{TaskID: "original-task", Phase: "complete", IsComplete: true, Timestamp: time.Now()}
			if outcome == "rollback" {
				terminal.Error = "config update rolled back: candidate unavailable"
			} else if outcome == "access_repair" {
				terminal.Metadata = map[string]any{
					"access_repair_pending": true,
					"access_repair_message": "Config committed, but publication needs repair.",
				}
			}
			reporter.Report(terminal)
			conn = dial()
			defer conn.Close()
			var replay message
			if err := conn.ReadJSON(&replay); err != nil {
				t.Fatal(err)
			}
			if replay.Type != "task_progress" || replay.Payload.TaskID != terminal.TaskID || !replay.Payload.IsComplete || replay.Payload.Error != terminal.Error {
				t.Fatalf("terminal replay = %+v, want %+v", replay, terminal)
			}
			if outcome == "access_repair" && (replay.Payload.Metadata["access_repair_pending"] != true || replay.Payload.Metadata["access_repair_message"] != terminal.Metadata["access_repair_message"]) {
				t.Fatalf("terminal replay lost repair details: %+v", replay.Payload)
			}
		})
	}
}
