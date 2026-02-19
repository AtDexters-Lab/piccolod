package server

import (
	"encoding/base64"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// WebSocket keep-alive constants. Proxies (including remote-access tunnels)
// drop idle connections; periodic ping frames prevent this.
const (
	wsPingInterval = 15 * time.Second // how often to send Ping frames
	wsPongWait     = 20 * time.Second // max time to wait for a Pong response
	wsWriteWait    = 10 * time.Second // deadline for any single write
)

// PTYSession manages bidirectional communication between a PTY and WebSocket.
// It handles the common logic for both host terminal and workspace terminal.
type PTYSession struct {
	conn *websocket.Conn
	ptmx *os.File
	wsMu sync.Mutex
}

// NewPTYSession creates a new session by starting the command with a PTY.
// The caller must call Close() to clean up resources.
func NewPTYSession(conn *websocket.Conn, cmd *exec.Cmd) (*PTYSession, error) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &PTYSession{
		conn: conn,
		ptmx: ptmx,
	}, nil
}

// Run starts bidirectional I/O loops and blocks until the connection closes.
// It handles PTY output -> WebSocket and WebSocket input -> PTY.
func (s *PTYSession) Run() {
	// Set up WebSocket keep-alive: server sends Ping frames, browser auto-responds
	// with Pong. If no Pong arrives within wsPongWait, the read deadline expires
	// and handleInput exits, tearing down the session.
	_ = s.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	pingDone := make(chan struct{})
	outputDone := make(chan struct{})

	// Handle PTY -> WebSocket (Output)
	go func() {
		defer close(outputDone)
		s.handleOutput()
	}()

	// Periodic Ping frames to keep proxies from killing idle connections
	go s.pingLoop(pingDone)

	// Handle WebSocket -> PTY (Input + Resize) — blocks until connection closes
	s.handleInput()
	close(pingDone)
	_ = s.ptmx.Close() // unblock handleOutput's ptmx.Read
	<-outputDone
}

// Close cleans up PTY resources.
func (s *PTYSession) Close() {
	if s.ptmx != nil {
		_ = s.ptmx.Close()
	}
}

// handleOutput reads from PTY and writes to WebSocket.
// When the PTY closes (shell exits), this sends a proper close frame to signal the frontend.
func (s *PTYSession) handleOutput() {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if err != nil {
			// PTY closed (shell exited) - send proper WebSocket close frame
			// Code 1000 = normal closure, frontend should NOT reconnect
			s.wsMu.Lock()
			s.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			_ = s.conn.WriteMessage(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shell exited"),
			)
			_ = s.conn.Close()
			s.wsMu.Unlock()
			break
		}

		// Encode raw PTY bytes to Base64
		encoded := base64.StdEncoding.EncodeToString(buf[:n])

		msg := terminalMessage{
			Type: "stdout",
			Data: encoded,
		}

		s.wsMu.Lock()
		s.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
		if err := s.conn.WriteJSON(msg); err != nil {
			s.wsMu.Unlock()
			break
		}
		s.wsMu.Unlock()
	}
}

// pingLoop sends WebSocket Ping frames at regular intervals to keep the
// connection alive through proxies and detect dead peers.
func (s *PTYSession) pingLoop(done <-chan struct{}) {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.wsMu.Lock()
			s.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			err := s.conn.WriteMessage(websocket.PingMessage, nil)
			s.wsMu.Unlock()
			if err != nil {
				_ = s.conn.Close() // unblock handleInput's ReadJSON
				return
			}
		case <-done:
			return
		}
	}
}

// handleInput reads from WebSocket and writes to PTY.
func (s *PTYSession) handleInput() {
	for {
		var msg terminalMessage
		if err := s.conn.ReadJSON(&msg); err != nil {
			break
		}

		switch msg.Type {
		case "stdin":
			if msg.Data != "" {
				decoded, err := base64.StdEncoding.DecodeString(msg.Data)
				if err == nil {
					_, _ = s.ptmx.Write(decoded)
				}
			}
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				_ = pty.Setsize(s.ptmx, &pty.Winsize{Rows: uint16(msg.Rows), Cols: uint16(msg.Cols)})
			}
		}
	}
}
