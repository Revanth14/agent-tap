package install

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// modFS is the Claude Code mod that shows the daemon's status line. Its tests
// stay in the repository.
//
//go:embed mod/.claude-plugin/plugin.json mod/hooks
var modFS embed.FS

// modDaemonFile is the mod's one generated file: it names the daemon's URL.
const modDaemonFile = "hooks/daemon.ts"

func (e Env) modDir() string { return filepath.Join(e.dataDir(), "mod") }

// installMod replaces the mod folder with the embedded mod, pointed at this
// daemon's port.
func (e Env) installMod() error {
	if err := os.RemoveAll(e.modDir()); err != nil {
		return err
	}
	return fs.WalkDir(modFS, "mod", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel("mod", filepath.FromSlash(p))
		data, err := modFS.ReadFile(p)
		if err != nil {
			return err
		}
		if filepath.ToSlash(rel) == modDaemonFile {
			data = fmt.Appendf(nil, "// The daemon's base URL. atap init rewrites this file with its own port.\nexport const DAEMON = '%s'\n", e.baseURL())
		}
		return writeFileAtomic(filepath.Join(e.modDir(), rel), data, 0o644)
	})
}
