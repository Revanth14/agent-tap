package telemetry

import "testing"

func TestMaxContextWindow(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"claude-opus-4-5-20251101", 1_000_000},
		{"claude-opus", 1_000_000},
		{"claude-sonnet-4-5", 1_000_000},
		{"claude-haiku-4-5-20251001", 200_000},
		{"some-unknown-model", 200_000},
		{"", 200_000},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := MaxContextWindow(tt.model); got != tt.want {
				t.Errorf("MaxContextWindow(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}
