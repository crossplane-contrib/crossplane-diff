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
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	k8sapiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	xpv1 "github.com/crossplane/crossplane-runtime/v2/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
	"github.com/crossplane/crossplane/v2/test/e2e"
	"github.com/crossplane/crossplane/v2/test/e2e/config"
	"github.com/crossplane/crossplane/v2/test/e2e/funcs"
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

// runCrossplaneDiff runs the crossplane diff command with the specified subcommand on the provided resources.
// It returns the output and any error encountered.
func runCrossplaneDiff(t *testing.T, c *envconf.Config, binPath, subcommand string, resourcePaths ...string) (string, string, error) {
	t.Helper()

	// Prepare the command to run
	args := append([]string{"--verbose", subcommand, "--timeout=2m"}, resourcePaths...)
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

	return stdout.String(), stderr.String(), err
}

// RunXRDiff runs the crossplane xr diff command on the provided resources.
// It returns the output and any error encountered.
func RunXRDiff(t *testing.T, c *envconf.Config, binPath string, resourcePaths ...string) (string, string, error) {
	t.Helper()
	return runCrossplaneDiff(t, c, binPath, "xr", resourcePaths...)
}

// RunCompDiff runs the crossplane comp diff command on the provided compositions.
// It returns the output and any error encountered.
func RunCompDiff(t *testing.T, c *envconf.Config, binPath string, compositionPaths ...string) (string, string, error) {
	t.Helper()
	return runCrossplaneDiff(t, c, binPath, "comp", compositionPaths...)
}

// TestDiffNewResourceV2Cluster tests the crossplane diff command against net-new resources in v2-cluster variant.
func TestDiffNewResourceV2Cluster(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v2-cluster")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("DiffNewResourceV2Cluster").
			WithLabel(e2e.LabelArea, LabelAreaDiff).
			WithLabel(e2e.LabelSize, e2e.LabelSizeSmall).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithLabel(LabelCrossplaneVersion, CrossplaneVersionMain).
			WithSetup("CreatePrerequisites", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, setupPath, "*.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, setupPath, "*.yaml"),
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "definition.yaml", apiextensionsv1.WatchingComposite()),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, setupPath, "provider.yaml", pkgv1.Healthy(), pkgv1.Active()),
			)).
			Assess("CanDiffNewResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "new-xr.yaml"))
				if err != nil {
					t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "new-xr.ansi"), log)

				return ctx
			}).
			WithTeardown("DeletePrerequisites", funcs.AllOf(
				funcs.DeleteResourcesWithPropagationPolicy(setupPath, "*.yaml", metav1.DeletePropagationForeground),
				funcs.ResourcesDeletedWithin(3*time.Minute, setupPath, "*.yaml"),
			)).
			Feature(),
	)
}

// TestDiffExistingResourceV2Cluster tests the crossplane diff command against existing resources in v2-cluster variant.
func TestDiffExistingResourceV2Cluster(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v2-cluster")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("DiffExistingResourceV2Cluster").
			WithLabel(e2e.LabelArea, LabelAreaDiff).
			WithLabel(e2e.LabelSize, e2e.LabelSizeSmall).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithLabel(LabelCrossplaneVersion, CrossplaneVersionMain).
			WithSetup("CreatePrerequisites", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, setupPath, "*.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, setupPath, "*.yaml"),
			)).
			WithSetup("PrerequisitesAreReady", funcs.AllOf(
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "definition.yaml", apiextensionsv1.WatchingComposite()),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, setupPath, "provider.yaml", pkgv1.Healthy(), pkgv1.Active()),
			)).
			WithSetup("CreateExistingXR", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-xr.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "existing-xr.yaml"),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-xr.yaml", xpv1.Available()),
			)).
			Assess("CanDiffExistingResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "modified-xr.yaml"))
				if err != nil {
					t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "existing-xr.ansi"), log)

				return ctx
			}).
			WithTeardown("DeleteResources", funcs.AllOf(
				funcs.DeleteResources(manifests, "existing-xr.yaml"),
				funcs.ResourcesDeletedWithin(2*time.Minute, manifests, "existing-xr.yaml"),
			)).
			WithTeardown("DeletePrerequisites", funcs.ResourcesDeletedAfterListedAreGone(3*time.Minute, setupPath, "*.yaml", clusterNopList)).
			Feature(),
	)
}

