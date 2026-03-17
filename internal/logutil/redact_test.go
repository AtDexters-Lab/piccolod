package logutil

import (
	"net"
	"strings"
	"testing"
)

func TestRedact_IPv4(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain_ipv4",
			input: "client connected from 192.168.1.100",
			want:  "client connected from <IPv4>",
		},
		{
			name:  "ipv4_with_port_preserved",
			input: "client 10.0.0.5:8080 connected",
			want:  "client <IPv4>:8080 connected",
		},
		{
			name:  "preserves_loopback",
			input: "listening on 127.0.0.1:8080",
			want:  "listening on 127.0.0.1:8080",
		},
		{
			name:  "preserves_loopback_range",
			input: "host 127.0.1.1 resolved",
			want:  "host 127.0.1.1 resolved",
		},
		{
			name:  "preserves_unspecified_v4",
			input: "listening on 0.0.0.0:80",
			want:  "listening on 0.0.0.0:80",
		},
		{
			name:  "multiple_ipv4",
			input: "from 192.168.1.1 to 10.0.0.1",
			want:  "from <IPv4> to <IPv4>",
		},
		{
			name:  "mixed_loopback_and_external",
			input: "proxy 127.0.0.1:8080 -> 10.0.0.1:443",
			want:  "proxy 127.0.0.1:8080 -> <IPv4>:443",
		},
		{
			name:  "gin_log_line",
			input: `2024-01-15T10:30:00+0000 piccolod[123]: [GIN] 2024/01/15 - 10:30:00 | 200 | 1.234ms | 192.168.1.50 | GET "/api/v1/health/live"`,
			want:  `2024-01-15T10:30:00+0000 piccolod[123]: [GIN] 2024/01/15 - 10:30:00 | 200 | 1.234ms | <IPv4> | GET "/api/v1/health/live"`,
		},
		{
			name:  "no_ip",
			input: "app piccolo-files started successfully",
			want:  "app piccolo-files started successfully",
		},
		{
			name:  "invalid_octet_preserved",
			input: "version 999.999.999.999 released",
			want:  "version 999.999.999.999 released",
		},
		{
			name:  "preserves_subnet_mask_24",
			input: "netmask 255.255.255.0 on eth0",
			want:  "netmask 255.255.255.0 on eth0",
		},
		{
			name:  "preserves_subnet_mask_16",
			input: "mask=255.255.0.0",
			want:  "mask=255.255.0.0",
		},
		{
			name:  "preserves_subnet_mask_25",
			input: "subnet 255.255.255.128 configured",
			want:  "subnet 255.255.255.128 configured",
		},
		{
			name:  "preserves_limited_broadcast",
			input: "broadcast 255.255.255.255",
			want:  "broadcast 255.255.255.255", // caught by isSubnetMask (/32)
		},
		{
			name:  "preserves_multicast_all_hosts",
			input: "joined group 224.0.0.1",
			want:  "joined group 224.0.0.1",
		},
		{
			name:  "preserves_multicast_mdns",
			input: "mdns on 224.0.0.251:5353",
			want:  "mdns on 224.0.0.251:5353",
		},
		{
			name:  "preserves_link_local",
			input: "assigned 169.254.1.50 (DHCP fallback)",
			want:  "assigned 169.254.1.50 (DHCP fallback)",
		},
		{
			name:  "redacts_rfc1918_class_c",
			input: "from 192.168.1.1",
			want:  "from <IPv4>",
		},
		{
			name:  "redacts_rfc1918_class_a",
			input: "from 10.0.0.1",
			want:  "from <IPv4>",
		},
		{
			name:  "redacts_rfc1918_class_b",
			input: "from 172.16.0.1",
			want:  "from <IPv4>",
		},
		{
			name:  "redacts_non_contiguous_mask",
			input: "host 255.255.255.1",
			want:  "host <IPv4>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(Redact([]byte(tt.input)))
			if got != tt.want {
				t.Errorf("Redact(%q)\n got: %q\nwant: %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRedact_IPv6(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "full_ipv6",
			input: "client 2001:0db8:85a3:0000:0000:8a2e:0370:7334 connected",
			want:  "client <IPv6> connected",
		},
		{
			name:  "preserves_link_local_v6",
			input: "from fe80::1 request",
			want:  "from fe80::1 request",
		},
		{
			name:  "compressed_ipv6_multi_group",
			input: "addr 2001:db8::1 seen",
			want:  "addr <IPv6> seen",
		},
		{
			name:  "preserves_loopback_v6",
			input: "listening on ::1",
			want:  "listening on ::1",
		},
		{
			name:  "preserves_unspecified",
			input: "bind to :: port 80",
			want:  "bind to :: port 80",
		},
		{
			name:  "timestamp_not_redacted",
			input: "2024-01-15T10:30:00+0000 event",
			want:  "2024-01-15T10:30:00+0000 event",
		},
		{
			name:  "ipv4_mapped_ipv6_redacted",
			input: "client ::ffff:192.168.1.50 connected",
			want:  "client <IPv6>:<IPv4> connected",
		},
		{
			name:  "preserves_multicast_all_nodes_v6",
			input: "joined ff02::1",
			want:  "joined ff02::1",
		},
		{
			name:  "preserves_multicast_mdns_v6",
			input: "mdns query to ff02::fb",
			want:  "mdns query to ff02::fb",
		},
		{
			name:  "preserves_link_local_full_v6",
			input: "neighbor fe80::1a2b:3c4d:5e6f:7890 reachable",
			want:  "neighbor fe80::1a2b:3c4d:5e6f:7890 reachable",
		},
		{
			name:  "redacts_global_unicast_v6",
			input: "from 2001:db8::1",
			want:  "from <IPv6>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(Redact([]byte(tt.input)))
			if got != tt.want {
				t.Errorf("Redact(%q)\n got: %q\nwant: %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRedact_PreservesLogMetadata(t *testing.T) {
	input := `2024-01-15T10:30:00+0000 piccolod[123]: INFO: app piccolo-files started on port 8080
2024-01-15T10:30:01+0000 piccolod[123]: WARN: health check degraded
2024-01-15T10:30:02+0000 piccolod[123]: [GIN] 200 | 127.0.0.1 | GET /api/v1/health/live`

	got := string(Redact([]byte(input)))

	// Timestamps, log levels, component names, port numbers should be preserved
	for _, preserved := range []string{
		"2024-01-15T10:30:00+0000",
		"piccolod[123]",
		"INFO:",
		"piccolo-files",
		"port 8080",
		"health check degraded",
		"127.0.0.1",
	} {
		if !strings.Contains(got, preserved) {
			t.Errorf("expected %q to be preserved in output, but it was redacted", preserved)
		}
	}
}

func TestIsSubnetMask(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"255.255.255.0", true},   // /24
		{"255.255.0.0", true},     // /16
		{"255.0.0.0", true},       // /8
		{"255.255.255.128", true}, // /25
		{"255.255.255.252", true}, // /30
		{"255.255.255.255", true}, // /32
		{"128.0.0.0", true},       // /1
		{"255.255.255.1", false},  // not contiguous
		{"255.0.255.0", false},    // not contiguous
		{"0.0.0.0", false},        // excluded (unspecified)
		{"192.168.1.1", false},    // normal IP
		{"10.0.0.1", false},       // normal IP
		{"::1", false},            // IPv6 — function is IPv4-only
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if got := isSubnetMask(ip); got != tt.want {
				t.Errorf("isSubnetMask(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func BenchmarkRedact(b *testing.B) {
	// Simulate realistic 10k-line journal output
	var lines []string
	for i := 0; i < 10000; i++ {
		switch i % 5 {
		case 0:
			lines = append(lines, `2024-01-15T10:30:00+0000 piccolod[123]: [GIN] 200 | 1.234ms | 192.168.1.50 | GET "/api/v1/health/live"`)
		case 1:
			lines = append(lines, `2024-01-15T10:30:00+0000 piccolod[123]: INFO: app piccolo-files started successfully`)
		case 2:
			lines = append(lines, `2024-01-15T10:30:00+0000 piccolod[123]: proxy: forwarding to 127.0.0.1:8080`)
		case 3:
			lines = append(lines, `2024-01-15T10:30:00+0000 piccolod[123]: WARN: health check timeout for listener web`)
		case 4:
			lines = append(lines, `2024-01-15T10:30:00+0000 piccolod[123]: [GIN] 200 | 0.5ms | 10.0.0.1:52341 | GET "/api/v1/apps"`)
		}
	}
	input := []byte(strings.Join(lines, "\n"))

	b.ResetTimer()
	b.SetBytes(int64(len(input)))
	for i := 0; i < b.N; i++ {
		Redact(input)
	}
}
