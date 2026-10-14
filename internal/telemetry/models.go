package telemetry

import (
	"strconv"
	"strings"
)

const (
	// DefaultMaxContextWindow applies to models without a 1M window.
	DefaultMaxContextWindow = 200_000

	longContextWindow = 1_000_000
)

type modelVersion struct{ major, minor int }

func (v modelVersion) atLeast(w modelVersion) bool {
	return v.major > w.major || (v.major == w.major && v.minor >= w.minor)
}

// native1M maps a model family to the first version with a 1M context window
// by default (Claude Code docs, 2026-10). Unknown families get the default.
var native1M = map[string]modelVersion{
	"opus":   {4, 7},
	"sonnet": {5, 0},
	"haiku":  {5, 5},
	"fable":  {0, 0}, // every Fable model
}

// MaxContextWindow returns the context window for a model ID as sent on the
// wire. longContext reports a per-request 1M opt-in, which the provider adapter
// detects (for Claude Code, the context-1m beta flag).
func MaxContextWindow(model string, longContext bool) int {
	if longContext {
		return longContextWindow
	}
	family, v, ok := parseModelID(model)
	if !ok {
		return DefaultMaxContextWindow
	}
	if first, ok := native1M[family]; ok && v.atLeast(first) {
		return longContextWindow
	}
	return DefaultMaxContextWindow
}

// parseModelID parses "claude-<family>-<major>[-<minor>][-<yyyymmdd>]", e.g.
// "claude-opus-4-7" or "claude-haiku-4-5-20251001".
func parseModelID(id string) (family string, v modelVersion, ok bool) {
	rest, ok := strings.CutPrefix(id, "claude-")
	if !ok {
		return "", v, false
	}
	parts := strings.Split(rest, "-")
	if len(parts) < 2 {
		return "", v, false
	}
	major, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", v, false
	}
	v.major = major
	// An 8-digit part is a date snapshot, not a minor version.
	if len(parts) > 2 && len(parts[2]) < 8 {
		if minor, err := strconv.Atoi(parts[2]); err == nil {
			v.minor = minor
		}
	}
	return parts[0], v, true
}
