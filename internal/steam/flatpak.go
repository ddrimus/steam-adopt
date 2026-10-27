// Flatpak Steam's sandbox, and whether its filesystem overrides let it see a folder

package steam

import (
	"os"
	"path/filepath"
	"strings"
)

// Override files in the order Flatpak applies them, later ones winning
func overrideFiles(home string) []string {
	var out []string
	for _, dir := range []string{"/var/lib/flatpak/overrides", filepath.Join(home, ".local/share/flatpak/overrides")} {
		out = append(out, filepath.Join(dir, "global"), filepath.Join(dir, FlatpakID))
	}
	return out
}

// Check whether the Flatpak client at steamDir can read path, judged from its filesystem overrides
func FlatpakCanSee(steamDir, path string) bool {
	i := strings.Index(filepath.ToSlash(steamDir), "/.var/app/")
	if i < 0 {
		return true
	}
	return canSee(steamDir[:i], path, overrideFiles(steamDir[:i]))
}

// Check whether the filesystems granted in the override files cover path
func canSee(home, path string, files []string) bool {
	granted := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			v, ok := strings.CutPrefix(strings.TrimSpace(line), "filesystems=")
			if !ok {
				continue
			}
			for _, e := range strings.Split(v, ";") {
				neg := strings.HasPrefix(e, "!")
				e = strings.TrimPrefix(e, "!")
				if j := strings.LastIndex(e, ":"); j > 0 {
					e = e[:j]
				}
				if dir := grantedDir(home, e); dir != "" {
					granted[dir] = !neg
				}
			}
		}
	}
	path = filepath.Clean(path)
	for dir, ok := range granted {
		if ok && (path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")) {
			return true
		}
	}
	return false
}

// Turn one filesystems entry into the folder it grants, "" for entries that grant no folder
func grantedDir(home, e string) string {
	switch {
	// host-os only grants the system folders, not home
	case e == "host":
		return "/"
	case e == "home" || e == "~":
		return home
	case strings.HasPrefix(e, "~/"):
		return filepath.Join(home, e[2:])
	case e == "xdg-data" || strings.HasPrefix(e, "xdg-data/"):
		return filepath.Join(home, ".local/share", strings.TrimPrefix(strings.TrimPrefix(e, "xdg-data"), "/"))
	case strings.HasPrefix(e, "/"):
		return filepath.Clean(e)
	}
	return ""
}
