package server

import (
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"piccolod/internal/terminal"

	"github.com/gin-gonic/gin"
)

// --- Host terminal session handlers ---

func (s *GinServer) handleCreateHostTerminalSession(c *gin.Context) {
	sess, err := s.terminalManager.CreateContext(c.Request.Context(), terminal.SessionKindHost, "", func() (*exec.Cmd, error) {
		cmd := exec.Command(getShell())
		cmd.WaitDelay = 5 * time.Second
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		return cmd, nil
	})
	if err != nil {
		log.Printf("terminal: create host session failed: %v", err)
		if writeTaskPressureError(c, err) {
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": sess.ID})
}

func (s *GinServer) handleListHostTerminalSessions(c *gin.Context) {
	list := s.terminalManager.List(terminal.SessionKindHost, "")
	c.JSON(http.StatusOK, list)
}

func (s *GinServer) handleDeleteHostTerminalSession(c *gin.Context) {
	id := c.Param("id")
	if !s.terminalManager.Delete(id) {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *GinServer) handleAttachHostTerminalSession(c *gin.Context) {
	id := c.Param("id")
	s.attachTerminalSession(c, id)
}

// --- Container (workspace) terminal session handlers ---

func (s *GinServer) handleCreateWorkspaceTerminalSession(c *gin.Context) {
	appName := c.Param("name")
	if appName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "app name required"})
		return
	}
	service := c.Query("service")

	// Use request context for the container/filesystem lookups, but the
	// resulting *exec.Cmd will be wrapped with a fresh context by NewSession
	// so the PTY outlives this HTTP request.
	reqCtx := c.Request.Context()
	sess, err := s.terminalManager.CreateContext(c.Request.Context(), terminal.SessionKindContainer, appName, func() (*exec.Cmd, error) {
		return s.appManager.ExecShellCmdForService(reqCtx, appName, service)
	})
	if err != nil {
		log.Printf("terminal: create workspace session failed for %s: %v", appName, err)
		if writeTaskPressureError(c, err) {
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": sess.ID})
}

func (s *GinServer) handleListWorkspaceTerminalSessions(c *gin.Context) {
	appName := c.Param("name")
	list := s.terminalManager.List(terminal.SessionKindContainer, appName)
	c.JSON(http.StatusOK, list)
}

func (s *GinServer) handleDeleteWorkspaceTerminalSession(c *gin.Context) {
	appName := c.Param("name")
	id := c.Param("id")

	sess, ok := s.terminalManager.Get(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	// Validate the session belongs to this app
	if sess.AppName != appName {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	s.terminalManager.Delete(id)
	c.Status(http.StatusNoContent)
}

func (s *GinServer) handleAttachWorkspaceTerminalSession(c *gin.Context) {
	appName := c.Param("name")
	id := c.Param("id")

	sess, ok := s.terminalManager.Get(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if sess.AppName != appName {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	s.attachTerminalSession(c, id)
}

// --- Shared attach logic ---

func (s *GinServer) attachTerminalSession(c *gin.Context, id string) {
	sess, ok := s.terminalManager.Get(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if sess.IsClosed() {
		c.JSON(http.StatusGone, gin.H{"error": "session ended"})
		return
	}

	conn, err := wsupgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("terminal: WS upgrade failed for session %s: %v", id, err)
		return
	}
	defer conn.Close()

	client := terminal.NewClient(conn)
	if err := sess.Attach(client); err != nil {
		sendError(conn, "Failed to attach: "+err.Error())
		return
	}

	client.Run(sess) // blocks until WS closes or session ends
}
