package telemetry

import "strings"

// DefaultMaxContextWindow applies to models without a known prefix.
const DefaultMaxContextWindow = 200_000

// contextWindows maps model ID prefixes to their context window in tokens.
// "claude-opus" also covers "claude-opus-4", and likewise for sonnet and haiku.
var contextWindows = []struct {
	prefix string
	tokens int
}{
	{"claude-opus", 1_000_000},
	{"claude-sonnet", 1_000_000},
	{"claude-haiku", 200_000},
}

// MaxContextWindow returns the context window for a model ID, falling back to
// DefaultMaxContextWindow.
func MaxContextWindow(model string) int {
	for _, w := range contextWindows {
		if strings.HasPrefix(model, w.prefix) {
			return w.tokens
		}
	}
	return DefaultMaxContextWindow
}
