package autounlock

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"piccolod/internal/crypt"
	"piccolod/internal/state/paths"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

func TestRecoverReconciliationMatrix(t *testing.T) {
	t.Run("no blob clears orphan metadata without pickup", func(t *testing.T) {
		o, _, nc, _ := newOrchestrator(t)
		state := State{Enabled: true}
		if err := setHandoffMetadata(&state, namekV1ProviderID, o.deps.Now().Add(5*time.Minute), []byte("older")); err != nil {
			t.Fatal(err)
		}
		if err := SaveState(state); err != nil {
			t.Fatal(err)
		}

		if got := o.RunPickup(context.Background(), neverCalledChain(t)); got != PickupOutcomeNoBlob {
			t.Fatalf("outcome = %v; want NoBlob", got)
		}
		if nc.pickupCount != 0 {
			t.Fatalf("provider pickup count = %d; want 0", nc.pickupCount)
		}
		got, _ := LoadState()
		if len(got.Handoff) != 0 {
			t.Fatalf("orphan metadata retained: %s", got.Handoff)
		}
	})

	t.Run("raw blob without metadata is legacy namek v1", func(t *testing.T) {
		o, mgr, nc, _ := newOrchestrator(t)
		if err := SaveState(State{Enabled: true}); err != nil {
			t.Fatal(err)
		}
		blob := []byte("legacy-opaque-blob")
		if err := WriteBlob(blob); err != nil {
			t.Fatal(err)
		}
		factor := bytes.Repeat([]byte{0x31}, fSize)
		nc.pickupResp = &namekclient.PickupUnlockEscrowResponse{Secret: base64.RawURLEncoding.EncodeToString(factor)}

		if got := o.RunPickup(context.Background(), func(context.Context) error { return nil }); got != PickupOutcomeUnlocked {
			t.Fatalf("outcome = %v; want Unlocked", got)
		}
		if nc.pickupCount != 1 {
			t.Fatalf("provider pickup count = %d; want 1", nc.pickupCount)
		}
		if !bytes.Equal(mgr.lastUnwrapAAD, []byte("dev-test-1|"+aadSuffix)) {
			t.Fatalf("AAD changed: %q", mgr.lastUnwrapAAD)
		}
	})

	t.Run("preparing custom metadata without raw blob is orphaned", func(t *testing.T) {
		o, _, nc, _ := newOrchestrator(t)
		provider := &observingProvider{factor: bytes.Repeat([]byte{0x30}, fSize)}
		o.deps.RecoveryProvider = func() RecoveryFactorProvider { return provider }
		o.deps.RecoveryProviderID = "custom-v1"
		state := State{Enabled: true}
		if err := setPreparingHandoffMetadata(&state, "custom-v1", []byte("not-yet-written")); err != nil {
			t.Fatal(err)
		}
		if err := SaveState(state); err != nil {
			t.Fatal(err)
		}

		if got := o.RunPickup(context.Background(), neverCalledChain(t)); got != PickupOutcomeNoBlob {
			t.Fatalf("outcome = %v; want NoBlob", got)
		}
		if provider.pickupCount != 0 || nc.pickupCount != 0 {
			t.Fatalf("orphan metadata reached provider: custom/namek = %d/%d", provider.pickupCount, nc.pickupCount)
		}
		got, err := LoadState()
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Handoff) != 0 {
			t.Fatalf("orphan preparing metadata retained: %s", got.Handoff)
		}
	})

	t.Run("raw blob before custom deposit dispatches custom and never namek", func(t *testing.T) {
		o, _, nc, _ := newOrchestrator(t)
		provider := &observingProvider{}
		o.deps.RecoveryProvider = func() RecoveryFactorProvider { return provider }
		o.deps.RecoveryProviderID = "custom-v1"
		blob := []byte("custom-before-deposit")
		state := State{Enabled: true}
		if err := setPreparingHandoffMetadata(&state, "custom-v1", blob); err != nil {
			t.Fatal(err)
		}
		if err := SaveState(state); err != nil {
			t.Fatal(err)
		}
		if err := WriteBlob(blob); err != nil {
			t.Fatal(err)
		}

		if got := o.RunPickup(context.Background(), neverCalledChain(t)); got != PickupOutcomeFailed {
			t.Fatalf("outcome = %v; want Failed", got)
		}
		if provider.pickupCount != 1 || nc.pickupCount != 0 {
			t.Fatalf("provider dispatch custom/namek = %d/%d; want 1/0", provider.pickupCount, nc.pickupCount)
		}
		if !BlobExists() {
			t.Fatal("pre-deposit crash handoff was consumed")
		}
	})

	t.Run("custom deposit before final metadata recovers through custom provider", func(t *testing.T) {
		o, mgr, nc, _ := newOrchestrator(t)
		factor := bytes.Repeat([]byte{0x34}, fSize)
		provider := &observingProvider{factor: factor}
		o.deps.RecoveryProvider = func() RecoveryFactorProvider { return provider }
		o.deps.RecoveryProviderID = "custom-v1"
		blob := []byte("custom-after-deposit")
		state := State{Enabled: true}
		if err := setPreparingHandoffMetadata(&state, "custom-v1", blob); err != nil {
			t.Fatal(err)
		}
		if err := SaveState(state); err != nil {
			t.Fatal(err)
		}
		if err := WriteBlob(blob); err != nil {
			t.Fatal(err)
		}

		if got := o.RunPickup(context.Background(), func(context.Context) error { return nil }); got != PickupOutcomeUnlocked {
			t.Fatalf("outcome = %v; want Unlocked", got)
		}
		if provider.pickupCount != 1 || nc.pickupCount != 0 {
			t.Fatalf("provider dispatch custom/namek = %d/%d; want 1/0", provider.pickupCount, nc.pickupCount)
		}
		if !bytes.Equal(mgr.lastUnwrapF, factor) {
			t.Fatalf("custom factor changed before unwrap")
		}
	})

	t.Run("namek deposit before final metadata remains recoverable", func(t *testing.T) {
		o, mgr, nc, _ := newOrchestrator(t)
		factor := bytes.Repeat([]byte{0x35}, fSize)
		nc.pickupResp = &namekclient.PickupUnlockEscrowResponse{Secret: base64.RawURLEncoding.EncodeToString(factor)}
		blob := []byte("namek-after-deposit")
		state := State{Enabled: true}
		if err := setPreparingHandoffMetadata(&state, namekV1ProviderID, blob); err != nil {
			t.Fatal(err)
		}
		if err := SaveState(state); err != nil {
			t.Fatal(err)
		}
		if err := WriteBlob(blob); err != nil {
			t.Fatal(err)
		}

		if got := o.RunPickup(context.Background(), func(context.Context) error { return nil }); got != PickupOutcomeUnlocked {
			t.Fatalf("outcome = %v; want Unlocked", got)
		}
		if nc.pickupCount != 1 {
			t.Fatalf("namek pickup count = %d; want 1", nc.pickupCount)
		}
		if !bytes.Equal(mgr.lastUnwrapF, factor) {
			t.Fatalf("namek factor changed before unwrap")
		}
	})

	t.Run("digest mismatch clears unknown metadata and uses current blob as legacy", func(t *testing.T) {
		o, _, nc, _ := newOrchestrator(t)
		currentBlob := []byte("current-raw-blob")
		if err := WriteBlob(currentBlob); err != nil {
			t.Fatal(err)
		}
		state := State{Enabled: true}
		state.Handoff = mustMetadataJSON(t, handoffMetadata{
			SchemaVersion: 77,
			ProviderID:    "future-provider",
			Expiry:        o.deps.Now().Add(5 * time.Minute).Format(time.RFC3339Nano),
			BlobSHA256:    rawBlobDigest([]byte("older-raw-blob")),
		})
		if err := SaveState(state); err != nil {
			t.Fatal(err)
		}
		factor := bytes.Repeat([]byte{0x32}, fSize)
		nc.pickupResp = &namekclient.PickupUnlockEscrowResponse{Secret: base64.RawURLEncoding.EncodeToString(factor)}

		if got := o.RunPickup(context.Background(), func(context.Context) error { return nil }); got != PickupOutcomeUnlocked {
			t.Fatalf("outcome = %v; want Unlocked", got)
		}
		if nc.pickupCount != 1 {
			t.Fatalf("provider pickup count = %d; want 1", nc.pickupCount)
		}
	})

	t.Run("matching recognized metadata dispatches", func(t *testing.T) {
		o, mgr, nc, _ := newOrchestrator(t)
		blob := []byte("metadata-bound-raw-blob")
		if err := WriteBlob(blob); err != nil {
			t.Fatal(err)
		}
		state := State{Enabled: true}
		if err := setHandoffMetadata(&state, namekV1ProviderID, o.deps.Now().Add(5*time.Minute), blob); err != nil {
			t.Fatal(err)
		}
		if err := SaveState(state); err != nil {
			t.Fatal(err)
		}
		factor := bytes.Repeat([]byte{0x33}, fSize)
		nc.pickupResp = &namekclient.PickupUnlockEscrowResponse{Secret: base64.RawURLEncoding.EncodeToString(factor)}

		if got := o.RunPickup(context.Background(), func(context.Context) error { return nil }); got != PickupOutcomeUnlocked {
			t.Fatalf("outcome = %v; want Unlocked", got)
		}
		if !bytes.Equal(mgr.lastUnwrapF, factor) {
			t.Fatalf("provider factor changed before unwrap")
		}
	})

	t.Run("recorded lease below retry margin clears locally without pickup", func(t *testing.T) {
		o, _, nc, _ := newOrchestrator(t)
		blob := []byte("short-retry-margin-blob")
		if err := WriteBlob(blob); err != nil {
			t.Fatal(err)
		}
		state := State{Enabled: true}
		if err := setHandoffMetadata(&state, namekV1ProviderID, o.deps.Now().Add(minimumRecoveryAttemptLifetime-time.Second), blob); err != nil {
			t.Fatal(err)
		}
		if err := SaveState(state); err != nil {
			t.Fatal(err)
		}

		result, err := o.Recover(context.Background(), neverCalledChain(t))
		if !errors.Is(err, ErrEffectiveExpiryTooShort) {
			t.Fatalf("Recover err = %v; want ErrEffectiveExpiryTooShort", err)
		}
		if result.Disposition != RecoverDispositionManualUnlockRequired || result.Reason != ReasonEscrowNotFound {
			t.Fatalf("Recover result = %+v", result)
		}
		if nc.pickupCount != 0 {
			t.Fatalf("provider pickup count = %d; want 0", nc.pickupCount)
		}
		if BlobExists() {
			t.Fatal("short-lived retry retained local blob")
		}
		gotState, stateErr := LoadState()
		if stateErr != nil {
			t.Fatal(stateErr)
		}
		if len(gotState.Handoff) != 0 {
			t.Fatalf("short-lived retry retained metadata: %s", gotState.Handoff)
		}
	})

	for _, tc := range []struct {
		name string
		meta func(t *testing.T, o *Orchestrator, blob []byte) json.RawMessage
	}{
		{
			name: "matching unknown provider fails closed",
			meta: func(t *testing.T, o *Orchestrator, blob []byte) json.RawMessage {
				return mustMetadataJSON(t, handoffMetadata{
					SchemaVersion: handoffSchemaVersion,
					ProviderID:    "future-provider",
					Expiry:        o.deps.Now().Add(5 * time.Minute).Format(time.RFC3339Nano),
					BlobSHA256:    rawBlobDigest(blob),
				})
			},
		},
		{
			name: "matching malformed metadata fails closed",
			meta: func(t *testing.T, o *Orchestrator, blob []byte) json.RawMessage {
				return json.RawMessage(`{"schema_version":"bad","provider_id":"namek-v1","expiry":"` + o.deps.Now().Add(5*time.Minute).Format(time.RFC3339Nano) + `","blob_sha256":"` + rawBlobDigest(blob) + `"}`)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _, nc, _ := newOrchestrator(t)
			blob := []byte("matching-raw-blob")
			if err := WriteBlob(blob); err != nil {
				t.Fatal(err)
			}
			if err := SaveState(State{Enabled: true, Handoff: tc.meta(t, o, blob)}); err != nil {
				t.Fatal(err)
			}

			if got := o.RunPickup(context.Background(), neverCalledChain(t)); got != PickupOutcomeFailed {
				t.Fatalf("outcome = %v; want Failed", got)
			}
			if nc.pickupCount != 0 {
				t.Fatalf("provider pickup count = %d; want 0", nc.pickupCount)
			}
			if !BlobExists() {
				t.Fatalf("fail-closed path consumed raw blob")
			}
		})
	}

	t.Run("expired recognized metadata deletes local handoff", func(t *testing.T) {
		o, _, nc, _ := newOrchestrator(t)
		blob := []byte("expired-raw-blob")
		if err := WriteBlob(blob); err != nil {
			t.Fatal(err)
		}
		state := State{Enabled: true}
		if err := setHandoffMetadata(&state, namekV1ProviderID, o.deps.Now().Add(-time.Second), blob); err != nil {
			t.Fatal(err)
		}
		if err := SaveState(state); err != nil {
			t.Fatal(err)
		}

		if got := o.RunPickup(context.Background(), neverCalledChain(t)); got != PickupOutcomeFailed {
			t.Fatalf("outcome = %v; want Failed", got)
		}
		if nc.pickupCount != 0 {
			t.Fatalf("provider pickup count = %d; want 0", nc.pickupCount)
		}
		if BlobExists() {
			t.Fatalf("expired raw blob retained")
		}
		got, _ := LoadState()
		if len(got.Handoff) != 0 {
			t.Fatalf("expired metadata retained: %s", got.Handoff)
		}
	})

	t.Run("unreadable state with blob remains fail closed across retries", func(t *testing.T) {
		o, _, nc, _ := newOrchestrator(t)
		if err := WriteBlob([]byte("raw-blob-with-unknown-state")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath(), []byte(`{"handoff":`), 0o600); err != nil {
			t.Fatal(err)
		}

		for attempt := 0; attempt < 2; attempt++ {
			if got := o.RunPickup(context.Background(), neverCalledChain(t)); got != PickupOutcomeFailed {
				t.Fatalf("attempt %d outcome = %v; want Failed", attempt+1, got)
			}
		}
		if nc.pickupCount != 0 {
			t.Fatalf("provider pickup count = %d; want 0", nc.pickupCount)
		}
		if _, err := LoadState(); !errors.Is(err, ErrInvalidStateFile) {
			t.Fatalf("invalid state was rewritten and could downgrade next retry: %v", err)
		}
	})
}

