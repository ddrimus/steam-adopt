// Entry point for the steam-adopt CLI
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ddrimus/steam-adopt/internal/app"
	"github.com/ddrimus/steam-adopt/internal/steam"
	"github.com/ddrimus/steam-adopt/internal/store"
)

// Set at build time with -ldflags "-X main.version=..."
var version = "dev"

// Run steam-adopt and exit with its exit code
func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// Parse the flags and run one sync, returning the process exit code
func run(args []string, stdout, stderr io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "Can't find the home folder:", err)
		return 1
	}

	// Flags and usage text
	flags := flag.NewFlagSet("steam-adopt", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var dry, yes, showVersion bool
	games := os.Getenv("STEAM_ADOPT_GAMES")
	if games == "" {
		games = app.DefaultPaths(home).Games
	}
	flags.StringVar(&games, "games", games, "")
	flags.BoolVar(&dry, "n", false, "")
	flags.BoolVar(&dry, "dry-run", false, "")
	flags.BoolVar(&yes, "y", false, "")
	flags.BoolVar(&yes, "yes", false, "")
	flags.BoolVar(&showVersion, "version", false, "")
	flags.Usage = func() {
		fmt.Fprintf(stderr, `usage: steam-adopt [-n] [-y] [--games DIR] [-h] [--version]

Add every game folder inside the games folder to Steam as a non-Steam game, with artwork,
and keep Steam in sync with it on every run. Settings are in _steam-adopt/steam-adopt.conf
inside the games folder.

options:
  -n, --dry-run  show what would change without touching anything
  -y, --yes      remove skipped and deleted games from Steam without asking
  --games DIR    folder holding one subfolder per game
                 (default %s, or $STEAM_ADOPT_GAMES)
  -h, --help     show this help and exit
  --version      print the version and exit

Run with -n first to see what would change.
`, app.DefaultPaths(home).Games)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "steam-adopt: unexpected arguments: %v\n", flags.Args())
		flags.Usage()
		return 2
	}
	if showVersion {
		fmt.Fprintln(stdout, "steam-adopt", version)
		return 0
	}
	if games == "" {
		fmt.Fprintln(stderr, "steam-adopt: --games needs a folder")
		return 2
	}
	// The shell leaves ~ alone in --games=~/x and in a quoted or env file value
	if games == "~" {
		games = home
	} else if rest, ok := strings.CutPrefix(games, "~/"); ok {
		games = filepath.Join(home, rest)
	} else if rest, ok := strings.CutPrefix(games, `~\`); ok && filepath.Separator == '\\' {
		games = filepath.Join(home, rest)
	}
	if abs, err := filepath.Abs(games); err == nil {
		games = abs
	}
	paths := app.NewPaths(home, games)

	// Single instance, cancelled cleanly on Ctrl-C or SIGTERM
	unlock, err := lock(lockPath())
	if err != nil {
		fmt.Fprintln(stderr, app.Sentence(err))
		return 1
	}
	defer unlock()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The sync itself, with Ctrl-C reported as exit code 130
	a := &app.App{
		Paths:   paths,
		Steam:   steam.Process{Flatpak: steam.IsFlatpak(paths.Steam), Dir: paths.Steam},
		Store:   store.New(),
		Out:     stdout,
		Warn:    stderr,
		DryRun:  dry,
		Confirm: confirmer(ctx, yes, os.Stdin, stdout),
	}
	if err := a.Run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "Interrupted")
			return 130
		}
		fmt.Fprintln(stderr, app.Sentence(err))
		return 1
	}
	return 0
}

// Pick how removals are approved: always with --yes, by asking in a terminal, and never without one
func confirmer(ctx context.Context, yes bool, in *os.File, out io.Writer) func([]string) bool {
	switch {
	case yes:
		return func([]string) bool { return true }
	case isTerminal(in):
		return func(names []string) bool { return confirm(ctx, in, out, names) }
	}
	return nil
}

// Check whether f is a terminal someone can answer from, not /dev/null as cron and systemd give
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(st, null)
}

// Ask before games leave Steam, since their Proton prefix and its saves can go with them, Ctrl-C answering no
func confirm(ctx context.Context, in io.Reader, out io.Writer, names []string) bool {
	fmt.Fprintln(out, "These games would be removed from Steam:")
	for _, n := range names {
		fmt.Fprintf(out, "  - %s\n", n)
	}
	fmt.Fprint(out, "Saves kept in their Proton prefix (compatdata) can be lost with them. Remove? [y/N] ")
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(in).ReadString('\n')
		answer <- line
	}()
	var line string
	select {
	case line = <-answer:
	case <-ctx.Done():
	}
	// Without a typed Enter the report would go on the question's line
	if !strings.HasSuffix(line, "\n") {
		fmt.Fprintln(out)
	}
	if ctx.Err() != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
