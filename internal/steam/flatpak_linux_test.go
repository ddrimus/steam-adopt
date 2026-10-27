package steam

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Test that the filesystem overrides decide whether the games folder is visible
func TestCanSee(t *testing.T) {
	home := "/home/u"
	games := "/home/u/.local/share/games"
	for name, tc := range map[string]struct {
		files []string
		want  bool
	}{
		"no overrides":             {nil, false},
		"home":                     {[]string{"[Context]\nfilesystems=home;\n"}, true},
		"host":                     {[]string{"[Context]\nfilesystems=host\n"}, true},
		"games folder read-only":   {[]string{"[Context]\nfilesystems=~/.local/share/games:ro;\n"}, true},
		"xdg-data":                 {[]string{"[Context]\nfilesystems=xdg-data/games;\n"}, true},
		"absolute parent":          {[]string{"[Context]\nfilesystems=/home/u/.local;\n"}, true},
		"other folder":             {[]string{"[Context]\nfilesystems=~/Games;xdg-music:ro\n"}, false},
		"sibling with same start":  {[]string{"[Context]\nfilesystems=/home/u/.local/share/gam\n"}, false},
		"later file takes it back": {[]string{"[Context]\nfilesystems=home\n", "[Context]\nfilesystems=!home\n"}, false},
	} {
		dir := t.TempDir()
		var files []string
		for i, f := range tc.files {
			p := filepath.Join(dir, string(rune('a'+i)))
			os.WriteFile(p, []byte(f), 0o644)
			files = append(files, p)
		}
		if got := canSee(home, games, files); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	if !FlatpakCanSee("/home/u/.local/share/Steam", games) {
		t.Error("native Steam reported as sandboxed")
	}
}

// Test that the Steam install logged in to most recently is used
func TestFindDirPrefersLatestLogin(t *testing.T) {
	home := t.TempDir()
	native := filepath.Join(home, ".local/share/Steam")
	flatpak := filepath.Join(home, ".var/app", FlatpakID, ".local/share/Steam")
	if got := FindDir(home); got != native {
		t.Errorf("no Steam: got %q", got)
	}
	for i, p := range []string{native, flatpak} {
		os.MkdirAll(filepath.Join(p, "config"), 0o755)
		f := filepath.Join(p, "config/loginusers.vdf")
		os.WriteFile(f, nil, 0o644)
		mt := int64(1000 + i)
		os.Chtimes(f, unix(mt), unix(mt))
	}
	if got := FindDir(home); got != flatpak || !IsFlatpak(got) || IsFlatpak(native) {
		t.Errorf("got %q, want the Flatpak one", got)
	}
}

// A time from seconds since the epoch
func unix(s int64) time.Time { return time.Unix(s, 0) }
