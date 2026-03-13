package tpm

import (
	"log"
	"os"
	"path/filepath"
)

// RecoverAK deletes stale AK files from akStateDir and retries Open.
// Called when TPM ownership was cleared between reboots (AK blobs reference old SRK).
func RecoverAK(akStateDir, swtpmStateDir string) (*OpenResult, error) {
	log.Printf("INFO: tpm: recovering AK — deleting stale blobs from %s", akStateDir)
	for _, name := range []string{"ak_pub", "ak_priv"} {
		path := filepath.Join(akStateDir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("WARN: tpm: failed to remove %s: %v", path, err)
		}
	}
	return Open(akStateDir, swtpmStateDir)
}
