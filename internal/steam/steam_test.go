package steam

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Test that shortcut IDs match the ones Steam generates
func TestShortcutAppID(t *testing.T) {
	// Value from Python's zlib.crc32, which Steam's own IDs match
	if got := ShortcutAppID(`"/games/A/a.exe"`, "A"); got != 2885987580 {
		t.Fatalf("got %d", got)
	}
}

// Test that the most recently logged-in account's folder is found
func TestActiveUserdata(t *testing.T) {
	dir := t.TempDir()
	write := func(txt string) {
		os.MkdirAll(filepath.Join(dir, "config"), 0o755)
		os.WriteFile(filepath.Join(dir, "config/loginusers.vdf"), []byte(txt), 0o644)
	}
	if _, err := ActiveUserdata(dir); err == nil || !strings.Contains(err.Error(), "login file wasn't found") {
		t.Errorf("missing file: %v", err)
	}
	write(`"users" {}`)
	if _, err := ActiveUserdata(dir); err == nil || !strings.Contains(err.Error(), "no Steam account") {
		t.Errorf("no account: %v", err)
	}
	write(`"users"
{
	"76561197960265729"
	{
		"AccountName"		"old"
		"MostRecent"		"0"
	}
	"76561198350641752"
	{
		"AccountName"		"current"
		"MostRecent"		"1"
	}
}`)
	got, err := ActiveUserdata(dir)
	if want := filepath.Join(dir, "userdata", "390376024"); err != nil || got != want {
		t.Errorf("got %q, %v, want %q", got, err, want)
	}
}

// Steam config with a default tool and two shortcut mappings
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
					"111"
					{
						"name"		"proton_8"
						"config"		""
						"priority"		"250"
					}
					"222"
					{
						"name"		"proton_9"
						"config"		""
						"priority"		"250"
					}
				}
			}
		}
	}
	"Other"
	{
		"111"
		{
			"keep"		"me"
		}
	}
}
`

// Test that the default Proton is read from config.vdf's mapping block only, with a fallback
func TestDefaultProton(t *testing.T) {
	if got := DefaultProton(configVDF); got != "proton_9" {
		t.Errorf("got %q", got)
	}
	if got := DefaultProton(`"x" {}`); got != FallbackProton {
		t.Errorf("got %q", got)
	}
	outside := "\"x\"\n{\n\t\"CompatToolMapping\"\n\t{\n\t}\n\t\"Other\"\n\t{\n\t\t\"0\"\n\t\t{\n\t\t\t\"name\"\t\t\"wrong\"\n\t\t}\n\t}\n}\n"
	if got := DefaultProton(outside); got != FallbackProton {
		t.Errorf("got %q from outside the mapping block", got)
	}
}

// Test that a shortcut's tool is read from the mapping block only, "none" without an entry, and a missing block is told apart
func TestProton(t *testing.T) {
	for _, tc := range []struct {
		appid uint32
		want  string
	}{{111, "proton_8"}, {222, "proton_9"}, {333, "none"}, {11, "none"}} {
		if got, found := Proton(configVDF, tc.appid); got != tc.want || !found {
			t.Errorf("appid %d: got %q, %v, want %q", tc.appid, got, found, tc.want)
		}
	}
	if _, found := Proton(`"x" {}`, 111); found {
		t.Error("mapping block found where there is none")
	}
}

// Test that Valve's tools always count, and custom ones by folder name or the name in their compatibilitytool.vdf
func TestToolInstalled(t *testing.T) {
	steamDir := t.TempDir()
	tools := filepath.Join(steamDir, "compatibilitytools.d")
	os.MkdirAll(filepath.Join(tools, "GE-Proton10-1"), 0o755)
	os.MkdirAll(filepath.Join(tools, "renamed"), 0o755)
	os.WriteFile(filepath.Join(tools, "renamed/compatibilitytool.vdf"),
		[]byte("\"compatibilitytools\"\n{\n\t\"compat_tools\"\n\t{\n\t\t\"Proton-Custom\"\n\t\t{\n\t\t}\n\t}\n}\n"), 0o644)
	for tool, want := range map[string]bool{
		"proton_experimental": true, "Proton_9": true, "SteamLinuxRuntime_sniper": true,
		"GE-Proton10-1": true, "Proton-Custom": true, "GE-Proton9-1": false, "Proton-Cust": false,
	} {
		if got := ToolInstalled(steamDir, tool); got != want {
			t.Errorf("%s: got %v, want %v", tool, got, want)
		}
	}
}

// Test that Proton mappings are added, removed and kept only inside the mapping block
func TestApplyProton(t *testing.T) {
	ind := "\t\t\t\t\t"
	entry := func(id, tool string) string {
		return ind + `"` + id + "\"\n" + ind + "{\n" + ind + "\t\"name\"\t\t\"" + tool + "\"\n" +
			ind + "\t\"config\"\t\t\"\"\n" + ind + "\t\"priority\"\t\t\"250\"\n" + ind + "}\n"
	}
	var p ProtonMap

	// One new, one removed, one already right, one absent
	p.Set(333, "GE-Proton10-1")
	p.Remove(111)
	p.Set(222, "proton_9")
	p.Remove(444)

	got, changed, found := ApplyProton(configVDF, &p)
	if !found {
		t.Fatal("block not found")
	}
	if !reflect.DeepEqual(changed, []uint32{333, 111}) {
		t.Errorf("changed = %v", changed)
	}
	if !strings.Contains(got, "\t\t\t\t\"CompatToolMapping\"\n\t\t\t\t{\n"+entry("333", "GE-Proton10-1")+ind+"\"0\"\n") {
		t.Errorf("new entry not at the top of the block:\n%s", got)
	}
	if strings.Contains(got, "proton_8") {
		t.Error("removed entry still there")
	}
	if !strings.Contains(got, entry("222", "proton_9")) {
		t.Error("unchanged entry lost")
	}
	if !strings.Contains(got, "\"keep\"\t\t\"me\"") {
		t.Error("an entry with the same ID outside the mapping block was touched")
	}

	// Applying again changes nothing
	again, changed, _ := ApplyProton(got, &p)
	if again != got || len(changed) != 0 {
		t.Errorf("not idempotent, changed %v", changed)
	}
}

