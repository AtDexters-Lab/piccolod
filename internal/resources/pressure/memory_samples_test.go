package pressure

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"piccolod/internal/events"
)

const fixturePSI = "some avg10=35.25 avg60=22.00 avg300=10.00 total=100\nfull avg10=12.50 avg60=5.00 avg300=1.00 total=20\n"

func writeMemoryFixture(t *testing.T, base string) {
	t.Helper()
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"memory.current":      "84492288\n",
		"memory.high":         "79998976\n",
		"memory.max":          "128000000\n",
		"memory.events.local": "low 0\nhigh 472366\nmax 0\noom 0\noom_kill 0\n",
		"memory.pressure":     fixturePSI,
	} {
		writeFixture(t, filepath.Join(base, name), value)
	}
}

func writeFixture(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReadMemorySampleFinitePageRoundedLimits(t *testing.T) {
	base := t.TempDir()
	writeMemoryFixture(t, base)
	at := time.Now()
	s := readMemorySamplePath(467, base, at)
	if !s.Valid || s.UID != 467 || !s.At.Equal(at) || s.CgroupID == 0 {
		t.Fatalf("invalid identity: %+v", s)
	}
	if s.CurrentBytes != 84492288 || s.HighBytes != 79998976 || s.MaxBytes != 128000000 || s.HighEvents != 472366 || s.SomeAvg10 != 35.25 || s.FullAvg10 != 12.5 {
		t.Fatalf("wrong sample: %+v", s)
	}
	// Notification reads continue to use avg60, independent of actuation.
	if got := readPSI(filepath.Join(base, "memory.pressure"), "some"); got != 22 {
		t.Fatalf("notification avg60 changed: %v", got)
	}
}

func TestReadMemorySampleRejectsInvalidControlInputs(t *testing.T) {
	cases := []struct{ name, file, value string }{
		{"current negative", "memory.current", "-1"},
		{"current overflow", "memory.current", "9223372036854775808"},
		{"high unlimited", "memory.high", "max"},
		{"high zero", "memory.high", "0"},
		{"high above max", "memory.high", "128000001"},
		{"max unlimited", "memory.max", "max"},
		{"max malformed", "memory.max", "bad"},
		{"max zero", "memory.max", "0"},
		{"counter missing", "memory.events.local", "oom_kill 0\n"},
		{"counter negative", "memory.events.local", "high -1\n"},
		{"counter overflow", "memory.events.local", "high 18446744073709551616\n"},
		{"counter duplicate", "memory.events.local", "high 1\nhigh 2\n"},
		{"PSI missing full", "memory.pressure", strings.Split(fixturePSI, "full")[0]},
		{"PSI missing avg10", "memory.pressure", strings.Replace(fixturePSI, "avg10=35.25", "", 1)},
		{"PSI malformed", "memory.pressure", strings.Replace(fixturePSI, "35.25", "bad", 1)},
		{"PSI NaN", "memory.pressure", strings.Replace(fixturePSI, "35.25", "NaN", 1)},
		{"PSI infinity", "memory.pressure", strings.Replace(fixturePSI, "35.25", "+Inf", 1)},
		{"PSI negative", "memory.pressure", strings.Replace(fixturePSI, "35.25", "-1", 1)},
		{"PSI too large", "memory.pressure", strings.Replace(fixturePSI, "35.25", "100.1", 1)},
		{"PSI avg60 NaN", "memory.pressure", strings.Replace(fixturePSI, "22.00", "NaN", 1)},
		{"PSI negative total", "memory.pressure", strings.Replace(fixturePSI, "total=100", "total=-1", 1)},
		{"PSI duplicate", "memory.pressure", fixturePSI + strings.Split(fixturePSI, "full")[0]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			writeMemoryFixture(t, base)
			writeFixture(t, filepath.Join(base, tc.file), tc.value)
			if s := readMemorySamplePath(467, base, time.Now()); s.Valid {
				t.Fatalf("accepted invalid input: %+v", s)
			}
		})
	}
	for _, file := range []string{"memory.current", "memory.high", "memory.max", "memory.events.local", "memory.pressure"} {
		t.Run("missing "+file, func(t *testing.T) {
			base := t.TempDir()
			writeMemoryFixture(t, base)
			if err := os.Remove(filepath.Join(base, file)); err != nil {
				t.Fatal(err)
			}
			if readMemorySamplePath(467, base, time.Now()).Valid {
				t.Fatal("accepted missing control file")
			}
		})
	}
	if readMemorySamplePath(467, filepath.Join(t.TempDir(), "missing"), time.Now()).Valid {
		t.Fatal("accepted absent cgroup")
	}
}

func TestReadMemorySampleCgroupReplacementIdentity(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "user-467.slice")
	writeMemoryFixture(t, base)
	before := readMemorySamplePath(467, base, time.Now())
	// Keep the old inode allocated to ensure the replacement has a new identity.
	if err := os.Rename(base, base+".old"); err != nil {
		t.Fatal(err)
	}
	writeMemoryFixture(t, base)
	after := readMemorySamplePath(467, base, time.Now())
	if !before.Valid || !after.Valid || before.CgroupID == after.CgroupID {
		t.Fatalf("replacement identity not detected: before=%+v after=%+v", before, after)
	}
}

