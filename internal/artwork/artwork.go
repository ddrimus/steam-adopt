// Game artwork kept in its own folder and linked into Steam's grid folder
package artwork

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ddrimus/steam-adopt/internal/fsutil"
	"github.com/ddrimus/steam-adopt/internal/store"
)

// One picture: its file name in the artwork folder and its suffix after the ID in Steam's grid
type Kind struct {
	File, GridSuffix string
}

// Pictures linked into Steam's grid folder
var Kinds = []Kind{
	{"cover.jpg", "p.jpg"},
	{"banner.jpg", ".jpg"},
	{"hero.jpg", "_hero.jpg"},
	{"logo.png", "_logo.png"},
}

// Logos are missing for many games, so only these trigger a new store lookup when absent
var required = []string{"cover.jpg", "banner.jpg", "hero.jpg"}

// File holding the artwork's store ID, since not every app has an icon to name after it
const idFile = ".steam_id"

// Get the store ID the artwork in dir came from, or 0
func SteamID(dir string) int64 {
	if b, err := os.ReadFile(filepath.Join(dir, idFile)); err == nil {
		if id, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && id > 0 {
			return id
		}
	}
	return 0
}

// File recording a store search that found nothing, so reruns don't search again for a while
const notFoundFile = ".not_found"

// How long a search that found nothing is trusted, since a store page can be added later
const notFoundFor = 7 * 24 * time.Hour

// Check whether a search for term recently found nothing on the store
func RecentlyNotFound(dir, term string, now time.Time) bool {
	path := filepath.Join(dir, notFoundFile)
	st, err := os.Stat(path)
	if err != nil || now.Sub(st.ModTime()) > notFoundFor {
		return false
	}
	b, err := os.ReadFile(path)
	return err == nil && strings.TrimSpace(string(b)) == term
}

// Record that a search for term found nothing, or clear the record when term is ""
func MarkNotFound(dir, term string) error {
	path := filepath.Join(dir, notFoundFile)
	if term == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, []byte(term+"\n"))
}

// Get the path of the icon in dir, or "" when there is none
func Icon(dir string) string {
	if icons := fsutil.GlobDir(dir, "icon-*.jpg"); len(icons) > 0 {
		return icons[0]
	}
	return ""
}

// Fetch the store artwork dir is missing, never overwriting a file so custom pictures stay
func Download(ctx context.Context, c *store.Client, steamID int64, dir string) ([]string, error) {
	var notes []string
	oldID := SteamID(dir)

	// Skip the store entirely once the main pictures are there, so reruns cost no network requests
	if oldID == steamID && all(dir, required) {
		return notes, nil
	}

	// Fetched before deleting anything, so a mistyped steam_id can't wipe the artwork already there
	a, err := c.Assets(ctx, steamID)
	if err != nil {
		return notes, err
	}

	// Another store ID replaces only the old pictures downloaded and left unchanged, never ones put there by hand
	got := readManifest(dir)
	if oldID != 0 && oldID != steamID {
		var kept []string
		for _, k := range Kinds {
			path := filepath.Join(dir, k.File)
			if !fsutil.Exists(path) {
				delete(got, k.File)
				continue
			}
			if sum, ok := got[k.File]; ok && sum == fileSum(path) {
				os.Remove(path)
				delete(got, k.File)
			} else {
				// Replaced by the user, so no longer a download
				delete(got, k.File)
				kept = append(kept, k.File)
			}
		}
		icon := fmt.Sprintf("icon-%d.jpg", oldID)
		os.Remove(filepath.Join(dir, icon))
		delete(got, icon)
		notes = append(notes, fmt.Sprintf("replaced artwork from Steam ID %d", oldID))
		// Not downloaded by steam-adopt, changed since, or from a version that didn't record its downloads
		if len(kept) > 0 {
			notes = append(notes, "kept pictures not downloaded for that ID, delete them to download the new ones: "+
				strings.Join(kept, ", "))
		}
	}

	// A picture to download, a failed guessed URL only meaning the app has no such picture
	type want struct {
		file    string
		urls    []string
		guessed bool
	}
	one := func(file, url string) want {
		if url == "" {
			return want{file: file}
		}
		return want{file: file, urls: []string{url}}
	}
	logos, guessed := c.LogoURLs(a, steamID)

	// Folder and store ID record for the pictures
	wanted := []want{
		one("cover.jpg", c.URL(a, "library_capsule_2x", "library_capsule")),
		one("banner.jpg", c.URL(a, "header_2x", "header", "main_capsule")),
		one("hero.jpg", c.URL(a, "library_hero_2x", "library_hero")),
		{"logo.png", logos, guessed},
		one(fmt.Sprintf("icon-%d.jpg", steamID), c.IconURL(a, steamID)),
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return notes, err
	}
	if oldID != steamID {
		if err := fsutil.WriteAtomic(filepath.Join(dir, idFile), []byte(strconv.FormatInt(steamID, 10)+"\n")); err != nil {
			return notes, err
		}
	}

	// Missing pictures only, failures noted rather than stopping the rest
	var failed []string
	for _, w := range wanted {
		path := filepath.Join(dir, w.file)
		if len(w.urls) == 0 || fsutil.Exists(path) {
			continue
		}
		var data []byte
		err := errors.New("no URL")
		for _, u := range w.urls {
			if data, err = c.Get(ctx, u); err == nil {
				break
			}
		}
		if err == nil {
			err = fsutil.WriteAtomic(path, data)
		} else if w.guessed && ctx.Err() == nil {
			continue
		}
		if err != nil {
			failed = append(failed, w.file)
			continue
		}
		got[w.file] = sum(data)
	}
	if err := writeManifest(dir, got); err != nil {
		notes = append(notes, fmt.Sprintf("couldn't record the downloads (%v)", err))
	}
	if len(failed) > 0 {
		notes = append(notes, "failed: "+strings.Join(failed, ", "))
	}
	return notes, nil
}

