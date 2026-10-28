package state

import (
	"os"
	"path/filepath"
	"testing"
)

// Test that each combination of config and Steam changes picks the right value
func TestMerge(t *testing.T) {
	last := Field{Want: "a", Have: "a"}
	kept := Field{Want: "a", Have: "s"}
	for name, tc := range map[string]struct {
		want, have string
		last       Field
		ok         bool
		got        string
		src        Source
	}{
		"nothing changed":                 {"a", "a", last, true, "a", FromConfig},
		"no last run, config wins":        {"c", "s", Field{}, false, "c", FromConfig},
		"config changed":                  {"c", "a", last, true, "c", FromConfig},
		"steam changed":                   {"a", "s", last, true, "s", FromSteam},
		"steam edit stays on later runs":  {"a", "s", kept, true, "s", FromSteam},
		"config changed after steam edit": {"c", "s", kept, true, "c", FromConfig},
		"both changed the same way":       {"c", "c", last, true, "c", FromConfig},
		"both changed differently":        {"c", "s", last, true, "c", Conflict},
	} {
		got, src := Merge(tc.want, tc.have, tc.last, tc.ok)
		if got != tc.got || src != tc.src {
			t.Errorf("%s: got %q, %v, want %q, %v", name, got, src, tc.got, tc.src)
		}
	}
}

// Test that the state survives a save and load, and a broken file reads as empty
func TestLoadSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil || len(s.Games) != 0 || s.Game("x") != nil {
		t.Fatalf("missing file: %v, %v", s, err)
	}
	s.Games["My Game"] = &Game{AppID: 7, Fields: map[string]Field{"AppName": {Want: "a", Have: "b"}}}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	s, err = Load(path)
	if err != nil || s.Game("My Game").AppID != 7 || s.Game("My Game").Fields["AppName"].Have != "b" {
		t.Fatalf("round trip: %+v, %v", s.Game("My Game"), err)
	}

	os.WriteFile(path, []byte("{broken"), 0o644)
	if s, err := Load(path); err == nil || s == nil || len(s.Games) != 0 {
		t.Errorf("broken file: %v, %v", s, err)
	}
	var none *State
	if none.Game("x") != nil {
		t.Error("nil state has a game")
	}
}
