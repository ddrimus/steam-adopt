// Sync of every game folder into Steam as a non-Steam shortcut with Proton, launch options and artwork
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ddrimus/steam-adopt/internal/artwork"
	"github.com/ddrimus/steam-adopt/internal/config"
	"github.com/ddrimus/steam-adopt/internal/fsutil"
	"github.com/ddrimus/steam-adopt/internal/ini"
	"github.com/ddrimus/steam-adopt/internal/state"
	"github.com/ddrimus/steam-adopt/internal/steam"
	"github.com/ddrimus/steam-adopt/internal/store"
)

// One sync run and what it needs from the outside world
type App struct {
	Paths Paths
	Steam steam.Controller
	Store *store.Client
	// Progress and the summary
	Out io.Writer
	// Problems that don't stop the run
	Warn io.Writer
	// Show what would change without touching anything
	DryRun bool
	// Ask whether the listed games may be removed from Steam, nil keeps them all
	Confirm func(names []string) bool

	// Waiting for Steam to close: total timeout, poll interval, helper flush delay; zero uses the defaults
	CloseTimeout time.Duration
	PollInterval time.Duration
	SettleDelay  time.Duration

	cfg           *ini.File
	state         *state.State
	settings      map[string]string
	grid          string
	shortcutsFile string
	configVDF     string
	autoProton    string
	// steam-adopt.conf didn't exist before this run, as when the games folder is an empty mountpoint
	fresh bool
	// Whether each compatibility tool named in the config is installed
	tools map[string]bool
	// shortcuts.vdf as backed up before closing Steam, and the list to plan from when Steam emptied it while closing
	backedUp []byte
	restore  []byte
}

// Error for when Steam is still running after being asked to shut down
var ErrSteamDidNotClose = errors.New("steam didn't close, close it manually and run steam-adopt again")

// Error for when Steam would have to close in Game Mode, which starts it again right away
var ErrGameMode = errors.New("steam is in Game Mode, where it starts again as soon as it's closed, so its files can't be " +
	"changed safely: switch to Desktop Mode and run steam-adopt again (steam-adopt -n shows the changes anywhere)")

