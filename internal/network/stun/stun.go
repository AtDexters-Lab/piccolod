// Package stun provides a minimal STUN client for public IP discovery.
// Implements only the Binding Request (RFC 5389) with XOR-MAPPED-ADDRESS parsing.
package stun

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	stunMagicCookie = 0x2112A442
	attrXORMapped   = 0x0020
	attrMapped      = 0x0001 // fallback: non-XOR MAPPED-ADDRESS (RFC 3489)
	familyIPv4      = 0x01
	familyIPv6      = 0x02
)

// QueryPublicIP sends a STUN Binding Request to the given server and returns
// the reflexive transport address (the device's public IP as seen by the server).
func QueryPublicIP(server string, timeout time.Duration) (string, error) {
	conn, err := net.DialTimeout("udp", server, timeout)
	if err != nil {
		return "", fmt.Errorf("stun: dial %s: %w", server, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	// Build Binding Request: 20-byte header, no attributes.
	var req [20]byte
	req[0], req[1] = 0x00, 0x01 // Message Type: Binding Request
	// Message Length: 0 (no attributes)
	binary.BigEndian.PutUint32(req[4:8], stunMagicCookie)
	if _, err := rand.Read(req[8:20]); err != nil {
		return "", fmt.Errorf("stun: random txid: %w", err)
	}

	if _, err := conn.Write(req[:]); err != nil {
		return "", fmt.Errorf("stun: write: %w", err)
	}

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return "", fmt.Errorf("stun: read: %w", err)
	}
	if n < 20 {
		return "", fmt.Errorf("stun: response too short (%d bytes)", n)
	}

	// Verify response type (Binding Success = 0x0101) and magic cookie.
	respType := binary.BigEndian.Uint16(buf[0:2])
	if respType != 0x0101 {
		return "", fmt.Errorf("stun: unexpected response type 0x%04x", respType)
	}
	if binary.BigEndian.Uint32(buf[4:8]) != stunMagicCookie {
		return "", fmt.Errorf("stun: magic cookie mismatch")
	}

	// Parse attributes looking for XOR-MAPPED-ADDRESS (preferred) or MAPPED-ADDRESS.
	attrStart := 20
	msgLen := int(binary.BigEndian.Uint16(buf[2:4]))
	if 20+msgLen > n {
		msgLen = n - 20
	}

	for pos := attrStart; pos+4 <= attrStart+msgLen; {
		attrType := binary.BigEndian.Uint16(buf[pos : pos+2])
		attrLen := int(binary.BigEndian.Uint16(buf[pos+2 : pos+4]))
		valueStart := pos + 4
		if valueStart+attrLen > n {
			break
		}

		if attrType == attrXORMapped {
			ip, err := parseXORMappedAddress(buf[valueStart:valueStart+attrLen], buf[4:20])
			if err == nil {
				return NormalizeIP(ip), nil
			}
		}
		if attrType == attrMapped {
			ip, err := parseMappedAddress(buf[valueStart : valueStart+attrLen])
			if err == nil {
				return NormalizeIP(ip), nil
			}
		}

		// Attributes are padded to 4-byte boundaries.
		pos = valueStart + ((attrLen + 3) &^ 3)
	}

	return "", fmt.Errorf("stun: no mapped address in response")
}

// parseXORMappedAddress decodes a XOR-MAPPED-ADDRESS attribute value.
// magicAndTxID is the 16-byte slice: magic cookie (4) + transaction ID (12).
func parseXORMappedAddress(data, magicAndTxID []byte) (string, error) {
	if len(data) < 4 {
		return "", fmt.Errorf("stun: xor-mapped too short")
	}
	family := data[1]
	switch family {
	case familyIPv4:
		if len(data) < 8 {
			return "", fmt.Errorf("stun: xor-mapped-v4 too short")
		}
		ip := make(net.IP, 4)
		for i := 0; i < 4; i++ {
			ip[i] = data[4+i] ^ magicAndTxID[i]
		}
		return ip.String(), nil
	case familyIPv6:
		if len(data) < 20 || len(magicAndTxID) < 16 {
			return "", fmt.Errorf("stun: xor-mapped-v6 too short")
		}
		ip := make(net.IP, 16)
		for i := 0; i < 16; i++ {
			ip[i] = data[4+i] ^ magicAndTxID[i]
		}
		return ip.String(), nil
	default:
		return "", fmt.Errorf("stun: unknown family 0x%02x", family)
	}
}

// parseMappedAddress decodes a MAPPED-ADDRESS attribute value (non-XOR, RFC 3489).
func parseMappedAddress(data []byte) (string, error) {
	if len(data) < 4 {
		return "", fmt.Errorf("stun: mapped too short")
	}
	family := data[1]
	switch family {
	case familyIPv4:
		if len(data) < 8 {
			return "", fmt.Errorf("stun: mapped-v4 too short")
		}
		ip := make(net.IP, 4)
		copy(ip, data[4:8])
		return ip.String(), nil
	case familyIPv6:
		if len(data) < 20 {
			return "", fmt.Errorf("stun: mapped-v6 too short")
		}
		ip := make(net.IP, 16)
		copy(ip, data[4:20])
		return ip.String(), nil
	default:
		return "", fmt.Errorf("stun: unknown family 0x%02x", family)
	}
}

// NormalizeIP canonicalizes an IP string for consistent comparison.
// Strips port if present (e.g., "203.0.113.5:14812" → "203.0.113.5",
// "[::1]:8080" → "::1"), handles IPv6-mapped-IPv4
// (e.g., "::ffff:203.0.113.5" → "203.0.113.5"), and any other
// representation differences via net.ParseIP.
func NormalizeIP(s string) string {
	s = strings.TrimSpace(s)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return s
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}
