// Small file helpers: existence checks, literal globbing, atomic writes and faithful copies
package fsutil

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Check whether p exists, following symlinks
func Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Check whether p exists without following symlinks, so a link to an unmounted drive still counts
func LExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Check whether p is a directory, following symlinks
func IsDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Check whether p is a regular file, following symlinks
func IsFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Check whether p itself is a symlink
func IsSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&fs.ModeSymlink != 0
}

// List entries of dir matching pattern, taking dir literally unlike filepath.Glob so [ or * are safe
func GlobDir(dir, pattern string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if ok, _ := filepath.Match(pattern, e.Name()); ok {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// Replace path through a synced temp file and rename, so a crash never leaves it half-written, writing through a
// symlink to the file it points at rather than replacing the link
func WriteAtomic(path string, data []byte) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	perm := fs.FileMode(0o666)
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	tmp := path + ".steam-adopt-tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// Flush a directory entry change to disk, best effort
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// Copy src to dst with its permissions and modification time, both best effort since exFAT refuses permissions
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	os.Chmod(dst, st.Mode().Perm())
	os.Chtimes(dst, st.ModTime(), st.ModTime())
	return nil
}

// Rename src to dst, falling back to copy and delete across filesystems
func MoveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := CopyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// Copy path to a timestamped backup next to it, keeping only the newest keep backups, and return the backup's path
func Backup(path string, keep int, now time.Time) (string, error) {
	dst := path + ".steam-adopt-" + now.Format("20060102-150405") + ".bak"

	// Two runs in the same second: the first backup is the older file, the one worth keeping
	if !Exists(dst) {
		if err := CopyFile(path, dst); err != nil {
			os.Remove(dst)
			return "", err
		}
	}

	// Timestamps sort by name, oldest first
	old := GlobDir(filepath.Dir(path), filepath.Base(path)+".steam-adopt-*.bak")
	sort.Strings(old)
	for len(old) > keep {
		os.Remove(old[0])
		old = old[1:]
	}
	return dst, nil
}
