package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/events"
	"piccolod/internal/persistence"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type recordingArtifactProgressReporter struct {
	mu     sync.Mutex
	events []events.TaskProgressEvent
}

func (r *recordingArtifactProgressReporter) Report(event events.TaskProgressEvent) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *recordingArtifactProgressReporter) Last(taskID string) (events.TaskProgressEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := len(r.events) - 1; index >= 0; index-- {
		if r.events[index].TaskID == taskID {
			return r.events[index], true
		}
	}
	return events.TaskProgressEvent{}, false
}

func (r *recordingArtifactProgressReporter) snapshot() []events.TaskProgressEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.TaskProgressEvent(nil), r.events...)
}

func TestArtifactProgressInheritsLifecycleAndKeepsTaskFresh(t *testing.T) {
	reporter := &recordingArtifactProgressReporter{}
	manager := &AppManager{}
	manager.SetProgressReporter(reporter)
	ctx := WithTaskID(context.Background(), "start-provider")
	manager.emitProgress(
		ctx,
		taskTypeStartApp,
		"provider",
		taskPhaseStarting,
		40,
		"Starting app",
		false,
		nil,
	)

	finish := manager.beginArtifactProgress(
		ctx,
		"provider",
		"model",
		"huggingface",
		1,
		2,
		10*time.Millisecond,
	)
	time.Sleep(25 * time.Millisecond)
	finish(nil)

	got := reporter.snapshot()
	if len(got) < 4 {
		t.Fatalf("progress events = %d, want seed, start, heartbeat, and ready", len(got))
	}
	var artifactEvents []events.TaskProgressEvent
	for _, event := range got {
		if event.Phase == taskPhaseMaterializingArtifact {
			artifactEvents = append(artifactEvents, event)
		}
	}
	if len(artifactEvents) < 3 {
		t.Fatalf("artifact progress events = %d, want start, heartbeat, and ready", len(artifactEvents))
	}
	for _, event := range artifactEvents {
		if event.TaskType != taskTypeStartApp {
			t.Fatalf("artifact task type = %q, want inherited %q", event.TaskType, taskTypeStartApp)
		}
		if event.Progress < 40 {
			t.Fatalf("artifact progress regressed to %d", event.Progress)
		}
		if event.Metadata["artifact"] != "model" ||
			event.Metadata["artifact_index"] != 1 ||
			event.Metadata["artifact_total"] != 2 ||
			event.Metadata["source_type"] != "huggingface" {
			t.Fatalf("artifact metadata = %#v", event.Metadata)
		}
	}
	if stage := artifactEvents[len(artifactEvents)-1].Metadata["stage"]; stage != "ready" {
		t.Fatalf("final artifact stage = %v, want ready", stage)
	}

	countAfterFinish := len(got)
	time.Sleep(25 * time.Millisecond)
	if count := len(reporter.snapshot()); count != countAfterFinish {
		t.Fatalf("heartbeat continued after completion: before=%d after=%d", countAfterFinish, count)
	}
}

func TestArtifactOperationTimeoutCoversProjectionAndFinalization(t *testing.T) {
	if ArtifactOperationTimeout <= huggingFaceMaxAttempt {
		t.Fatalf(
			"artifact operation timeout %s must exceed projection timeout %s",
			ArtifactOperationTimeout,
			huggingFaceMaxAttempt,
		)
	}
}

func TestResolveAndDownloadHuggingFaceDirectoryProjection(t *testing.T) {
	t.Parallel()

	const commit = "0123456789abcdef0123456789abcdef01234567"
	files := map[string]string{
		"models/config.json":   `{"ok":true}`,
		"models/weights.bin":   "weights",
		"unselected/readme.md": "ignore",
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/acme/model/revision/main"):
			return testHTTPResponse(
				http.StatusOK,
				fmt.Sprintf(`{"sha":%q,"siblings":[{"rfilename":"models/config.json","size":11},{"rfilename":"models/weights.bin","size":7},{"rfilename":"unselected/readme.md","size":6}]}`, commit),
			), nil
		case strings.Contains(r.URL.Path, "/resolve/"+commit+"/"):
			name := strings.SplitN(r.URL.Path, "/resolve/"+commit+"/", 2)[1]
			value, ok := files[name]
			if !ok {
				return testHTTPResponse(http.StatusNotFound, ""), nil
			}
			return testHTTPResponse(http.StatusOK, value), nil
		default:
			return testHTTPResponse(http.StatusNotFound, ""), nil
		}
	})}

	source := api.ArtifactSource{
		Type:       "huggingface",
		Repository: "acme/model",
		Revision:   "main",
		Path:       "models",
	}
	resolved, err := resolveHuggingFaceSource(context.Background(), client, "https://huggingface.test", source)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Commit != commit || resolved.SelectedFile || len(resolved.Files) != 2 {
		t.Fatalf("unexpected resolution: %+v", resolved)
	}

	target := t.TempDir()
	if err := downloadHuggingFaceProjection(context.Background(), client, "https://huggingface.test", source, resolved, target); err != nil {
		t.Fatalf("download: %v", err)
	}
	for relative, want := range map[string]string{
		"config.json": "{" + `"ok":true}`,
		"weights.bin": "weights",
	} {
		got, err := os.ReadFile(filepath.Join(target, relative))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", relative, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "unselected")); !os.IsNotExist(err) {
		t.Fatalf("unselected subtree was materialized")
	}
}

