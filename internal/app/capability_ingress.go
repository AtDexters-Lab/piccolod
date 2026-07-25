package app

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/netutil"
	"golang.org/x/sys/unix"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

const (
	capabilityIngressPortStart = 26000
	capabilityIngressPortSpan  = 10000
	capabilityIngressMaxHeader = 64 << 10
	capabilityIngressMaxConns  = 128
)

type capabilityIngressKey struct {
	Consumer   string
	Capability string
}

type capabilityIngress struct {
	anchorID  string
	anchorPID int
	port      int
	listener  net.Listener
	server    *http.Server
}

type namespaceListenResult struct {
	listener net.Listener
	err      error
}

type capabilityIngressListenFunc func(pid, port int) (net.Listener, error)

func listenInNetworkNamespace(pid, port int) (net.Listener, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("network namespace PID is unavailable")
	}

	// Namespace changes are per-thread. Run the transition in a dedicated
	// locked goroutine so a restore failure makes that goroutine exit while
	// still locked; Go then retires the contaminated OS thread instead of
	// returning it to the scheduler.
	result := make(chan namespaceListenResult, 1)
	go func() {
		runtime.LockOSThread()
		restored := false
		defer func() {
			if restored {
				runtime.UnlockOSThread()
			}
		}()

		currentPath := "/proc/self/task/" + strconv.Itoa(unix.Gettid()) + "/ns/net"
		current, err := os.Open(currentPath)
		if err != nil {
			restored = true // The thread never left its original namespace.
			result <- namespaceListenResult{err: err}
			return
		}
		defer current.Close()
		target, err := os.Open("/proc/" + strconv.Itoa(pid) + "/ns/net")
		if err != nil {
			restored = true
			result <- namespaceListenResult{err: err}
			return
		}
		defer target.Close()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			restored = true
			result <- namespaceListenResult{err: fmt.Errorf("enter consumer network namespace: %w", err)}
			return
		}
		listener, listenErr := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if restoreErr := unix.Setns(int(current.Fd()), unix.CLONE_NEWNET); restoreErr != nil {
			if listener != nil {
				_ = listener.Close()
			}
			result <- namespaceListenResult{err: fmt.Errorf("restore Piccolod network namespace: %w", restoreErr)}
			return
		}
		restored = true
		result <- namespaceListenResult{listener: listener, err: listenErr}
	}()
	outcome := <-result
	return outcome.listener, outcome.err
}

func (m *AppManager) listenCapabilityIngress(pid, port int) (net.Listener, error) {
	if m.capabilityListen != nil {
		return m.capabilityListen(pid, port)
	}
	return listenInNetworkNamespace(pid, port)
}

func initialCapabilityPort(consumer, capability string) int {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(consumer))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(capability))
	return capabilityIngressPortStart + int(hash.Sum32()%capabilityIngressPortSpan)
}

