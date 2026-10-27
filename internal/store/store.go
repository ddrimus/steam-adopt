// Public Steam store client: app search by name and artwork assets
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Error for when the store has no artwork for an app
var ErrNoArtwork = errors.New("no artwork on the Steam store for this ID")

// Largest response read, well above any artwork file
const maxBody = 64 << 20

// Steam store client, its URLs pointable elsewhere for tests
type Client struct {
	HTTP     *http.Client
	StoreURL string
	APIURL   string
	CDNURL   string
	// First wait before retrying a request turned away, doubled each time, zero using a second
	Backoff time.Duration
}

// Tries for a request the store turns away for now, as when a big library's first run hits its rate limit
const attempts = 4

// Longest wait asked for by Retry-After that's honored
const maxRetryAfter = 30 * time.Second

// Create a client for the real Steam store
func New() *Client {
	return &Client{
		HTTP:     &http.Client{Timeout: 20 * time.Second, Transport: transport()},
		StoreURL: "https://store.steampowered.com",
		APIURL:   "https://api.steampowered.com",
		CDNURL:   "https://shared.fastly.steamstatic.com",
	}
}

// Fetch a URL with the browser-like User-Agent the store API requires, retrying while it's rate limiting
func (c *Client) Get(ctx context.Context, u string) ([]byte, error) {
	wait := c.Backoff
	if wait <= 0 {
		wait = time.Second
	}
	for try := 1; ; try++ {
		body, retryAfter, err := c.get(ctx, u)
		if retryAfter < 0 || try == attempts {
			return body, err
		}
		if retryAfter > 0 {
			wait = min(retryAfter, maxRetryAfter)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
		wait *= 2
	}
}

// Statuses that mean try again shortly
var retryable = map[int]bool{http.StatusTooManyRequests: true, http.StatusBadGateway: true,
	http.StatusServiceUnavailable: true, http.StatusGatewayTimeout: true}

// Fetch a URL once, with the wait the server asked for before a retry, 0 for none given, or -1 for no retry
func (c *Client) get(ctx context.Context, u string) ([]byte, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, -1, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, -1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		err := fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		if !retryable[resp.StatusCode] {
			return nil, -1, err
		}
		after := time.Duration(0)
		if n, perr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); perr == nil && n > 0 {
			after = time.Duration(n) * time.Second
		}
		return nil, after, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, -1, err
	}
	if len(body) > maxBody {
		return nil, -1, fmt.Errorf("response larger than %d MiB", maxBody>>20)
	}
	return body, -1, nil
}

// Keep more idle connections than the default 2 per host, so parallel downloads reuse them
func transport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 8
	return t
}

// Store entries that aren't the game itself
var extraRe = regexp.MustCompile(`(?i)\b(demo|soundtrack|ost|dlc)\b`)

// Release tags like [GOG], (2019) or {v1.2}, and a trailing version like v1.2 or 1.0.4 but not a sequel's 2
var (
	tagRe     = regexp.MustCompile(`\s*(\[[^\]]*\]|\([^)]*\)|\{[^}]*\})\s*`)
	versionRe = regexp.MustCompile(`(?i)[\s_-]+(v\d+(\.\d+)*|\d+(\.\d+)+)[a-z]?$`)
)

// Drop release tags and a trailing version from a folder name, so "Game [GOG] v1.2" is searched as "Game"
func Untag(name string) string {
	out := strings.Join(strings.Fields(tagRe.ReplaceAllString(name, " ")), " ")
	out = versionRe.ReplaceAllString(out, "")
	if out != "" {
		return out
	}
	return name
}

// Keep only lowercase letters and digits, so "IRON NEST: Heavy..." matches "Iron Nest Heavy..."
func Norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SearchID's reason when the store answered but has no such game, as opposed to a failed search
const NotFound = "not found on Steam"

// Ending of SearchID's store name when no app matched the name exactly and the first likely one was taken
const BestGuess = " (best guess)"