// Prepare every game while Steam keeps running, closing Steam only if its files actually need to change
func (a *App) Run(ctx context.Context) error {
	// Steam account, config and the default Proton, nothing created before both folders are known to be there
	if !fsutil.IsDir(a.Paths.Games) {
		if fsutil.Exists(a.Paths.Games) {
			return fmt.Errorf("%s isn't a folder, pass the folder holding your game folders with --games", a.Paths.Games)
		}
		return fmt.Errorf("games folder %s doesn't exist yet: create it, put each game in its own folder inside it, "+
			"and run steam-adopt again (or use --games DIR)", a.Paths.Games)
	}
	user, err := steam.ActiveUserdata(a.Paths.Steam)
	if err != nil {
		return err
	}
	if name := steam.SharedAccountName(a.Paths.Steam); name != "" {
		fmt.Fprintf(a.Out, "Steam account: %s (the last one logged in)\n", name)
	}
	if err := a.loadConfig(); err != nil {
		return err
	}
	a.grid = filepath.Join(user, "config/grid")
	a.shortcutsFile = filepath.Join(user, "config/shortcuts.vdf")
	a.configVDF = filepath.Join(a.Paths.Steam, "config/config.vdf")
	if a.state, err = state.Load(a.Paths.State); err != nil {
		fmt.Fprintf(a.Warn, "  ! %s\n", Sentence(err))
	}
	// The Flatpak client's sandbox hides the home folder unless it's shared, and its shortcuts then won't start
	if steam.IsFlatpak(a.Paths.Steam) && !steam.FlatpakCanSee(a.Paths.Steam, a.Paths.Games) {
		fmt.Fprintf(a.Warn, "  ! Flatpak Steam can't see %s, so its games won't start; share it with:\n"+
			"      flatpak override --user --filesystem=%s %s\n", a.Paths.Games, shellQuote(a.Paths.Games), steam.FlatpakID)
	}
	a.autoProton = steam.FallbackProton
	if raw, err := os.ReadFile(a.configVDF); err == nil {
		a.autoProton = steam.DefaultProton(string(raw))
	}

	// Games and the shortcut IDs Steam already has for them
	games, err := a.games()
	if err != nil {
		return err
	}
	_, data, err := readShortcuts(a.shortcutsFile)
	if err != nil {
		return err
	}
	// Folders renamed since the last run take over their old shortcut, so the Proton prefix and its saves stay
	for _, r := range a.renames(games, shortcutList(data)) {
		a.adoptRename(r.g, r.old)
	}
	for _, m := range a.moves(games, shortcutList(data)) {
		m.g.movedFrom = m.old
		fmt.Fprintf(a.Out, "  ~ %s: moved from %s, keeping its shortcut and Proton prefix\n", m.g.folderName(), m.old)
	}
	known := map[string]uint32{}
	for _, e := range shortcutList(data) {
		for _, g := range games {
			if _, ok := known[g.folder]; !ok && a.owns(g, exePath(e)) {
				known[g.folder] = shortcutID(e)
			}
		}
	}

	// Everything that's safe while Steam runs: exe, Proton, artwork
	for _, g := range games {
		a.prepareGame(g, known)
	}
	// The lookups move pictures set in Steam out of its grid folder, so they're linked back even when the run stops early
	synced := false
	defer func() {
		if !synced && !a.DryRun {
			a.linkArtwork(games)
		}
	}()
	if err := a.fetchAll(ctx, games); err != nil {
		return err
	}

	// In folder order once every lookup is done, so the config reads the same however the lookups finished
	if !a.DryRun {
		for _, g := range games {
			if g.isNew {
				if err := a.appendSection(g); err != nil {
					return fmt.Errorf("can't write %s: %w", a.Paths.Config, err)
				}
			}
		}
	}

	// Removals only with the user's yes, asked before Steam is closed
	p, err := a.buildPlan(games, nil)
	if err != nil {
		return err
	}
	approved := a.approve(p.pending)

	// Ctrl-C at the question stops the run before anything is closed or written
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(approved) > 0 {
		if p, err = a.buildPlan(games, approved); err != nil {
			return err
		}
	}

	// Steam rewrites its files on exit, so close it only to write, then replan from what it flushed
	changed := p.changed()
	closed := false
	if changed && !a.DryRun {
		if a.Steam.Running() {
			// Checked before the backup, so runs refused in Game Mode don't push older backups out
			if a.Steam.GameMode() {
				return ErrGameMode
			}
			// Steam can flush an empty or shorter list while closing, so keep the one it has open first
			backup := ""
			if len(p.original) > 0 {
				if backup, err = fsutil.Backup(a.shortcutsFile, backups, time.Now()); err != nil {
					return fmt.Errorf("can't back up %s: %w", a.shortcutsFile, err)
				}
				a.backedUp = p.original
			}
			if err := a.closeSteam(ctx); err != nil {
				// Ctrl-C while Steam was closing: nothing was written, so it comes back like after a failed write
				if ctx.Err() != nil && config.Yes(config.Get(a.settings, "restart_steam")) {
					a.startWhenClosed()
				}
				return err
			}
			closed = true
			a.checkClosingSave(p.original, backup)
		}
		// Steam's files are replaced atomically, so after a failure Steam can start again on what's there
		failed := func(err error) error {
			if closed && config.Yes(config.Get(a.settings, "restart_steam")) {
				a.startSteam()
			}
			return err
		}
		if closed {
			if p, err = a.buildPlan(games, approved); err != nil {
				return failed(err)
			}
		}
		if err := a.write(p); err != nil {
			return failed(err)
		}
	}
	p.apply(games)

	// Remembered only once Steam has what it describes
	if !a.DryRun {
		if err := p.state.Save(a.Paths.State); err != nil {
			fmt.Fprintf(a.Warn, "  ! Can't write %s: %v\n", a.Paths.State, err)
		}
	}

	a.report(games, p)

	if !a.DryRun {
		synced = true
		a.syncArtwork(games, p)
	}

	// Summary line
	added := 0
	for _, g := range games {
		if g.status == statusAdded {
			added++
		}
	}
	head, tail := "Done", ""
	if a.DryRun {
		head = "Dry run, nothing changed"
	}
	if !changed {
		tail = ", Steam already up to date"
	}
	fmt.Fprintf(a.Out, "%s: %d added, %d removed%s\n", head, added, len(p.removed), tail)

	// Start Steam again only when steam-adopt closed it, or after every sync with start_steam
	restart := closed && config.Yes(config.Get(a.settings, "restart_steam"))
	start := config.Yes(config.Get(a.settings, "start_steam"))
	if !a.DryRun && (restart || start) {
		a.startSteam()
	}
	return nil
}

// Turn an error into a sentence for the user, errors themselves starting lowercase as Go's are
func Sentence(err error) string {
	msg := err.Error()
	r, size := utf8.DecodeRuneInString(msg)
	return string(unicode.ToUpper(r)) + msg[size:]
}

// Start Steam unless it's already running, a failure only warned about
func (a *App) startSteam() {
	if a.Steam.Running() {
		return
	}
	if err := a.Steam.Start(); err != nil {
		fmt.Fprintf(a.Warn, "Couldn't start Steam: %v\n", err)
	} else {
		fmt.Fprintln(a.Out, "Starting Steam...")
	}
}

// Longest wait after Ctrl-C for a closing Steam to exit before starting it again
const reopenWait = 10 * time.Second

