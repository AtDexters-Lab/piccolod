package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func isolateSlicePolicy(t *testing.T) {
	t.Helper()
	oldRoot, oldExecutor := dropinRootForTest, defaultExecutor
	dropinRootForTest = t.TempDir()
	t.Cleanup(func() {
		dropinRootForTest, defaultExecutor = oldRoot, oldExecutor
	})
}

func testAdaptivePolicy() (SliceResourcePolicy, SliceResourcePolicy) {
	baseline := SliceResourcePolicy{UID: 467, MemoryHighBytes: 80000000, MemoryMaxBytes: 128000000, CPUWeight: 100}
	adjusted := baseline
	adjusted.BaselineMemoryHighBytes = baseline.MemoryHighBytes
	adjusted.MemoryHighBytes = 100000000
	return baseline, adjusted
}

func TestSliceResourcePolicy_AdaptiveRestoreAfterRestart(t *testing.T) {
	isolateSlicePolicy(t)
	baseline, adjusted := testAdaptivePolicy()
	if _, err := adjusted.writeFile(); err != nil {
		t.Fatal(err)
	}
	// A fresh baseline has no in-process history; the owned drop-in is sufficient.
	restored, err := baseline.RestoreAdaptiveMemoryHigh()
	if err != nil || restored != adjusted {
		t.Fatalf("restored = %+v, err = %v; want %+v", restored, err, adjusted)
	}
	if got := adjusted.Render(); !strings.Contains(got, adaptiveMemoryBaselinePrefix+"80000000\n") {
		t.Fatalf("saved adaptive policy lacks baseline identity: %q", got)
	}
	adjusted.MemoryHighBytes = baseline.MemoryHighBytes
	if strings.Contains(adjusted.Render(), adaptiveMemoryBaselinePrefix) {
		t.Fatal("unadjusted high must not claim adaptive state")
	}
}

func TestSliceResourcePolicy_AdaptiveRestoreInvalidatesPolicyChanges(t *testing.T) {
	for _, field := range []string{"baseline", "max", "cpu"} {
		t.Run(field, func(t *testing.T) {
			isolateSlicePolicy(t)
			baseline, adjusted := testAdaptivePolicy()
			if _, err := adjusted.writeFile(); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "baseline":
				baseline.MemoryHighBytes++
			case "max":
				baseline.MemoryMaxBytes++
			case "cpu":
				baseline.CPUWeight++
			}
			got, err := baseline.RestoreAdaptiveMemoryHigh()
			if err != nil || got != baseline {
				t.Fatalf("changed %s restored stale policy: %+v, %v", field, got, err)
			}
		})
	}
}

func TestSliceResourcePolicy_AdaptiveRestoreRejectsUnownedContent(t *testing.T) {
	baseline, adjusted := testAdaptivePolicy()
	unmarked := adjusted
	unmarked.BaselineMemoryHighBytes = 0
	cases := map[string]string{
		"unmarked manual high": unmarked.Render(),
		"extra directive":      adjusted.Render() + "MemoryHigh=127000000\n",
		"extra comment":        adjusted.Render() + "# manually edited\n",
		"duplicate marker":     adjusted.Render() + adaptiveMemoryBaselinePrefix + "80000000\n",
		"malformed marker":     strings.ReplaceAll(adjusted.Render(), "baseline=80000000", "baseline=garbage"),
		"over maximum":         strings.ReplaceAll(adjusted.Render(), "MemoryHigh=100000000", "MemoryHigh=129000000"),
		"below baseline":       strings.ReplaceAll(adjusted.Render(), "MemoryHigh=100000000", "MemoryHigh=70000000"),
		"truncated":            strings.TrimSuffix(adjusted.Render(), "\n"),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			isolateSlicePolicy(t)
			if err := os.MkdirAll(baseline.DropinDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(baseline.DropinFile(), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := baseline.RestoreAdaptiveMemoryHigh()
			if err != nil || got != baseline {
				t.Fatalf("adopted invalid content: %+v, %v", got, err)
			}
		})
	}
}

