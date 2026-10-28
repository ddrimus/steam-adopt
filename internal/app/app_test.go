package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ddrimus/steam-adopt/internal/fsutil"
	"github.com/ddrimus/steam-adopt/internal/steam"
	"github.com/ddrimus/steam-adopt/internal/store"
	"github.com/ddrimus/steam-adopt/internal/vdf"
)

// Create a file of the given size, with any missing folders
func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Test that the main executable is picked by name, helpers, depth, kind and size
func TestFindExe(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]int
		want  string
	}{
		"named like the folder beats bigger helpers": {
			map[string]int{"My Game.exe": 10, "UnityCrashHandler64.exe": 999, "other.exe": 500}, "My Game.exe"},
		"punctuation doesn't matter for the name": {
			map[string]int{"my_game.exe": 1, "big.exe": 99}, "my_game.exe"},
		"non-helper beats helper": {
			map[string]int{"setup.exe": 999, "run.exe": 1}, "run.exe"},
		"shallowest wins": {
			map[string]int{"bin/deep.exe": 999, "top.exe": 1}, "top.exe"},
		"native beats windows": {
			map[string]int{"game.exe": 999, "game.x86_64": 1}, "game.x86_64"},
		"windows beats a script": {
			map[string]int{"run.sh": 999, "game.exe": 1}, "game.exe"},
		"a script on its own is found": {
			map[string]int{"start.sh": 1, "data.pak": 9}, "start.sh"},
		"biggest wins": {
			map[string]int{"a.exe": 1, "b.exe": 2}, "b.exe"},
		"too deep is ignored": {
			map[string]int{"a/b/c/game.exe": 1}, ""},
		"two levels down is found": {
			map[string]int{"a/b/game.exe": 1, "data.pak": 9}, "a/b/game.exe"},
	} {
		dir := filepath.Join(t.TempDir(), "My Game")
		for f, size := range tc.files {
			write(t, filepath.Join(dir, f), size)
		}
		want := ""
		if tc.want != "" {
			want = filepath.Join(dir, tc.want)
		}
		if got := findExe(dir); got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}

// Steam client that only exists in memory
type fakeSteam struct {
	mu                sync.Mutex
	running, gameMode bool
	shutdowns, starts int
	// Run as Steam exits, like Steam flushing its files
	onShutdown func()
}

// Report whether the fake Steam is running
func (f *fakeSteam) Running() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.running }

// Report whether the fake Steam runs in Game Mode
func (f *fakeSteam) GameMode() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.running && f.gameMode }

// Close the fake Steam at once, counting the shutdowns
func (f *fakeSteam) Shutdown(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdowns++
	f.running = false
	if f.onShutdown != nil {
		f.onShutdown()
	}
	return nil
}

// Start the fake Steam, counting the starts
func (f *fakeSteam) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.running = true
	return nil
}

// Serve a Steam store that knows one game
func fakeStore(t *testing.T) *store.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/storesearch/" && r.URL.Query().Get("term") == "Windows Game":
			w.Write([]byte(`{"items":[{"type":"app","name":"Windows Game","id":100}]}`))
		case r.URL.Path == "/api/storesearch/":
			w.Write([]byte(`{"items":[]}`))
		case r.URL.Path == "/IStoreBrowseService/GetItems/v1/":
			w.Write([]byte(`{"response":{"store_items":[{"assets":{"asset_url_format":"apps/100/${FILENAME}",` +
				`"library_capsule":"c.jpg","header":"h.jpg","library_hero":"hero.jpg","community_icon":"ic"}}]}}`))
		default:
			w.Write([]byte("image"))
		}
	}))
	t.Cleanup(srv.Close)
	c := store.New()
	c.StoreURL, c.APIURL, c.CDNURL = srv.URL, srv.URL, srv.URL
	return c
}

// Steam login file with one most recent account
const loginUsers = `"users"
{
	"76561198350641752"
	{
		"MostRecent"		"1"
	}
}
`

// Steam config with proton_9 as the default compatibility tool
const configVDF = `"InstallConfigStore"
{
	"Software"
	{
		"Valve"
		{
			"Steam"
			{
				"CompatToolMapping"
				{
					"0"
					{
						"name"		"proton_9"
						"config"		""
						"priority"		"75"
					}
				}
			}
		}
	}
}
`

// Fake home folder with Steam, a few games and a fake store
type env struct {
	t     *testing.T
	home  string
	paths Paths
	steam *fakeSteam
	store *store.Client
	user  string
	// Answer to the removal question, nil runs without asking like a non-interactive run
	confirm func([]string) bool
}

// Set up a home folder with a logged-in Steam and four game folders
func newEnv(t *testing.T) *env {
	home := t.TempDir()
	steamDir := filepath.Join(home, ".local/share/Steam")
	os.MkdirAll(filepath.Join(steamDir, "config"), 0o755)
	os.WriteFile(filepath.Join(steamDir, "config/loginusers.vdf"), []byte(loginUsers), 0o644)
	os.WriteFile(filepath.Join(steamDir, "config/config.vdf"), []byte(configVDF), 0o644)
	e := &env{t: t, home: home, paths: DefaultPaths(home), steam: &fakeSteam{running: true}, store: fakeStore(t),
		user: filepath.Join(steamDir, "userdata/390376024")}
	// Windows looks for Steam outside the home folder, so the fake one is named directly
	e.paths.Steam = steamDir
	write(t, filepath.Join(e.paths.Games, "Windows Game/Windows Game.exe"), 100)
	write(t, filepath.Join(e.paths.Games, "Windows Game/UnityCrashHandler64.exe"), 900)
	write(t, filepath.Join(e.paths.Games, "Linux Game/start.sh"), 10)
	os.MkdirAll(filepath.Join(e.paths.Games, "No Exe"), 0o755)
	os.MkdirAll(filepath.Join(e.paths.Games, ".hidden"), 0o755)
	return e
}

// Build an App on the fake home folder, writing its output and warnings to out
func (e *env) app(out io.Writer) *App {
	return &App{Paths: e.paths, Steam: e.steam, Store: e.store, Out: out, Warn: out, SettleDelay: 1, PollInterval: 1,
		Confirm: e.confirm}
}

// Run one sync, failing the test on error and returning its output
func (e *env) run(dry bool) string {
	e.t.Helper()
	var out bytes.Buffer
	a := e.app(&out)
	a.DryRun = dry
	if err := a.Run(context.Background()); err != nil {
		e.t.Fatalf("Run: %v\n%s", err, out.String())
	}
	return out.String()
}

// Read back the shortcut entries written to shortcuts.vdf
func (e *env) shortcuts() []*vdf.Object {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.user, "config/shortcuts.vdf"))
	if err != nil {
		e.t.Fatal(err)
	}
	o, err := vdf.Unmarshal(data)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []*vdf.Object
	for _, v := range shortcutList(o) {
		out = append(out, v.(*vdf.Object))
	}
	return out
}

// Read back Steam's config.vdf
func (e *env) configVDF() string {
	b, _ := os.ReadFile(filepath.Join(e.paths.Steam, "config/config.vdf"))
	return string(b)
}

// Fail the test for each wanted string missing from got
func mustContain(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%s doesn't contain %q:\n%s", what, w, got)
		}
	}
}

