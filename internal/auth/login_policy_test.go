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