func TestDownloadHuggingFaceSelectedFileVerifiesDigest(t *testing.T) {
	t.Parallel()

	const commit = "0123456789abcdef0123456789abcdef01234567"
	payload := []byte("model")
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/acme/model/revision/release"):
			return testHTTPResponse(
				http.StatusOK,
				fmt.Sprintf(`{"sha":%q,"siblings":[{"rfilename":"nested/model.gguf","size":5}]}`, commit),
			), nil
		case strings.Contains(r.URL.Path, "/resolve/"+commit+"/nested/model.gguf"):
			return testHTTPResponse(http.StatusOK, string(payload)), nil
		default:
			return testHTTPResponse(http.StatusNotFound, ""), nil
		}
	})}

	source := api.ArtifactSource{
		Type:       "huggingface",
		Repository: "acme/model",
		Revision:   "release",
		Path:       "nested/model.gguf",
		Digest:     digest,
	}
	resolved, err := resolveHuggingFaceSource(context.Background(), client, "https://huggingface.test", source)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	target := t.TempDir()
	if err := downloadHuggingFaceProjection(context.Background(), client, "https://huggingface.test", source, resolved, target); err != nil {
		t.Fatalf("download: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "model.gguf")); err != nil {
		t.Fatalf("selected file basename missing: %v", err)
	}

	source.Digest = "sha256:" + strings.Repeat("0", 64)
	second := t.TempDir()
	if err := downloadHuggingFaceProjection(context.Background(), client, "https://huggingface.test", source, resolved, second); err == nil {
		t.Fatalf("expected digest mismatch")
	}
}

func TestResolveHuggingFaceSourceRequiresFileSizes(t *testing.T) {
	t.Parallel()

	const commit = "0123456789abcdef0123456789abcdef01234567"
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return testHTTPResponse(
			http.StatusOK,
			fmt.Sprintf(`{"sha":%q,"siblings":[{"rfilename":"model.gguf"}]}`, commit),
		), nil
	})}
	_, err := resolveHuggingFaceSource(context.Background(), client, "https://huggingface.test", api.ArtifactSource{
		Type:       "huggingface",
		Repository: "acme/model",
		Revision:   "main",
		Path:       "model.gguf",
	})
	if err == nil || !strings.Contains(err.Error(), "no valid size") {
		t.Fatalf("missing file size error = %v", err)
	}
}

func TestDownloadHuggingFaceFileEnforcesResolvedSize(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name         string
		body         string
		expectedSize int64
	}{
		{name: "overrun", body: "12345", expectedSize: 4},
		{name: "underrun", body: "123", expectedSize: 4},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return testHTTPResponse(http.StatusOK, test.body), nil
			})}
			target := filepath.Join(t.TempDir(), "model.gguf")
			err := downloadHuggingFaceFile(
				context.Background(),
				client,
				"https://huggingface.test",
				"acme/model",
				strings.Repeat("a", 40),
				"model.gguf",
				target,
				test.expectedSize,
				"",
			)
			if err == nil {
				t.Fatal("size mismatch was accepted")
			}
			if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
				t.Fatalf("partial target was retained: %v", statErr)
			}
		})
	}
}

func TestDownloadHuggingFaceFileStopsStalledTransfer(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	defer writer.Close()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       reader,
		}, nil
	})}
	target := filepath.Join(t.TempDir(), "model.gguf")
	err := downloadHuggingFaceFileWithStallTimeout(
		context.Background(),
		client,
		"https://huggingface.test",
		"acme/model",
		strings.Repeat("a", 40),
		"model.gguf",
		target,
		1,
		"",
		50*time.Millisecond,
		time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("stalled download error = %v", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("stalled target was retained: %v", statErr)
	}
}

