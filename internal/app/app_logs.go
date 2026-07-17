package app

import (
	"bufio"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"piccolod/internal/persistence"
	"piccolod/internal/state/paths"
)

// App persistent logs live on the singleton app-logs volume,
// one subtree per app instance: {app-logs mount}/{instanceID}/{svcName}.log.
// The volume is mounted for the whole unlocked session (disjoint from any
// container lifecycle); this file owns the per-app subtree lifecycle and the
// historic read/merge path. Per-service write redirection (LogPath) is wired in
// multi_container.go (buildServiceContainerSpec / appLogPathForService).

const (
	// appLogsTempDirName holds in-flight download snapshots. It
	// lives at the mount root so it shares the volume's bounded budget; the
	// startup reaper empties it (covers SIGKILL leaks).
	appLogsTempDirName = ".download-tmp"

	// appLogMaxLineBytes caps a reassembled logical line so an adversarial
	// multi-MB single line cannot defeat the merge's bounded memory.
	appLogMaxLineBytes = 64 << 10

	// appLogMaxServiceFiles bounds how many per-service log files a single
	// download will open/merge. Normal apps have a handful of services; this
	// caps fd/temp-space use if a compromised per-app user floods its subtree
	// with *.log files.
	appLogMaxServiceFiles = 64
)

// ErrAppLogsUnavailable signals the app-logs volume is not currently mounted
// (crypto locked / not yet attached). Distinct from "no logs in the window" so
// the handler can return a retryable status rather than an empty success.
var ErrAppLogsUnavailable = errors.New("app logs volume unavailable")

// removeAppLogSubtree deletes an app's persistent-log subtree. Best-effort: the
// startup reaper is the backstop, so a failure here is logged, not fatal.
// Invoked via defer from both teardown paths (uninstall + failed-install) so it
// runs regardless of a DestroyVolume early-return.
func (m *AppManager) removeAppLogSubtree(instanceID string) {
	dir := appLogsInstanceDir(instanceID)
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("WARN: remove app-logs subtree %s: %v", dir, err)
	}
}

// ReapOrphanAppLogs removes per-app log subtrees whose instanceID is no longer a
// known app (uninstall/failed-install crashes that skipped removeAppLogSubtree),
// and empties the download temp dir. Runs at unlock, before the app-manager
// reload that can create new subtrees.
//
// FAIL-CLOSED: the known-app set comes from ensureStateManager,
// which returns a disk-loaded cache or an error — never an empty-because-unloaded
// cache. If it errors (crypto locked / state unavailable) we ABORT without
// deleting anything; "can't enumerate" is never treated as "all orphaned."
func (m *AppManager) ReapOrphanAppLogs(ctx context.Context) error {
	mount := paths.MountDir(persistence.AppLogsVolumeID)
	mounted, err := persistence.IsMountPoint(mount)
	if err != nil {
		return fmt.Errorf("reap app-logs: mount probe: %w", err)
	}
	if !mounted {
		// Volume not attached — nothing of ours is present to reap. (Logging is
		// simply unavailable this session; not an orphan condition.)
		return nil
	}

	state, err := m.ensureStateManager()
	if err != nil {
		// Fail-closed: without a positively-loaded known set we must not delete.
		return fmt.Errorf("reap app-logs: known-app set unavailable, aborting: %w", err)
	}
	known := make(map[string]bool)
	for _, app := range state.ListApps() {
		known[app.InstanceID] = true
	}

	entries, err := os.ReadDir(mount)
	if err != nil {
		return fmt.Errorf("reap app-logs: read mount: %w", err)
	}
	// Defense: a validly-loaded but EMPTY known-app set
	// alongside populated subtrees is more likely state truncation/corruption
	// than genuine "zero apps" — refuse to mass-delete in that case.
	if len(known) == 0 {
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() && n != "lost+found" && !strings.HasPrefix(n, ".") {
				log.Printf("WARN: reap app-logs: zero known apps but subtree %q present; aborting to avoid mass-delete", n)
				return nil
			}
		}
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "lost+found":
			continue
		case name == appLogsTempDirName:
			// Sweep leftover download snapshots (safe at startup: no download in
			// flight). Remove the dir; it is recreated on demand.
			if err := os.RemoveAll(filepath.Join(mount, name)); err != nil {
				log.Printf("WARN: reap app-logs temp dir: %v", err)
			}
			continue
		case strings.HasPrefix(name, "."):
			// Leave other dotfiles/metadata alone.
			continue
		case e.IsDir() && !known[name]:
			sub := filepath.Join(mount, name)
			if err := os.RemoveAll(sub); err != nil {
				log.Printf("WARN: reap orphan app-logs subtree %s: %v", sub, err)
			} else {
				log.Printf("INFO: reaped orphan app-logs subtree %s", sub)
			}
		}
	}
	return nil
}

