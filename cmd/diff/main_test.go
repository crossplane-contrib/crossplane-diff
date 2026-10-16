package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils/envtestreaper"
)

// TestMain runs before all tests.
func TestMain(m *testing.M) {
	// Kill envtest servers that an earlier run left behind when it died abnormally.
	envtestreaper.Reap(func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) })

	// Run all tests
	exitCode := m.Run()

	// Exit with the test suite's exit code
	os.Exit(exitCode)
}
