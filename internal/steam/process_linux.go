// Steam client control on Linux, through /proc and the steam or flatpak command

package steam

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Build the command that runs the Steam client with args
func (p Process) command(ctx context.Context, args ...string) *exec.Cmd {
	if p.Flatpak {
		return exec.CommandContext(ctx, find("flatpak", "/usr/local/bin/flatpak"),
			append([]string{"run", FlatpakID}, args...)...)
	}
	var last []string
	if p.Dir != "" {
		last = []string{filepath.Join(p.Dir, "steam.sh")}
	}
	return exec.CommandContext(ctx, find("steam", append([]string{"/usr/games/steam", "/usr/local/bin/steam"}, last...)...), args...)
}

// Run steam -shutdown
func (p Process) shutdownCommand(ctx context.Context) *exec.Cmd {
	return p.command(ctx, "-shutdown")
}

// Only count this user's client named exactly steam, read from /proc, which also shows the Flatpak client
func (Process) Running() bool {
	return running("/proc", "steam", uint32(os.Getuid()))
}

// Check whether a process of uid with exactly this name is in proc
func running(proc, name string, uid uint32) bool {
	found := false
	each(proc, uid, func(_, comm string) { found = found || comm == name })
	return found
}

// Game Mode runs Steam with -gamepadui inside gamescope, Big Picture on a desktop has no gamescope
func (Process) GameMode() bool {
	return gameMode("/proc", uint32(os.Getuid()))
}

// Check whether proc holds uid's steam started with -gamepadui and a gamescope process
func gameMode(proc string, uid uint32) bool {
	gamepadUI, gamescope := false, false
	each(proc, uid, func(dir, comm string) {
		if strings.HasPrefix(comm, "gamescope") {
			gamescope = true
		}
		if comm == "steam" {
			cmdline, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
			for _, arg := range strings.Split(string(cmdline), "\x00") {
				gamepadUI = gamepadUI || arg == "-gamepadui"
			}
		}
	})
	return gamepadUI && gamescope
}

// Call fn with the folder and name of every process of uid in proc
func each(proc string, uid uint32, fn func(dir, comm string)) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return
	}
	for _, e := range entries {
		if n := e.Name(); n[0] < '0' || n[0] > '9' {
			continue
		}
		dir := filepath.Join(proc, e.Name())
		if st, err := os.Stat(dir); err != nil {
			continue
		} else if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Uid != uid {
			continue
		}
		if comm, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
			fn(dir, strings.TrimSuffix(string(comm), "\n"))
		}
	}
}

// Launch steam in its own session, so it outlives steam-adopt
func (p Process) Start() error {
	cmd := p.command(context.Background())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Find a command on PATH, else at the first usual place, since cron's PATH leaves out /usr/games and /usr/local/bin
func find(name string, places ...string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, p := range places {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return name
}