// Test that a dry run, a first run, a rerun and config edits all sync Steam as expected
func TestRun(t *testing.T) {
	e := newEnv(t)

	// A dry run touches nothing
	out := e.run(true)
	mustContain(t, "dry run output", out, "Dry run, nothing changed: 2 added, 0 removed")
	if _, err := os.Stat(e.paths.Sync); err == nil || e.steam.shutdowns != 0 {
		t.Fatal("dry run changed something")
	}

	// First real run adds both games, closing and restarting Steam around the write
	out = e.run(false)
	mustContain(t, "first run output", out,
		"Created config file:",
		"Closing Steam...",
		"  + Linux Game: added (start.sh)",
		"  ? No Exe: no executable found, set exe under [No Exe] in steam-adopt.conf",
		"  + Windows Game: added (Windows Game.exe)",
		"      artwork: Steam ID 100 (Windows Game)",
		"Proton setting changed for 1 game(s)",
		"Done: 2 added, 0 removed",
		"Starting Steam...")
	if e.steam.shutdowns != 1 || e.steam.starts != 1 {
		t.Errorf("shutdowns %d, starts %d", e.steam.shutdowns, e.steam.starts)
	}

	sc := e.shortcuts()
	if len(sc) != 2 {
		t.Fatalf("got %d shortcuts", len(sc))
	}
	win := sc[1]
	winExe := filepath.Join(e.paths.Games, "Windows Game/Windows Game.exe")
	appid := steam.ShortcutAppID(`"`+winExe+`"`, "Windows Game")
	if got := entryAppid(win); got != appid {
		t.Errorf("appid %d, want %d", got, appid)
	}
	if s, _ := win.String("LaunchOptions"); s != "" {
		t.Errorf("LaunchOptions %q", s)
	}
	if s, _ := win.String("icon"); s != filepath.Join(e.paths.Art, "Windows Game/icon-100.jpg") {
		t.Errorf("icon %q", s)
	}
	mustContain(t, "config.vdf", e.configVDF(), `"`+itoa(appid)+`"`, `"name"		"proton_9"`)
	grid := filepath.Join(e.user, "config/grid")
	if target, _ := os.Readlink(filepath.Join(grid, itoa(appid)+"p.jpg")); target != filepath.Join(e.paths.Art, "Windows Game/cover.jpg") {
		t.Errorf("cover link -> %q", target)
	}
	conf, _ := os.ReadFile(e.paths.Config)
	mustContain(t, "steam-adopt.conf", string(conf), "[Windows Game]\n", "# steam_id = 100\n", "# proton = proton_9\n",
		"[Linux Game]\n", "# exe = start.sh\n", "# proton = none\n")
	// A game without an executable gets a section with the exe line ready to fill in
	// It's never looked up, so its steam_id line doesn't claim the store has no such game, unlike one that was searched
	mustContain(t, "steam-adopt.conf", string(conf), "[No Exe]\n# skip = yes\n# name = No Exe\n# exe = (no executable found",
		"like bin/Game.exe)\n# steam_id = (put the store ID here", "# exe = start.sh\n# steam_id = (not found on Steam")

	// Nothing changed, so Steam is left alone
	out = e.run(false)
	mustContain(t, "rerun output", out, "  = Windows Game: up to date", "(already downloaded)", "Steam already up to date")
	if e.steam.shutdowns != 1 {
		t.Error("Steam was closed with nothing to write")
	}

	// Rename a game, switch off Proton for it, skip another, delete a folder
	edit(t, e.paths.Config, "[Windows Game]\n", "[Windows Game]\nname = Renamed\nproton = none\n")
	edit(t, e.paths.Config, "[No Exe]\n", "[No Exe]\nskip = yes\n")
	os.RemoveAll(filepath.Join(e.paths.Games, "Linux Game"))
	var asked []string
	e.confirm = func(names []string) bool { asked = names; return true }
	out = e.run(false)
	if len(asked) != 1 || asked[0] != "Linux Game" {
		t.Errorf("asked about %v", asked)
	}
	mustContain(t, "edit run output", out,
		"  ~ Renamed: updated",
		"  - No Exe: skipped",
		"  - Linux Game: game folder deleted, removed from Steam",
		"Done: 0 added, 1 removed")
	sc = e.shortcuts()
	if len(sc) != 1 || entryAppid(sc[0]) != appid {
		t.Fatalf("after edits: %d shortcuts", len(sc))
	}
	if name, _ := sc[0].String("AppName"); name != "Renamed" {
		t.Errorf("AppName %q", name)
	}
	if strings.Contains(e.configVDF(), `"`+itoa(appid)+`"`) {
		t.Error("Proton mapping not removed")
	}
	conf, _ = os.ReadFile(e.paths.Config)
	if strings.Contains(string(conf), "[Linux Game]") {
		t.Error("deleted game's section kept")
	}
	if b, _ := filepath.Glob(filepath.Join(e.user, "config/shortcuts.vdf.steam-adopt-*.bak")); len(b) == 0 {
		t.Error("no backup of shortcuts.vdf")
	}
}

// Test that artwork = no and restart_steam = no are honored
func TestRunWithArtworkAndRestartOff(t *testing.T) {
	// A config written before the first run is used as is
	e := newEnv(t)
	os.MkdirAll(e.paths.Sync, 0o755)
	os.WriteFile(e.paths.Config, []byte("[settings]\nartwork = no\nrestart_steam = no\n"), 0o644)
	out := e.run(false)
	mustContain(t, "output", out, "artwork: off", "Done: 2 added")
	if strings.Contains(out, "Created config file") {
		t.Error("existing config replaced")
	}
	if e.steam.starts != 0 {
		t.Error("Steam started with restart_steam = no")
	}
	if _, err := os.Stat(e.paths.Art); err == nil {
		t.Error("artwork downloaded with artwork = no")
	}
}

// Test that a damaged shortcuts.vdf or a cancelled run stops with an error
func TestRunErrors(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(filepath.Join(e.user, "config"), 0o755)
	os.WriteFile(filepath.Join(e.user, "config/shortcuts.vdf"), []byte("\x00broken"), 0o644)
	a := e.app(io.Discard)
	if err := a.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "fix or remove it") {
		t.Errorf("damaged shortcuts.vdf: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	os.Remove(filepath.Join(e.user, "config/shortcuts.vdf"))
	if err := a.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled run: %v", err)
	}
}

// Test that a game whose name looks like a helper is still picked
func TestFindExeHelperLookingGame(t *testing.T) {
	// A game called "Crash ..." still wins over a non-helper name
	dir := filepath.Join(t.TempDir(), "Crash Course")
	write(t, filepath.Join(dir, "CrashCourse.exe"), 1)
	write(t, filepath.Join(dir, "play.exe"), 99)
	if got := findExe(dir); got != filepath.Join(dir, "CrashCourse.exe") {
		t.Errorf("got %q", got)
	}
}

// Test that waiting for a Steam that never closes gives up after the timeout
func TestCloseSteamTimeout(t *testing.T) {
	stuck := &stuckSteam{}
	a := &App{Steam: stuck, Out: &bytes.Buffer{}, CloseTimeout: 5, PollInterval: 1}
	if err := a.closeSteam(context.Background()); !errors.Is(err, ErrSteamDidNotClose) {
		t.Errorf("got %v", err)
	}
}

// Steam client that never closes
type stuckSteam struct{}

// Report the stuck Steam as always running
func (stuckSteam) Running() bool { return true }

// Report the stuck Steam as a desktop client
func (stuckSteam) GameMode() bool { return false }

// Ignore the request to shut down
func (stuckSteam) Shutdown(context.Context) error { return nil }

// Ignore the request to start
func (stuckSteam) Start() error { return nil }

// Replace the first occurrence of old in a file, failing when it isn't there
func edit(t *testing.T, path, old, repl string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), old) {
		t.Fatalf("%s doesn't contain %q", path, old)
	}
	os.WriteFile(path, []byte(strings.Replace(string(b), old, repl, 1)), 0o644)
}

