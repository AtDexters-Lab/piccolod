package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"

	"piccolod/internal/identity"
	"piccolod/internal/remote"
)

// namekDomainState tracks the namek-side state for an alias domain.
// Ephemeral — rebuilt from namek on startup via ListDomains().
type namekDomainState struct {
	DomainID    string `json:"domain_id"`
	CNAMETarget string `json:"cname_target"`
	Status      string `json:"status"` // "pending", "verified", "assigned"
}

// normalizeHostname lowercases and strips trailing dot.
func normalizeHostname(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// namekDomainActive returns true if namek domain orchestration is available and ready.
// Requires both the client to be initialized and the device to be enrolled+active.
func (s *GinServer) namekDomainActive() bool {
	if s.namekDomainClient == nil || s.identityService == nil {
		return false
	}
	svc := s.identityService
	return svc.IsEnrolled() && svc.IsEnabled() && !svc.IsSuspended()
}

// getNamekDomainState returns the namek state for a hostname, or nil if none.
func (s *GinServer) getNamekDomainState(hostname string) *namekDomainState {
	h := normalizeHostname(hostname)
	s.namekMu.RLock()
	defer s.namekMu.RUnlock()
	if s.namekDomains == nil {
		return nil
	}
	st, ok := s.namekDomains[h]
	if !ok {
		return nil
	}
	cp := *st
	return &cp
}

// allNamekDomainStates returns a snapshot of all namek domain states.
func (s *GinServer) allNamekDomainStates() map[string]*namekDomainState {
	s.namekMu.RLock()
	defer s.namekMu.RUnlock()
	if s.namekDomains == nil {
		return nil
	}
	out := make(map[string]*namekDomainState, len(s.namekDomains))
	for k, v := range s.namekDomains {
		cp := *v
		out[k] = &cp
	}
	return out
}

// namekRegisterAlias registers an alias on namek after it's been added to remote.Config.
// Returns the CNAME target for the UI to display. No-op if namek is not active.
func (s *GinServer) namekRegisterAlias(ctx context.Context, hostname string) (*namekDomainState, error) {
	dc := s.namekDomainClient
	if dc == nil {
		return nil, nil
	}

	h := normalizeHostname(hostname)

	info, err := dc.Register(ctx, h)
	if err != nil {
		// 409 = already registered — try to find existing entry.
		// namekclient errors are formatted as "server error <status>: <body>".
		if strings.Contains(err.Error(), "server error 409") {
			return s.namekRecoverExisting(ctx, dc, h)
		}
		return nil, err
	}

	state := s.ensureAssigned(ctx, dc, info, h)
	s.setNamekDomain(h, state)
	s.logNamekEvent("info", "Alias %s registered on namek (cname_target=%s, status=%s)", h, state.CNAMETarget, state.Status)
	return state, nil
}

// namekRecoverExisting handles 409 by listing domains and finding the existing entry.
func (s *GinServer) namekRecoverExisting(ctx context.Context, dc *identity.NamekDomainClient, h string) (*namekDomainState, error) {
	domains, err := dc.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range domains {
		if normalizeHostname(d.Domain) == h {
			state := s.ensureAssigned(ctx, dc, &d, h)
			s.setNamekDomain(h, state)
			return state, nil
		}
	}
	return nil, nil
}

// namekVerifyAlias triggers CNAME verification on namek for a pending alias.
func (s *GinServer) namekVerifyAlias(ctx context.Context, hostname string) (*namekDomainState, error) {
	h := normalizeHostname(hostname)

	s.namekMu.RLock()
	dc := s.namekDomainClient
	var existing *namekDomainState
	if s.namekDomains != nil {
		existing = s.namekDomains[h]
	}
	s.namekMu.RUnlock()

	if dc == nil || existing == nil {
		return nil, nil
	}

	info, err := dc.Verify(ctx, existing.DomainID)
	if err != nil {
		return nil, err
	}

	state := s.ensureAssigned(ctx, dc, info, h)
	s.setNamekDomain(h, state)
	s.logNamekEvent("info", "Alias %s verified on namek (status=%s)", h, state.Status)

	// Force-retry cert issuance now that domain is verified+assigned (resets ACME backoff)
	if state.Status == "assigned" {
		s.forceRetryAliasCerts(h)
	}

	return state, nil
}

// ensureAssigned converts DomainInfo to state and assigns to self if verified but not yet assigned.
// This is the single path for the verify+assign pattern used by register, recover, and verify flows.
func (s *GinServer) ensureAssigned(ctx context.Context, dc *identity.NamekDomainClient, info *namekclient.DomainInfo, hostname string) *namekDomainState {
	state := s.domainInfoToState(info)
	if info.Status == "verified" && state.Status != "assigned" {
		if _, assignErr := dc.Assign(ctx, info.ID); assignErr != nil {
			log.Printf("WARN: server: namek assign domain %s: %v", hostname, assignErr)
		} else {
			state.Status = "assigned"
		}
	}
	return state
}

// namekDeleteAlias removes an alias from namek (delete domain).
// No-op if namek is not active or alias has no namek state.
func (s *GinServer) namekDeleteAlias(ctx context.Context, hostname string) error {
	h := normalizeHostname(hostname)

	s.namekMu.RLock()
	dc := s.namekDomainClient
	var existing *namekDomainState
	if s.namekDomains != nil {
		existing = s.namekDomains[h]
	}
	s.namekMu.RUnlock()

	if dc == nil || existing == nil {
		return nil
	}

	err := dc.Delete(ctx, existing.DomainID)

	// Remove from map regardless (will be cleaned up by reconciliation if delete failed)
	s.namekMu.Lock()
	delete(s.namekDomains, h)
	s.namekMu.Unlock()

	if err != nil {
		log.Printf("WARN: server: namek delete domain %s (id=%s): %v", h, existing.DomainID, err)
		s.logNamekEvent("warn", "Failed to delete alias %s from namek: %v", h, err)
		return err
	}

	s.logNamekEvent("info", "Alias %s deleted from namek", h)
	return nil
}

// rebuildNamekDomains rebuilds the namekDomains map from namek's ListDomains API
// and reconciles against local aliases. Called in a background goroutine from applyNamekState().
func (s *GinServer) rebuildNamekDomains(ctx context.Context) {
	dc := s.namekDomainClient
	if dc == nil {
		return
	}

	domains, err := dc.List(ctx)
	if err != nil {
		log.Printf("WARN: server: namek list domains for rebuild: %v", err)
		return
	}

	newMap := make(map[string]*namekDomainState, len(domains))
	for i := range domains {
		h := normalizeHostname(domains[i].Domain)
		if h == "" {
			continue
		}
		newMap[h] = s.domainInfoToState(&domains[i])
	}

	s.namekMu.Lock()
	s.namekDomains = newMap
	s.namekMu.Unlock()

	log.Printf("INFO: server: rebuilt namek domain map (%d domains)", len(newMap))

	// Reconcile synchronously — we're already in a background goroutine.
	s.reconcileNamekDomains(ctx, domains)
}

// reconcileNamekDomains reconciles local aliases against namek domain state.
// - Orphaned namek domains (in namek but not in local config): delete from namek
// - Unregistered local aliases (in local config but not in namek): register on namek
func (s *GinServer) reconcileNamekDomains(ctx context.Context, serverDomains []namekclient.DomainInfo) {
	rm := s.remoteManager
	if rm == nil {
		return
	}

	dc := s.namekDomainClient
	if dc == nil {
		return
	}

	localAliases := rm.ListAliases()
	localSet := make(map[string]struct{}, len(localAliases))
	for _, a := range localAliases {
		localSet[normalizeHostname(a.Hostname)] = struct{}{}
	}

	serverSet := make(map[string]struct{}, len(serverDomains))
	for _, d := range serverDomains {
		serverSet[normalizeHostname(d.Domain)] = struct{}{}
	}

	// Orphaned namek domains: in namek but not in local config
	for _, d := range serverDomains {
		if ctx.Err() != nil {
			return
		}
		h := normalizeHostname(d.Domain)
		if _, ok := localSet[h]; !ok {
			log.Printf("INFO: server: reconcile: deleting orphaned namek domain %s (id=%s)", h, d.ID)
			if err := dc.Delete(ctx, d.ID); err != nil {
				log.Printf("WARN: server: reconcile: delete orphaned domain %s: %v", h, err)
			} else {
				s.namekMu.Lock()
				delete(s.namekDomains, h)
				s.namekMu.Unlock()
			}
		}
	}

	// Unregistered local aliases: in local config but not in namek
	for _, a := range localAliases {
		if ctx.Err() != nil {
			return
		}
		h := normalizeHostname(a.Hostname)
		if _, ok := serverSet[h]; !ok {
			log.Printf("INFO: server: reconcile: registering unregistered alias %s on namek", h)
			if _, err := s.namekRegisterAlias(ctx, h); err != nil {
				log.Printf("WARN: server: reconcile: register alias %s: %v", h, err)
			}
		}
	}
}

// domainInfoToState converts a namekclient.DomainInfo to a namekDomainState.
func (s *GinServer) domainInfoToState(info *namekclient.DomainInfo) *namekDomainState {
	status := info.Status
	if status == "verified" && s.isDeviceAssigned(info.AssignedDevices) {
		status = "assigned"
	}
	return &namekDomainState{
		DomainID:    info.ID,
		CNAMETarget: info.CNAMETarget,
		Status:      status,
	}
}

// isDeviceAssigned checks if the current device is in the assigned devices list.
func (s *GinServer) isDeviceAssigned(assignedDevices []string) bool {
	if s.identityService == nil {
		return false
	}
	nc := s.identityService.NamekClient()
	if nc == nil {
		return false
	}
	deviceID := nc.DeviceID()
	if deviceID == "" {
		return false
	}
	for _, id := range assignedDevices {
		if id == deviceID {
			return true
		}
	}
	return false
}

// setNamekDomain stores a namek domain state entry under namekMu.
func (s *GinServer) setNamekDomain(hostname string, state *namekDomainState) {
	s.namekMu.Lock()
	if s.namekDomains == nil {
		s.namekDomains = make(map[string]*namekDomainState)
	}
	s.namekDomains[hostname] = state
	s.namekMu.Unlock()
}

// forceRetryAliasCerts force-retries alias cert issuance and per-app certs for the alias hostname.
func (s *GinServer) forceRetryAliasCerts(hostname string) {
	rm := s.remoteManager
	if rm == nil {
		return
	}

	// Force-retry the alias base cert
	rm.EnqueueCertIssuance(remote.CertIssuanceRequest{
		ID:         "alias:" + hostname,
		Domains:    []string{hostname},
		CommonName: hostname,
		Solver:     "http-01",
		Force:      true,
	})

	// Force-retry per-app certs
	s.queueAllEndpointCerts()
}

// logNamekEvent appends an event to the remote activity log.
func (s *GinServer) logNamekEvent(level, format string, args ...any) {
	rm := s.remoteManager
	if rm == nil {
		return
	}
	rm.AppendEvent(remote.Event{
		Timestamp: time.Now().UTC(),
		Level:     level,
		Source:    "namek",
		Message:   fmt.Sprintf(format, args...),
	})
}
