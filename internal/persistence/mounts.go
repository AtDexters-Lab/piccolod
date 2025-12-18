package persistence

// IsMountPoint reports whether the given path is a mount point in the current mount namespace.
//
// Note: This is Linux-specific and is primarily intended to prevent writing to an unmounted
// plaintext directory where an encrypted volume is expected to be attached.
func IsMountPoint(path string) (bool, error) {
	return isMountPoint(path)
}
