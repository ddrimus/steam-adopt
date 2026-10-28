// Single-instance lock on Windows, a lock file opened without sharing

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock file in the user's own temp folder, the same for every copy of the binary
func lockPath() string {
	return filepath.Join(os.TempDir(), "steam-adopt.lock")
}

// Open the file at path unshared so two runs never edit Steam's files at once, released by Windows on exit
func lock(path string) (func(), error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		// ERROR_SHARING_VIOLATION
		if errors.Is(err, syscall.Errno(32)) {
			return nil, errors.New("steam-adopt is already running")
		}
		return nil, fmt.Errorf("can't create the lock file %s: %w", path, err)
	}
	return func() { syscall.CloseHandle(h) }, nil
}
