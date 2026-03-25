package firewall

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
)

// Rule describes a port to open or close in the firewall.
type Rule struct {
	Port     int    // well-known port number
	Protocol string // "tcp" or "udp"
}

// Manager manages runtime firewalld rules for port claims.
type Manager interface {
	OpenPort(rule Rule) error
	ClosePort(rule Rule) error
}

var rfc1918Subnets = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
}

// FirewalldManager manages runtime-only rich rules via firewall-cmd.
// Rules are not permanent — they vanish on reboot. The service manager
// re-applies them during endpoint restoration on boot.
type FirewalldManager struct {
	zone string
}

// NewFirewalldManager returns a firewalld-backed Manager if firewall-cmd is
// available, otherwise returns a no-op stub.
func NewFirewalldManager() Manager {
	if _, err := exec.LookPath("firewall-cmd"); err != nil {
		log.Printf("INFO: firewall-cmd not found, using no-op firewall stub")
		return &stubManager{}
	}
	return &FirewalldManager{zone: "piccolo"}
}

func (m *FirewalldManager) richRule(rule Rule, subnet string) string {
	return fmt.Sprintf(
		`rule family="ipv4" source address="%s" port port="%d" protocol="%s" accept`,
		subnet, rule.Port, rule.Protocol,
	)
}

func (m *FirewalldManager) applyRule(rule Rule, action string) error {
	flag := "--add-rich-rule="
	if action == "remove" {
		flag = "--remove-rich-rule="
	}
	type result struct {
		subnet string
		output string
		err    error
	}
	results := make([]result, len(rfc1918Subnets))
	var wg sync.WaitGroup
	for i, subnet := range rfc1918Subnets {
		wg.Add(1)
		go func(i int, subnet string) {
			defer wg.Done()
			rr := m.richRule(rule, subnet)
			cmd := exec.Command("firewall-cmd", "--zone="+m.zone, flag+rr)
			out, err := cmd.CombinedOutput()
			results[i] = result{subnet: subnet, output: strings.TrimSpace(string(out)), err: err}
		}(i, subnet)
	}
	wg.Wait()
	var errs []string
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s (%v)", r.subnet, r.output, r.err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("firewall %s port %d/%s: %s", action, rule.Port, rule.Protocol, strings.Join(errs, "; "))
	}
	return nil
}

// OpenPort adds runtime rich rules for all RFC1918 subnets.
func (m *FirewalldManager) OpenPort(rule Rule) error { return m.applyRule(rule, "add") }

// ClosePort removes runtime rich rules for all RFC1918 subnets.
func (m *FirewalldManager) ClosePort(rule Rule) error { return m.applyRule(rule, "remove") }

// stubManager is a no-op fallback when firewall-cmd is not available.
type stubManager struct{}

func (s *stubManager) OpenPort(rule Rule) error {
	log.Printf("INFO: firewall stub: open port %d/%s (no-op)", rule.Port, rule.Protocol)
	return nil
}

func (s *stubManager) ClosePort(rule Rule) error {
	log.Printf("INFO: firewall stub: close port %d/%s (no-op)", rule.Port, rule.Protocol)
	return nil
}
