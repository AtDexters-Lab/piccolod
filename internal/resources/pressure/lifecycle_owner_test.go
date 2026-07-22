package pressure

import "testing"

func TestLifecycleOwnerUsesMostRecentActiveClaim(t *testing.T) {
	first := BeginLifecycleOwner("storage")
	defer first()
	if got := CurrentLifecycleOwner(); got != "storage" {
		t.Fatalf("owner = %q, want storage", got)
	}
	second := BeginLifecycleOwner("app:demo")
	if got := CurrentLifecycleOwner(); got != "app:demo" {
		t.Fatalf("owner = %q, want app:demo", got)
	}
	second()
	second()
	if got := CurrentLifecycleOwner(); got != "storage" {
		t.Fatalf("owner after release = %q, want storage", got)
	}
}