func TestDownloadHuggingFaceFileStopsTrickleTransferAtAttemptDeadline(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	defer writer.Close()
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := writer.Write([]byte{'x'}); err != nil {
				return
			}
		}
	}()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       reader,
		}, nil
	})}
	target := filepath.Join(t.TempDir(), "model.gguf")
	err := downloadHuggingFaceFileWithStallTimeout(
		context.Background(),
		client,
		"https://huggingface.test",
		"acme/model",
		strings.Repeat("a", 40),
		"model.gguf",
		target,
		1<<20,
		"",
		50*time.Millisecond,
		75*time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), "attempt deadline") {
		t.Fatalf("trickle download error = %v", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("trickle target was retained: %v", statErr)
	}
}

func TestHuggingFaceDownloadAttemptTimeoutIsFiniteAndSizeAware(t *testing.T) {
	t.Parallel()

	if got := huggingFaceDownloadAttemptTimeout(0); got != huggingFaceAttemptGrace {
		t.Fatalf("zero-size timeout = %s, want %s", got, huggingFaceAttemptGrace)
	}
	small := huggingFaceDownloadAttemptTimeout(1 << 20)
	large := huggingFaceDownloadAttemptTimeout(1 << 30)
	if small <= huggingFaceAttemptGrace || large <= small {
		t.Fatalf("timeouts are not size-aware: small=%s large=%s", small, large)
	}
	if got := huggingFaceDownloadAttemptTimeout(int64(^uint64(0) >> 1)); got != huggingFaceMaxAttempt {
		t.Fatalf("maximum-size timeout = %s, want cap %s", got, huggingFaceMaxAttempt)
	}
}

func TestSafeRepositoryPath(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"../escape", "/absolute", "a//b", `a\b`, "a/../b", "."} {
		if safeRepositoryPath(value) {
			t.Fatalf("unsafe path accepted: %q", value)
		}
	}
	if !safeRepositoryPath("nested/model.gguf") {
		t.Fatalf("safe path rejected")
	}
}

func TestHuggingFacePinnedProjectionCannotReuseUnpinnedIdentity(t *testing.T) {
	t.Parallel()
	unpinned := huggingFaceGoldenProjection("model.gguf", "")
	pinned := huggingFaceGoldenProjection("model.gguf", "sha256:"+strings.Repeat("a", 64))
	if pinned == unpinned || !strings.Contains(pinned, "@sha256:") {
		t.Fatalf("pinned projection %q did not retain verification input", pinned)
	}
}

func TestArtifactReferenceIDIsBoundedAndTupleStable(t *testing.T) {
	instanceID := "provider12345678"
	artifactName := "a" + strings.Repeat("b", 300)
	goldenID := "golden-" + strings.Repeat("c", 64)

	got := artifactReferenceID(instanceID, artifactName, goldenID)
	prefix := instanceID + "--artifact--"
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("reference ID %q does not retain instance prefix %q", got, prefix)
	}
	digest := strings.TrimPrefix(got, prefix)
	if len(got) > 180 || len(digest) != 64 {
		t.Fatalf("reference ID length=%d digest length=%d", len(got), len(digest))
	}
	if _, err := hex.DecodeString(digest); err != nil {
		t.Fatalf("reference digest is not lowercase hexadecimal: %v", err)
	}
	if repeat := artifactReferenceID(instanceID, artifactName, goldenID); repeat != got {
		t.Fatalf("same tuple produced %q, want stable %q", repeat, got)
	}

	seen := map[string]struct{}{got: {}}
	for _, changed := range []string{
		artifactReferenceID("other", artifactName, goldenID),
		artifactReferenceID(instanceID, artifactName+"x", goldenID),
		artifactReferenceID(instanceID, artifactName, goldenID+"x"),
	} {
		if _, duplicate := seen[changed]; duplicate {
			t.Fatalf("different tuple reused reference ID %q", changed)
		}
		seen[changed] = struct{}{}
	}
}