// Format an appid the way Steam names its files
func itoa(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

// Test that a shortcut from an older Steam is edited in place under its derived ID
func TestRunOldFormatShortcut(t *testing.T) {
	// Old lowercase keys edited in place, and Steam's derived ID kept rather than becoming app 0
	e := newEnv(t)
	exe := filepath.Join(e.paths.Games, "Windows Game/Windows Game.exe")
	old := vdf.NewObject()
	old.Set("appname", "Old Name")
	old.Set("exe", `"`+exe+`"`)
	old.Set("StartDir", `"`+filepath.Dir(exe)+`"`)
	shortcuts := vdf.NewObject()
	shortcuts.Set("0", old)
	data := vdf.NewObject()
	data.Set("shortcuts", shortcuts)
	os.MkdirAll(filepath.Join(e.user, "config"), 0o755)
	os.WriteFile(filepath.Join(e.user, "config/shortcuts.vdf"), vdf.Marshal(data), 0o644)

	e.run(false)
	appid := steam.ShortcutAppID(`"`+exe+`"`, "Old Name")
	var entry *vdf.Object
	for _, s := range e.shortcuts() {
		if exePath(s) == exe {
			entry = s
		}
	}
	if entry == nil || entryAppid(entry) != appid {
		t.Fatalf("shortcut %v, want appid %d", entry, appid)
	}
	if name, _ := entry.String("appname"); name != "Windows Game" {
		t.Errorf("appname %q", name)
	}
	for _, k := range entry.Keys() {
		if k == "AppName" || k == "Exe" {
			t.Errorf("duplicate key %q added: %v", k, entry.Keys())
		}
	}
	mustContain(t, "config.vdf", e.configVDF(), "\"0\"\n\t\t\t\t\t{\n\t\t\t\t\t\t\"name\"\t\t\"proton_9\"", `"`+itoa(appid)+`"`)
	if target, _ := os.Readlink(filepath.Join(e.user, "config/grid", itoa(appid)+"p.jpg")); target == "" {
		t.Error("artwork not linked under Steam's derived ID")
	}
}

// Test that a Proton name that would break config.vdf is refused
func TestRunRefusesBrokenProtonName(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	edit(t, e.paths.Config, "[Windows Game]\n", "[Windows Game]\nproton = GE Proton\"9\n")
	before := e.configVDF()
	out := e.run(false)
	mustContain(t, "output", out, "isn't a valid tool name")
	if e.configVDF() != before {
		t.Errorf("config.vdf changed:\n%s", e.configVDF())
	}

	// Quotes around a valid name are forgiven
	edit(t, e.paths.Config, "proton = GE Proton\"9\n", "proton = \"GE-Proton10-1\"\n")
	e.run(false)
	mustContain(t, "config.vdf", e.configVDF(), "\"name\"\t\t\"GE-Proton10-1\"\n")
}

// Test that pictures set in Steam are kept for a game not on the store
func TestRunKeepsSteamArtworkWithoutStoreID(t *testing.T) {
	// Linux Game isn't on the store, but has a custom cover and a hero picked from inside Steam
	e := newEnv(t)
	e.run(false)
	exe := filepath.Join(e.paths.Games, "Linux Game/start.sh")
	appid := steam.ShortcutAppID(`"`+exe+`"`, "Linux Game")
	art := filepath.Join(e.paths.Art, "Linux Game")
	grid := filepath.Join(e.user, "config/grid")
	os.MkdirAll(art, 0o755)
	os.WriteFile(filepath.Join(art, "hero.jpg"), []byte("custom"), 0o644)
	os.Remove(filepath.Join(grid, itoa(appid)+"_hero.jpg"))
	os.WriteFile(filepath.Join(grid, itoa(appid)+"_hero.jpg"), []byte("from steam"), 0o644)

	e.run(false)
	if b, _ := os.ReadFile(filepath.Join(art, "hero.jpg")); string(b) != "from steam" {
		t.Errorf("picture set in Steam lost, artwork folder has %q", b)
	}
	if target, _ := os.Readlink(filepath.Join(grid, itoa(appid)+"_hero.jpg")); target != filepath.Join(art, "hero.jpg") {
		t.Errorf("hero link -> %q", target)
	}
}

// Test that Steam is started only by restart_steam after closing it, or by start_steam
func TestRunStartsSteamOnlyWhenAsked(t *testing.T) {
	// Steam isn't running: restart_steam alone leaves it closed, even with changes written
	e := newEnv(t)
	e.steam.running = false
	e.run(false)
	if e.steam.shutdowns != 0 || e.steam.starts != 0 {
		t.Fatalf("shutdowns %d, starts %d with Steam already closed", e.steam.shutdowns, e.steam.starts)
	}

	// start_steam starts it after every sync, even with nothing to change
	edit(t, e.paths.Config, "start_steam = no\n", "start_steam = yes\n")
	out := e.run(false)
	mustContain(t, "output", out, "Steam already up to date", "Starting Steam...")
	if e.steam.starts != 1 {
		t.Errorf("starts %d, want 1", e.steam.starts)
	}

	// Already running, so it isn't started twice
	e.run(false)
	if e.steam.starts != 1 {
		t.Errorf("Steam started while running, starts %d", e.steam.starts)
	}
}

// Find the shortcut whose exe sits in a game folder
func (e *env) shortcut(folder string) *vdf.Object {
	e.t.Helper()
	for _, s := range e.shortcuts() {
		if inside(exePath(s), filepath.Join(e.paths.Games, folder)) {
			return s
		}
	}
	e.t.Fatalf("no shortcut for %s", folder)
	return nil
}

// Change a shortcut's fields the way an edit inside Steam would
func (e *env) editShortcut(folder string, fields map[string]string) {
	e.t.Helper()
	data, _ := os.ReadFile(filepath.Join(e.user, "config/shortcuts.vdf"))
	o, err := vdf.Unmarshal(data)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, v := range shortcutList(o) {
		if s := v.(*vdf.Object); inside(exePath(s), filepath.Join(e.paths.Games, folder)) {
			for k, v := range fields {
				s.Set(k, v)
			}
		}
	}
	os.WriteFile(filepath.Join(e.user, "config/shortcuts.vdf"), vdf.Marshal(o), 0o644)
}

// Test that edits made inside Steam are kept, unless the config changes the same field
func TestRunKeepsSteamEdits(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	exe := filepath.Join(e.paths.Games, "Windows Game/Windows Game.exe")
	appid := steam.ShortcutAppID(`"`+exe+`"`, "Windows Game")

	// Renamed, new launch options and another Proton picked inside Steam
	e.editShortcut("Windows Game", map[string]string{"AppName": "Steam Name", "LaunchOptions": "%command% -dx11"})
	edit(t, filepath.Join(e.paths.Steam, "config/config.vdf"), `"name"		"proton_9"
						"config"		""
						"priority"		"250"`, `"name"		"GE-Proton10-1"
						"config"		""
						"priority"		"250"`)
	shutdowns := e.steam.shutdowns
	out := e.run(false)
	mustContain(t, "output", out, "  = Steam Name: up to date", "kept from Steam: name, launch options, Proton",
		"Steam already up to date")
	if e.steam.shutdowns != shutdowns {
		t.Error("Steam was closed to keep its own edits")
	}

	// Later runs keep them too, without reporting them again
	out = e.run(false)
	if strings.Contains(out, "kept from Steam") {
		t.Errorf("edit reported again:\n%s", out)
	}
	sc := e.shortcut("Windows Game")
	if name, _ := sc.String("AppName"); name != "Steam Name" {
		t.Errorf("AppName %q", name)
	}
	if opts, _ := sc.String("LaunchOptions"); opts != "%command% -dx11" {
		t.Errorf("LaunchOptions %q", opts)
	}
	if tool, _ := steam.Proton(e.configVDF(), appid); tool != "GE-Proton10-1" {
		t.Errorf("Proton %q", tool)
	}

	// A config change still wins, and one made on both sides warns
	edit(t, e.paths.Config, "[Windows Game]\n", "[Windows Game]\nname = Config Name\nlaunch_options = %command%\n")
	e.editShortcut("Windows Game", map[string]string{"LaunchOptions": "-other"})
	out = e.run(false)
	mustContain(t, "output", out, "  ~ Config Name: updated",
		"! Config Name: launch options changed both in Steam and in the config, the config's value was used")
	sc = e.shortcut("Windows Game")
	if name, _ := sc.String("AppName"); name != "Config Name" {
		t.Errorf("AppName %q", name)
	}
	if opts, _ := sc.String("LaunchOptions"); opts != "%command%" {
		t.Errorf("LaunchOptions %q", opts)
	}
}

// Test that a deleted game is removed only once the removal is confirmed
func TestRunAsksBeforeRemoving(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	exe := filepath.Join(e.paths.Games, "Linux Game/start.sh")
	appid := steam.ShortcutAppID(`"`+exe+`"`, "Linux Game")
	art := filepath.Join(e.paths.Art, "Linux Game")
	os.MkdirAll(art, 0o755)
	os.WriteFile(filepath.Join(art, "hero.jpg"), []byte("custom"), 0o644)

	// Declined: shortcut, artwork and config section all stay, and Steam isn't closed for it
	os.RemoveAll(filepath.Join(e.paths.Games, "Linux Game"))
	e.confirm = func([]string) bool { return false }
	shutdowns := e.steam.shutdowns
	out := e.run(false)
	mustContain(t, "declined output", out, "! Linux Game: game folder deleted, kept in Steam since its removal wasn't confirmed",
		"Done: 0 added, 0 removed")
	if e.steam.shutdowns != shutdowns {
		t.Error("Steam closed with nothing to write")
	}
	if entryAppid(e.shortcut("Linux Game")) != appid {
		t.Error("shortcut removed")
	}
	if _, err := os.Stat(art); err != nil {
		t.Error("artwork of a kept shortcut deleted")
	}
	conf, _ := os.ReadFile(e.paths.Config)
	mustContain(t, "steam-adopt.conf", string(conf), "[Linux Game]")

	// Without anyone to ask, nothing is removed either
	e.confirm = nil
	out = e.run(false)
	mustContain(t, "unattended output", out, "with --yes to remove them")

	// Confirmed: gone
	e.confirm = func([]string) bool { return true }
	out = e.run(false)
	mustContain(t, "confirmed output", out, "- Linux Game: game folder deleted, removed from Steam")
	if len(e.shortcuts()) != 1 {
		t.Errorf("%d shortcuts left", len(e.shortcuts()))
	}
}

// Test that Flatpak Steam and a custom games folder work, with the sandbox warning
func TestRunFlatpakSteamAndOwnGamesFolder(t *testing.T) {
	linuxOnly(t, "Flatpak is Linux only")
	// Flatpak Steam and games kept outside the default folder
	e := newEnv(t)
	flatpak := filepath.Join(e.home, ".var/app", steam.FlatpakID, ".local/share/Steam")
	os.MkdirAll(filepath.Dir(flatpak), 0o755)
	if err := os.Rename(e.paths.Steam, flatpak); err != nil {
		t.Fatal(err)
	}
	games := filepath.Join(e.home, "Games")
	if err := os.Rename(e.paths.Games, games); err != nil {
		t.Fatal(err)
	}
	e.paths = NewPaths(e.home, games)
	e.user = filepath.Join(flatpak, "userdata/390376024")
	if e.paths.Steam != flatpak || e.paths.Config != filepath.Join(games, "_steam-adopt/steam-adopt.conf") {
		t.Fatalf("paths %+v", e.paths)
	}

	out := e.run(false)
	mustContain(t, "output", out, "Flatpak Steam can't see "+games,
		`flatpak override --user --filesystem='`+games+`' `+steam.FlatpakID, "Done: 2 added")
	if len(e.shortcuts()) != 2 || !inside(exePath(e.shortcuts()[0]), games) {
		t.Errorf("shortcuts %v", e.shortcuts())
	}

	// Once shared, the warning is gone
	os.MkdirAll(filepath.Join(e.home, ".local/share/flatpak/overrides"), 0o755)
	os.WriteFile(filepath.Join(e.home, ".local/share/flatpak/overrides", steam.FlatpakID),
		[]byte("[Context]\nfilesystems=~/Games;\n"), 0o644)
	if out := e.run(false); strings.Contains(out, "can't see") {
		t.Errorf("still warned:\n%s", out)
	}

	// Tools in launch options live in the sandbox, so the host's missing ones aren't reported
	edit(t, e.paths.Config, "launch_options =\n", "launch_options = steam-adopt-no-such-tool %command%\n")
	if out := e.run(false); strings.Contains(out, "Not installed") {
		t.Errorf("host tools checked for Flatpak Steam:\n%s", out)
	}
}

// Add a shortcut into a game folder the way Steam would for one added by hand
func (e *env) addShortcut(name, exe string) {
	e.t.Helper()
	path := filepath.Join(e.user, "config/shortcuts.vdf")
	data, _ := os.ReadFile(path)
	o, err := vdf.Unmarshal(data)
	if err != nil {
		e.t.Fatal(err)
	}
	list, _ := o.Get("shortcuts")
	l := list.(*vdf.Object)
	l.Set(strconv.Itoa(l.Len()), newShortcut(steam.ShortcutAppID(quote(exe), name), name, exe))
	os.WriteFile(path, vdf.Marshal(o), 0o644)
}

// Test that duplicate shortcuts into a game folder are removed, keeping the first
func TestRunRemovesDuplicates(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	exe := filepath.Join(e.paths.Games, "Windows Game/Windows Game.exe")
	appid := steam.ShortcutAppID(quote(exe), "Windows Game")
	dup := steam.ShortcutAppID(quote(exe), "Windows Game Copy")
	grid := filepath.Join(e.user, "config/grid")
	os.WriteFile(filepath.Join(grid, itoa(dup)+"p.jpg"), []byte("dup cover"), 0o644)

	// One duplicate with an ID of its own, one with the kept shortcut's ID
	e.addShortcut("Windows Game Copy", exe)
	e.addShortcut("Windows Game", exe)

	// Unconfirmed: the same-ID one goes, the other is kept and reported
	out := e.run(false)
	mustContain(t, "unconfirmed output", out,
		"! Windows Game Copy: duplicate shortcut, kept in Steam since its removal wasn't confirmed")
	if len(e.shortcuts()) != 3 {
		t.Fatalf("%d shortcuts, want the game, its kept duplicate and Linux Game", len(e.shortcuts()))
	}

	// Confirmed: only the first shortcut stays, with its artwork
	var asked []string
	e.confirm = func(names []string) bool { asked = names; return true }
	out = e.run(false)
	if len(asked) != 1 || asked[0] != "Windows Game Copy (duplicate shortcut)" {
		t.Errorf("asked about %v", asked)
	}
	mustContain(t, "confirmed output", out, "- Windows Game Copy: duplicate shortcut, removed from Steam",
		"  = Windows Game: up to date")
	if len(e.shortcuts()) != 2 || entryAppid(e.shortcut("Windows Game")) != appid {
		t.Errorf("shortcuts after removal: %d", len(e.shortcuts()))
	}
	if _, err := os.Stat(filepath.Join(grid, itoa(dup)+"p.jpg")); err == nil {
		t.Error("duplicate's grid file kept")
	}
	if target, _ := os.Readlink(filepath.Join(grid, itoa(appid)+"p.jpg")); target != filepath.Join(e.paths.Art, "Windows Game/cover.jpg") {
		t.Errorf("kept game's cover link -> %q", target)
	}
}

// Test that folder names clashing with config sections are left out
func TestRunLeavesOutClashingFolderNames(t *testing.T) {
	unixOnly(t, "Windows doesn't allow line breaks in folder names")
	e := newEnv(t)
	write(t, filepath.Join(e.paths.Games, "settings/settings.exe"), 1)
	write(t, filepath.Join(e.paths.Games, "DEFAULT/game.exe"), 1)
	write(t, filepath.Join(e.paths.Games, "Two\nLines/game.exe"), 1)
	// Brackets are fine, the header runs to the last ]
	write(t, filepath.Join(e.paths.Games, "Game [GOG]/game.exe"), 1)

	out := e.run(false)
	mustContain(t, "output", out, `! "settings": folder name can't be a section`, `! "DEFAULT":`, `! "Two\nLines":`,
		"  + Game [GOG]: added", "Done: 3 added")
	conf, _ := os.ReadFile(e.paths.Config)
	if strings.Count(string(conf), "[settings]") != 1 || strings.Contains(string(conf), "[DEFAULT]") {
		t.Errorf("clashing section added:\n%s", conf)
	}
	mustContain(t, "steam-adopt.conf", string(conf), "\n[Game [GOG]]\n")

	// The config still reads, and the bracketed game keeps its own section
	edit(t, e.paths.Config, "[Game [GOG]]\n", "[Game [GOG]]\nname = Bracketed\n")
	mustContain(t, "rerun output", e.run(false), "  ~ Bracketed: updated")
}

// Test that only commands run before %command% are checked for
func TestMissingTools(t *testing.T) {
	unixOnly(t, "the tools it looks for are Unix commands")
	// A tool under ~ is looked for in the home folder, the way Steam's shell expands it
	home := t.TempDir()
	t.Setenv("HOME", home)
	write(t, filepath.Join(home, "bin/my-tool"), 1)
	got := missingTools(map[string]bool{
		"~/bin/my-tool %command%":              true,
		"steam-adopt-no-such-tool %command%":   true,
		"A=1 sh -c %command%":                  true,
		"-windowed steam-adopt-not-run-either": true,
		"sh -c 0-3 %command%":                  true,
		"sh -W 1920 -H 1080 -- %command%":      true,
		"":                                     true,
	})
	if len(got) != 1 || got[0] != "steam-adopt-no-such-tool" {
		t.Errorf("got %v", got)
	}
}

// Test that planning leaves the games unchanged until the final plan is applied
func TestBuildPlanLeavesGamesAlone(t *testing.T) {
	// Planned twice, as around closing Steam, the games read the same until the final plan is applied
	e := newEnv(t)
	e.run(false)
	exe := filepath.Join(e.paths.Games, "Windows Game/Windows Game.exe")
	g := &game{folder: filepath.Dir(exe), exe: exe, name: "Windows Game", section: map[string]string{},
		settings: map[string]string{}, proton: "proton_9"}
	a := &App{Paths: e.paths, shortcutsFile: filepath.Join(e.user, "config/shortcuts.vdf"),
		configVDF: filepath.Join(e.paths.Steam, "config/config.vdf")}
	before := *g
	var p *plan
	for range 2 {
		var err error
		if p, err = a.buildPlan([]*game{g}, nil); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*g, before) {
			t.Fatalf("game changed by planning: %+v", *g)
		}
	}
	p.apply([]*game{g})
	if g.status != statusKept || g.appid != steam.ShortcutAppID(quote(exe), "Windows Game") {
		t.Errorf("applied status %q, appid %d", g.status, g.appid)
	}
}

