// Steam's data folders on Linux, for the native and the Flatpak client

package steam

import "path/filepath"

// Places Steam keeps its data on Linux, the native client's default first, then the Flatpak client's
func dirs(home string) []string {
	flatpak := filepath.Join(home, ".var/app", FlatpakID)
	return []string{
		filepath.Join(home, ".local/share/Steam"),
		filepath.Join(home, ".steam/steam"),
		filepath.Join(flatpak, ".local/share/Steam"),
		filepath.Join(flatpak, "data/Steam"),
	}
}
