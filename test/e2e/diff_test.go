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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	xpv1 "github.com/crossplane/crossplane-runtime/v2/apis/common/v1"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
	"github.com/crossplane/crossplane/v2/test/e2e"
	"github.com/crossplane/crossplane/v2/test/e2e/config"
	"github.com/crossplane/crossplane/v2/test/e2e/funcs"
)

// LabelAreaDiff is applied to all features pertaining to the diff command.
const LabelAreaDiff = "diff"

// RunDiff runs the crossplane diff command on the provided resources.
// It returns the output and any error encountered.
func RunDiff(t *testing.T, c *envconf.Config, binPath string, resourcePaths ...string) (string, string, error) {
	t.Helper()

	var err error

	// Prepare the command to run
	args := append([]string{"--verbose", "diff", "--timeout=2m", "-n", namespace}, resourcePaths...)
	t.Logf("Running command: %s %s", binPath, strings.Join(args, " "))
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+c.KubeconfigFile())
	t.Logf("ENV: %s KUBECONFIG=%s", binPath, c.KubeconfigFile())

	// Capture standard output and error
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run the command
	err = cmd.Run()

	return stdout.String(), stderr.String(), err
}

// TestCrossplaneDiffCommand tests the functionality of the crossplane diff command.
func TestCrossplaneDiffCommand(t *testing.T) {
	binPath := "./crossplane-diff"
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	root := filepath.Join("test/e2e/manifests/beta/diff", imageTag)
	versions, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("Failed to read manifests directory: %v", err)
	}

	setup := func(setupPath string) features.Func {
		return funcs.AllOf(
			funcs.ApplyResources(e2e.FieldManager, setupPath, "*.yaml"),
			funcs.ResourcesCreatedWithin(30*time.Second, setupPath, "*.yaml"),
			funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "definition.yaml", apiextensionsv1.WatchingComposite()),
			funcs.ResourcesHaveConditionWithin(2*time.Minute, setupPath, "provider.yaml", pkgv1.Healthy(), pkgv1.Active()),
		)
	}

	// Follow Crossplane's established pattern: explicit ordered teardowns
	deleteXRs := func(manifests string) features.Func {
		return funcs.AllOf(
			funcs.DeleteResourcesWithPropagationPolicy(manifests, "existing-xr.yaml", metav1.DeletePropagationForeground),
			funcs.ResourcesDeletedWithin(1*time.Minute, manifests, "existing-xr.yaml"),
		)
	}

	deletePrerequisites := func(setupPath string) features.Func {
		return funcs.AllOf(
			funcs.DeleteResourcesWithPropagationPolicy(setupPath, "*.yaml", metav1.DeletePropagationForeground),
			funcs.ResourcesDeletedWithin(3*time.Minute, setupPath, "*.yaml"),
		)
	}

	cases := features.Table{}
	for _, v := range versions {
		if !v.IsDir() {
			continue // Skip non-directory entries
		}

		// TODO remove.  skip v1 to test v2 for setup/teardown issues at test boundaries
		if v.Name() == "v1" {
			continue
		}

		manifests := filepath.Join(root, v.Name())
		setupPath := filepath.Join(manifests, "setup")
		expectPath := filepath.Join(manifests, "expect")

		cases = append(cases, features.Table{
			{
				Name:        fmt.Sprintf("WithNewResource (%s)", v.Name()),
				Description: "Test that we can diff against a net-new resource with `crossplane diff`",
				Assessment: func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
					t.Helper()
					ctx = setup(setupPath)(ctx, t, c)

					// Run the diff command on a new resource that doesn't exist yet
					output, log, err := RunDiff(t, c, binPath, filepath.Join(manifests, "new-xr.yaml"))
					if err != nil {
						t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
					}

					assertDiffMatchesFile(t, output, filepath.Join(expectPath, "new-xr.ansi"), log)

					// No XRs to delete for new-xr test, just delete prerequisites
					ctx = deletePrerequisites(setupPath)(ctx, t, c)

					// Add delay to prevent API rate limiting between test iterations
					ctx = funcs.SleepFor(5*time.Second)(ctx, t, c)

					return ctx
				},
			},
			{
				Name:        fmt.Sprintf("WithExistingResource (%s)", v.Name()),
				Description: "Test that we can diff against an existing resource with `crossplane diff`",
				Assessment: func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
					t.Helper()

					ctx = setup(setupPath)(ctx, t, c)

					// Create the initial resource first
					setupFuncs := funcs.AllOf(
						funcs.ApplyResources(e2e.FieldManager, manifests, "existing-xr.yaml"),
						funcs.ResourcesCreatedWithin(30*time.Second, manifests, "existing-xr.yaml"),
						funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-xr.yaml", xpv1.Available()),
					)

					ctx = setupFuncs(ctx, t, c)

					// Run the diff command on a modified existing resource
					output, log, err := RunDiff(t, c, binPath, filepath.Join(manifests, "modified-xr.yaml"))
					if err != nil {
						t.Fatalf("Error running diff command: %v\n Log output:\n%s", err, log)
					}

					assertDiffMatchesFile(t, output, filepath.Join(expectPath, "existing-xr.ansi"), log)

					// Clean up the resource we created (following Crossplane pattern)
					ctx = deleteXRs(manifests)(ctx, t, c)
					ctx = deletePrerequisites(setupPath)(ctx, t, c)

					// Add delay to prevent API rate limiting between test iterations
					ctx = funcs.SleepFor(5*time.Second)(ctx, t, c)

					return ctx
				},
			},
		}...)
	}

	environment.Test(t,
		cases.Build(t.Name()).
			WithLabel(e2e.LabelArea, LabelAreaDiff).
			WithLabel(e2e.LabelSize, e2e.LabelSizeSmall).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			Feature(),
	)
}

// Regular expressions to match the dynamic parts.
var (
	resourceNameRegex        = regexp.MustCompile(`(existing-resource)-[a-z0-9]{5,}(?:-nop-resource)?`)
	compositionRevisionRegex = regexp.MustCompile(`(xnopresources\.diff\.example\.org)-[a-z0-9]{7,}`)
	ansiEscapeRegex          = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

// NormalizeLine replaces dynamic parts with fixed placeholders.
func normalizeLine(line string) string {
	// Remove ANSI escape sequences
	line = ansiEscapeRegex.ReplaceAllString(line, "")

	// Replace resource names with random suffixes
	line = resourceNameRegex.ReplaceAllString(line, "${1}-XXXXX")

	// Replace composition revision refs with random hash
	line = compositionRevisionRegex.ReplaceAllString(line, "${1}-XXXXXXX")

	// Trim trailing whitespace
	line = strings.TrimRight(line, " ")

	return line
}

// parseStringContent converts a string to raw and normalized lines.
func parseStringContent(content string) ([]string, []string) {
	var rawLines []string
	var normalizedLines []string

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

	maxLines := len(expectedNormalized)
	if len(actualNormalized) > maxLines {
		maxLines = len(actualNormalized)
	}

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
			result.WriteString(fmt.Sprintf("\\x%02x", r))
		default:
			result.WriteRune(r)
		}
	}
	return result.String()
}