// Start Steam again once the one asked to close has exited, since starting it while it's closing would be lost on it
func (a *App) startWhenClosed() {
	poll, settle := or(a.PollInterval, time.Second), or(a.SettleDelay, 2*time.Second)
	for deadline := time.Now().Add(reopenWait); a.Steam.Running(); time.Sleep(poll) {
		if time.Now().After(deadline) {
			fmt.Fprintln(a.Warn, "  ! Steam was still closing when interrupted, start it again once it has closed")
			return
		}
	}
	time.Sleep(settle)
	a.startSteam()
}

// Read steam-adopt.conf, creating it with the defaults on the first run
func (a *App) loadConfig() error {
	a.fresh = !fsutil.Exists(a.Paths.Config)
	if !a.DryRun {
		if err := os.MkdirAll(a.Paths.Sync, 0o777); err != nil {
			return err
		}
		created, err := config.Create(a.Paths.Config)
		if err != nil {
			return fmt.Errorf("can't create %s: %w", a.Paths.Config, err)
		}
		if created {
			fmt.Fprintf(a.Out, "Created config file: %s\n", a.Paths.Config)
		}
	}
	cfg, err := config.Read(a.Paths.Config)
	if err != nil {
		return config.FormatError(a.Paths.Config, err)
	}
	a.cfg = cfg
	a.settings = cfg.Section(config.SettingsSection)
	return nil
}

// List every game folder by name, leaving out hidden folders and steam-adopt's own
func (a *App) games() ([]*game, error) {
	entries, err := os.ReadDir(a.Paths.Games)
	if err != nil {
		return nil, err
	}
	var games []*game
	for _, d := range entries {
		p := filepath.Join(a.Paths.Games, d.Name())
		if strings.HasPrefix(d.Name(), ".") || a.Paths.isSyncDir(p) || !fsutil.IsDir(p) {
			continue
		}
		if !sectionName(d.Name()) {
			fmt.Fprintf(a.Warn, "  ! %q: folder name can't be a section in %s, rename the folder to add it\n",
				d.Name(), filepath.Base(a.Paths.Config))
			continue
		}
		games = append(games, &game{folder: p, section: a.cfg.Section(d.Name()), settings: a.settings})
	}
	return games, nil
}

// Check whether a folder name can be a game's own config section: not one of the shared sections, and on one line
func sectionName(name string) bool {
	return name != config.SettingsSection && name != ini.DefaultSection && !strings.ContainsAny(name, "\r\n")
}

// Work out a game's exe, name, shortcut ID and Proton from what's on disk, safe while Steam runs
func (a *App) prepareGame(g *game, known map[string]uint32) {
	if g.skip() {
		g.status = statusSkip
		return
	}
	// Quotes are how a path with spaces is often written, but the value is taken literally
	if e := unquote(g.section["exe"]); e != "" {
		if filepath.IsAbs(e) {
			g.exe = filepath.Clean(e)
		} else {
			g.exe = filepath.Join(g.folder, e)
		}
	} else {
		g.exe = findExe(g.folder)
	}

	// Shortcuts are matched by folder, so an exe elsewhere would add a shortcut every run or take another game's
	if g.exe != "" && !inside(g.exe, g.folder) {
		fmt.Fprintf(a.Warn, "  ! %s: exe = \"%s\" is outside the game folder, set a path inside it\n", g.folderName(), g.section["exe"])
		g.status = statusNoExe
		return
	}
	if g.exe == "" || !launchable(g.exe) {
		if e := g.section["exe"]; e != "" {
			fmt.Fprintf(a.Warn, "  ! %s: exe = %s isn't a file in the game folder\n", g.folderName(), e)
		}
		g.status = statusNoExe
		// A section to fill in, with the exe line ready
		g.isNew = a.isNew(g)
		return
	}
	g.name = g.section["name"]
	if g.name == "" {
		g.name = g.folderName()
	}
	g.appid = known[g.folder]
	if g.appid == 0 {
		g.appid = steam.ShortcutAppID(quote(g.exe), g.name)
	}
	g.isNew = a.isNew(g)

	// Only Steam for Linux runs Windows builds through Proton, elsewhere its setting is left alone
	if goos != "linux" {
		return
	}

	// Written into config.vdf as is, so anything that would break its quoting is refused
	tool := strings.Trim(strings.TrimSpace(g.opt("proton")), `"'`)
	switch {
	case tool == "" || strings.EqualFold(tool, "auto"):
		g.proton = "none"
		if g.isWindows() {
			g.proton = a.autoProton
		}
	case strings.EqualFold(tool, "none"):
		g.proton = "none"
	case strings.ContainsAny(tool, "\"\\ \t\r\n"):
		fmt.Fprintf(a.Warn, "  ! %s: proton = %q isn't a valid tool name, its Proton setting is left as is\n", g.folderName(), tool)
	default:
		g.proton = tool
		// Flatpak Steam keeps its tools inside its sandbox, where they can't be looked for reliably
		if !steam.IsFlatpak(a.Paths.Steam) && !a.toolInstalled(tool) {
			fmt.Fprintf(a.Warn, "  ! %s: proton = %s isn't installed in compatibilitytools.d, the game won't start until it is\n",
				g.folderName(), tool)
		}
	}
}

