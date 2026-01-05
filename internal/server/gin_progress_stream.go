package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/events"
)

type progressMessage struct {
	Type    string `json:"type"`
	Payload any    `json:"payload,omitempty"`
}

func (s *GinServer) handleGinTaskProgressStream(c *gin.Context) {
	taskID := strings.TrimSpace(c.Query("task_id"))
	if taskID == "" {
		writeGinError(c, http.StatusBadRequest, "Missing query parameter: task_id")
		return
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

	if s.progress != nil {
		if evt, ok := s.progress.Last(taskID); ok {
			_ = sendJSON(progressMessage{Type: "task_progress", Payload: evt})
			if evt.IsComplete {
				return
			}
		}
	}

	evtCh, unsubscribe := s.events.SubscribeWithCancel(events.TopicTaskProgress, 256)
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
			_ = sendJSON(progressMessage{Type: "keepalive"})
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
			payload, ok := evt.Payload.(events.TaskProgressEvent)
			if !ok {
				continue
			}
			if payload.TaskID != taskID {
				continue
			}
			if err := sendJSON(progressMessage{Type: "task_progress", Payload: payload}); err != nil {
				cancel()
				continue
			}
			if payload.IsComplete {
				cancel()
			}
		}
	}
}