// TestDiffNewResourceV2Namespaced tests the crossplane diff command against net-new resources in v2-namespaced variant.
func TestDiffNewResourceV2Namespaced(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v2-namespaced")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("DiffNewResourceV2Namespaced").
			WithLabel(e2e.LabelArea, LabelAreaDiff).
			WithLabel(e2e.LabelSize, e2e.LabelSizeSmall).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithLabel(LabelCrossplaneVersion, CrossplaneVersionMain).
			WithSetup("CreatePrerequisites", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, setupPath, "*.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, setupPath, "*.yaml"),
			)).
			WithSetup("PrerequisitesAreReady", funcs.AllOf(
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "definition.yaml", apiextensionsv1.WatchingComposite()),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, setupPath, "provider.yaml", pkgv1.Healthy(), pkgv1.Active()),
			)).
			Assess("CanDiffNewResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "new-xr.yaml"))
				if err != nil {
					t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "new-xr.ansi"), log)

				return ctx
			}).
			WithTeardown("DeletePrerequisites", funcs.AllOf(
				funcs.DeleteResourcesWithPropagationPolicy(setupPath, "*.yaml", metav1.DeletePropagationForeground),
				funcs.ResourcesDeletedWithin(3*time.Minute, setupPath, "*.yaml"),
				funcs.ResourceDeletedWithin(3*time.Minute, &k8sapiextensionsv1.CustomResourceDefinition{
					TypeMeta:   metav1.TypeMeta{Kind: "CustomResourceDefinition", APIVersion: "apiextensions.k8s.io/v1"},
					ObjectMeta: metav1.ObjectMeta{Name: "nopresources.diff.example.org"},
				}),
			)).
			Feature(),
	)
}

// TestDiffExistingResourceV2Namespaced tests the crossplane diff command against existing resources in v2-namespaced variant.
func TestDiffExistingResourceV2Namespaced(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v2-namespaced")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("DiffExistingResourceV2Namespaced").
			WithLabel(e2e.LabelArea, LabelAreaDiff).
			WithLabel(e2e.LabelSize, e2e.LabelSizeSmall).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithLabel(LabelCrossplaneVersion, CrossplaneVersionMain).
			WithSetup("CreatePrerequisites", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, setupPath, "*.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, setupPath, "*.yaml"),
			)).
			WithSetup("PrerequisitesAreReady", funcs.AllOf(
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "definition.yaml", apiextensionsv1.WatchingComposite()),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, setupPath, "provider.yaml", pkgv1.Healthy(), pkgv1.Active()),
			)).
			WithSetup("CreateExistingXR", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-xr.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "existing-xr.yaml"),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-xr.yaml", xpv1.Available()),
			)).
			Assess("CanDiffExistingResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "modified-xr.yaml"))
				if err != nil {
					t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "existing-xr.ansi"), log)

				return ctx
			}).
			WithTeardown("DeleteResources", funcs.AllOf(
				funcs.DeleteResources(manifests, "existing-xr.yaml"),
				funcs.ResourcesDeletedWithin(2*time.Minute, manifests, "existing-xr.yaml"),
			)).
			WithTeardown("DeletePrerequisites", funcs.AllOf(
				funcs.ResourcesDeletedAfterListedAreGone(3*time.Minute, setupPath, "*.yaml", nsNopList),
				funcs.ResourceDeletedWithin(3*time.Minute, &k8sapiextensionsv1.CustomResourceDefinition{
					TypeMeta:   metav1.TypeMeta{Kind: "CustomResourceDefinition", APIVersion: "apiextensions.k8s.io/v1"},
					ObjectMeta: metav1.ObjectMeta{Name: "nopresources.diff.example.org"},
				}),
			)).
			Feature(),
	)
}

// TestDiffNewResourceV1 tests the crossplane diff command against net-new resources in v1 variant.
func TestDiffNewResourceV1(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v1")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("DiffNewResourceV1").
			WithLabel(e2e.LabelArea, LabelAreaDiff).
			WithLabel(e2e.LabelSize, e2e.LabelSizeSmall).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithLabel(LabelCrossplaneVersion, CrossplaneVersionRelease120).
			WithLabel(LabelCrossplaneVersion, CrossplaneVersionMain).
			WithSetup("CreatePrerequisites", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, setupPath, "*.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, setupPath, "*.yaml"),
			)).
			WithSetup("PrerequisitesAreReady", funcs.AllOf(
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "definition.yaml", apiextensionsv1.WatchingComposite()),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, setupPath, "provider.yaml", pkgv1.Healthy(), pkgv1.Active()),
			)).
			Assess("CanDiffNewResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "new-xr.yaml"))
				if err != nil {
					t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "new-xr.ansi"), log)

				return ctx
			}).
			WithTeardown("DeletePrerequisites", funcs.AllOf(
				funcs.DeleteResourcesWithPropagationPolicy(setupPath, "*.yaml", metav1.DeletePropagationForeground),
				funcs.ResourcesDeletedWithin(3*time.Minute, setupPath, "*.yaml"),
				funcs.ResourceDeletedWithin(3*time.Minute, &k8sapiextensionsv1.CustomResourceDefinition{
					TypeMeta:   metav1.TypeMeta{Kind: "CustomResourceDefinition", APIVersion: "apiextensions.k8s.io/v1"},
					ObjectMeta: metav1.ObjectMeta{Name: "nopresources.diff.example.org"},
				}),
			)).
			Feature(),
	)
}

