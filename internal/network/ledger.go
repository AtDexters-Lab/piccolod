package network

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"piccolod/internal/fsutil"
)

// APEvent records an AP-mode toggle for rate-shaping decideAP.
type APEvent struct {
	When   time.Time `json:"when"`
	Enter  bool      `json:"enter"` // true=enter, false=exit
	Reason string    `json:"reason,omitempty"`
}

// ActionLedger records what the supervisor itself recently did, used purely
// for self-rate-shaping. Split into persistent (Reboots — survives boot) and
// volatile (Bounces, APToggles, LastBounceAt — cleared on tmpfs).
type ActionLedger struct {
	Reboots []time.Time

	Bounces      map[DeviceKind][]time.Time
	APToggles    []APEvent
	LastBounceAt map[DeviceKind]time.Time
}

// LedgerStore persists the ledger to disk. Writes happen on action initiation
// (not completion) — F-B2: a crash mid-bounce still consumes a budget slot.
type LedgerStore struct {
	persistentPath string // /var/lib/piccolo/net-ledger.json
	volatilePath   string // /run/piccolo/net-ledger-volatile.json
	legacyPath     string // /var/lib/piccolo/net-watchdog-reboots (one-shot migrate)

	mu     sync.Mutex
	ledger ActionLedger
}

const (
	defaultPersistentPath = "/var/lib/piccolo/net-ledger.json"
	defaultVolatilePath   = "/run/piccolo/net-ledger-volatile.json"
	defaultLegacyPath     = "/var/lib/piccolo/net-watchdog-reboots"
)

// NewLedgerStore returns a store with default piccolo paths.
func NewLedgerStore() *LedgerStore {
	return &LedgerStore{
		persistentPath: defaultPersistentPath,
		volatilePath:   defaultVolatilePath,
		legacyPath:     defaultLegacyPath,
	}
}

// NewLedgerStoreWithPaths is the test constructor.
func NewLedgerStoreWithPaths(persistent, volatile, legacy string) *LedgerStore {
	return &LedgerStore{
		persistentPath: persistent,
		volatilePath:   volatile,
		legacyPath:     legacy,
	}
}

// persistentJSON is the on-disk schema for the persistent file.
type persistentJSON struct {
	Reboots []time.Time `json:"reboots"`
}

// volatileJSON is the on-disk schema for the volatile file.
type volatileJSON struct {
	Bounces      map[string][]time.Time `json:"bounces"`
	APToggles    []APEvent              `json:"ap_toggles"`
	LastBounceAt map[string]time.Time   `json:"last_bounce_at"`
}

