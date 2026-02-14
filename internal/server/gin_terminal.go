package server

import (
	"encoding/base64"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var wsupgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// CheckOrigin is handled by corsMiddleware, but we can double check if needed.
	// Allow all origins here because we rely on session auth + local network check.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// terminalMessage defines the shared JSON protocol for the WebSocket
type terminalMessage struct {
	Type string `json:"type"`           // "stdin", "stdout", "resize"
	Data string `json:"data,omitempty"` // Base64 encoded string
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// getShell returns the path to the preferred shell, defaulting to bash then sh.
func getShell() string {
	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	return shell
}

func (s *GinServer) handleTerminal(c *gin.Context) {
	log.Println("DEBUG: handleTerminal - Request received from", c.ClientIP())

	conn, err := wsupgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Terminal upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	log.Println("DEBUG: handleTerminal - WebSocket upgraded successfully")

	cmd := exec.CommandContext(c.Request.Context(), getShell())
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	session, err := NewPTYSession(conn, cmd)
	if err != nil {
		sendError(conn, "Failed to start shell: "+err.Error())
		return
	}
	defer session.Close()

	session.Run()
}

func sendError(conn *websocket.Conn, msg string) {
	// Best effort error reporting
	encoded := base64.StdEncoding.EncodeToString([]byte(msg + "\r\n"))
	_ = conn.WriteJSON(terminalMessage{Type: "stdout", Data: encoded})
}
