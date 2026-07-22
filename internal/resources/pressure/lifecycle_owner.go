package pressure

import (
	"strings"
	"sync"
)

type lifecycleClaim struct {
	owner string
	seq   uint64
}

var lifecycleOwners = struct {
	sync.Mutex
	next   uint64
	claims map[uint64]lifecycleClaim
}{claims: make(map[uint64]lifecycleClaim)}

// BeginLifecycleOwner exposes only a bounded owner class or app identity to
// emergency diagnostics. It never records request arguments or user data.
func BeginLifecycleOwner(owner string) func() {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return func() {}
	}
	lifecycleOwners.Lock()
	lifecycleOwners.next++
	token := lifecycleOwners.next
	lifecycleOwners.claims[token] = lifecycleClaim{owner: owner, seq: token}
	lifecycleOwners.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			lifecycleOwners.Lock()
			delete(lifecycleOwners.claims, token)
			lifecycleOwners.Unlock()
		})
	}
}

// CurrentLifecycleOwner returns the most recently entered active owner. The
// tracker is constant-size in normal operation because every claim is scoped
// with a deferred release.
func CurrentLifecycleOwner() string {
	lifecycleOwners.Lock()
	defer lifecycleOwners.Unlock()
	var latest lifecycleClaim
	for _, claim := range lifecycleOwners.claims {
		if claim.seq > latest.seq {
			latest = claim
		}
	}
	return latest.owner
}
