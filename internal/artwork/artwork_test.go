package artwork

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ddrimus/steam-adopt/internal/fsutil"
	"github.com/ddrimus/steam-adopt/internal/store"
)

// Create a file with content, with any missing folders
func touch(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Create a store client whose API and CDN are both the given handler
func testStore(t *testing.T, h http.HandlerFunc) *store.Client {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := store.New()
	c.APIURL, c.CDNURL = srv.URL, srv.URL
	return c
}

// Read a file, "" when it's missing
func read(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// Test that pictures are adopted from Steam, linked into the grid and cleaned up
func TestLinkAdoptClean(t *testing.T) {
	root := t.TempDir()

	// A folder name with glob characters must be taken literally
	dir, grid := filepath.Join(root, "art", "Game [x]"), filepath.Join(root, "grid")
	const id = 3000000000
	touch(t, filepath.Join(dir, "cover.jpg"), "cover")
	touch(t, filepath.Join(dir, "hero.jpg"), "hero")
	touch(t, filepath.Join(dir, "icon-42.jpg"), "icon")

	// Set from inside Steam: should be moved into the artwork folder
	touch(t, filepath.Join(grid, "3000000000.jpg"), "banner from steam")

	// A stale link to a picture that's gone
	if err := os.Symlink(filepath.Join(dir, "logo.png"), filepath.Join(grid, "3000000000_logo.png")); err != nil {
		t.Fatal(err)
	}

	// Another shortcut whose ID starts with the same digits
	touch(t, filepath.Join(grid, "30000000001p.jpg"), "other game")

	if err := Adopt(id, dir, grid); err != nil {
		t.Fatal(err)
	}
	if read(filepath.Join(dir, "banner.jpg")) != "banner from steam" {
		t.Fatal("banner set in Steam wasn't adopted")
	}
	if err := Link(id, dir, grid); err != nil {
		t.Fatal(err)
	}
	for suffix, file := range map[string]string{"p.jpg": "cover.jpg", ".jpg": "banner.jpg", "_hero.jpg": "hero.jpg"} {
		if target, err := os.Readlink(filepath.Join(grid, "3000000000"+suffix)); err != nil || target != filepath.Join(dir, file) {
			t.Errorf("%s -> %q, %v", suffix, target, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(grid, "3000000000_logo.png")); err == nil {
		t.Error("stale logo link should be removed")
	}
	if !strings.HasSuffix(Icon(dir), "icon-42.jpg") {
		t.Errorf("Icon = %q", Icon(dir))
	}

	// Big Picture copy: 3000000000<<32 | 0x02000000
	big := filepath.Join(grid, "12884901888033554432p.jpg")
	touch(t, big, "big")
	if err := Clean(id, dir, grid); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(grid)
	if len(entries) != 1 || entries[0].Name() != "30000000001p.jpg" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("grid after clean: %v", names)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("artwork folder not removed")
	}
}

// Test that missing pictures are downloaded once and replaced when the store ID changes
func TestDownload(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	count := func() int { mu.Lock(); defer mu.Unlock(); n := len(requests); requests = nil; return n }
	c := testStore(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/IStoreBrowseService/GetItems/v1/":
			w.Write([]byte(`{"response":{"store_items":[{"assets":{"asset_url_format":"apps/7/${FILENAME}",` +
				`"library_capsule":"cap.jpg","header":"head.jpg","library_hero":"hero.jpg","library_logo":"missing.png","community_icon":"ic"}}]}}`))
		case strings.HasSuffix(r.URL.Path, "missing.png"):
			http.NotFound(w, r)
		default:
			w.Write([]byte("img:" + filepath.Base(r.URL.Path)))
		}
	})
	dir := filepath.Join(t.TempDir(), "Game")

	// A custom picture, never overwritten
	touch(t, filepath.Join(dir, "hero.jpg"), "custom")

	notes, err := Download(context.Background(), c, 7, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0] != "failed: logo.png" {
		t.Errorf("notes = %q", notes)
	}
	for file, want := range map[string]string{"cover.jpg": "img:cap.jpg", "banner.jpg": "img:head.jpg", "hero.jpg": "custom", "icon-7.jpg": "img:ic.jpg"} {
		if got := read(filepath.Join(dir, file)); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}

	// Everything required is there, so a rerun makes no requests
	count()
	if _, err := Download(context.Background(), c, 7, dir); err != nil || count() != 0 {
		t.Errorf("rerun made requests, %v", err)
	}

	// A different store ID replaces the downloaded artwork
	notes, err = Download(context.Background(), c, 8, dir)
	if err != nil || len(notes) == 0 || notes[0] != "replaced artwork from Steam ID 7" {
		t.Errorf("notes = %q, %v", notes, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "icon-7.jpg")); err == nil {
		t.Error("old icon kept")
	}
}

// Test that an app without an icon still has its store ID remembered
func TestDownloadWithoutIcon(t *testing.T) {
	// Store apps without a community icon must still have their ID remembered, so reruns stay offline
	var mu sync.Mutex
	n := 0
	c := testStore(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		if r.URL.Path == "/IStoreBrowseService/GetItems/v1/" {
			w.Write([]byte(`{"response":{"store_items":[{"assets":{"asset_url_format":"apps/9/${FILENAME}",` +
				`"library_capsule":"cap.jpg","header":"head.jpg","library_hero":"hero.jpg"}}]}}`))
			return
		}
		w.Write([]byte("img"))
	})
	dir := filepath.Join(t.TempDir(), "Game")

	if _, err := Download(context.Background(), c, 9, dir); err != nil {
		t.Fatal(err)
	}
	if SteamID(dir) != 9 {
		t.Errorf("SteamID = %d, want 9", SteamID(dir))
	}
	if Icon(dir) != "" {
		t.Errorf("Icon = %q without an icon", Icon(dir))
	}
	mu.Lock()
	n = 0
	mu.Unlock()
	if _, err := Download(context.Background(), c, 9, dir); err != nil || n != 0 {
		t.Errorf("rerun made %d requests, %v", n, err)
	}

	// Switching IDs still clears the old pictures
	if notes, _ := Download(context.Background(), c, 10, dir); len(notes) == 0 || notes[0] != "replaced artwork from Steam ID 9" {
		t.Errorf("notes = %q", notes)
	}
	if SteamID(dir) != 10 {
		t.Errorf("SteamID = %d after switching, want 10", SteamID(dir))
	}
}

