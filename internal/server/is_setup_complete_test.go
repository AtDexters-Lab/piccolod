package server

import (
	"context"
	"errors"
	"testing"

	authpkg "piccolod/internal/auth"
	"piccolod/internal/persistence"
)

// TestIsSetupComplete_ContractMatrix exercises the three-way contract that
// Cluster B made explicit: (true, nil), (false, nil), (false, ErrLocked),
// (false, transientErr). Critically, ErrLocked must propagate — the
// pre-Cluster-B contract collapsed it into (false, nil), which is the
// lossy path that originally enabled the boot-handler race-window bug
// (already-provisioned device transiently routed to setup wizard while
// persistence was still loading).
//
// The function checks userManager first, then falls back to authManager.
// We exercise the authManager fallback path because UserManager is a
// concrete struct that's harder to stub in isolation; the fallback's
// behavior is the same shape of contract and covers the propagation
// property.
func TestIsSetupComplete_ContractMatrix(t *testing.T) {
	cases := []struct {
		name         string
		repo         *fakeAuthRepo
		wantComplete bool
		wantErr      error // matched via errors.Is when non-nil
	}{
		{
			name:         "Initialized → complete",
			repo:         &fakeAuthRepo{initialized: true},
			wantComplete: true,
			wantErr:      nil,
		},
		{
			name:         "NotInitialized → incomplete",
			repo:         &fakeAuthRepo{initialized: false},
			wantComplete: false,
			wantErr:      nil,
		},
		{
			name:         "ErrLocked propagates (was the lossy path)",
			repo:         &fakeAuthRepo{loadErr: persistence.ErrLocked},
			wantComplete: false,
			wantErr:      persistence.ErrLocked,
		},
		{
			name:         "Transient error propagates",
			repo:         &fakeAuthRepo{loadErr: errors.New("transient db failure")},
			wantComplete: false,
			wantErr:      errors.New("transient db failure"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storage := newPersistenceAuthStorage(tc.repo)
			am, err := authpkg.NewManagerWithStorage(storage)
			if err != nil {
				t.Fatalf("NewManagerWithStorage: %v", err)
			}
			srv := &GinServer{authManager: am} // userManager intentionally nil → falls through
			complete, err := srv.isSetupComplete(context.Background())
			if complete != tc.wantComplete {
				t.Errorf("complete = %v, want %v", complete, tc.wantComplete)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Errorf("err = nil, want %v", tc.wantErr)
				return
			}
			if errors.Is(tc.wantErr, persistence.ErrLocked) {
				if !errors.Is(err, persistence.ErrLocked) {
					t.Errorf("err = %v, want errors.Is(ErrLocked)", err)
				}
				return
			}
			// Non-typed transient — match on error message since fake
			// constructs a fresh errors.New each time.
			if err.Error() != tc.wantErr.Error() {
				t.Errorf("err = %q, want %q", err.Error(), tc.wantErr.Error())
			}
		})
	}
}

// TestIsSetupComplete_NilManagers asserts the ground-state contract —
// no userManager, no authManager → (false, nil). This is the path
// hit by the test fixture before crypto setup runs.
func TestIsSetupComplete_NilManagers(t *testing.T) {
	srv := &GinServer{}
	complete, err := srv.isSetupComplete(context.Background())
	if complete || err != nil {
		t.Errorf("expected (false, nil), got (%v, %v)", complete, err)
	}
}
