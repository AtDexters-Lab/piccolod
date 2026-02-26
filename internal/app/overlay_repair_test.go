package app

import (
	"strings"
	"testing"
)

func TestParseOverlayMounts(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		prefixes []string
		want     []staleMount
	}{
		{
			name:     "empty",
			input:    "",
			prefixes: []string{"/var/lib/piccolod"},
			want:     nil,
		},
		{
			name: "no_overlay_mounts",
			input: `sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
/dev/sda1 / ext4 rw,relatime 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     nil,
		},
		{
			name:     "overlay_outside_prefix",
			input:    `overlay /home/user/.local/share/containers/overlay/abc/merged overlay rw,nosuid,nodev,relatime 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     nil,
		},
		{
			name:     "single_image_root_overlay",
			input:    `overlay /var/lib/piccolod/podman/image-root/overlay/610fd6c/merged overlay rw,nosuid,nodev,relatime 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     []staleMount{{path: "/var/lib/piccolod/podman/image-root/overlay/610fd6c/merged", fstype: "overlay"}},
		},
		{
			name: "multiple_stale_mounts",
			input: `overlay /var/lib/piccolod/podman/image-root/overlay/aaa/merged overlay rw 0 0
overlay /var/lib/piccolod/podman/image-root/overlay/bbb/merged overlay rw 0 0
overlay /var/lib/piccolod/mounts/app-d1/disk/workspace/merged overlay rw 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want: []staleMount{
				{path: "/var/lib/piccolod/podman/image-root/overlay/aaa/merged", fstype: "overlay"},
				{path: "/var/lib/piccolod/podman/image-root/overlay/bbb/merged", fstype: "overlay"},
				{path: "/var/lib/piccolod/mounts/app-d1/disk/workspace/merged", fstype: "overlay"},
			},
		},
		{
			name: "mixed_overlay_and_non_overlay",
			input: `sysfs /sys sysfs rw 0 0
overlay /var/lib/piccolod/podman/image-root/overlay/abc/merged overlay rw 0 0
/dev/sda1 /var/lib/piccolod ext4 rw 0 0
overlay /other/path overlay rw 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     []staleMount{{path: "/var/lib/piccolod/podman/image-root/overlay/abc/merged", fstype: "overlay"}},
		},
		{
			name:     "prefix_not_directory_boundary",
			input:    `overlay /var/lib/piccolod-other/overlay/abc/merged overlay rw 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     nil,
		},
		{
			name:     "fuse_overlay_ignored",
			input:    `fuse-overlayfs /var/lib/piccolod/mounts/app-d1/disk/workspace/merged fuse.fuse-overlayfs rw 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     nil,
		},
		// Multi-prefix test cases
		{
			name:     "runroot_overlay_matched_by_second_prefix",
			input:    `overlay /run/piccolo/podman/image-root/overlay/abc123/merged overlay rw 0 0`,
			prefixes: []string{"/piccolo-core", "/run/piccolo/podman"},
			want:     []staleMount{{path: "/run/piccolo/podman/image-root/overlay/abc123/merged", fstype: "overlay"}},
		},
		{
			name:     "image_graphroot_overlay_matched_by_third_prefix",
			input:    `overlay /piccolo-data/node/podman/image-root/overlay/def456/merged overlay rw 0 0`,
			prefixes: []string{"/piccolo-core", "/run/piccolo/podman", "/piccolo-data/node/podman/image-root"},
			want:     []staleMount{{path: "/piccolo-data/node/podman/image-root/overlay/def456/merged", fstype: "overlay"}},
		},
		{
			name:     "prefix_boundary_no_false_positive",
			input:    `overlay /piccolo-data/node/podman-backup/overlay/abc/merged overlay rw 0 0`,
			prefixes: []string{"/piccolo-core", "/run/piccolo/podman", "/piccolo-data/node/podman/image-root"},
			want:     nil,
		},
		{
			name: "all_three_prefixes_in_single_call",
			input: `overlay /piccolo-core/mounts/app-d1/disk/podman/overlay/aaa/merged overlay rw 0 0
overlay /run/piccolo/podman/vol-x/overlay/bbb/merged overlay rw 0 0
overlay /piccolo-data/node/podman/image-root/overlay/ccc/merged overlay rw 0 0
overlay /home/user/.local/share/containers/overlay/ddd/merged overlay rw 0 0`,
			prefixes: []string{"/piccolo-core", "/run/piccolo/podman", "/piccolo-data/node/podman/image-root"},
			want: []staleMount{
				{path: "/piccolo-core/mounts/app-d1/disk/podman/overlay/aaa/merged", fstype: "overlay"},
				{path: "/run/piccolo/podman/vol-x/overlay/bbb/merged", fstype: "overlay"},
				{path: "/piccolo-data/node/podman/image-root/overlay/ccc/merged", fstype: "overlay"},
			},
		},
		{
			name:     "mount_outside_all_prefixes",
			input:    `overlay /tmp/other/overlay/merged overlay rw 0 0`,
			prefixes: []string{"/piccolo-core", "/run/piccolo/podman", "/piccolo-data/node/podman/image-root"},
			want:     nil,
		},
		{
			name:     "no_prefixes",
			input:    `overlay /piccolo-core/mounts/app-d1/disk/workspace/merged overlay rw 0 0`,
			prefixes: nil,
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseOverlayMounts(strings.NewReader(tt.input), tt.prefixes...)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d mounts, want %d: %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("mount[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}