// AppLogsAvailable reports whether the app-logs volume is currently mounted, so
// the download handler can return a clean retryable error before committing an
// attachment response.
func AppLogsAvailable() bool {
	mounted, err := persistence.IsMountPoint(paths.MountDir(persistence.AppLogsVolumeID))
	return err == nil && mounted
}

// --- Historic read + merge ---

// parseK8sFileLogLine parses one physical k8s-file line:
//
//	<RFC3339Nano> <stream> <F|P> <message...>
//
// ok=false for a malformed line (dropped, not errored — handles torn trailing
// lines from a writer mid-append). partial is true for a "P" (incomplete) line
// that continues in the following entry.
func parseK8sFileLogLine(raw string) (ts time.Time, partial bool, msg string, ok bool) {
	// SplitN on space: [0]=ts [1]=stream [2]=tag [3]=message (may contain spaces).
	parts := strings.SplitN(raw, " ", 4)
	if len(parts) < 3 {
		return time.Time{}, false, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, false, "", false
	}
	tag := parts[2]
	if tag != "F" && tag != "P" {
		return time.Time{}, false, "", false
	}
	message := ""
	if len(parts) == 4 {
		message = parts[3]
	}
	return t, tag == "P", message, true
}

// logicalLineReader yields complete logical lines from one service's snapshot,
// reassembling P-continuation segments up to the terminating F and capping the
// reassembled length. It holds one peeked line (cur*) so the k-way merge can
// compare heads. Files are read in append (chronological) order.
type logicalLineReader struct {
	service string
	source  io.ReadSeeker
	br      *bufio.Reader
	closer  io.Closer

	curTS  time.Time
	curMsg string
	valid  bool // a logical line is currently buffered in cur*
	eof    bool
}

func newLogicalLineReader(service string, source io.ReadSeeker, closer io.Closer) *logicalLineReader {
	return &logicalLineReader{
		service: service,
		source:  source,
		br:      bufio.NewReader(source),
		closer:  closer,
	}
}

// reset rewinds a stable snapshot for another merge pass. Historic downloads
// use this to count matching lines before emitting the newest maxLines without
// buffering those lines in memory.
func (r *logicalLineReader) reset() error {
	if _, err := r.source.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r.br.Reset(r.source)
	r.curTS = time.Time{}
	r.curMsg = ""
	r.valid = false
	r.eof = false
	return nil
}