// Check whether a compatibility tool is installed, looked up once per run since every game can name the same one
func (a *App) toolInstalled(tool string) bool {
	if a.tools == nil {
		a.tools = map[string]bool{}
	}
	if ok, seen := a.tools[tool]; seen {
		return ok
	}
	a.tools[tool] = steam.ToolInstalled(a.Paths.Steam, tool)
	return a.tools[tool]
}

// Check whether a game has no config section yet, under its folder's name or the one it was renamed from
func (a *App) isNew(g *game) bool {
	return !a.cfg.Has(g.folderName()) && (g.renamedFrom == "" || !a.cfg.Has(g.renamedFrom))
}

// Strip one pair of matching quotes around a config value
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// Check whether a shortcut's exe belongs to a game, by its folder or the folder it was renamed from
func (a *App) owns(g *game, exe string) bool {
	return inside(exe, g.folder) || (g.renamedFrom != "" && inside(exe, filepath.Join(a.Paths.Games, g.renamedFrom))) ||
		(g.movedFrom != "" && inside(exe, g.movedFrom))
}

// Pair shortcuts to a missing exe outside the games folder with the game holding it now, as after a move, one to one only
func (a *App) moves(games []*game, entries []any) []rename {
	owned := map[*game]bool{}
	byGame := map[*game]map[string]bool{}
	byOld := map[string]map[*game]bool{}
	sep := string(filepath.Separator)
	for _, e := range entries {
		exe := filepath.Clean(exePath(e))
		for _, g := range games {
			if a.owns(g, exe) {
				owned[g] = true
			}
		}
		if !filepath.IsAbs(exe) || inside(exe, a.Paths.Games) || fsutil.LExists(exe) {
			continue
		}
		for _, g := range games {
			// The last /<folder>/ in the old exe path, with the same exe below it in the game now
			i := strings.LastIndex(exe, sep+g.folderName()+sep)
			if i < 0 {
				continue
			}
			old := exe[:i+1+len(g.folderName())]
			if !fsutil.IsFile(filepath.Join(g.folder, exe[len(old)+1:])) {
				continue
			}
			if byGame[g] == nil {
				byGame[g] = map[string]bool{}
			}
			if byOld[old] == nil {
				byOld[old] = map[*game]bool{}
			}
			byGame[g][old], byOld[old][g] = true, true
		}
	}
	var out []rename
	for _, g := range games {
		if len(byGame[g]) != 1 || owned[g] || g.renamedFrom != "" {
			continue
		}
		for old := range byGame[g] {
			if len(byOld[old]) == 1 {
				out = append(out, rename{g, old})
			}
		}
	}
	return out
}

// A game folder found to be another one renamed
type rename struct {
	g   *game
	old string
}

// Pair gone folders with new ones of the same name and exe, like Game and Game [GOG], anything unclear asked about
func (a *App) renames(games []*game, entries []any) []rename {
	// Exe paths, relative to their folder, of shortcuts into folders that are gone
	gone := map[string][]string{}
	for _, e := range entries {
		exe := filepath.Clean(exePath(e))
		if !inside(exe, a.Paths.Games) || a.Paths.inSyncDir(exe) {
			continue
		}
		rel, err := filepath.Rel(a.Paths.Games, exe)
		if err != nil {
			continue
		}
		folder, inner, ok := strings.Cut(rel, string(filepath.Separator))
		if !ok || fsutil.LExists(filepath.Join(a.Paths.Games, folder)) {
			continue
		}
		gone[folder] = append(gone[folder], inner)
	}
	if len(gone) == 0 {
		return nil
	}

	// New folders only: no config section and no shortcut of their own
	hasShortcut := func(g *game) bool {
		for _, e := range entries {
			if inside(exePath(e), g.folder) {
				return true
			}
		}
		return false
	}
	byOld := map[string][]*game{}
	byGame := map[*game]int{}
	for old, rels := range gone {
		for _, g := range games {
			if a.cfg.Has(g.folderName()) || !related(old, g.folderName()) || hasShortcut(g) {
				continue
			}
			for _, rel := range rels {
				if fsutil.IsFile(filepath.Join(g.folder, rel)) {
					byOld[old] = append(byOld[old], g)
					byGame[g]++
					break
				}
			}
		}
	}
	var out []rename
	for old, gs := range byOld {
		if len(gs) == 1 && byGame[gs[0]] == 1 {
			out = append(out, rename{gs[0], old})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].old < out[j].old })
	return out
}

