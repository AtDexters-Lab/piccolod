package autounlock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const handoffSchemaVersion = 1

const handoffPhasePreparing = "preparing"

type handoffMetadata struct {
	SchemaVersion int    `json:"schema_version"`
	ProviderID    string `json:"provider_id"`
	Phase         string `json:"phase,omitempty"`
	Expiry        string `json:"expiry,omitempty"`
	BlobSHA256    string `json:"blob_sha256"`
}

type resolvedHandoff struct {
	blob       []byte
	providerID string
	expiry     time.Time
}

func rawBlobDigest(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

func setHandoffMetadata(state *State, providerID string, expiry time.Time, blob []byte) error {
	raw, err := json.Marshal(handoffMetadata{
		SchemaVersion: handoffSchemaVersion,
		ProviderID:    providerID,
		Expiry:        expiry.UTC().Format(time.RFC3339Nano),
		BlobSHA256:    rawBlobDigest(blob),
	})
	if err != nil {
		return err
	}
	state.Handoff = raw
	return nil
}

// setPreparingHandoffMetadata persists provider dispatch authority before the
// raw blob is written. A crash after the blob write can therefore never
// reinterpret a custom-provider handoff as a legacy Namek handoff. The random
// factor and SDEK remain absent from this non-secret record.
func setPreparingHandoffMetadata(state *State, providerID string, blob []byte) error {
	raw, err := json.Marshal(handoffMetadata{
		SchemaVersion: handoffSchemaVersion,
		ProviderID:    providerID,
		Phase:         handoffPhasePreparing,
		BlobSHA256:    rawBlobDigest(blob),
	})
	if err != nil {
		return err
	}
	state.Handoff = raw
	return nil
}

// metadataDigest extracts only the digest. Reconciliation deliberately calls
// this before interpreting schema or provider, so stale metadata from any
// generation can be discarded safely when it describes a different raw blob.
func metadataDigest(raw json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", ErrHandoffMetadataInvalid
	}
	var digest string
	if field, ok := fields["blob_sha256"]; !ok || json.Unmarshal(field, &digest) != nil {
		return "", ErrHandoffMetadataInvalid
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return "", ErrHandoffMetadataInvalid
	}
	return hex.EncodeToString(decoded), nil
}

func decodeHandoffMetadata(raw json.RawMessage) (handoffMetadata, time.Time, error) {
	var meta handoffMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return handoffMetadata{}, time.Time{}, ErrHandoffMetadataInvalid
	}
	if meta.SchemaVersion != handoffSchemaVersion || meta.ProviderID == "" || meta.BlobSHA256 == "" {
		return handoffMetadata{}, time.Time{}, ErrHandoffMetadataInvalid
	}
	if meta.Phase == handoffPhasePreparing && meta.Expiry == "" {
		return meta, time.Time{}, nil
	}
	if meta.Phase != "" || meta.Expiry == "" {
		return handoffMetadata{}, time.Time{}, ErrHandoffMetadataInvalid
	}
	expiry, err := time.Parse(time.RFC3339Nano, meta.Expiry)
	if err != nil {
		return handoffMetadata{}, time.Time{}, ErrHandoffMetadataInvalid
	}
	return meta, expiry, nil
}

// reconcileHandoffLocked implements the rolling-upgrade matrix. The caller
// must hold the operation gate.
func (o *Orchestrator) reconcileHandoffLocked(state *State, stateErr error) (resolvedHandoff, error) {
	blob, err := ReadBlob()
	if errors.Is(err, ErrBlobMissing) {
		o.clearHandoffClaimsLocked()
		if stateErr == nil && len(state.Handoff) != 0 {
			state.Handoff = nil
			if saveErr := SaveState(*state); saveErr != nil {
				return resolvedHandoff{}, fmt.Errorf("autounlock: clear orphan handoff metadata: %w", saveErr)
			}
		}
		return resolvedHandoff{}, ErrBlobMissing
	}
	if err != nil {
		return resolvedHandoff{}, err
	}
	if stateErr != nil {
		// A blob plus an unreadable state file could contain matching future
		// metadata. Treating it as legacy would be an unsafe downgrade.
		return resolvedHandoff{}, ErrHandoffMetadataInvalid
	}
	if len(state.Handoff) == 0 {
		return resolvedHandoff{blob: blob, providerID: namekV1ProviderID}, nil
	}

	digest, err := metadataDigest(state.Handoff)
	if err != nil {
		return resolvedHandoff{}, err
	}
	if digest != rawBlobDigest(blob) {
		state.Handoff = nil
		if err := SaveState(*state); err != nil {
			return resolvedHandoff{}, fmt.Errorf("autounlock: clear stale handoff metadata: %w", err)
		}
		return resolvedHandoff{blob: blob, providerID: namekV1ProviderID}, nil
	}

	meta, expiry, err := decodeHandoffMetadata(state.Handoff)
	if err != nil {
		return resolvedHandoff{}, err
	}
	if !o.isRecognizedProviderID(meta.ProviderID) {
		return resolvedHandoff{}, ErrHandoffMetadataInvalid
	}
	if !expiry.IsZero() && !expiry.After(o.deps.Now()) {
		if err := o.clearLocalHandoffLocked(state); err != nil {
			return resolvedHandoff{}, fmt.Errorf("autounlock: clear expired handoff: %w", err)
		}
		return resolvedHandoff{}, ErrEffectiveExpiryTooShort
	}
	return resolvedHandoff{blob: blob, providerID: meta.ProviderID, expiry: expiry}, nil
}

// clearLocalHandoffLocked deletes the raw blob first. A crash between the two
// writes therefore leaves harmless orphan metadata, which reconciliation
// clears without provider pickup. Reversing the order could resurrect a raw
// blob as a legacy handoff after cancellation.
func (o *Orchestrator) clearLocalHandoffLocked(state *State) error {
	if err := DeleteBlob(); err != nil {
		return err
	}
	o.clearHandoffClaimsLocked()
	if len(state.Handoff) == 0 {
		return nil
	}
	state.Handoff = nil
	return SaveState(*state)
}

func (o *Orchestrator) clearHandoffClaimsLocked() {
	o.restartHandoffClaimDigest = ""
	o.taskWarningHandoffClaimDigest = ""
}