func TestRetainedArtifactReferenceIDsIncludesPendingTransactionOwners(t *testing.T) {
	state := newCapabilityTestState(t)
	state.cache["provider"] = &AppInstance{
		InstanceID:         "provider",
		ArtifactReferences: map[string]string{"installed": "ref-installed"},
	}
	if err := state.StoreManifestUpdateTransaction("provider", &ManifestUpdateTransaction{
		Phase:                 "runtime_touched",
		PreviousArtifactRefs:  map[string]string{"model": "ref-previous"},
		CandidateArtifactRefs: map[string]string{"model": "ref-candidate"},
	}); err != nil {
		t.Fatalf("StoreManifestUpdateTransaction: %v", err)
	}
	record := transitionTestRecord("provider", TransitionPhaseCandidateTouched)
	record.Resources.PreviousArtifactRefs = map[string]string{"tokenizer": "ref-transition-previous"}
	record.Resources.CandidateArtifactRefs = map[string]string{"tokenizer": "ref-transition-candidate"}
	if err := state.StoreTransitionRecord("provider", record); err != nil {
		t.Fatalf("StoreTransitionRecord: %v", err)
	}

	retained, err := retainedArtifactReferenceIDs(state)
	if err != nil {
		t.Fatalf("retainedArtifactReferenceIDs: %v", err)
	}
	for _, referenceID := range []string{
		"ref-installed",
		"ref-previous",
		"ref-candidate",
		"ref-transition-previous",
		"ref-transition-candidate",
	} {
		if _, ok := retained[referenceID]; !ok {
			t.Fatalf("pending owner reference %q was not retained: %v", referenceID, retained)
		}
	}
	if _, ok := retained["ref-orphan"]; ok {
		t.Fatal("unowned reference was retained")
	}
}

func TestRetainedArtifactReferenceIDsFailsClosedOnUnreadableOwner(t *testing.T) {
	state := newCapabilityTestState(t)
	appDir := filepath.Join(state.appsDir, "provider")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir app owner: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(appDir, manifestUpdateTxnFilename),
		[]byte("{not-json"),
		0o600,
	); err != nil {
		t.Fatalf("write corrupt owner: %v", err)
	}
	if _, err := retainedArtifactReferenceIDs(state); err == nil {
		t.Fatal("unreadable durable artifact owner did not fail closed")
	}
}

func TestRetainedArtifactReferenceIDsFailsClosedOnUnreadableInstalledApp(t *testing.T) {
	state := newCapabilityTestState(t)
	app := &AppInstance{
		InstanceID:         "provider",
		Enabled:            true,
		Definition:         capabilityProviderDefinition("/v1"),
		ArtifactReferences: map[string]string{"model": "ref-installed"},
	}
	if err := state.StoreApp(app); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}
	state.cacheMu.Lock()
	delete(state.cache, app.InstanceID)
	state.cacheMu.Unlock()
	if err := os.WriteFile(
		filepath.Join(state.appsDir, app.InstanceID, "metadata.json"),
		[]byte("{not-json"),
		0o600,
	); err != nil {
		t.Fatalf("corrupt metadata: %v", err)
	}
	if _, err := retainedArtifactReferenceIDs(state); err == nil {
		t.Fatal("unreadable installed artifact owner did not fail closed")
	}
}

func TestExactSourceForRecordedArtifactDoesNotFollowMutableSource(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("a", 64)
	oci, err := exactSourceForRecordedArtifact(api.ArtifactSource{
		Type:      "oci",
		Reference: "registry.example/model:latest",
	}, persistence.GoldenContentIdentity{
		SourceKind:       persistence.GoldenSourceOCI,
		ResolvedIdentity: digest,
		Projection:       persistence.GoldenProjectionOCIArtifact,
	})
	if err != nil {
		t.Fatalf("exact OCI source: %v", err)
	}
	if oci.Reference != "registry.example/model:latest@"+digest || oci.Digest != digest {
		t.Fatalf("exact OCI source = %+v", oci)
	}

	commit := strings.Repeat("b", 40)
	huggingFace, err := exactSourceForRecordedArtifact(api.ArtifactSource{
		Type:       "huggingface",
		Repository: "acme/model",
		Revision:   "main",
		Path:       "model.gguf",
	}, persistence.GoldenContentIdentity{
		SourceKind:       persistence.GoldenSourceHuggingFace,
		ResolvedIdentity: commit,
		Projection:       persistence.GoldenProjectionHuggingFace + ":model.gguf",
	})
	if err != nil {
		t.Fatalf("exact Hugging Face source: %v", err)
	}
	if huggingFace.Revision != commit {
		t.Fatalf("exact Hugging Face revision = %q, want %q", huggingFace.Revision, commit)
	}
}