func TestReadHostMemorySample(t *testing.T) {
	cases := []struct {
		name, data string
		valid      bool
	}{
		{"valid", "MemTotal: 2048000 kB\nMemAvailable: 700000 kB\nMemFree: 10000 kB\n", true},
		{"no available", "MemTotal: 2048000 kB\nMemFree: 700000 kB\n", false},
		{"no total", "MemAvailable: 700000 kB\n", false},
		{"zero total", "MemTotal: 0 kB\nMemAvailable: 0 kB\n", false},
		{"negative", "MemTotal: 2048000 kB\nMemAvailable: -1 kB\n", false},
		{"overflow multiplication", "MemTotal: 9007199254740992 kB\nMemAvailable: 1 kB\n", false},
		{"overflow parsing", "MemTotal: 9223372036854775808 kB\nMemAvailable: 1 kB\n", false},
		{"wrong unit", "MemTotal: 2048000 MB\nMemAvailable: 1 kB\n", false},
		{"missing unit", "MemTotal: 2048000\nMemAvailable: 1 kB\n", false},
		{"available above total", "MemTotal: 100 kB\nMemAvailable: 101 kB\n", false},
		{"duplicate", "MemTotal: 100 kB\nMemTotal: 100 kB\nMemAvailable: 1 kB\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "meminfo")
			writeFixture(t, path, tc.data)
			s := readHostMemorySamplePath(path)
			if s.Valid != tc.valid {
				t.Fatalf("valid=%v, want %v: %+v", s.Valid, tc.valid, s)
			}
			if tc.valid && (s.TotalBytes != 2048000*1024 || s.AvailableBytes != 700000*1024) {
				t.Fatalf("wrong kB conversion: %+v", s)
			}
		})
	}
	if readHostMemorySamplePath(filepath.Join(t.TempDir(), "missing")).Valid {
		t.Fatal("accepted absent meminfo")
	}
}

type memoryRoundLister struct {
	uids    []uint32
	respond func(context.Context, []MemorySample, HostMemorySample)
}

func (l *memoryRoundLister) ListAppUIDs() []uint32 { return l.uids }
func (l *memoryRoundLister) RespondToMemoryPressure(ctx context.Context, samples []MemorySample, host HostMemorySample) {
	l.respond(ctx, samples, host)
}

type uidOnlyLister struct{ uids []uint32 }

func (l uidOnlyLister) ListAppUIDs() []uint32 { return l.uids }

func TestMonitorMemoryResponderReceivesOneCompleteRoundUnlocked(t *testing.T) {
	lister := &memoryRoundLister{uids: []uint32{4000000001, 4000000002}}
	m := New(events.NewBus(), lister)
	var sampled []uint32
	hostReads, callbacks := 0, 0
	host := HostMemorySample{TotalBytes: 2000000000, AvailableBytes: 700000000, Valid: true}
	lister.respond = func(ctx context.Context, samples []MemorySample, gotHost HostMemorySample) {
		callbacks++
		if !m.mu.TryLock() {
			t.Fatal("callback under monitor lock")
		}
		m.mu.Unlock()
		if ctx.Err() != nil || gotHost != host || len(samples) != 2 || samples[0].UID != lister.uids[0] || samples[1].UID != lister.uids[1] {
			t.Fatalf("wrong complete round: samples=%+v host=%+v", samples, gotHost)
		}
		if samples[1].Valid {
			t.Fatal("invalid sample omitted or changed")
		}
	}
	m.pollWithMemoryReaders(context.Background(), func(uid uint32) MemorySample {
		sampled = append(sampled, uid)
		return MemorySample{UID: uid, Valid: uid == lister.uids[0]}
	}, func() HostMemorySample {
		hostReads++
		if !reflect.DeepEqual(sampled, lister.uids) {
			t.Fatal("host read before app samples completed")
		}
		return host
	})
	if callbacks != 1 || hostReads != 1 || !reflect.DeepEqual(sampled, lister.uids) {
		t.Fatalf("callbacks=%d hostReads=%d sampled=%v", callbacks, hostReads, sampled)
	}
}

func TestMonitorMemoryResponderCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lister := &memoryRoundLister{uids: []uint32{4000000001, 4000000002}, respond: func(context.Context, []MemorySample, HostMemorySample) {
		t.Fatal("callback invoked after cancellation")
	}}
	m := New(events.NewBus(), lister)
	reads := 0
	m.pollWithMemoryReaders(ctx, func(uid uint32) MemorySample {
		reads++
		cancel()
		return MemorySample{UID: uid}
	}, func() HostMemorySample { return HostMemorySample{} })
	if reads != 1 {
		t.Fatalf("continued reading cancelled round: %d", reads)
	}
	m.pollWithMemoryReaders(ctx, func(uint32) MemorySample {
		t.Fatal("sampled cancelled poll")
		return MemorySample{}
	}, func() HostMemorySample {
		t.Fatal("read host in cancelled poll")
		return HostMemorySample{}
	})
}

func TestMonitorWithoutMemoryResponderDoesNotReadControlSamples(t *testing.T) {
	m := New(events.NewBus(), uidOnlyLister{uids: []uint32{4000000001}})
	m.pollWithMemoryReaders(context.Background(), func(uint32) MemorySample {
		t.Fatal("sampled without optional responder")
		return MemorySample{}
	}, func() HostMemorySample {
		t.Fatal("read host without optional responder")
		return HostMemorySample{}
	})
	if len(m.state) != 1 {
		t.Fatal("existing monitoring stopped")
	}
}

func TestMonitorStopCancelsActiveMemoryResponder(t *testing.T) {
	entered, exited := make(chan struct{}), make(chan struct{})
	lister := &memoryRoundLister{respond: func(ctx context.Context, samples []MemorySample, host HostMemorySample) {
		close(entered)
		<-ctx.Done()
		close(exited)
	}}
	m := New(events.NewBus(), lister)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		_ = m.Stop(context.Background())
		t.Fatal("responder did not run")
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("Stop did not cancel responder and await its exit")
	}
}