// Test that an exe outside the game folder is refused instead of adding a shortcut on every run
func TestRunRefusesExeOutsideFolder(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	before := len(e.shortcuts())
	other := filepath.Join(e.home, "elsewhere/Game.exe")
	write(t, other, 10)
	edit(t, e.paths.Config, "[No Exe]\n", "[No Exe]\nexe = "+other+"\n")
	edit(t, e.paths.Config, "[Linux Game]\n", "[Linux Game]\nexe = ../Windows Game/Windows Game.exe\n")
	for i := 0; i < 3; i++ {
		out := e.run(false)
		mustContain(t, "output", out, "! No Exe: exe = \""+other+"\" is outside the game folder",
			"! Linux Game: exe = \"../Windows Game/Windows Game.exe\" is outside the game folder")
	}
	if len(e.shortcuts()) != before {
		t.Errorf("%d shortcuts, want %d", len(e.shortcuts()), before)
	}
}

// Test that a games folder whose config is gone, like an unmounted drive's empty mountpoint, removes nothing, artwork included
func TestRunKeepsGamesWhenFolderLooksUnmounted(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	before := len(e.shortcuts())
	art := filepath.Join(e.paths.Art, "Linux Game/cover.jpg")
	write(t, art, 5)
	entries, _ := os.ReadDir(e.paths.Games)
	for _, d := range entries {
		if d.Name() != filepath.Base(e.paths.Sync) {
			os.RemoveAll(filepath.Join(e.paths.Games, d.Name()))
		}
	}
	os.Remove(e.paths.Config)
	e.confirm = func([]string) bool { return true }
	out := e.run(false)
	mustContain(t, "output", out, "2 shortcut(s) point into missing game folders but were kept", "0 removed")
	if len(e.shortcuts()) != before {
		t.Errorf("%d shortcuts left, want %d", len(e.shortcuts()), before)
	}
	if _, err := os.Stat(art); err != nil {
		t.Error("artwork of a kept shortcut deleted")
	}

	// With the config back in place, a second run treats the folders as really deleted
	out = e.run(false)
	mustContain(t, "second output", out, "2 removed")
}

