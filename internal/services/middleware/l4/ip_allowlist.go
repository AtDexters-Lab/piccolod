package l4

import (
	"fmt"
	"net"

	"piccolod/internal/services/middleware"
)

// ipAllowlistConfig is the parsed shape of an ip_allowlist params block.
//
// Evaluation order (per plan §D14):
//  1. If SourceIP matches any Deny CIDR → deny.
//  2. Else if SourceIP matches any Allow CIDR → allow.
//  3. Else fall through to Default.
//
// Default values: "deny" (fail-closed). Operator MUST opt in to "allow" by
// listing it explicitly to avoid an accidental world-open listener from a
// misconfigured params block.
type ipAllowlistConfig struct {
	Allow   []*net.IPNet
	Deny    []*net.IPNet
	Default string // "allow" or "deny"
}

func parseIPAllowlistParams(params map[string]any) (*ipAllowlistConfig, error) {
	cfg := &ipAllowlistConfig{Default: "deny"}

	allow, err := parseCIDRList(params, "allow")
	if err != nil {
		return nil, fmt.Errorf("ip_allowlist: %w", err)
	}
	cfg.Allow = allow

	deny, err := parseCIDRList(params, "deny")
	if err != nil {
		return nil, fmt.Errorf("ip_allowlist: %w", err)
	}
	cfg.Deny = deny

	if rawDef, ok := params["default"]; ok {
		def, ok := rawDef.(string)
		if !ok {
			return nil, fmt.Errorf("ip_allowlist: default: expected string, got %T", rawDef)
		}
		switch def {
		case "allow", "deny":
			cfg.Default = def
		default:
			return nil, fmt.Errorf("ip_allowlist: default: must be \"allow\" or \"deny\", got %q", def)
		}
	}

	return cfg, nil
}

func parseCIDRList(params map[string]any, key string) ([]*net.IPNet, error) {
	raw, ok := params[key]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected list, got %T", key, raw)
	}
	out := make([]*net.IPNet, 0, len(list))
	for i, entry := range list {
		s, ok := entry.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%d]: expected string, got %T", key, i, entry)
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: invalid CIDR %q: %w", key, i, s, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// ipAllowlistDecide evaluates the configured allow/deny/default policy
// against an effective source IP. Returns true when the connection is
// allowed; false denies.
func ipAllowlistDecide(cfg *ipAllowlistConfig, ip net.IP) bool {
	if ip == nil {
		// No resolvable IP → fail-closed: deny regardless of Default.
		// Default=allow is a "no rules matched" verdict; an unknown source
		// shouldn't bypass an explicit policy by being unidentifiable.
		return false
	}
	for _, n := range cfg.Deny {
		if n.Contains(ip) {
			return false
		}
	}
	for _, n := range cfg.Allow {
		if n.Contains(ip) {
			return true
		}
	}
	return cfg.Default == "allow"
}

// IPAllowlist constructs an L4 (TCP) ip_allowlist middleware. The chain
// signals deny by returning without calling next; the chain entry
// (l4AcceptBridge for HTTP listeners, the dispatch helper in startTCPProxy
// for raw/TLS listeners) closes the conn on deny. The metrics registry
// receives a deny event with reason "ip_allowlist".
func IPAllowlist(params map[string]any, metrics *MetricsRegistry) (middleware.L4Middleware, error) {
	cfg, err := parseIPAllowlistParams(params)
	if err != nil {
		return nil, err
	}
	return func(next middleware.ConnHandler) middleware.ConnHandler {
		return func(ctx middleware.ConnContext, c net.Conn) {
			ip := middleware.EffectiveSourceIP(ctx)
			if ipAllowlistDecide(cfg, ip) {
				next(ctx, c)
				return
			}
			recordDeny(metrics, ctx.Endpoint.App, ctx.Endpoint.Listener, ip, "ip_allowlist")
		}
	}, nil
}

// IPAllowlistUDP constructs the L4UDP analogue. Denied datagrams are dropped
// silently (UDP has no connection to close); metrics record the deny event.
func IPAllowlistUDP(params map[string]any, metrics *MetricsRegistry) (middleware.L4UDPMiddleware, error) {
	cfg, err := parseIPAllowlistParams(params)
	if err != nil {
		return nil, err
	}
	return func(next middleware.UDPHandler) middleware.UDPHandler {
		return func(ctx middleware.UDPContext, payload []byte, sink middleware.UDPSink) {
			ip := middleware.EffectiveSourceIPUDP(ctx)
			if ipAllowlistDecide(cfg, ip) {
				next(ctx, payload, sink)
				return
			}
			recordDeny(metrics, ctx.Endpoint.App, ctx.Endpoint.Listener, ip, "ip_allowlist")
		}
	}, nil
}

func recordDeny(metrics *MetricsRegistry, app, listener string, ip net.IP, reason string) {
	if metrics == nil {
		return
	}
	metrics.RecordDenied(app, listener, ipStringOrUnknown(ip), reason)
}
