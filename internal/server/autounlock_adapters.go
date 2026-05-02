package server

import (
	"context"
	"log"
)

// osUpdateManagerAdapter bridges the autounlock.UpdateManager interface to the
// existing GinServer.updateManager surface. The osUpdateManager already has
// Status() returning a RequiresReboot bool — adapter exposes that as
// HasStagedUpdate so autounlock doesn't depend on the update package types.
//
// `inner` is captured by closure so the value is read at call time —
// updateManager is set during NewGinServer AFTER autounlock construction in
// the auto-init path, so an early-bound pointer would observe nil.
type osUpdateManagerAdapter struct {
	inner func() osUpdateManager
}

func (a *osUpdateManagerAdapter) HasStagedUpdate(ctx context.Context) bool {
	m := a.inner()
	if m == nil {
		return false
	}
	st, err := m.Status(ctx)
	if err != nil {
		log.Printf("WARN: autounlock adapter: update Status: %v", err)
		return false
	}
	return st.RequiresReboot
}

func (a *osUpdateManagerAdapter) Reboot(ctx context.Context) error {
	m := a.inner()
	if m == nil {
		return nil
	}
	return m.Reboot(ctx)
}
