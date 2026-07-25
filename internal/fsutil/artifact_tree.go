package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateArtifactTree permits only directories, regular files, and symlinks.
// Callers use it at the artifact-attachment boundary so the same golden content
// may still serve as an ordinary container rootfs when that is appropriate.
func ValidateArtifactTree(root string) error {
	cleanRoot := filepath.Clean(root)
	return filepath.WalkDir(cleanRoot, func(candidate string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(cleanRoot, filepath.Clean(candidate))
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("artifact entry escapes root")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		if mode.IsDir() || mode.IsRegular() || mode&os.ModeSymlink != 0 {
			return nil
		}
		return fmt.Errorf("artifact entry %s has unsupported file type %s", candidate, mode.Type())
	})
}
