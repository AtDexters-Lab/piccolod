package timezone

import (
	"context"
	"errors"
	"testing"
)

type fakeRunner struct {
	calls   [][]string
	runErr  error
	runResp []byte
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.runErr
}
func (f *fakeRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.runResp, f.runErr
}
func (f *fakeRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.runErr
}

func TestSet_RejectsInvalidZone(t *testing.T) {
	r := &fakeRunner{}
	m := NewManager(r)
	err := m.Set(context.Background(), "Mars/Olympus_Mons")
	if !errors.Is(err, ErrInvalidTimezone) {
		t.Fatalf("err = %v; want ErrInvalidTimezone", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("timedatectl invoked for invalid zone: %v", r.calls)
	}
}

func TestSet_HappyPath(t *testing.T) {
	r := &fakeRunner{}
	m := NewManager(r)
	if err := m.Set(context.Background(), "America/New_York"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("call count = %d; want 1", len(r.calls))
	}
	want := []string{"timedatectl", "set-timezone", "America/New_York"}
	for i, w := range want {
		if r.calls[0][i] != w {
			t.Errorf("call[%d] = %q; want %q", i, r.calls[0][i], w)
		}
	}
}

func TestSet_PropagatesRunnerError(t *testing.T) {
	r := &fakeRunner{runErr: errors.New("polkit denied")}
	m := NewManager(r)
	err := m.Set(context.Background(), "Europe/Berlin")
	if err == nil {
		t.Fatalf("expected runner error to propagate")
	}
}
