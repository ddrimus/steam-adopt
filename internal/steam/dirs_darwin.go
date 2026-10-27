// Steam's data folder on macOS

package steam

import "path/filepath"

// Place the Steam app keeps its data on macOS
func dirs(home string) []string {
	return []string{filepath.Join(home, "Library/Application Support/Steam")}
}
