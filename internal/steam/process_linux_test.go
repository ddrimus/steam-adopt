package steam

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Test that only the main Steam process counts as running
func TestRunning(t *testing.T) {
	proc := t.TempDir()
	for pid, comm := range map[string]string{"1": "systemd\n", "42": "steamwebhelper\n", "self": "steam\n"} {
		if err := os.MkdirAll(filepath.Join(proc, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proc, pid, "comm"), []byte(comm), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	me := uint32(os.Getuid())
	if running(proc, "steam", me) {
		t.Error("helpers and non-numeric entries must not count as Steam")
	}
	os.MkdirAll(filepath.Join(proc, "77"), 0o755)
	os.WriteFile(filepath.Join(proc, "77", "comm"), []byte("steam\n"), 0o644)
	if !running(proc, "steam", me) {
		t.Error("the steam process wasn't found")
	}

	// Another user's Steam can't be closed from here, so it doesn't count
	if running(proc, "steam", me+1) {
		t.Error("another user's Steam counted as running")
	}
}

// Test that Game Mode needs both Steam's -gamepadui and a gamescope, so Big Picture on a desktop doesn't count
func TestGameMode(t *testing.T) {
	proc := t.TempDir()
	add := func(pid, comm, cmdline string) {
		os.MkdirAll(filepath.Join(proc, pid), 0o755)
		os.WriteFile(filepath.Join(proc, pid, "comm"), []byte(comm+"\n"), 0o644)
		os.WriteFile(filepath.Join(proc, pid, "cmdline"), []byte(cmdline), 0o644)
	}
	me := uint32(os.Getuid())
	add("10", "steam", "/home/deck/.local/share/Steam/ubuntu12_32/steam\x00-steamos3\x00-gamepadui\x00")
	if gameMode(proc, me) {
		t.Error("Big Picture without gamescope taken for Game Mode")
	}
	add("5", "gamescope-wl", "gamescope\x00--steam\x00")
	if !gameMode(proc, me) {
		t.Error("Game Mode not detected")
	}
	add("10", "steam", "/usr/bin/steam\x00-silent\x00")
	if gameMode(proc, me) {
		t.Error("desktop Steam beside a gamescope game taken for Game Mode")
	}
}

// Test that a command missing from PATH is found at the usual places, the first executable one winning
func TestFind(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	notExec, steamSh := filepath.Join(dir, "steam"), filepath.Join(dir, "steam.sh")
	os.WriteFile(notExec, nil, 0o644)
	os.WriteFile(steamSh, nil, 0o755)
	if got := find("steam", filepath.Join(dir, "missing"), notExec, steamSh); got != steamSh {
		t.Errorf("find = %q, want %q", got, steamSh)
	}
	if got := find("steam", filepath.Join(dir, "missing")); got != "steam" {
		t.Errorf("find with nothing = %q, want the bare name", got)
	}

	// The native client falls back to steam.sh in its data folder, after any system-wide steam
	want := find("steam", "/usr/games/steam", "/usr/local/bin/steam")
	if want == "steam" {
		want = steamSh
	}
	if cmd := (Process{Dir: dir}).command(context.Background(), "-shutdown"); cmd.Path != want {
		t.Errorf("command path %q, want %q", cmd.Path, want)
	}
}