// Test that an entry listed twice is set or removed as one, so a stale copy can't come back on the next run
func TestApplyProtonDropsDuplicates(t *testing.T) {
	dup := "\"CompatToolMapping\"\n\t{\n\t\t\"111\"\n\t\t{\n\t\t\t\"name\"\t\t\"proton_8\"\n\t\t}\n" +
		"\t\t\"111\"\n\t\t{\n\t\t\t\"name\"\t\t\"proton_7\"\n\t\t}\n\t}\n"
	for _, tool := range []string{"proton_9", ""} {
		var p ProtonMap
		want := "none"
		if tool == "" {
			p.Remove(111)
		} else {
			p.Set(111, tool)
			want = tool
		}
		got, _, _ := ApplyProton(dup, &p)
		if strings.Count(got, "\"111\"") > 1 {
			t.Errorf("tool %q: duplicate kept:\n%s", tool, got)
		}
		if have, _ := Proton(got, 111); have != want {
			t.Errorf("tool %q: reads %q", tool, have)
		}
	}
}

// Test that a config.vdf without a mapping block is left untouched
func TestApplyProtonNoBlock(t *testing.T) {
	var p ProtonMap
	p.Set(1, "x")
	txt := "\"InstallConfigStore\"\n{\n}\n"
	got, changed, found := ApplyProton(txt, &p)
	if found || changed != nil || got != txt {
		t.Errorf("got %q %v %v", got, changed, found)
	}
}

// Test that Steam's global default mapping is never touched
func TestProtonMapIgnoresAppZero(t *testing.T) {
	var p ProtonMap
	p.Remove(0)
	p.Set(0, "proton_9")
	if got, changed, _ := ApplyProton(configVDF, &p); got != configVDF || len(changed) != 0 {
		t.Errorf("Steam's global default mapping was touched: %v", changed)
	}
}

