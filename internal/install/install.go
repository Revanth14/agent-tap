// Package install wires Claude Code to the atap daemon and back: atap init,
// atap uninit and atap status. Every path and side effect goes through Env,
// so it can be exercised against a temporary home directory.
package install

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const label = "dev.agent-tap.daemon"

// Env is everything init, uninit and status touch.
type Env struct {
	Home          string // The user's home directory
	Binary        string // The running atap executable, copied by init
	Port          int
	UID           int
	Launchctl     func(args ...string) error
	Client        *http.Client
	HealthTimeout time.Duration // How long init waits for the daemon
}

// System returns the Env for the current user and executable.
func System(port int) (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, err
	}
	bin, err := os.Executable()
	if err != nil {
		return Env{}, err
	}
	return Env{
		Home:   home,
		Binary: bin,
		Port:   port,
		UID:    os.Getuid(),
		Launchctl: func(args ...string) error {
			if out, err := exec.Command("launchctl", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		Client:        &http.Client{Timeout: 2 * time.Second},
		HealthTimeout: 15 * time.Second,
	}, nil
}

func (e Env) settingsPath() string { return filepath.Join(e.Home, ".claude", "settings.json") }
func (e Env) backupPath() string   { return e.settingsPath() + ".atap.bak" }
func (e Env) dataDir() string      { return filepath.Join(e.Home, ".agent-tap") }
func (e Env) installedBinary() string {
	return filepath.Join(e.dataDir(), "bin", "atap")
}
func (e Env) statePath() string { return filepath.Join(e.dataDir(), "init-state.json") }
func (e Env) logPath() string   { return filepath.Join(e.dataDir(), "atap.log") }
func (e Env) plistPath() string {
	return filepath.Join(e.Home, "Library", "LaunchAgents", label+".plist")
}
func (e Env) service() string { return fmt.Sprintf("gui/%d/%s", e.UID, label) }
func (e Env) baseURL() string { return "http://127.0.0.1:" + strconv.Itoa(e.Port) }
func (e Env) hookURL() string { return e.baseURL() + "/api/hooks/claude" }

func (e Env) env() map[string]string {
	return map[string]string{
		envBaseURL:    e.baseURL(),
		envToolSearch: "true",
		envHints:      "1",
	}
}

// initState records what init changed, for uninit.
type initState struct {
	Env map[string]*string `json:"env"` // Values before init; null means absent
}

// Init installs the daemon and only then routes Claude Code through it:
// copy the binary, install the LaunchAgent, wait for /api/state, and write
// settings.json last. If the daemon never answers, the LaunchAgent is
// removed again and settings.json is left untouched.
func Init(e Env, out io.Writer) error {
	s, raw, mode, err := readSettings(e.settingsPath())
	if err != nil {
		return err
	}

	// A re-init replaces the running daemon. launchctl bootout returns
	// while launchd is still unloading the service, and bootstrapping it
	// again before the unload finishes fails ("Bootstrap failed: 5"), so wait
	// until launchd no longer knows the service, not just until the port
	// goes quiet.
	if _, err := os.Stat(e.plistPath()); err == nil {
		e.Launchctl("bootout", e.service())
		e.waitFor(func() bool { return !e.loaded() && !e.answering() })
	}
	if e.answering() {
		return fmt.Errorf("an atap daemon is already answering on port %d; stop it first (Ctrl-C, or atap uninit)", e.Port)
	}

	if err := e.copyBinary(); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}
	fmt.Fprintf(out, "copied %s -> %s\n", e.Binary, e.installedBinary())
	if err := writeFileAtomic(e.plistPath(), []byte(e.plist()), 0o644); err != nil {
		return fmt.Errorf("write LaunchAgent: %w", err)
	}
	if err := e.bootstrap(); err != nil {
		os.Remove(e.plistPath())
		return e.failed(s, err)
	}
	fmt.Fprintf(out, "installed LaunchAgent %s\n", e.plistPath())
	if !e.waitFor(e.answering) {
		e.Launchctl("bootout", e.service())
		os.Remove(e.plistPath())
		return e.failed(s, fmt.Errorf("the daemon did not answer on %s within %s, so the LaunchAgent was removed; see %s",
			e.baseURL(), e.HealthTimeout, e.logPath()))
	}
	fmt.Fprintf(out, "daemon answering on %s\n", e.baseURL())

	env := e.env()
	if !wired(s, env, e.hookURL()) {
		if raw != nil {
			if err := writeFileAtomic(e.backupPath(), raw, mode); err != nil {
				return fmt.Errorf("back up settings: %w", err)
			}
			fmt.Fprintf(out, "backed up %s -> %s\n", e.settingsPath(), e.backupPath())
		}
		state, _ := json.MarshalIndent(initState{Env: priorEnv(s, env)}, "", "  ")
		if err := writeFileAtomic(e.statePath(), state, 0o600); err != nil {
			return fmt.Errorf("record init state: %w", err)
		}
	}
	wire(s, env, e.hookURL())
	if err := writeSettings(e.settingsPath(), s, mode); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	fmt.Fprintf(out, "wired %s: %s, hooks %s\n", e.settingsPath(), strings.Join(sortedKeys(env), ", "), strings.Join(hookEvents, ", "))
	fmt.Fprintln(out, "New Claude Code sessions now route through atap; sessions already open keep their old settings.")
	return nil
}

