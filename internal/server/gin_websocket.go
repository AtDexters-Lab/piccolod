package server

import (
	"encoding/base64"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"

	"piccolod/internal/resources/pressure"
)

const (
	wsPingInterval = 15 * time.Second
	wsPongWait     = 20 * time.Second
	wsWriteWait    = 10 * time.Second
)

var wsupgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Origin policy is enforced by the HTTP middleware before upgrade.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// terminalMessage is the existing wire envelope shared by terminal-session
// errors and child-backed log streams.
type terminalMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

func getShell() string {
	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	return shell
}

func sendError(conn *websocket.Conn, msg string) {
	encoded := base64.StdEncoding.EncodeToString([]byte(msg + "\r\n"))
	_ = conn.WriteJSON(terminalMessage{Type: "stdout", Data: encoded})
}

func writeTaskPressureError(c interface {
	AbortWithStatusJSON(int, interface{})
}, err error) bool {
	if !pressure.IsAdmissionError(err) {
		return false
	}
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, map[string]interface{}{
		"error":     "Piccolo is temporarily limiting new process work",
		"code":      "task_pressure",
		"retryable": true,
	})
	return true
}
