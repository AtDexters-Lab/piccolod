package app

import (
	"context"
	"errors"
	"testing"
)

func TestInstalledConfigApplyTerminalProgressOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "repair"} {
		t.Run(outcome, func(t *testing.T) {
			mgr, state, raw, _, _ := installedConfigTestApp(t)
			mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": raw}})
			read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
			if err != nil {
				t.Fatal(err)
			}
			dryRun, err := mgr.DryRunInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
				LedgerRevision: read.LedgerRevision, SourceHash: read.SourceHash, InputSchemaHash: read.InputSchemaHash,
			})
			if err != nil {
				t.Fatal(err)
			}
			req := InstalledConfigUpdateRequest{
				DryRunToken: dryRun.DryRunToken, CandidateDigest: dryRun.CandidateDigest,
				LedgerRevision: dryRun.LedgerRevision, SourceHash: dryRun.SourceHash,
				InputSchemaHash: dryRun.InputSchemaHash, BaseManifestHash: dryRun.BaseManifestHash,
				RuntimeFingerprint: dryRun.RuntimeFingerprint, TransitionPlanHash: dryRun.TransitionPlanHash,
			}
			if outcome == "failure" {
				req.CandidateDigest = "stale-candidate"
			}
			if outcome == "repair" {
				state.storeManifestUpdateTransactionHook = func(_ string, txn *ManifestUpdateTransaction) error {
					if txn.Phase == "publishing_access" {
						return errors.New("publication unavailable")
					}
					return nil
				}
			}
			reporter := &recordingArtifactProgressReporter{}
			mgr.SetProgressReporter(reporter)
			result, err := mgr.ApplyInstalledConfigUpdate(WithTaskID(context.Background(), "config-result"), "piclu", req)
			terminal, ok := reporter.Last("config-result")
			if !ok || !terminal.IsComplete {
				t.Fatalf("missing terminal event: %+v", terminal)
			}
			if outcome == "failure" {
				if err == nil || terminal.Error == "" || terminal.Metadata["access_repair_pending"] == true {
					t.Fatalf("failure was not preserved: err=%v event=%+v", err, terminal)
				}
				return
			}
			if err != nil || terminal.Error != "" {
				t.Fatalf("unexpected failure: err=%v event=%+v", err, terminal)
			}
			if outcome == "repair" {
				if !result.AccessRepairPending || terminal.Metadata["access_repair_pending"] != true || terminal.Metadata["access_repair_message"] != result.AccessRepairMessage {
					t.Fatalf("terminal progress lost repair outcome: result=%+v event=%+v", result, terminal)
				}
			} else if result.AccessRepairPending || terminal.Metadata["access_repair_pending"] == true {
				t.Fatalf("success gained repair warning: result=%+v event=%+v", result, terminal)
			}
		})
	}
}
