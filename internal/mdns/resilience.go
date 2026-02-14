package mdns

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// updateInterfaceHealth calculates and updates interface health score
func (m *Manager) updateInterfaceHealth(state *InterfaceState) {
	now := time.Now()

	// Calculate failure rate over recent period
	totalQueries := atomic.LoadUint64(&state.QueryCount)
	errorCount := atomic.LoadUint64(&state.ErrorCount)
	failureCount := atomic.LoadUint64(&state.FailureCount)

	state.resilienceMu.Lock()
	defer state.resilienceMu.Unlock()

	// Start from existing health to accumulate degradation over time
	healthScore := state.HealthScore

	// Factor in error rate
	if totalQueries > 0 {
		errorRate := float64(errorCount) / float64(totalQueries)
		healthScore -= errorRate * 0.5 // Errors reduce health by up to 50%
	}

	// Factor in failure history with time decay
	if failureCount > 0 {
		timeSinceFailure := now.Sub(state.LastFailure)
		failureImpact := float64(failureCount) * 0.1 // Each failure reduces health by 10%

		// Decay failure impact over time (recover over 10 minutes)
		decayFactor := timeSinceFailure.Seconds() / (10 * 60)
		if decayFactor > 1.0 {
			decayFactor = 1.0
		}
		failureImpact *= (1.0 - decayFactor)

		healthScore -= failureImpact
	}

	// Ensure health score is between 0.0 and 1.0
	if healthScore < 0.0 {
		healthScore = 0.0
	}
	if healthScore > 1.0 {
		healthScore = 1.0
	}

	// Update interface health
	state.HealthScore = healthScore
	m.healthMonitor.mutex.Lock()
	m.healthMonitor.InterfaceHealth[state.Interface.Name] = healthScore
	m.healthMonitor.mutex.Unlock()

	log.Printf("DEBUG: Interface %s health updated to %.2f (errors: %d/%d, failures: %d)",
		state.Interface.Name, healthScore, errorCount, totalQueries, failureCount)
}

// calculateBackoffDuration determines how long to wait before retrying a failed interface
func (m *Manager) calculateBackoffDuration(state *InterfaceState) time.Duration {
	attempts := atomic.LoadUint64(&state.RecoveryAttempts)

	// Exponential backoff: initial * (multiplier ^ attempts)
	backoff := float64(m.resilienceConfig.InitialBackoff) *
		math.Pow(m.resilienceConfig.BackoffMultiplier, float64(attempts))

	// Cap at maximum backoff
	if backoff > float64(m.resilienceConfig.MaxBackoff) {
		backoff = float64(m.resilienceConfig.MaxBackoff)
	}

	return time.Duration(backoff)
}

// isInterfaceInBackoff checks if an interface is currently in backoff period
func (m *Manager) isInterfaceInBackoff(state *InterfaceState) bool {
	state.resilienceMu.RLock()
	backoffUntil := state.BackoffUntil
	state.resilienceMu.RUnlock()
	return time.Now().Before(backoffUntil)
}

// markInterfaceFailure records a failure and updates resilience tracking
func (m *Manager) markInterfaceFailure(state *InterfaceState, err error) {
	// Skip resilience tracking during shutdown - interfaces may be gone
	if m.stopped.Load() {
		return
	}
	now := time.Now()

	atomic.AddUint64(&state.FailureCount, 1)
	atomic.AddUint64(&state.RecoveryAttempts, 1) // Increment for exponential backoff
	// Calculate and set backoff period
	backoff := m.calculateBackoffDuration(state)

	state.resilienceMu.Lock()
	state.LastFailure = now
	state.BackoffUntil = now.Add(backoff)
	state.resilienceMu.Unlock()

	log.Printf("RESILIENCE: Interface %s failed (attempt %d), backing off for %v: %v",
		state.Interface.Name, atomic.LoadUint64(&state.FailureCount), backoff, err)

	// Update health score
	m.updateInterfaceHealth(state)

	// Update system error counter
	atomic.AddUint64(&m.healthMonitor.SystemErrors, 1)
}

// attemptInterfaceRecovery tries to recover a failed interface
func (m *Manager) attemptInterfaceRecovery(name string, state *InterfaceState) bool {
	// Check if still in backoff period
	if m.isInterfaceInBackoff(state) {
		return false
	}

	// Note: RecoveryAttempts is already incremented in markInterfaceFailure()
	atomic.AddUint64(&m.healthMonitor.RecoveryAttempts, 1)

	log.Printf("RESILIENCE: Attempting recovery of interface %s (attempt %d)",
		name, atomic.LoadUint64(&state.RecoveryAttempts))

	// Close old connections if they exist
	if state.IPv4Conn != nil {
		state.IPv4Conn.Close()
		state.IPv4Conn = nil
	}
	if state.IPv6Conn != nil {
		state.IPv6Conn.Close()
		state.IPv6Conn = nil
	}

	// Try to recreate the interface connection
	if err := m.setupInterface(state.Interface); err != nil {
		m.markInterfaceFailure(state, fmt.Errorf("recovery failed: %w", err))
		return false
	}

	// Reset failure tracking on successful recovery
	atomic.StoreUint64(&state.FailureCount, 0)
	atomic.StoreUint64(&state.RecoveryAttempts, 0)
	state.resilienceMu.Lock()
	state.BackoffUntil = time.Time{} // Clear backoff
	state.resilienceMu.Unlock()

	log.Printf("RESILIENCE: Successfully recovered interface %s", name)
	m.updateInterfaceHealth(state)

	return true
}

