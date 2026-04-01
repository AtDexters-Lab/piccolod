// Package ap manages the WiFi AP mode lifecycle: NM hotspot activation,
// dnsmasq DNS redirect, firewalld zone rules, and captive portal server.
package ap

import (
	"context"
	"fmt"
	"log"

	"piccolod/internal/runner"
)

const apZone = "piccolo-ap"

// firewallManager manages firewalld rules for the AP zone.
type firewallManager struct {
	runner runner.CommandRunner
}

// ensureZone creates the piccolo-ap zone if it doesn't exist.
func (f *firewallManager) ensureZone(ctx context.Context) error {
	// Check if zone exists
	if err := f.runner.Run(ctx, "firewall-cmd", "--info-zone="+apZone); err == nil {
		return nil // already exists
	}

	// Create permanent zone
	if err := f.runner.Run(ctx, "firewall-cmd", "--permanent", "--new-zone="+apZone); err != nil {
		return fmt.Errorf("create zone %s: %w", apZone, err)
	}
	// Reload to pick up new zone
	if err := f.runner.Run(ctx, "firewall-cmd", "--reload"); err != nil {
		return fmt.Errorf("reload after zone creation: %w", err)
	}
	log.Printf("INFO: ap: created firewalld zone %s", apZone)
	return nil
}

// assignInterface moves an interface into the AP zone.
func (f *firewallManager) assignInterface(ctx context.Context, iface string) error {
	return f.runner.Run(ctx, "firewall-cmd", "--zone="+apZone, "--change-interface="+iface)
}

// verifyZoneAssignment checks that an interface is in the AP zone.
func (f *firewallManager) verifyZoneAssignment(ctx context.Context, iface string) bool {
	out, err := f.runner.RunWithOutput(ctx, "firewall-cmd", "--get-zone-of-interface="+iface)
	if err != nil {
		return false
	}
	return string(out) == apZone+"\n" || string(out) == apZone
}

// applyRules adds the AP-mode firewall rules: DHCP, DNS, HTTP.
func (f *firewallManager) applyRules(ctx context.Context) error {
	rules := [][]string{
		{"--zone=" + apZone, "--add-service=dhcp"},
		{"--zone=" + apZone, "--add-service=dns"},
		{"--zone=" + apZone, "--add-port=80/tcp"},
	}

	for _, args := range rules {
		if err := f.runner.Run(ctx, "firewall-cmd", args...); err != nil {
			return fmt.Errorf("apply firewall rule %v: %w", args, err)
		}
	}
	return nil
}

// addNATRedirect adds a port forward from 80 to captivePort on the AP zone.
func (f *firewallManager) addNATRedirect(ctx context.Context, captivePort int) error {
	rule := fmt.Sprintf("--add-forward-port=port=80:proto=tcp:toport=%d", captivePort)
	return f.runner.Run(ctx, "firewall-cmd", "--zone="+apZone, rule)
}

// removeRules removes all runtime rules from the AP zone.
func (f *firewallManager) removeRules(ctx context.Context) {
	_ = f.runner.Run(ctx, "firewall-cmd", "--zone="+apZone, "--remove-service=dhcp")
	_ = f.runner.Run(ctx, "firewall-cmd", "--zone="+apZone, "--remove-service=dns")
	_ = f.runner.Run(ctx, "firewall-cmd", "--zone="+apZone, "--remove-port=80/tcp")
	// Reload clears forward-port rules (easier than specifying the full rule)
	_ = f.runner.Run(ctx, "firewall-cmd", "--reload")
}

// applySTAValidationLockdown restricts the WiFi interface to DHCP + ICMP only
// during the STA validation window.
func (f *firewallManager) applySTAValidationLockdown(ctx context.Context) error {
	rules := []string{
		`rule family="ipv4" service name="dhcp" accept`,
		`rule family="ipv4" protocol value="icmp" accept`,
	}
	for _, rule := range rules {
		if err := f.runner.Run(ctx, "firewall-cmd", "--zone=piccolo", "--add-rich-rule="+rule); err != nil {
			return fmt.Errorf("STA lockdown rule: %w", err)
		}
	}
	return nil
}

// removeSTAValidationLockdown removes the STA validation firewall rules.
func (f *firewallManager) removeSTAValidationLockdown(ctx context.Context) {
	rules := []string{
		`rule family="ipv4" service name="dhcp" accept`,
		`rule family="ipv4" protocol value="icmp" accept`,
	}
	for _, rule := range rules {
		_ = f.runner.Run(ctx, "firewall-cmd", "--zone=piccolo", "--remove-rich-rule="+rule)
	}
}
