package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const maxLogTail = 5000

func parseLogTail(c *gin.Context, def int) int {
	tail := def
	if raw := strings.TrimSpace(c.Query("tail")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			tail = v
		}
	}
	if tail <= 0 {
		tail = def
	}
	if tail > maxLogTail {
		tail = maxLogTail
	}
	return tail
}

func parseBoolQuery(c *gin.Context, key string, def bool) bool {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return def
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}

type commandStream struct {
	r    *io.PipeReader
	once sync.Once
	stop func() error
}

func (s *commandStream) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s *commandStream) Close() error {
	var err error
	s.once.Do(func() {
		if s.stop != nil {
			err = s.stop()
		}
		_ = s.r.Close()
	})
	return err
}

func startCommandStream(ctx context.Context, cmd *exec.Cmd, cancel context.CancelFunc) (io.ReadCloser, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(pw, stdout)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(pw, stderr)
	}()

	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		wg.Wait()
		_ = pw.Close()
		done <- err
		close(done)
	}()

	stop := func() error {
		cancel()
		err, ok := <-done
		if ok && err != nil {
			return err
		}
		return nil
	}
	return &commandStream{r: pr, stop: stop}, nil
}

func (s *GinServer) handleGinAppLogStream(c *gin.Context) {
	instanceID := c.Param("name")
	tail := parseLogTail(c, 200)
	timestamps := parseBoolQuery(c, "timestamps", true)

	ctx, cancel := context.WithCancel(c.Request.Context())
	stream, err := s.appManager.LogsStream(ctx, instanceID, tail, timestamps)
	if err != nil {
		cancel()
		if handleAppManagerError(c, err, "stream logs") {
			return
		}
		if strings.Contains(err.Error(), "not found") {
			writeGinError(c, http.StatusNotFound, err.Error())
			return
		}
		writeGinError(c, http.StatusInternalServerError, "Failed to stream logs: "+err.Error())
		return
	}
	defer func() { _ = stream.Close() }()

	conn, err := wsupgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		cancel()
		return
	}
	defer conn.Close()

	s.streamBytesToWebsocket(ctx, cancel, conn, stream)
}

var allowedSystemLogUnits = map[string]string{
	"piccolod":                     "piccolod.service",
	"piccolod.service":             "piccolod.service",
	"transactional-update":         "transactional-update.service",
	"transactional-update.service": "transactional-update.service",
}

func resolveSystemLogUnit(raw string) (string, bool) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return "", false
	}
	unit, ok := allowedSystemLogUnits[raw]
	return unit, ok
}

func (s *GinServer) handleGinSystemLogStream(c *gin.Context) {
	unitRaw := c.Query("unit")
	unit, ok := resolveSystemLogUnit(unitRaw)
	if !ok {
		writeGinError(c, http.StatusBadRequest, "Unsupported system log unit")
		return
	}
	tail := parseLogTail(c, 200)

	ctx, cancel := context.WithCancel(c.Request.Context())
	cmd := exec.CommandContext(ctx, "journalctl",
		"-u", unit,
		"-f",
		"-n", fmt.Sprintf("%d", tail),
		"--no-pager",
		"-o", "short-iso",
	)
	stream, err := startCommandStream(ctx, cmd, cancel)
	if err != nil {
		writeGinError(c, http.StatusInternalServerError, "Failed to start journald stream: "+err.Error())
		return
	}
	defer func() { _ = stream.Close() }()

	conn, err := wsupgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		cancel()
		return
	}
	defer conn.Close()

	s.streamBytesToWebsocket(ctx, cancel, conn, stream)
}

func (s *GinServer) streamBytesToWebsocket(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, stream io.ReadCloser) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				cancel()
				_ = stream.Close()
				return
			}
		}
	}()

	buf := make([]byte, 4096)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			encoded := base64.StdEncoding.EncodeToString(buf[:n])
			if err2 := conn.WriteJSON(terminalMessage{Type: "stdout", Data: encoded}); err2 != nil {
				cancel()
				_ = stream.Close()
				break
			}
		}
		if err != nil {
			cancel()
			_ = stream.Close()
			break
		}
	}
	_ = conn.Close()
	<-done
}
