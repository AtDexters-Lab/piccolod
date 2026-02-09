package app

import (
	"strings"
	"testing"
)

func TestParseFUSEMounts(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		prefix string
		want   []fuseMount
	}{
		{
			name:   "empty",
			input:  "",
			prefix: "/var/lib/piccolod",
			want:   nil,
		},
		{
			name: "no_fuse_mounts",
			input: `sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
/dev/sda1 / ext4 rw,relatime 0 0`,
			prefix: "/var/lib/piccolod",
			want:   nil,
		},
		{
			name: "fuse_mount_outside_prefix",
			input: `fuse-overlayfs /home/user/.local/share/containers/overlay/abc/merged fuse.fuse-overlayfs rw,nosuid,nodev,relatime 0 0`,
			prefix: "/var/lib/piccolod",
			want:   nil,
		},
		{
			name: "single_image_root_overlay",
			input: `fuse-overlayfs /var/lib/piccolod/podman/image-root/overlay/610fd6c/merged fuse.fuse-overlayfs rw,nosuid,nodev,relatime 0 0`,
			prefix: "/var/lib/piccolod",
			want:   []fuseMount{{path: "/var/lib/piccolod/podman/image-root/overlay/610fd6c/merged", fstype: "fuse.fuse-overlayfs"}},
		},
		{
			name: "multiple_stale_mounts",
			input: `fuse-overlayfs /var/lib/piccolod/podman/image-root/overlay/aaa/merged fuse.fuse-overlayfs rw 0 0
fuse-overlayfs /var/lib/piccolod/podman/image-root/overlay/bbb/merged fuse.fuse-overlayfs rw 0 0
fuse-overlayfs /var/lib/piccolod/mounts/app-d1/disk/workspace/merged fuse.fuse-overlayfs rw 0 0`,
			prefix: "/var/lib/piccolod",
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
			prefix: "/var/lib/piccolod",
			want:   []fuseMount{{path: "/var/lib/piccolod/podman/image-root/overlay/abc/merged", fstype: "fuse.fuse-overlayfs"}},
		},
		{
			name: "prefix_not_directory_boundary",
			input: `fuse-overlayfs /var/lib/piccolod-other/overlay/abc/merged fuse.fuse-overlayfs rw 0 0`,
			prefix: "/var/lib/piccolod",
			want:   nil,
		},
		{
			name:   "gocryptfs_mount_under_prefix",
			input:  `gocryptfs /var/lib/piccolod/mounts/app-d1 fuse.gocryptfs rw,nosuid,nodev,relatime 0 0`,
			prefix: "/var/lib/piccolod",
			want:   []fuseMount{{path: "/var/lib/piccolod/mounts/app-d1", fstype: "fuse.gocryptfs"}},
		},
		{
			name: "mixed_fuse_types",
			input: `gocryptfs /var/lib/piccolod/mounts/app-d1 fuse.gocryptfs rw 0 0
fuse-overlayfs /var/lib/piccolod/mounts/app-d1/disk/workspace/merged fuse.fuse-overlayfs rw 0 0
sshfs /home/user/remote fuse.sshfs rw 0 0`,
			prefix: "/var/lib/piccolod",
			want: []fuseMount{
				{path: "/var/lib/piccolod/mounts/app-d1", fstype: "fuse.gocryptfs"},
				{path: "/var/lib/piccolod/mounts/app-d1/disk/workspace/merged", fstype: "fuse.fuse-overlayfs"},
			},
		},
		{
			name:   "gocryptfs_outside_prefix",
			input:  `gocryptfs /home/user/encrypted fuse.gocryptfs rw 0 0`,
			prefix: "/var/lib/piccolod",
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFUSEMounts(strings.NewReader(tt.input), tt.prefix)
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
