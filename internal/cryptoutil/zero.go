package cryptoutil

import "runtime"

// SecureZero overwrites a byte slice with zeroes. The runtime.KeepAlive
// call prevents the compiler from optimising the wipe away.
func SecureZero(b []byte) {
	for i := range b {
		b[i] = 0
	}
	runtime.KeepAlive(b)
}
