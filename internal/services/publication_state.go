package services

// AppPublicationActive proves that an app has a non-empty, complete active
// listener set. Registry presence alone is insufficient because transaction
// recovery intentionally retains endpoints while publication is suspended.
func (m *ServiceManager) AppPublicationActive(appName string) bool {
	if m == nil {
		return false
	}
	m.publicationLifecycleMu.Lock()
	defer m.publicationLifecycleMu.Unlock()

	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.proxyManager == nil {
		return false
	}
	if _, inactive := m.deactivated[appName]; inactive {
		return false
	}
	endpoints, exists := m.registry[appName]
	if !exists || len(endpoints) == 0 {
		return false
	}
	for _, endpoint := range endpoints {
		if !m.endpointPublicationRunningLocked(endpoint) {
			return false
		}
	}
	return true
}