// Test that a game folder that's a symlink to a missing drive isn't taken for deleted
func TestRunKeepsGameBehindDanglingSymlink(t *testing.T) {
	e := newEnv(t)
	drive := filepath.Join(e.home, "drive/Linux Game")
	write(t, filepath.Join(drive, "start.sh"), 10)
	os.RemoveAll(filepath.Join(e.paths.Games, "Linux Game"))
	if err := os.Symlink(drive, filepath.Join(e.paths.Games, "Linux Game")); err != nil {
		t.Fatal(err)
	}
	e.run(false)
	before := len(e.shortcuts())
	art := filepath.Join(e.paths.Art, "Linux Game")
	write(t, filepath.Join(art, "hero.jpg"), 1)

	// Drive unmounted
	os.RemoveAll(filepath.Join(e.home, "drive"))
	e.confirm = func([]string) bool { return true }
	out := e.run(false)
	if strings.Contains(out, "removed from Steam") || len(e.shortcuts()) != before {
		t.Errorf("game behind a dangling symlink removed:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(art, "hero.jpg")); err != nil {
		t.Error("artwork of a game behind a dangling symlink deleted")
	}
}

// Test that Steam is started again when writing its files fails after steam-adopt closed it
func TestRunRestartsSteamAfterFailedWrite(t *testing.T) {
	unixOnly(t, "Windows lets a read-only folder be written to")
	if os.Getuid() == 0 {
		t.Skip("root can write to read-only folders")
	}
	e := newEnv(t)
	e.run(false)
	// Only config.vdf changes, so the backup made before closing Steam succeeds and the write fails
	cfg := filepath.Join(e.paths.Steam, "config")
	edit(t, e.paths.Config, "[Windows Game]\n", "[Windows Game]\nproton = none\n")
	os.Chmod(cfg, 0o555)
	defer os.Chmod(cfg, 0o755)
	e.steam.running = true
	starts := e.steam.starts

	var out bytes.Buffer
	if err := e.app(&out).Run(context.Background()); err == nil {
		t.Fatal("Run succeeded writing into a read-only folder")
	}
	if e.steam.starts != starts+1 || !e.steam.running {
		t.Errorf("Steam not started again after the failed write:\n%s", out.String())
	}
}

// Round tripper counting the store searches made
type countingTransport struct {
	mu       sync.Mutex
	searches int
	next     http.RoundTripper
}

// Count a store search and pass the request on
func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/api/storesearch/" {
		c.mu.Lock()
		c.searches++
		c.mu.Unlock()
	}
	return c.next.RoundTrip(r)
}

// Test that a game the store doesn't know isn't searched for again on every run
func TestRunRemembersGamesNotOnTheStore(t *testing.T) {
	e := newEnv(t)
	ct := &countingTransport{next: e.store.HTTP.Transport}
	e.store.HTTP.Transport = ct
	e.run(false)
	first := ct.searches
	out := e.run(false)
	mustContain(t, "rerun output", out, "not found on Steam (checked in the last week)")
	if ct.searches != first {
		t.Errorf("rerun made %d more searches", ct.searches-first)
	}

	// An old record is searched again
	marker := filepath.Join(e.paths.Art, "Linux Game/.not_found")
	old := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(marker, old, old)
	e.run(false)
	if ct.searches != first+1 {
		t.Errorf("%d searches after the record expired, want 1", ct.searches-first)
	}
}

// Test that a release tag in a folder name is left out of the store search and the exe guess
func TestRunUntagsFolderNames(t *testing.T) {
	e := newEnv(t)
	tagged := filepath.Join(e.paths.Games, "Windows Game [GOG] (2019)")
	if err := os.Rename(filepath.Join(e.paths.Games, "Windows Game"), tagged); err != nil {
		t.Fatal(err)
	}
	out := e.run(false)
	mustContain(t, "output", out, "Windows Game [GOG] (2019): added (Windows Game.exe)", "Steam ID 100 (Windows Game)")
}

// Test that shell quoting survives quotes in a path
func TestShellQuote(t *testing.T) {
	if got := shellQuote("/a b/it's"); got != `'/a b/it'\''s'` {
		t.Errorf("got %s", got)
	}
}

// Test that skipping a game removes its grid files but keeps its artwork folder for when it's added back
func TestRunSkipKeepsArtworkFolder(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	exe := filepath.Join(e.paths.Games, "Linux Game/start.sh")
	appid := steam.ShortcutAppID(quote(exe), "Linux Game")
	custom := filepath.Join(e.paths.Art, "Linux Game/cover.jpg")
	write(t, custom, 5)
	e.run(false)
	grid := filepath.Join(e.user, "config/grid", itoa(appid)+"p.jpg")
	if !fsutil.IsSymlink(grid) {
		t.Fatal("cover not linked before skipping")
	}

	// A hero set from inside Steam right before skipping is kept too, as a real file in the grid
	hero := filepath.Join(e.user, "config/grid", itoa(appid)+"_hero.jpg")
	os.WriteFile(hero, []byte("set in steam"), 0o644)
	edit(t, e.paths.Config, "[Linux Game]\n", "[Linux Game]\nskip = yes\n")
	e.confirm = func([]string) bool { return true }
	mustContain(t, "skip output", e.run(false), "- Linux Game: skipped, removed from Steam")
	if _, err := os.Stat(custom); err != nil {
		t.Error("custom artwork of a skipped game deleted")
	}
	if b, _ := os.ReadFile(filepath.Join(e.paths.Art, "Linux Game/hero.jpg")); string(b) != "set in steam" {
		t.Error("picture set in Steam for a skipped game deleted")
	}
	if fsutil.IsSymlink(grid) {
		t.Error("skipped game's grid link kept")
	}
}