func TestPreparePreservesRawFormatWriteOrderAndNoPlaintextPersistence(t *testing.T) {
	o, _, nc, _ := newOrchestrator(t)
	mgr := &opaqueRecordingManager{
		loaded: true,
		blob:   []byte("opaque-aead-v1-bytes"),
		sdek:   []byte("sentinel-plaintext-sdek-must-not-persist"),
	}
	o.deps.Manager = mgr
	nc.depositResp = &namekclient.DepositUnlockEscrowResponse{
		ExpiresAt:              o.deps.Now().Add(10 * time.Minute).Format(time.RFC3339Nano),
		EffectiveWindowSeconds: 600,
	}
	provider := &observingProvider{
		expiry: o.deps.Now().Add(10 * time.Minute),
		onDeposit: func() {
			if got, err := ReadBlob(); err != nil || !bytes.Equal(got, mgr.blob) {
				t.Errorf("provider deposit observed blob = %q, %v; want exact raw blob", got, err)
			}
			state, err := LoadState()
			if err != nil {
				t.Errorf("LoadState during deposit: %v", err)
			} else {
				meta, expiry, metaErr := decodeHandoffMetadata(state.Handoff)
				if metaErr != nil {
					t.Errorf("decode preparing metadata: %v", metaErr)
				} else if meta.ProviderID != "test-provider" || meta.Phase != handoffPhasePreparing || !expiry.IsZero() || meta.BlobSHA256 != rawBlobDigest(mgr.blob) {
					t.Errorf("pre-deposit dispatch metadata = %+v expiry=%s", meta, expiry)
				}
			}
		},
	}
	o.deps.RecoveryProvider = func() RecoveryFactorProvider { return provider }
	o.deps.RecoveryProviderID = "test-provider"

	result, err := o.Prepare(context.Background(), PrepareTriggerTaskWarning, 10*time.Minute)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if result.Disposition != PrepareDispositionPrepared {
		t.Fatalf("disposition = %q; want prepared", result.Disposition)
	}
	rawBlob, err := ReadBlob()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rawBlob, mgr.blob) {
		t.Fatalf("raw blob changed: %q", rawBlob)
	}
	if !bytes.Equal(mgr.aad, []byte("dev-test-1|"+aadSuffix)) {
		t.Fatalf("AAD changed: %q", mgr.aad)
	}
	if provider.depositCount != 1 {
		t.Fatalf("provider deposit count = %d; want 1", provider.depositCount)
	}

	stateBytes, err := os.ReadFile(statePath())
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string][]byte{
		"factor raw":    mgr.factor,
		"factor base64": []byte(base64.RawURLEncoding.EncodeToString(mgr.factor)),
		"factor hex":    []byte(hex.EncodeToString(mgr.factor)),
		"SDEK":          mgr.sdek,
	} {
		if len(secret) != 0 && (bytes.Contains(stateBytes, secret) || bytes.Contains(rawBlob, secret)) {
			t.Fatalf("%s persisted in local handoff", name)
		}
	}

	var shape struct {
		Handoff map[string]any `json:"handoff"`
	}
	if err := json.Unmarshal(stateBytes, &shape); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(shape.Handoff))
	for key := range shape.Handoff {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	wantKeys := []string{"blob_sha256", "expiry", "provider_id", "schema_version"}
	if !equalStrings(keys, wantKeys) {
		t.Fatalf("handoff metadata keys = %v; want %v", keys, wantKeys)
	}
}

