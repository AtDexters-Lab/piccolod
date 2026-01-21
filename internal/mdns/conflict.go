package mdns

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// sendConflictProbes sends probe queries on all interfaces to trigger responses
// from any devices already using our hostname. Actual conflict detection happens
// asynchronously via handleConflictDetection() in the responder goroutines,
// which sets the ConflictDetected flag when a conflicting response is received.
func (m *Manager) sendConflictProbes() {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	serviceName := m.currentServiceName()

	// Send probes on all active interfaces
	for _, state := range m.interfaces {
		if !state.Active {
			continue
		}

		// Probe both IPv4 and IPv6 if available
		if state.HasIPv4 && state.IPv4Conn != nil {
			m.sendConflictProbe(state, "IPv4", serviceName+".local.")
		}

		if state.HasIPv6 && state.IPv6Conn != nil {
			m.sendConflictProbe(state, "IPv6", serviceName+".local.")
		}
	}
}

// sendConflictProbe sends a probe query to the multicast group. This triggers
// any device already using the hostname to respond. The response is received
// by our responder goroutines and processed by handleConflictDetection().
func (m *Manager) sendConflictProbe(state *InterfaceState, stack, hostname string) {
	// Create probe query
	msg := &dns.Msg{}
	msg.SetQuestion(hostname, dns.TypeANY)
	msg.RecursionDesired = false

	data, err := msg.Pack()
	if err != nil {
		log.Printf("CONFLICT: Failed to pack probe query: %v", err)
		return
	}

	var multicastAddr *net.UDPAddr
	var conn *net.UDPConn

	if stack == "IPv4" {
		multicastAddr = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
		conn = state.IPv4Conn
	} else {
		multicastAddr = &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353}
		conn = state.IPv6Conn
	}

	if conn == nil {
		return
	}

	// Send probe query
	if _, err := conn.WriteToUDP(data, multicastAddr); err != nil {
		log.Printf("CONFLICT: Failed to send probe on %s-%s: %v", state.Interface.Name, stack, err)
		return
	}

	log.Printf("DEBUG: [%s-%s] Sent conflict probe for %s", state.Interface.Name, stack, hostname)

	// Allow time for responses to arrive and be processed by handleConflictDetection()
	time.Sleep(1000 * time.Millisecond)
}

// handleConflictDetection processes responses that might indicate conflicts
func (m *Manager) handleConflictDetection(msg *dns.Msg, clientAddr *net.UDPAddr) {
	// Check if this is a response to our hostname query
	serviceName := m.currentServiceName()
	for _, answer := range msg.Answer {
		if !strings.EqualFold(answer.Header().Name, serviceName+".local.") {
			continue
		}

		// Ignore responses from self
		if m.isSelfResponse(clientAddr.IP) {
			log.Printf("DEBUG: Ignoring self-response from %s", clientAddr.IP)
			continue
		}

		// Found a conflict - someone else is responding to our name
		hostKey := clientAddr.IP.String()

		m.conflictDetector.mutex.Lock()

		conflict, exists := m.conflictDetector.ConflictingSources[hostKey]
		if !exists {
			conflict = ConflictingHost{
				IP:         clientAddr.IP,
				FirstSeen:  time.Now(),
				QueryCount: 0,
			}
			log.Printf("CONFLICT: New conflicting host detected: %s for %s.local",
				clientAddr.IP, serviceName)
		}

		conflict.LastSeen = time.Now()
		conflict.QueryCount++
		m.conflictDetector.ConflictingSources[hostKey] = conflict

		shouldResolve := false

		if !m.conflictDetector.ConflictDetected {
			m.conflictDetector.ConflictDetected = true
			log.Printf("CONFLICT: Name conflict detected for %s.local!", serviceName)
			shouldResolve = true
		}

		m.conflictDetector.mutex.Unlock()

		if shouldResolve {
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				m.resolveNameConflict()
			}()
		}
	}
}

// isSelfResponse checks if an IP address belongs to any of the local interfaces
func (m *Manager) isSelfResponse(addr net.IP) bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	for _, state := range m.interfaces {
		if !state.Active {
			continue
		}
		addrs, err := state.Interface.Addrs()
		if err != nil {
			log.Printf("ERROR: Failed to get addresses for interface %s: %v", state.Interface.Name, err)
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if ipnet.IP.Equal(addr) {
					return true
				}
			}
		}
	}
	return false
}

// resolveNameConflict handles name conflicts using deterministic resolution
func (m *Manager) resolveNameConflict() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.conflictDetector.mutex.Lock()
	defer m.conflictDetector.mutex.Unlock()

	atomic.AddUint64(&m.conflictDetector.ResolutionAttempts, 1)

	// Use our deterministic machine ID as suffix
	newName := fmt.Sprintf("%s-%s", m.baseName, m.machineID)

	// Check if we've already applied this suffix
	if m.finalName == newName {
		log.Printf("CONFLICT: Already using deterministic name %s, conflict resolution complete", newName)
		return
	}

	// Update to deterministic name
	oldName := m.finalName
	m.setFinalNameLocked(newName)
	m.conflictDetector.CurrentSuffix = m.machineID

	log.Printf("CONFLICT: Resolved conflict - renamed from %s.local to %s.local", oldName, m.finalName)

	// Send immediate announcements with new name
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		// Send multiple announcements to establish the new name quickly
		// Include both hostname (A/AAAA) and service (PTR/SRV/TXT) records
		for i := 0; i < 3; i++ {
			// Check stop channel to allow early termination
			select {
			case <-m.stopCh:
				return
			default:
				m.sendMultiInterfaceAnnouncements()
				m.sendServiceAnnouncement()
				time.Sleep(time.Second)
			}
		}
	}()

	// Clear conflict state after resolution
	m.conflictDetector.ConflictDetected = false
	m.conflictDetector.ConflictingSources = make(map[string]ConflictingHost)
}

// probeNameAvailability performs initial conflict detection during startup.
// It sends probe queries and waits for the responder goroutines to detect
// any conflicting responses via handleConflictDetection().
func (m *Manager) probeNameAvailability() error {
	initialName := m.currentServiceName()
	log.Printf("CONFLICT: Probing name availability for %s.local", initialName)

	// Send probes on all interfaces. Responses are processed asynchronously
	// by handleConflictDetection() which sets ConflictDetected if needed.
	m.sendConflictProbes()

	// Allow additional time for late responses to arrive
	time.Sleep(time.Second)

	// Check if any conflicts were detected during probing
	m.conflictDetector.mutex.RLock()
	conflictDetected := m.conflictDetector.ConflictDetected
	m.conflictDetector.mutex.RUnlock()

	// If conflict was detected, resolution already happened in handleConflictDetection()
	finalName := m.currentServiceName()

	if conflictDetected {
		log.Printf("CONFLICT: Using resolved name: %s.local", finalName)
	} else {
		log.Printf("CONFLICT: No conflicts detected, using: %s.local", finalName)
	}

	return nil
}

// conflictMonitor periodically sends probe queries to detect name conflicts.
// This supplements the passive conflict detection in handleConflictDetection().
func (m *Manager) conflictMonitor() {
	defer m.wg.Done()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.sendConflictProbes()
			m.conflictDetector.mutex.Lock()
			m.conflictDetector.LastConflictCheck = time.Now()
			m.conflictDetector.mutex.Unlock()
		}
	}
}
