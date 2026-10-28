package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Test that each flag gives the right exit code and output
func TestRunFlags(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		code int
		out  string
	}{
		"version":         {[]string{"--version"}, 0, "steam-adopt dev\n"},
		"help":            {[]string{"-h"}, 0, ""},
		"unknown flag":    {[]string{"--nope"}, 2, ""},
		"extra arguments": {[]string{"extra"}, 2, ""},
		"empty games":     {[]string{"--games", ""}, 2, ""},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != tc.code || stdout.String() != tc.out {
			t.Errorf("%s: code %d, stdout %q, stderr %q", name, code, stdout.String(), stderr.String())
		}
	}
}

// Keep the lock file in the test's own folder, never the real one a running steam-adopt holds
func isolateLock(t *testing.T) {
	dir := t.TempDir()
	// The runtime folder on Unix, the temp folder on Windows
	for _, env := range []string{"XDG_RUNTIME_DIR", "TMP", "TEMP"} {
		t.Setenv(env, dir)
	}
}

// Test that a second run stops while another one holds the lock
func TestRunRefusesSecondInstance(t *testing.T) {
	isolateLock(t)
	unlock, err := lock(lockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--games", t.TempDir()}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "already running") {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
}

// Test that the lock is exclusive until released, and fails in a missing folder
func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steam-adopt.lock")
	unlock, err := lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock(path); err == nil || err.Error() != "steam-adopt is already running" {
		t.Errorf("second lock: %v", err)
	}
	unlock()
	again, err := lock(path)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	again()

	if _, err := lock(filepath.Join(t.TempDir(), "missing/steam-adopt.lock")); err == nil ||
		!strings.Contains(err.Error(), "can't create the lock file") {
		t.Errorf("lock in a missing folder: %v", err)
	}
}

// Test that only a yes answer confirms a removal, and that an answer without Enter still ends the question's line
func TestConfirm(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, " YES \n": true, "y": true, "n\n": false, "\n": false, "": false} {
		var out bytes.Buffer
		if got := confirm(context.Background(), strings.NewReader(in), &out, []string{"A", "B"}); got != want {
			t.Errorf("%q: got %v", in, got)
		}
		if !strings.Contains(out.String(), "  - A\n  - B\n") || !strings.Contains(out.String(), "[y/N]") {
			t.Errorf("question:\n%s", out.String())
		}
		typed := strings.HasSuffix(in, "\n")
		if ended := strings.HasSuffix(out.String(), "[y/N] \n"); ended == typed {
			t.Errorf("%q: question's line ended by steam-adopt %v, by the typed Enter %v", in, ended, typed)
		}
	}
}

// Test that a leading ~ is expanded, since the shell leaves it alone in --games=~/x and in env values
func TestRunExpandsTilde(t *testing.T) {
	isolateLock(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, args := range [][]string{{"--games=~/no-such-folder"}, {}} {
		t.Setenv("STEAM_ADOPT_GAMES", "~/no-such-folder")
		var stdout, stderr bytes.Buffer
		run(args, &stdout, &stderr)
		if want := filepath.Join(home, "no-such-folder"); !strings.Contains(stderr.String(), want) {
			t.Errorf("%v: stderr %q doesn't name %s", args, stderr.String(), want)
		}
	}
}

// Test that Ctrl-C at the question answers no at once instead of waiting for Enter
func TestConfirmInterrupted(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() { done <- confirm(ctx, r, io.Discard, []string{"A"}) }()
	cancel()
	select {
	case got := <-done:
		if got {
			t.Error("interrupted question answered yes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("question still waiting after Ctrl-C")
	}
}

// Test that /dev/null, cron's and systemd's input, isn't taken for a terminal, nor is a regular file
func TestIsTerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Error("/dev/null taken for a terminal")
	}
	f, _ := os.CreateTemp(t.TempDir(), "in")
	defer f.Close()
	if isTerminal(f) {
		t.Error("a file taken for a terminal")
	}
}

// Test that --yes approves every removal, and that without it removals are asked in a terminal only
func TestConfirmer(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer null.Close()
	if c := confirmer(context.Background(), true, null, io.Discard); c == nil || !c([]string{"A"}) {
		t.Error("--yes didn't approve the removal")
	}
	if c := confirmer(context.Background(), false, null, io.Discard); c != nil {
		t.Error("asked without a terminal, where nobody can answer")
	}
}
