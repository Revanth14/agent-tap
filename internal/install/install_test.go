package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDaemon stands in for atap behind launchctl: bootstrap starts it on the
// reserved port, bootout stops it.
type fakeDaemon struct {
	t       *testing.T
	addr    string
	starts  bool // Whether bootstrap brings the daemon up
	mu      sync.Mutex
	srv     *http.Server
	calls   []string
	onCall  func(args []string) // Observes each launchctl call
	payload string
	// Real launchd finishes a bootout asynchronously: the service stays
	// loaded for a few more print calls, and bootstrap fails meanwhile.
	unloadLag int
	loaded    bool
	unloading int
}

func (d *fakeDaemon) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.srv != nil {
		return
	}
	ln, err := net.Listen("tcp", d.addr)
	if err != nil {
		d.t.Fatalf("fake daemon listen: %v", err)
	}
	d.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/state" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, d.payload)
	})}
	go d.srv.Serve(ln)
}

func (d *fakeDaemon) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.srv != nil {
		d.srv.Close()
		d.srv = nil
	}
}

func (d *fakeDaemon) launchctl(args ...string) error {
	if args[0] == "print" {
		if d.unloading > 0 {
			d.unloading--
			if d.unloading == 0 {
				d.loaded = false
			}
		}
		if !d.loaded {
			return errors.New("Could not find service")
		}
		return nil
	}
	d.calls = append(d.calls, args[0])
	if d.onCall != nil {
		d.onCall(args)
	}
	switch args[0] {
	case "bootstrap":
		if d.loaded {
			return errors.New("Bootstrap failed: 5: Input/output error")
		}
		d.loaded = true
		if d.starts {
			d.start()
		}
	case "bootout":
		d.stop()
		if d.unloadLag == 0 {
			d.loaded = false
		} else {
			d.unloading = d.unloadLag
		}
	}
	return nil
}

// newEnv returns an Env over a temporary home with a fake binary, and the
// fake daemon behind its launchctl.
func newEnv(t *testing.T, healthTimeout time.Duration) (Env, *fakeDaemon) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // Free: nothing answers until bootstrap.

	d := &fakeDaemon{t: t, addr: ln.Addr().String(), starts: true, payload: `{"sessions":[],"plan_usage":null}`}
	t.Cleanup(d.stop)
	home := t.TempDir()
	bin := filepath.Join(t.TempDir(), "atap")
	if err := os.WriteFile(bin, []byte("atap-binary-v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Env{
		Home: home, Binary: bin, Port: port, UID: 501,
		Launchctl:     d.launchctl,
		Client:        &http.Client{Timeout: 500 * time.Millisecond},
		HealthTimeout: healthTimeout,
	}, d
}

const userSettings = `{
  "env": {"FOO": "bar", "CLAUDE_CODE_GATEWAY_HINT_HEADERS": "0"},
  "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/guard"}]}]},
  "permissions": {"allow": ["Read"]}
}
`

