// Steam client control on Windows, through tasklist and steam.exe

package steam

import (
	"context"
	"os/exec"
	"path/filepath"
)

// Path of steam.exe in Steam's folder, which on Windows holds both the program and its data
func (p Process) exe() string { return filepath.Join(p.Dir, "steam.exe") }

// Check whether steam.exe is running, as listed by tasklist
func (Process) Running() bool {
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq steam.exe", "/FO", "CSV", "/NH").Output()
	return err == nil && listsSteam(string(out))
}

// Windows has no console-like Steam session
func (Process) GameMode() bool { return false }

// Run steam.exe -shutdown
func (p Process) shutdownCommand(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, p.exe(), "-shutdown")
}

// Launch steam.exe, which goes on running after steam-adopt exits
func (p Process) Start() error {
	cmd := exec.Command(p.exe())
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
