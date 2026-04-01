package ap

import (
	"fmt"
	"log"
	"os"
)

const (
	// NM's shared hotspot mode starts its own dnsmasq for DHCP + DNS.
	// We configure it to redirect all DNS queries to the AP IP by dropping
	// a config file into this directory BEFORE activating the hotspot.
	nmDnsmasqSharedDir  = "/etc/NetworkManager/dnsmasq-shared.d"
	nmDnsmasqConfFile   = nmDnsmasqSharedDir + "/piccolo-captive.conf"

	// Legacy standalone dnsmasq paths (for cleanup/reconciliation only)
	legacyDnsmasqPIDFile  = "/run/piccolo/dnsmasq-ap.pid"
	legacyDnsmasqConfFile = "/run/piccolo/dnsmasq-ap.conf"
)

// writeDNSRedirectConfig writes a dnsmasq config that redirects all DNS
// queries to the AP IP. This must be called BEFORE activating the NM hotspot
// so that NM's dnsmasq picks up the config on startup.
func writeDNSRedirectConfig(apIP string) error {
	if err := os.MkdirAll(nmDnsmasqSharedDir, 0o755); err != nil {
		return fmt.Errorf("create dnsmasq-shared.d: %w", err)
	}

	// address=/#/<IP> tells dnsmasq to resolve ALL queries to this IP.
	// This triggers captive portal detection on phones (connectivity probes
	// resolve to the portal IP instead of the real server).
	conf := fmt.Sprintf("address=/#/%s\n", apIP)
	if err := os.WriteFile(nmDnsmasqConfFile, []byte(conf), 0o644); err != nil {
		return fmt.Errorf("write dnsmasq config: %w", err)
	}

	log.Printf("INFO: ap: wrote DNS redirect config → %s", apIP)
	return nil
}

// removeDNSRedirectConfig removes the captive portal DNS redirect config.
func removeDNSRedirectConfig() {
	if err := os.Remove(nmDnsmasqConfFile); err != nil && !os.IsNotExist(err) {
		log.Printf("WARN: ap: remove DNS config: %v", err)
	}
}
