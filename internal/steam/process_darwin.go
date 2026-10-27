// Steam client control on macOS, through the Steam app

package steam

import (
	"context"
	"os"
	"os/exec"
	"strconv"
)

// Check whether this user's Steam app is running, whose main process is steam_osx
func (Process) Running() bool {
	return exec.Command("pgrep", "-x", "-u", strconv.Itoa(os.Getuid()), "steam_osx").Run() == nil
}

// macOS has no console-like Steam session
func (Process) GameMode() bool { return false }

// Ask the running Steam app to exit through its own URL handler
func (Process) shutdownCommand(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, "open", "steam://exit")
}

// Open the Steam app, which goes on running on its own
func (Process) Start() error {
	return exec.Command("open", "-a", "Steam").Run()
}
