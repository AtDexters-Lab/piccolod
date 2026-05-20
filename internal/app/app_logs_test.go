package app

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseK8sFileLogLine(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantOK      bool
		wantPartial bool
		wantMsg     string
	}{
		{"full", "2026-05-20T15:47:20.083891798+05:30 stdout F hello world", true, false, "hello world"},
		{"partial", "2026-05-20T15:47:20.0+00:00 stdout P chunk", true, true, "chunk"},
		{"stderr full", "2026-05-20T15:47:20.0Z stderr F oops", true, false, "oops"},
		{"empty message", "2026-05-20T15:47:20.0Z stdout F ", true, false, ""},
		{"bad timestamp", "not-a-ts stdout F hi", false, false, ""},
		{"bad tag", "2026-05-20T15:47:20.0Z stdout X hi", false, false, ""},
		{"too few fields", "2026-05-20T15:47:20.0Z stdout", false, false, ""},
		{"app line that looks like a log", "INFO something happened", false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, partial, msg, ok := parseK8sFileLogLine(c.raw)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if partial != c.wantPartial {
				t.Errorf("partial=%v want %v", partial, c.wantPartial)
			}
			if msg != c.wantMsg {
				t.Errorf("msg=%q want %q", msg, c.wantMsg)
			}
		})
	}
}

func newReader(service, content string) *logicalLineReader {
	return &logicalLineReader{service: service, br: bufio.NewReader(strings.NewReader(content))}
}

// drain reads all logical lines from a reader for assertions.
func drain(r *logicalLineReader) []string {
	var out []string
	for {
		r.advance()
		if !r.valid {
			return out
		}
		out = append(out, r.curMsg)
	}
}

func TestLogicalLineReader_Reassembly(t *testing.T) {
	// Two P segments + terminating F = one logical line; a following F = another.
	content := strings.Join([]string{
		"2026-05-20T10:00:00Z stdout P part1-",
		"2026-05-20T10:00:00Z stdout P part2-",
		"2026-05-20T10:00:00Z stdout F part3",
		"2026-05-20T10:00:01Z stdout F second",
	}, "\n") + "\n"
	got := drain(newReader("web", content))
	want := []string{"part1-part2-part3", "second"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q want %q", i, got[i], want[i])
		}
	}
}

func TestLogicalLineReader_TornTrailingDropped(t *testing.T) {
	// A dangling P with no terminating F (writer mid-append) is dropped (Q2).
	content := "2026-05-20T10:00:00Z stdout F complete\n2026-05-20T10:00:01Z stdout P dangling"
	got := drain(newReader("web", content))
	if len(got) != 1 || got[0] != "complete" {
		t.Fatalf("got %v, want [complete] (torn line dropped)", got)
	}
}

func TestLogicalLineReader_MalformedDropped(t *testing.T) {
	content := "garbage line\n2026-05-20T10:00:00Z stdout F good\nmore garbage\n"
	got := drain(newReader("web", content))
	if len(got) != 1 || got[0] != "good" {
		t.Fatalf("got %v, want [good]", got)
	}
}

func TestLogicalLineReader_LengthCap(t *testing.T) {
	huge := strings.Repeat("x", appLogMaxLineBytes+5000)
	content := "2026-05-20T10:00:00Z stdout F " + huge + "\n"
	got := drain(newReader("web", content))
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1", len(got))
	}
	if !strings.HasSuffix(got[0], "…[truncated]") {
		t.Errorf("expected truncation marker, got suffix %q", got[0][len(got[0])-20:])
	}
	if len(got[0]) > appLogMaxLineBytes+64 {
		t.Errorf("line length %d exceeds cap+marker", len(got[0]))
	}
}

func TestWriteMergedLogs_InterleaveAndWindow(t *testing.T) {
	web := newReader("web", strings.Join([]string{
		"2026-05-20T10:00:01Z stdout F web-a",
		"2026-05-20T10:00:03Z stdout F web-b",
		"2026-05-20T10:00:09Z stdout F web-late",
	}, "\n")+"\n")
	db := newReader("db", strings.Join([]string{
		"2026-05-20T10:00:02Z stdout F db-a",
		"2026-05-20T10:00:04Z stdout F db-b",
	}, "\n")+"\n")

	since := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 20, 10, 0, 5, 0, time.UTC)

	var sb strings.Builder
	if err := writeMergedLogs(context.Background(), []*logicalLineReader{web, db}, since, until, 1000, &sb); err != nil {
		t.Fatalf("writeMergedLogs: %v", err)
	}
	out := sb.String()

	// Interleaved by timestamp; web-late (10:00:09) is outside the window.
	wantOrder := []string{"web-a", "db-a", "web-b", "db-b"}
	idx := -1
	for _, tag := range wantOrder {
		i := strings.Index(out, tag)
		if i < 0 {
			t.Fatalf("missing %q in output:\n%s", tag, out)
		}
		if i < idx {
			t.Fatalf("out of order: %q at %d before previous %d:\n%s", tag, i, idx, out)
		}
		idx = i
	}
	if strings.Contains(out, "web-late") {
		t.Errorf("web-late should be filtered out of [since,until]:\n%s", out)
	}
}

func TestWriteMergedLogs_MaxLinesTruncates(t *testing.T) {
	web := newReader("web", strings.Join([]string{
		"2026-05-20T10:00:01Z stdout F a",
		"2026-05-20T10:00:02Z stdout F b",
		"2026-05-20T10:00:03Z stdout F c",
	}, "\n")+"\n")
	since := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 20, 11, 0, 0, 0, time.UTC)
	var sb strings.Builder
	if err := writeMergedLogs(context.Background(), []*logicalLineReader{web}, since, until, 2, &sb); err != nil {
		t.Fatalf("writeMergedLogs: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "truncated at 2 lines") {
		t.Errorf("expected truncation marker, got:\n%s", out)
	}
	if strings.Contains(out, "] c") {
		t.Errorf("third line should not appear past maxLines:\n%s", out)
	}
}

func TestWriteMergedLogs_EvictedHeader(t *testing.T) {
	// Requested since is BEFORE the oldest available line → per-service header.
	web := newReader("web", "2026-05-20T10:00:05Z stdout F only\n")
	since := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 20, 11, 0, 0, 0, time.UTC)
	var sb strings.Builder
	if err := writeMergedLogs(context.Background(), []*logicalLineReader{web}, since, until, 1000, &sb); err != nil {
		t.Fatalf("writeMergedLogs: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "# [web] earliest available") {
		t.Errorf("expected evicted-header for web, got:\n%s", out)
	}
}
