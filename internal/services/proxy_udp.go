package services

import (
	"errors"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	udpMaxFlows        = 4096             // max concurrent UDP flows
	udpMaxFlowsPerIP   = 64              // max flows per source IP
	udpReadBufSize     = 8192            // sufficient for DNS with EDNS0 and other UDP protocols
	udpFlowIdleTimeout = 60 * time.Second
	udpSweepInterval   = 30 * time.Second
)

// udpFlow tracks a single source-address-to-backend relay.
// lastActiveNano is accessed from multiple goroutines via atomic operations.
type udpFlow struct {
	srcAddr        *net.UDPAddr
	backendConn    *net.UDPConn
	lastActiveNano atomic.Int64
}

// udpProxyState manages the lifecycle of a single UDP proxy listener.
type udpProxyState struct {
	conn        *net.UDPConn
	backendAddr *net.UDPAddr // resolved once at startup
	stopCh      chan struct{}
	stopped     chan struct{}
	mu          sync.Mutex
	flows       map[string]*udpFlow // keyed by srcAddr.String()
	ipCounts    map[string]int      // keyed by srcAddr.IP.String()
}

func (s *udpProxyState) stop() {
	close(s.stopCh)
	_ = s.conn.Close()

	// Close all backend conns to unblock relayReturn goroutines, then clear
	// the map so concurrent sweepIdleFlows (if ticker fires) is a no-op.
	s.mu.Lock()
	for _, flow := range s.flows {
		_ = flow.backendConn.Close()
	}
	s.flows = nil
	s.ipCounts = nil
	s.mu.Unlock()

	<-s.stopped
}

func (p *ProxyManager) startUDPProxy(ep ServiceEndpoint) {
	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(ep.PublicPort))

	p.mu.Lock()
	if _, exists := p.udpListeners[ep.PublicPort]; exists {
		p.mu.Unlock()
		return
	}
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		log.Printf("WARN: UDP proxy resolve %s: %v", addr, err)
		p.mu.Unlock()
		return
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Printf("WARN: Failed to bind UDP listener on %s: %v", addr, err)
		p.mu.Unlock()
		return
	}

	// Resolve backend address once.
	backendStr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ep.HostBind))
	backendUDP, err := net.ResolveUDPAddr("udp", backendStr)
	if err != nil {
		log.Printf("WARN: UDP proxy resolve backend %s: %v", backendStr, err)
		_ = conn.Close()
		p.mu.Unlock()
		return
	}

	state := &udpProxyState{
		conn:        conn,
		backendAddr: backendUDP,
		stopCh:      make(chan struct{}),
		stopped:     make(chan struct{}),
		flows:       make(map[string]*udpFlow),
		ipCounts:    make(map[string]int),
	}
	p.udpListeners[ep.PublicPort] = state
	p.mu.Unlock()

	log.Printf("INFO: UDP proxy %s → %s (app=%s listener=%s)", addr, backendStr, ep.App, ep.Name)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(state.stopped)

		go state.sweepLoop()

		buf := make([]byte, udpReadBufSize)
		for {
			n, srcAddr, err := conn.ReadFromUDP(buf)
			if err != nil {
				select {
				case <-state.stopCh:
				default:
					if !isClosedConnErr(err) {
						log.Printf("WARN: UDP proxy read error: %v", err)
					}
				}
				return
			}

			flow := state.getOrCreateFlow(srcAddr)
			if flow == nil {
				continue // at capacity
			}
			flow.lastActiveNano.Store(time.Now().UnixNano())
			_, _ = flow.backendConn.Write(buf[:n])
		}
	}()
}

func isClosedConnErr(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

func (s *udpProxyState) getOrCreateFlow(srcAddr *net.UDPAddr) *udpFlow {
	key := srcAddr.String()
	s.mu.Lock()
	defer s.mu.Unlock()

	if flow, ok := s.flows[key]; ok {
		return flow
	}

	// Check per-IP limit first to avoid wasting an eviction slot.
	srcIP := srcAddr.IP.String()
	if s.ipCounts[srcIP] >= udpMaxFlowsPerIP {
		return nil
	}
	// Check global capacity — evict oldest if at limit.
	if len(s.flows) >= udpMaxFlows {
		s.evictOldestLocked()
	}

	backendConn, err := net.DialUDP("udp", nil, s.backendAddr)
	if err != nil {
		log.Printf("WARN: UDP proxy backend dial: %v", err)
		return nil
	}

	flow := &udpFlow{
		srcAddr:     srcAddr,
		backendConn: backendConn,
	}
	flow.lastActiveNano.Store(time.Now().UnixNano())
	s.flows[key] = flow
	s.ipCounts[srcIP]++

	go s.relayReturn(flow, key)
	return flow
}

func (s *udpProxyState) relayReturn(flow *udpFlow, key string) {
	buf := make([]byte, udpReadBufSize)
	for {
		_ = flow.backendConn.SetReadDeadline(time.Now().Add(udpFlowIdleTimeout))
		n, err := flow.backendConn.Read(buf)
		if err != nil {
			break
		}
		flow.lastActiveNano.Store(time.Now().UnixNano())
		_, _ = s.conn.WriteToUDP(buf[:n], flow.srcAddr)
	}
	s.removeFlow(key)
}

// removeFlowLocked removes a flow and cleans up ipCounts. Caller must hold s.mu.
func (s *udpProxyState) removeFlowLocked(key string) {
	flow, ok := s.flows[key]
	if !ok {
		return
	}
	_ = flow.backendConn.Close()
	srcIP := flow.srcAddr.IP.String()
	s.ipCounts[srcIP]--
	if s.ipCounts[srcIP] <= 0 {
		delete(s.ipCounts, srcIP)
	}
	delete(s.flows, key)
}

func (s *udpProxyState) removeFlow(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeFlowLocked(key)
}

func (s *udpProxyState) evictOldestLocked() {
	var oldestKey string
	var oldestNano int64
	for k, f := range s.flows {
		nano := f.lastActiveNano.Load()
		if oldestKey == "" || nano < oldestNano {
			oldestKey = k
			oldestNano = nano
		}
	}
	if oldestKey != "" {
		s.removeFlowLocked(oldestKey)
	}
}

func (s *udpProxyState) sweepLoop() {
	ticker := time.NewTicker(udpSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.sweepIdleFlows()
		}
	}
}

func (s *udpProxyState) sweepIdleFlows() {
	nowNano := time.Now().UnixNano()
	timeoutNano := udpFlowIdleTimeout.Nanoseconds()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, flow := range s.flows {
		if nowNano-flow.lastActiveNano.Load() > timeoutNano {
			s.removeFlowLocked(key)
		}
	}
}
