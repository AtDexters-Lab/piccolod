package auth

import (
	"testing"
)

func TestDetermineRPID(t *testing.T) {
	t.Run("local_hostname", func(t *testing.T) {
		got := DetermineRPID("piccolo.local", "")
		if got != "piccolo.local" {
			t.Fatalf("expected piccolo.local, got %s", got)
		}
	})

	t.Run("local_hostname_with_port", func(t *testing.T) {
		got := DetermineRPID("piccolo.local:8080", "")
		if got != "piccolo.local" {
			t.Fatalf("expected piccolo.local, got %s", got)
		}
	})

	t.Run("local_hostname_case_insensitive", func(t *testing.T) {
		got := DetermineRPID("Piccolo.LOCAL", "")
		if got != "piccolo.local" {
			t.Fatalf("expected piccolo.local, got %s", got)
		}
	})

	t.Run("namek_slug_under_base_domain", func(t *testing.T) {
		got := DetermineRPID("slug.example.com", "example.com")
		if got != "example.com" {
			t.Fatalf("expected example.com, got %s", got)
		}
	})

	t.Run("namek_custom_name_under_base_domain", func(t *testing.T) {
		got := DetermineRPID("my-device.example.com", "example.com")
		if got != "example.com" {
			t.Fatalf("expected example.com, got %s", got)
		}
	})

	t.Run("namek_base_domain_case_insensitive", func(t *testing.T) {
		got := DetermineRPID("slug.Example.COM", "Example.COM")
		if got != "example.com" {
			t.Fatalf("expected example.com, got %s", got)
		}
	})

	t.Run("self_hosted_nexus_different_domain", func(t *testing.T) {
		got := DetermineRPID("mydevice.otherdomain.com", "example.com")
		if got != "mydevice.otherdomain.com" {
			t.Fatalf("expected mydevice.otherdomain.com, got %s", got)
		}
	})

	t.Run("suffix_confusion_not_a_subdomain", func(t *testing.T) {
		// "notexample.com" is NOT a subdomain of "example.com" — the dot
		// prefix in HasSuffix prevents this domain confusion.
		got := DetermineRPID("notexample.com", "example.com")
		if got != "notexample.com" {
			t.Fatalf("expected notexample.com, got %s", got)
		}
	})

	t.Run("trailing_dot_fqdn_under_base_domain", func(t *testing.T) {
		got := DetermineRPID("slug.example.com.", "example.com")
		if got != "example.com" {
			t.Fatalf("expected example.com, got %s", got)
		}
	})

	t.Run("exact_base_domain_match", func(t *testing.T) {
		got := DetermineRPID("example.com", "example.com")
		if got != "example.com" {
			t.Fatalf("expected example.com, got %s", got)
		}
	})

	t.Run("remote_without_base_domain", func(t *testing.T) {
		got := DetermineRPID("custom.example.org", "")
		if got != "custom.example.org" {
			t.Fatalf("expected custom.example.org, got %s", got)
		}
	})

	t.Run("remote_without_base_domain_with_port", func(t *testing.T) {
		got := DetermineRPID("custom.example.org:443", "")
		if got != "custom.example.org" {
			t.Fatalf("expected custom.example.org, got %s", got)
		}
	})

	t.Run("ipv6_with_port", func(t *testing.T) {
		got := DetermineRPID("[::1]:8080", "")
		if got != "::1" {
			t.Fatalf("expected ::1, got %s", got)
		}
	})

	t.Run("ipv4_without_port", func(t *testing.T) {
		got := DetermineRPID("192.168.1.100", "")
		if got != "192.168.1.100" {
			t.Fatalf("expected 192.168.1.100, got %s", got)
		}
	})
}

