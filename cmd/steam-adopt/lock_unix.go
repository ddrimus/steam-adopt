// Single-instance lock on Unix, an flock on a file in the user's runtime folder

//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Per-user lock file in the runtime folder, the same for every copy of the binary and for runs from cron,
// which often leaves XDG_RUNTIME_DIR unset although the folder is there
func lockPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return filepath.Join(os.TempDir(), fmt.Sprintf("steam-adopt-%d.lock", os.Getuid()))
	}
	return filepath.Join(dir, "steam-adopt.lock")
}

// Lock the file at path so two runs never edit Steam's files at once, released when steam-adopt exits
func lock(path string) (func(), error) {
	// The fallback sits in the shared temp folder, where another user could plant a link or hold the lock
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("can't create the lock file %s: %w", path, err)
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || !ownedByMe(st) {
		f.Close()
		return nil, fmt.Errorf("the lock file %s isn't a file of your own, remove it and run steam-adopt again", path)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("steam-adopt is already running")
		}
		return nil, fmt.Errorf("can't lock %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}

// Check whether the current user owns a file
func ownedByMe(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && sys.Uid == uint32(os.Getuid())
}
