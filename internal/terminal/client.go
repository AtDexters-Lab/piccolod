package terminal

import (
	"encoding/base64"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// CloseCodeDetached is sent when another client attaches to the same session,
// displacing this one.
const CloseCodeDetached = 4000

// Client bridges a single WebSocket connection to a persistent Session.
type Client struct {
	conn    *websocket.Conn
	session *Session
	wsMu    sync.Mutex
}

// NewClient wraps a WebSocket connection.
func NewClient(conn *websocket.Conn) *Client {
	return &Client{
		conn: conn,
	}
}

// Run blocks while reading WS messages and routing them to the session.
// Returns when the WebSocket closes or the session ends.
func (c *Client) Run(session *Session) {
	c.session = session

	// Set up ping/pong keep-alive
	_ = c.conn.SetReadDeadline(time.Now().Add(PongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(PongWait))
	})

	pingDone := make(chan struct{})
	pingExited := make(chan struct{})
	go func() {
		defer close(pingExited)
		c.pingLoop(pingDone)
	}()

	// Read loop
	for {
		var msg Message
		if err := c.conn.ReadJSON(&msg); err != nil {
			break
		}
		switch msg.Type {
		case "stdin":
			if msg.Data != "" {
				decoded, err := base64.StdEncoding.DecodeString(msg.Data)
				if err == nil {
					_ = session.WriteToPTY(decoded)
				}
			}
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				_ = session.Resize(msg.Cols, msg.Rows)
			}
		}
	}

	close(pingDone)
	<-pingExited
	session.DetachClient(c)
}

// SendOutput sends PTY output data to the WebSocket as a base64-encoded JSON message.
func (c *Client) SendOutput(data []byte) {
	encoded := base64.StdEncoding.EncodeToString(data)
	msg := Message{Type: "stdout", Data: encoded}

	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(WriteWait))
	_ = c.conn.WriteJSON(msg)
}

// SendSessionEnded sends a WebSocket close frame (code 1000) indicating the
// shell exited normally.
func (c *Client) SendSessionEnded() {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(WriteWait))
	_ = c.conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shell exited"),
	)
	_ = c.conn.Close()
}

// Detach sends a close frame (code 4000) to tell the client it was displaced
// by another attach, then closes the connection.
func (c *Client) Detach() {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(WriteWait))
	_ = c.conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(CloseCodeDetached, "detached"),
	)
	_ = c.conn.Close()
}

// pingLoop sends periodic ping frames to keep the WebSocket alive.
func (c *Client) pingLoop(done <-chan struct{}) {
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.wsMu.Lock()
			_ = c.conn.SetWriteDeadline(time.Now().Add(WriteWait))
			err := c.conn.WriteMessage(websocket.PingMessage, nil)
			c.wsMu.Unlock()
			if err != nil {
				_ = c.conn.Close()
				return
			}
		case <-done:
			return
		}
	}
}
