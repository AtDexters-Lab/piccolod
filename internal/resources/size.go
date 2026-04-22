// Package resources implements resource-stewardship derivation and policy
// computation for piccolo apps. Authors declare shape (priority, profile)
// and floors (min_required); runtime derives kernel knobs.
// See plans/resource-stewardship.md.
package resources

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// ParseSize parses a size string into bytes. Accepts decimal units (KB, MB,
// GB, TB — powers of 1000) and binary units (KiB, MiB, GiB, TiB — powers of
// 1024). The unit suffix is case-insensitive; a bare "B" or no suffix means
// bytes. Integer values only; fractional values are rejected to keep the
// manifest surface unambiguous.
//
// Examples: "6GB" → 6_000_000_000; "512MB" → 512_000_000; "500GiB" →
// 500 * 2^30; "1024" → 1024.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	m := sizePattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid size %q: expected <integer>[B|KB|MB|GB|TB|KiB|MiB|GiB|TiB]", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid size %q: must be non-negative", s)
	}
	mult := sizeMultiplier(m[2])
	if mult == 0 {
		return 0, fmt.Errorf("invalid size unit %q", m[2])
	}
	// Overflow guard: n*mult must fit in int64. Without this, pathological
	// inputs like "9999999TiB" silently wrap to negative and bypass
	// downstream install-gate thresholds that assume positive bytes.
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q overflows int64", s)
	}
	return n * mult, nil
}

var sizePattern = regexp.MustCompile(`^(\d+)\s*([A-Za-z]*)$`)

func sizeMultiplier(unit string) int64 {
	switch strings.ToUpper(unit) {
	case "", "B":
		return 1
	case "KB":
		return 1000
	case "MB":
		return 1000 * 1000
	case "GB":
		return 1000 * 1000 * 1000
	case "TB":
		return 1000 * 1000 * 1000 * 1000
	case "KIB":
		return 1024
	case "MIB":
		return 1024 * 1024
	case "GIB":
		return 1024 * 1024 * 1024
	case "TIB":
		return 1024 * 1024 * 1024 * 1024
	}
	return 0
}
