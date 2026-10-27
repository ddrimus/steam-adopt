// Steam's files: the logged-in account, shortcut IDs and the Proton mapping in config.vdf
package steam

import (
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Steam's 64-bit account IDs are the 32-bit userdata folder number plus this base
const steamID64Base = 76561197960265728

// Used when Steam has no default compatibility tool set
const FallbackProton = "proton_experimental"

// Flatpak ID of the Steam client
const FlatpakID = "com.valvesoftware.Steam"

// Find Steam's data folder: the known location whose login changed last, else the first one
func FindDir(home string) string {
	places := dirs(home)
	best, newest := places[0], int64(-1)
	for _, p := range places {
		// A leftover install from before a switch to or from Flatpak has an older login
		if st, err := os.Stat(filepath.Join(p, "config/loginusers.vdf")); err == nil && st.ModTime().UnixNano() > newest {
			best, newest = p, st.ModTime().UnixNano()
		}
	}
	return best
}

// Get the folder in reg query's output for a REG_SZ value, "" when there is none
func regValue(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if _, v, ok := strings.Cut(line, "REG_SZ"); ok {
			if v = strings.TrimSpace(v); v != "" {
				return filepath.Clean(v)
			}
		}
	}
	return ""
}

// Check whether a Steam data folder belongs to the Flatpak client
func IsFlatpak(dir string) bool {
	return strings.Contains(filepath.ToSlash(dir), "/.var/app/"+FlatpakID+"/")
}

// Account IDs in loginusers.vdf, and the one marked most recent
var (
	loginIDRe    = regexp.MustCompile(`"(7656\d{13})"`)
	mostRecentRe = regexp.MustCompile(`"(7656\d{13})"\s*\{[^}]*"MostRecent"\s*"1"`)
)

// Find the userdata folder of the most recently logged-in account
func ActiveUserdata(steamDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(steamDir, "config/loginusers.vdf"))
	if err != nil {
		return "", fmt.Errorf("steam's login file wasn't found in %s: install Steam, log in once, then run steam-adopt again", steamDir)
	}
	txt := string(data)
	ids := loginIDRe.FindAllStringSubmatch(txt, -1)
	if len(ids) == 0 {
		return "", fmt.Errorf("no Steam account found, log in to Steam once first")
	}
	id := ids[0][1]
	if m := mostRecentRe.FindStringSubmatch(txt); m != nil {
		id = m[1]
	}
	sid, err := strconv.ParseUint(id, 10, 64)
	if err != nil || sid < steamID64Base {
		return "", fmt.Errorf("invalid Steam account ID %s in loginusers.vdf", id)
	}
	return filepath.Join(steamDir, "userdata", strconv.FormatUint(sid-steamID64Base, 10)), nil
}

// One account's block in loginusers.vdf, and its shown name
var (
	accountRe = regexp.MustCompile(`"7656\d{13}"\s*\{([^}]*)\}`)
	personaRe = regexp.MustCompile(`"PersonaName"\s*"([^"]*)"`)
	recentRe  = regexp.MustCompile(`"MostRecent"\s*"1"`)
)

// Get the most recent account's shown name when several have logged in, "" with a single account
func SharedAccountName(steamDir string) string {
	data, err := os.ReadFile(filepath.Join(steamDir, "config/loginusers.vdf"))
	if err != nil {
		return ""
	}
	blocks := accountRe.FindAllStringSubmatch(string(data), -1)
	if len(blocks) < 2 {
		return ""
	}
	name := ""
	for i, b := range blocks {
		m := personaRe.FindStringSubmatch(b[1])
		if m != nil && (i == 0 || recentRe.MatchString(b[1])) {
			name = m[1]
		}
	}
	return name
}

// Compute the ID Steam generates for a shortcut from its quoted exe and name
func ShortcutAppID(exe, name string) uint32 {
	return crc32.ChecksumIEEE([]byte(exe+name)) | 0x80000000
}

// App 0's tool inside the mapping block
var defaultProtonRe = regexp.MustCompile(`(?m)^\s*"0"\s*\{\s*"name"\s*"([^"]+)"`)

// Get the tool picked under "Enable Steam Play for all other titles", stored as app 0
func DefaultProton(configVDF string) string {
	if start, end, _, ok := mappingBlock(configVDF); ok {
		if m := defaultProtonRe.FindStringSubmatch(configVDF[start:end]); m != nil {
			return m[1]
		}
	}
	return FallbackProton
}

// Ordered set of wanted Proton mappings by shortcut appid, a nil tool meaning remove
type ProtonMap struct {
	order []uint32
	tools map[uint32]*string
}

// Record the wanted tool for a shortcut, keeping first-seen order
func (p *ProtonMap) put(appid uint32, tool *string) {
	// App 0 is Steam's global default, never a shortcut
	if appid == 0 {
		return
	}
	if p.tools == nil {
		p.tools = map[uint32]*string{}
	}
	if _, ok := p.tools[appid]; !ok {
		p.order = append(p.order, appid)
	}
	p.tools[appid] = tool
}