// Test that the account is only named when several have logged in, and that it's the most recent one
func TestSharedAccountName(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "config"), 0o755)
	write := func(txt string) { os.WriteFile(filepath.Join(dir, "config/loginusers.vdf"), []byte(txt), 0o644) }
	write(`"users" { "76561197960265729" { "PersonaName" "Solo" "MostRecent" "1" } }`)
	if got := SharedAccountName(dir); got != "" {
		t.Errorf("single account named %q", got)
	}
	write(`"users" {
	"76561197960265729" { "PersonaName" "Kid" "MostRecent" "0" }
	"76561198350641752" { "PersonaName" "Parent" "MostRecent" "1" }
}`)
	if got := SharedAccountName(dir); got != "Parent" {
		t.Errorf("got %q, want Parent", got)
	}
}

// Test that steam.exe is found in tasklist's output, and not in the plain text it prints when nothing matched
func TestListsSteam(t *testing.T) {
	if !listsSteam("\"steam.exe\",\"8124\",\"Console\",\"1\",\"78,212 K\"\r\n") {
		t.Error("steam.exe row not seen")
	}
	if listsSteam("INFO: No tasks are running which match the specified criteria.\r\n") ||
		listsSteam("\"steamwebhelper.exe\",\"9\",\"Console\",\"1\",\"1 K\"") {
		t.Error("no steam.exe taken for one")
	}
}

// Test that Steam's folder is read from reg query's output, which Steam writes with forward slashes
func TestRegValue(t *testing.T) {
	out := "\r\nHKEY_CURRENT_USER\\Software\\Valve\\Steam\r\n    SteamPath    REG_SZ    c:/program files (x86)/steam\r\n\r\n"
	if got, want := regValue(out), filepath.Clean("c:/program files (x86)/steam"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := regValue("ERROR: The system was unable to find the specified registry key or value.\r\n"); got != "" {
		t.Errorf("error output gave %q", got)
	}
}

// Fuzz test that Proton edits stay inside the mapping block, read back as set and change nothing when applied again
func FuzzApplyProton(f *testing.F) {
	f.Add(configVDF, uint32(333), "GE-Proton10-1", uint32(111), true)
	f.Add("\"x\"\n{\n\t\"CompatToolMapping\"\n\t{\n\t}\n}\n", uint32(5), "proton_9", uint32(5), false)
	// An entry listed twice, whose second copy came back on the next run
	f.Add("\"CompatToolMapping\"\n\t{\n\t\t\"111\"\n\t\t{\n\t\t}\n\t\t\"111\"\n\t\t{\n\t\t}\n\t}", uint32(333), "0", uint32(111), true)
	f.Fuzz(func(t *testing.T, txt string, a uint32, tool string, b uint32, removeB bool) {
		if tool == "" || a == 0 || b == 0 || a == b {
			return
		}
		for _, r := range tool {
			if r == '"' || r == '\\' || r <= ' ' {
				return
			}
		}
		var p ProtonMap
		p.Set(a, tool)
		if removeB {
			p.Remove(b)
		} else {
			p.Set(b, "proton_9")
		}
		start, end, _, ok := mappingBlock(txt)
		got, _, found := ApplyProton(txt, &p)
		if !found {
			if got != txt {
				t.Fatal("changed without a block")
			}
			return
		}
		ns, ne, _, ok2 := mappingBlock(got)
		if !ok || !ok2 || txt[:start] != got[:ns] || txt[end:] != got[ne:] {
			t.Fatalf("text outside the block changed:\n%q\n->\n%q", txt, got)
		}
		if have, _ := Proton(got, a); have != tool {
			t.Fatalf("appid %d reads %q, want %q:\n%q", a, have, tool, got)
		}
		wantB := "proton_9"
		if removeB {
			wantB = "none"
		}
		if have, _ := Proton(got, b); have != wantB {
			t.Fatalf("appid %d reads %q, want %q:\n%q\n->\n%q", b, have, wantB, txt, got)
		}
		if again, changed, _ := ApplyProton(got, &p); again != got || len(changed) != 0 {
			t.Fatalf("not idempotent:\n%q", got)
		}
	})
}
