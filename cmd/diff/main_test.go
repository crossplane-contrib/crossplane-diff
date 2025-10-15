package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain runs before all tests and cleans up after all tests complete.
func TestMain(m *testing.M) {
	// Run all tests
	exitCode := m.Run()

	// Clean up orphaned function containers after tests complete
	cleanupFunctionContainers()

	// Exit with the test suite's exit code
	os.Exit(exitCode)
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
