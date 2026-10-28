// Where the games, steam-adopt's own files and Steam's data are

package app

import (
	"path/filepath"

	"github.com/ddrimus/steam-adopt/internal/steam"
)

// Locations of the games, steam-adopt's own files and Steam's data
type Paths struct {
	// One subfolder per game
	Games string
	// steam-adopt's own folder inside Games
	Sync   string
	Config string
	// What the last run wanted and left in Steam, to tell edits made inside Steam apart
	State string
	// One artwork folder per game
	Art string
	// Steam's data folder
	Steam string
}

// Build the standard locations under a home folder, the games in ~/.local/share/games on Linux, ~/Games elsewhere
func DefaultPaths(home string) Paths {
	if goos != "linux" {
		return NewPaths(home, filepath.Join(home, "Games"))
	}
	return NewPaths(home, filepath.Join(home, ".local/share/games"))
}

// Build the locations for a games folder of the user's choosing, steam-adopt's own folder inside it
func NewPaths(home, games string) Paths {

	// The leading underscore keeps it listed above the game folders
	sync := filepath.Join(games, "_steam-adopt")
	return Paths{
		Games:  games,
		Sync:   sync,
		Config: filepath.Join(sync, "steam-adopt.conf"),
		State:  filepath.Join(sync, "state.json"),
		Art:    filepath.Join(sync, "artwork"),
		Steam:  steam.FindDir(home),
	}
}

// Check whether dir is steam-adopt's own folder
func (p Paths) isSyncDir(dir string) bool { return dir == p.Sync }

// Check whether path sits in steam-adopt's own folder
func (p Paths) inSyncDir(path string) bool { return inside(path, p.Sync) }
