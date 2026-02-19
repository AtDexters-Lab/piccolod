package terminal

import "time"

// WebSocket keep-alive constants. Proxies (including remote-access tunnels)
// drop idle connections; periodic ping frames prevent this.
const (
	PingInterval = 15 * time.Second // how often to send Ping frames
	PongWait     = 20 * time.Second // max time to wait for a Pong response
	WriteWait    = 10 * time.Second // deadline for any single write
)

// Message is the JSON protocol spoken over the terminal WebSocket.
type Message struct {
	Type string `json:"type"`           // "stdin", "stdout", "resize"
	Data string `json:"data,omitempty"` // Base64 encoded string
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}
