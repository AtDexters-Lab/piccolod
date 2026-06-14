package services

import "testing"

func TestPortAllocator_ReleaseDoesNotRewindCursors(t *testing.T) {
	alloc := NewPortAllocator(
		PortRange{Start: 55000, End: 55100},
		PortRange{Start: 56000, End: 56100},
	)

	alloc.nextHostBind = 55042
	alloc.nextPublic = 56042

	alloc.usedHost[55010] = struct{}{}
	alloc.usedPublic[publicKey(56010, "tcp")] = struct{}{}

	alloc.Release(55010, 56010)

	if alloc.nextHostBind != 55042 {
		t.Fatalf("expected host-bind cursor to remain at %d, got %d", 55042, alloc.nextHostBind)
	}
	if alloc.nextPublic != 56042 {
		t.Fatalf("expected public cursor to remain at %d, got %d", 56042, alloc.nextPublic)
	}
}

func TestClaimPublicPort(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		alloc := NewPortAllocator(PortRange{Start: 15000, End: 25000}, PortRange{Start: 35000, End: 45000})
		if err := alloc.ClaimPublicPort(50053, true); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := alloc.usedPublic[publicKey(50053, "udp")]; !ok {
			t.Fatal("expected 53/udp to be tracked in usedPublic")
		}
	})

	t.Run("conflict_same_protocol", func(t *testing.T) {
		alloc := NewPortAllocator(PortRange{Start: 15000, End: 25000}, PortRange{Start: 35000, End: 45000})
		if err := alloc.ClaimPublicPort(50053, true); err != nil {
			t.Fatalf("first claim failed: %v", err)
		}
		if err := alloc.ClaimPublicPort(50053, true); err == nil {
			t.Fatal("expected error on duplicate UDP claim")
		}
	})

	t.Run("different_protocols_independent", func(t *testing.T) {
		alloc := NewPortAllocator(PortRange{Start: 15000, End: 25000}, PortRange{Start: 35000, End: 45000})
		if err := alloc.ClaimPublicPort(50053, true); err != nil {
			t.Fatalf("UDP claim failed: %v", err)
		}
		if err := alloc.ClaimPublicPort(50053, false); err != nil {
			t.Fatalf("TCP claim on same port should succeed: %v", err)
		}
		if _, ok := alloc.usedPublic[publicKey(50053, "udp")]; !ok {
			t.Fatal("expected 53/udp tracked")
		}
		if _, ok := alloc.usedPublic[publicKey(50053, "tcp")]; !ok {
			t.Fatal("expected 53/tcp tracked")
		}
	})

	t.Run("outside_auto_range", func(t *testing.T) {
		alloc := NewPortAllocator(PortRange{Start: 15000, End: 25000}, PortRange{Start: 35000, End: 45000})
		// Port 53 is outside 35000-45000 — should still work for claims
		if err := alloc.ClaimPublicPort(50053, false); err != nil {
			t.Fatalf("claim outside range should succeed: %v", err)
		}
	})
}

func TestAllocateWithClaim(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		alloc := NewPortAllocator(PortRange{Start: 15000, End: 25000}, PortRange{Start: 35000, End: 45000})
		hb, err := alloc.AllocateWithClaim(50053, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hb < 15000 || hb > 25000 {
			t.Fatalf("host-bind %d outside expected range", hb)
		}
	})

	t.Run("rollback_on_claim_failure", func(t *testing.T) {
		alloc := NewPortAllocator(PortRange{Start: 15000, End: 25000}, PortRange{Start: 35000, End: 45000})
		// Claim port 53 first
		if err := alloc.ClaimPublicPort(50053, true); err != nil {
			t.Fatalf("first claim failed: %v", err)
		}
		hostsBefore := len(alloc.usedHost)
		// Second claim should fail and rollback host-bind
		if _, err := alloc.AllocateWithClaim(50053, true); err == nil {
			t.Fatal("expected error on duplicate claim")
		}
		if len(alloc.usedHost) != hostsBefore {
			t.Fatal("host-bind port was not rolled back after claim failure")
		}
	})
}

func TestAllocateForClaim(t *testing.T) {
	t.Run("with_claim", func(t *testing.T) {
		alloc := NewPortAllocator(PortRange{Start: 15000, End: 25000}, PortRange{Start: 35000, End: 45000})
		port := 50053
		hb, pp, err := alloc.AllocateForClaim(&port, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pp != 50053 {
			t.Fatalf("expected public port 53, got %d", pp)
		}
		if hb < 15000 || hb > 25000 {
			t.Fatalf("host-bind %d outside expected range", hb)
		}
	})

	t.Run("without_claim", func(t *testing.T) {
		alloc := NewPortAllocator(PortRange{Start: 15000, End: 25000}, PortRange{Start: 35000, End: 45000})
		hb, pp, err := alloc.AllocateForClaim(nil, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pp < 35000 || pp > 45000 {
			t.Fatalf("auto-allocated port %d outside range", pp)
		}
		if hb < 15000 || hb > 25000 {
			t.Fatalf("host-bind %d outside expected range", hb)
		}
	})
}

func TestAllocateForClaimAutoUDPSkipsUDPUnavailablePort(t *testing.T) {
	alloc := NewPortAllocator(PortRange{Start: 15000, End: 15010}, PortRange{Start: 35000, End: 35002})
	alloc.portAvailable = func(host string, port int, network string) bool {
		_ = host
		return !(network == "udp" && port == 35000)
	}

	_, public, err := alloc.AllocateForClaim(nil, true)
	if err != nil {
		t.Fatalf("allocate auto udp: %v", err)
	}
	if public != 35001 {
		t.Fatalf("public port = %d, want 35001 after skipping UDP-unavailable 35000", public)
	}
	if _, ok := alloc.usedPublic[publicKey(35000, "tcp")]; ok {
		t.Fatalf("UDP-unavailable port was reserved under auto key")
	}
	if _, ok := alloc.usedPublic[publicKey(35001, "tcp")]; !ok {
		t.Fatalf("allocated UDP auto port not tracked under auto key")
	}
}

func TestFreePublicProto(t *testing.T) {
	alloc := NewPortAllocator(PortRange{Start: 15000, End: 25000}, PortRange{Start: 35000, End: 45000})
	// Claim both TCP and UDP on port 53
	alloc.ClaimPublicPort(50053, false)
	alloc.ClaimPublicPort(50053, true)

	// Free only UDP — TCP should remain
	alloc.FreePublicProto(50053, "udp")
	if _, ok := alloc.usedPublic[publicKey(50053, "tcp")]; !ok {
		t.Fatal("TCP 50053 should still be tracked after freeing UDP 50053")
	}
	if _, ok := alloc.usedPublic[publicKey(50053, "udp")]; ok {
		t.Fatal("UDP 50053 should be freed")
	}

	// Re-claim UDP should work now
	if err := alloc.ClaimPublicPort(50053, true); err != nil {
		t.Fatalf("re-claim after free should succeed: %v", err)
	}
}

func TestAutoAllocateDoesNotConflictWithClaims(t *testing.T) {
	// Claims are outside the auto-allocate range, so they should never interfere
	alloc := NewPortAllocator(PortRange{Start: 15000, End: 25000}, PortRange{Start: 35000, End: 45000})
	alloc.ClaimPublicPort(50053, true) // outside 35000-45000

	// Auto-allocate should still work
	_, pp, err := alloc.AllocatePair()
	if err != nil {
		t.Fatalf("auto-allocate should not be affected by external claim: %v", err)
	}
	if pp < 35000 || pp > 45000 {
		t.Fatalf("auto-allocated port %d outside range", pp)
	}
}
