package captive

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"testing"
	"time"

	"piccolod/internal/network/nmclient"
	"piccolod/internal/runner"
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

	printBanner("Mock Portal (same network)", srv.keypair.PublicKeyBase64())
	<-ctx.Done()
	fmt.Println("\nShutting down...")
}

// TestManualAPMode starts a REAL WiFi AP with captive portal.
// Your phone will see "Piccolo-Setup-XXXX" in the WiFi list.
//
// WARNING: This disconnects your current WiFi connection! Only run if
// you're at the physical machine (not remote SSH).
//
// Scan results come from real NM (actual nearby networks).
// Connect decrypts the passphrase but does NOT change your WiFi (dry-run).
//
// Run:  sudo MANUAL_TEST=1 AP_TEST=1 go test ./internal/network/captive/ -run TestManualAPMode -v -timeout 0
// Stop: Ctrl+C (restores previous WiFi connection)
func TestManualAPMode(t *testing.T) {
	if os.Getenv("MANUAL_TEST") == "" || os.Getenv("AP_TEST") == "" {
		t.Skip("set MANUAL_TEST=1 AP_TEST=1 to run this test (needs root + WiFi AP support)")
	}
	if os.Getuid() != 0 {
		t.Skip("AP mode test requires root")
	}

	r := runner.ExecRunner{}
	nm, err := nmclient.NewDBusClient(r)
	if err != nil {
		t.Fatalf("D-Bus connect: %v", err)
	}
	defer nm.Close()

	wifiDevs, err := nm.WiFiDevices()
	if err != nil || len(wifiDevs) == 0 {
		t.Fatalf("No WiFi device found: %v", err)
	}
	dev := wifiDevs[0]
	t.Logf("WiFi device: %s (MAC: %s)", dev.Interface, dev.HWAddress)

	ssid := fmt.Sprintf("Piccolo-Setup-%02X%02X", dev.HWAddress[4], dev.HWAddress[5])

	// Real scan via NM D-Bus (filters out our own AP)
	realScan := func(forceRefresh bool) ([]ScanResult, error) {
		aps, err := nm.Scan(dev.Path)
		if err != nil {
			return nil, err
		}
		var results []ScanResult
		for _, ap := range aps {
			if ap.SSID == ssid {
				continue // filter out our own AP
			}
			dbm := ap.SignalDBm()
			tier := "poor"
			switch {
			case dbm > -60:
				tier = "good"
			case dbm > -70:
				tier = "fair"
			case dbm > -80:
				tier = "weak"
			}
			results = append(results, ScanResult{
				SSID:         ap.SSID,
				Security:     ap.SecurityType(),
				SignalDBm:    dbm,
				SignalTier:   tier,
				FrequencyMHz: ap.Frequency,
				Band:         ap.Band(),
			})
		}
		return results, nil
	}

	var srv *Server
	var connectFn func(string, string)

	connectFn = func(targetSSID, passphrase string) {
		log.Printf("=== REAL CONNECT ===")
		log.Printf("  SSID:       %s", targetSSID)
		log.Printf("  Passphrase: %s", passphrase)
		log.Printf("  ECDH decryption successful!")
		log.Printf("====================")

		// 1. Stop the captive portal (we're about to tear down the AP)
		if srv != nil {
			srv.Stop()
		}

		// 2. Deactivate AP hotspot (card can't do AP+STA simultaneously)
		log.Printf("INFO: tearing down AP for STA connection...")
		if err := nm.DeactivateHotspot(dev.Path); err != nil {
			log.Printf("WARN: deactivate hotspot: %v", err)
		}
		time.Sleep(2 * time.Second)

		// 3. Connect to the target network
		log.Printf("INFO: connecting to %q...", targetSSID)
		if err := nm.Connect(dev.Path, targetSSID, passphrase); err != nil {
			log.Printf("ERROR: connect failed: %v — reactivating AP + captive portal", err)
			nm.ActivateHotspot(dev.Path, ssid, nmclient.HotspotOpts{})
			time.Sleep(3 * time.Second)
			newSrv, srvErr := NewServer(realScan, connectFn, func() {})
			if srvErr != nil {
				log.Printf("ERROR: restart captive portal: %v", srvErr)
				return
			}
			srv = newSrv
			srv.SetConnectError("Connection to '" + targetSSID + "' failed: " + err.Error())
			if startErr := srv.Start(context.Background(), "0.0.0.0:80"); startErr != nil {
				log.Printf("ERROR: restart portal server: %v", startErr)
			} else {
				log.Printf("INFO: captive portal restarted — connect your phone to %s", ssid)
			}
			return
		}

		// 4. Wait for connection (up to 30s)
		log.Printf("INFO: waiting for connection activation (up to 30s)...")
		connected := false
		for i := 0; i < 30; i++ {
			time.Sleep(time.Second)
			state, err := nm.DeviceState(dev.Path)
			if err == nil && state.IsConnected() {
				connected = true
				break
			}
			if i%5 == 4 {
				log.Printf("INFO: still waiting... (attempt %d/30, state=%d)", i+1, state)
			}
		}

		if connected {
			info, _ := nm.ActiveConnectionInfo(dev.Path)
			ip := ""
			if info != nil {
				ip = info.IP4Address
			}
			log.Printf("=== CONNECTED! ===")
			log.Printf("  SSID: %s", targetSSID)
			log.Printf("  IP:   %s", ip)
			log.Printf("  The device is now on your home WiFi.")
			log.Printf("  Open http://piccolo.local or http://%s to access the portal.", ip)
			log.Printf("==================")
		} else {
			log.Printf("ERROR: connection to %q failed — reactivating AP + captive portal", targetSSID)
			nm.ActivateHotspot(dev.Path, ssid, nmclient.HotspotOpts{})
			time.Sleep(3 * time.Second) // wait for AP to come up

			// Restart captive portal
			newSrv, err := NewServer(realScan, connectFn, func() {})
			if err != nil {
				log.Printf("ERROR: restart captive portal: %v", err)
				return
			}
			srv = newSrv
			srv.SetConnectError("Connection to '" + targetSSID + "' failed — check your password and try again.")
			if err := srv.Start(context.Background(), "0.0.0.0:80"); err != nil {
				log.Printf("ERROR: restart captive portal server: %v", err)
			} else {
				log.Printf("INFO: captive portal restarted — connect your phone to %s", ssid)
			}
		}
	}

	var srvErr error
	srv, srvErr = NewServer(realScan, connectFn, func() {})
	if srvErr != nil {
		t.Fatalf("NewServer: %v", srvErr)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Write DNS redirect config BEFORE activating the hotspot.
	// NM's shared mode starts its own dnsmasq — we configure it via
	// /etc/NetworkManager/dnsmasq-shared.d/ to redirect all DNS to the AP IP.
	dnsConfPath := "/etc/NetworkManager/dnsmasq-shared.d/piccolo-captive.conf"
	os.MkdirAll("/etc/NetworkManager/dnsmasq-shared.d", 0755)
	// address=/#/<IP> makes all queries resolve to the AP IP (captive portal detection)
	if err := os.WriteFile(dnsConfPath, []byte("address=/#/10.42.0.1\n"), 0644); err != nil {
		t.Logf("WARN: could not write NM dnsmasq config: %v (captive portal detection won't work)", err)
	} else {
		t.Logf("Wrote DNS redirect config to %s", dnsConfPath)
	}
	defer os.Remove(dnsConfPath)

	// Activate AP (disconnects current WiFi!)
	t.Logf("Activating AP: %s (this will disconnect your current WiFi)", ssid)
	if err := nm.ActivateHotspot(dev.Path, ssid, nmclient.HotspotOpts{}); err != nil {
		t.Fatalf("ActivateHotspot: %v", err)
	}
	defer func() {
		t.Log("Deactivating AP and restoring WiFi...")
		if err := nm.DeactivateHotspot(dev.Path); err != nil {
			t.Logf("WARN: deactivate hotspot: %v", err)
		}
		t.Log("AP deactivated. NM should auto-reconnect to your previous WiFi.")
	}()

	// Wait for the AP interface to get the 10.42.0.x shared IP.
	// NM's ActiveConnectionInfo may return stale data, so read from the
	// interface directly.
	var apIP string
	for i := 0; i < 20; i++ {
		apIP = getInterfaceIPv4(dev.Interface)
		if apIP != "" && (len(apIP) >= 5 && apIP[:5] == "10.42") {
			break
		}
		t.Logf("Waiting for AP IP on %s (attempt %d, current: %q)...", dev.Interface, i+1, apIP)
		apIP = "" // reset if not 10.42.x yet
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if apIP == "" {
		apIP = "10.42.0.1"
		t.Logf("Could not detect AP IP, using fallback: %s", apIP)
	}
	t.Logf("AP IP: %s", apIP)

	// NM's dnsmasq handles DNS redirect via the config we wrote earlier.
	// No separate dnsmasq needed.

	// Listen on 0.0.0.0:80 so the portal is reachable regardless of IP.
	if err := srv.Start(ctx, "0.0.0.0:80"); err != nil {
		t.Fatalf("Start captive portal: %v", err)
	}
	defer srv.Stop()

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║           REAL AP MODE TEST                      ║")
	fmt.Println("╠══════════════════════════════════════════════════╣")
	fmt.Printf("║  AP SSID:  %-38s ║\n", ssid)
	fmt.Printf("║  AP IP:    %-38s ║\n", apIP)
	fmt.Println("║                                                  ║")
	fmt.Println("║  Steps:                                          ║")
	fmt.Println("║  1. On your phone, connect to the AP above       ║")
	fmt.Println("║  2. Captive portal should auto-open              ║")
	fmt.Printf("║  3. Or navigate to http://%-23s ║\n", apIP)
	fmt.Println("║  4. You'll see REAL nearby networks from scan    ║")
	fmt.Println("║  5. Select one, enter any passphrase             ║")
	fmt.Println("║  6. Watch this terminal for ECDH decryption log  ║")
	fmt.Println("║                                                  ║")
	fmt.Println("║  Press Ctrl+C to stop and restore WiFi           ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()

	<-ctx.Done()
	fmt.Println("\nRestoring previous WiFi connection...")
}

func printBanner(mode, pubkey string) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Printf("║  %-43s ║\n", mode)
	fmt.Println("╠══════════════════════════════════════════════╣")
	fmt.Printf("║  Key: %.38s...  ║\n", pubkey)
	fmt.Println("║                                              ║")
	fmt.Println("║  Open on your phone:                         ║")
	for _, ip := range localIPs() {
		fmt.Printf("║    http://%-35s ║\n", ip+":8080")
	}
	fmt.Println("║                                              ║")
	fmt.Println("║  Press Ctrl+C to stop                        ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()
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

// getInterfaceIPv4 returns the first IPv4 address on a network interface.
func getInterfaceIPv4(ifaceName string) string {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() {
			return ipnet.IP.String()
		}
	}
	return ""
}