// advance buffers the next logical line into cur*. Sets valid=false at EOF.
func (r *logicalLineReader) advance() {
	if r.eof {
		r.valid = false
		return
	}
	var (
		acc       strings.Builder
		firstTS   time.Time
		haveFirst bool
		truncated bool
	)
	for {
		raw, err := r.br.ReadString('\n')
		line := strings.TrimRight(raw, "\n")
		if line != "" {
			if ts, partial, msg, ok := parseK8sFileLogLine(line); ok {
				if !haveFirst {
					firstTS = ts
					haveFirst = true
				}
				if !truncated {
					if acc.Len()+len(msg) > appLogMaxLineBytes {
						msg = msg[:max(0, appLogMaxLineBytes-acc.Len())]
						acc.WriteString(msg)
						acc.WriteString(" …[truncated]")
						truncated = true
					} else {
						acc.WriteString(msg)
					}
				}
				if !partial {
					// Terminating F — logical line complete.
					if err == nil {
						r.curTS, r.curMsg, r.valid = firstTS, acc.String(), true
						return
					}
					// F on the final line with EOF — still a complete line.
					r.eof = true
					r.curTS, r.curMsg, r.valid = firstTS, acc.String(), true
					return
				}
				// partial — keep accumulating
			}
			// malformed line: dropped, keep reading
		}
		if err != nil {
			// EOF (or read error). A pending accumulator here is a torn trailing
			// line (P with no terminating F) — dropped per Q2.
			r.eof = true
			r.valid = false
			return
		}
	}
}

// logicalLineHeap orders readers by their buffered head timestamp (min-heap).
// Best-effort across an intra-file backward clock step: readers
// are never early-terminated, so a backward step only locally misorders.
type logicalLineHeap []*logicalLineReader

func (h logicalLineHeap) Len() int           { return len(h) }
func (h logicalLineHeap) Less(i, j int) bool { return h[i].curTS.Before(h[j].curTS) }
func (h logicalLineHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *logicalLineHeap) Push(x any)        { *h = append(*h, x.(*logicalLineReader)) }
func (h *logicalLineHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// WriteHistoricLogsForApp streams an app's persistent logs in [since, until],
// combined and time-interleaved across all services, to w. Each line is tagged
// "TS [service] message". Output keeps the newest maxLines matching lines and
// emits an omission marker when older matches exceed the cap. Returns
// ErrAppLogsUnavailable when the app-logs volume is not mounted (retryable); an
// absent/empty subtree yields zero lines and a nil error.
func (m *AppManager) WriteHistoricLogsForApp(ctx context.Context, instanceID string, since, until time.Time, maxLines int, w io.Writer) error {
	mount := paths.MountDir(persistence.AppLogsVolumeID)
	mounted, err := persistence.IsMountPoint(mount)
	if err != nil {
		return fmt.Errorf("app-logs mount probe: %w", err)
	}
	if !mounted {
		return ErrAppLogsUnavailable
	}

	instanceDir := appLogsInstanceDir(instanceID)
	entries, err := os.ReadDir(instanceDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No subtree yet: either the app was installed before this feature
			// (persistent logs are forward-only — they begin at the app's next
			// recreate/update) or it has simply never logged. Emit a header line
			// so the UI can explain the activation path instead of showing a
			// bare "no logs."
			fmt.Fprintln(w, "# No persistent logs for this app yet — restart or update the app to start collecting them.")
			return nil
		}
		return fmt.Errorf("read app-logs subtree: %w", err)
	}

	// Snapshot each service file to a temp copy before reading, to avoid a
	// long-lived reader on a file conmon may be rotating. Cleaned up
	// regardless of how this returns (covers panic/cancel).
	tempDir := filepath.Join(mount, appLogsTempDirName, instanceID)
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return fmt.Errorf("create app-logs temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	var readers []*logicalLineReader
	defer func() {
		for _, r := range readers {
			if r.closer != nil {
				_ = r.closer.Close()
			}
		}
	}()

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".log") {
			continue
		}
		if len(readers) >= appLogMaxServiceFiles {
			log.Printf("WARN: app-logs %s: capping merge at %d service files (more present)", instanceID, appLogMaxServiceFiles)
			break
		}
		service := strings.TrimSuffix(name, ".log")
		snap := filepath.Join(tempDir, name)
		if err := copyFile(filepath.Join(instanceDir, name), snap); err != nil {
			log.Printf("WARN: snapshot app-logs %s/%s: %v", instanceID, name, err)
			continue
		}
		f, err := os.Open(snap)
		if err != nil {
			log.Printf("WARN: open app-logs snapshot %s: %v", snap, err)
			continue
		}
		readers = append(readers, newLogicalLineReader(service, f, f))
	}

	return writeMergedLogs(ctx, readers, since, until, maxLines, w)
}

