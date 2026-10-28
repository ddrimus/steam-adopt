package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddrimus/steam-adopt/internal/ini"
)

// Test that a section and the blank lines before it are removed
func TestRemoveSection(t *testing.T) {
	// A section runs until the next line starting with [, and takes the blank lines before it along
	for _, tc := range []struct{ in, want string }{
		{"[a]\nx=1\n\n[g]\n# c\n\n[b]\ny=2\n", "[a]\nx=1\n\n[b]\ny=2\n"},
		{"[a]\nx=1\n\n\n[g]\nz=1\n", "[a]\nx=1\n\n"},
		{"[g]\n[a]\n", "[a]\n"},
		// Headers the parser accepts, and CRLF files
		{"[a]\nx=1\n[g]  \nz=1\n[b]\n", "[a]\nx=1\n\n[b]\n"},
		{"[a]\nx=1\n[g] # note\nz=1\n", "[a]\nx=1\n\n"},
		{"[a]\r\nx=1\r\n\r\n[g]\r\nz=1\r\n[b]\r\n", "[a]\r\nx=1\r\n\r\n[b]\r\n"},
		// An indented [g] continuing a value isn't a header
		{"[a]\nx=1\n  [g]\n[g]\nz=1\n", "[a]\nx=1\n  [g]\n\n"},
		// A file that doesn't parse is left alone
		{"x[g]\n[g]\nq\n", "x[g]\n[g]\nq\n"},
		{"[a]\n", "[a]\n"},
		{"[gg]\n[g]\n", "[gg]\n\n"},
	} {
		if got := removeSection(tc.in, "g"); got != tc.want {
			t.Errorf("removeSection(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Test that the config is created once, and game sections are added and removed
func TestCreateAppendRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steam-adopt.conf")
	if created, err := Create(path); !created || err != nil {
		t.Fatalf("Create = %v, %v", created, err)
	}
	if created, err := Create(path); created || err != nil {
		t.Fatalf("second Create = %v, %v", created, err)
	}
	if err := AppendGame(path, Game{Folder: "My Game", Exe: "bin/game.exe", SteamID: 42, LaunchOptions: "%command%", Proton: "proton_9"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendGame(path, Game{Folder: "Other", Exe: "run.sh", NotFound: true, Proton: "none"}); err != nil {
		t.Fatal(err)
	}
	f, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Has("My Game") || !f.Has("Other") || len(f.Section("My Game")) != 0 {
		b, _ := os.ReadFile(path)
		t.Fatalf("sections wrong:\n%s", b)
	}
	if got := f.Section(SettingsSection)["proton"]; got != "auto" {
		t.Errorf("settings proton = %q", got)
	}

	if err := RemoveSection(path, "My Game"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := Header + "\n[Other]\n# skip = yes\n# name = Other\n# exe = run.sh\n# steam_id = (not found on Steam, put the ID here)\n" +
		"# launch_options = \n# proton = none\n# artwork = no\n"
	if string(data) != want {
		t.Errorf("after remove:\n%s\nwant:\n%s", data, want)
	}
}

// Test that a section header on the last line without a newline is removed
func TestRemoveSectionLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steam-adopt.conf")
	os.WriteFile(path, []byte("[a]\nx=1\n\n[g]"), 0o644)
	if err := RemoveSection(path, "g"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "[a]\nx=1\n" {
		t.Errorf("got %q", b)
	}
}

// Test that a multi-line value can't break the config
func TestAppendGameMultiLineValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steam-adopt.conf")
	Create(path)
	if err := AppendGame(path, Game{Folder: "G", Exe: "g.exe", LaunchOptions: "A=1\nmangohud %command%", Proton: "none"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err != nil {
		t.Errorf("config no longer parses: %v", err)
	}
}

// Test that renaming a section keeps its contents and line endings, and refuses to clash with an existing one
func TestRenameSection(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"[a]\nx=1\n[g] # note\nz=1\n", "[a]\nx=1\n[h]\nz=1\n", true},
		{"[a]\r\n[g]\r\nz=1\r\n", "[a]\r\n[h]\r\nz=1\r\n", true},
		{"[g]", "[h]", true},
		{"[a]\nx=1\n  [g]\n", "[a]\nx=1\n  [g]\n", false},
		{"[g]\n[h]\n", "[g]\n[h]\n", false},
	} {
		if got, ok := renameSection(tc.in, "g", "h"); got != tc.want || ok != tc.ok {
			t.Errorf("renameSection(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// Fuzz test that removing or renaming a section never leaves a config that fails to parse
func FuzzSections(f *testing.F) {
	f.Add(Header+"\n[A]\nx = 1\n\n[B]\n# c\n", "A", "C")
	f.Add("[A]\r\nx=1\r\n  y\r\n[A2]\r\n", "A", "B")
	f.Fuzz(func(t *testing.T, txt, name, to string) {
		if strings.Contains(strings.ReplaceAll(txt, "\r\n", ""), "\r") {
			return
		}
		if _, err := ini.Parse(txt, ""); err != nil {
			return
		}
		out := removeSection(txt, name)
		f2, err := ini.Parse(out, "")
		if err != nil {
			t.Fatalf("remove broke the file: %v\n%q -> %q", err, txt, out)
		}
		if f2.Has(name) {
			t.Fatalf("section still there: %q -> %q", txt, out)
		}
		if strings.ContainsAny(to, "\r\n]\x00") || to == "" || to == ini.DefaultSection || to == name {
			return
		}
		r, ok := renameSection(txt, name, to)
		if !ok {
			return
		}
		f3, err := ini.Parse(r, "")
		if err != nil || !f3.Has(to) {
			t.Fatalf("rename broke the file: %v\n%q -> %q", err, txt, r)
		}
	})
}