// Load reads both files plus migrates the legacy reboot ledger. See the RFC
// "Ledger startup semantics" for the fail-open vs fail-closed rules.
//
// Behavior:
//   - persistent missing: empty Reboots
//   - persistent corrupt: fail-closed → Reboots=[now()] (budget exhausted)
//   - volatile missing: fail-open (steady-state post-clean-reboot)
//   - volatile corrupt: fail-closed → 3 synthetic Bounces[dev] timestamps at now()
//   - legacy file present: parse + merge into Reboots, then delete legacy file
func (s *LedgerStore) Load() ActionLedger {
	s.mu.Lock()
	defer s.mu.Unlock()

	led := ActionLedger{
		Bounces:      make(map[DeviceKind][]time.Time),
		LastBounceAt: make(map[DeviceKind]time.Time),
	}

	// 1. Persistent — Reboots
	if data, err := os.ReadFile(s.persistentPath); err == nil {
		var pj persistentJSON
		if jerr := json.Unmarshal(data, &pj); jerr == nil {
			led.Reboots = pj.Reboots
		} else {
			log.Printf("WARN: net-ledger: persistent corrupt (%v) — fail-closed: budget exhausted", jerr)
			led.Reboots = []time.Time{time.Now()}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		log.Printf("WARN: net-ledger: persistent read error (%v) — fail-closed: budget exhausted", err)
		led.Reboots = []time.Time{time.Now()}
	}

	// 2. Legacy migration — one-shot. Each line is a unix timestamp.
	// Persist the merged result BEFORE deleting the legacy file so a
	// crash window or quick restart between merge and the next
	// RecordReboot cannot lose the migrated history (RFC S8-rev:
	// "preserves reboot budget across cutover").
	if data, err := os.ReadFile(s.legacyPath); err == nil {
		merged := false
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			ts, perr := strconv.ParseInt(line, 10, 64)
			if perr != nil {
				continue
			}
			led.Reboots = append(led.Reboots, time.Unix(ts, 0))
			merged = true
		}
		if merged {
			pj := persistentJSON{Reboots: append([]time.Time(nil), led.Reboots...)}
			if perr := marshalAndWrite(s.persistentPath, pj); perr != nil {
				// Leave legacy file in place for next-boot retry —
				// in-memory ledger reflects the merge for the current
				// run, but the on-disk state is still legacy-only.
				log.Printf("WARN: net-ledger: persist after migration failed (%v) — legacy file retained for retry", perr)
			} else if rerr := os.Remove(s.legacyPath); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				log.Printf("WARN: net-ledger: legacy file delete failed: %v", rerr)
			}
		} else {
			// Empty/garbage legacy file — no merge happened, but still
			// remove it so we don't keep parsing garbage every boot.
			if rerr := os.Remove(s.legacyPath); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				log.Printf("WARN: net-ledger: legacy file delete failed: %v", rerr)
			}
		}
	}

	// 3. Volatile — Bounces / APToggles / LastBounceAt
	switch data, err := os.ReadFile(s.volatilePath); {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		// fail-open: steady-state post-clean-reboot. tmpfs is always cleared.
	case err != nil:
		log.Printf("WARN: net-ledger: volatile read error (%v) — fail-closed", err)
		s.synthesizeFailClosed(&led)
	default:
		var vj volatileJSON
		if jerr := json.Unmarshal(data, &vj); jerr != nil {
			log.Printf("WARN: net-ledger: volatile corrupt (%v) — fail-closed", jerr)
			s.synthesizeFailClosed(&led)
		} else {
			for k, v := range vj.Bounces {
				led.Bounces[parseDeviceKind(k)] = v
			}
			led.APToggles = vj.APToggles
			for k, v := range vj.LastBounceAt {
				led.LastBounceAt[parseDeviceKind(k)] = v
			}
		}
	}

	s.ledger = led
	return led
}

// Snapshot returns a deep copy of the in-memory ledger.
func (s *LedgerStore) Snapshot() ActionLedger {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneLedger(s.ledger)
}

// LastBounceAtSnapshot returns a deep-copied LastBounceAt map. Cheaper than
// Snapshot when callers only need the quiet-period anchor (snapshot.go's
// Recovering classification).
func (s *LedgerStore) LastBounceAtSnapshot() map[DeviceKind]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[DeviceKind]time.Time, len(s.ledger.LastBounceAt))
	for k, v := range s.ledger.LastBounceAt {
		out[k] = v
	}
	return out
}

// RecordBounce appends a bounce timestamp for dev and updates LastBounceAt.
// Persists to volatile file synchronously; failure logs but does not
// short-circuit the in-memory mutation (in-memory is the source of truth
// for the current tick).
func (s *LedgerStore) RecordBounce(dev DeviceKind, when time.Time) {
	s.mu.Lock()
	s.ledger.Bounces[dev] = append(s.ledger.Bounces[dev], when)
	s.ledger.LastBounceAt[dev] = when
	s.mu.Unlock()
	s.persistVolatile()
}

// RecordAPToggle appends an AP toggle event.
func (s *LedgerStore) RecordAPToggle(enter bool, when time.Time, reason string) {
	s.mu.Lock()
	s.ledger.APToggles = append(s.ledger.APToggles, APEvent{When: when, Enter: enter, Reason: reason})
	s.mu.Unlock()
	s.persistVolatile()
}

