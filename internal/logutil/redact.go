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

// Redact applies defense-in-depth redaction to journal output.
// It redacts client IP addresses while preserving loopback and unspecified
// addresses (127.0.0.0/8, 0.0.0.0, ::1, ::) which are safe and useful for debugging.
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
		if parsed.IsLoopback() || parsed.IsUnspecified() {
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
		if ip.IsLoopback() || ip.IsUnspecified() {
			return match
		}
		return []byte("<IPv6>")
	})

	return data
}