// Test that a real picture in the grid folder is never replaced by a link
func TestLinkNeverDeletesRealFiles(t *testing.T) {
	root := t.TempDir()
	dir, grid := filepath.Join(root, "art"), filepath.Join(root, "grid")
	touch(t, filepath.Join(dir, "cover.jpg"), "custom")
	touch(t, filepath.Join(grid, "5p.jpg"), "set in steam")
	if err := Link(5, dir, grid); err != nil {
		t.Fatal(err)
	}
	if read(filepath.Join(grid, "5p.jpg")) != "set in steam" {
		t.Error("picture set in Steam was replaced")
	}
}

// Test that switching store IDs only deletes downloaded pictures, never ones put there by hand or from before
func TestDownloadKeepsOwnPicturesOnNewID(t *testing.T) {
	c := testStore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/IStoreBrowseService/GetItems/v1/" {
			w.Write([]byte(`{"response":{"store_items":[{"assets":{"asset_url_format":"apps/x/${FILENAME}",` +
				`"library_capsule":"cap.jpg","header":"head.jpg","library_hero":"hero.jpg"}}]}}`))
			return
		}
		w.Write([]byte("img:" + r.URL.Path))
	})
	dir := filepath.Join(t.TempDir(), "Game")

	if _, err := Download(context.Background(), c, 7, dir); err != nil {
		t.Fatal(err)
	}
	// Replaced by hand after the download
	touch(t, filepath.Join(dir, "cover.jpg"), "my cover")

	notes, err := Download(context.Background(), c, 8, dir)
	if err != nil {
		t.Fatal(err)
	}
	if read(filepath.Join(dir, "cover.jpg")) != "my cover" {
		t.Error("own cover deleted when the store ID changed")
	}
	if !strings.Contains(read(filepath.Join(dir, "banner.jpg")), "head.jpg") || len(notes) < 2 ||
		notes[1] != "kept pictures not downloaded for that ID, delete them to download the new ones: cover.jpg" {
		t.Errorf("notes = %q", notes)
	}

	// A folder from before the manifest existed: nothing can be told apart, so everything stays
	os.Remove(filepath.Join(dir, manifestFile))
	before := read(filepath.Join(dir, "banner.jpg"))
	notes, _ = Download(context.Background(), c, 9, dir)
	if read(filepath.Join(dir, "banner.jpg")) != before || read(filepath.Join(dir, "cover.jpg")) != "my cover" {
		t.Error("pictures from before the manifest deleted")
	}
	if len(notes) < 2 || !strings.HasPrefix(notes[1], "kept pictures not downloaded for that ID") {
		t.Errorf("notes = %q", notes)
	}
}

