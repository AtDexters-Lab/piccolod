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
	alloc.usedPublic[56010] = struct{}{}

	alloc.Release(55010, 56010)

	if alloc.nextHostBind != 55042 {
		t.Fatalf("expected host-bind cursor to remain at %d, got %d", 55042, alloc.nextHostBind)
	}
	if alloc.nextPublic != 56042 {
		t.Fatalf("expected public cursor to remain at %d, got %d", 56042, alloc.nextPublic)
	}
}
