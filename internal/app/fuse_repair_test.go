package app

import (
	"strings"
	"testing"
)

func TestParseFUSEMounts(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		prefixes []string
		want     []fuseMount
	}{
		{
			name:     "empty",
			input:    "",
			prefixes: []string{"/var/lib/piccolod"},
			want:     nil,
		},
		{
			name: "no_fuse_mounts",
			input: `sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
/dev/sda1 / ext4 rw,relatime 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     nil,
		},
		{
			name:     "fuse_mount_outside_prefix",
			input:    `fuse-overlayfs /home/user/.local/share/containers/overlay/abc/merged fuse.fuse-overlayfs rw,nosuid,nodev,relatime 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     nil,
		},
		{
			name:     "single_image_root_overlay",
			input:    `fuse-overlayfs /var/lib/piccolod/podman/image-root/overlay/610fd6c/merged fuse.fuse-overlayfs rw,nosuid,nodev,relatime 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     []fuseMount{{path: "/var/lib/piccolod/podman/image-root/overlay/610fd6c/merged", fstype: "fuse.fuse-overlayfs"}},
		},
		{
			name: "multiple_stale_mounts",
			input: `fuse-overlayfs /var/lib/piccolod/podman/image-root/overlay/aaa/merged fuse.fuse-overlayfs rw 0 0
fuse-overlayfs /var/lib/piccolod/podman/image-root/overlay/bbb/merged fuse.fuse-overlayfs rw 0 0
fuse-overlayfs /var/lib/piccolod/mounts/app-d1/disk/workspace/merged fuse.fuse-overlayfs rw 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want: []fuseMount{
				{path: "/var/lib/piccolod/podman/image-root/overlay/aaa/merged", fstype: "fuse.fuse-overlayfs"},
				{path: "/var/lib/piccolod/podman/image-root/overlay/bbb/merged", fstype: "fuse.fuse-overlayfs"},
				{path: "/var/lib/piccolod/mounts/app-d1/disk/workspace/merged", fstype: "fuse.fuse-overlayfs"},
			},
		},
		{
			name: "mixed_fuse_and_non_fuse",
			input: `sysfs /sys sysfs rw 0 0
fuse-overlayfs /var/lib/piccolod/podman/image-root/overlay/abc/merged fuse.fuse-overlayfs rw 0 0
/dev/sda1 /var/lib/piccolod ext4 rw 0 0
fuse-overlayfs /other/path fuse.fuse-overlayfs rw 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     []fuseMount{{path: "/var/lib/piccolod/podman/image-root/overlay/abc/merged", fstype: "fuse.fuse-overlayfs"}},
		},
		{
			name:     "prefix_not_directory_boundary",
			input:    `fuse-overlayfs /var/lib/piccolod-other/overlay/abc/merged fuse.fuse-overlayfs rw 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     nil,
		},
		{
			name:     "gocryptfs_mount_under_prefix",
			input:    `gocryptfs /var/lib/piccolod/mounts/app-d1 fuse.gocryptfs rw,nosuid,nodev,relatime 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     []fuseMount{{path: "/var/lib/piccolod/mounts/app-d1", fstype: "fuse.gocryptfs"}},
		},
		{
			name: "mixed_fuse_types",
			input: `gocryptfs /var/lib/piccolod/mounts/app-d1 fuse.gocryptfs rw 0 0
fuse-overlayfs /var/lib/piccolod/mounts/app-d1/disk/workspace/merged fuse.fuse-overlayfs rw 0 0
sshfs /home/user/remote fuse.sshfs rw 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want: []fuseMount{
				{path: "/var/lib/piccolod/mounts/app-d1", fstype: "fuse.gocryptfs"},
				{path: "/var/lib/piccolod/mounts/app-d1/disk/workspace/merged", fstype: "fuse.fuse-overlayfs"},
			},
		},
		{
			name:     "gocryptfs_outside_prefix",
			input:    `gocryptfs /home/user/encrypted fuse.gocryptfs rw 0 0`,
			prefixes: []string{"/var/lib/piccolod"},
			want:     nil,
		},
		// Multi-prefix test cases
		{
			name:     "runroot_overlay_matched_by_second_prefix",
			input:    `fuse-overlayfs /run/piccolo/podman/image-root/overlay/abc123/merged fuse.fuse-overlayfs rw 0 0`,
			prefixes: []string{"/piccolo-core", "/run/piccolo/podman"},
			want:     []fuseMount{{path: "/run/piccolo/podman/image-root/overlay/abc123/merged", fstype: "fuse.fuse-overlayfs"}},
		},
		{
			name:     "image_graphroot_overlay_matched_by_third_prefix",
			input:    `fuse-overlayfs /piccolo-data/node/podman/image-root/overlay/def456/merged fuse.fuse-overlayfs rw 0 0`,
			prefixes: []string{"/piccolo-core", "/run/piccolo/podman", "/piccolo-data/node/podman/image-root"},
			want:     []fuseMount{{path: "/piccolo-data/node/podman/image-root/overlay/def456/merged", fstype: "fuse.fuse-overlayfs"}},
		},
		{
			name:     "prefix_boundary_no_false_positive",
			input:    `fuse-overlayfs /piccolo-data/node/podman-backup/overlay/abc/merged fuse.fuse-overlayfs rw 0 0`,
			prefixes: []string{"/piccolo-core", "/run/piccolo/podman", "/piccolo-data/node/podman/image-root"},
			want:     nil,
		},
		{
			name: "all_three_prefixes_in_single_call",
			input: `gocryptfs /piccolo-core/mounts/app-d1 fuse.gocryptfs rw 0 0
fuse-overlayfs /piccolo-core/mounts/app-d1/disk/podman/overlay/aaa/merged fuse.fuse-overlayfs rw 0 0
fuse-overlayfs /run/piccolo/podman/vol-x/overlay/bbb/merged fuse.fuse-overlayfs rw 0 0
fuse-overlayfs /piccolo-data/node/podman/image-root/overlay/ccc/merged fuse.fuse-overlayfs rw 0 0
fuse-overlayfs /home/user/.local/share/containers/overlay/ddd/merged fuse.fuse-overlayfs rw 0 0`,
			prefixes: []string{"/piccolo-core", "/run/piccolo/podman", "/piccolo-data/node/podman/image-root"},
			want: []fuseMount{
				{path: "/piccolo-core/mounts/app-d1", fstype: "fuse.gocryptfs"},
				{path: "/piccolo-core/mounts/app-d1/disk/podman/overlay/aaa/merged", fstype: "fuse.fuse-overlayfs"},
				{path: "/run/piccolo/podman/vol-x/overlay/bbb/merged", fstype: "fuse.fuse-overlayfs"},
				{path: "/piccolo-data/node/podman/image-root/overlay/ccc/merged", fstype: "fuse.fuse-overlayfs"},
			},
		},
		{
			name:     "mount_outside_all_prefixes",
			input:    `fuse-overlayfs /tmp/other/overlay/merged fuse.fuse-overlayfs rw 0 0`,
			prefixes: []string{"/piccolo-core", "/run/piccolo/podman", "/piccolo-data/node/podman/image-root"},
			want:     nil,
		},
		{
			name:     "no_prefixes",
			input:    `fuse-overlayfs /piccolo-core/mounts/app-d1/disk/workspace/merged fuse.fuse-overlayfs rw 0 0`,
			prefixes: nil,
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFUSEMounts(strings.NewReader(tt.input), tt.prefixes...)
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
