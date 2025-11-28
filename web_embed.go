package webassets

import (
	"embed"
)

// Embedded UI assets. The directory name includes all files recursively.
//
//go:embed all:web
var FS embed.FS