func TestCustomProviderFinalMetadataFailureRetainsPreparingDispatch(t *testing.T) {
	o, mgr, nc, _ := newOrchestrator(t)
	provider := &observingProvider{expiry: o.deps.Now().Add(10 * time.Minute)}
	var restore func() error
	var hookErr error
	provider.onDeposit = func() {
		restore, hookErr = forceNextStateSaveFailure()
	}
	o.deps.RecoveryProvider = func() RecoveryFactorProvider { return provider }
	o.deps.RecoveryProviderID = "custom-v1"

	result, err := o.Prepare(context.Background(), PrepareTriggerTaskWarning, 10*time.Minute)
	if err == nil || result.Disposition != PrepareDispositionUnavailable {
		t.Fatalf("Prepare = %+v, %v; want unavailable final metadata-save failure", result, err)
	}
	if hookErr != nil {
		t.Fatalf("install metadata-save failure: %v", hookErr)
	}
	if restore == nil {
		t.Fatal("provider deposit did not install metadata-save failure")
	}
	if err := restore(); err != nil {
		t.Fatalf("restore preparing metadata: %v", err)
	}

	blob, err := ReadBlob()
	if err != nil {
		t.Fatalf("retained custom blob: %v", err)
	}
	state, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	meta, expiry, err := decodeHandoffMetadata(state.Handoff)
	if err != nil {
		t.Fatalf("decode retained dispatch: %v", err)
	}
	if meta.ProviderID != "custom-v1" || meta.Phase != handoffPhasePreparing || !expiry.IsZero() || meta.BlobSHA256 != rawBlobDigest(blob) {
		t.Fatalf("retained dispatch = %+v expiry=%s", meta, expiry)
	}

	if got := o.RunPickup(context.Background(), func(context.Context) error { return nil }); got != PickupOutcomeUnlocked {
		t.Fatalf("outcome = %v; want Unlocked", got)
	}
	if provider.pickupCount != 1 || nc.pickupCount != 0 {
		t.Fatalf("provider dispatch custom/namek = %d/%d; want 1/0", provider.pickupCount, nc.pickupCount)
	}
	if !bytes.Equal(mgr.lastUnwrapF, provider.factor) {
		t.Fatal("retained custom factor changed before unwrap")
	}
}

