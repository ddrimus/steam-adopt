package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Test that atomic writes keep permissions and leave nothing behind on failure
func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	// New file
	if err := WriteAtomic(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "one" {
		t.Errorf("got %q", b)
	}

	// Replacing keeps the old file's permissions
	os.Chmod(path, 0o600)
	if err := WriteAtomic(path, []byte("two")); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if b, _ := os.ReadFile(path); string(b) != "two" || (unixPerms && st.Mode().Perm() != 0o600) {
		t.Errorf("got %q, mode %v", b, st.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}

	// A failed write leaves nothing
	if err := WriteAtomic(filepath.Join(dir, "missing/f"), []byte("x")); err == nil {
		t.Error("write into a missing folder succeeded")
	}

	// A failed rename removes the temp file and keeps what's at path
	target := filepath.Join(dir, "dir")
	os.MkdirAll(filepath.Join(target, "child"), 0o755)
	if err := WriteAtomic(target, []byte("x")); err == nil {
		t.Error("replacing a non-empty folder succeeded")
	}
	if Exists(target+".steam-adopt-tmp") || !IsDir(target) {
		t.Error("failed write left a temp file or lost the original")
	}
}

// Test that a copy keeps the permissions and modification time
func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	os.WriteFile(src, []byte("data"), 0o640)
	mtime := time.Unix(1_600_000_000, 0)
	os.Chtimes(src, mtime, mtime)

	if err := CopyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(dst)
	if b, _ := os.ReadFile(dst); string(b) != "data" {
		t.Errorf("got %q", b)
	}
	if (unixPerms && st.Mode().Perm() != 0o640) || !st.ModTime().Equal(mtime) {
		t.Errorf("mode %v, mtime %v", st.Mode().Perm(), st.ModTime())
	}
	if err := CopyFile(filepath.Join(dir, "missing"), dst); err == nil {
		t.Error("copy of a missing file succeeded")
	}
}

// Test that glob characters in the folder name are taken literally
func TestGlobDirTakesDirLiterally(t *testing.T) {
	// A folder name that is itself a glob pattern, without the * Windows doesn't allow in names
	dir := filepath.Join(t.TempDir(), "Game [GOG]")
	os.MkdirAll(dir, 0o755)
	for _, f := range []string{"icon-1.jpg", "icon-2.jpg", "cover.jpg"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o644)
	}
	if got := GlobDir(dir, "icon-*.jpg"); len(got) != 2 || filepath.Dir(got[0]) != dir {
		t.Errorf("got %v", got)
	}
	if got := GlobDir(filepath.Join(dir, "missing"), "*"); got != nil {
		t.Errorf("missing folder: %v", got)
	}
}

// Test that backups are timestamped, never overwrite an earlier one from the same second, and are pruned to keep
func TestBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shortcuts.vdf")
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := 0; i < 7; i++ {
		os.WriteFile(path, []byte{byte('a' + i)}, 0o644)
		if _, err := Backup(path, 5, start.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	got := GlobDir(dir, "shortcuts.vdf.steam-adopt-*.bak")
	if len(got) != 5 {
		t.Fatalf("%d backups, want 5: %v", len(got), got)
	}
	if b, _ := os.ReadFile(got[0]); string(b) != "c" {
		t.Errorf("oldest kept backup holds %q, want the third version", b)
	}

	// Same second again: the first copy of that second stays
	os.WriteFile(path, []byte("z"), 0o644)
	dst, _ := Backup(path, 5, start.Add(6*time.Second))
	if dst != path+".steam-adopt-20260102-030411.bak" {
		t.Errorf("backup path %s", dst)
	}
	if b, _ := os.ReadFile(dst); string(b) != "g" {
		t.Errorf("backup from the same second holds %q, want g", b)
	}
}

// Windows has no Unix permission bits to keep
var unixPerms = runtime.GOOS != "windows"

// Test that writing through a symlink replaces the file it points at and keeps the link
func TestWriteAtomicFollowsSymlink(t *testing.T) {
	if !unixPerms {
		t.Skip("symlinks need Developer Mode on Windows")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real.vdf")
	link := filepath.Join(dir, "link.vdf")
	os.WriteFile(real, []byte("old"), 0o644)
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(link, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if !IsSymlink(link) {
		t.Error("the link was replaced by a file")
	}
	if b, _ := os.ReadFile(real); string(b) != "new" {
		t.Errorf("target holds %q, want new", b)
	}
}
