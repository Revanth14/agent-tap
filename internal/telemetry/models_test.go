package telemetry

import "testing"

func TestMaxContextWindow(t *testing.T) {
	tests := []struct {
		model       string
		longContext bool
		want        int
	}{
		{"claude-opus-5-5", false, 1_000_000},
		{"claude-opus-4-7", false, 1_000_000},
		{"claude-opus-4-6", false, 200_000},
		{"claude-opus-4-6", true, 1_000_000}, // [1m] opt-in, seen on the wire as the beta flag
		{"claude-opus-4-20250514", false, 200_000},
		{"claude-sonnet-5-5", false, 1_000_000},
		{"claude-sonnet-4-5-20250929", false, 200_000},
		{"claude-haiku-5-5", false, 1_000_000},
		{"claude-haiku-4-5-20251001", false, 200_000},
		{"claude-fable-5-1", false, 1_000_000},
		{"claude-3-5-haiku-20241022", false, 200_000},
		{"some-unknown-model", false, 200_000},
		{"", false, 200_000},
	}
	for _, tt := range tests {
		if got := MaxContextWindow(tt.model, tt.longContext); got != tt.want {
			t.Errorf("MaxContextWindow(%q, %v) = %d, want %d", tt.model, tt.longContext, got, tt.want)
		}
	}
}
