//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test that the lock file goes in the runtime folder, or the temp folder without one
func TestLockPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	if got := lockPath(); got != filepath.Join(dir, "steam-adopt.lock") {
		t.Errorf("got %q", got)
	}

	// A runtime folder that isn't there falls back to the temp folder
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(dir, "missing"))
	if got := lockPath(); filepath.Dir(got) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(got), "steam-adopt-") {
		t.Errorf("got %q", got)
	}
}

// Test that a lock file planted as a link, as another user could in the shared temp folder, is refused
func TestLockRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	os.WriteFile(target, nil, 0o600)
	path := filepath.Join(dir, "steam-adopt.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if unlock, err := lock(path); err == nil {
		unlock()
		t.Error("lock followed a symlink")
	}
}
