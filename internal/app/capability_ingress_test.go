package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

type fakeCapabilityIngressListener struct {
	port   int
	closed chan struct{}
	once   sync.Once
}

func newFakeCapabilityIngressListener(port int) *fakeCapabilityIngressListener {
	return &fakeCapabilityIngressListener{
		port:   port,
		closed: make(chan struct{}),
	}
}

func (l *fakeCapabilityIngressListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *fakeCapabilityIngressListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeCapabilityIngressListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: l.port}
}

func capabilityIngressPortTestManager(
	t *testing.T,
	retainedPort int,
) (*AppManager, *FilesystemStateManager) {
	t.Helper()
	state := newCapabilityTestState(t)
	durable := newCapabilityState()
	if retainedPort != 0 {
		durable.IngressPorts["consumer"] = map[string]int{
			api.CapabilityAIInferenceOpenAIV1: retainedPort,
		}
	}
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store capability state: %v", err)
	}
	mock := NewMockContainerManager()
	mock.inspectStateForContainer = map[string]container.ContainerState{
		"anchor": {Exists: true, Running: true, PID: 4321},
	}
	manager := &AppManager{
		containerManager:    mock,
		capabilityIngresses: make(map[capabilityIngressKey]*capabilityIngress),
	}
	t.Cleanup(manager.closeCapabilityIngresses)
	return manager, state
}

func TestRetainedCapabilityIngressPortNeverMoves(t *testing.T) {
	const retainedPort = 27001

	t.Run("manifest port collision fails before listen", func(t *testing.T) {
		manager, state := capabilityIngressPortTestManager(t, retainedPort)
		listenCalls := 0
		manager.capabilityListen = func(int, int) (net.Listener, error) {
			listenCalls++
			return nil, errors.New("unexpected listen")
		}
		_, err := manager.ensureCapabilityIngress(
			context.Background(),
			state,
			"consumer",
			api.CapabilityAIInferenceOpenAIV1,
			"anchor",
			container.PodmanRuntime{},
			map[int]struct{}{retainedPort: {}},
		)
		if err == nil || !strings.Contains(err.Error(), "conflicts with an app service port") {
			t.Fatalf("reserved retained port error = %v", err)
		}
		if listenCalls != 0 {
			t.Fatalf("listen calls = %d, want zero", listenCalls)
		}
	})

	t.Run("runtime collision fails without probing", func(t *testing.T) {
		manager, state := capabilityIngressPortTestManager(t, retainedPort)
		var attempted []int
		manager.capabilityListen = func(_ int, port int) (net.Listener, error) {
			attempted = append(attempted, port)
			return nil, fmt.Errorf("listen tcp4 127.0.0.1:%d: bind: address already in use", port)
		}
		_, err := manager.ensureCapabilityIngress(
			context.Background(),
			state,
			"consumer",
			api.CapabilityAIInferenceOpenAIV1,
			"anchor",
			container.PodmanRuntime{},
			nil,
		)
		if err == nil || !strings.Contains(err.Error(), "retained private capability ingress port") {
			t.Fatalf("occupied retained port error = %v", err)
		}
		if len(attempted) != 1 || attempted[0] != retainedPort {
			t.Fatalf("attempted ports = %v, want only %d", attempted, retainedPort)
		}
		durable, loadErr := state.loadCapabilityState()
		if loadErr != nil {
			t.Fatalf("load capability state: %v", loadErr)
		}
		if got := durable.IngressPorts["consumer"][api.CapabilityAIInferenceOpenAIV1]; got != retainedPort {
			t.Fatalf("durable port = %d, want retained %d", got, retainedPort)
		}
	})
}

func TestNewCapabilityIngressMayProbeForFirstAllocation(t *testing.T) {
	manager, state := capabilityIngressPortTestManager(t, 0)
	initial := initialCapabilityPort("consumer", api.CapabilityAIInferenceOpenAIV1)
	next := capabilityIngressPortStart + ((initial - capabilityIngressPortStart + 1) % capabilityIngressPortSpan)
	var attempted []int
	manager.capabilityListen = func(_ int, port int) (net.Listener, error) {
		attempted = append(attempted, port)
		if port == initial {
			return nil, fmt.Errorf("listen tcp4 127.0.0.1:%d: bind: address already in use", port)
		}
		return newFakeCapabilityIngressListener(port), nil
	}
	origin, err := manager.ensureCapabilityIngress(
		context.Background(),
		state,
		"consumer",
		api.CapabilityAIInferenceOpenAIV1,
		"anchor",
		container.PodmanRuntime{},
		nil,
	)
	if err != nil {
		t.Fatalf("ensure first capability ingress: %v", err)
	}
	if len(attempted) != 2 || attempted[0] != initial || attempted[1] != next {
		t.Fatalf("attempted ports = %v, want [%d %d]", attempted, initial, next)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d", next); origin != want {
		t.Fatalf("origin = %q, want %q", origin, want)
	}
	durable, err := state.loadCapabilityState()
	if err != nil {
		t.Fatalf("load capability state: %v", err)
	}
	if got := durable.IngressPorts["consumer"][api.CapabilityAIInferenceOpenAIV1]; got != next {
		t.Fatalf("durable first allocation = %d, want %d", got, next)
	}
}

