package persistence

import "testing"

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