// Uninit reverses Init in the safe order: settings first, so Claude Code
// stops depending on the daemon, then the LaunchAgent. Data in
// ~/.agent-tap is left in place.
func Uninit(e Env, out io.Writer) error {
	s, _, mode, err := readSettings(e.settingsPath())
	if err != nil {
		return err
	}
	if env := e.env(); wired(s, env, e.hookURL()) {
		var state initState
		if data, err := os.ReadFile(e.statePath()); err == nil {
			json.Unmarshal(data, &state)
		}
		unwire(s, env, state.Env, e.hookURL())
		if err := writeSettings(e.settingsPath(), s, mode); err != nil {
			return fmt.Errorf("write settings: %w", err)
		}
		os.Remove(e.statePath())
		fmt.Fprintf(out, "unwired %s\n", e.settingsPath())
	} else {
		fmt.Fprintf(out, "%s was not wired to atap\n", e.settingsPath())
	}
	if _, err := os.Stat(e.plistPath()); err == nil {
		e.Launchctl("bootout", e.service()) // Already stopped is fine.
		if err := os.Remove(e.plistPath()); err != nil {
			return err
		}
		fmt.Fprintf(out, "removed LaunchAgent %s\n", e.plistPath())
	}
	fmt.Fprintf(out, "left %s in place\n", e.dataDir())
	return nil
}

// Status reports whether settings route through atap, whether the
// LaunchAgent is installed, and what the daemon has seen. It warns loudly
// when settings point at a daemon that does not answer, because Claude Code
// cannot work then.
func Status(e Env, out io.Writer) error {
	s, _, _, err := readSettings(e.settingsPath())
	if err != nil {
		return err
	}
	isWired := wired(s, e.env(), e.hookURL())
	_, plistErr := os.Stat(e.plistPath())
	fmt.Fprintf(out, "settings:    %s (%s)\n", yesNo(isWired, "wired to atap", "not wired"), e.settingsPath())
	fmt.Fprintf(out, "LaunchAgent: %s\n", yesNo(plistErr == nil, "installed", "not installed"))

	snap, err := e.state()
	if err != nil {
		fmt.Fprintf(out, "daemon:      not answering on %s\n", e.baseURL())
		if isWired {
			fmt.Fprintf(out, "\nWARNING: settings route Claude Code through %s, but nothing answers there.\n", e.baseURL())
			fmt.Fprintln(out, "Claude Code requests will fail until the daemon is back (launchctl kickstart, or atap start) or atap uninit is run.")
		}
		return nil
	}
	var in, cw, cr, outTok int
	for _, sess := range snap.Sessions {
		in, cw, cr, outTok = in+sess.TotalInputTokens, cw+sess.TotalCacheWrite, cr+sess.TotalCacheRead, outTok+sess.TotalOutputTokens
	}
	fmt.Fprintf(out, "daemon:      answering on %s\n", e.baseURL())
	fmt.Fprintf(out, "sessions:    %d live\n", len(snap.Sessions))
	fmt.Fprintf(out, "tokens:      %d (uncached %d, cache write %d, cache read %d, output %d)\n", in+cw+cr+outTok, in, cw, cr, outTok)
	if total := in + cw + cr; total > 0 {
		fmt.Fprintf(out, "cache hits:  %.1f%% of input\n", 100*float64(cr)/float64(total))
	}
	if snap.PlanUsage != nil {
		for _, w := range snap.PlanUsage.Windows {
			fmt.Fprintf(out, "plan usage:  %s %.0f%%%s\n", w.Name, 100*w.Utilization, resets(w.ResetsAt))
		}
	}
	return nil
}

