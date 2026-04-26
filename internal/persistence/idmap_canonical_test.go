package persistence

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// TestCanonicalIDMapBytes_GoldenBytes locks the canonical byte format. If
// this test fails after a code change, the byte format has been mutated —
// any volume with a persisted fingerprint will report
// AttachStateForeignMountAtPath at next attach. DO NOT update the expected
// bytes to make this test pass; revert the code change instead.
func TestCanonicalIDMapBytes_GoldenBytes(t *testing.T) {
	// Canonical input: a deliberately asymmetric IDMap so each field is
	// observable in the output. AppUID=0x01020304, AppGID=0x05060708,
	// SubUIDStart=0x09000000, SubUIDCount=0x00010000, SubGIDStart=0x07000000,
	// SubGIDCount=0x00040000.
	in := IDMapMeta{
		AppUID:      0x01020304,
		AppGID:      0x05060708,
		SubUIDStart: 0x09000000,
		SubUIDCount: 0x00010000,
		SubGIDStart: 0x07000000,
		SubGIDCount: 0x00040000,
	}

	got := canonicalIDMapBytes(in)

	// Expected: little-endian uint32 concatenation in declaration order.
	want := []byte{
		0x04, 0x03, 0x02, 0x01, // AppUID
		0x08, 0x07, 0x06, 0x05, // AppGID
		0x00, 0x00, 0x00, 0x09, // SubUIDStart
		0x00, 0x00, 0x01, 0x00, // SubUIDCount
		0x00, 0x00, 0x00, 0x07, // SubGIDStart
		0x00, 0x00, 0x04, 0x00, // SubGIDCount
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical bytes mutated:\n  got:  %s\n  want: %s",
			hex.EncodeToString(got), hex.EncodeToString(want))
	}
	if len(got) != 24 {
		t.Fatalf("canonical bytes length: got %d, want 24", len(got))
	}
}

func TestCanonicalIDMapBytes_AllZero(t *testing.T) {
	got := canonicalIDMapBytes(IDMapMeta{})
	want := make([]byte, 24)
	if !bytes.Equal(got, want) {
		t.Fatalf("zero-value canonical bytes: got %s, want all-zero", hex.EncodeToString(got))
	}
}

func TestComputeIDMapFingerprint_Stable(t *testing.T) {
	in := &IDMapMeta{
		AppUID:      1000,
		AppGID:      1000,
		SubUIDStart: 100000,
		SubUIDCount: 65536,
		SubGIDStart: 100000,
		SubGIDCount: 65536,
	}
	a := computeIDMapFingerprint(in)
	b := computeIDMapFingerprint(in)
	if a != b {
		t.Fatalf("fingerprint not stable: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("fingerprint length: got %d, want 64 (BLAKE2b-256 hex)", len(a))
	}
}

func TestComputeIDMapFingerprint_Distinct(t *testing.T) {
	a := computeIDMapFingerprint(&IDMapMeta{AppUID: 1000})
	b := computeIDMapFingerprint(&IDMapMeta{AppUID: 1001})
	if a == b {
		t.Fatalf("expected different fingerprints for AppUID 1000 vs 1001")
	}
}

func TestComputeIDMapFingerprint_NilEmpty(t *testing.T) {
	if got := computeIDMapFingerprint(nil); got != "" {
		t.Fatalf("nil IDMap → fingerprint, got %q, want empty", got)
	}
}