// Test that a search that found nothing is remembered for a week, for the same name only
func TestRecentlyNotFound(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Game")
	now := time.Now()
	if RecentlyNotFound(dir, "Game", now) {
		t.Error("not found without a record")
	}
	if err := MarkNotFound(dir, "Game"); err != nil {
		t.Fatal(err)
	}
	if !RecentlyNotFound(dir, "Game", now) || RecentlyNotFound(dir, "Other", now) {
		t.Error("record not matched by name")
	}
	if RecentlyNotFound(dir, "Game", now.Add(8*24*time.Hour)) {
		t.Error("record trusted after a week")
	}
	MarkNotFound(dir, "")
	if RecentlyNotFound(dir, "Game", now) {
		t.Error("record not cleared")
	}
}

// Test that the logo is found at its usual CDN names when the store doesn't list it, and its absence isn't a failure
func TestDownloadGuessesLogo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		serveLogo bool
		wantLogo  string
	}{{"logo served as logo.png", true, "png:logo.png"}, {"no logo at all", false, ""}} {
		c := testStore(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/IStoreBrowseService/GetItems/v1/":
				w.Write([]byte(`{"response":{"store_items":[{"assets":{"asset_url_format":"steam/apps/5/${FILENAME}?t=1",` +
					`"library_capsule":"cap.jpg","header":"head.jpg","library_hero":"hero.jpg"}}]}}`))
			case strings.HasSuffix(r.URL.Path, "/logo_2x.png"):
				http.NotFound(w, r)
			case strings.HasSuffix(r.URL.Path, "/steam/apps/5/logo.png") && tc.serveLogo:
				w.Write([]byte("png:logo.png"))
			case strings.HasSuffix(r.URL.Path, ".png"):
				http.NotFound(w, r)
			default:
				w.Write([]byte("img"))
			}
		})
		dir := filepath.Join(t.TempDir(), "Game")
		notes, err := Download(context.Background(), c, 5, dir)
		if err != nil || len(notes) != 0 {
			t.Errorf("%s: notes %q, err %v", tc.name, notes, err)
		}
		if got := read(filepath.Join(dir, "logo.png")); got != tc.wantLogo {
			t.Errorf("%s: logo.png = %q, want %q", tc.name, got, tc.wantLogo)
		}
	}
}

// Test that a picture Steam saved as PNG is kept like a JPG one, the newer of the two winning
func TestAdoptPNGSetInSteam(t *testing.T) {
	grid, dir := t.TempDir(), filepath.Join(t.TempDir(), "art")
	const id = 2000000000
	os.WriteFile(filepath.Join(grid, "2000000000p.jpg"), []byte("old jpg"), 0o644)
	os.WriteFile(filepath.Join(grid, "2000000000p.png"), []byte("new png"), 0o644)
	os.Chtimes(filepath.Join(grid, "2000000000p.jpg"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	os.WriteFile(filepath.Join(grid, "2000000000_logo.png"), []byte("logo"), 0o644)
	if err := Adopt(id, dir, grid); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cover.jpg")); string(b) != "new png" {
		t.Errorf("cover %q, want the newer PNG", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "logo.png")); string(b) != "logo" {
		t.Errorf("logo %q", b)
	}
	if left := fsutil.GlobDir(grid, "*"); len(left) != 0 {
		t.Errorf("left in grid: %v", left)
	}
}
