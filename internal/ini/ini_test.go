package ini

import (
	"errors"
	"strings"
	"testing"
)

// Test that sections, defaults, comments and multi-line values parse like configparser
func TestParse(t *testing.T) {
	f, err := Parse(`# leading comment
[DEFAULT]
shared = from default

[settings]
Launch_Options = gamemoderun %command%
proton: proton_9
empty =
; another comment
multi = first
    second

    third

[My Game: Deluxe [2024]]
name = My Game
shared = own
`, "test.conf")
	if err != nil {
		t.Fatal(err)
	}
	if !f.Has("settings") || !f.Has("My Game: Deluxe [2024]") || f.Has(DefaultSection) {
		t.Error("sections not parsed as expected")
	}
	s := f.Section("settings")
	for k, want := range map[string]string{
		"launch_options": "gamemoderun %command%",
		"proton":         "proton_9",
		"empty":          "",
		"multi":          "first\nsecond\n\nthird",
		"shared":         "from default",
	} {
		if got, ok := s[k]; !ok || got != want {
			t.Errorf("settings %s = %q, want %q", k, got, want)
		}
	}
	if got := f.Section("My Game: Deluxe [2024]")["shared"]; got != "own" {
		t.Errorf("section value should win over DEFAULT, got %q", got)
	}
	if f.Has(DefaultSection) || !f.Has("settings") || f.Has("missing") {
		t.Error("Has gives wrong answers")
	}
	if len(f.Section("missing")) != 0 {
		t.Error("a missing section should be empty, without DEFAULT values")
	}
}

// Test that Windows line endings are accepted
func TestCRLF(t *testing.T) {
	f, err := Parse("[a]\r\nk = v\r\n", "x")
	if err != nil || f.Section("a")["k"] != "v" {
		t.Fatalf("got %v, %v", f.Section("a"), err)
	}
}

// Test that parse errors name the line and the problem
func TestErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		text string
		line int
		msg  string
	}{
		"no header":         {"k = v\n", 1, "no section headers"},
		"duplicate section": {"[a]\n[b]\n[a]\n", 3, "section 'a' already exists"},
		"duplicate option":  {"[a]\nK = 1\nk = 2\n", 3, "option 'k' in section 'a' already exists"},
		"no delimiter":      {"[a]\ngarbage line\n", 2, "can't parse"},
		"empty key":         {"[a]\n= v\n", 2, "can't parse"},
	} {
		_, err := Parse(tc.text, "x.conf")
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("%s: got %v, want *Error", name, err)
			continue
		}
		if e.Line != tc.line || !strings.Contains(e.Msg, tc.msg) {
			t.Errorf("%s: got line %d %q, want line %d containing %q", name, e.Line, e.Msg, tc.line, tc.msg)
		}
	}
}

// Test that a NUL byte is refused, since it would corrupt shortcuts.vdf
func TestParseRefusesNul(t *testing.T) {
	if _, err := Parse("[g]\nname = a\x00b\n", "f"); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Errorf("err = %v", err)
	}
}
