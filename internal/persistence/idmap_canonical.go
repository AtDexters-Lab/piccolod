package persistence

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/blake2b"
)

// canonicalIDMapBytes serializes an IDMapMeta into a fixed 24-byte big-endian
// (no — little-endian, see the doc comment) representation suitable for
// fingerprinting. The byte format is fixed and MUST NOT change once shipped:
// six little-endian uint32 fields concatenated in declaration order.
//
//	bytes[0:4]   = AppUID      (le32)
//	bytes[4:8]   = AppGID      (le32)
//	bytes[8:12]  = SubUIDStart (le32)
//	bytes[12:16] = SubUIDCount (le32)
//	bytes[16:20] = SubGIDStart (le32)
//	bytes[20:24] = SubGIDCount (le32)
//
// Why not JSON: JSON canonicalization (key order, whitespace, integer encoding)
// is an avoidable footgun across implementations. A fixed binary format with a
// golden-byte test locks the format against accidental mutation.
func canonicalIDMapBytes(m IDMapMeta) []byte {
	out := make([]byte, 24)
	binary.LittleEndian.PutUint32(out[0:4], m.AppUID)
	binary.LittleEndian.PutUint32(out[4:8], m.AppGID)
	binary.LittleEndian.PutUint32(out[8:12], m.SubUIDStart)
	binary.LittleEndian.PutUint32(out[12:16], m.SubUIDCount)
	binary.LittleEndian.PutUint32(out[16:20], m.SubGIDStart)
	binary.LittleEndian.PutUint32(out[20:24], m.SubGIDCount)
	return out
}

// computeIDMapFingerprint returns the lowercase hex BLAKE2b-256 of the
// canonical 24-byte IDMap encoding. Empty IDMap (no idmap) returns empty
// string by convention so persisted metadata distinguishes "no idmap" from
// "all-zero idmap."
func computeIDMapFingerprint(m *IDMapMeta) string {
	if m == nil {
		return ""
	}
	sum := blake2b.Sum256(canonicalIDMapBytes(*m))
	return hex.EncodeToString(sum[:])
}

// verifyIDMapFingerprint checks that the live IDMap matches its persisted
// fingerprint. Returns ErrIDMapImmutable on mismatch (wrapped with
// volumeID + observed values for triage). No-op when the fingerprint is
// not yet pinned (pre-shipment volumes — grandfathering path).
//
// Called from every rootfs attach return path so any code that returns a
// RootfsHandle has validated the IDMap against the immutability guard.
func verifyIDMapFingerprint(volumeID string, meta *volumeMetaV3) error {
	if meta.IDMap == nil || meta.IDMapFingerprint == "" {
		return nil
	}
	computed := computeIDMapFingerprint(meta.IDMap)
	if computed == meta.IDMapFingerprint {
		return nil
	}
	return fmt.Errorf("rootfs %s: IDMap fingerprint mismatch (persisted=%s computed=%s); volume in unrecoverable state — logged for diagnostics: %w",
		volumeID, meta.IDMapFingerprint, computed, ErrIDMapImmutable)
}

// ErrIDMapImmutable is declared in attach_state.go — see that file for the
// canonical declaration. This file owns the canonicalization helper only.
