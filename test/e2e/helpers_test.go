/*
Copyright 2025 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
)

// LabelAreaDiff is applied to all features pertaining to the diff command.
const LabelAreaDiff = "diff"

// LabelCrossplaneVersion represents the crossplane version compatibility of a test.
const LabelCrossplaneVersion = "crossplane-version"

// Crossplane version values.
const (
	CrossplaneVersionMain       = "main"
	CrossplaneVersionRelease120 = "release-1.20"
)

// exitCodeDiffDetected is the exit code when diffs are detected.
// Defined locally to avoid importing cmd/diff/diffprocessor into E2E tests.
const exitCodeDiffDetected = 3

var v1NopList = composed.NewList(
	composed.FromReferenceToList(corev1.ObjectReference{
		APIVersion: "nop.crossplane.io/v1alpha1",
		Kind:       "NopResource",
	}),
)

var nsNopList = composed.NewList(
	composed.FromReferenceToList(corev1.ObjectReference{
		APIVersion: "nop.crossplane.io/v1alpha1",
		Kind:       "NopResource",
		Namespace:  "default",
	}))

var clusterNopList = composed.NewList(composed.FromReferenceToList(corev1.ObjectReference{
	APIVersion: "nop.crossplane.io/v1alpha1",
	Kind:       "ClusterNopResource",
}))

// Regular expressions to match the dynamic parts.
var (
	resourceNameRegex                 = regexp.MustCompile(`(existing-resource)-[a-z0-9]{5,}(?:-nop-resource)?`)
	compResourceNameRegex             = regexp.MustCompile(`(test-comp-resource)-[a-z0-9]{5,}`)
	getComposedResourceNameRegex      = regexp.MustCompile(`(test-getcomposed-resource)-[a-z0-9]{5,}`)
	fanoutResourceNameRegex           = regexp.MustCompile(`(test-fanout-resource-\d{2})-[a-z0-9]{5,}`)
	claimNameRegex                    = regexp.MustCompile(`(test-claim)-[a-z0-9]{5,}(?:-[a-z0-9]{5,})?`)
	compClaimNameRegex                = regexp.MustCompile(`(test-comp-claim)-[a-z0-9]{5,}`)
	nestedGenerateNameRegex           = regexp.MustCompile(`(test-parent-generatename-child)-[a-z0-9]{12,16}`)
	nestedClaimGenerateNameRegex      = regexp.MustCompile(`(existing-parent-claim)-[a-z0-9]{5,}(?:-[a-z0-9]{12,16})?`)
	claimCompositionRevisionRegex     = regexp.MustCompile(`(xnopclaims\.claim\.diff\.example\.org)-[a-z0-9]{7,}`)
	compositionRevisionRegex          = regexp.MustCompile(`(xnopresources\.(cluster\.|legacy\.|v2withv1paths\.)?diff\.example\.org)-[a-z0-9]{7,}`)
	nestedCompositionRevisionRegex    = regexp.MustCompile(`(child-nop-composition|parent-nop-composition)-[a-z0-9]{7,}`)
	compClaimCompositionRevisionRegex = regexp.MustCompile(`(xnopclaimdiffresources\.claimdiff\.example\.org)-[a-z0-9]{7,}`)
	ansiEscapeRegex                   = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

// runCrossplaneDiff runs the crossplane diff command with the specified subcommand on the provided resources.
// It returns the stdout, stderr, exit code, and any error that is not an ExitError.
func runCrossplaneDiff(t *testing.T, c *envconf.Config, binPath, subcommand string, extraArgs []string, resourcePaths ...string) (string, string, int, error) {
	t.Helper()

	// Prepare the command to run
	args := make([]string, 0, 3+len(extraArgs)+len(resourcePaths))
	args = append(args, "--verbose", subcommand, "--timeout=2m")
	args = append(args, extraArgs...)
	args = append(args, resourcePaths...)
	t.Logf("Running command: %s %s", binPath, strings.Join(args, " "))
	cmd := exec.Command(binPath, args...)

	cmd.Env = append(os.Environ(), "KUBECONFIG="+c.KubeconfigFile())
	t.Logf("ENV: %s KUBECONFIG=%s", binPath, c.KubeconfigFile())

	// Capture standard output and error
	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run the command
	err := cmd.Run()

	// Extract exit code from error
	exitCode := 0

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			err = nil // Not a real error, just a non-zero exit code
		}
	}

	return stdout.String(), stderr.String(), exitCode, err
}

// RunXRDiff runs the crossplane xr diff command on the provided resources.
// It returns the output, log, and any error encountered.
// expectedExitCode specifies which exit code is expected.
func RunXRDiff(t *testing.T, c *envconf.Config, binPath string, expectedExitCode int, resourcePaths ...string) (string, string, error) {
	t.Helper()
	return RunXRDiffWithArgs(t, c, binPath, expectedExitCode, nil, resourcePaths...)
}

// RunXRDiffWithArgs runs the crossplane xr diff command with extra CLI arguments.
// Extra args are inserted after the subcommand but before resource paths.
// expectedExitCode specifies which exit code is expected.
func RunXRDiffWithArgs(t *testing.T, c *envconf.Config, binPath string, expectedExitCode int, extraArgs []string, resourcePaths ...string) (string, string, error) {
	t.Helper()

	stdout, stderr, exitCode, err := runCrossplaneDiff(t, c, binPath, "xr", extraArgs, resourcePaths...)
	if err != nil {
		return stdout, stderr, err
	}

	if exitCode != expectedExitCode {
		return stdout, stderr, fmt.Errorf("unexpected exit code %d, expected %d", exitCode, expectedExitCode)
	}

	return stdout, stderr, nil
}

// RunCompDiff runs the crossplane comp diff command on the provided compositions.
// It returns the output, log, and any error encountered.
// expectedExitCode specifies which exit code is expected.
func RunCompDiff(t *testing.T, c *envconf.Config, binPath string, expectedExitCode int, compositionPaths ...string) (string, string, error) {
	t.Helper()

	stdout, stderr, exitCode, err := runCrossplaneDiff(t, c, binPath, "comp", nil, compositionPaths...)
	if err != nil {
		return stdout, stderr, err
	}

	if exitCode != expectedExitCode {
		return stdout, stderr, fmt.Errorf("unexpected exit code %d, expected %d", exitCode, expectedExitCode)
	}

	return stdout, stderr, nil
}

// NormalizeLine replaces dynamic parts with fixed placeholders.
func normalizeLine(line string) string {
	// Remove ANSI escape sequences
	line = ansiEscapeRegex.ReplaceAllString(line, "")

	// Replace resource names with random suffixes
	line = resourceNameRegex.ReplaceAllString(line, "${1}-XXXXX")
	line = compResourceNameRegex.ReplaceAllString(line, "${1}-XXXXX")
	line = getComposedResourceNameRegex.ReplaceAllString(line, "${1}-XXXXX")
	line = fanoutResourceNameRegex.ReplaceAllString(line, "${1}-XXXXX")
	line = claimNameRegex.ReplaceAllString(line, "${1}-XXXXX")
	line = compClaimNameRegex.ReplaceAllString(line, "${1}-XXXXX")
	line = nestedGenerateNameRegex.ReplaceAllString(line, "${1}-XXXXX")
	line = nestedClaimGenerateNameRegex.ReplaceAllString(line, "${1}-XXXXX")

	// Replace composition revision refs with random hash
	line = compositionRevisionRegex.ReplaceAllString(line, "${1}-XXXXXXX")
	line = claimCompositionRevisionRegex.ReplaceAllString(line, "${1}-XXXXXXX")
	line = nestedCompositionRevisionRegex.ReplaceAllString(line, "${1}-XXXXXXX")
	line = compClaimCompositionRevisionRegex.ReplaceAllString(line, "${1}-XXXXXXX")

	// Trim trailing whitespace
	line = strings.TrimRight(line, " ")

	return line
}

// parseStringContent converts a string to raw and normalized lines.
func parseStringContent(content string) ([]string, []string) {
	var (
		rawLines        []string
		normalizedLines []string
	)

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		rawLine := scanner.Text()
		rawLines = append(rawLines, rawLine)
		normalizedLines = append(normalizedLines, normalizeLine(rawLine))
	}

	return rawLines, normalizedLines
}

// AssertDiffMatchesFile compares a diff output with an expected file, ignoring dynamic parts.
func assertDiffMatchesFile(t *testing.T, actual, expectedSource, log string) {
	t.Helper()

	// If E2E_DUMP_EXPECTED is set, write the actual output to the expected file
	if os.Getenv("E2E_DUMP_EXPECTED") != "" {
		// Ensure the directory exists
		if err := os.MkdirAll(filepath.Dir(expectedSource), 0o755); err != nil {
			t.Fatalf("Failed to create directory for expected file: %v", err)
		}

		// Normalize the output before writing to reduce churn from random generated names
		_, normalizedLines := parseStringContent(actual)

		normalizedOutput := strings.Join(normalizedLines, "\n")
		if strings.HasSuffix(actual, "\n") {
			// Add trailing newline if original had one
			normalizedOutput += "\n"
		}

		// Write the normalized output to the expected file
		if err := os.WriteFile(expectedSource, []byte(normalizedOutput), 0o644); err != nil {
			t.Fatalf("Failed to write expected file: %v", err)
		}

		t.Logf("Wrote normalized expected output to %s", expectedSource)

		return
	}

	expected, err := os.ReadFile(expectedSource)
	if err != nil {
		t.Fatalf("Failed to read expected file: %v", err)
	}

	expectedRaw, expectedNormalized := parseStringContent(string(expected))
	actualRaw, actualNormalized := parseStringContent(actual)

	if len(expectedNormalized) != len(actualNormalized) {
		t.Errorf("Line count mismatch: expected %d lines in %s, got %d lines in output",
			len(expectedNormalized), expectedSource,
			len(actualNormalized))
	}

	maxLines := max(len(actualNormalized), len(expectedNormalized))

	failed := false

	for i := range maxLines {
		if i >= len(expectedNormalized) {
			t.Errorf("Line %d: Extra line in output: %s",
				i+1, makeStringReadable(actualRaw[i]))

			failed = true

			continue
		}

		if i >= len(actualNormalized) {
			t.Errorf("Line %d: Missing line in output: %s",
				i+1, makeStringReadable(expectedRaw[i]))

			failed = true

			continue
		}

		if expectedNormalized[i] != actualNormalized[i] {
			// ignore white space at end of lines
			// if strings.TrimRight(expectedNormalized[i], " ") == strings.TrimRight(actualNormalized[i], " ") {
			//	continue
			//}
			rawExpected := ""
			if i < len(expectedRaw) {
				rawExpected = expectedRaw[i]
			}

			rawActual := ""
			if i < len(actualRaw) {
				rawActual = actualRaw[i]
			}

			t.Errorf("Line %d mismatch:\n  Expected: %s\n  Actual:   %s\n\n"+
				"Raw Values (with escape chars visible):\n"+
				"  Expected Raw: %s\n"+
				"  Actual Raw:   %s",
				i+1,
				expectedNormalized[i],
				actualNormalized[i],
				makeStringReadable(rawExpected),
				makeStringReadable(rawActual))

			failed = true
		}
	}

	if failed {
		t.Errorf("###### Manifest (actual): \n%s\n", actual)
		t.Errorf("------------------------------------------------------------------")
		t.Errorf("###### Manifest (expected): \n%s\n", string(expected))

		t.Fatalf("Log output:\n%s", log)
	}
}

// makeStringReadable converts a string to a form where all non-printable characters
// (including ANSI escape codes) are converted to visible escape sequences.
func makeStringReadable(s string) string {
	var result strings.Builder

	for _, r := range s {
		switch {
		case r == '\x1b':
			result.WriteString("\\x1b")
		case r == '\n':
			result.WriteString("\\n")
		case r == '\r':
			result.WriteString("\\r")
		case r == '\t':
			result.WriteString("\\t")
		case r == ' ':
			result.WriteString("\\space")
		case !unicode.IsPrint(r):
			fmt.Fprintf(&result, "\\x%02x", r)
		default:
			result.WriteRune(r)
		}
	}

	return result.String()
}

// RunXRDiffJSON runs the crossplane xr diff command with JSON output format.
// It returns the parsed structured output, raw JSON string, log output, and any error.
// expectedExitCode specifies which exit code is expected.
func RunXRDiffJSON(t *testing.T, c *envconf.Config, binPath string, expectedExitCode int, resourcePaths ...string) (*tu.StructuredDiffOutput, string, string, error) {
	t.Helper()

	stdout, stderr, err := RunXRDiffWithArgs(t, c, binPath, expectedExitCode, []string{"--output=json"}, resourcePaths...)
	if err != nil {
		return nil, stdout, stderr, err
	}

	var output tu.StructuredDiffOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		return nil, stdout, stderr, fmt.Errorf("failed to parse JSON output: %w\nRaw output:\n%s", err, stdout)
	}

	return &output, stdout, stderr, nil
}

// RunCompDiffJSON runs the crossplane comp diff command with JSON output format.
// It returns the parsed structured output, raw JSON string, log output, and any error.
// expectedExitCode specifies which exit code is expected.
func RunCompDiffJSON(t *testing.T, c *envconf.Config, binPath string, expectedExitCode int, compositionPaths ...string) (*tu.StructuredDiffOutput, string, string, error) {
	t.Helper()

	stdout, stderr, exitCode, err := runCrossplaneDiff(t, c, binPath, "comp", []string{"--output=json"}, compositionPaths...)
	if err != nil {
		return nil, stdout, stderr, err
	}

	if exitCode != expectedExitCode {
		return nil, stdout, stderr, fmt.Errorf("unexpected exit code %d, expected %d", exitCode, expectedExitCode)
	}

	var output tu.StructuredDiffOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		return nil, stdout, stderr, fmt.Errorf("failed to parse JSON output: %w\nRaw output:\n%s", err, stdout)
	}

	return &output, stdout, stderr, nil
}

// AssertStructuredDiff validates JSON diff output against an ExpectedDiff specification.
// This is a wrapper around tu.AssertStructuredDiff for E2E test convenience.
func AssertStructuredDiff(t *testing.T, jsonOutput string, expected *tu.ExpectedDiff) {
	t.Helper()
	tu.AssertStructuredDiff(t, jsonOutput, expected)
}

// assertStructuredDiffMatchesFile compares JSON diff output against an expected JSON file.
// If E2E_DUMP_EXPECTED is set, writes the actual output to the expected file for regeneration.
func assertStructuredDiffMatchesFile(t *testing.T, actual string, expectedPath string, log string) {
	t.Helper()

	// If E2E_DUMP_EXPECTED is set, write the actual output to the expected file
	if os.Getenv("E2E_DUMP_EXPECTED") != "" {
		// Ensure the directory exists
		if err := os.MkdirAll(filepath.Dir(expectedPath), 0o755); err != nil {
			t.Fatalf("Failed to create directory for expected file: %v", err)
		}

		// Parse and re-marshal to get consistent formatting
		var parsed tu.StructuredDiffOutput
		if err := json.Unmarshal([]byte(actual), &parsed); err != nil {
			t.Fatalf("Failed to parse actual JSON output: %v", err)
		}

		// Normalize dynamic values before writing
		normalizeStructuredOutput(&parsed)

		formatted, err := json.MarshalIndent(parsed, "", "  ")
		if err != nil {
			t.Fatalf("Failed to format JSON output: %v", err)
		}

		// Add trailing newline for clean file endings
		formatted = append(formatted, '\n')

		if err := os.WriteFile(expectedPath, formatted, 0o644); err != nil {
			t.Fatalf("Failed to write expected file: %v", err)
		}

		t.Logf("Wrote normalized expected JSON to %s", expectedPath)

		return
	}

	// Read expected file
	expected, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("Failed to read expected file %s: %v", expectedPath, err)
	}

	// Parse both actual and expected
	var actualParsed, expectedParsed tu.StructuredDiffOutput
	if err := json.Unmarshal([]byte(actual), &actualParsed); err != nil {
		t.Fatalf("Failed to parse actual JSON output: %v\nRaw output:\n%s", err, actual)
	}

	if err := json.Unmarshal(expected, &expectedParsed); err != nil {
		t.Fatalf("Failed to parse expected JSON file %s: %v", expectedPath, err)
	}

	// Normalize dynamic values for comparison
	normalizeStructuredOutput(&actualParsed)

	// Compare summaries
	if actualParsed.Summary.Added != expectedParsed.Summary.Added {
		t.Errorf("Summary.Added: expected %d, got %d", expectedParsed.Summary.Added, actualParsed.Summary.Added)
	}

	if actualParsed.Summary.Modified != expectedParsed.Summary.Modified {
		t.Errorf("Summary.Modified: expected %d, got %d", expectedParsed.Summary.Modified, actualParsed.Summary.Modified)
	}

	if actualParsed.Summary.Removed != expectedParsed.Summary.Removed {
		t.Errorf("Summary.Removed: expected %d, got %d", expectedParsed.Summary.Removed, actualParsed.Summary.Removed)
	}

	// Compare change counts
	if len(actualParsed.Changes) != len(expectedParsed.Changes) {
		t.Errorf("Change count mismatch: expected %d, got %d", len(expectedParsed.Changes), len(actualParsed.Changes))
		t.Logf("Expected changes: %v", summarizeChanges(expectedParsed.Changes))
		t.Logf("Actual changes: %v", summarizeChanges(actualParsed.Changes))
	}

	// Compare individual changes
	for i, expectedChange := range expectedParsed.Changes {
		if i >= len(actualParsed.Changes) {
			t.Errorf("Missing change at index %d: expected %s %s/%s", i, expectedChange.Type, expectedChange.Kind, expectedChange.Name)

			continue
		}

		actualChange := actualParsed.Changes[i]
		if actualChange.Type != expectedChange.Type {
			t.Errorf("Change %d: type mismatch - expected %s, got %s", i, expectedChange.Type, actualChange.Type)
		}

		if actualChange.Kind != expectedChange.Kind {
			t.Errorf("Change %d: kind mismatch - expected %s, got %s", i, expectedChange.Kind, actualChange.Kind)
		}

		// Normalize names for comparison
		normalizedActualName := normalizeLine(actualChange.Name)
		normalizedExpectedName := normalizeLine(expectedChange.Name)
		if normalizedActualName != normalizedExpectedName {
			t.Errorf("Change %d: name mismatch - expected %s, got %s (normalized: %s vs %s)",
				i, expectedChange.Name, actualChange.Name, normalizedExpectedName, normalizedActualName)
		}
	}

	if t.Failed() {
		t.Logf("Log output:\n%s", log)
	}
}

// normalizeStructuredOutput normalizes dynamic values in a StructuredDiffOutput for comparison.
func normalizeStructuredOutput(output *tu.StructuredDiffOutput) {
	for i := range output.Changes {
		output.Changes[i].Name = normalizeLine(output.Changes[i].Name)
		// Normalize namespace if present
		if output.Changes[i].Namespace != "" {
			output.Changes[i].Namespace = normalizeLine(output.Changes[i].Namespace)
		}
	}
}

// summarizeChanges returns a string summarizing the changes for debug output.
func summarizeChanges(changes []tu.ChangeDetail) string {
	var parts []string
	for _, c := range changes {
		parts = append(parts, fmt.Sprintf("%s %s/%s", c.Type, c.Kind, c.Name))
	}

	return strings.Join(parts, ", ")
}
