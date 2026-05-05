package network

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"testing"

	"piccolod/internal/events"
	"piccolod/internal/network/nmclient"
	"piccolod/internal/runner"
)

// TestManualAPMode starts a REAL WiFi AP with captive portal using the
// production Manager — the exact same code path as piccolod.
//
// WARNING: Disconnects your current WiFi! Only run at the physical machine.
//
// Run:  sudo MANUAL_TEST=1 AP_TEST=1 go test ./internal/network/ -run TestManualAPMode -v -timeout 0
// Stop: Ctrl+C (restores previous WiFi connection)
func TestManualAPMode(t *testing.T) {
	if os.Getenv("MANUAL_TEST") == "" || os.Getenv("AP_TEST") == "" {
		t.Skip("set MANUAL_TEST=1 AP_TEST=1 to run this test (needs root + WiFi AP support)")
	}
	if os.Getuid() != 0 {
		t.Skip("AP mode test requires root")
	}

	// Create real NM client
	r := runner.ExecRunner{}
	nm, err := nmclient.NewDBusClient(r)
	if err != nil {
		t.Fatalf("D-Bus connect: %v", err)
	}
	defer nm.Close()

	// Create production Manager with real NM + event bus (no health tracker needed)
	bus := events.NewBus()
	defer bus.Close()
	mgr := NewManager(nm, r, bus)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Wire the supervisor (production path). Uses a private dbus connection
	// so its NM signal subscriptions don't cross-talk with the manager's.
	sys := NewStaticSystemState(false, "")
	sup, err := BuildSupervisor(ctx, r, sys)
	if err != nil {
		t.Fatalf("BuildSupervisor: %v", err)
	}
	mgr.SetSupervisor(sup)
	sup.SetEventBus(bus)
	sup.WireActuators(nm, r, mgr.APMgr())
	sup.EnableActuation(true)

	// Full production startup: reconcile, discover devices, wire portal, background loops
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	defer mgr.Stop(ctx)
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Supervisor.Start: %v", err)
	}
	defer sup.Stop(ctx)

	// Force AP mode — bypasses the slow state machine escalation.
	// Blocks until AP + captive portal are ready.
	t.Log("Forcing AP mode (this will disconnect your current WiFi)...")
	if err := mgr.ForceAPMode(); err != nil {
		t.Fatalf("ForceAPMode: %v", err)
	}

	apStatus := mgr.APStatus()
	apIP := mgr.apMgr.APIP()

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║           REAL AP MODE TEST (Production Path)    ║")
	fmt.Println("╠══════════════════════════════════════════════════╣")
	fmt.Printf("║  AP SSID:  %-38s ║\n", apStatus.SSID)
	fmt.Printf("║  AP IP:    %-38s ║\n", apIP)
	fmt.Println("║                                                  ║")
	fmt.Println("║  Steps:                                          ║")
	fmt.Println("║  1. On your phone, connect to the AP above       ║")
	fmt.Println("║  2. Captive portal should auto-open              ║")
	fmt.Printf("║  3. Or navigate to http://%-23s ║\n", apIP)
	fmt.Println("║  4. Select a network, enter passphrase           ║")
	fmt.Println("║  5. Connect goes through production code path    ║")
	fmt.Println("║                                                  ║")
	fmt.Println("║  Press Ctrl+C to stop and restore WiFi           ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("  Local IPs:", localIPs())
	fmt.Println()

	<-ctx.Done()
	fmt.Println("\nShutting down...")
}

func localIPs() []string {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return []string{"<unknown>"}
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			ips = append(ips, ipnet.IP.String())
		}
	}
	if len(ips) == 0 {
		return []string{"<no-ip-found>"}
	}
	return ips
}