// loaded reports whether launchd still knows the service.
func (e Env) loaded() bool { return e.Launchctl("print", e.service()) == nil }

// bootstrap loads the LaunchAgent, retrying while launchd finishes an
// earlier unload of the same service.
func (e Env) bootstrap() error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = e.Launchctl("bootstrap", fmt.Sprintf("gui/%d", e.UID), e.plistPath()); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	return err
}

// failed describes an install failure by what it means for Claude Code: on a
// fresh init settings were never touched, but on a re-init they already route
// through the daemon that just failed to come back.
func (e Env) failed(s settings, err error) error {
	if wired(s, e.env(), e.hookURL()) {
		return fmt.Errorf("%w\nWARNING: %s still routes Claude Code through %s, which is not running; run atap init again, or atap uninit to restore direct access",
			err, e.settingsPath(), e.baseURL())
	}
	return fmt.Errorf("%w; %s was left untouched", err, e.settingsPath())
}

// stateSnapshot is the part of /api/state that status prints.
type stateSnapshot struct {
	Sessions []struct {
		TotalInputTokens  int `json:"total_input_tokens"`
		TotalOutputTokens int `json:"total_output_tokens"`
		TotalCacheRead    int `json:"total_cache_read"`
		TotalCacheWrite   int `json:"total_cache_write"`
	} `json:"sessions"`
	PlanUsage *struct {
		Windows []struct {
			Name        string     `json:"name"`
			Utilization float64    `json:"utilization"`
			ResetsAt    *time.Time `json:"resets_at"`
		} `json:"windows"`
	} `json:"plan_usage"`
}

func (e Env) state() (*stateSnapshot, error) {
	resp, err := e.Client.Get(e.baseURL() + "/api/state")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/api/state: %s", resp.Status)
	}
	var snap stateSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// answering reports whether an atap daemon serves /api/state on the port.
func (e Env) answering() bool {
	_, err := e.state()
	return err == nil
}

// waitFor polls cond until it holds or HealthTimeout passes.
func (e Env) waitFor(cond func() bool) bool {
	deadline := time.Now().Add(e.HealthTimeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// copyBinary installs the running executable via a temporary file and
// rename: overwriting a running signed binary in place can get the old
// daemon killed on macOS.
func (e Env) copyBinary() error {
	src, err := os.Open(e.Binary)
	if err != nil {
		return err
	}
	defer src.Close()
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.dataDir(), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(e.installedBinary(), data, 0o755)
}

func (e Env) plist() string {
	esc := func(s string) string {
		var b strings.Builder
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + esc(e.installedBinary()) + `</string>
		<string>start</string>
		<string>--port</string>
		<string>` + strconv.Itoa(e.Port) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>` + esc(e.logPath()) + `</string>
	<key>StandardErrorPath</key>
	<string>` + esc(e.logPath()) + `</string>
</dict>
</plist>
`
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func yesNo(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

func resets(t *time.Time) string {
	if t == nil {
		return ""
	}
	return ", resets " + t.Local().Format("Jan 2 15:04")
}