// Check whether two folder names are the same game without tags and versions, so Saga 2 isn't Saga
func related(a, b string) bool {
	x := store.Norm(store.Untag(a))
	return x != "" && x == store.Norm(store.Untag(b))
}

// Move a renamed folder's config section, artwork and last run over to its new name
func (a *App) adoptRename(g *game, old string) {
	g.renamedFrom = old
	verb := "keeping"
	if a.DryRun {
		verb = "would keep"
	}
	fmt.Fprintf(a.Out, "  ~ %s: renamed from %s, %s its shortcut, settings and artwork\n", g.folderName(), old, verb)
	if !a.DryRun {
		if a.cfg.Has(old) {
			if err := config.RenameSection(a.Paths.Config, old, g.folderName()); err != nil {
				fmt.Fprintf(a.Warn, "  ! Can't rename [%s] in %s: %v\n", old, filepath.Base(a.Paths.Config), err)
			}
		}
		oldArt := filepath.Join(a.Paths.Art, old)
		if fsutil.IsDir(oldArt) && !fsutil.LExists(g.art(a.Paths)) {
			if err := os.Rename(oldArt, g.art(a.Paths)); err != nil {
				fmt.Fprintf(a.Warn, "  ! Can't move the artwork of %s: %v\n", old, err)
			}
		}
	}
	g.section = a.cfg.Section(old)
	if last := a.state.Game(old); last != nil {
		a.state.Games[g.folderName()] = last
	}
}

// Games fetched at once, few enough to stay polite to the Steam store
const fetchWorkers = 4

// Look up and download the artwork of every prepared game, a few at a time
func (a *App) fetchAll(ctx context.Context, games []*game) error {
	sem := make(chan struct{}, fetchWorkers)
	var wg sync.WaitGroup
	for _, g := range games {
		if g.status != "" {
			continue
		}
		select {
		case <-ctx.Done():
		case sem <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				a.fetchArtwork(ctx, g)
			}()
		}
	}
	wg.Wait()
	return ctx.Err()
}

// Find a game's Steam store ID and download its missing artwork
func (a *App) fetchArtwork(ctx context.Context, g *game) {
	// Runs in parallel with other games, so problems go into this game's notes, never straight to a.Warn
	info := ""
	var markErr error
	raw := strings.TrimSpace(g.section["steam_id"])
	id, validID := parseSteamID(raw)
	if validID {
		g.steamID, info = id, "from config"
	} else if found := artwork.SteamID(g.art(a.Paths)); found != 0 && !g.isNew {
		g.steamID, info = found, "already downloaded"
	} else if !g.isNew && artwork.RecentlyNotFound(g.art(a.Paths), g.folderName(), time.Now()) {
		info = store.NotFound + " (checked in the last week)"
	} else if g.artwork() || g.isNew {
		// A new game is always looked up once, so its config section can show the ID found
		g.steamID, info = a.Store.SearchID(ctx, store.Untag(g.folderName()))
		g.notFound = g.steamID == 0 && info == store.NotFound

		// Only a definite answer is remembered, a failed search is tried again next run
		if !a.DryRun && g.artwork() {
			term := ""
			if g.notFound {
				term = g.folderName()
			}
			markErr = artwork.MarkNotFound(g.art(a.Paths), term)
		}
	}

	if !g.artwork() {
		g.artNotes = []string{"off"}
		return
	}
	conf := filepath.Base(a.Paths.Config)
	if guess, ok := strings.CutSuffix(info, store.BestGuess); ok && g.steamID != 0 {
		g.artNotes = []string{fmt.Sprintf("best guess Steam ID %d %q, set steam_id in %s if it's the wrong game", g.steamID, guess, conf)}
	} else if g.steamID == 0 {
		g.artNotes = []string{fmt.Sprintf("%s, set steam_id in %s to fix", info, conf)}
	} else {
		g.artNotes = []string{fmt.Sprintf("Steam ID %d (%s)", g.steamID, info)}
	}
	if raw != "" && !validID {
		g.artNotes = append(g.artNotes, fmt.Sprintf("steam_id = %s ignored, it isn't a store ID or store page URL", raw))
	}
	if markErr != nil {
		g.artNotes = append(g.artNotes, fmt.Sprintf("couldn't remember the search (%v)", markErr))
	}
	if a.DryRun {
		return
	}

	// Even without a store ID, since linking custom artwork would otherwise replace pictures set in Steam
	if err := artwork.Adopt(g.appid, g.art(a.Paths), a.grid); err != nil {
		g.artNotes = append(g.artNotes, fmt.Sprintf("couldn't keep artwork set in Steam (%v)", err))
	}
	if g.steamID == 0 {
		return
	}

	// Store pictures the folder is still missing
	notes, err := artwork.Download(ctx, a.Store, g.steamID, g.art(a.Paths))
	g.artNotes = append(g.artNotes, notes...)
	if err != nil {
		g.artNotes = append(g.artNotes, fmt.Sprintf("download failed (%v)", err))
	}
}