func (m *AppManager) ensureCapabilityIngress(
	ctx context.Context,
	state *FilesystemStateManager,
	consumer, capability, anchorID string,
	runtimeConfig container.PodmanRuntime,
	reservedPorts map[int]struct{},
) (string, error) {
	key := capabilityIngressKey{Consumer: consumer, Capability: capability}
	m.capabilityIngressMu.Lock()
	defer m.capabilityIngressMu.Unlock()
	if m.capabilityIngresses == nil {
		m.capabilityIngresses = make(map[capabilityIngressKey]*capabilityIngress)
	}

	anchorState, err := m.containerManager.InspectContainerState(ctx, runtimeConfig, anchorID)
	if err != nil {
		return "", fmt.Errorf("inspect consumer network anchor: %w", err)
	}
	if !anchorState.Running || anchorState.PID <= 0 {
		return "", fmt.Errorf("consumer network anchor is not running")
	}
	if current := m.capabilityIngresses[key]; current != nil &&
		current.anchorID == anchorID &&
		current.anchorPID == anchorState.PID {
		return "http://127.0.0.1:" + strconv.Itoa(current.port), nil
	}
	if old := m.capabilityIngresses[key]; old != nil {
		_ = old.server.Close()
		_ = old.listener.Close()
		delete(m.capabilityIngresses, key)
	}

	durable, err := state.loadCapabilityState()
	if err != nil {
		return "", err
	}
	retainedPort := durable.IngressPorts[consumer][capability]
	port := retainedPort
	var listener net.Listener
	if retainedPort != 0 {
		if _, reserved := reservedPorts[retainedPort]; reserved {
			return "", fmt.Errorf(
				"retained private capability ingress port %d conflicts with an app service port",
				retainedPort,
			)
		}
		listener, err = m.listenCapabilityIngress(anchorState.PID, retainedPort)
		if err != nil {
			return "", fmt.Errorf(
				"retained private capability ingress port %d is unavailable: %w",
				retainedPort,
				err,
			)
		}
	} else {
		port = initialCapabilityPort(consumer, capability)
		for attempt := 0; attempt < capabilityIngressPortSpan; attempt++ {
			candidate := capabilityIngressPortStart + ((port - capabilityIngressPortStart + attempt) % capabilityIngressPortSpan)
			if _, reserved := reservedPorts[candidate]; reserved {
				continue
			}
			listener, err = m.listenCapabilityIngress(anchorState.PID, candidate)
			if err == nil {
				port = candidate
				break
			}
			if !strings.Contains(strings.ToLower(err.Error()), "address already in use") {
				return "", err
			}
		}
		if listener == nil {
			return "", fmt.Errorf("no private capability ingress port available")
		}
	}
	confirmedAnchor, confirmErr := m.containerManager.InspectContainerState(ctx, runtimeConfig, anchorID)
	if confirmErr != nil ||
		!confirmedAnchor.Running ||
		confirmedAnchor.PID != anchorState.PID {
		_ = listener.Close()
		if confirmErr != nil {
			return "", fmt.Errorf("reconfirm consumer network anchor: %w", confirmErr)
		}
		return "", fmt.Errorf("consumer network anchor changed while creating private ingress")
	}
	if durable.IngressPorts[consumer] == nil {
		durable.IngressPorts[consumer] = make(map[string]int)
	}
	durable.IngressPorts[consumer][capability] = port
	if err := state.storeCapabilityState(durable); err != nil {
		_ = listener.Close()
		return "", err
	}
	listener = netutil.LimitListener(listener, capabilityIngressMaxConns)

	server := newCapabilityIngressHTTPServer(m.capabilityIngressHandler(state, consumer, capability))
	ingress := &capabilityIngress{
		anchorID:  anchorID,
		anchorPID: anchorState.PID,
		port:      port,
		listener:  listener,
		server:    server,
	}
	m.capabilityIngresses[key] = ingress
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("WARN: capability ingress %s/%s: %v", consumer, capability, err)
		}
	}()
	return "http://127.0.0.1:" + strconv.Itoa(port), nil
}

func newCapabilityIngressHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    capabilityIngressMaxHeader,
	}
}

func capabilityProviderRouteAvailable(state *FilesystemStateManager, providerID string) bool {
	record, err := state.LoadTransitionRecord(providerID)
	if err == nil && record != nil {
		switch record.Phase {
		case TransitionPhasePrepared,
			TransitionPhaseResourcesPrepared,
			TransitionPhaseCommittedMetadataPending,
			TransitionPhaseCommittedCleanupPending,
			TransitionPhaseCommitted,
			TransitionPhaseRuntimeGroupCommitted:
			// The legacy journal below may be one durable write ahead of this
			// projection, so an allowed v2 phase is not sufficient by itself.
		default:
			return false
		}
	}
	if err != nil && !os.IsNotExist(err) {
		return false
	}

	// The v2 transition is authoritative for current writers. The legacy
	// manifest journal remains a fail-closed fallback for an interrupted update
	// created before that transition could be stored.
	txn, err := state.LoadManifestUpdateTransaction(providerID)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil || txn == nil {
		return false
	}
	switch txn.Phase {
	case "prepared",
		"rootfs_staging",
		"rootfs_staged",
		"listeners_prepared",
		"data_snapshot_planned",
		"data_snapshot_failed",
		"data_snapshot_created":
		return true
	case "access_published",
		"committed_metadata_pending",
		"committed_cleanup_pending",
		"committed":
		return txn.AccessPublished
	default:
		return false
	}
}

func (m *AppManager) capabilityIngressHandler(state *FilesystemStateManager, consumer, capability string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !capabilityConsumerBindingCurrent(state, consumer, capability) {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		durable, err := state.loadCapabilityState()
		if err != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		providerID := durable.Defaults[capability]
		if !capabilityProviderRouteAvailable(state, providerID) {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		provider, ok := state.GetApp(providerID)
		if !ok || provider == nil || m.getObservedStatus(providerID) != StatusRunning {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		listenerName, basePath, ok := providedCapability(provider.Definition, capability)
		if !ok || m.serviceManager == nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		endpoint, ok := m.serviceManager.GetAppListener(providerID, listenerName)
		if !ok || endpoint.HostBind <= 0 {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		requestPath, ok := canonicalCapabilityRequestPath(r.URL)
		if !ok || !pathWithinCapabilityBase(requestPath, basePath) {
			http.NotFound(w, r)
			return
		}
		target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(endpoint.HostBind))}
		proxy := httputil.NewSingleHostReverseProxy(target)
		originalDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			originalDirector(req)
			req.URL.Path = requestPath
			req.URL.RawPath = ""
			stripCapabilityIdentityHeaders(req.Header)
			// ReverseProxy otherwise appends the caller address after Director.
			// A nil slice explicitly disables that behavior, so no consumer
			// identity metadata crosses the private binding.
			req.Header["X-Forwarded-For"] = nil
		}
		proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		}
		proxy.ServeHTTP(w, r)
	})
}

