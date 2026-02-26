package blockdev

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fakeDevice implements BlockDevice for testing.
type fakeDevice struct {
	name      string
	path      string
	sizeBytes int64
	openErr   error
	closeErr  error

	mu       sync.Mutex
	opened   bool
	closed   bool
	openSeq  int // order in which Open was called (set by test)
	closeSeq int
}

func (d *fakeDevice) Name() string     { return d.name }
func (d *fakeDevice) Path() string     { return d.path }
func (d *fakeDevice) SizeBytes() int64 { return d.sizeBytes }

func (d *fakeDevice) Open(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.openErr != nil {
		return d.openErr
	}
	d.opened = true
	return nil
}

func (d *fakeDevice) Close(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closeErr != nil {
		return d.closeErr
	}
	d.closed = true
	return nil
}

func (d *fakeDevice) isOpened() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.opened
}

func (d *fakeDevice) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

func TestNewDeviceStack_RequiresLayers(t *testing.T) {
	_, err := NewDeviceStack("vol-test")
	if err == nil {
		t.Fatal("expected error for empty layers")
	}
}

func TestDeviceStack_TopAndBottom(t *testing.T) {
	d1 := &fakeDevice{name: "bottom", path: "/dev/lv"}
	d2 := &fakeDevice{name: "middle", path: "/dev/nbd0"}
	d3 := &fakeDevice{name: "top", path: "/dev/drbd0"}

	stack, err := NewDeviceStack("vol-test", d1, d2, d3)
	if err != nil {
		t.Fatal(err)
	}

	if stack.Bottom().Name() != "bottom" {
		t.Errorf("expected bottom, got %s", stack.Bottom().Name())
	}
	if stack.Top().Name() != "top" {
		t.Errorf("expected top, got %s", stack.Top().Name())
	}
	if stack.VolumeID() != "vol-test" {
		t.Errorf("expected vol-test, got %s", stack.VolumeID())
	}
}

func TestDeviceStack_Open_BottomUp(t *testing.T) {
	var mu sync.Mutex
	var seq int

	d1 := &fakeDevice{name: "thinlv", path: "/dev/vg/lv"}
	d2 := &fakeDevice{name: "nbd", path: "/dev/nbd0"}
	d3 := &fakeDevice{name: "drbd", path: "/dev/drbd0"}

	// Track open order via wrapping.
	devices := []*fakeDevice{d1, d2, d3}
	wrappers := make([]BlockDevice, 3)
	for i, d := range devices {
		wrappers[i] = &orderTrackingDevice{
			fakeDevice: d,
			mu:         &mu,
			seq:        &seq,
		}
	}

	stack, err := NewDeviceStack("vol-test", wrappers...)
	if err != nil {
		t.Fatal(err)
	}

	if err := stack.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Verify all opened.
	for _, w := range wrappers {
		otd := w.(*orderTrackingDevice)
		if otd.openOrder == 0 {
			t.Errorf("device %s was not opened", otd.Name())
		}
	}

	// Verify bottom-up order.
	w0 := wrappers[0].(*orderTrackingDevice)
	w1 := wrappers[1].(*orderTrackingDevice)
	w2 := wrappers[2].(*orderTrackingDevice)
	if w0.openOrder >= w1.openOrder || w1.openOrder >= w2.openOrder {
		t.Errorf("expected bottom-up open order, got %d, %d, %d",
			w0.openOrder, w1.openOrder, w2.openOrder)
	}
}

func TestDeviceStack_Close_TopDown(t *testing.T) {
	var mu sync.Mutex
	var seq int

	wrappers := make([]BlockDevice, 3)
	for i, name := range []string{"thinlv", "nbd", "drbd"} {
		wrappers[i] = &orderTrackingDevice{
			fakeDevice: &fakeDevice{name: name, path: fmt.Sprintf("/dev/%s", name)},
			mu:         &mu,
			seq:        &seq,
		}
	}

	stack, _ := NewDeviceStack("vol-test", wrappers...)
	_ = stack.Open(context.Background())

	// Reset sequence counter for close tracking.
	mu.Lock()
	seq = 0
	mu.Unlock()

	if err := stack.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify top-down close order.
	w0 := wrappers[0].(*orderTrackingDevice)
	w1 := wrappers[1].(*orderTrackingDevice)
	w2 := wrappers[2].(*orderTrackingDevice)
	if w2.closeOrder >= w1.closeOrder || w1.closeOrder >= w0.closeOrder {
		t.Errorf("expected top-down close order, got drbd=%d, nbd=%d, thinlv=%d",
			w2.closeOrder, w1.closeOrder, w0.closeOrder)
	}
}

