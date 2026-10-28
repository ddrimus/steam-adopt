// What steam-adopt last wanted and left in Steam per game, so edits made inside Steam can be told apart from config changes
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/ddrimus/steam-adopt/internal/fsutil"
)

// One shortcut field after a run: the config's value and the value left in Steam
type Field struct {
	Want string `json:"want"`
	Have string `json:"have"`
}

// One game's shortcut after a run, tied to its ID so a re-added shortcut starts fresh
type Game struct {
	AppID  uint32           `json:"appid"`
	Fields map[string]Field `json:"fields"`
}

// Every game's last run, keyed by folder name
type State struct {
	Games map[string]*Game `json:"games"`
}

// Create an empty state, what a first run starts from
func New() *State { return &State{Games: map[string]*Game{}} }

// Get a game's last run, nil when there is none
func (s *State) Game(folder string) *Game {
	if s == nil {
		return nil
	}
	return s.Games[folder]
}

// Get a field from the last run, false when the game or field is unknown
func (g *Game) Field(key string) (Field, bool) {
	if g == nil {
		return Field{}, false
	}
	f, ok := g.Fields[key]
	return f, ok
}

// Read the state file, empty when missing
func Load(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return New(), err
	}
	s := New()
	if err := json.Unmarshal(raw, s); err != nil {
		return New(), fmt.Errorf("can't read %s (%w), the config is applied as is this run", path, err)
	}
	if s.Games == nil {
		s.Games = map[string]*Game{}
	}
	return s, nil
}

// Write the state file, only when it differs from what's there
func (s *State) Save(path string) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if old, err := os.ReadFile(path); err == nil && string(old) == string(raw) {
		return nil
	}
	return fsutil.WriteAtomic(path, raw)
}

// Where a merged value came from
type Source int

// Possible sources of a merged value
const (
	// The config's value, because it changed or Steam agrees or nothing is known yet
	FromConfig Source = iota
	// Steam's value, because only Steam changed it
	FromSteam
	// The config's value, although Steam changed it too
	Conflict
)

// Pick a field's value from the config's (want), Steam's (have) and the last run's, ok false when there is no last run
func Merge(want, have string, last Field, ok bool) (string, Source) {
	switch {
	case !ok || want == have:
		return want, FromConfig
	case want == last.Want:
		return have, FromSteam
	case have == last.Have:
		return want, FromConfig
	}
	return want, Conflict
}
