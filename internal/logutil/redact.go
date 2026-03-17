package logutil

import (
	"net"
	"regexp"
)

// IPv4 candidate pattern — intentionally broad, validated by net.ParseIP.
// Matches 1-3 digits . 1-3 digits . 1-3 digits . 1-3 digits, optional :port.
var ipv4Re = regexp.MustCompile(`\b(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})(:\d+)?\b`)

// IPv6 candidate patterns — intentionally broad, validated by net.ParseIP to
// avoid false positives on timestamps (10:30:00) and other colon-separated values.
var ipv6CandidateRe = regexp.MustCompile(`(?i)` +
	// 3-8 colon-separated hex groups (no ::)
	`\b[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){2,7}\b` +
	`|` +
	// :: compressed forms: optional groups before ::, optional groups after
	`(?:\b[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)?::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)?\b`)

// shouldPreserveIP returns true for addresses that carry no device-identifying
// information: loopback, unspecified, link-local, and multicast.
func shouldPreserveIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast()
}

// isSubnetMask returns true if ip is a valid IPv4 CIDR subnet mask:
// contiguous 1-bits followed by contiguous 0-bits (e.g. 255.255.255.0).
// Returns false for 0.0.0.0 (handled separately by IsUnspecified).
func isSubnetMask(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	// A valid mask in binary is 111...1000...0. Inverting gives 000...0111...1.
	// If inverted+1 is a power of two, then inverted & (inverted+1) == 0.
	n := uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3])
	if n == 0 {
		return false
	}
	inverted := ^n
	return inverted&(inverted+1) == 0
}

// Redact applies defense-in-depth redaction to journal output.
// It redacts client IP addresses while preserving addresses that carry no
// device-identifying information: loopback (127.0.0.0/8, ::1), unspecified
// (0.0.0.0, ::), link-local (169.254.0.0/16, fe80::/10), multicast
// (224.0.0.0/4, ff00::/8), and IPv4 subnet masks (255.255.255.0, etc.).
//
// Note: IPv6 link-local (fe80::/10) with EUI-64 embeds the MAC address in the
// lower 64 bits. This is acceptable because diagnostic logs are only accessible
// to the device owner (admin-authenticated or LAN-only emergency endpoint).
func Redact(data []byte) []byte {
	// Redact IPv4 first — handles bare addresses and the IPv4 portion of
	// mapped IPv6 (::ffff:192.168.1.50 → ::ffff:<IPv4>) since the hex-only
	// IPv6 regex cannot capture the dotted decimal tail.
	data = ipv4Re.ReplaceAllFunc(data, func(match []byte) []byte {
		sub := ipv4Re.FindSubmatch(match)
		if sub == nil {
			return match
		}
		ipStr := string(sub[1])
		port := string(sub[2])
		parsed := net.ParseIP(ipStr)
		if parsed == nil {
			return match // not a valid IP, leave unchanged
		}
		if shouldPreserveIP(parsed) || isSubnetMask(parsed) {
			return match
		}
		return []byte("<IPv4>" + port)
	})

	// Redact IPv6 — runs after IPv4 so mapped addresses are already handled.
	data = ipv6CandidateRe.ReplaceAllFunc(data, func(match []byte) []byte {
		s := string(match)
		ip := net.ParseIP(s)
		if ip == nil {
			return match // not valid IPv6 (e.g. timestamp), leave unchanged
		}
		if shouldPreserveIP(ip) {
			return match
		}
		return []byte("<IPv6>")
	})

	return data
}