// performHealthCheck runs comprehensive health monitoring
func (m *Manager) performHealthCheck() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	now := time.Now()

	m.healthMonitor.mutex.Lock()
	m.healthMonitor.LastHealthCheck = now
	m.healthMonitor.mutex.Unlock()

	totalInterfaces := len(m.interfaces)
	healthyInterfaces := 0
	totalHealth := 0.0

	// Check each interface
	for name, state := range m.interfaces {
		m.updateInterfaceHealth(state)

		stateActive := state.Active

		state.resilienceMu.Lock()
		healthScore := state.HealthScore
		if !stateActive {
			healthScore = 0.0
			state.HealthScore = 0.0
		}
		state.resilienceMu.Unlock()

		m.healthMonitor.mutex.Lock()
		m.healthMonitor.InterfaceHealth[name] = healthScore
		m.healthMonitor.mutex.Unlock()

		if healthScore >= m.resilienceConfig.MinHealthScore {
			healthyInterfaces++
		}
		totalHealth += healthScore

		// Attempt recovery for unhealthy interfaces
		if !stateActive || healthScore < m.resilienceConfig.MinHealthScore {
			m.attemptInterfaceRecovery(name, state)
		}
	}

	// Calculate overall system health
	var overallHealth float64
	if totalInterfaces > 0 {
		overallHealth = totalHealth / float64(totalInterfaces)
	} else {
		overallHealth = 0.0
	}

	m.healthMonitor.mutex.Lock()
	m.healthMonitor.OverallHealth = overallHealth
	recoveryActive := m.healthMonitor.RecoveryActive
	m.healthMonitor.mutex.Unlock()

	// Log health summary
	log.Printf("RESILIENCE: Health check - Overall: %.2f, Healthy: %d/%d, System errors: %d",
		overallHealth, healthyInterfaces, totalInterfaces,
		atomic.LoadUint64(&m.healthMonitor.SystemErrors))

	// Trigger recovery mode if health is critically low
	if overallHealth < 0.3 && !recoveryActive {
		m.enterRecoveryMode()
	} else if overallHealth > 0.8 && recoveryActive {
		m.exitRecoveryMode()
	}
}

// enterRecoveryMode activates aggressive recovery measures
func (m *Manager) enterRecoveryMode() {
	m.healthMonitor.mutex.Lock()
	m.healthMonitor.RecoveryActive = true
	overallHealth := m.healthMonitor.OverallHealth
	m.healthMonitor.mutex.Unlock()

	log.Printf("RESILIENCE: ENTERING RECOVERY MODE - System health critically low (%.2f)",
		overallHealth)

	// Trigger immediate interface rediscovery
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.discoverInterfaces(); err != nil {
			log.Printf("RESILIENCE: Emergency interface discovery failed: %v", err)
		}
	}()
}

// exitRecoveryMode deactivates recovery mode when health improves
func (m *Manager) exitRecoveryMode() {
	m.healthMonitor.mutex.Lock()
	m.healthMonitor.RecoveryActive = false
	overallHealth := m.healthMonitor.OverallHealth
	m.healthMonitor.mutex.Unlock()

	log.Printf("RESILIENCE: EXITING RECOVERY MODE - System health restored (%.2f)",
		overallHealth)
}

// healthMonitorLoop runs periodic health checks and recovery operations
func (m *Manager) healthMonitorLoop() {
	defer m.wg.Done()

	ticker := time.NewTicker(m.resilienceConfig.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.performHealthCheck()
		}
	}
}

// isClosedConnError returns true if the error indicates a closed network connection.
func isClosedConnError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection")
}

// recoverClosedConnection asynchronously recovers an interface whose connections
// have been closed (e.g., by a network reset). The staleState parameter ensures
// recovery only runs once per stale state — concurrent callers detecting the same
// dead connections will no-op via pointer comparison.
func (m *Manager) recoverClosedConnection(name string, staleState *InterfaceState) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		m.mutex.Lock()
		if m.stopped.Load() {
			m.mutex.Unlock()
			return
		}
		current, exists := m.interfaces[name]
		if !exists || current != staleState {
			// Already recovered by another goroutine, or interface removed.
			m.mutex.Unlock()
			return
		}

		// Prevent rapid recovery loops: if we recovered this interface recently,
		// defer to the health monitor which uses exponential backoff.
		if lastRecovery, ok := m.recoveryCooldowns[name]; ok && time.Since(lastRecovery) < 30*time.Second {
			staleState.Active = false
			m.mutex.Unlock()
			return
		}
		m.recoveryCooldowns[name] = time.Now()

		log.Printf("RESILIENCE: Recovering closed connection on interface %s", name)

		// Close old connections. Do NOT set to nil — the responder goroutine reads
		// these pointers without holding the mutex, so nilling would race with its
		// nil-check and cause a panic. Close() is sufficient: the responder's next
		// ReadFromUDP will return "use of closed network connection" and exit.
		if staleState.IPv4Conn != nil {
			staleState.IPv4Conn.Close()
		}
		if staleState.IPv6Conn != nil {
			staleState.IPv6Conn.Close()
		}

		recovered := false
		if err := m.setupInterface(staleState.Interface); err != nil {
			// Mark inactive and record failure so health monitor retries with backoff.
			staleState.Active = false
			m.markInterfaceFailure(staleState, fmt.Errorf("closed connection recovery failed: %w", err))
			log.Printf("RESILIENCE: Failed to recover interface %s, will retry via health monitor: %v", name, err)
		} else {
			log.Printf("RESILIENCE: Successfully recovered interface %s", name)
			recovered = true
		}
		m.mutex.Unlock()

		if recovered && !m.stopped.Load() {
			m.sendMultiInterfaceAnnouncements()
			m.sendServiceAnnouncement()
			m.sendPeerDiscoveryQuery()
		}
	}()
}