// Test that a native launcher without the execute bit is reported
func TestRunWarnsAboutNonExecutableLauncher(t *testing.T) {
	unixOnly(t, "Windows files have no execute bit")
	e := newEnv(t)
	os.Chmod(filepath.Join(e.paths.Games, "Linux Game/start.sh"), 0o644)
	mustContain(t, "output", e.run(false), "! Linux Game: start.sh isn't executable, so Steam can't start it; fix with: chmod +x '")
	os.Chmod(filepath.Join(e.paths.Games, "Linux Game/start.sh"), 0o755)
	if out := e.run(false); strings.Contains(out, "isn't executable") {
		t.Errorf("warned about an executable launcher:\n%s", out)
	}
}

// Test that failing to record a store search is reported in the game's notes, not written from the parallel lookups
func TestRunReportsUnrecordedSearch(t *testing.T) {
	e := newEnv(t)
	// The artwork folder can't be created, so no record can be written for either game
	write(t, e.paths.Art, 0)
	out := e.run(false)
	mustContain(t, "output", out, "couldn't remember the search")
}

// Test that renaming a game folder keeps its shortcut ID, and so its Proton prefix, along with its settings and artwork
func TestRunFollowsRenamedFolder(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	appid := entryAppid(e.shortcut("Windows Game"))
	edit(t, e.paths.Config, "[Windows Game]\n", "[Windows Game]\nlaunch_options = -dx11\n")
	e.run(false)
	write(t, filepath.Join(e.paths.Art, "Windows Game/logo.png"), 3)

	if err := os.Rename(filepath.Join(e.paths.Games, "Windows Game"), filepath.Join(e.paths.Games, "Windows Game [GOG]")); err != nil {
		t.Fatal(err)
	}
	asked := false
	e.confirm = func([]string) bool { asked = true; return true }

	// A dry run says so and changes nothing
	if out := e.run(true); !strings.Contains(out, "~ Windows Game [GOG]: renamed from Windows Game, would keep") {
		t.Errorf("dry run output:\n%s", out)
	}
	out := e.run(false)
	mustContain(t, "output", out, "~ Windows Game [GOG]: renamed from Windows Game, keeping its shortcut, settings and artwork",
		"Done: 0 added, 0 removed")
	if asked {
		t.Error("asked to remove the renamed game")
	}
	sc := e.shortcut("Windows Game [GOG]")
	if entryAppid(sc) != appid || len(e.shortcuts()) != 2 {
		t.Errorf("appid %d, want %d; %d shortcuts", entryAppid(sc), appid, len(e.shortcuts()))
	}
	if lo, _ := sc.String("LaunchOptions"); lo != "-dx11" {
		t.Errorf("launch options %q, want the old section's", lo)
	}
	conf, _ := os.ReadFile(e.paths.Config)
	mustContain(t, "steam-adopt.conf", string(conf), "[Windows Game [GOG]]\nlaunch_options = -dx11\n")
	if strings.Contains(string(conf), "[Windows Game]\n") {
		t.Error("old section kept")
	}
	if _, err := os.Stat(filepath.Join(e.paths.Art, "Windows Game [GOG]/logo.png")); err != nil {
		t.Error("artwork not moved with the folder")
	}

	// Settled: the next run has nothing to do
	if out := e.run(false); !strings.Contains(out, "Steam already up to date") || strings.Contains(out, "renamed") {
		t.Errorf("rerun output:\n%s", out)
	}
}

// Test that a rename is only assumed for a related name with the same exe, and never when two folders could be it
func TestRunLeavesUnclearRenamesAlone(t *testing.T) {
	for name, folders := range map[string][]string{
		"unrelated name":         {"Other Thing"},
		"a sequel":               {"Windows Game 2"},
		"two possible new names": {"Windows Game [GOG]", "Windows Game v2.0"},
	} {
		e := newEnv(t)
		e.run(false)
		appid := entryAppid(e.shortcut("Windows Game"))
		for _, f := range folders {
			write(t, filepath.Join(e.paths.Games, f, "Windows Game.exe"), 100)
		}
		os.RemoveAll(filepath.Join(e.paths.Games, "Windows Game"))
		out := e.run(false)
		if strings.Contains(out, "renamed from") {
			t.Errorf("%s: taken for a rename:\n%s", name, out)
		}
		mustContain(t, name, out, "! Windows Game: game folder deleted, kept in Steam since its removal wasn't confirmed")
		for _, f := range folders {
			if entryAppid(e.shortcut(f)) == appid {
				t.Errorf("%s: %s took over the old shortcut", name, f)
			}
		}
	}
}

// Test that quotes around exe, a store URL as steam_id and an uninstalled Proton are handled or reported
func TestRunConfigValuesAsUsersWriteThem(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	// Too deep to be found on its own, so only the quoted exe can point at it
	write(t, filepath.Join(e.paths.Games, "No Exe/bin/x/y/My Game.x86_64"), 1)
	os.Chmod(filepath.Join(e.paths.Games, "No Exe/bin/x/y/My Game.x86_64"), 0o755)
	edit(t, e.paths.Config, "[No Exe]\n", "[No Exe]\nexe = \"bin/x/y/My Game.x86_64\"\n"+
		"steam_id = https://store.steampowered.com/app/100/Windows_Game/\nproton = GE-Proton99-1\n")
	edit(t, e.paths.Config, "[Linux Game]\n", "[Linux Game]\nexe = missing.sh\nsteam_id = abc\n")
	out := e.run(false)
	mustContain(t, "output", out, "+ No Exe: added (My Game.x86_64)", "artwork: Steam ID 100 (from config)",
		"! No Exe: proton = GE-Proton99-1 isn't installed in compatibilitytools.d",
		"! Linux Game: exe = missing.sh isn't a file in the game folder")

	// An installed custom tool isn't reported
	write(t, filepath.Join(e.paths.Steam, "compatibilitytools.d/GE-Proton99-1/compatibilitytool.vdf"), 1)
	if out := e.run(false); strings.Contains(out, "isn't installed") {
		t.Errorf("installed tool reported:\n%s", out)
	}
}

// Test that Ctrl-C at the removal question stops the run before Steam's files are touched
func TestRunStopsWhenInterruptedAtQuestion(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	os.RemoveAll(filepath.Join(e.paths.Games, "Linux Game"))
	// Another change, so the run would write Steam's files after the question
	edit(t, e.paths.Config, "[Windows Game]\n", "[Windows Game]\nname = Renamed\n")
	path := filepath.Join(e.user, "config/shortcuts.vdf")
	before, _ := os.ReadFile(path)
	e.steam.running = false

	ctx, cancel := context.WithCancel(context.Background())
	e.confirm = func([]string) bool { cancel(); return false }
	if err := e.app(io.Discard).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("shortcuts.vdf written after Ctrl-C")
	}
}

// Steam client whose command can't be found
type noCommandSteam struct{ stuckSteam }

// Fail like a steam command missing from PATH
func (noCommandSteam) Shutdown(context.Context) error {
	return &exec.Error{Name: "steam", Err: exec.ErrNotFound}
}

// Test that a missing steam command fails at once instead of waiting out the timeout
func TestCloseSteamWithoutCommand(t *testing.T) {
	a := &App{Steam: noCommandSteam{}, Out: io.Discard, CloseTimeout: time.Hour, PollInterval: time.Hour}
	start := time.Now()
	if err := a.closeSteam(context.Background()); err == nil || !strings.Contains(err.Error(), "no steam command found") {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("waited for a Steam that can't be closed")
	}
}

// Test that a native binary without an extension is found by its ELF header, and other plain files aren't
func TestFindExeExtensionlessBinary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Celeste")
	elf := append([]byte("\x7fELF"), make([]byte, 64)...)
	os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	os.WriteFile(filepath.Join(dir, "LICENSE"), make([]byte, 999), 0o644)
	os.WriteFile(filepath.Join(dir, "Celeste"), elf, 0o755)
	write(t, filepath.Join(dir, "Celeste.exe"), 9999)
	if got := findExe(dir); got != filepath.Join(dir, "Celeste") {
		t.Errorf("got %q, want the ELF binary", got)
	}
	os.Remove(filepath.Join(dir, "Celeste"))
	os.Remove(filepath.Join(dir, "Celeste.exe"))
	if got := findExe(dir); got != "" {
		t.Errorf("plain file taken for a game: %q", got)
	}
}

