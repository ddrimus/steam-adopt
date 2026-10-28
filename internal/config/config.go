// Reading and text-level editing of steam-adopt.conf, keeping the user's comments intact
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/ddrimus/steam-adopt/internal/fsutil"
	"github.com/ddrimus/steam-adopt/internal/ini"
)

// Section with the settings that apply to every game
const SettingsSection = "settings"

// Built-in value of every setting
var Defaults = map[string]string{
	// Empty, since a wrapper like gamemoderun that isn't installed stops every game from starting
	"launch_options": "",
	"proton":         "auto",
	"artwork":        "yes",
	"restart_steam":  "yes",
	"start_steam":    "no",
}

// Contents of a new config file
var Header = `# Settings for steam-adopt, applied the next time it runs
# Lines starting with # are ignored, remove the # in front of a line to use it

[settings]
# Launch options for every game, a game's own section can override it, for example: gamemoderun mangohud %command%
launch_options =` + Defaults["launch_options"] + `
# Proton for Windows games, on Linux only: auto picks the default set in Steam, or name one like proton_11, proton_experimental, GE-Proton10-1
proton = ` + Defaults["proton"] + `
# Download cover, banner, hero, logo and icon from the Steam store: yes / no
artwork = ` + Defaults["artwork"] + `
# Start Steam again after the sync when steam-adopt had to close it: yes / no
restart_steam = ` + Defaults["restart_steam"] + `
# Start Steam after every sync, even when it wasn't running before: yes / no
start_steam = ` + Defaults["start_steam"] + `

# One section per game folder, these options can be set in it:
#   skip = yes             leave this game out, removing it and its artwork from Steam if it was already added
#   name = My Game         the name shown in Steam
#   exe = bin/Game.exe     the file to launch, relative to the game folder and inside it
#   steam_id = 123456      Steam store ID used for artwork, the number in the game's store URL
#   launch_options = ...   launch options just for this game, empty for none
#   proton = proton_11     Proton for this game on Linux, or none to run it without Proton
#   artwork = no           leave this game's artwork alone so it can be set from inside Steam
# To use your own artwork, put cover.jpg, banner.jpg, hero.jpg or logo.png in _steam-adopt/artwork/<game folder>/,
# replacing the downloaded one, and it's kept from then on
# The commented values under each game are what steam-adopt picked on its own
`

// Check whether a setting is switched on: yes, true, 1 or on in any case
func Yes(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "1", "on":
		return true
	}
	return false
}

// Get a setting from m, or its built-in default when m doesn't set it
func Get(m map[string]string, key string) string {
	if v, ok := m[key]; ok {
		return v
	}
	return Defaults[key]
}

// Write the default config to path unless a file is already there, reporting whether it did
func Create(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = f.WriteString(Header)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err == nil, err
}

// Parse the config at path, or the default config when there is no file yet
func Read(path string) (*ini.File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ini.Parse(Header, path)
	}
	if err != nil {
		return nil, err
	}
	return ini.Parse(string(data), path)
}

// What steam-adopt picked for a new game, shown commented out in its section
type Game struct {
	Folder string
	// Relative to the game folder, "" when none was found
	Exe string
	// 0 when not found
	SteamID int64
	// The store was searched and has no such game, as opposed to not searched or the search failing
	NotFound      bool
	LaunchOptions string
	// "" leaves the line out
	Proton string
}

// Add a section for a new game to the end of the config
func AppendGame(path string, g Game) error {
	// "not found" only when the store said so, a game without an exe is never looked up
	sid := "# steam_id = (put the store ID here, the number in the game's store URL)"
	switch {
	case g.SteamID != 0:
		sid = "# steam_id = " + strconv.FormatInt(g.SteamID, 10)
	case g.NotFound:
		sid = "# steam_id = (not found on Steam, put the ID here)"
	}

	// A multi-line value would leave its later lines uncommented, breaking the config
	flat := strings.NewReplacer("\r", " ", "\n", " ")
	exe := "# exe = " + flat.Replace(g.Exe)
	if g.Exe == "" {
		exe = "# exe = (no executable found, put the file to launch here, like bin/Game.exe)"
	}
	lines := []string{
		"\n[" + g.Folder + "]",
		"# skip = yes",
		"# name = " + g.Folder,
		exe,
		sid,
		"# launch_options = " + flat.Replace(g.LaunchOptions),
	}
	// Proton is only for Steam on Linux
	if g.Proton != "" {
		lines = append(lines, "# proton = "+flat.Replace(g.Proton))
	}
	lines = append(lines, "# artwork = no")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Delete a section and the blank lines in front of it from the config
func RemoveSection(path, name string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	txt := string(data)

	// A header on the last line without a newline still counts
	if out := removeSection(txt+"\n", name); out != txt+"\n" {
		return fsutil.WriteAtomic(path, []byte(strings.TrimRight(out, "\n")+"\n"))
	}
	return nil
}

// Remove every [name] section and the blank lines before it, found by the INI parser, txt unchanged if it doesn't parse
func removeSection(txt, name string) string {
	if strings.Contains(strings.ReplaceAll(txt, "\r\n", ""), "\r") {
		return txt
	}
	f, err := ini.Parse(txt, "")
	if err != nil {
		return txt
	}

	// Line index ranges of the sections to drop, each running until the next header
	lines := strings.SplitAfter(txt, "\n")
	drop := make([]bool, len(lines))
	hs := f.Headers()
	for i, h := range hs {
		if h.Name != name {
			continue
		}
		end := len(lines)
		if i+1 < len(hs) {
			end = hs[i+1].Line - 1
		}
		for j := h.Line - 1; j < end; j++ {
			drop[j] = true
		}
	}

	eol := "\n"
	if strings.Contains(txt, "\r\n") {
		eol = "\r\n"
	}
	var out []string
	for i, line := range lines {
		if !drop[i] {
			out = append(out, line)
			continue
		}
		if i > 0 && drop[i-1] {
			continue
		}
		// Start of a dropped section: its leading blank lines go, one blank line stays between what's left
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		if len(out) > 0 {
			out = append(out, eol)
		}
	}
	return strings.Join(out, "")
}

// Rename the [old] section to [name], keeping everything in it, for a game folder that was renamed
func RenameSection(path, old, name string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, ok := renameSection(string(data), old, name)
	if !ok {
		return fmt.Errorf("can't find section [%s] to rename", old)
	}
	return fsutil.WriteAtomic(path, []byte(out))
}

// Replace the [old] header line in txt by [name], found by the INI parser like removeSection does
func renameSection(txt, old, name string) (string, bool) {
	if strings.Contains(strings.ReplaceAll(txt, "\r\n", ""), "\r") {
		return txt, false
	}
	f, err := ini.Parse(txt, "")
	if err != nil || f.Has(name) {
		return txt, false
	}
	lines := strings.SplitAfter(txt, "\n")
	for _, h := range f.Headers() {
		if h.Name != old {
			continue
		}
		line := lines[h.Line-1]
		eol := line[len(strings.TrimRight(line, "\r\n")):]
		lines[h.Line-1] = "[" + name + "]" + eol
		return strings.Join(lines, ""), true
	}
	return txt, false
}

// Wrap an error reading the config into the message shown to the user
func FormatError(path string, err error) error {
	return fmt.Errorf("error in %s:\n  %w", path, err)
}
