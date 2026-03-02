package persistence

import (
	"testing"
)

func TestServiceRootfsVolumeID(t *testing.T) {
	tests := []struct {
		name        string
		instanceID  string
		serviceName string
		want        string
	}{
		{
			name:        "empty_service_name_returns_legacy",
			instanceID:  "myapp-123",
			serviceName: "",
			want:        "svc-rootfs-myapp-123",
		},
		{
			name:        "single_service",
			instanceID:  "myapp-123",
			serviceName: "web",
			want:        "svc-rootfs-myapp-123--web",
		},
		{
			name:        "postgres_service",
			instanceID:  "nextcloud-456",
			serviceName: "postgres",
			want:        "svc-rootfs-nextcloud-456--postgres",
		},
		{
			name:        "service_with_hyphens",
			instanceID:  "app-1",
			serviceName: "my-service",
			want:        "svc-rootfs-app-1--my-service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ServiceRootfsVolumeID(tt.instanceID, tt.serviceName)
			if got != tt.want {
				t.Errorf("ServiceRootfsVolumeID(%q, %q) = %q, want %q",
					tt.instanceID, tt.serviceName, got, tt.want)
			}
		})
	}
}

func TestVersionedServiceRootfsVolumeID(t *testing.T) {
	tests := []struct {
		name        string
		instanceID  string
		serviceName string
		shortDigest string
		want        string
	}{
		{
			name:        "single_service_with_digest",
			instanceID:  "myapp",
			serviceName: "web",
			shortDigest: "a1b2c3d4e5f6",
			want:        "svc-rootfs-myapp--web--a1b2c3d4e5f6",
		},
		{
			name:        "legacy_service_empty_name",
			instanceID:  "myapp",
			serviceName: "",
			shortDigest: "abc123",
			want:        "svc-rootfs-myapp--abc123",
		},
		{
			name:        "long_instance_and_service",
			instanceID:  "my-home-assistant-app",
			serviceName: "core-service",
			shortDigest: "deadbeef1234",
			want:        "svc-rootfs-my-home-assistant-app--core-service--deadbeef1234",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := VersionedServiceRootfsVolumeID(tt.instanceID, tt.serviceName, tt.shortDigest)
			if got != tt.want {
				t.Errorf("VersionedServiceRootfsVolumeID(%q, %q, %q) = %q, want %q",
					tt.instanceID, tt.serviceName, tt.shortDigest, got, tt.want)
			}
		})
	}
}

func TestGoldenLVSizeForImage(t *testing.T) {
	tests := []struct {
		name          string
		imageSizeBytes int64
		want          int64
	}{
		{
			name:          "zero_falls_back_to_default",
			imageSizeBytes: 0,
			want:          defaultGoldenLVSize,
		},
		{
			name:          "negative_falls_back_to_default",
			imageSizeBytes: -100,
			want:          defaultGoldenLVSize,
		},
		{
			name:          "small_image_plus_1GiB_wins",
			imageSizeBytes: 50 << 20, // 50 MiB
			// 1.5x = 75 MiB, +1 GiB = ~1074 MiB → +1 GiB wins
			want: 50<<20 + 1<<30,
		},
		{
			name:          "medium_image_plus_1GiB_wins",
			imageSizeBytes: 500 << 20, // 500 MiB
			// 1.5x = 750 MiB, +1 GiB = 1524 MiB → +1 GiB wins
			want: 500<<20 + 1<<30,
		},
		{
			name:          "large_image_1.5x_wins",
			imageSizeBytes: 4 << 30, // 4 GiB
			// 1.5x = 6 GiB, +1 GiB = 5 GiB → 1.5x wins
			want: 4<<30 + 4<<30/2,
		},
		{
			name:          "boundary_at_2GiB",
			imageSizeBytes: 2 << 30, // 2 GiB
			// 1.5x = 3 GiB, +1 GiB = 3 GiB → equal, sizeA is used (both 3 GiB)
			want: 2<<30 + 2<<30/2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := goldenLVSizeForImage(tt.imageSizeBytes)
			if got != tt.want {
				t.Errorf("goldenLVSizeForImage(%d) = %d, want %d", tt.imageSizeBytes, got, tt.want)
			}
		})
	}
}
