package pressure

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MemorySample describes the whole rootless app user slice. Valid requires all
// control inputs to be readable and finite; invalid samples must not actuate.
type MemorySample struct {
	UID          uint32
	At           time.Time
	CgroupID     uint64
	CurrentBytes int64
	HighBytes    int64
	MaxBytes     int64
	HighEvents   uint64
	SomeAvg10    float64
	FullAvg10    float64
	Valid        bool
}

// HostMemorySample supplies physical host headroom, including reclaimable
// memory. Global PSI is deliberately excluded: app-local memcg stalls can
// contribute to it even when physical memory is available.
type HostMemorySample struct {
	TotalBytes     int64
	AvailableBytes int64
	Valid          bool
}

func ReadMemorySample(uid uint32) MemorySample {
	return readMemorySamplePath(uid, fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice", uid), time.Now())
}

func ReadHostMemorySample() HostMemorySample {
	return readHostMemorySamplePath("/proc/meminfo")
}

func readMemorySamplePath(uid uint32, base string, at time.Time) MemorySample {
	s := MemorySample{UID: uid, At: at}
	id, ok := cgroupIdentity(base)
	if !ok {
		return s
	}
	s.CgroupID = id
	var err error
	if s.CurrentBytes, err = readMemoryBytes(filepath.Join(base, "memory.current")); err != nil {
		return s
	}
	if s.HighBytes, err = readMemoryBytes(filepath.Join(base, "memory.high")); err != nil || s.HighBytes == 0 {
		return s
	}
	if s.MaxBytes, err = readMemoryBytes(filepath.Join(base, "memory.max")); err != nil || s.MaxBytes == 0 || s.HighBytes > s.MaxBytes {
		return s
	}
	if s.HighEvents, err = readHighEvents(filepath.Join(base, "memory.events.local")); err != nil {
		return s
	}
	if s.SomeAvg10, s.FullAvg10, err = readMemoryPSI(filepath.Join(base, "memory.pressure")); err != nil {
		return s
	}
	// A recreation during I/O cannot produce a coherent sample.
	endID, ok := cgroupIdentity(base)
	s.Valid = ok && endID == id
	return s
}

func cgroupIdentity(path string) (uint64, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return 0, false
	}
	return stat.Ino, true
}

func readMemoryBytes(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid memory bytes in %s", path)
	}
	return v, nil
}

func readHighEvents(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var high uint64
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return 0, fmt.Errorf("invalid memory event")
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		if fields[0] == "high" {
			if found {
				return 0, fmt.Errorf("duplicate high counter")
			}
			high, found = v, true
		}
	}
	if !found {
		return 0, fmt.Errorf("missing high counter")
	}
	return high, nil
}

func readMemoryPSI(path string) (float64, float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	averages := make(map[string]float64, 2)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 || (fields[0] != "some" && fields[0] != "full") {
			return 0, 0, fmt.Errorf("invalid memory PSI line")
		}
		if _, duplicate := averages[fields[0]]; duplicate {
			return 0, 0, fmt.Errorf("duplicate memory PSI line")
		}
		values := make(map[string]string, 4)
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				return 0, 0, fmt.Errorf("invalid memory PSI field")
			}
			if _, duplicate := values[key]; duplicate {
				return 0, 0, fmt.Errorf("duplicate memory PSI field")
			}
			values[key] = value
		}
		for _, key := range []string{"avg10", "avg60", "avg300"} {
			v, err := strconv.ParseFloat(values[key], 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
				return 0, 0, fmt.Errorf("invalid memory PSI average")
			}
			if key == "avg10" {
				averages[fields[0]] = v
			}
		}
		if _, err := strconv.ParseUint(values["total"], 10, 64); err != nil {
			return 0, 0, fmt.Errorf("invalid memory PSI total")
		}
	}
	if len(averages) != 2 {
		return 0, 0, fmt.Errorf("missing memory PSI line")
	}
	return averages["some"], averages["full"], nil
}

func readHostMemorySamplePath(path string) HostMemorySample {
	f, err := os.Open(path)
	if err != nil {
		return HostMemorySample{}
	}
	defer f.Close()
	var s HostMemorySample
	seen := make(map[string]bool, 2)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || (fields[0] != "MemTotal:" && fields[0] != "MemAvailable:") {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" || seen[fields[0]] {
			return HostMemorySample{}
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || v < 0 || v > math.MaxInt64/1024 {
			return HostMemorySample{}
		}
		seen[fields[0]] = true
		if fields[0] == "MemTotal:" {
			s.TotalBytes = v * 1024
		} else {
			s.AvailableBytes = v * 1024
		}
	}
	s.Valid = sc.Err() == nil && len(seen) == 2 && s.TotalBytes > 0 && s.AvailableBytes <= s.TotalBytes
	return s
}
