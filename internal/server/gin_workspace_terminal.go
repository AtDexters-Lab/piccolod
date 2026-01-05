package server

import (
	"log"

	"github.com/gin-gonic/gin"
)

// handleWorkspaceTerminal handles WebSocket connections for exec'ing into workspace containers.
// Route: GET /api/v1/apps/:name/terminal
func (s *GinServer) handleWorkspaceTerminal(c *gin.Context) {
	appName := c.Param("name")
	if appName == "" {
		c.JSON(400, gin.H{"error": "app name required"})
		return
	}
	service := c.Query("service")

	log.Printf("DEBUG: handleWorkspaceTerminal - Request for app %s from %s", appName, c.ClientIP())

	conn, err := wsupgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Workspace terminal upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	log.Printf("DEBUG: handleWorkspaceTerminal - WebSocket upgraded for app %s", appName)

	cmd, err := s.appManager.ExecShellCmdForService(c.Request.Context(), appName, service)
	if err != nil {
		sendError(conn, "Failed to exec into container: "+err.Error())
		return
	}

	session, err := NewPTYSession(conn, cmd)
	if err != nil {
		sendError(conn, "Failed to start PTY: "+err.Error())
		return
	}
	defer session.Close()

	session.Run()
}
