package client

import "testing"

func TestShouldApplyRelease(t *testing.T) {
	tests := []struct {
		name    string
		current string
		latest  string
		want    bool
	}{
		{
			name:    "same version",
			current: "0.12.3",
			latest:  "v0.12.3",
			want:    false,
		},
		{
			name:    "newer patch",
			current: "0.12.3",
			latest:  "v0.12.4",
			want:    true,
		},
		{
			name:    "newer minor",
			current: "0.12.9",
			latest:  "v0.13.0",
			want:    true,
		},
		{
			name:    "newer major",
			current: "0.12.9",
			latest:  "v1.0.0",
			want:    true,
		},
		{
			name:    "current is newer",
			current: "0.12.4",
			latest:  "v0.12.3",
			want:    false,
		},
		{
			name:    "source build updates to release",
			current: "dev",
			latest:  "v0.12.3",
			want:    true,
		},
		{
			name:    "sha build updates to release",
			current: "164c0dd",
			latest:  "v0.12.3",
			want:    true,
		},
		{
			name:    "invalid latest does not update",
			current: "0.12.3",
			latest:  "main",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldApplyRelease(tt.current, tt.latest); got != tt.want {
				t.Fatalf("shouldApplyRelease(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
			}
		})
	}
}
