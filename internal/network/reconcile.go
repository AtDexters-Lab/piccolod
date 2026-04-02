package network

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"piccolod/internal/network/nmclient"
	"piccolod/internal/runner"
)

const (
	dnsmasqPIDFile = "/run/piccolo/dnsmasq-ap.pid"
	apFirewallZone = "piccolo-ap"
)

// reconcileStartup cleans up stale state from a previous piccolod instance
// that may have crashed during AP mode or STA validation.
func reconcileStartup(ctx context.Context, nm nmclient.Client, r runner.CommandRunner) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	killOrphanDnsmasq()
	cleanStaleDNSRedirectConfig()
	cleanStaleAPFirewall(timeoutCtx, r)
	cleanStaleSTAValidationRules(timeoutCtx, r)
	cleanStaleHotspot(nm)
}

// killOrphanDnsmasq checks for a stale PID file and kills any orphan dnsmasq.
func killOrphanDnsmasq() {
	data, err := os.ReadFile(dnsmasqPIDFile)
	if err != nil {
		return // no PID file
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		os.Remove(dnsmasqPIDFile)
		return
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		os.Remove(dnsmasqPIDFile)
		return
	}

	// Check if the process is actually a dnsmasq
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		// Process doesn't exist
		os.Remove(dnsmasqPIDFile)
		return
	}
	if !strings.Contains(string(cmdline), "dnsmasq") {
		// PID was reused by a different process
		os.Remove(dnsmasqPIDFile)
		return
	}

	log.Printf("INFO: network: killing orphan dnsmasq (pid=%d)", pid)
	_ = proc.Kill()
	os.Remove(dnsmasqPIDFile)
}

// cleanStaleDNSRedirectConfig removes the NM dnsmasq-shared.d config that
// redirects all DNS to the AP IP. Left behind if piccolod crashes during AP mode.
func cleanStaleDNSRedirectConfig() {
	const confPath = "/etc/NetworkManager/dnsmasq-shared.d/piccolo-captive.conf"
	if _, err := os.Stat(confPath); err != nil {
		return // doesn't exist
	}
	log.Printf("INFO: network: removing stale DNS redirect config %s", confPath)
	os.Remove(confPath)
}

// cleanStaleAPFirewall removes leftover firewalld rules from the piccolo-ap zone.
func cleanStaleAPFirewall(ctx context.Context, r runner.CommandRunner) {
	// Check if the zone exists and has rules
	out, err := r.RunWithOutput(ctx, "firewall-cmd", "--zone="+apFirewallZone, "--list-all")
	if err != nil {
		return // zone doesn't exist or firewall-cmd not available
	}
	content := string(out)
	if content == "" {
		return
	}

	// Check for active rules (services, ports, forward-ports)
	servicesLine := strings.TrimSpace(extractAfter(content, "services:"))
	hasRules := servicesLine != ""
	hasForward := strings.Contains(content, "forward-ports:") && strings.Contains(content, "port=")

	if !hasRules && !hasForward {
		return
	}

	log.Printf("INFO: network: cleaning stale AP firewall rules from zone %s", apFirewallZone)

	// Remove all runtime rules from the AP zone
	_ = r.Run(ctx, "firewall-cmd", "--zone="+apFirewallZone, "--remove-service=dhcp")
	_ = r.Run(ctx, "firewall-cmd", "--zone="+apFirewallZone, "--remove-service=dns")
	_ = r.Run(ctx, "firewall-cmd", "--zone="+apFirewallZone, "--remove-port=80/tcp")
	// Forward-port rules need the full spec to remove; just reload to clear runtime rules
	_ = r.Run(ctx, "firewall-cmd", "--reload")
}

// cleanStaleSTAValidationRules removes leftover DHCP+ICMP-only rich rules
// from the piccolo zone that would persist if piccolod crashed during STA
// validation.
func cleanStaleSTAValidationRules(ctx context.Context, r runner.CommandRunner) {
	out, err := r.RunWithOutput(ctx, "firewall-cmd", "--zone=piccolo", "--list-rich-rules")
	if err != nil {
		return
	}

	rules := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, rule := range rules {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		// STA validation rules match patterns like:
		// rule family="ipv4" service name="dhcp" accept
		// rule family="ipv4" protocol value="icmp" accept
		if (strings.Contains(rule, `service name="dhcp"`) || strings.Contains(rule, `protocol value="icmp"`)) &&
			strings.Contains(rule, "accept") {
			log.Printf("INFO: network: removing stale STA validation rule: %s", rule)
			_ = r.Run(ctx, "firewall-cmd", "--zone=piccolo", "--remove-rich-rule="+rule)
		}
	}
}

// cleanStaleHotspot deactivates any leftover NM hotspot connections.
func cleanStaleHotspot(nm nmclient.Client) {
	wifiDevs, err := nm.WiFiDevices()
	if err != nil {
		return
	}
	for _, dev := range wifiDevs {
		info, err := nm.ActiveConnectionInfo(dev.Path)
		if err != nil || info == nil {
			continue
		}
		if strings.HasPrefix(info.ID, nmclient.HotspotIDPrefix) {
			log.Printf("INFO: network: deactivating stale hotspot %q on %s", info.ID, dev.Interface)
			_ = nm.DeactivateHotspot(dev.Path)
		}
	}
}

// extractAfter returns the content after the first occurrence of prefix in s.
func extractAfter(s, prefix string) string {
	idx := strings.Index(s, prefix)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(prefix):]
	// Take until end of line
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	return strings.TrimSpace(rest)
}