func TestSliceResourcePolicy_AdaptiveRestoreMissingAndReadError(t *testing.T) {
	isolateSlicePolicy(t)
	baseline, _ := testAdaptivePolicy()
	got, err := baseline.RestoreAdaptiveMemoryHigh()
	if err != nil || got != baseline {
		t.Fatalf("missing file: %+v, %v", got, err)
	}
	if err := os.MkdirAll(baseline.DropinFile(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := baseline.RestoreAdaptiveMemoryHigh(); err == nil {
		t.Fatal("non-missing I/O error must fail closed")
	}
}

func TestSliceResourcePolicy_AdaptiveValidation(t *testing.T) {
	_, adjusted := testAdaptivePolicy()
	for _, value := range []int64{-1, adjusted.MemoryHighBytes + 1} {
		adjusted.BaselineMemoryHighBytes = value
		if err := adjusted.Validate(); err == nil {
			t.Fatalf("invalid baseline %d accepted", value)
		}
	}
	if err := (SliceResourcePolicy{UID: 467, BaselineMemoryHighBytes: 1}).Validate(); err == nil {
		t.Fatal("baseline without memory limits accepted")
	}
	if err := (SliceResourcePolicy{UID: 467, MemoryHighBytes: -1, MemoryMaxBytes: -1}).Validate(); err == nil {
		t.Fatal("negative limits accepted")
	}
}

func TestSliceResourcePolicy_AdaptiveApplyPersistsBeforeLiveAndRetries(t *testing.T) {
	isolateSlicePolicy(t)
	_, adjusted := testAdaptivePolicy()
	setCommand := "systemctl set-property user-467.slice MemoryHigh=100000000 MemoryMax=128000000 CPUWeight=100"
	liveFailure := errors.New("dbus temporarily unavailable")
	executor := &mockExecutor{results: map[string]mockResult{setCommand: {err: liveFailure}}}
	executor.onRun = func(name string, args ...string) {
		content, err := os.ReadFile(adjusted.DropinFile())
		if err != nil || string(content) != adjusted.Render() {
			t.Errorf("live command preceded durable policy: %q, %v", content, err)
		}
	}
	defaultExecutor = executor
	if err := adjusted.ApplyContext(context.Background()); !errors.Is(err, liveFailure) {
		t.Fatalf("live failure = %v", err)
	}
	delete(executor.results, setCommand)
	if err := adjusted.Apply(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	want := [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "set-property", "user-467.slice", "MemoryHigh=100000000", "MemoryMax=128000000", "CPUWeight=100"},
	}
	if !reflect.DeepEqual(executor.calls, append(want, want...)) {
		t.Fatalf("live apply must repeat despite identical file and remain persistent: %v", executor.calls)
	}
}

type policyDeadlineExecutor struct {
	t *testing.T
}

func (e policyDeadlineExecutor) Run(string, ...string) ([]byte, error) {
	e.t.Fatal("policy used unbounded Run")
	return nil, nil
}

func (e policyDeadlineExecutor) RunContext(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > slicePolicyApplyTimeout {
		e.t.Fatal("policy command lacks bounded context")
	}
	return nil, nil
}

func TestSliceResourcePolicy_ApplyContextIsBoundedAndCancellable(t *testing.T) {
	isolateSlicePolicy(t)
	baseline, _ := testAdaptivePolicy()
	defaultExecutor = policyDeadlineExecutor{t: t}
	if err := baseline.Apply(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := baseline.ApplyContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled apply = %v", err)
	}
	executor := &mockExecutor{blockUntilContext: map[string]bool{"systemctl daemon-reload": true}}
	defaultExecutor = executor
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := baseline.ApplyContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung command deadline = %v", err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("live properties ran after command timed out: %v", executor.calls)
	}
}

func TestSliceResourcePolicy_AdaptiveRemovalAndEmptyReset(t *testing.T) {
	isolateSlicePolicy(t)
	baseline, adjusted := testAdaptivePolicy()
	if _, err := adjusted.writeFile(); err != nil {
		t.Fatal(err)
	}
	if err := adjusted.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(adjusted.DropinFile()); !os.IsNotExist(err) {
		t.Fatalf("removed adaptive file still exists: %v", err)
	}
	if _, err := adjusted.writeFile(); err != nil {
		t.Fatal(err)
	}
	executor := &mockExecutor{}
	defaultExecutor = executor
	if err := (SliceResourcePolicy{UID: baseline.UID}).Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Clean(adjusted.DropinFile())); !os.IsNotExist(err) {
		t.Fatalf("empty policy retained saved adaptive state: %v", err)
	}
	want := [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "set-property", "user-467.slice", "MemoryHigh=infinity", "MemoryMax=infinity", "CPUWeight=100"}}
	if !reflect.DeepEqual(executor.calls, want) {
		t.Fatalf("empty policy reset calls = %v", executor.calls)
	}
}
