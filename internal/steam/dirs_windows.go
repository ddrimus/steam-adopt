// Steam's install folder on Windows, from the registry or the default place

package steam

import (
	"os"
	"os/exec"
	"path/filepath"
)

// Places Steam is installed on Windows: where its installer says, then the default folder
func dirs(string) []string {
	var places []string
	if out, err := exec.Command("reg", "query", `HKCU\Software\Valve\Steam`, "/v", "SteamPath").Output(); err == nil {
		if p := regValue(string(out)); p != "" {
			places = append(places, p)
		}
	}
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
		if pf := os.Getenv(env); pf != "" {
			places = append(places, filepath.Join(pf, "Steam"))
		}
	}
	return append(places, `C:\Program Files (x86)\Steam`)
}
