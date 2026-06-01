package server

import (
	"os"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/remote/nexusclient"
	"piccolod/internal/services"
)

func TestServiceRemoteResolver(t *testing.T) {
	oldPort := os.Getenv("PORT")
	os.Setenv("PORT", "8081")
	defer os.Setenv("PORT", oldPort)

	svc := services.NewServiceManager()
	resolver := newServiceRemoteResolver(svc)

	// RFC 20260130: listener name is the app identity, so app "demo" needs listener named "demo"
	listeners := []api.AppListener{
		{Name: "demo", GuestPort: 8080, Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP, RemotePorts: []int{80, 443, 8000}, Primary: true},
	}
	eps, err := svc.AllocateForApp("demo", listeners)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("expected one endpoint, got %d", len(eps))
	}
	webEndpoint := eps[0]

	resolver.SetRemoteBases("self-hosted", []remoteBase{
		{source: "self-hosted", portalHost: "portal.example.com", domain: "portal.example.com"},
	})

	resolver.SetTlsMuxPort(9090)

	port, ok := resolver.Resolve("portal.example.com", 443, true)
	if !ok || port != 9090 {
		t.Fatalf("expected portal TLS traffic to map to tls mux (9090), got %d (ok=%v)", port, ok)
	}

	port, ok = resolver.Resolve("demo.portal.example.com", 443, true)
	if !ok || port != 9090 {
		t.Fatalf("expected tls mux to terminate tls traffic on 443, got %d (ok=%v)", port, ok)
	}

	port, ok = resolver.Resolve("demo.portal.example.com", 443, false)
	if !ok || port != webEndpoint.PublicPort {
		t.Fatalf("expected plain traffic on 443 to map to %d, got %d (ok=%v)", webEndpoint.PublicPort, port, ok)
	}

	port, ok = resolver.Resolve("portal.example.com", 80, false)
	if !ok || port != 8081 {
		t.Fatalf("expected portal HTTP traffic to map to 8081, got %d (ok=%v)", port, ok)
	}

	port, ok = resolver.Resolve("demo.portal.example.com", 80, false)
	if !ok || port != 8081 {
		t.Fatalf("expected app HTTP-01/redirect traffic to map to portal HTTP port 8081, got %d (ok=%v)", port, ok)
	}

	port, ok = resolver.Resolve("demo.portal.example.com", 80, true)
	if !ok || port != 9090 {
		t.Fatalf("expected tls mux to handle tls requests on port 80, got %d (ok=%v)", port, ok)
	}

	port, ok = resolver.Resolve("demo.portal.example.com", 8000, true)
	if !ok || port != 9090 {
		t.Fatalf("expected tls mux to handle tls requests on 8000, got %d (ok=%v)", port, ok)
	}

	port, ok = resolver.Resolve("demo.portal.example.com", 8000, false)
	if !ok || port != webEndpoint.PublicPort {
		t.Fatalf("expected plain requests on 8000 to route to %d, got %d (ok=%v)", webEndpoint.PublicPort, port, ok)
	}

	svc.Stop()
}

