package middleware

import "net"

// EffectiveSourceIP resolves the effective client IP for an L4 connection.
//
// Algorithm:
//
//  1. If SourceTrust == TrustedLoopback, attempt the lazy hint lookup. If a
//     hint is present and Hint.ClientIP is non-empty and parses as a valid IP,
//     return its IPv4 form (via .To4() if IPv4-mapped IPv6, else as-is).
//
//  2. Empty/malformed Hint.ClientIP OR SourceTrust == Direct: extract the IP
//     from SourceAddr. Supports *net.TCPAddr and *net.UDPAddr; other concrete
//     types return nil. IPv4-mapped IPv6 (::ffff:a.b.c.d) is canonicalized to
//     its v4 form so CIDR matches written as 192.0.2.0/24 work for both v4
//     and IPv4-mapped-v6 sources.
//
// Returns nil only when SourceAddr is nil or an unsupported concrete type.
// Caller treats nil as fail-closed.
//
// IMPORTANT: on TrustedLoopback connections WITHOUT a registered hint, this
// returns the loopback SourceAddr (127.0.0.1 / ::1). This is intentional —
// the legacy proxy paths relied on it. Operators wanting fail-closed
// behavior in this case MUST add an explicit `127.0.0.0/8` deny rule (and
// the IPv6 loopback equivalent) to their `ConnectionAuth` /
// `ip_allowlist` rules, or use `Default: deny`.
//
// All IP-rule middlewares (connection_auth, ip_allowlist, ip_rate_limit) MUST
// use this helper rather than reading ConnContext.SourceAddr or invoking
// ctx.Hint directly. Single resolution site, single bug surface.
func EffectiveSourceIP(ctx ConnContext) net.IP {
	if ctx.SourceTrust == TrustedLoopback && ctx.Hint != nil {
		if hint, ok := ctx.Hint(); ok && hint.ClientIP != "" {
			if ip := net.ParseIP(hint.ClientIP); ip != nil {
				return CanonicalIP(ip)
			}
			// Malformed ClientIP on a TrustedLoopback connection: no authoritative
			// real-client IP available. Returning the loopback SourceAddr would let
			// 127.0.0.0/8 deny rules block legitimate Nexus traffic; falling through
			// keeps the same behavior. This is the documented fail-closed path.
		}
	}

	switch a := ctx.SourceAddr.(type) {
	case *net.TCPAddr:
		if a == nil {
			return nil
		}
		return CanonicalIP(a.IP)
	case *net.UDPAddr:
		if a == nil {
			return nil
		}
		return CanonicalIP(a.IP)
	default:
		return nil
	}
}

// CanonicalIP reduces IPv4-mapped IPv6 (::ffff:a.b.c.d) to its 4-byte v4 form so
// a CIDR like 192.0.2.0/24 matches both natively-v4 and IPv4-mapped-v6 sources.
// Pure IPv6 addresses are returned unchanged.
//
// Exported so UDP rule middlewares (which can't go through EffectiveSourceIP
// — UDPContext has no Hint) can apply the same v4/v6 normalization on
// ctx.Source.IP.
func CanonicalIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}
