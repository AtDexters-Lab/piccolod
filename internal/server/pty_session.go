package server

import (
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
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
	// Handle PTY -> WebSocket (Output)
	go s.handleOutput()

	// Handle WebSocket -> PTY (Input + Resize)
	s.handleInput()
}

// Close cleans up PTY resources.
func (s *PTYSession) Close() {
	if s.ptmx != nil {
		_ = s.ptmx.Close()
	}
}

// handleOutput reads from PTY and writes to WebSocket.
func (s *PTYSession) handleOutput() {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if err != nil {
			if err != io.EOF {
				// Connection closed or PTY terminated
			}
			break
		}

		// Encode raw PTY bytes to Base64
		encoded := base64.StdEncoding.EncodeToString(buf[:n])

		msg := terminalMessage{
			Type: "stdout",
			Data: encoded,
		}

		s.wsMu.Lock()
		if err := s.conn.WriteJSON(msg); err != nil {
			s.wsMu.Unlock()
			break
		}
		s.wsMu.Unlock()
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