// Store page URLs, whose app number is what steam_id wants
var storeURLRe = regexp.MustCompile(`^(?:https?://)?store\.steampowered\.com/app/(\d+)(?:[/?#].*)?$`)

// Parse a steam_id setting: digits, or a store page URL pasted as is
func parseSteamID(s string) (int64, bool) {
	s = unquote(s)
	if m := storeURLRe.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	// Store IDs start at 1, a 0 would turn the lookup off without saying so
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

// Add a section for a new game, showing what steam-adopt picked as commented-out values
func (a *App) appendSection(g *game) error {
	rel, proton := "", config.Defaults["proton"]
	if goos != "linux" {
		proton = ""
	}
	if g.exe != "" {
		rel, _ = filepath.Rel(g.folder, g.exe)
		proton = g.proton
	}
	return config.AppendGame(a.Paths.Config, config.Game{
		Folder:        g.folderName(),
		Exe:           rel,
		SteamID:       g.steamID,
		NotFound:      g.notFound,
		LaunchOptions: config.Get(a.settings, "launch_options"),
		Proton:        proton,
	})
}

// Compare the shortcuts Steam flushed while closing with the ones it had before, planning from those when it emptied the
// list and naming the missing ones when it dropped only some, which may also have been removed inside Steam
func (a *App) checkClosingSave(before []byte, backup string) {
	if len(before) == 0 {
		return
	}
	old, err := parseShortcuts(before)
	if err != nil {
		return
	}
	after, _ := os.ReadFile(a.shortcutsFile)
	now, err := parseShortcuts(after)
	if err != nil {
		return
	}
	kept := map[uint32]bool{}
	list := shortcutList(now)
	for _, e := range list {
		kept[shortcutID(e)] = true
	}
	var missing []string
	for _, e := range shortcutList(old) {
		if id := shortcutID(e); id != 0 && !kept[id] {
			missing = append(missing, appName(e, strconv.FormatUint(uint64(id), 10)))
		}
	}
	switch {
	case len(missing) == 0:
	case len(list) == 0:
		a.restore = before
		fmt.Fprintf(a.Warn, "  ! Steam emptied its shortcuts while closing, the %d it had are kept\n", len(missing))
	default:
		fmt.Fprintf(a.Warn, "  ! Steam dropped %d shortcut(s) while closing: %s; if you didn't remove them in Steam, close Steam and copy %s over %s\n",
			len(missing), strings.Join(missing, ", "), backup, a.shortcutsFile)
	}
}

// Ask Steam to shut down and wait for it, so it has written its files before they're edited
func (a *App) closeSteam(ctx context.Context) error {
	timeout, poll, settle := or(a.CloseTimeout, time.Minute), or(a.PollInterval, time.Second), or(a.SettleDelay, 2*time.Second)
	fmt.Fprintln(a.Out, "Closing Steam...")

	// Only whether Steam goes away matters, unless there was no command at all, which would just wait out the timeout
	if err := a.Steam.Shutdown(ctx); errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("can't close Steam, no steam command found (%v), close Steam manually and run steam-adopt again", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !a.Steam.Running() {
			// Steam's helper processes flush their files a moment after the main one exits
			return sleep(ctx, settle)
		}
		if err := sleep(ctx, poll); err != nil {
			return err
		}
	}
	return ErrSteamDidNotClose
}

// Ask once about every pending removal, returning the approved shortcut IDs
func (a *App) approve(pending []removal) map[uint32]bool {
	if len(pending) == 0 || a.DryRun || a.Confirm == nil {
		return nil
	}
	names := make([]string, len(pending))
	for i, r := range pending {
		names[i] = r.name
		if r.why == whyDuplicate {
			names[i] += " (" + whyDuplicate + ")"
		}
	}
	if !a.Confirm(names) {
		return nil
	}
	approved := map[uint32]bool{}
	for _, r := range pending {
		approved[r.appid] = true
	}
	return approved
}

// Quote s for a POSIX shell, so a command shown to the user can be pasted as is
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Use fallback for an unset duration
func or(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// Sleep, ending early when ctx is cancelled, and never going on once it is, which select alone picks at random
func sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Backups kept of each of Steam's files
const backups = 5

// Write the changed Steam files, backing up each old one first
func (a *App) write(p *plan) error {
	now := time.Now()
	if err := os.MkdirAll(filepath.Dir(a.shortcutsFile), 0o777); err != nil {
		return err
	}
	if !bytes.Equal(p.shortcuts, p.original) {
		// Already backed up before closing Steam when Steam left the file as it was
		if len(p.original) > 0 && !bytes.Equal(p.original, a.backedUp) {
			if _, err := fsutil.Backup(a.shortcutsFile, backups, now); err != nil {
				return fmt.Errorf("can't back up %s: %w", a.shortcutsFile, err)
			}
		}
		if err := fsutil.WriteAtomic(a.shortcutsFile, p.shortcuts); err != nil {
			return fmt.Errorf("can't write %s: %w", a.shortcutsFile, err)
		}
	}
	if p.newConfigTxt != p.configTxt {
		if _, err := fsutil.Backup(a.configVDF, backups, now); err != nil {
			return fmt.Errorf("can't back up %s: %w", a.configVDF, err)
		}
		if err := fsutil.WriteAtomic(a.configVDF, []byte(p.newConfigTxt)); err != nil {
			return fmt.Errorf("can't write %s: %w", a.configVDF, err)
		}
	}
	return nil
}

// Report line icon and wording of each game status
var (
	statusIcons  = map[string]string{statusAdded: "+", statusUpdated: "~", statusKept: "=", statusSkip: "-", statusNoExe: "?"}
	statusLabels = map[string]string{statusUpdated: "updated", statusKept: "up to date", statusSkip: "skipped"}
)

// Print one line per game, then what was removed and anything needing the user's attention
func (a *App) report(games []*game, p *plan) {
	removedFolders := map[string]bool{}
	for _, r := range append(append([]removal(nil), p.removed...), p.pending...) {
		removedFolders[r.folder] = true
	}
	for _, g := range games {
		// Skipped games that were in Steam get their own "removed" or "kept" line below
		if g.status == statusSkip && removedFolders[g.folderName()] {
			continue
		}
		label := statusLabels[g.status]
		switch g.status {
		case statusAdded:
			label = "added (" + filepath.Base(g.exe) + ")"
		case statusNoExe:
			label = fmt.Sprintf("no executable found, set exe under [%s] in %s", g.folderName(), filepath.Base(a.Paths.Config))
		}
		name := g.name
		if name == "" {
			name = g.folderName()
		}
		fmt.Fprintf(a.Out, "  %s %s: %s\n", statusIcons[g.status], name, label)
		if len(g.artNotes) > 0 {
			fmt.Fprintf(a.Out, "      artwork: %s\n", strings.Join(g.artNotes, ", "))
		}
		if len(g.fromSteam) > 0 {
			fmt.Fprintf(a.Out, "      kept from Steam: %s\n", fieldLabels(g.fromSteam))
		}
		if len(g.conflicts) > 0 {
			fmt.Fprintf(a.Warn, "  ! %s: %s changed both in Steam and in the config, the config's value was used\n",
				name, fieldLabels(g.conflicts))
		}
	}
	for _, r := range p.removed {
		fmt.Fprintf(a.Out, "  - %s: %s, removed from Steam\n", r.name, r.why)
	}
	for _, r := range p.pending {
		if a.DryRun {
			fmt.Fprintf(a.Out, "  ! %s: %s, would be removed from Steam after asking\n", r.name, r.why)
		} else {
			fmt.Fprintf(a.Out, "  ! %s: %s, kept in Steam since its removal wasn't confirmed\n", r.name, r.why)
		}
	}
	if len(p.held) > 0 {
		fmt.Fprintf(a.Warn, "  ! %d shortcut(s) point into missing game folders but were kept, since %s didn't exist before this run;\n"+
			"      if the games folder is on a drive, check that it's mounted, otherwise run steam-adopt again to remove them\n",
			len(p.held), filepath.Base(a.Paths.Config))
	}
	if len(p.pending) > 0 && !a.DryRun && a.Confirm == nil {
		fmt.Fprintln(a.Out, "  Run steam-adopt in a terminal to be asked, or with --yes to remove them")
	}

	// Problems that would stop games from starting
	anyWindows := false
	options := map[string]bool{}
	for _, g := range games {
		if g.active() {
			anyWindows = anyWindows || (g.isWindows() && goos == "linux")
			options[g.opt("launch_options")] = true

			// Steam runs a native game directly, which fails without the execute bit, as after unzipping, unknown on Windows
			if st, err := os.Stat(g.exe); goos != "windows" && !g.isWindows() && err == nil && st.Mode().IsRegular() &&
				st.Mode().Perm()&0o111 == 0 {
				fmt.Fprintf(a.Warn, "  ! %s: %s isn't executable, so Steam can't start it; fix with: chmod +x %s\n",
					g.name, filepath.Base(g.exe), shellQuote(g.exe))
			}
		}
	}
	// Mappings dropped with a removed game are already told by its removal line
	gone := map[uint32]bool{}
	for _, r := range p.removed {
		gone[r.appid] = true
	}
	protonSet := 0
	for _, id := range p.protonChanged {
		if !gone[id] {
			protonSet++
		}
	}
	if protonSet > 0 {
		fmt.Fprintf(a.Out, "  Proton setting changed for %d game(s)\n", protonSet)
	} else if !p.protonFound && anyWindows {
		fmt.Fprintln(a.Out, "  ! Proton mapping not found in Steam's config.vdf, enable Steam Play in Steam's settings once")
	}
	// Flatpak Steam runs them from its sandbox, so what the host has installed says nothing about them
	if !steam.IsFlatpak(a.Paths.Steam) {
		if tools := missingTools(options); len(tools) > 0 {
			fmt.Fprintf(a.Out, "  ! Not installed but used in launch options: %s\n", strings.Join(tools, ", "))
		}
	}
}

// Report wording of the shortcut fields steam-adopt manages
var fieldNames = map[string]string{
	"AppName": "name", "StartDir": "start folder", "LaunchOptions": "launch options", "icon": "icon", "proton": "Proton",
}

// Join field keys into their report wording
func fieldLabels(keys []string) string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = fieldNames[k]
	}
	return strings.Join(out, ", ")
}

