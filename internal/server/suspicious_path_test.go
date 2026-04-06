package server

import "testing"

func TestIsSuspiciousPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// Dotfile probes — should block
		{"/.env", true},
		{"/.env.production", true},
		{"/.env.staging", true},
		{"/.git/config", true},
		{"/.git/HEAD", true},
		{"/.svn/entries", true},
		{"/.htaccess", true},
		{"/.htpasswd", true},
		{"/.DS_Store", true},
		{"/.aws/credentials", true},
		{"/.docker/config.json", true},
		{"/.ssh/id_rsa", true},
		{"/.npmrc", true},

		// CMS probes — should block
		{"/wp-admin/", true},
		{"/wp-content/debug.log", true},
		{"/wp-includes/version.php", true},
		{"/wp-login.php", true},
		{"/wp-config.php", true},
		{"/wp-config.php.bak", true},
		{"/backup/wp-config.old", true},

		// Server debug — should block
		{"/server-status", true},
		{"/server-info", true},
		{"/phpinfo.php", true},
		{"/actuator/health", true},

		// Case insensitive — should block
		{"/.ENV", true},
		{"/.Git/config", true},
		{"/WP-ADMIN/", true},

		// Legitimate SPA routes — should NOT block
		{"/", false},
		{"/setup", false},
		{"/apps", false},
		{"/settings/network", false},
		{"/apps/my-app/settings", false},

		// API/OAuth — not suspicious (handled separately)
		{"/api/v1/health", false},
		{"/oauth/callback", false},

		// .well-known — NOT suspicious (handled by explicit Gin routes, never reaches NoRoute)
		{"/.well-known/acme-challenge/token123", false},
		{"/.well-known/openid-configuration", false},

		// Static assets — should NOT block
		{"/flutter.js", false},
		{"/main.dart.js", false},
		{"/assets/fonts/material.woff2", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isSuspiciousPath(tt.path); got != tt.want {
				t.Errorf("isSuspiciousPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
