// One game folder: its settings, the executable found in it and how Steam runs it

package app

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/ddrimus/steam-adopt/internal/artwork"
	"github.com/ddrimus/steam-adopt/internal/config"
	"github.com/ddrimus/steam-adopt/internal/fsutil"
	"github.com/ddrimus/steam-adopt/internal/store"
)

// Outcome of a game in this run
const (
	statusAdded   = "added"
	statusUpdated = "updated"
	statusKept    = "kept"
	statusSkip    = "skip"
	statusNoExe   = "noexe"
)

// Everything known about one game folder, filled in by prepareGame and the final plan's apply
type game struct {
	folder string
	// The game's own config section, empty when it has none
	section map[string]string
	// The [settings] section
	settings map[string]string
	exe      string
	name     string
	steamID  int64
	// The store was searched this run and has no such game
	notFound bool
	artNotes []string
	status   string
	appid    uint32
	// No config section yet, one is added for it
	isNew bool
	// The folder's name before it was renamed, whose shortcut, settings and artwork it took over, "" if none
	renamedFrom string
	// Full path the folder had before the whole games folder moved, like to another drive, "" when it didn't
	movedFrom string
	// Compatibility tool to map, "none" to remove it, "" to leave it alone
	proton string
	// Shortcut fields edited inside Steam and kept this run
	fromSteam []string
	// Shortcut fields edited both inside Steam and in the config, the config's value used
	conflicts []string
}

// Read a setting from the game's section, then [settings], then the built-in default
func (g *game) opt(key string) string {
	if v, ok := g.section[key]; ok {
		return v
	}
	return config.Get(g.settings, key)
}

// The game's folder name, which also names its config section and artwork folder
func (g *game) folderName() string { return filepath.Base(g.folder) }

// The name the config asks for, the folder's when unset
func (g *game) configName() string {
	if n := g.section["name"]; n != "" {
		return n
	}
	return g.folderName()
}

// Only a game's own section can skip it, a skip in [settings] would hide every game
func (g *game) skip() bool { return config.Yes(g.section["skip"]) }

// Check whether steam-adopt manages this game's artwork
func (g *game) artwork() bool { return config.Yes(g.opt("artwork")) }

// The system steam-adopt runs on, a variable so tests can check every system's rules
var goos = runtime.GOOS

// Windows builds need Proton on Linux, and fall back to the exe as their icon everywhere
func (g *game) isWindows() bool { return strings.EqualFold(suffix(filepath.Base(g.exe)), ".exe") }

// Check whether the game ends up as a shortcut in Steam
func (g *game) active() bool {
	return g.status == statusAdded || g.status == statusUpdated || g.status == statusKept
}

// The downloaded icon, "" when there is none or steam-adopt doesn't manage the artwork
func (g *game) icon(p Paths) string {
	if !g.artwork() {
		return ""
	}
	return artwork.Icon(g.art(p))
}

// Artwork folder, named after the folder rather than the display name so a rename keeps it
func (g *game) art(p Paths) string { return filepath.Join(p.Art, g.folderName()) }

// Folder levels searched for executables, games keep theirs near the top
const exeDepth = 3

// Helper-looking names, and native Linux binaries
var (
	helperRe  = regexp.MustCompile(`(?i)(crash|unins|setup|redist|vcredist|dxsetup|launcherhelper|helper|updater|config)`)
	nativeExt = []string{".x86_64", ".x86", ".AppImage"}
)

// Kinds of launchable file, in order of preference: a native binary, then a Windows build,
// then a shell script, which is as often a helper shipped next to a Windows build as a launcher
const (
	kindNative = iota
	kindWindows
	kindScript
	// Not something a game is started with
	kindNone
)

// Tell what kind of launchable file a name is on this system, Mac apps being folders found separately
func exeKind(name string) int {
	switch {
	case strings.EqualFold(suffix(name), ".exe"):
		if goos == "darwin" {
			return kindNone
		}
		return kindWindows
	case goos == "windows":
		return kindNone
	case strings.HasSuffix(name, ".sh"):
		return kindScript
	case goos != "linux":
		return kindNone
	}
	for _, ext := range nativeExt {
		if strings.HasSuffix(name, ext) {
			return kindNative
		}
	}
	return kindNone
}

// Check whether path can be a game's exe: a file, or on macOS an app bundle
func launchable(path string) bool {
	return fsutil.IsFile(path) || (isApp(path) && fsutil.IsDir(path))
}

// Check whether path names a macOS app bundle, which Steam starts like a program
func isApp(path string) bool {
	return goos == "darwin" && strings.HasSuffix(path, ".app")
}

// One possible main executable and the traits it's ranked by
type exeCandidate struct {
	path string
	// Name differs from the folder's
	notNamed bool
	// Looks like a crash handler, installer or other helper
	helper bool
	depth  int
	kind   int
	size   int64
}

// Rank by folder name (even over helper names), then non-helpers, shallowest, kind, biggest
func (a *exeCandidate) better(b *exeCandidate) bool {
	if a.notNamed != b.notNamed {
		return !a.notNamed
	}
	if a.helper != b.helper {
		return !a.helper
	}
	if a.depth != b.depth {
		return a.depth < b.depth
	}
	if a.kind != b.kind {
		return a.kind < b.kind
	}
	return a.size > b.size
}

// Guess a game's main executable, skipping helpers, "" when the folder has none
func findExe(folder string) string {
	name := store.Norm(store.Untag(filepath.Base(folder)))
	var best *exeCandidate

	// Bounded walk, a full one would crawl through every asset file of a big game
	filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(folder, p)
		depth := 0
		if rel != "." {
			depth = strings.Count(rel, string(filepath.Separator)) + 1
		}
		fname := d.Name()
		var size int64
		kind := kindNone
		switch {
		case d.IsDir() && depth > 0 && isApp(fname):
			// A Mac app is launched as a whole, so it's a candidate and nothing inside it is
			kind = kindNative
		case d.IsDir():
			if depth >= exeDepth {
				return filepath.SkipDir
			}
			return nil
		default:
			kind = exeKind(fname)
			if kind == kindNone && (suffix(fname) != "" || goos != "linux") {
				return nil
			}

			// Symlinks are followed, so a link to the real binary counts
			st, err := os.Stat(p)
			if err != nil || !st.Mode().IsRegular() {
				return nil
			}
			size = st.Size()

			// Most native Linux builds are a binary without any extension, like Celeste or bin/Game
			if kind == kindNone {
				if !isELF(p) {
					return nil
				}
				kind = kindNative
			}
		}
		c := &exeCandidate{
			path:     p,
			notNamed: store.Norm(stem(fname)) != name,
			helper:   helperRe.MatchString(fname),
			depth:    depth,
			kind:     kind,
			size:     size,
		}
		if best == nil || c.better(best) {
			best = c
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if best == nil {
		return ""
	}
	return best.path
}

// Check whether a file is a Linux program, by its first bytes
func isELF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 4)
	_, err = io.ReadFull(f, magic)
	return err == nil && string(magic) == "\x7fELF"
}

// Get the last extension of a file name, "" for dotfiles and names ending in a dot
func suffix(name string) string {
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	return name[i:]
}

// Strip the last extension from a file name
func stem(name string) string {
	return strings.TrimSuffix(name, suffix(name))
}

// Check whether a path sits somewhere under folder, checked without touching the disk
func inside(path, folder string) bool {
	if path == "" {
		return false
	}
	sep := string(filepath.Separator)
	p, f := filepath.Clean(path), filepath.Clean(folder)
	return p == f || strings.HasPrefix(p, strings.TrimSuffix(f, sep)+sep)
}