// File listing the downloaded pictures with their SHA-256, so one replaced or added by the user is never deleted
const manifestFile = ".downloaded"

// Read the downloaded pictures' checksums by file name, empty when the folder has no manifest
func readManifest(dir string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if hash, file, ok := strings.Cut(line, "  "); ok && hash != "" && file != "" {
			out[file] = hash
		}
	}
	return out
}

// Write the manifest, removing it when nothing downloaded is left
func writeManifest(dir string, sums map[string]string) error {
	path := filepath.Join(dir, manifestFile)
	if len(sums) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	files := make([]string, 0, len(sums))
	for f := range sums {
		files = append(files, f)
	}
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		b.WriteString(sums[f] + "  " + f + "\n")
	}
	if old, err := os.ReadFile(path); err == nil && string(old) == b.String() {
		return nil
	}
	return fsutil.WriteAtomic(path, []byte(b.String()))
}

// SHA-256 of data in hex
func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// SHA-256 of a file in hex, "" when it can't be read
func fileSum(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return sum(data)
}

// Check whether every file is in dir
func all(dir string, files []string) bool {
	for _, f := range files {
		if !fsutil.Exists(filepath.Join(dir, f)) {
			return false
		}
	}
	return true
}

// Path of a picture in Steam's grid folder
func gridFile(grid string, appid uint32, k Kind) string {
	return filepath.Join(grid, strconv.FormatUint(uint64(appid), 10)+k.GridSuffix)
}

// Move pictures set from inside Steam into dir so they survive, as .jpg or .png, the newest one winning
func Adopt(appid uint32, dir, grid string) error {
	var errs []error
	for _, k := range Kinds {
		main := gridFile(grid, appid, k)
		other := strings.TrimSuffix(main, ".jpg") + ".png"
		if strings.HasSuffix(main, ".png") {
			other = strings.TrimSuffix(main, ".png") + ".jpg"
		}
		var set []string
		for _, g := range []string{main, other} {
			if st, err := os.Lstat(g); err == nil && st.Mode().IsRegular() {
				set = append(set, g)
			}
		}
		if len(set) == 0 {
			continue
		}
		sort.Slice(set, func(i, j int) bool { return modTime(set[i]).Before(modTime(set[j])) })
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return err
		}
		for _, g := range set {
			errs = append(errs, fsutil.MoveFile(g, filepath.Join(dir, k.File)))
		}
	}
	return errors.Join(errs...)
}

// Get a file's modification time, the zero time when it can't be read
func modTime(p string) time.Time {
	st, err := os.Lstat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// Point Steam's grid files for a shortcut at the pictures in dir, dropping links whose picture is gone
func Link(appid uint32, dir, grid string) error {
	if err := os.MkdirAll(grid, 0o777); err != nil {
		return err
	}

	// One link per picture that exists
	var errs []error
	for _, k := range Kinds {
		g, a := gridFile(grid, appid, k), filepath.Join(dir, k.File)
		if fsutil.Exists(a) {
			// A real file is a picture set from inside Steam that Adopt couldn't move, never delete it
			if st, err := os.Lstat(g); err == nil && st.Mode().IsRegular() {
				continue
			}
			if target, err := os.Readlink(g); err != nil || target != a {
				if err := os.Remove(g); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, err)
					continue
				}
				errs = append(errs, os.Symlink(a, g))
			}
		} else if fsutil.IsSymlink(g) {
			errs = append(errs, os.Remove(g))
		}
	}
	return errors.Join(errs...)
}

// Remove a shortcut's files from Steam's grid folder and delete dir, unless dir is ""
func Clean(appid uint32, dir, grid string) error {
	// Big Picture mode keeps its own copies under the 64-bit game ID
	big := uint64(appid)<<32 | 0x02000000
	var errs []error
	for _, id := range []string{strconv.FormatUint(uint64(appid), 10), strconv.FormatUint(big, 10)} {
		for _, f := range fsutil.GlobDir(grid, id+"*") {
			// 123p.jpg belongs to 123, 1234p.jpg doesn't
			if rest := strings.TrimPrefix(filepath.Base(f), id); rest == "" || rest[0] < '0' || rest[0] > '9' {
				errs = append(errs, os.Remove(f))
			}
		}
	}
	if dir != "" {
		errs = append(errs, os.RemoveAll(dir))
	}
	return errors.Join(errs...)
}