// Test that folder names differing only in tags or version count as the same game for a rename
func TestRelated(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"Game", "Game [GOG]", true},
		{"Game v1.0.3", "Game v1.0.4", true},
		{"game", "GAME (2019)", true},
		{"Saga", "Saga 2", false},
		{"Saga", "Saga Deluxe", false},
		{"[GOG]", "[GOG]", true},
	} {
		if got := related(tc.a, tc.b); got != tc.want {
			t.Errorf("related(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// Test that moving the games folder, like to an SD card, keeps every shortcut and its ID instead of adding them again
func TestRunFollowsMovedGamesFolder(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	ids := map[string]uint32{}
	for _, s := range e.shortcuts() {
		name, _ := s.String("AppName")
		ids[name] = entryAppid(s)
	}
	moved := filepath.Join(e.home, "sdcard/Games")
	os.MkdirAll(filepath.Dir(moved), 0o755)
	if err := os.Rename(e.paths.Games, moved); err != nil {
		t.Fatal(err)
	}
	e.paths = withGames(e.paths, moved)

	out := e.run(false)
	mustContain(t, "output", out, "~ Windows Game: moved from "+filepath.Join(e.home, ".local/share/games/Windows Game"),
		"~ Linux Game: moved from", "Done: 0 added, 0 removed")
	sc := e.shortcuts()
	if len(sc) != 2 {
		t.Fatalf("got %d shortcuts, want the 2 old ones", len(sc))
	}
	for _, s := range sc {
		name, _ := s.String("AppName")
		exe, _ := s.String("Exe")
		start, _ := s.String("StartDir")
		in := `"` + moved + string(filepath.Separator)
		if entryAppid(s) != ids[name] || !strings.HasPrefix(exe, in) || !strings.HasPrefix(start, in) {
			t.Errorf("%s: appid %d (want %d), exe %s, start %s", name, entryAppid(s), ids[name], exe, start)
		}
	}
	if out := e.run(false); !strings.Contains(out, "Steam already up to date") || strings.Contains(out, "moved from") {
		t.Errorf("rerun output:\n%s", out)
	}
}

// Test that Steam isn't closed in Game Mode, where it would start again at once, and nothing is written
func TestRunRefusesGameMode(t *testing.T) {
	e := newEnv(t)
	e.steam.gameMode = true
	var out bytes.Buffer
	if err := e.app(&out).Run(context.Background()); !errors.Is(err, ErrGameMode) {
		t.Fatalf("got %v, want ErrGameMode\n%s", err, out.String())
	}
	if e.steam.shutdowns != 0 {
		t.Error("Steam was closed in Game Mode")
	}
	if _, err := os.Stat(filepath.Join(e.user, "config/shortcuts.vdf")); err == nil {
		t.Error("shortcuts.vdf written in Game Mode")
	}
	// A dry run still works
	if out := e.run(true); !strings.Contains(out, "Dry run, nothing changed: 2 added") {
		t.Errorf("dry run:\n%s", out)
	}

	// A refused run leaves no backup behind, so it can't push the older ones out
	e.steam.gameMode = false
	e.run(false)
	e.steam.gameMode = true
	edit(t, e.paths.Config, "[Windows Game]\n", "[Windows Game]\nname = Renamed\n")
	backups := func() []string {
		b, _ := filepath.Glob(filepath.Join(e.user, "config/shortcuts.vdf.steam-adopt-*.bak"))
		return b
	}
	before := backups()
	if err := e.app(io.Discard).Run(context.Background()); !errors.Is(err, ErrGameMode) {
		t.Fatalf("got %v, want ErrGameMode", err)
	}
	if after := backups(); len(after) != len(before) {
		t.Errorf("backups before %v, after %v", before, after)
	}
}

// Test that Ctrl-C while Steam is closing writes nothing and starts Steam again, unless restart_steam = no
func TestRunInterruptedWhileClosingSteam(t *testing.T) {
	for _, restart := range []bool{true, false} {
		e := newEnv(t)
		if !restart {
			os.MkdirAll(e.paths.Sync, 0o755)
			os.WriteFile(e.paths.Config, []byte("[settings]\nrestart_steam = no\n"), 0o644)
		}
		ctx, cancel := context.WithCancel(context.Background())
		e.steam.onShutdown = cancel
		if err := e.app(io.Discard).Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("restart %v: got %v, want context.Canceled", restart, err)
		}
		if _, err := os.Stat(filepath.Join(e.user, "config/shortcuts.vdf")); err == nil {
			t.Errorf("restart %v: shortcuts.vdf written after Ctrl-C", restart)
		}
		if want := map[bool]int{true: 1, false: 0}[restart]; e.steam.starts != want {
			t.Errorf("restart %v: Steam started %d times, want %d", restart, e.steam.starts, want)
		}
	}
}

// Test that steam_id takes digits or a store page URL, and nothing that can't be a store ID
func TestParseSteamID(t *testing.T) {
	for in, want := range map[string]int64{
		"100": 100, `"100"`: 100, "store.steampowered.com/app/100": 100,
		"https://store.steampowered.com/app/100/Game/?l=en": 100,
		"0": 0, "-5": 0, "abc": 0, "": 0, "https://example.com/app/100": 0,
	} {
		id, ok := parseSteamID(in)
		if id != want && ok || ok != (want != 0) {
			t.Errorf("%q: got %d, %v, want %d", in, id, ok, want)
		}
	}
}

// Test that pictures set in Steam are linked back into its grid folder when the run stops early, as in Game Mode
func TestRunKeepsSteamArtworkWhenStoppedEarly(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	cover := filepath.Join(e.user, "config/grid", itoa(entryAppid(e.shortcuts()[1]))+"p.jpg")
	os.Remove(cover)
	os.WriteFile(cover, []byte("set in Steam"), 0o644)
	e.steam.gameMode = true
	edit(t, e.paths.Config, "[Windows Game]\n", "[Windows Game]\nname = Renamed\n")
	if err := e.app(io.Discard).Run(context.Background()); !errors.Is(err, ErrGameMode) {
		t.Fatalf("got %v, want ErrGameMode", err)
	}
	if b, err := os.ReadFile(cover); err != nil || string(b) != "set in Steam" {
		t.Errorf("cover set in Steam lost from the grid folder: %q, %v", b, err)
	}
}

// Test that Steam isn't closed to write an empty shortcut list when it has none and there's nothing to add
func TestRunLeavesSteamAloneWithNothingToAdd(t *testing.T) {
	e := newEnv(t)
	os.RemoveAll(filepath.Join(e.paths.Games, "Windows Game"))
	os.RemoveAll(filepath.Join(e.paths.Games, "Linux Game"))
	out := e.run(false)
	mustContain(t, "output", out, "Done: 0 added, 0 removed, Steam already up to date")
	if e.steam.shutdowns != 0 {
		t.Error("Steam was closed with nothing to write")
	}
	if _, err := os.Stat(filepath.Join(e.user, "config/shortcuts.vdf")); err == nil {
		t.Error("an empty shortcuts.vdf was written")
	}
}

// Test that a run without Steam installed stops before creating anything in the games folder
func TestRunWithoutSteamCreatesNothing(t *testing.T) {
	e := newEnv(t)
	os.RemoveAll(e.paths.Steam)
	a := e.app(io.Discard)
	err := a.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "install Steam, log in once") {
		t.Errorf("got %v", err)
	}
	if _, err := os.Stat(e.paths.Sync); err == nil {
		t.Error("steam-adopt's folder created without Steam")
	}

	// A missing games folder says how to start
	a.Paths = withGames(e.paths, filepath.Join(e.home, "nope"))
	if err := a.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "create it, put each game in its own folder") {
		t.Errorf("missing games folder: %v", err)
	}

	// A games path that's a file says what --games wants
	file := filepath.Join(e.home, "games.txt")
	write(t, file, 1)
	a.Paths = withGames(e.paths, file)
	if err := a.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "isn't a folder, pass the folder holding your game folders") {
		t.Errorf("games path that's a file: %v", err)
	}
}

// Run a test as if on another system
func onSystem(t *testing.T, system string) {
	old := goos
	goos = system
	t.Cleanup(func() { goos = old })
}

