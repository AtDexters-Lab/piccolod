package server

import (
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
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

func (s *GinServer) handleTerminal(c *gin.Context) {
	log.Println("DEBUG: handleTerminal - Request received from", c.ClientIP())

	conn, err := wsupgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Terminal upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	log.Println("DEBUG: handleTerminal - WebSocket upgraded successfully")

	// Default to bash, fall back to sh
	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}

	cmd := exec.Command(shell)
	// Inherit environment but force xterm-256color as requested by UI
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		sendError(conn, "Failed to start shell: "+err.Error())
		return
	}
	defer func() { _ = ptmx.Close() }() // closes the pty, killing the command

	// Use a mutex to prevent concurrent writes to the websocket
	// (NextWriter and WriteMessage are not thread-safe)
	var wsMu sync.Mutex

	// Handle PTY -> WebSocket (Output)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				if err != io.EOF {
					// log.Printf("PTY read error: %v", err)
				}
				break
			}

			// Encode raw PTY bytes to Base64
			encoded := base64.StdEncoding.EncodeToString(buf[:n])

			msg := terminalMessage{
				Type: "stdout",
				Data: encoded,
			}

			wsMu.Lock()
			if err := conn.WriteJSON(msg); err != nil {
				wsMu.Unlock()
				break
			}
			wsMu.Unlock()
		}
	}()

	// Handle WebSocket -> PTY (Input + Resize)
	for {
		// Read JSON message
		var msg terminalMessage
		err := conn.ReadJSON(&msg)
		if err != nil {
			break
		}

		switch msg.Type {
		case "stdin":
			if msg.Data != "" {
				decoded, err := base64.StdEncoding.DecodeString(msg.Data)
				if err == nil {
					_, _ = ptmx.Write(decoded)
				}
			}
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(msg.Rows), Cols: uint16(msg.Cols)})
			}
		}
	}
}

func sendError(conn *websocket.Conn, msg string) {
	// Best effort error reporting
	encoded := base64.StdEncoding.EncodeToString([]byte(msg + "\r\n"))
	_ = conn.WriteJSON(terminalMessage{Type: "stdout", Data: encoded})
}
