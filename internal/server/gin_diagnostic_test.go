package server

import (
	"context"
	"reflect"
	"syscall"
	"testing"
	"time"

	"piccolod/internal/container"
)

func TestParseDiagnosticRange(t *testing.T) {
	now := time.Date(2026, 2, 15, 14, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		from, to  string
		wantSince time.Time
		wantUntil time.Time
		wantErr   bool
	}{
		{
			name:      "default_last_3_days",
			from:      "",
			to:        "",
			wantSince: now.AddDate(0, 0, -3),
			wantUntil: now,
		},
		{
			name:      "from_only",
			from:      "2026-02-13",
			to:        "",
			wantSince: time.Date(2026, 2, 13, 0, 0, 0, 0, time.UTC),
			wantUntil: now,
		},
		{
			name:      "to_only",
			from:      "",
			to:        "2026-02-14",
			wantSince: time.Date(2026, 2, 12, 0, 0, 0, 0, time.UTC),
			wantUntil: time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			name:      "both_specified",
			from:      "2026-02-10",
			to:        "2026-02-14",
			wantSince: time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC),
			wantUntil: time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			name:    "exceeds_7_days",
			from:    "2026-02-01",
			to:      "2026-02-14",
			wantErr: true,
		},
		{
			name:    "from_after_to",
			from:    "2026-02-15",
			to:      "2026-02-10",
			wantErr: true,
		},
		{
			name:    "invalid_from_format",
			from:    "not-a-date",
			to:      "",
			wantErr: true,
		},
		{
			name:    "invalid_to_format",
			from:    "",
			to:      "2026/02/14",
			wantErr: true,
		},
		{
			name:      "same_day",
			from:      "2026-02-14",
			to:        "2026-02-14",
			wantSince: time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC),
			wantUntil: time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			name:      "exactly_7_days",
			from:      "2026-02-08",
			to:        "2026-02-14",
			wantSince: time.Date(2026, 2, 8, 0, 0, 0, 0, time.UTC),
			wantUntil: time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			name:    "from_only_exceeds_7_days",
			from:    "2026-02-01",
			to:      "",
			wantErr: true,
		},
		{
			name:    "from_one_day_after_to",
			from:    "2026-02-15",
			to:      "2026-02-14",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			since, until, err := parseDiagnosticRange(tt.from, tt.to, now)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !since.Equal(tt.wantSince) {
				t.Errorf("since = %v, want %v", since, tt.wantSince)
			}
			if !until.Equal(tt.wantUntil) {
				t.Errorf("until = %v, want %v", until, tt.wantUntil)
			}
		})
	}
}

func TestNewDiagnosticPodmanImagesCommandAppliesRootlessWrapper(t *testing.T) {
	rt := container.PodmanRuntime{
		Credential: &syscall.Credential{Uid: 475, Gid: 475},
		HomeDir:    "/var/lib/piccolo/apps/namek/home",
	}
	cmd := newDiagnosticPodmanImagesCommand(
		context.Background(),
		rt,
		[]string{"--root", "/apps/namek/podman", "images", "--format", "{{.Repository}}:{{.Tag}}"},
	)

	wantArgs := []string{
		"/usr/bin/choom", "-n", "0", "--", "/usr/bin/podman",
		"--root", "/apps/namek/podman", "images", "--format", "{{.Repository}}:{{.Tag}}",
	}
	if cmd.Path != "/usr/bin/choom" || !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("diagnostic command = path %q args %v, want %q %v", cmd.Path, cmd.Args, "/usr/bin/choom", wantArgs)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential != rt.Credential {
		t.Fatal("diagnostic command did not receive the ephemeral runtime credential")
	}
}