// Test that on Windows games run as they are: no Proton anywhere, shell scripts aren't games, and no chmod hint
func TestRunOnWindows(t *testing.T) {
	onSystem(t, "windows")
	e := newEnv(t)
	before := e.configVDF()
	out := e.run(false)
	mustContain(t, "output", out, "+ Windows Game: added (Windows Game.exe)", "? Linux Game: no executable found",
		"Done: 1 added")
	if strings.Contains(out, "Proton") || strings.Contains(out, "chmod") {
		t.Errorf("Proton or chmod mentioned on Windows:\n%s", out)
	}
	if e.configVDF() != before {
		t.Error("config.vdf changed on Windows")
	}
	conf, _ := os.ReadFile(e.paths.Config)
	if strings.Contains(string(conf), "\n# proton =") {
		t.Errorf("a game section suggests Proton on Windows:\n%s", conf)
	}
	if DefaultPaths(e.home).Games != filepath.Join(e.home, "Games") {
		t.Error("default games folder isn't ~/Games")
	}
}

// Test that on macOS an app bundle is the game, nothing inside it is looked at, and Windows builds aren't games
func TestFindExeOnMac(t *testing.T) {
	onSystem(t, "darwin")
	dir := filepath.Join(t.TempDir(), "My Game")
	write(t, filepath.Join(dir, "My Game.app/Contents/MacOS/My Game"), 10)
	write(t, filepath.Join(dir, "My Game.app/Contents/Resources/run.sh"), 10)
	write(t, filepath.Join(dir, "My Game.exe"), 999)
	if got := findExe(dir); got != filepath.Join(dir, "My Game.app") {
		t.Errorf("got %q, want the app bundle", got)
	}
	if !launchable(filepath.Join(dir, "My Game.app")) {
		t.Error("app bundle not launchable")
	}
	os.RemoveAll(filepath.Join(dir, "My Game.app"))
	if got := findExe(dir); got != "" {
		t.Errorf("Windows build taken for a Mac game: %q", got)
	}
}

// Point paths at another games folder, keeping the Steam folder
func withGames(p Paths, games string) Paths {
	steamDir := p.Steam
	p = NewPaths("", games)
	p.Steam = steamDir
	return p
}

// Skip a test that relies on how Unix systems behave when it runs on Windows
func unixOnly(t *testing.T, why string) {
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
}

// Skip a test that relies on something only Linux has, like Flatpak, on other systems
func linuxOnly(t *testing.T, why string) {
	if runtime.GOOS != "linux" {
		t.Skip(why)
	}
}

// Test that a mistake in the config stops the run before Steam is touched, naming the file and the line
func TestRunReportsBrokenConfig(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	edit(t, e.paths.Config, "[Linux Game]\n", "[Linux Game]\nthis line has no equals sign\n")
	path := filepath.Join(e.user, "config/shortcuts.vdf")
	before, _ := os.ReadFile(path)
	shutdowns := e.steam.shutdowns
	err := e.app(io.Discard).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with a broken config")
	}
	conf, _ := os.ReadFile(e.paths.Config)
	line := strings.Count(string(conf[:strings.Index(string(conf), "this line")]), "\n") + 1
	// Printed as a sentence, the error itself starting lowercase like Go's do
	mustContain(t, "error", Sentence(err), "Error in "+e.paths.Config+":\n", fmt.Sprintf("[line %2d]", line), "can't parse")
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) || e.steam.shutdowns != shutdowns {
		t.Error("Steam closed or its files changed with a broken config")
	}
}

// Test that the account synced is named when several share the computer, and only then
func TestRunNamesSharedAccount(t *testing.T) {
	e := newEnv(t)
	if out := e.run(true); strings.Contains(out, "Steam account:") {
		t.Errorf("single account named:\n%s", out)
	}
	os.WriteFile(filepath.Join(e.paths.Steam, "config/loginusers.vdf"), []byte(`"users"
{
	"76561197960265729"
	{
		"PersonaName"		"Kid"
		"MostRecent"		"0"
	}
	"76561198350641752"
	{
		"PersonaName"		"Parent"
		"MostRecent"		"1"
	}
}
`), 0o644)
	mustContain(t, "output", e.run(false), "Steam account: Parent (the last one logged in)", "Done: 2 added")
	if len(e.shortcuts()) != 2 {
		t.Error("shortcuts not written to the most recent account")
	}
}

// Test that without Steam Play turned on, Proton is left alone and the user is told how to turn it on
func TestRunWithoutSteamPlay(t *testing.T) {
	e := newEnv(t)
	noMapping := "\"InstallConfigStore\"\n{\n}\n"
	os.WriteFile(filepath.Join(e.paths.Steam, "config/config.vdf"), []byte(noMapping), 0o644)
	mustContain(t, "output", e.run(false), "+ Windows Game: added",
		"! Proton mapping not found in Steam's config.vdf, enable Steam Play in Steam's settings once")
	if e.configVDF() != noMapping {
		t.Errorf("config.vdf changed:\n%s", e.configVDF())
	}
}

// Test that on Windows a failed artwork link says to turn on Developer Mode, without failing the sync
func TestRunOnWindowsSuggestsDeveloperMode(t *testing.T) {
	onSystem(t, "windows")
	e := newEnv(t)
	// The grid folder can't be made, as when symbolic links aren't allowed
	write(t, filepath.Join(e.user, "config/grid"), 0)
	mustContain(t, "output", e.run(false), "+ Windows Game: added",
		"! To link artwork into Steam, turn on Developer Mode in Windows' settings")
	if len(e.shortcuts()) != 1 {
		t.Errorf("%d shortcuts, want the Windows game", len(e.shortcuts()))
	}
}

// Report whether shortcuts.vdf has a shortcut with this name
func (e *env) hasShortcut(name string) bool {
	e.t.Helper()
	for _, s := range e.shortcuts() {
		if appName(s, "") == name {
			return true
		}
	}
	return false
}

// Test that shortcuts Steam empties while closing are kept, and that the list it had open is backed up first
func TestRunKeepsShortcutsSteamEmptiesWhileClosing(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	e.addShortcut("Outside Game", "/opt/outside/run")
	write(t, filepath.Join(e.paths.Games, "New Game/New Game.exe"), 100)
	path := filepath.Join(e.user, "config/shortcuts.vdf")
	before, _ := os.ReadFile(path)
	e.steam.running = true
	e.steam.onShutdown = func() {
		empty := vdf.NewObject()
		empty.Set("shortcuts", vdf.NewObject())
		os.WriteFile(path, vdf.Marshal(empty), 0o644)
	}

	out := e.run(false)
	mustContain(t, "output", out, "Steam emptied its shortcuts while closing, the 3 it had are kept")
	for _, name := range []string{"Outside Game", "Windows Game", "Linux Game", "New Game"} {
		if !e.hasShortcut(name) {
			t.Errorf("%s missing after Steam emptied its shortcuts:\n%s", name, out)
		}
	}
	saved := false
	for _, b := range fsutil.GlobDir(filepath.Dir(path), "shortcuts.vdf.steam-adopt-*.bak") {
		if data, _ := os.ReadFile(b); bytes.Equal(data, before) {
			saved = true
		}
	}
	if !saved {
		t.Error("no backup of the shortcuts Steam had open")
	}
}

// Test that shortcuts Steam drops while closing are named with their backup and not put back, as Steam may have removed them
func TestRunReportsShortcutsSteamDropsWhileClosing(t *testing.T) {
	e := newEnv(t)
	e.run(false)
	e.addShortcut("Outside Game", "/opt/outside/run")
	path := filepath.Join(e.user, "config/shortcuts.vdf")
	flushed, _ := os.ReadFile(path)
	e.addShortcut("Dropped Game", "/opt/dropped/run")
	write(t, filepath.Join(e.paths.Games, "New Game/New Game.exe"), 100)
	e.steam.running = true
	e.steam.onShutdown = func() { os.WriteFile(path, flushed, 0o644) }

	out := e.run(false)
	mustContain(t, "output", out, "Steam dropped 1 shortcut(s) while closing: Dropped Game;", ".bak over "+path)
	if e.hasShortcut("Dropped Game") || !e.hasShortcut("Outside Game") || !e.hasShortcut("New Game") {
		t.Errorf("wrong shortcuts after Steam dropped one:\n%s", out)
	}
}
