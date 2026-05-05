package l4

import (
	"fmt"
	"net"

	"piccolod/internal/api"
	"piccolod/internal/services/middleware"
)

// connectionAuthRule is the parsed (CIDR-resolved) form of an
// api.ConnectionAuthRule. Stored once at chain-build time so per-conn
// evaluation skips parsing.
type connectionAuthRule struct {
	cidr     *net.IPNet
	strategy string // "allow" | "deny"
}

// parseConnectionAuth resolves the api shape into per-rule CIDRs ready for
// evaluation. Returns the rule list + the resolved Default ("allow" | "deny";
// nil and explicit Default="" both default to "allow" per plan §D6).
func parseConnectionAuth(cfg *api.ConnectionAuth) ([]connectionAuthRule, string, error) {
	if cfg == nil {
		return nil, "allow", nil
	}
	def := cfg.Default
	if def == "" {
		def = "allow"
	}
	if def != "allow" && def != "deny" {
		return nil, "", fmt.Errorf("connection_auth: default must be \"allow\" or \"deny\", got %q", def)
	}
	rules := make([]connectionAuthRule, 0, len(cfg.Rules))
	for i, r := range cfg.Rules {
		if r.Strategy != "allow" && r.Strategy != "deny" {
			return nil, "", fmt.Errorf("connection_auth: rules[%d].strategy must be \"allow\" or \"deny\", got %q", i, r.Strategy)
		}
		_, n, err := net.ParseCIDR(r.Match)
		if err != nil {
			return nil, "", fmt.Errorf("connection_auth: rules[%d].match: invalid CIDR %q: %w", i, r.Match, err)
		}
		rules = append(rules, connectionAuthRule{cidr: n, strategy: r.Strategy})
	}
	return rules, def, nil
}

// connectionAuthDecide walks the rule list (first-match-wins) and falls
// through to default when no rule matches. Returns true to allow.
//
// Unresolvable source IP → fail-closed (deny) regardless of default —
// mirrors ip_allowlist semantics: an unidentifiable source can't bypass an
// explicit policy by being unidentifiable.
func connectionAuthDecide(rules []connectionAuthRule, def string, ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, r := range rules {
		if r.cidr.Contains(ip) {
			return r.strategy == "allow"
		}
	}
	return def == "allow"
}

// ConnectionAuth constructs an L4 (TCP) connection_auth middleware from the
// listener's typed ConnectionAuth field. Denied connections are closed and
// the metrics registry receives a deny event with reason "connection_auth".
//
// Composes via the registry as a CONDITIONALLY-CANONICAL entry — runs only
// when the listener's ConnectionAuth field is non-nil (gated via
// spec.HasConnectionAuth in registry.canonicalApplies). Per plan §D6.
func ConnectionAuth(cfg *api.ConnectionAuth, metrics *MetricsRegistry) (middleware.L4Middleware, error) {
	rules, def, err := parseConnectionAuth(cfg)
	if err != nil {
		return nil, err
	}
	return func(next middleware.ConnHandler) middleware.ConnHandler {
		return func(ctx middleware.ConnContext, c net.Conn) {
			ip := middleware.EffectiveSourceIP(ctx)
			if connectionAuthDecide(rules, def, ip) {
				next(ctx, c)
				return
			}
			recordDeny(metrics, ctx.Endpoint.Listener, ip, "connection_auth")
		}
	}, nil
}

// ConnectionAuthUDP constructs the L4UDP analogue.
func ConnectionAuthUDP(cfg *api.ConnectionAuth, metrics *MetricsRegistry) (middleware.L4UDPMiddleware, error) {
	rules, def, err := parseConnectionAuth(cfg)
	if err != nil {
		return nil, err
	}
	return func(next middleware.UDPHandler) middleware.UDPHandler {
		return func(ctx middleware.UDPContext, payload []byte, sink middleware.UDPSink) {
			var ip net.IP
			if ctx.Source != nil {
				ip = ctx.Source.IP
			}
			if connectionAuthDecide(rules, def, ip) {
				next(ctx, payload, sink)
				return
			}
			recordDeny(metrics, ctx.Endpoint.Listener, ip, "connection_auth")
		}
	}, nil
}