func canonicalCapabilityRequestPath(value *url.URL) (string, bool) {
	if value == nil {
		return "", false
	}
	decoded, err := canonicalCapabilityPath(value.EscapedPath())
	return decoded, err == nil
}

func pathWithinCapabilityBase(requestPath, basePath string) bool {
	decodedBasePath, err := canonicalCapabilityPath(basePath)
	if err != nil {
		return false
	}
	basePath = decodedBasePath
	if basePath == "/" {
		return strings.HasPrefix(requestPath, "/")
	}
	return requestPath == basePath || strings.HasPrefix(requestPath, basePath+"/")
}

func stripCapabilityIdentityHeaders(header http.Header) {
	for name := range header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-piccolo-") ||
			strings.HasPrefix(lower, "x-forwarded-") ||
			lower == "forwarded" ||
			lower == "x-real-ip" {
			header.Del(name)
		}
	}
}

func (m *AppManager) ensureCapabilityBindingEnvironment(
	ctx context.Context,
	state *FilesystemStateManager,
	app *AppInstance,
	def *api.AppDefinition,
	anchorID string,
	runtimeConfig container.PodmanRuntime,
) (map[string]map[string]string, error) {
	result := make(map[string]map[string]string)
	expected := consumedCapabilities(def)
	if err := m.pruneCapabilityIngresses(state, app.InstanceID, expected); err != nil {
		return nil, err
	}
	if def == nil || len(expected) == 0 {
		return result, nil
	}
	durable, err := state.loadCapabilityState()
	if err != nil {
		return nil, err
	}
	origins := make(map[string]string)
	reservedPorts := make(map[int]struct{})
	for _, service := range def.Services {
		for _, port := range service.BindPorts {
			reservedPorts[port] = struct{}{}
		}
	}
	for capability := range expected {
		origin, err := m.ensureCapabilityIngress(ctx, state, app.InstanceID, capability, anchorID, runtimeConfig, reservedPorts)
		if err != nil {
			return nil, err
		}
		origins[capability] = origin
	}
	for serviceName, service := range def.Services {
		for _, consumer := range service.Consumes {
			baseURL := origins[consumer.Capability]
			if provider, ok := state.GetApp(durable.Defaults[consumer.Capability]); ok {
				if _, basePath, provided := providedCapability(provider.Definition, consumer.Capability); provided {
					baseURL += basePath
				}
			}
			if result[serviceName] == nil {
				result[serviceName] = make(map[string]string)
			}
			for environmentName, property := range consumer.Env {
				if property == api.CapabilityBindingBaseURL {
					result[serviceName][environmentName] = baseURL
				}
			}
		}
	}
	return result, nil
}

func (m *AppManager) pruneCapabilityIngresses(
	state *FilesystemStateManager,
	consumer string,
	expected map[string]struct{},
) error {
	m.capabilityIngressMu.Lock()
	for key, ingress := range m.capabilityIngresses {
		if key.Consumer != consumer {
			continue
		}
		if _, keep := expected[key.Capability]; keep {
			continue
		}
		_ = ingress.server.Close()
		_ = ingress.listener.Close()
		delete(m.capabilityIngresses, key)
	}
	m.capabilityIngressMu.Unlock()

	durable, err := state.loadCapabilityState()
	if err != nil {
		return err
	}
	ports := durable.IngressPorts[consumer]
	changed := false
	for capability := range ports {
		if _, keep := expected[capability]; !keep {
			delete(ports, capability)
			changed = true
		}
	}
	if len(ports) == 0 {
		if _, exists := durable.IngressPorts[consumer]; exists {
			delete(durable.IngressPorts, consumer)
			changed = true
		}
	}
	if changed {
		return state.storeCapabilityState(durable)
	}
	return nil
}

func (m *AppManager) removeCapabilityIngresses(consumer string) {
	m.capabilityIngressMu.Lock()
	defer m.capabilityIngressMu.Unlock()
	for key, ingress := range m.capabilityIngresses {
		if key.Consumer != consumer {
			continue
		}
		_ = ingress.server.Close()
		_ = ingress.listener.Close()
		delete(m.capabilityIngresses, key)
	}
}

func (m *AppManager) closeCapabilityIngresses() {
	m.capabilityIngressMu.Lock()
	defer m.capabilityIngressMu.Unlock()
	for key, ingress := range m.capabilityIngresses {
		_ = ingress.server.Close()
		_ = ingress.listener.Close()
		delete(m.capabilityIngresses, key)
	}
}
