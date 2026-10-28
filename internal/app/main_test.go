package app

import (
	"os"
	"testing"
)

// Sync tests follow Steam for Linux, the full feature set, onSystem switches one to another system
func TestMain(m *testing.M) {
	goos = "linux"
	os.Exit(m.Run())
}