// TestDiffExistingResourceV1 tests the crossplane diff command against existing resources in v1 variant.
func TestDiffExistingResourceV1(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v1")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("DiffExistingResourceV1").
			WithLabel(e2e.LabelArea, LabelAreaDiff).
			WithLabel(e2e.LabelSize, e2e.LabelSizeSmall).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithLabel(LabelCrossplaneVersion, CrossplaneVersionRelease120).
			WithLabel(LabelCrossplaneVersion, CrossplaneVersionMain).
			WithSetup("CreatePrerequisites", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, setupPath, "*.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, setupPath, "*.yaml"),
			)).
			WithSetup("PrerequisitesAreReady", funcs.AllOf(
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "definition.yaml", apiextensionsv1.WatchingComposite()),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, setupPath, "provider.yaml", pkgv1.Healthy(), pkgv1.Active()),
			)).
			WithSetup("CreateXR", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-xr.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "existing-xr.yaml"),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-xr.yaml", xpv1.Available()),
			)).
			Assess("CanDiffExistingResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "modified-xr.yaml"))
				if err != nil {
					t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "existing-xr.ansi"), log)

				return ctx
			}).
			WithTeardown("DeleteResources", funcs.AllOf(
				funcs.DeleteResources(manifests, "existing-xr.yaml"),
				funcs.ResourcesDeletedWithin(2*time.Minute, manifests, "existing-xr.yaml"),
			)).
			WithTeardown("DeletePrerequisites", funcs.AllOf(
				func(ctx context.Context, t *testing.T, e *envconf.Config) context.Context {
					t.Helper()
					// default to `main` variant
					nopList := clusterNopList

					// we should only ever be running with one version label
					if slices.Contains(e.Labels()[LabelCrossplaneVersion], CrossplaneVersionRelease120) {
						nopList = v1NopList
					}

					funcs.ResourcesDeletedAfterListedAreGone(3*time.Minute, setupPath, "*.yaml", nopList)(ctx, t, e)

					return ctx
				},
				funcs.ResourceDeletedWithin(3*time.Minute, &k8sapiextensionsv1.CustomResourceDefinition{
					TypeMeta:   metav1.TypeMeta{Kind: "CustomResourceDefinition", APIVersion: "apiextensions.k8s.io/v1"},
					ObjectMeta: metav1.ObjectMeta{Name: "nopresources.diff.example.org"},
				}),
			)).
			Feature(),
	)
}

// I don't like this merged list here, but we have a weird condition where v1 tests run against v2 providers in the
// `main` branch case.  so namespaced NopResources don't exist and we have to create cluster ones.  this is fair because
// v1 XRDs should still work with v2 providers but i don't want to write tests that differ only in the nopList.

// doesn't yet pass -- theory:  we need to figure out how to pass a list of only valid elements instead of a
// catch-all list?  figure out which variant to run based on... label?
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
	resourceNameRegex        = regexp.MustCompile(`(existing-resource)-[a-z0-9]{5,}(?:-nop-resource)?`)
	compositionRevisionRegex = regexp.MustCompile(`(xnopresources\.(cluster\.|legacy\.)?diff\.example\.org)-[a-z0-9]{7,}`)
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

// TestDiffExistingComposition tests the crossplane comp diff command against existing XRs in the cluster.
func TestDiffExistingComposition(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "comp")
	setupPath := filepath.Join(manifests, "setup")

	environment.Test(t,
		features.New("DiffExistingComposition").
			WithLabel(e2e.LabelArea, LabelAreaDiff).
			WithLabel(e2e.LabelSize, e2e.LabelSizeSmall).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithLabel(LabelCrossplaneVersion, CrossplaneVersionMain).
			WithSetup("CreatePrerequisites", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, setupPath, "*.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, setupPath, "*.yaml"),
			)).
			WithSetup("PrerequisitesAreReady", funcs.AllOf(
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "definition.yaml", apiextensionsv1.WatchingComposite()),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, setupPath, "provider.yaml", pkgv1.Healthy(), pkgv1.Active()),
			)).
			WithSetup("CreateExistingXR", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-xr.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "existing-xr.yaml"),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-xr.yaml", xpv1.Available()),
			)).
			Assess("CanDiffComposition", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunCompDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "updated-composition.yaml"))
				if err != nil {
					t.Fatalf("Error running comp diff command: %v\nLog output:\n%s", err, log)
				}

				// Basic validation - ensure we get meaningful output
				if output == "" {
					t.Fatalf("Expected non-empty output from comp diff command")
				}

				// Check that output contains references to our test resource
				if !strings.Contains(output, "test-comp-resource") {
					t.Errorf("Expected output to contain reference to test-comp-resource, got: %s", output)
				}

				// Check that output shows the expected changes
				if !strings.Contains(output, "resource-tier") || !strings.Contains(output, "config-data") {
					t.Logf("Output: %s", output)
					t.Errorf("Expected output to show changes to resource-tier and config-data annotations")
				}

				t.Logf("Comp diff output:\n%s", output)

				return ctx
			}).
			WithTeardown("DeleteExistingXR", funcs.AllOf(
				funcs.DeleteResources(manifests, "existing-xr.yaml"),
				funcs.ResourcesDeletedWithin(2*time.Minute, manifests, "existing-xr.yaml"),
			)).
			WithTeardown("DeletePrerequisites", funcs.ResourcesDeletedAfterListedAreGone(3*time.Minute, setupPath, "*.yaml", clusterNopList)).
			Feature(),
	)
}