// Look up a game's store ID by name, returning the ID and store name, or 0 and the reason
func (c *Client) SearchID(ctx context.Context, name string) (int64, string) {
	body, err := c.Get(ctx, c.StoreURL+"/api/storesearch/?term="+url.QueryEscape(name)+"&cc=us&l=english")
	var res struct {
		Items *[]struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"items"`
	}
	if err == nil {
		err = json.Unmarshal(body, &res)
	}
	if err == nil && res.Items == nil {
		err = errors.New("no items in the response")
	}
	if err != nil {
		return 0, fmt.Sprintf("store search failed (%v)", err)
	}

	// Apps only, an exact name match preferred over the first hit
	type app struct {
		id   int64
		name string
	}
	var apps []app
	for _, i := range *res.Items {
		if i.Type == "app" {
			apps = append(apps, app{i.ID, i.Name})
		}
	}
	for _, a := range apps {
		if Norm(a.name) == Norm(name) {
			return a.id, a.name
		}
	}

	// Demos and soundtracks often rank first for a close-but-not-exact name
	for _, a := range apps {
		if !extraRe.MatchString(a.name) {
			return a.id, a.name + BestGuess
		}
	}
	return 0, NotFound
}

// An app's artwork file names by kind, plus asset_url_format and community_icon
type Assets map[string]string

// Get a store app's artwork, which newer games keep under hashed folders plain CDN URLs miss
func (c *Client) Assets(ctx context.Context, appid int64) (Assets, error) {
	q, err := json.Marshal(map[string]any{
		"ids":          []map[string]any{{"appid": appid}},
		"context":      map[string]any{"language": "english", "country_code": "US"},
		"data_request": map[string]any{"include_assets": true},
	})
	if err != nil {
		return nil, err
	}
	body, err := c.Get(ctx, c.APIURL+"/IStoreBrowseService/GetItems/v1/?input_json="+url.QueryEscape(string(q)))
	if err != nil {
		return nil, err
	}
	var res struct {
		Response struct {
			StoreItems []struct {
				Assets map[string]any `json:"assets"`
			} `json:"store_items"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}

	// String values only, and no artwork without a URL format to build links from
	a := Assets{}
	if items := res.Response.StoreItems; len(items) > 0 {
		for k, v := range items[0].Assets {
			if s, ok := v.(string); ok {
				a[k] = s
			}
		}
	}
	if a["asset_url_format"] == "" {
		return nil, ErrNoArtwork
	}
	return a, nil
}

// Build the URL of the first of keys the app has, or "" when it has none of them
func (c *Client) URL(a Assets, keys ...string) string {
	base := c.CDNURL + "/store_item_assets/" + strings.SplitN(a["asset_url_format"], "${FILENAME}", 2)[0]
	for _, k := range keys {
		if a[k] != "" {
			return base + a[k]
		}
	}
	return ""
}

// List the logo URLs to try, best first, and whether they're guesses, since the API rarely lists library_logo now
func (c *Client) LogoURLs(a Assets, appid int64) ([]string, bool) {
	if u := c.URL(a, "library_logo_2x", "library_logo"); u != "" {
		return []string{u}, false
	}
	bases := []string{c.CDNURL + "/store_item_assets/" + strings.SplitN(a["asset_url_format"], "${FILENAME}", 2)[0]}
	if plain := c.CDNURL + "/store_item_assets/steam/apps/" + strconv.FormatInt(appid, 10) + "/"; plain != bases[0] {
		bases = append(bases, plain)
	}
	var out []string
	for _, b := range bases {
		out = append(out, b+"logo_2x.png", b+"logo.png")
	}
	return out, true
}

// Build the URL of the app's community icon, or "" when it has none
func (c *Client) IconURL(a Assets, appid int64) string {
	if a["community_icon"] == "" {
		return ""
	}
	return c.CDNURL + "/community_assets/images/apps/" + strconv.FormatInt(appid, 10) + "/" + a["community_icon"] + ".jpg"
}
