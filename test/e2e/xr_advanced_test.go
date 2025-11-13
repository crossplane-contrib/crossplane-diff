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
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	xpv1 "github.com/crossplane/crossplane-runtime/v2/apis/common/v1"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/v2/test/e2e"
	"github.com/crossplane/crossplane/v2/test/e2e/config"
	"github.com/crossplane/crossplane/v2/test/e2e/funcs"
)

// TestDiffConcurrentDirectory tests issue #59 - concurrent function startup failures
// when processing multiple XR files from a directory with a composition using multiple functions.
func TestDiffConcurrentDirectory(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v2-concurrent-dir")
	setupPath := filepath.Join(manifests, "setup")
	xrsPath := filepath.Join(manifests, "xrs")

	environment.Test(t,
		features.New("TestDiffConcurrentDirectory").
			WithLabel(e2e.LabelArea, LabelAreaDiff).
			WithLabel(e2e.LabelSize, e2e.LabelSizeLarge).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithLabel(LabelCrossplaneVersion, CrossplaneVersionMain).
			WithSetup("CreatePrerequisites", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, setupPath, "*.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, setupPath, "*.yaml"),
			)).
			WithSetup("PrerequisitesAreReady", funcs.AllOf(
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "definition.yaml", apiextensionsv1.WatchingComposite()),
			)).
			Assess("CanProcessDirectoryWithMultipleXRs", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				// Get all XR files from the directory
				xrFiles, err := filepath.Glob(filepath.Join(xrsPath, "*.yaml"))
				if err != nil {
					t.Fatalf("Failed to find XR files: %v", err)
				}

				if len(xrFiles) != 21 {
					t.Fatalf("Expected 21 XR files, found %d", len(xrFiles))
				}

				// Run diff on all XR files - this tests concurrent function processing
				output, log, err := RunXRDiff(t, c, "./crossplane-diff", xrFiles...)

				// Always log output for debugging
				t.Logf("crossplane-diff stdout: %s", output)
				t.Logf("crossplane-diff stderr: %s", log)

				if err != nil {
					t.Fatalf("Error running diff command: %v", err)
				}

				// Verify we processed all XRs - each XR creates 1 NopResource
				// With 21 XRs, we should see 21 "+++ NopResource/" lines in the output
				addedCount := strings.Count(output, "+++ NopResource/")
				if addedCount != 21 {
					t.Errorf("Expected 21 NopResource additions, found %d", addedCount)
				}

				return ctx
			}).
			WithTeardown("DeletePrerequisites", funcs.AllOf(
				funcs.DeleteResourcesWithPropagationPolicy(setupPath, "*.yaml", metav1.DeletePropagationForeground),
				funcs.ResourcesDeletedWithin(3*time.Minute, setupPath, "*.yaml"),
			)).
			Feature(),
	)
}

// TestDiffNewNestedResourceV2 tests the crossplane diff command against net-new nested XR resources in v2 variant.
func TestDiffNewNestedResourceV2(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v2-nested")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("DiffNewNestedResourceV2").
			WithLabel(e2e.LabelArea, LabelAreaDiff).
			WithLabel(e2e.LabelSize, e2e.LabelSizeSmall).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithLabel(LabelCrossplaneVersion, CrossplaneVersionMain).
			WithSetup("CreatePrerequisites", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, setupPath, "*.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, setupPath, "*.yaml"),
			)).
			WithSetup("PrerequisitesAreReady", funcs.AllOf(
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "parent-definition.yaml", apiextensionsv1.WatchingComposite()),
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "child-definition.yaml", apiextensionsv1.WatchingComposite()),
			)).
			Assess("CanDiffNewNestedResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "new-parent-xr.yaml"))
				if err != nil {
					t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "new-parent-xr.ansi"), log)

				return ctx
			}).
			WithTeardown("DeletePrerequisites", funcs.AllOf(
				funcs.ResourcesDeletedAfterListedAreGone(3*time.Minute, setupPath, "*.yaml", nsNopList),
			)).
			Feature(),
	)
}

// TestDiffExistingNestedResourceV2 tests the crossplane diff command against existing nested XR resources in v2 variant.
func TestDiffExistingNestedResourceV2(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v2-nested")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("DiffExistingNestedResourceV2").
			WithLabel(e2e.LabelArea, LabelAreaDiff).
			WithLabel(e2e.LabelSize, e2e.LabelSizeSmall).
			WithLabel(config.LabelTestSuite, config.TestSuiteDefault).
			WithLabel(LabelCrossplaneVersion, CrossplaneVersionMain).
			WithSetup("CreatePrerequisites", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, setupPath, "*.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, setupPath, "*.yaml"),
			)).
			WithSetup("PrerequisitesAreReady", funcs.AllOf(
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "parent-definition.yaml", apiextensionsv1.WatchingComposite()),
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "child-definition.yaml", apiextensionsv1.WatchingComposite()),
			)).
			WithSetup("CreateExistingXR", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-parent-xr.yaml"),
				funcs.ResourcesCreatedWithin(1*time.Minute, manifests, "existing-parent-xr.yaml"),
			)).
			WithSetup("ExistingXRIsReady", funcs.AllOf(
				funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-parent-xr.yaml", xpv1.Available()),
			)).
			Assess("CanDiffExistingNestedResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "modified-parent-xr.yaml"))
				if err != nil {
					t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "existing-parent-xr.ansi"), log)

				return ctx
			}).
			WithTeardown("DeleteResources", funcs.AllOf(
				funcs.DeleteResources(manifests, "existing-parent-xr.yaml"),
				funcs.ResourcesDeletedWithin(2*time.Minute, manifests, "existing-parent-xr.yaml"),
			)).
			WithTeardown("DeletePrerequisites", funcs.AllOf(
				funcs.ResourcesDeletedAfterListedAreGone(3*time.Minute, setupPath, "*.yaml", nsNopList),
			)).
			Feature(),
	)
}
