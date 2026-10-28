// Changes to Steam's shortcuts.vdf and config.vdf, worked out before anything is written

package app

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ddrimus/steam-adopt/internal/state"
	"github.com/ddrimus/steam-adopt/internal/steam"
	"github.com/ddrimus/steam-adopt/internal/vdf"
)

// Steam's files as they are and as they should be, so nothing is written or closed unless they differ
type plan struct {
	// shortcuts.vdf as read, empty when missing
	original []byte
	// shortcuts.vdf as it should be
	shortcuts []byte
	// config.vdf as read, empty when missing
	configTxt     string
	newConfigTxt  string
	protonChanged []uint32
	// Whether config.vdf has a Proton mapping block
	protonFound bool
	removed     []removal
	// Removals waiting for the user's yes, their shortcuts kept until then
	pending []removal
	// Shortcuts into missing game folders not even offered for removal, since the config was just created
	held []removal
	// What to remember for the next run
	state *state.State
	// What happens to each game that ends up in Steam, recorded on the games by apply
	outcomes map[*game]*outcome
}

// One game's shortcut as planned, kept apart from the game so planning again starts from the same input
type outcome struct {
	appid  uint32
	status string
	name   string
	// Shortcut fields edited inside Steam and kept
	fromSteam []string
	// Shortcut fields edited both inside Steam and in the config, the config's value used
	conflicts []string
}

// Record the planned outcomes on the games, once the plan that gets written is final
func (p *plan) apply(games []*game) {
	for _, g := range games {
		if o := p.outcomes[g]; o != nil {
			g.appid, g.status, g.name, g.fromSteam, g.conflicts = o.appid, o.status, o.name, o.fromSteam, o.conflicts
		}
	}
}

// Check whether either of Steam's files needs writing
func (p *plan) changed() bool {
	return !bytes.Equal(p.shortcuts, p.original) || p.newConfigTxt != p.configTxt
}

// A shortcut taken out of Steam, because its game is skipped, its folder was deleted or it's a duplicate
type removal struct {
	appid  uint32
	folder string
	name   string
	why    string
}

// Why a shortcut is removed, worded for the report
const (
	whySkipped   = "skipped"
	whyDeleted   = "game folder deleted"
	whyDuplicate = "duplicate shortcut"
)

// Load shortcuts.vdf, refusing a damaged file rather than overwrite the user's shortcuts
func readShortcuts(path string) ([]byte, *vdf.Object, error) {
	original, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("can't read %s (%w)", path, err)
	}
	data, err := parseShortcuts(original)
	if err != nil {
		return nil, nil, fmt.Errorf("can't read %s (%w), fix or remove it and run steam-adopt again", path, err)
	}
	return original, data, nil
}

// Parse shortcuts.vdf contents, an empty file being an empty list
func parseShortcuts(raw []byte) (*vdf.Object, error) {
	if len(raw) == 0 {
		data := vdf.NewObject()
		data.Set("shortcuts", vdf.NewObject())
		return data, nil
	}
	return vdf.Unmarshal(raw)
}

// List the shortcut entries in file order, keeping anything that isn't an object as is
func shortcutList(data *vdf.Object) []any {
	v, _ := data.Get("shortcuts")
	o, ok := v.(*vdf.Object)
	if !ok {
		return nil
	}
	out := make([]any, 0, o.Len())
	for _, k := range o.Keys() {
		v, _ := o.Get(k)
		out = append(out, v)
	}
	return out
}

// Get a shortcut's executable without the quotes Steam stores around it
func exePath(e any) string {
	o, ok := e.(*vdf.Object)
	if !ok {
		return ""
	}
	s, _ := o.String(field(o, "Exe"))
	return strings.Trim(s, `"`)
}

// Find the spelling o uses for key, since older Steam files write keys like exe in lowercase
func field(o *vdf.Object, key string) string {
	if _, ok := o.Get(key); ok {
		return key
	}
	for _, k := range o.Keys() {
		if strings.EqualFold(k, key) {
			return k
		}
	}
	return key
}

// Set a shortcut field under the entry's own spelling, so no duplicate key is added
func setField(o *vdf.Object, key string, v any) { o.Set(field(o, key), v) }

// Read the stored appid, signed in shortcuts.vdf but unsigned in grid files and config.vdf
func entryAppid(e any) uint32 {
	o, ok := e.(*vdf.Object)
	if !ok {
		return 0
	}
	v, _ := o.Get(field(o, "appid"))
	switch n := v.(type) {
	case int32:
		return uint32(n)
	case uint64:
		return uint32(n)
	}
	return 0
}