// writeMergedLogs performs the k-way merge over per-service readers and writes
// the newest maxLines of the interleaved, window-filtered result to w. It makes
// two passes over the stable snapshots: one to count matching lines, then one
// to omit the oldest overflow and emit the tail. This preserves O(k) memory
// without allowing a chatty app to hide recent incident evidence behind the
// download cap. Split out from WriteHistoricLogsForApp so the merge/parse logic
// is testable without a mounted volume.
func writeMergedLogs(ctx context.Context, readers []*logicalLineReader, since, until time.Time, maxLines int, w io.Writer) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	if maxLines < 0 {
		maxLines = 0
	}

	newHeap := func() *logicalLineHeap {
		h := &logicalLineHeap{}
		for _, r := range readers {
			r.advance()
			if r.valid {
				heap.Push(h, r)
			}
		}
		return h
	}

	// Count matching logical lines first so the second pass can skip exactly
	// the oldest overflow while keeping only one head per service in memory.
	total := 0
	h := newHeap()
	for h.Len() > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r := heap.Pop(h).(*logicalLineReader)
		ts := r.curTS
		r.advance()
		if r.valid {
			heap.Push(h, r)
		}
		if !ts.Before(since) && !ts.After(until) {
			total++
		}
	}
	for _, r := range readers {
		if err := r.reset(); err != nil {
			return fmt.Errorf("rewind app-logs snapshot for %s: %w", r.service, err)
		}
	}

	// Per-service header when the oldest retained line is later than `since`:
	// the window starts before what this service's file still holds. Stated
	// claim-neutral — this is true both when the ring evicted older lines AND
	// when the service simply started logging after `since`; asserting a cause
	// here would over-claim eviction for young apps.
	// Computed from each reader's first buffered line, then the reader is reused
	// for the merge.
	h = &logicalLineHeap{}
	for _, r := range readers {
		r.advance()
		if r.valid {
			if r.curTS.After(since) {
				fmt.Fprintf(bw, "# [%s] earliest available log: %s\n",
					r.service, r.curTS.Format(time.RFC3339))
			}
			heap.Push(h, r)
		}
	}

	omitted := max(0, total-maxLines)
	if omitted > 0 {
		fmt.Fprintf(bw, "# Showing newest %d of %d matching log lines; older matching lines omitted: %d.\n",
			maxLines, total, omitted)
	}

	skipped := 0
	for h.Len() > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r := heap.Pop(h).(*logicalLineReader)
		ts, msg, svc := r.curTS, r.curMsg, r.service
		// Advance this reader and re-push if it still has lines (no early
		// terminate — every line is read and window-filtered).
		r.advance()
		if r.valid {
			heap.Push(h, r)
		}
		// Window filter on absolute time.
		if ts.Before(since) || ts.After(until) {
			continue
		}
		if skipped < omitted {
			skipped++
			continue
		}
		fmt.Fprintf(bw, "%s [%s] %s\n", ts.Format(time.RFC3339), svc, msg)
	}
	return nil
}

// copyFile copies src to dst (root reads the per-app-owned source; dst is the
// root-owned temp snapshot). The source lives in a per-app-user-writable
// subtree, so it is opened with O_NOFOLLOW + O_NONBLOCK and verified to be a
// regular file: a compromised per-app user must not be able to point a *.log
// entry at a symlink (e.g. /etc/shadow) and have root copy the target
// (confused-deputy read), nor plant a FIFO whose open blocks until a writer
// connects (which would wedge the per-app single-flight). O_NONBLOCK makes the
// FIFO open return immediately so the regular-file check can reject it.
func copyFile(src, dst string) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular log source %s (mode %v)", src, fi.Mode())
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
