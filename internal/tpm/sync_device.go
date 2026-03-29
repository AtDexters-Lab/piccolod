package tpm

import (
	"errors"
	"sync"

	"github.com/AtDexters-Lab/namek-server/pkg/tpmdevice"
)

var errDeviceClosed = errors.New("tpm: device closed")

// syncDevice wraps a tpmdevice.Device with a mutex to serialize all TPM
// operations. The go-tpm library's EmulatorReadWriteCloser has no internal
// locking — concurrent callers race on the connection, causing nil-pointer
// panics when one goroutine's Read sets conn=nil while another reads from it.
type syncDevice struct {
	mu     sync.Mutex
	inner  tpmdevice.Device
	closed bool
}

func newSyncDevice(inner tpmdevice.Device) *syncDevice {
	return &syncDevice{inner: inner}
}

func (d *syncDevice) EKCertDER() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, errDeviceClosed
	}
	return d.inner.EKCertDER()
}

func (d *syncDevice) EKPublicDER() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, errDeviceClosed
	}
	return d.inner.EKPublicDER()
}

func (d *syncDevice) AKPublic() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, errDeviceClosed
	}
	return d.inner.AKPublic()
}

func (d *syncDevice) ActivateCredential(encCredential []byte) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, errDeviceClosed
	}
	return d.inner.ActivateCredential(encCredential)
}

func (d *syncDevice) Quote(nonce []byte) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return "", errDeviceClosed
	}
	return d.inner.Quote(nonce)
}

func (d *syncDevice) QuoteOverData(data []byte) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return "", errDeviceClosed
	}
	return d.inner.QuoteOverData(data)
}

func (d *syncDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errDeviceClosed
	}
	d.closed = true
	return d.inner.Close()
}