// RecordReboot appends a reboot timestamp and persists synchronously.
// Called BEFORE issuing systemctl reboot (F-B2: initiation, not completion).
func (s *LedgerStore) RecordReboot(when time.Time) error {
	s.mu.Lock()
	s.ledger.Reboots = append(s.ledger.Reboots, when)
	s.mu.Unlock()
	return s.persistPersistent()
}

// Prune drops entries outside the relevant rate-shaping windows. Called once
// per tick before deciders read.
func (s *LedgerStore) Prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ledger.Reboots = pruneTimes(s.ledger.Reboots, now, rebootWindowDuration)
	for k, v := range s.ledger.Bounces {
		s.ledger.Bounces[k] = pruneTimes(v, now, bounceWindowDuration)
	}
	s.ledger.APToggles = pruneAPEvents(s.ledger.APToggles, now, apToggleWindow)
}

// Rate-shaping windows (kept package-private — deciders import these).
const (
	bounceWindowDuration = time.Hour
	rebootWindowDuration = 2 * time.Hour
	apToggleWindow       = time.Hour
)

func (s *LedgerStore) persistPersistent() error {
	s.mu.Lock()
	pj := persistentJSON{Reboots: append([]time.Time(nil), s.ledger.Reboots...)}
	s.mu.Unlock()
	return marshalAndWrite(s.persistentPath, pj)
}

func (s *LedgerStore) persistVolatile() {
	s.mu.Lock()
	vj := volatileJSON{
		Bounces:      make(map[string][]time.Time, len(s.ledger.Bounces)),
		APToggles:    append([]APEvent(nil), s.ledger.APToggles...),
		LastBounceAt: make(map[string]time.Time, len(s.ledger.LastBounceAt)),
	}
	for k, v := range s.ledger.Bounces {
		vj.Bounces[k.String()] = append([]time.Time(nil), v...)
	}
	for k, v := range s.ledger.LastBounceAt {
		vj.LastBounceAt[k.String()] = v
	}
	s.mu.Unlock()
	if err := marshalAndWrite(s.volatilePath, vj); err != nil {
		log.Printf("WARN: net-ledger: volatile persist failed: %v", err)
	}
}

// synthesizeFailClosed pre-populates Bounces with 3 synthetic now() timestamps
// per device kind so the bounce budget is exhausted from start. Used only on
// volatile-corrupt path (genuinely adversarial).
func (s *LedgerStore) synthesizeFailClosed(led *ActionLedger) {
	now := time.Now()
	synth := []time.Time{now, now, now}
	for _, k := range []DeviceKind{DeviceWiFi, DeviceEthernet} {
		led.Bounces[k] = append([]time.Time(nil), synth...)
	}
}

// marshalAndWrite JSON-marshals v with indentation and atomically writes
// to path via fsutil.AtomicWriteFile (fsync + parent-dir sync for power-loss
// durability). Creates parent directories as needed.
func marshalAndWrite(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, data, 0o644)
}

func pruneTimes(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	out := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

func pruneAPEvents(es []APEvent, now time.Time, window time.Duration) []APEvent {
	cutoff := now.Add(-window)
	out := es[:0]
	for _, e := range es {
		if e.When.After(cutoff) {
			out = append(out, e)
		}
	}
	return out
}

func cloneLedger(in ActionLedger) ActionLedger {
	out := ActionLedger{
		Reboots:      append([]time.Time(nil), in.Reboots...),
		Bounces:      make(map[DeviceKind][]time.Time, len(in.Bounces)),
		APToggles:    append([]APEvent(nil), in.APToggles...),
		LastBounceAt: make(map[DeviceKind]time.Time, len(in.LastBounceAt)),
	}
	for k, v := range in.Bounces {
		out.Bounces[k] = append([]time.Time(nil), v...)
	}
	for k, v := range in.LastBounceAt {
		out.LastBounceAt[k] = v
	}
	return out
}

func parseDeviceKind(s string) DeviceKind {
	switch s {
	case "wifi":
		return DeviceWiFi
	case "ethernet":
		return DeviceEthernet
	default:
		return DeviceWiFi // default; round-trips with String() for known values
	}
}