func TestServiceRemoteResolver_MTLSRemotePortsAreTLSMuxOnly(t *testing.T) {
	oldPort := os.Getenv("PORT")
	os.Setenv("PORT", "8081")
	defer os.Setenv("PORT", oldPort)

	svc := services.NewServiceManager()
	defer svc.Stop()
	resolver := newServiceRemoteResolver(svc)
	resolver.SetRemoteBases("self-hosted", []remoteBase{
		{source: "self-hosted", portalHost: "portal.example.com", domain: "portal.example.com"},
	})
	resolver.SetTlsMuxPort(9090)

	listeners := []api.AppListener{
		{Name: "demo", GuestPort: 8080, Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP, Primary: true},
		{
			Name:        "admin",
			GuestPort:   2022,
			Protocol:    api.ListenerProtocolHTTP,
			Flow:        api.FlowTCP,
			RemotePorts: []int{8443},
			ConnectionAuth: &api.ConnectionAuth{MTLS: &api.ConnectionAuthMTLS{
				Verifier: api.ConnectionAuthMTLSVerifier{Type: "piccolo_session"},
			}},
		},
		{
			Name:      "ops",
			GuestPort: 9091,
			Protocol:  api.ListenerProtocolHTTP,
			Flow:      api.FlowTCP,
			ConnectionAuth: &api.ConnectionAuth{MTLS: &api.ConnectionAuthMTLS{
				Verifier: api.ConnectionAuthMTLSVerifier{Type: "piccolo_session"},
			}},
		},
		{
			Name:      "ssh",
			GuestPort: 22,
			Protocol:  api.ListenerProtocolRaw,
			Flow:      api.FlowTCP,
			TLSWrap:   true,
			ConnectionAuth: &api.ConnectionAuth{MTLS: &api.ConnectionAuthMTLS{
				Verifier: api.ConnectionAuthMTLSVerifier{Type: "piccolo_session"},
			}},
		},
	}
	if _, err := svc.AllocateForApp("demo", listeners); err != nil {
		t.Fatalf("allocate: %v", err)
	}

	port, ok := resolver.Resolve("admin-demo.portal.example.com", 8443, true)
	if !ok || port != 9090 {
		t.Fatalf("expected mTLS custom remote port to route to mux 9090, got %d ok=%v", port, ok)
	}
	if port, ok := resolver.Resolve("admin-demo.portal.example.com", 8443, false); ok {
		t.Fatalf("expected non-TLS mTLS hostname to have no legacy route, got %d", port)
	}
	if got := resolver.ResolveRoute("admin-demo.portal.example.com", 8443, false); got.Decision != nexusclient.RouteDeny {
		t.Fatalf("expected terminal deny for non-TLS mTLS hostname, got %+v", got)
	}
	if got := resolver.ResolveRoute("admin-demo.portal.example.com", 443, true); got.Decision != nexusclient.RouteDeny {
		t.Fatalf("expected terminal deny for disallowed mTLS remote port, got %+v", got)
	}
	port, ok = resolver.Resolve("ops-demo.portal.example.com", 80, true)
	if !ok || port != 9090 {
		t.Fatalf("expected TLS traffic on inherited port 80 for mTLS HTTP listener to map to mux 9090, got %d ok=%v", port, ok)
	}
	port, ok = resolver.Resolve("ops-demo.portal.example.com", 80, false)
	if !ok || port != 8081 {
		t.Fatalf("expected cleartext port 80 for mTLS HTTP listener to map to portal HTTP port 8081, got %d ok=%v", port, ok)
	}
	port, ok = resolver.Resolve("ssh-demo.portal.example.com", 80, false)
	if !ok || port != 8081 {
		t.Fatalf("expected raw mTLS hostname HTTP-01 traffic to map to portal HTTP port 8081, got %d ok=%v", port, ok)
	}
	port, ok = resolver.Resolve("ssh-demo.portal.example.com", 443, true)
	if !ok || port != 9090 {
		t.Fatalf("expected raw mTLS hostname TLS traffic to map to mux 9090, got %d ok=%v", port, ok)
	}
	if got := resolver.ResolveRoute("ssh-demo.portal.example.com", 443, false); got.Decision != nexusclient.RouteDeny {
		t.Fatalf("expected terminal deny for non-TLS raw mTLS hostname payload port, got %+v", got)
	}
}

func TestResolvePortalPortZero(t *testing.T) {
	// When portalPort is 0, Resolve must return (0, false) for port-80 requests.
	// This prevents the connectHandler from receiving a resolved port of 0.
	svc := services.NewServiceManager()
	defer svc.Stop()
	resolver := &serviceRemoteResolver{services: svc, port: 0} // portalPort = 0

	resolver.SetRemoteBases("self-hosted", []remoteBase{
		{source: "self-hosted", portalHost: "portal.example.com", domain: "portal.example.com"},
	})

	t.Run("base_portal_port80", func(t *testing.T) {
		port, ok := resolver.Resolve("portal.example.com", 80, false)
		if ok && port == 0 {
			t.Fatal("must not return (0, true) — this causes the wrong cert to be served")
		}
		if ok {
			t.Fatalf("expected no match when portalPort=0, got port=%d", port)
		}
	})

	t.Run("alias_portal_port80", func(t *testing.T) {
		resolver.UpdateAliases(map[string]string{"custom.com": "__portal"})
		port, ok := resolver.Resolve("custom.com", 80, false)
		if ok && port == 0 {
			t.Fatal("must not return (0, true) — this causes the wrong cert to be served")
		}
		if ok {
			t.Fatalf("expected no match when portalPort=0, got port=%d", port)
		}
	})
}

func TestPortalHostsStableOrdering(t *testing.T) {
	resolver := newServiceRemoteResolver(nil)

	// Set namek first, then self-hosted — self-hosted should still come first.
	resolver.SetRemoteBases("namek", []remoteBase{
		{source: "namek", portalHost: "slug.test.local", domain: "test.local"},
	})
	resolver.SetRemoteBases("self-hosted", []remoteBase{
		{source: "self-hosted", portalHost: "portal.example.com", domain: "portal.example.com"},
	})

	hosts := resolver.PortalHosts()
	if len(hosts) != 2 {
		t.Fatalf("expected 2 hosts, got %d", len(hosts))
	}
	if hosts[0] != "portal.example.com" {
		t.Errorf("expected self-hosted first, got %q", hosts[0])
	}
	if hosts[1] != "slug.test.local" {
		t.Errorf("expected namek second, got %q", hosts[1])
	}

	// Re-set self-hosted — order should remain stable.
	resolver.SetRemoteBases("self-hosted", []remoteBase{
		{source: "self-hosted", portalHost: "new-portal.example.com", domain: "new-portal.example.com"},
	})
	hosts = resolver.PortalHosts()
	if len(hosts) != 2 {
		t.Fatalf("expected 2 hosts, got %d", len(hosts))
	}
	if hosts[0] != "new-portal.example.com" {
		t.Errorf("expected updated self-hosted first, got %q", hosts[0])
	}
	if hosts[1] != "slug.test.local" {
		t.Errorf("expected namek second, got %q", hosts[1])
	}
}
