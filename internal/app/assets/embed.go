// Package assets provides embedded assets for the app package.
package assets

import _ "embed"

// BootSh contains the universal workspace boot wrapper script.
// This script is bind-mounted into workspace containers to provide
// the "VPS Experience" - keeping containers alive for terminal access.
//
//go:embed boot.sh
var BootSh []byte

// PiccoloStartup contains the piccolo-startup helper command.
// This script helps users discover and manage their workspace startup hook.
//
//go:embed piccolo-startup
var PiccoloStartup []byte