// Get a shortcut's ID, falling back to the one Steam derives from its exe and name
func shortcutID(e any) uint32 {
	if id := entryAppid(e); id != 0 {
		return id
	}
	o, ok := e.(*vdf.Object)
	if !ok {
		return 0
	}
	exe, _ := o.String(field(o, "Exe"))
	if exe == "" {
		return 0
	}
	name, _ := o.String(field(o, "AppName"))
	return steam.ShortcutAppID(exe, name)
}

// Get a shortcut's display name, or fallback when it has none
func appName(e any, fallback string) string {
	if o, ok := e.(*vdf.Object); ok {
		if s, ok := o.String(field(o, "AppName")); ok {
			return s
		}
	}
	return fallback
}

// Quote a path the way shortcuts.vdf stores it
func quote(s string) string { return `"` + s + `"` }

// Create a shortcut entry with the fields Steam writes for one added by hand
func newShortcut(appid uint32, name, exe string) *vdf.Object {
	e := vdf.NewObject()
	e.Set("appid", int32(appid))
	e.Set("AppName", name)
	e.Set("Exe", quote(exe))
	e.Set("StartDir", quote(filepath.Dir(exe)))
	e.Set("icon", "")
	e.Set("ShortcutPath", "")
	e.Set("LaunchOptions", "")
	e.Set("IsHidden", int32(0))
	e.Set("AllowDesktopConfig", int32(1))
	e.Set("AllowOverlay", int32(1))
	e.Set("OpenVR", int32(0))
	e.Set("Devkit", int32(0))
	e.Set("DevkitGameID", "")
	e.Set("DevkitOverrideAppID", int32(0))
	e.Set("LastPlayTime", int32(0))
	e.Set("FlatpakAppID", "")
	e.Set("tags", vdf.NewObject())
	return e
}

