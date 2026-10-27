package store

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Create a client whose store, API and CDN are all the given handler
func testClient(t *testing.T, h http.HandlerFunc) *Client {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New()
	c.StoreURL, c.APIURL, c.CDNURL = srv.URL, srv.URL, srv.URL
	return c
}

// Test that the search prefers an exact match, skips demos and reports failures
func TestSearchID(t *testing.T) {
	results := map[string]string{
		"exact":    `{"items":[{"type":"app","name":"Exact Game Demo","id":1},{"type":"app","name":"EXACT: game","id":2}]}`,
		"guess":    `{"items":[{"type":"dlc","name":"Guess Pack","id":9},{"type":"app","name":"Guess Demo","id":3},{"type":"app","name":"Guess Game II","id":4}]}`,
		"onlydemo": `{"items":[{"type":"app","name":"Some Game Demo","id":5}]}`,
		"broken":   `not json`,
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/storesearch/" || r.Header.Get("User-Agent") == "" {
			http.NotFound(w, r)
			return
		}
		if body, ok := results[r.URL.Query().Get("term")]; ok {
			w.Write([]byte(body))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	ctx := context.Background()
	for _, tc := range []struct {
		term string
		id   int64
		info string
	}{
		{"exact", 2, "EXACT: game"},
		{"guess", 4, "Guess Game II (best guess)"},
		{"onlydemo", 0, "not found on Steam"},
		{"broken", 0, "store search failed"},
		{"error", 0, "store search failed (HTTP Error 500"},
	} {
		id, info := c.SearchID(ctx, tc.term)
		if id != tc.id || !strings.HasPrefix(info, tc.info) {
			t.Errorf("SearchID(%q) = %d, %q; want %d, %q", tc.term, id, info, tc.id, tc.info)
		}
	}
}

// Test that asset names become CDN URLs, and an app without artwork is an error
func TestAssetsAndURLs(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("input_json"), `"appid":7`) {
			w.Write([]byte(`{"response":{"store_items":[{"assets":{"asset_url_format":"steam/apps/7/${FILENAME}?t=1",` +
				`"header":"header.jpg","library_hero_2x":"hero_2x.jpg","community_icon":"abc","n":5}}]}}`))
			return
		}
		w.Write([]byte(`{"response":{"store_items":[{}]}}`))
	})
	a, err := c.Assets(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.URL(a, "header_2x", "header"); got != c.CDNURL+"/store_item_assets/steam/apps/7/header.jpg" {
		t.Errorf("URL = %q", got)
	}
	if got := c.URL(a, "library_logo"); got != "" {
		t.Errorf("missing asset gave %q", got)
	}
	if got := c.IconURL(a, 7); got != c.CDNURL+"/community_assets/images/apps/7/abc.jpg" {
		t.Errorf("IconURL = %q", got)
	}
	if _, err := c.Assets(context.Background(), 8); !errors.Is(err, ErrNoArtwork) {
		t.Errorf("no assets: got %v", err)
	}
}

// Test that release tags are dropped from folder names
func TestUntag(t *testing.T) {
	for in, want := range map[string]string{
		"Game [GOG]":                  "Game",
		"Game (2019) [v1.2] {Repack}": "Game",
		"Half (Life) Two":             "Half Two",
		"[GOG]":                       "[GOG]",
		"Plain Game":                  "Plain Game",
		"Game v1.0.4":                 "Game",
		"Game_v2.1b [GOG]":            "Game",
		"Game 1.2.3":                  "Game",
		"Saga 2":                      "Saga 2",
		"1.0":                         "1.0",
	} {
		if got := Untag(in); got != want {
			t.Errorf("Untag(%q) = %q, want %q", in, got, want)
		}
	}
}

// Test that logo URLs come from the listed asset when there is one, and are guessed otherwise
func TestLogoURLs(t *testing.T) {
	c := &Client{CDNURL: "https://cdn"}
	if urls, guessed := c.LogoURLs(Assets{"asset_url_format": "steam/apps/5/${FILENAME}", "library_logo": "l.png"}, 5); guessed ||
		len(urls) != 1 || urls[0] != "https://cdn/store_item_assets/steam/apps/5/l.png" {
		t.Errorf("listed: %v %v", urls, guessed)
	}
	urls, guessed := c.LogoURLs(Assets{"asset_url_format": "steam/apps/5/abc/${FILENAME}?t=1"}, 5)
	want := []string{"https://cdn/store_item_assets/steam/apps/5/abc/logo_2x.png", "https://cdn/store_item_assets/steam/apps/5/abc/logo.png",
		"https://cdn/store_item_assets/steam/apps/5/logo_2x.png", "https://cdn/store_item_assets/steam/apps/5/logo.png"}
	if !guessed || strings.Join(urls, " ") != strings.Join(want, " ") {
		t.Errorf("guessed: %v %v", urls, guessed)
	}
}

// Test that rate limiting and brief outages are retried a few times, and other errors aren't
func TestGetRetries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fails     int
		status    int
		wantTries int
		wantOK    bool
	}{
		{"rate limited twice", 2, http.StatusTooManyRequests, 3, true},
		{"unavailable once", 1, http.StatusServiceUnavailable, 2, true},
		{"rate limited for good", 99, http.StatusTooManyRequests, 4, false},
		{"not found isn't retried", 99, http.StatusNotFound, 1, false},
	} {
		var mu sync.Mutex
		tries := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			tries++
			n := tries
			mu.Unlock()
			if n <= tc.fails {
				w.WriteHeader(tc.status)
				return
			}
			w.Write([]byte("ok"))
		}))
		c := New()
		c.Backoff = time.Millisecond
		body, err := c.Get(context.Background(), srv.URL)
		srv.Close()
		if tries != tc.wantTries || (err == nil) != tc.wantOK || (tc.wantOK && string(body) != "ok") {
			t.Errorf("%s: %d tries, body %q, err %v", tc.name, tries, body, err)
		}
	}
}

// Test that waiting to retry ends at once on Ctrl-C
func TestGetRetryInterrupted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := New().Get(ctx, srv.URL); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("kept waiting after the context ended")
	}
}
