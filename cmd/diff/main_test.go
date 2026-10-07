package main

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain runs before all tests and cleans up after all tests complete.
func TestMain(m *testing.M) {
	// Parse now rather than in m.Run, so the envtest guards can read -test.timeout.
	flag.Parse()

	stopEnvtestGuards := startEnvtestGuards(testTimeout())

	// Run all tests
	exitCode := m.Run()

	stopEnvtestGuards()

	// Clean up orphaned function containers after tests complete
	cleanupFunctionContainers()

	// Exit with the test suite's exit code
	os.Exit(exitCode)
}

// testTimeout returns the value of go test's -timeout flag (-test.timeout), or 0 if it has none.
func testTimeout() time.Duration {
	f := flag.Lookup("test.timeout")
	if f == nil {
		return 0
	}

	getter, ok := f.Value.(flag.Getter)
	if !ok {
		return 0
	}

	timeout, _ := getter.Get().(time.Duration)

	return timeout
}

// cleanupFunctionContainers removes the named function containers used by integration tests.
func cleanupFunctionContainers() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Use exec.CommandContext to run docker command
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "-q", "--filter", "name=-it$")

	output, err := cmd.Output()
	if err != nil {
		// Docker might not be available or no containers found - that's okay
		return
	}

	containerIDs := strings.Fields(string(output))
	if len(containerIDs) == 0 {
		return
	}

	// Remove the containers
	args := append([]string{"rm", "-f"}, containerIDs...)
	cleanupCmd := exec.CommandContext(ctx, "docker", args...)
	_ = cleanupCmd.Run() // Ignore errors during cleanup
}