// Compute the new shortcuts.vdf and config.vdf from the prepared games, without writing anything,
// removing only the shortcuts in approved and keeping the others as pending
func (a *App) buildPlan(games []*game, approved map[uint32]bool) (*plan, error) {
	original, data, err := readShortcuts(a.shortcutsFile)
	if err != nil {
		return nil, err
	}
	// Steam emptied its list while closing, so plan from the one it had open
	if a.restore != nil {
		if data, err = parseShortcuts(a.restore); err != nil {
			return nil, err
		}
	}
	shortcuts := shortcutList(data)
	drop := func(e any) {
		for i, s := range shortcuts {
			if s == e {
				shortcuts = append(shortcuts[:i:i], shortcuts[i+1:]...)
				return
			}
		}
	}
	proton := &steam.ProtonMap{}
	var removed, pending, held []removal

	// Removing a shortcut can cost the saves in its Proton prefix, so only what the user approved goes
	remove := func(e any, r removal) bool {
		if !approved[r.appid] {
			pending = append(pending, r)
			return false
		}
		drop(e)
		proton.Remove(r.appid)
		removed = append(removed, r)
		return true
	}

	// Proton mappings as Steam has them now, compared with the last run's
	raw, err := os.ReadFile(a.configVDF)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("can't read %s (%w)", a.configVDF, err)
	}
	configTxt := string(raw)
	next := state.New()
	outcomes := map[*game]*outcome{}

	for _, g := range games {
		// Matched by folder rather than exe, so changing exe updates the shortcut instead of duplicating it
		var existing []*vdf.Object
		for _, e := range shortcuts {
			if o, ok := e.(*vdf.Object); ok && a.owns(g, exePath(o)) {
				existing = append(existing, o)
			}
		}

		// Skipped games leave Steam, games without an exe are left alone
		if g.status == statusSkip {
			kept := false
			for _, e := range existing {
				if !remove(e, removal{shortcutID(e), g.folderName(), appName(e, g.folderName()), whySkipped}) {
					kept = true
				}
			}
			if last := a.state.Game(g.folderName()); kept && last != nil {
				next.Games[g.folderName()] = last
			}
			continue
		}
		if g.status == statusNoExe {
			if last := a.state.Game(g.folderName()); last != nil {
				next.Games[g.folderName()] = last
			}
			continue
		}

		// Existing shortcut kept under its ID, so playtime, artwork and collections stay attached
		o := &outcome{}
		outcomes[g] = o
		var entry *vdf.Object
		var before []byte
		if len(existing) > 0 {
			entry = existing[0]
			before = vdf.Marshal(entry)
			o.appid, o.status = shortcutID(entry), statusKept

			// Older Steam versions stored no ID, the derived one is written down so a rename can't change it
			if entryAppid(entry) == 0 {
				setField(entry, "appid", int32(o.appid))
			}

			// Any further shortcut into the folder is a duplicate, one with the same ID has nothing of its own to lose
			for _, d := range existing[1:] {
				if id := shortcutID(d); id == o.appid {
					drop(d)
				} else {
					remove(d, removal{id, g.folderName(), appName(d, g.folderName()), whyDuplicate})
				}
			}
		} else {
			o.appid, o.status = steam.ShortcutAppID(quote(g.exe), g.configName()), statusAdded
			entry = newShortcut(o.appid, g.configName(), g.exe)
			shortcuts = append(shortcuts, entry)
		}

		// The last run only counts for the same shortcut, a re-added one starts from the config
		last := a.state.Game(g.folderName())
		if len(existing) == 0 || (last != nil && last.AppID != o.appid) {
			last = nil
		}
		rec := &state.Game{AppID: o.appid, Fields: map[string]state.Field{}}
		next.Games[g.folderName()] = rec

		// The config's value, unless only Steam changed it since the last run
		pick := func(key, want, have string) string {
			f, ok := last.Field(key)
			v, src := state.Merge(want, have, f, ok)
			if src == state.FromSteam && have != f.Have {
				o.fromSteam = append(o.fromSteam, key)
			} else if src == state.Conflict {
				o.conflicts = append(o.conflicts, key)
			}
			rec.Fields[key] = state.Field{Want: want, Have: v}
			return v
		}
		current := func(key string) string {
			s, _ := entry.String(field(entry, key))
			return s
		}

		// Exe stays the config's, since the shortcut is found by it
		setField(entry, "Exe", quote(g.exe))
		o.name = pick("AppName", g.configName(), current("AppName"))
		setField(entry, "AppName", o.name)
		setField(entry, "StartDir", pick("StartDir", quote(filepath.Dir(g.exe)), current("StartDir")))
		setField(entry, "LaunchOptions", pick("LaunchOptions", g.opt("launch_options"), current("LaunchOptions")))

		// Without an icon of its own, a Windows build falls back to its exe and anything else is left as is
		haveIcon := current("icon")
		wantIcon := haveIcon
		if icon := g.icon(a.Paths); icon != "" {
			wantIcon = icon
		} else if haveIcon == "" && g.isWindows() {
			wantIcon = g.exe
		}
		if icon := pick("icon", wantIcon, haveIcon); icon != haveIcon {
			setField(entry, "icon", icon)
		}
		if o.status == statusKept && !bytes.Equal(vdf.Marshal(entry), before) {
			o.status = statusUpdated
		}

		// Proton mapping wanted for the game, only compared when Steam has a mapping block to compare with
		tool := g.proton
		if have, found := steam.Proton(configTxt, o.appid); tool != "" && found {
			tool = pick("proton", tool, have)
		} else if f, ok := last.Field("proton"); ok {
			rec.Fields["proton"] = f
		}
		switch tool {
		case "":
		case "none":
			proton.Remove(o.appid)
		default:
			proton.Set(o.appid, tool)
		}
	}

	// Shortcuts into a game folder that no longer exists, removed with everything made for them
	for _, e := range append([]any(nil), shortcuts...) {
		exe := filepath.Clean(exePath(e))
		if !inside(exe, a.Paths.Games) || exe == filepath.Clean(a.Paths.Games) || a.Paths.inSyncDir(exe) {
			continue
		}
		rel, err := filepath.Rel(a.Paths.Games, exe)
		if err != nil {
			continue
		}
		folder := filepath.Join(a.Paths.Games, strings.SplitN(rel, string(filepath.Separator), 2)[0])

		// Only a folder that's really gone counts, not an unreadable one nor a link to a drive that isn't mounted
		if _, err := os.Lstat(folder); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		r := removal{shortcutID(e), filepath.Base(folder), appName(e, filepath.Base(folder)), whyDeleted}

		// Without its config the games folder is new or not mounted, and every game in it would look deleted
		if a.fresh {
			held = append(held, r)
			continue
		}
		remove(e, r)
	}

	// Renumbered from 0, since Steam expects the shortcut keys to be consecutive
	list := vdf.NewObject()
	for i, e := range shortcuts {
		list.Set(strconv.Itoa(i), e)
	}
	data.Set("shortcuts", list)

	// Without a shortcuts.vdf and nothing to put in it, Steam isn't closed just to write an empty list
	out := vdf.Marshal(data)
	if len(original) == 0 && len(shortcuts) == 0 {
		out = original
	}

	// Proton mappings applied to config.vdf as text, keeping the rest of it untouched
	p := &plan{original: original, shortcuts: out, removed: removed, pending: pending, held: held, state: next,
		outcomes: outcomes}
	p.configTxt = configTxt
	p.newConfigTxt = p.configTxt
	if p.configTxt != "" {
		p.newConfigTxt, p.protonChanged, p.protonFound = steam.ApplyProton(p.configTxt, proton)
	}
	return p, nil
}
