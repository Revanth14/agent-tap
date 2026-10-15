package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	envBaseURL    = "ANTHROPIC_BASE_URL"
	envToolSearch = "ENABLE_TOOL_SEARCH"
	envHints      = "CLAUDE_CODE_GATEWAY_HINT_HEADERS"
	envPluginDirs = "CLAUDE_CODE_PLUGIN_DIRS"
)

// hookEvents are the hooks the recorder consumes; nothing else is registered.
var hookEvents = []string{"PreToolUse", "PostToolUse", "SubagentStop"}

// settings is ~/.claude/settings.json, kept generic so unknown keys survive.
type settings map[string]any

// readSettings parses the settings file, returning its raw bytes (nil when
// the file does not exist yet) and its mode.
func readSettings(path string) (settings, []byte, fs.FileMode, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return settings{}, nil, 0o600, nil
	}
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, 0, err
	}
	s := settings{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, nil, 0, fmt.Errorf("%s is not valid JSON, so it was left untouched: %w", path, err)
		}
	}
	return s, raw, info.Mode().Perm(), nil
}

// writeFileAtomic replaces path via a temporary file and rename, so a crash
// or a concurrent reader never sees a partial file.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func writeSettings(path string, s settings, mode fs.FileMode) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), mode)
}

// child returns s[key] as an object, creating it if absent.
func child(s map[string]any, key string) map[string]any {
	if m, ok := s[key].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	s[key] = m
	return m
}

// wired reports whether s already routes Claude Code through this daemon.
func wired(s settings, env map[string]string, hookURL string) bool {
	if e, ok := s["env"].(map[string]any); ok && e[envBaseURL] == env[envBaseURL] {
		return true
	}
	hooks, _ := s["hooks"].(map[string]any)
	for _, groups := range hooks {
		if hasHookURL(groups, hookURL) {
			return true
		}
	}
	return false
}

// wire sets the env keys and adds this daemon's hooks, once per event.
func wire(s settings, env map[string]string, hookURL string) {
	e := child(s, "env")
	for k, v := range env {
		e[k] = v
	}
	hooks := child(s, "hooks")
	for _, ev := range hookEvents {
		groups, _ := hooks[ev].([]any)
		if hasHookURL(groups, hookURL) {
			continue
		}
		group := map[string]any{"hooks": []any{map[string]any{"type": "http", "url": hookURL, "timeout": 1}}}
		if ev != "SubagentStop" {
			group["matcher"] = "*"
		}
		hooks[ev] = append(groups, group)
	}
}

// unwire removes what wire added. An env key goes back to its value from
// before init (prior; nil means it was absent), but only if it still holds
// the value init set: anything changed since is left alone. Only hook
// entries pointing at hookURL are removed.
func unwire(s settings, env map[string]string, prior map[string]*string, hookURL string) {
	if e, ok := s["env"].(map[string]any); ok {
		for k, v := range env {
			if e[k] != v {
				continue
			}
			if p := prior[k]; p != nil {
				e[k] = *p
			} else {
				delete(e, k)
			}
		}
		if len(e) == 0 {
			delete(s, "env")
		}
	}
	hooks, ok := s["hooks"].(map[string]any)
	if !ok {
		return
	}
	for ev, v := range hooks {
		groups, ok := v.([]any)
		if !ok {
			continue
		}
		var kept []any
		for _, g := range groups {
			gm, ok := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			if !ok || len(hs) == 0 {
				kept = append(kept, g)
				continue
			}
			var keptHooks []any
			for _, h := range hs {
				if hm, ok := h.(map[string]any); ok && hm["url"] == hookURL {
					continue
				}
				keptHooks = append(keptHooks, h)
			}
			if len(keptHooks) > 0 {
				gm["hooks"] = keptHooks
				kept = append(kept, gm)
			}
		}
		if len(kept) == 0 {
			delete(hooks, ev)
		} else {
			hooks[ev] = kept
		}
	}
	if len(hooks) == 0 {
		delete(s, "hooks")
	}
}

func hasHookURL(groups any, hookURL string) bool {
	gs, _ := groups.([]any)
	for _, g := range gs {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		for _, h := range hs {
			if hm, ok := h.(map[string]any); ok && hm["url"] == hookURL {
				return true
			}
		}
	}
	return false
}

// addPluginDir appends dir to the CLAUDE_CODE_PLUGIN_DIRS path list, keeping
// any folders already there.
func addPluginDir(s settings, dir string) {
	e := child(s, "env")
	cur, _ := e[envPluginDirs].(string)
	dirs := filepath.SplitList(cur)
	if slices.Contains(dirs, dir) {
		return
	}
	e[envPluginDirs] = strings.Join(append(dirs, dir), string(os.PathListSeparator))
}

// removePluginDir removes dir, and only dir, from the CLAUDE_CODE_PLUGIN_DIRS
// path list, dropping the key once the list is empty. It reports whether it
// changed s.
func removePluginDir(s settings, dir string) bool {
	e, _ := s["env"].(map[string]any)
	cur, ok := e[envPluginDirs].(string)
	if !ok {
		return false
	}
	dirs := filepath.SplitList(cur)
	kept := slices.DeleteFunc(slices.Clone(dirs), func(d string) bool { return d == dir })
	if len(kept) == len(dirs) {
		return false
	}
	if len(kept) == 0 {
		delete(e, envPluginDirs)
	} else {
		e[envPluginDirs] = strings.Join(kept, string(os.PathListSeparator))
	}
	if len(e) == 0 {
		delete(s, "env")
	}
	return true
}

// priorEnv captures the env keys' values before init; nil means absent.
func priorEnv(s settings, env map[string]string) map[string]*string {
	prior := make(map[string]*string, len(env))
	e, _ := s["env"].(map[string]any)
	for k := range env {
		if v, ok := e[k].(string); ok {
			prior[k] = &v
		} else {
			prior[k] = nil
		}
	}
	return prior
}
