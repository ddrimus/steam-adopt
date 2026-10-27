// Steam client control shared by every system: the interface and shutdown

package steam

import (
	"context"
	"strings"
	"time"
)

// Checks on, stops and starts the Steam client
type Controller interface {
	// Check whether the Steam client is running
	Running() bool
	// Check whether Steam runs in a session like the Steam Deck's Game Mode, which starts it again once it exits
	GameMode() bool
	// Ask Steam to exit, returning before it has actually closed
	Shutdown(ctx context.Context) error
	// Launch Steam detached from this process
	Start() error
}

// Real Steam client, through steam or flatpak on Linux, steam.exe on Windows and the Steam app on macOS
type Process struct {
	Flatpak bool
	// Steam's data folder, holding steam.sh on Linux and steam.exe on Windows
	Dir string
}

// Ask Steam to exit, giving up on the command after 30 seconds
func (p Process) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return p.shutdownCommand(ctx).Run()
}

// Check whether tasklist's CSV output on Windows has a steam.exe row, its message for no match being plain text
func listsSteam(out string) bool {
	return strings.Contains(strings.ToLower(out), `"steam.exe"`)
}