func TestCapabilityIngressHTTPServerBoundsConnectionHeaders(t *testing.T) {
	t.Parallel()
	server := newCapabilityIngressHTTPServer(http.NotFoundHandler())
	if server.ReadHeaderTimeout <= 0 {
		t.Fatal("private ingress has no read-header deadline")
	}
	if server.IdleTimeout <= 0 {
		t.Fatal("private ingress has no idle deadline")
	}
	if server.MaxHeaderBytes != capabilityIngressMaxHeader {
		t.Fatalf("MaxHeaderBytes = %d, want %d", server.MaxHeaderBytes, capabilityIngressMaxHeader)
	}
	if server.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %s, want zero for streamed inference responses", server.WriteTimeout)
	}
	if server.ReadTimeout != 0 {
		t.Fatalf("ReadTimeout = %s, want zero for streamed request bodies", server.ReadTimeout)
	}
	if server.ReadHeaderTimeout > 30*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s, unexpectedly permissive", server.ReadHeaderTimeout)
	}
}

func TestCapabilityProviderRouteAvailableFollowsTransitionFence(t *testing.T) {
	tests := []struct {
		name  string
		phase TransitionPhase
		want  bool
	}{
		{name: "no transition", want: true},
		{name: "prepared", phase: TransitionPhasePrepared, want: true},
		{name: "resources prepared", phase: TransitionPhaseResourcesPrepared, want: true},
		{name: "candidate touched", phase: TransitionPhaseCandidateTouched},
		{name: "source committing", phase: TransitionPhaseSourceCommitting},
		{name: "publishing access", phase: TransitionPhasePublishingAccess},
		{name: "cleanup pending", phase: TransitionPhaseCommittedCleanupPending, want: true},
		{name: "committed", phase: TransitionPhaseCommitted, want: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			state := newCapabilityTestState(t)
			if test.phase != "" {
				if err := state.StoreTransitionRecord(
					"provider",
					transitionTestRecord("provider", test.phase),
				); err != nil {
					t.Fatalf("StoreTransitionRecord: %v", err)
				}
			}
			if got := capabilityProviderRouteAvailable(state, "provider"); got != test.want {
				t.Fatalf("route available = %v, want %v for phase %q", got, test.want, test.phase)
			}
		})
	}
}

func TestCapabilityProviderRouteAvailableFollowsLegacyPublicationFence(t *testing.T) {
	tests := []struct {
		name            string
		phase           string
		accessPublished bool
		want            bool
	}{
		{name: "before candidate", phase: "rootfs_staged", want: true},
		{name: "candidate persisting", phase: "candidate_persisting"},
		{name: "candidate persisted", phase: "candidate_persisted"},
		{name: "access phase without durable publication", phase: "access_published"},
		{name: "access published", phase: "access_published", accessPublished: true, want: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			state := newCapabilityTestState(t)
			if err := state.StoreManifestUpdateTransaction("provider", &ManifestUpdateTransaction{
				Phase:           test.phase,
				AccessPublished: test.accessPublished,
			}); err != nil {
				t.Fatalf("StoreManifestUpdateTransaction: %v", err)
			}
			if got := capabilityProviderRouteAvailable(state, "provider"); got != test.want {
				t.Fatalf("route available = %v, want %v for phase %q", got, test.want, test.phase)
			}
		})
	}
}