func TestOutstandingHandoffIsExclusiveAndCleanupNeverRevokes(t *testing.T) {
	o, mgr, nc, _ := newOrchestrator(t)
	nc.pickupEchoesF = true

	first, err := o.Prepare(context.Background(), PrepareTriggerTaskWarning, 10*time.Minute)
	if err != nil || first.Disposition != PrepareDispositionPrepared {
		t.Fatalf("first Prepare = %+v, %v", first, err)
	}
	second, err := o.Prepare(context.Background(), PrepareTriggerGracefulShutdown, 10*time.Minute)
	if err != nil || second.Disposition != PrepareDispositionReused {
		t.Fatalf("second Prepare = %+v, %v; want reused", second, err)
	}
	if nc.depositCount != 1 || mgr.wrapCallCount != 1 {
		t.Fatalf("outstanding handoff overwritten: deposit/wrap = %d/%d", nc.depositCount, mgr.wrapCallCount)
	}

	beforeDeposit, beforePickup := nc.depositCount, nc.pickupCount
	if _, err := o.RunTest(context.Background()); !errors.Is(err, ErrHandoffBusy) {
		t.Fatalf("RunTest err = %v; want ErrHandoffBusy", err)
	}
	if nc.depositCount != beforeDeposit || nc.pickupCount != beforePickup {
		t.Fatalf("busy Test touched provider: deposit/pickup %d/%d -> %d/%d", beforeDeposit, beforePickup, nc.depositCount, nc.pickupCount)
	}

	if err := o.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if BlobExists() {
		t.Fatalf("Cancel retained raw blob")
	}
	state, _ := LoadState()
	if len(state.Handoff) != 0 {
		t.Fatalf("Cancel retained metadata: %s", state.Handoff)
	}
	if _, err := o.RunTest(context.Background()); err != nil {
		t.Fatalf("RunTest after Cancel: %v", err)
	}
	if err := o.Update(context.Background(), UpdateInput{Enabled: ptrBool(false)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if nc.revokeCount != 0 {
		t.Fatalf("legacy revoke called %d times", nc.revokeCount)
	}
}

func TestPreparePreservesNonReusableExistingHandoff(t *testing.T) {
	for _, trigger := range []PrepareTrigger{
		PrepareTriggerTaskWarning,
		PrepareTriggerGracefulShutdown,
		PrepareTriggerTaskCritical,
		PrepareTriggerRecoveryFatal,
	} {
		for _, tc := range []struct {
			name     string
			metadata func(t *testing.T, o *Orchestrator, blob []byte) json.RawMessage
		}{
			{
				name: "legacy expiry unknown",
			},
			{
				name: "recognized lifetime below minimum",
				metadata: func(t *testing.T, o *Orchestrator, blob []byte) json.RawMessage {
					t.Helper()
					state := State{}
					if err := setHandoffMetadata(&state, namekV1ProviderID, o.deps.Now().Add(minimumEffectiveLifetime-time.Second), blob); err != nil {
						t.Fatal(err)
					}
					return state.Handoff
				},
			},
			{
				name: "recognized lifetime below pickup margin",
				metadata: func(t *testing.T, o *Orchestrator, blob []byte) json.RawMessage {
					t.Helper()
					state := State{}
					if err := setHandoffMetadata(&state, namekV1ProviderID, o.deps.Now().Add(minimumRecoveryAttemptLifetime-time.Second), blob); err != nil {
						t.Fatal(err)
					}
					return state.Handoff
				},
			},
		} {
			t.Run(string(trigger)+"/"+tc.name, func(t *testing.T) {
				o, mgr, nc, _ := newOrchestrator(t)
				oldBlob := []byte("existing-nonreusable-handoff")
				if err := WriteBlob(oldBlob); err != nil {
					t.Fatal(err)
				}
				state := State{Enabled: true}
				if tc.metadata != nil {
					state.Handoff = tc.metadata(t, o, oldBlob)
				}
				if err := SaveState(state); err != nil {
					t.Fatal(err)
				}
				storedState, err := LoadState()
				if err != nil {
					t.Fatal(err)
				}
				beforeMetadata := append(json.RawMessage(nil), storedState.Handoff...)
				beforeStateBytes, err := os.ReadFile(statePath())
				if err != nil {
					t.Fatal(err)
				}
				beforeClaim := o.restartHandoffClaimDigest

				result, err := o.Prepare(context.Background(), trigger, 10*time.Minute)
				if err != nil {
					t.Fatalf("Prepare: %v", err)
				}
				if result.Disposition != PrepareDispositionUnavailable || !result.ExpiresAt.IsZero() {
					t.Fatalf("Prepare = %+v; want unavailable without an expiry", result)
				}
				if nc.depositCount != 0 || mgr.wrapCallCount != 0 {
					t.Fatalf("replacement performed deposit/wrap = %d/%d", nc.depositCount, mgr.wrapCallCount)
				}
				gotBlob, err := ReadBlob()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(gotBlob, oldBlob) {
					t.Fatalf("raw handoff changed: %q", gotBlob)
				}
				gotState, err := LoadState()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(gotState.Handoff, beforeMetadata) {
					t.Fatalf("handoff metadata changed: got %s, want %s", gotState.Handoff, beforeMetadata)
				}
				afterStateBytes, err := os.ReadFile(statePath())
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(afterStateBytes, beforeStateBytes) {
					t.Fatalf("persisted handoff state changed: got %s, want %s", afterStateBytes, beforeStateBytes)
				}
				if o.restartHandoffClaimDigest != beforeClaim {
					t.Fatalf("unavailable Prepare changed restart claim: got %q, want %q", o.restartHandoffClaimDigest, beforeClaim)
				}
			})
		}
	}
}

func TestPrepareReusesRecognizedHandoffAtMinimumLifetimeBoundary(t *testing.T) {
	o, mgr, nc, _ := newOrchestrator(t)
	blob := []byte("minimum-lifetime-boundary")
	if err := WriteBlob(blob); err != nil {
		t.Fatal(err)
	}
	expiry := o.deps.Now().Add(minimumEffectiveLifetime)
	state := State{Enabled: true}
	if err := setHandoffMetadata(&state, namekV1ProviderID, expiry, blob); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(state); err != nil {
		t.Fatal(err)
	}

	result, err := o.Prepare(context.Background(), PrepareTriggerTaskWarning, 10*time.Minute)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if result.Disposition != PrepareDispositionReused || !result.ExpiresAt.Equal(expiry) {
		t.Fatalf("Prepare = %+v; want reused through %s", result, expiry)
	}
	if nc.depositCount != 0 || mgr.wrapCallCount != 0 {
		t.Fatalf("minimum valid lease was replaced: deposit/wrap = %d/%d", nc.depositCount, mgr.wrapCallCount)
	}
}

func TestPrepareKeepsNonReusableHandoffWhenReplacementProviderUnavailable(t *testing.T) {
	o, mgr, nc, _ := newOrchestrator(t)
	oldBlob := []byte("legacy-raw-provider-unavailable")
	if err := WriteBlob(oldBlob); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(State{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	o.deps.IsIdentityReady = func() bool { return false }

	result, err := o.Prepare(context.Background(), PrepareTriggerTaskWarning, 10*time.Minute)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if result.Disposition != PrepareDispositionUnavailable {
		t.Fatalf("Prepare disposition = %q; want unavailable", result.Disposition)
	}
	if nc.depositCount != 0 || mgr.wrapCallCount != 0 {
		t.Fatalf("unavailable replacement performed deposit/wrap = %d/%d", nc.depositCount, mgr.wrapCallCount)
	}
	gotBlob, err := ReadBlob()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBlob, oldBlob) {
		t.Fatalf("unavailable replacement changed existing raw handoff: %q", gotBlob)
	}
}

func TestManualUnlockFirstRetainsHandoffUntilJoinedChainSucceeds(t *testing.T) {
	o, mgr, nc, _ := newOrchestrator(t)
	blob := []byte("manual-first-joinable-blob")
	if err := WriteBlob(blob); err != nil {
		t.Fatal(err)
	}
	state := State{Enabled: true}
	if err := setHandoffMetadata(&state, namekV1ProviderID, o.deps.Now().Add(5*time.Minute), blob); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(state); err != nil {
		t.Fatal(err)
	}
	factor := bytes.Repeat([]byte{0x41}, fSize)
	nc.pickupResp = &namekclient.PickupUnlockEscrowResponse{Secret: base64.RawURLEncoding.EncodeToString(factor)}
	mgr.unwrapErr = crypt.ErrAutoUnlockAlreadyUnlocked
	chainErr := errors.New("joined chain not ready")
	chainCalled := false

	got := o.RunPickup(context.Background(), func(context.Context) error {
		chainCalled = true
		return chainErr
	})
	if got != PickupOutcomeFailed {
		t.Fatalf("outcome = %v; want Failed", got)
	}
	if !chainCalled {
		t.Fatalf("manual-first path did not join complete unlock chain")
	}
	if !BlobExists() {
		t.Fatalf("joined-chain failure consumed raw blob")
	}
	gotState, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotState.Handoff) == 0 {
		t.Fatalf("joined-chain failure consumed metadata")
	}
	if nc.revokeCount != 0 {
		t.Fatalf("manual-first failure called legacy revoke %d times", nc.revokeCount)
	}
}

func TestOperationGateHonorsBlockedCallerContext(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	mgr := &fakeManager{sdekLoaded: true, sdek: []byte("test-sdek")}
	provider := &observingProvider{
		expiry:  time.Now().Add(10 * time.Minute),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	o, err := New(Deps{
		Manager:                 mgr,
		RecoveryProvider:        func() RecoveryFactorProvider { return provider },
		RecoveryProviderID:      "blocking-provider",
		GetDeviceID:             func() string { return "gate-device" },
		IsRecoveryProviderReady: func() bool { return true },
		IsIdentityReady:         func() bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := o.Prepare(context.Background(), PrepareTriggerTaskWarning, 10*time.Minute)
		firstDone <- err
	}()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("first Prepare did not reach provider")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := o.Prepare(ctx, PrepareTriggerTaskCritical, 10*time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked Prepare err = %v; want deadline exceeded", err)
	}
	if result.Disposition != PrepareDispositionUnavailable {
		t.Fatalf("blocked disposition = %q; want unavailable", result.Disposition)
	}
	if provider.depositCount != 1 {
		t.Fatalf("blocked caller reached provider; deposits=%d", provider.depositCount)
	}

	close(provider.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
}

func TestOperationGateRejectsAlreadyCanceledCallerBeforeWork(t *testing.T) {
	o, mgr, nc, _ := newOrchestrator(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := o.Prepare(ctx, PrepareTriggerTaskCritical, 10*time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prepare err = %v; want context canceled", err)
	}
	if result.Disposition != PrepareDispositionUnavailable {
		t.Fatalf("disposition = %q; want unavailable", result.Disposition)
	}
	if mgr.wrapCallCount != 0 || nc.depositCount != 0 {
		t.Fatalf("canceled caller performed work: wrap/deposit=%d/%d", mgr.wrapCallCount, nc.depositCount)
	}
}

type opaqueRecordingManager struct {
	loaded bool
	blob   []byte
	sdek   []byte
	factor []byte
	aad    []byte
}

func (m *opaqueRecordingManager) SDEKLoaded() bool { return m.loaded }

func (m *opaqueRecordingManager) WrapSDEKForEscrow(factor, aad []byte) ([]byte, error) {
	m.factor = append([]byte(nil), factor...)
	m.aad = append([]byte(nil), aad...)
	return append([]byte(nil), m.blob...), nil
}

func (m *opaqueRecordingManager) UnwrapSDEKWithEscrow([]byte, []byte, []byte) error { return nil }

type observingProvider struct {
	mu           sync.Mutex
	expiry       time.Time
	factor       []byte
	depositCount int
	pickupCount  int
	onDeposit    func()
	started      chan struct{}
	release      chan struct{}
}

func (p *observingProvider) Deposit(ctx context.Context, factor []byte, _ time.Duration) (time.Time, error) {
	p.mu.Lock()
	p.depositCount++
	p.factor = append([]byte(nil), factor...)
	count := p.depositCount
	p.mu.Unlock()
	if p.onDeposit != nil {
		p.onDeposit()
	}
	if count == 1 && p.started != nil {
		close(p.started)
		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		case <-p.release:
		}
	}
	return p.expiry, nil
}

func (p *observingProvider) Pickup(context.Context) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pickupCount++
	return append([]byte(nil), p.factor...), nil
}

func mustMetadataJSON(t *testing.T, meta handoffMetadata) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// forceNextStateSaveFailure preserves the currently durable state under a
// temporary name and replaces its path with a directory. Atomic SaveState then
// fails at rename; restore puts the pre-failure state back exactly as a process
// crash would have left it.
func forceNextStateSaveFailure() (func() error, error) {
	backup := statePath() + ".before-forced-save-failure"
	if err := os.Rename(statePath(), backup); err != nil {
		return nil, err
	}
	if err := os.Mkdir(statePath(), 0o700); err != nil {
		_ = os.Rename(backup, statePath())
		return nil, err
	}
	return func() error {
		if err := os.RemoveAll(statePath()); err != nil {
			return err
		}
		return os.Rename(backup, statePath())
	}, nil
}