// List launch-option commands that aren't installed, which would stop games from starting
func missingTools(options map[string]bool) []string {
	cmds := map[string]bool{}
	for o := range options {
		// Without %command%, Steam passes the options to the game as arguments, so nothing in them is run
		before, _, ok := strings.Cut(o, "%command%")
		if !ok {
			continue
		}
		// Flags, VAR=value and the argument after a short flag, like the 0-3 of taskset -c 0-3, aren't commands
		afterFlag := false
		for _, w := range strings.Fields(before) {
			arg := afterFlag
			afterFlag = len(w) == 2 && w[0] == '-' && w[1] != '-'
			if arg || strings.Contains(w, "=") || strings.HasPrefix(w, "-") || !strings.ContainsFunc(w, unicode.IsLetter) {
				continue
			}
			cmds[w] = true
		}
	}
	// Steam runs launch options through a shell, which expands a leading ~ like in ~/bin/tool
	home, _ := os.UserHomeDir()
	var out []string
	for c := range cmds {
		path := c
		if rest, ok := strings.CutPrefix(c, "~/"); ok && home != "" {
			path = filepath.Join(home, rest)
		}
		if _, err := exec.LookPath(path); err != nil {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// Warn about a problem with what, when there is one
func (a *App) warnFor(what string, err error) {
	if err != nil {
		fmt.Fprintf(a.Warn, "  ! %s: %v\n", what, err)
	}
}

// Point Steam's grid files at every game's artwork, including pictures fetchArtwork moved out of the grid folder
func (a *App) linkArtwork(games []*game) {
	linkFailed := false
	for _, g := range games {
		if g.appid != 0 && g.artwork() {
			err := artwork.Link(g.appid, g.art(a.Paths), a.grid)
			linkFailed = linkFailed || err != nil
			a.warnFor("artwork for "+g.folderName(), err)
		}
	}
	// Windows only lets administrators make symbolic links, unless Developer Mode is on
	if linkFailed && goos == "windows" {
		fmt.Fprintln(a.Warn, "  ! To link artwork into Steam, turn on Developer Mode in Windows' settings "+
			"(System, For developers) and run steam-adopt again")
	}
}

// Link artwork into Steam's grid folder and clean up after removed games
func (a *App) syncArtwork(games []*game, p *plan) {
	warn := a.warnFor
	a.linkArtwork(games)
	for _, r := range p.removed {
		switch {
		case r.why == whyDuplicate || r.why == whySkipped:
			// The game stays, so only its grid files go, a skipped game's pictures set in Steam kept in its artwork folder
			if r.appid != 0 {
				if r.why == whySkipped {
					warn("artwork for "+r.folder, artwork.Adopt(r.appid, filepath.Join(a.Paths.Art, r.folder), a.grid))
				}
				warn("artwork for "+r.folder, artwork.Clean(r.appid, "", a.grid))
			}
		case r.appid == 0:
			// Without an ID there are no grid files to tell apart from other apps'
			warn("artwork for "+r.folder, os.RemoveAll(filepath.Join(a.Paths.Art, r.folder)))
		default:
			warn("artwork for "+r.folder, artwork.Clean(r.appid, filepath.Join(a.Paths.Art, r.folder), a.grid))
		}
		if r.why == whyDeleted {
			warn(a.Paths.Config, config.RemoveSection(a.Paths.Config, r.folder))
		}
	}

	// Artwork whose shortcut was already removed by hand from inside Steam, not of a shortcut kept unconfirmed
	kept := map[string]bool{}
	for _, r := range append(append([]removal(nil), p.pending...), p.held...) {
		kept[r.folder] = true
	}
	if arts, err := os.ReadDir(a.Paths.Art); err == nil {
		for _, d := range arts {
			if !kept[d.Name()] && !fsutil.LExists(filepath.Join(a.Paths.Games, d.Name())) {
				warn("artwork for "+d.Name(), os.RemoveAll(filepath.Join(a.Paths.Art, d.Name())))
			}
		}
	}
}