func writeUserSettings(t *testing.T, e Env, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.settingsPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.settingsPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadSettings(t *testing.T, e Env) settings {
	t.Helper()
	s, _, _, err := readSettings(e.settingsPath())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ourHooks counts hook entries pointing at the daemon, per event.
func ourHooks(s settings, url string) map[string]int {
	out := map[string]int{}
	hooks, _ := s["hooks"].(map[string]any)
	for ev, groups := range hooks {
		gs, _ := groups.([]any)
		for _, g := range gs {
			hs, _ := g.(map[string]any)["hooks"].([]any)
			for _, h := range hs {
				if h.(map[string]any)["url"] == url {
					out[ev]++
				}
			}
		}
	}
	return out
}

func TestInitInstallsThenWires(t *testing.T) {
	e, d := newEnv(t, 3*time.Second)
	writeUserSettings(t, e, userSettings)
	var out bytes.Buffer
	if err := Init(e, &out); err != nil {
		t.Fatalf("Init: %v\n%s", err, out.String())
	}

	if data, err := os.ReadFile(e.installedBinary()); err != nil || string(data) != "atap-binary-v1" {
		t.Errorf("installed binary = %q, %v", data, err)
	}
	if info, _ := os.Stat(e.installedBinary()); info == nil || info.Mode().Perm() != 0o755 {
		t.Errorf("installed binary mode = %v, want 0755", info)
	}
	plist, _ := os.ReadFile(e.plistPath())
	for _, want := range []string{e.installedBinary(), "<string>start</string>", "<string>" + itoa(e.Port) + "</string>", "<key>KeepAlive</key>"} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if !slices.Equal(d.calls, []string{"bootstrap"}) {
		t.Errorf("launchctl calls = %v, want [bootstrap]", d.calls)
	}

	s := loadSettings(t, e)
	env := s["env"].(map[string]any)
	if env["FOO"] != "bar" || env[envBaseURL] != e.baseURL() || env[envToolSearch] != "true" || env[envHints] != "1" {
		t.Errorf("env = %v", env)
	}
	if got := ourHooks(s, e.hookURL()); len(got) != 3 || got["PreToolUse"] != 1 || got["PostToolUse"] != 1 || got["SubagentStop"] != 1 {
		t.Errorf("our hooks = %v, want one each on PreToolUse, PostToolUse, SubagentStop", got)
	}
	if groups := s["hooks"].(map[string]any)["PreToolUse"].([]any); len(groups) != 2 {
		t.Errorf("PreToolUse groups = %v, want the user's Bash guard kept beside ours", groups)
	}
	if s["permissions"] == nil {
		t.Error("permissions dropped")
	}
	if info, _ := os.Stat(e.settingsPath()); info.Mode().Perm() != 0o600 {
		t.Errorf("settings mode = %v, want 0600 kept", info.Mode().Perm())
	}
	if backup, _ := os.ReadFile(e.backupPath()); string(backup) != userSettings {
		t.Errorf("backup = %q, want the original bytes", backup)
	}

	if env[envPluginDirs] != e.modDir() {
		t.Errorf("%s = %v, want the mod folder %s", envPluginDirs, env[envPluginDirs], e.modDir())
	}
	for _, f := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.ts", "hooks/status.ts"} {
		if _, err := os.Stat(filepath.Join(e.modDir(), f)); err != nil {
			t.Errorf("mod file %s: %v", f, err)
		}
	}
	if daemon, _ := os.ReadFile(filepath.Join(e.modDir(), modDaemonFile)); !strings.Contains(string(daemon), "'"+e.baseURL()+"'") {
		t.Errorf("%s = %q, want this daemon's URL %s", modDaemonFile, daemon, e.baseURL())
	}
	if _, err := os.Stat(filepath.Join(e.modDir(), "tests")); !os.IsNotExist(err) {
		t.Errorf("mod tests were installed: %v", err)
	}
}

// Init adds its mod to the user's own plugin folders, once, and uninit takes
// out only its own.
func TestPluginDirsKeepTheUsersFolders(t *testing.T) {
	e, _ := newEnv(t, 3*time.Second)
	writeUserSettings(t, e, `{"env": {"CLAUDE_CODE_PLUGIN_DIRS": "/Users/me/mods:/opt/mods"}}`)
	for range 2 {
		if err := Init(e, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if got := loadSettings(t, e)["env"].(map[string]any)[envPluginDirs]; got != "/Users/me/mods:/opt/mods:"+e.modDir() {
		t.Errorf("after two inits %s = %v, want the user's folders, then the mod's once", envPluginDirs, got)
	}
	if err := Uninit(e, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := loadSettings(t, e)["env"].(map[string]any)[envPluginDirs]; got != "/Users/me/mods:/opt/mods" {
		t.Errorf("after uninit %s = %v, want only the user's folders", envPluginDirs, got)
	}
}

func TestInitRollsBackWhenTheDaemonNeverAnswers(t *testing.T) {
	e, d := newEnv(t, 300*time.Millisecond)
	d.starts = false
	writeUserSettings(t, e, userSettings)
	if err := Init(e, io.Discard); err == nil || !strings.Contains(err.Error(), "left untouched") {
		t.Fatalf("Init = %v, want an error saying settings were left untouched", err)
	}
	if _, err := os.Stat(e.plistPath()); !os.IsNotExist(err) {
		t.Error("LaunchAgent plist left behind")
	}
	if !slices.Equal(d.calls, []string{"bootstrap", "bootout"}) {
		t.Errorf("launchctl calls = %v, want the LaunchAgent booted out again", d.calls)
	}
	if data, _ := os.ReadFile(e.settingsPath()); string(data) != userSettings {
		t.Errorf("settings changed:\n%s", data)
	}
	if _, err := os.Stat(e.backupPath()); !os.IsNotExist(err) {
		t.Error("backup written although settings were never touched")
	}
}

func TestInitRefusesBeforeInstallingAnything(t *testing.T) {
	t.Run("invalid settings", func(t *testing.T) {
		e, d := newEnv(t, time.Second)
		writeUserSettings(t, e, "{not json")
		if err := Init(e, io.Discard); err == nil {
			t.Fatal("Init succeeded with invalid settings.json")
		}
		if len(d.calls) != 0 {
			t.Errorf("launchctl calls = %v, want none", d.calls)
		}
		if _, err := os.Stat(e.installedBinary()); !os.IsNotExist(err) {
			t.Error("binary installed despite invalid settings")
		}
	})
	t.Run("atap already answering", func(t *testing.T) {
		e, d := newEnv(t, time.Second)
		d.start() // e.g. a manual `atap start` on the same port
		if err := Init(e, io.Discard); err == nil || !strings.Contains(err.Error(), "already answering") {
			t.Fatalf("Init = %v, want refusal", err)
		}
		if len(d.calls) != 0 {
			t.Errorf("launchctl calls = %v, want none", d.calls)
		}
	})
}

func TestInitTwiceIsIdempotent(t *testing.T) {
	e, d := newEnv(t, 3*time.Second)
	writeUserSettings(t, e, userSettings)
	for i := 0; i < 2; i++ {
		if err := Init(e, io.Discard); err != nil {
			t.Fatalf("Init #%d: %v", i+1, err)
		}
	}
	if !slices.Equal(d.calls, []string{"bootstrap", "bootout", "bootstrap"}) {
		t.Errorf("launchctl calls = %v, want the old daemon booted out before the new one", d.calls)
	}
	if got := ourHooks(loadSettings(t, e), e.hookURL()); got["PreToolUse"] != 1 || got["PostToolUse"] != 1 || got["SubagentStop"] != 1 {
		t.Errorf("our hooks after two inits = %v, want one each", got)
	}
	if backup, _ := os.ReadFile(e.backupPath()); string(backup) != userSettings {
		t.Error("second init overwrote the backup with already-wired settings")
	}
}

// TestReinitWaitsForLaunchdToUnload reproduces "Bootstrap failed: 5": the
// old service is still being unloaded when the new one is bootstrapped.
func TestReinitWaitsForLaunchdToUnload(t *testing.T) {
	e, d := newEnv(t, 3*time.Second)
	writeUserSettings(t, e, userSettings)
	if err := Init(e, io.Discard); err != nil {
		t.Fatal(err)
	}
	d.unloadLag = 3
	if err := Init(e, io.Discard); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if !e.answering() {
		t.Error("daemon not answering after re-init")
	}
}

func TestReinitFailureWarnsThatSettingsStillRouteThroughTheDaemon(t *testing.T) {
	e, d := newEnv(t, 300*time.Millisecond)
	if err := Init(e, io.Discard); err != nil {
		t.Fatal(err)
	}
	d.starts = false // The new daemon never comes up.
	err := Init(e, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "WARNING") || !strings.Contains(err.Error(), "atap uninit") {
		t.Fatalf("re-init failure = %v, want a warning that Claude Code still routes through the stopped daemon", err)
	}
}

func TestUninitRestoresSettingsBeforeRemovingTheDaemon(t *testing.T) {
	e, d := newEnv(t, 3*time.Second)
	writeUserSettings(t, e, userSettings)
	if err := Init(e, io.Discard); err != nil {
		t.Fatal(err)
	}
	// After init, the user turns tool search off themselves.
	s := loadSettings(t, e)
	s["env"].(map[string]any)[envToolSearch] = "false"
	if err := writeSettings(e.settingsPath(), s, 0o600); err != nil {
		t.Fatal(err)
	}

	wiredAtBootout := true
	d.onCall = func(args []string) {
		if args[0] == "bootout" {
			wiredAtBootout = wired(loadSettings(t, e), e.env(), e.hookURL())
		}
	}
	if err := Uninit(e, io.Discard); err != nil {
		t.Fatal(err)
	}
	if wiredAtBootout {
		t.Error("the daemon was stopped while settings still routed Claude Code through it")
	}

	s = loadSettings(t, e)
	env := s["env"].(map[string]any)
	if _, ok := env[envBaseURL]; ok {
		t.Errorf("%s still set: %v", envBaseURL, env)
	}
	if env[envHints] != "0" || env[envToolSearch] != "false" || env["FOO"] != "bar" {
		t.Errorf("env = %v, want the hint flag restored to its prior 0, the user's later tool-search change kept, FOO kept", env)
	}
	if got := ourHooks(s, e.hookURL()); len(got) != 0 {
		t.Errorf("our hooks left = %v", got)
	}
	hooks, _ := s["hooks"].(map[string]any)
	if groups, _ := hooks["PreToolUse"].([]any); len(hooks) != 1 || len(groups) != 1 {
		t.Errorf("hooks = %v, want only the user's Bash guard", hooks)
	}
	if _, err := os.Stat(e.plistPath()); !os.IsNotExist(err) {
		t.Error("LaunchAgent plist left behind")
	}
	if _, err := os.Stat(e.installedBinary()); err != nil {
		t.Errorf("~/.agent-tap data removed: %v", err)
	}
}

func TestUninitWithoutPriorSettingsRemovesEverythingItAdded(t *testing.T) {
	e, _ := newEnv(t, 3*time.Second)
	if err := Init(e, io.Discard); err != nil { // No settings.json before init.
		t.Fatal(err)
	}
	if err := Uninit(e, io.Discard); err != nil {
		t.Fatal(err)
	}
	if s := loadSettings(t, e); len(s) != 0 {
		t.Errorf("settings after init+uninit = %v, want empty", s)
	}
}

func TestStatus(t *testing.T) {
	e, d := newEnv(t, 3*time.Second)
	if err := Init(e, io.Discard); err != nil {
		t.Fatal(err)
	}
	d.payload = `{"sessions":[{"total_input_tokens":100,"total_cache_write":400,"total_cache_read":1500,"total_output_tokens":50}],` +
		`"plan_usage":{"windows":[{"name":"5h","utilization":0.17,"resets_at":"2026-10-15T00:40:00Z"}]}}`
	var out bytes.Buffer
	Status(e, &out)
	for _, want := range []string{"wired to atap", "LaunchAgent: installed", "answering", "1 live", "tokens:      2050", "cache hits:  75.0%", "5h 17%", "status line: mod in " + envPluginDirs} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}

	d.stop()
	out.Reset()
	Status(e, &out)
	if !strings.Contains(out.String(), "WARNING") || !strings.Contains(out.String(), "atap uninit") {
		t.Errorf("status with settings wired and no daemon must warn:\n%s", out.String())
	}

}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestPlistPassesPlutil(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not available")
	}
	e, _ := newEnv(t, time.Second)
	e.Home = filepath.Join(t.TempDir(), "home with spaces & <odd> chars")
	path := filepath.Join(t.TempDir(), "agent.plist")
	if err := os.WriteFile(path, []byte(e.plist()), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
		t.Errorf("plutil -lint: %v\n%s\n%s", err, out, e.plist())
	}
}