func TestCapabilityConsumerBindingCurrentTracksInjectedValue(t *testing.T) {
	state := newCapabilityTestState(t)
	state.cache["provider-a"] = &AppInstance{
		InstanceID: "provider-a",
		Enabled:    true,
		Definition: capabilityProviderDefinition("/v3"),
	}
	state.cache["provider-b"] = &AppInstance{
		InstanceID: "provider-b",
		Enabled:    true,
		Definition: capabilityProviderDefinition("/v3"),
	}
	for _, consumer := range []string{"consumer-a", "consumer-b"} {
		state.cache[consumer] = &AppInstance{
			InstanceID: consumer,
			Enabled:    true,
			Definition: capabilityConsumerDefinition("OPENAI_BASE_URL"),
		}
	}

	durable := newCapabilityState()
	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "provider-a"
	durable.IngressPorts["consumer-a"] = map[string]int{api.CapabilityAIInferenceOpenAIV1: 27001}
	durable.IngressPorts["consumer-b"] = map[string]int{api.CapabilityAIInferenceOpenAIV1: 27002}
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store initial capability state: %v", err)
	}
	for _, consumer := range []string{"consumer-a", "consumer-b"} {
		bindings, err := desiredCapabilityBindings(state, consumer, state.cache[consumer].Definition)
		if err != nil {
			t.Fatalf("initial bindings for %s: %v", consumer, err)
		}
		state.cache[consumer].CapabilityBindings = bindings
		if !capabilityConsumerBindingCurrent(state, consumer, api.CapabilityAIInferenceOpenAIV1) {
			t.Fatalf("initial binding for %s was rejected", consumer)
		}
	}

	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "provider-b"
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store replacement capability state: %v", err)
	}
	for _, consumer := range []string{"consumer-a", "consumer-b"} {
		if !capabilityConsumerBindingCurrent(state, consumer, api.CapabilityAIInferenceOpenAIV1) {
			t.Fatalf("same injected binding for %s was unnecessarily fenced", consumer)
		}
	}

	state.cache["provider-b"].Definition = capabilityProviderDefinition("/v4")
	for _, consumer := range []string{"consumer-a", "consumer-b"} {
		if capabilityConsumerBindingCurrent(state, consumer, api.CapabilityAIInferenceOpenAIV1) {
			t.Fatalf("provider base_path change did not fence %s", consumer)
		}
	}
}

func TestCanonicalCapabilityRequestPathRejectsAmbiguity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		target string
		want   string
		ok     bool
	}{
		{target: "/v3/chat/completions?stream=true", want: "/v3/chat/completions", ok: true},
		{target: "/v3", want: "/v3", ok: true},
		{target: "/v3//chat"},
		{target: "/v3/./chat"},
		{target: "/v3/%2e%2e/admin"},
		{target: "/v3/%252e%252e/admin"},
		{target: "/v3/%25252e%25252e/admin"},
		{target: "/v3%2fadmin"},
		{target: "/v3%252fadmin"},
		{target: "/v3%25252fadmin"},
		{target: "/v3%5cadmin"},
		{target: "/v3%255cadmin"},
		{target: "/v3/%25literal"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.target, func(t *testing.T) {
			parsed, err := url.ParseRequestURI(test.target)
			if err != nil {
				t.Fatalf("ParseRequestURI: %v", err)
			}
			got, ok := canonicalCapabilityRequestPath(parsed)
			if ok != test.ok || got != test.want {
				t.Fatalf("canonical path = %q, %v; want %q, %v", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestCapabilityPathAuthorizationUsesSegmentBoundary(t *testing.T) {
	t.Parallel()
	if !pathWithinCapabilityBase("/v3", "/v3") ||
		!pathWithinCapabilityBase("/v3/chat", "/v3") ||
		pathWithinCapabilityBase("/v30/chat", "/v3") ||
		pathWithinCapabilityBase("/admin", "/v3") {
		t.Fatalf("capability subtree comparison violated segment boundary")
	}
	if !pathWithinCapabilityBase("/anything", "/") {
		t.Fatalf("root capability path did not expose its declared subtree")
	}
	if !pathWithinCapabilityBase("/v1 beta/models", "/v1%20beta") {
		t.Fatal("canonical escaped base path was not compared in decoded form")
	}
}

func TestStripCapabilityIdentityHeaders(t *testing.T) {
	t.Parallel()
	header := http.Header{
		"Authorization":       []string{"Bearer consumer-owned"},
		"X-Piccolo-Identity":  []string{"spoofed"},
		"X-Forwarded-For":     []string{"10.0.0.1"},
		"X-Forwarded-Proto":   []string{"https"},
		"Forwarded":           []string{"for=10.0.0.1"},
		"X-Real-IP":           []string{"10.0.0.1"},
		"X-Unrelated-Request": []string{"keep"},
	}
	stripCapabilityIdentityHeaders(header)
	for _, name := range []string{"X-Piccolo-Identity", "X-Forwarded-For", "X-Forwarded-Proto", "Forwarded", "X-Real-IP"} {
		if header.Get(name) != "" {
			t.Fatalf("%s was not stripped", name)
		}
	}
	if header.Get("Authorization") == "" || header.Get("X-Unrelated-Request") == "" {
		t.Fatalf("non-identity request headers were stripped: %#v", header)
	}
}