func TestDeviceStack_Open_Rollback_OnFailure(t *testing.T) {
	d1 := &fakeDevice{name: "thinlv", path: "/dev/lv"}
	d2 := &fakeDevice{name: "nbd", path: "/dev/nbd0", openErr: fmt.Errorf("nbd connect failed")}
	d3 := &fakeDevice{name: "drbd", path: "/dev/drbd0"}

	stack, _ := NewDeviceStack("vol-test", d1, d2, d3)

	err := stack.Open(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "nbd connect failed") {
		t.Errorf("unexpected error: %v", err)
	}

	// d1 should have been rolled back (closed).
	if !d1.isClosed() {
		t.Error("expected thinlv to be closed after rollback")
	}
	// d3 should never have been opened.
	if d3.isOpened() {
		t.Error("expected drbd to NOT be opened")
	}
}

func TestDeviceStack_Close_ContinuesOnError(t *testing.T) {
	d1 := &fakeDevice{name: "thinlv", path: "/dev/lv"}
	d2 := &fakeDevice{name: "nbd", path: "/dev/nbd0", closeErr: fmt.Errorf("nbd disconnect failed")}
	d3 := &fakeDevice{name: "drbd", path: "/dev/drbd0"}

	stack, _ := NewDeviceStack("vol-test", d1, d2, d3)
	_ = stack.Open(context.Background())

	err := stack.Close(context.Background())
	if err == nil {
		t.Fatal("expected error from nbd close")
	}
	if !strings.Contains(err.Error(), "nbd disconnect failed") {
		t.Errorf("unexpected error: %v", err)
	}

	// d3 (drbd) should still be closed despite nbd error.
	if !d3.isClosed() {
		t.Error("expected drbd to be closed despite nbd error")
	}
	// d1 (thinlv) should also be closed.
	if !d1.isClosed() {
		t.Error("expected thinlv to be closed despite nbd error")
	}
}

func TestDeviceStack_Layers(t *testing.T) {
	d1 := &fakeDevice{name: "a"}
	d2 := &fakeDevice{name: "b"}
	stack, _ := NewDeviceStack("vol", d1, d2)

	layers := stack.Layers()
	if len(layers) != 2 {
		t.Fatalf("expected 2 layers, got %d", len(layers))
	}
	if layers[0].Name() != "a" || layers[1].Name() != "b" {
		t.Errorf("unexpected layer names: %s, %s", layers[0].Name(), layers[1].Name())
	}

	// Verify returned slice is a copy (modifying it doesn't affect stack).
	layers[0] = nil
	if stack.Layers()[0] == nil {
		t.Error("Layers() should return a copy")
	}
}

func TestDeviceStack_SingleLayer(t *testing.T) {
	d := &fakeDevice{name: "only", path: "/dev/only", sizeBytes: 1024}
	stack, err := NewDeviceStack("vol-single", d)
	if err != nil {
		t.Fatal(err)
	}

	if stack.Top() != stack.Bottom() {
		t.Error("single layer: Top and Bottom should be the same")
	}

	if err := stack.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stack.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// orderTrackingDevice wraps a fakeDevice and records the order of Open/Close calls.
type orderTrackingDevice struct {
	*fakeDevice
	mu         *sync.Mutex
	seq        *int
	openOrder  int
	closeOrder int
}

func (d *orderTrackingDevice) Open(ctx context.Context) error {
	if err := d.fakeDevice.Open(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	*d.seq++
	d.openOrder = *d.seq
	d.mu.Unlock()
	return nil
}

func (d *orderTrackingDevice) Close(ctx context.Context) error {
	if err := d.fakeDevice.Close(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	*d.seq++
	d.closeOrder = *d.seq
	d.mu.Unlock()
	return nil
}
