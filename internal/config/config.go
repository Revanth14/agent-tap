// Package config holds the atap daemon settings.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	// DefaultHost binds loopback only: the daemon serves prompts, transcripts
	// and tool arguments, which must not be reachable from the LAN.
	DefaultHost = "127.0.0.1"

	DefaultPort = 9090

	// DefaultAnthropicUpstream is where the /v1/messages proxy forwards requests.
	DefaultAnthropicUpstream = "https://api.anthropic.com"

	// DefaultRetention is how long persisted telemetry is kept. It holds
	// prompt previews and tool targets, so it is not kept indefinitely.
	DefaultRetention = 30 * 24 * time.Hour
)

// Config is the daemon configuration.
type Config struct {
	Host              string        // Listen host for proxy, REST API, hooks, WebSocket and UI
	Port              int           // Listen port
	AnthropicUpstream string        // Upstream base URL for the Claude Code adapter
	DBPath            string        // SQLite database file
	Retention         time.Duration // Rows older than this are deleted at startup
}

// Default returns the default configuration, with the database at
// ~/.agent-tap/state.db.
func Default() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("config: resolve home dir: %w", err)
	}
	return Config{
		Host:              DefaultHost,
		Port:              DefaultPort,
		AnthropicUpstream: DefaultAnthropicUpstream,
		DBPath:            filepath.Join(home, ".agent-tap", "state.db"),
		Retention:         DefaultRetention,
	}, nil
}

// Addr returns the listen address, host:port.
func (c Config) Addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}