func TestNormalizeHost(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{"plain_host", "piccolo.local", "piccolo.local"},
		{"with_port", "jane404.piccolospace.com:8443", "jane404.piccolospace.com"},
		{"uppercase", "Piccolo.Local", "piccolo.local"},
		{"trailing_dot", "example.com.", "example.com"},
		{"ipv6_with_port", "[::1]:8080", "::1"},
		{"ipv4", "192.168.1.100", "192.168.1.100"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeHost(tc.input); got != tc.want {
				t.Fatalf("NormalizeHost(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestShortHostLabel(t *testing.T) {
	cases := []struct {
		name, host, rpID, want string
	}{
		{"subdomain_stripped", "jane404.piccolospace.com", "piccolospace.com", "jane404"},
		{"multi_level_subdomain", "a.b.piccolospace.com", "piccolospace.com", "a.b"},
		{"host_equals_rpid", "piccolospace.com", "piccolospace.com", "piccolospace.com"},
		{"different_domain", "mydevice.example.com", "piccolospace.com", "mydevice.example.com"},
		{"local_host", "piccolo.local", "piccolo.local", "piccolo.local"},
		{"empty_rpid", "jane404.piccolospace.com", "", "jane404.piccolospace.com"},
		{"empty_host", "", "piccolospace.com", ""},
		{"ip_address", "192.168.1.100", "piccolospace.com", "192.168.1.100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShortHostLabel(tc.host, tc.rpID); got != tc.want {
				t.Fatalf("ShortHostLabel(%q, %q) = %q, want %q", tc.host, tc.rpID, got, tc.want)
			}
		})
	}
}

func TestAllowedMethods(t *testing.T) {
	t.Run("remote_secure_has_passkey", func(t *testing.T) {
		got := AllowedMethods(AccessContextRemote, true, "admin", true)
		expectMethods(t, got, []string{"passkey"})
	})

	t.Run("remote_secure_no_passkey", func(t *testing.T) {
		got := AllowedMethods(AccessContextRemote, true, "admin", false)
		expectMethods(t, got, []string{"passkey", "password"})
	})

	t.Run("lan_secure_admin", func(t *testing.T) {
		got := AllowedMethods(AccessContextLAN, true, "admin", false)
		expectMethods(t, got, []string{"passkey", "password"})
	})

	t.Run("lan_secure_standard", func(t *testing.T) {
		got := AllowedMethods(AccessContextLAN, true, "standard", false)
		expectMethods(t, got, []string{"passkey"})
	})

	t.Run("lan_insecure_admin", func(t *testing.T) {
		got := AllowedMethods(AccessContextLAN, false, "admin", false)
		expectMethods(t, got, []string{"password"})
	})

	t.Run("lan_insecure_standard", func(t *testing.T) {
		got := AllowedMethods(AccessContextLAN, false, "standard", false)
		if got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("lan_secure_unknown_role", func(t *testing.T) {
		// Empty role string treated like admin (fallback for pre-multi-user compat)
		got := AllowedMethods(AccessContextLAN, true, "", false)
		expectMethods(t, got, []string{"passkey", "password"})
	})

	t.Run("lan_insecure_unknown_role", func(t *testing.T) {
		got := AllowedMethods(AccessContextLAN, false, "", false)
		expectMethods(t, got, []string{"password"})
	})

	t.Run("remote_secure_standard_no_passkey", func(t *testing.T) {
		// Standard user on remote bootstrap: still gets passkey+password
		got := AllowedMethods(AccessContextRemote, true, "standard", false)
		expectMethods(t, got, []string{"passkey", "password"})
	})

	t.Run("remote_secure_standard_has_passkey", func(t *testing.T) {
		got := AllowedMethods(AccessContextRemote, true, "standard", true)
		expectMethods(t, got, []string{"passkey"})
	})

	t.Run("remote_insecure_admin", func(t *testing.T) {
		got := AllowedMethods(AccessContextRemote, false, "admin", false)
		expectMethods(t, got, []string{"password"})
	})

	t.Run("remote_insecure_standard", func(t *testing.T) {
		got := AllowedMethods(AccessContextRemote, false, "standard", false)
		if got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
}

func TestAccessContext_String(t *testing.T) {
	if AccessContextLAN.String() != "lan" {
		t.Fatalf("expected lan, got %s", AccessContextLAN.String())
	}
	if AccessContextRemote.String() != "remote" {
		t.Fatalf("expected remote, got %s", AccessContextRemote.String())
	}
}

func expectMethods(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}
