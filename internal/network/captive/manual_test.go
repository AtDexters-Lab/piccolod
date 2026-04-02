package captive

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"testing"
)

// TestManualPortal starts the captive portal on :8080 with mock scan data.
// No AP mode — your phone must be on the same network as this machine.
//
// Run:  MANUAL_TEST=1 go test ./internal/network/captive/ -run TestManualPortal -v -timeout 0
// Stop: Ctrl+C
func TestManualPortal(t *testing.T) {
	if os.Getenv("MANUAL_TEST") == "" {
		t.Skip("set MANUAL_TEST=1 to run this interactive test")
	}

	mockScan := func(forceRefresh bool) ([]ScanResult, error) {
		return []ScanResult{
			{SSID: "HomeNetwork-5G", Security: "wpa2", SignalDBm: -45, SignalTier: "good", FrequencyMHz: 5180, Band: "5GHz"},
			{SSID: "HomeNetwork", Security: "wpa2", SignalDBm: -55, SignalTier: "good", FrequencyMHz: 2437, Band: "2.4GHz"},
			{SSID: "Neighbor-WiFi", Security: "wpa3", SignalDBm: -68, SignalTier: "fair", FrequencyMHz: 5240, Band: "5GHz"},
			{SSID: "CoffeeShop", Security: "open", SignalDBm: -75, SignalTier: "weak", FrequencyMHz: 2412, Band: "2.4GHz"},
			{SSID: "IoT-Network", Security: "wpa", SignalDBm: -82, SignalTier: "poor", FrequencyMHz: 2462, Band: "2.4GHz"},
		}, nil
	}

	mockConnect := func(ssid, passphrase string) {
		log.Printf("=== DRY-RUN CONNECT ===")
		log.Printf("  SSID:       %s", ssid)
		log.Printf("  Passphrase: %s", passphrase)
		log.Printf("  ECDH decryption successful!")
		log.Printf("=======================")
	}

	srv, err := NewServer(mockScan, mockConnect, func() {})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := srv.Start(ctx, "0.0.0.0:8080"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║  Mock Portal (same network)                  ║")
	fmt.Println("╠══════════════════════════════════════════════╣")
	fmt.Printf("║  Key: %.38s...  ║\n", srv.keypair.PublicKeyBase64())
	fmt.Println("║                                              ║")
	fmt.Println("║  Open on your phone:                         ║")
	for _, ip := range localIPs() {
		fmt.Printf("║    http://%-35s ║\n", ip+":8080")
	}
	fmt.Println("║                                              ║")
	fmt.Println("║  Press Ctrl+C to stop                        ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	<-ctx.Done()
	fmt.Println("\nShutting down...")
}

// TestManualAPMode has been moved to internal/network/manual_test.go
// where it exercises the production Manager code path.
func TestManualAPMode(t *testing.T) {
	t.Skip("Moved to internal/network/ — run: sudo MANUAL_TEST=1 AP_TEST=1 go test ./internal/network/ -run TestManualAPMode -v -timeout 0")
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
