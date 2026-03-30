package stun

import (
	"context"
	"log"
	"sync"
	"time"
)

const (
	defaultRefreshInterval = 5 * time.Minute
	defaultQueryTimeout    = 3 * time.Second
)

// Default public STUN servers used as fallback when no relay-provided servers are available.
var defaultSTUNServers = []string{
	"stun.l.google.com:19302",
	"stun.cloudflare.com:3478",
}

// Service discovers the device's public IP via STUN.
// Implements supervisor.Component (Name/Start/Stop).
type Service struct {
	mu              sync.RWMutex
	publicIP        string
	servers         []string
	refreshInterval time.Duration
	queryTimeout    time.Duration
	stopCh          chan struct{}
	stopped         chan struct{}
	stopOnce        sync.Once
	started         bool
}

// Option configures the STUN service.
type Option func(*Service)

// WithRefreshInterval sets the periodic refresh interval.
func WithRefreshInterval(d time.Duration) Option {
	return func(s *Service) { s.refreshInterval = d }
}

// New creates a new STUN service with the given options.
func New(opts ...Option) *Service {
	s := &Service{
		servers:         append([]string(nil), defaultSTUNServers...),
		refreshInterval: defaultRefreshInterval,
		queryTimeout:    defaultQueryTimeout,
		stopCh:          make(chan struct{}),
		stopped:         make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *Service) Name() string { return "stun" }

// Start launches the background refresh loop which performs the initial STUN
// query and then refreshes periodically. Non-blocking — does not delay supervisor
// startup. STUN unavailability degrades gracefully (remote setup falls back to LAN).
func (s *Service) Start(_ context.Context) error {
	s.started = true
	go s.refreshLoop()
	return nil
}

// Stop signals the refresh loop to exit and waits for it.
// Safe to call even if Start was never called (supervisor rollback).
func (s *Service) Stop(_ context.Context) error {
	s.stopOnce.Do(func() { close(s.stopCh) })
	if s.started {
		<-s.stopped
	}
	return nil
}

// PublicIP returns the last known public IP. Empty if STUN failed or hasn't run.
func (s *Service) PublicIP() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.publicIP
}

// Refresh performs a synchronous on-demand STUN query and returns the result.
// Use for security-sensitive decisions where a stale cached value is unacceptable.
func (s *Service) Refresh() string {
	s.refresh()
	return s.PublicIP()
}

// SetServers updates the STUN server list. Called by the identity service when
// NexusEndpoints change (relay hosts double as STUN servers on port 3478).
func (s *Service) SetServers(addrs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(addrs) == 0 {
		s.servers = append([]string(nil), defaultSTUNServers...)
		return
	}
	// Relay-provided servers first, then public fallbacks.
	servers := make([]string, 0, len(addrs)+len(defaultSTUNServers))
	servers = append(servers, addrs...)
	servers = append(servers, defaultSTUNServers...)
	s.servers = servers
}


func (s *Service) refresh() {
	s.mu.RLock()
	servers := append([]string(nil), s.servers...)
	timeout := s.queryTimeout
	s.mu.RUnlock()

	for _, server := range servers {
		ip, err := QueryPublicIP(server, timeout)
		if err != nil {
			log.Printf("WARN: stun: query %s failed: %v", server, err)
			continue
		}

		s.mu.Lock()
		old := s.publicIP
		s.publicIP = ip
		s.mu.Unlock()

		if old != ip && old != "" {
			log.Printf("INFO: stun: public IP changed: %s -> %s", old, ip)
		} else if old == "" {
			log.Printf("INFO: stun: discovered public IP: %s", ip)
		}
		return
	}
}

func (s *Service) refreshLoop() {
	defer close(s.stopped)

	// Initial query (best-effort, non-blocking to supervisor).
	s.refresh()

	ticker := time.NewTicker(s.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.refresh()
		}
	}
}