// Map a shortcut to a compatibility tool
func (p *ProtonMap) Set(appid uint32, tool string) { p.put(appid, &tool) }

// Drop a shortcut's mapping so it runs without a compatibility tool
func (p *ProtonMap) Remove(appid uint32) { p.put(appid, nil) }

// Opening of the CompatToolMapping block, capturing its indentation
var mappingRe = regexp.MustCompile(`"CompatToolMapping"\s*\n(\t*)\{\n`)

// Locate the inside of the CompatToolMapping block and the indentation of its entries
func mappingBlock(txt string) (start, end int, ind string, ok bool) {
	m := mappingRe.FindStringSubmatchIndex(txt)
	if m == nil {
		return 0, 0, "", false
	}
	outer := txt[m[2]:m[3]]
	start = m[1]
	i := strings.Index(txt[start-1:], "\n"+outer+"}")
	if i < 0 {
		return 0, 0, "", false
	}
	return start, start + i, outer + "\t", true
}

// Locate an entry whose header starts a line in block, or -1 when there is none
func findEntry(block, head, closing string) (int, int) {
	for from := 0; ; {
		n := strings.Index(block[from:], head)
		if n < 0 {
			return -1, -1
		}
		s := from + n
		if s == 0 || block[s-1] == '\n' {
			e := strings.Index(block[s+len(head)-1:], closing)
			if e < 0 {
				return -1, -1
			}
			return s, s + len(head) - 1 + e + len(closing)
		}
		from = s + 1
	}
}

// A mapping entry's tool name
var toolNameRe = regexp.MustCompile(`"name"\s*"([^"]*)"`)

// Get the tool a shortcut is mapped to, "none" without one, and whether config.vdf has a mapping block at all
func Proton(txt string, appid uint32) (string, bool) {
	start, end, ind, ok := mappingBlock(txt)
	if !ok {
		return "", false
	}
	block := txt[start:end]
	head := ind + `"` + strconv.FormatUint(uint64(appid), 10) + "\"\n" + ind + "{\n"
	s, e := findEntry(block, head, "\n"+ind+"}\n")
	if s < 0 {
		return "none", true
	}
	if m := toolNameRe.FindStringSubmatch(block[s:e]); m != nil && m[1] != "" {
		return m[1], true
	}
	return "none", true
}

// Apply the wanted mappings to config.vdf text, reporting changed appids and whether the block exists
func ApplyProton(txt string, wanted *ProtonMap) (string, []uint32, bool) {
	start, end, ind, ok := mappingBlock(txt)
	if !ok {
		return txt, nil, false
	}

	// Only edit inside the mapping block, the same appid also appears in other sections of config.vdf
	block := txt[start:end]
	closing := "\n" + ind + "}\n"
	changed := []uint32{}
	for _, appid := range wanted.order {
		id := strconv.FormatUint(uint64(appid), 10)
		head := ind + `"` + id + "\"\n" + ind + "{\n"
		repl := ""
		if tool := wanted.tools[appid]; tool != nil {
			repl = head +
				ind + "\t\"name\"\t\t\"" + *tool + "\"\n" +
				ind + "\t\"config\"\t\t\"\"\n" +
				ind + "\t\"priority\"\t\t\"250\"\n" +
				ind + "}\n"
		}
		if s, e := findEntry(block, head, closing); s >= 0 {
			// Steam never writes an entry twice, but a hand-edited file can, and Steam could read the stale one
			rest := block[e:]
			for {
				ds, de := findEntry(rest, head, closing)
				if ds < 0 {
					break
				}
				rest = rest[:ds] + rest[de:]
			}
			if next := block[:s] + repl + rest; next != block {
				block = next
				changed = append(changed, appid)
			}
		} else if repl != "" {
			block = repl + block
			changed = append(changed, appid)
		}
	}
	return txt[:start] + block + txt[end:], changed, true
}

// Folders Steam loads custom compatibility tools from, besides its own data folder's
var systemToolDirs = []string{"/usr/share/steam/compatibilitytools.d", "/usr/local/share/steam/compatibilitytools.d"}

// Check whether a compatibility tool can be used: Valve's own, or one in compatibilitytools.d by folder or vdf name
func ToolInstalled(steamDir, tool string) bool {
	if l := strings.ToLower(tool); strings.HasPrefix(l, "proton_") || strings.HasPrefix(l, "steamlinuxruntime") {
		return true
	}
	quoted := `"` + tool + `"`
	for _, dir := range append([]string{filepath.Join(steamDir, "compatibilitytools.d")}, systemToolDirs...) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Name() == tool {
				return true
			}
			if b, err := os.ReadFile(filepath.Join(dir, e.Name(), "compatibilitytool.vdf")); err == nil && strings.Contains(string(b), quoted) {
				return true
			}
		}
	}
	return false
}
