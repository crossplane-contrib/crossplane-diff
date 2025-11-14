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

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	xpv1 "github.com/crossplane/crossplane-runtime/v2/apis/common/v1"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/v2/test/e2e"
	"github.com/crossplane/crossplane/v2/test/e2e/config"
	"github.com/crossplane/crossplane/v2/test/e2e/funcs"
)

// TestDiffExistingComposition tests the crossplane comp diff command against existing XRs in the cluster.
func TestDiffExistingComposition(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "comp")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

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

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "existing-xr.ansi"), log)

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

// TestCompDiffLargeFanout tests the comp diff command with a large number of XRs (15) using the same composition.
// This validates that:
// 1. Container reuse works correctly across many XRs
// 2. Cleanup happens properly after processing all XRs
// 3. Diff output is generated for all XRs
// 4. No performance issues or failures with large fanout.
func TestCompDiffLargeFanout(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "comp-fanout")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("CompDiffLargeFanout").
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
			WithSetup("CreateExistingXRs", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-xrs.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "existing-xrs.yaml"),
				funcs.ResourcesHaveConditionWithin(3*time.Minute, manifests, "existing-xrs.yaml", xpv1.Available()),
			)).
			Assess("CanDiffCompositionWithLargeFanout", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunCompDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "updated-composition.yaml"))
				if err != nil {
					t.Fatalf("Error running comp diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "existing-xrs.ansi"), log)

				return ctx
			}).
			WithTeardown("DeleteExistingXRs", funcs.AllOf(
				funcs.DeleteResources(manifests, "existing-xrs.yaml"),
				funcs.ResourcesDeletedWithin(2*time.Minute, manifests, "existing-xrs.yaml"),
			)).
			WithTeardown("DeletePrerequisites", funcs.ResourcesDeletedAfterListedAreGone(3*time.Minute, setupPath, "*.yaml", clusterNopList)).
			Feature(),
	)
}

// TestDiffCompositionWithGetComposedResource tests the crossplane comp diff command with a composition that uses GetComposedResource.
func TestDiffCompositionWithGetComposedResource(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "comp-getcomposed")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("DiffCompositionWithGetComposedResource").
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
			)).
			WithSetup("CreateExistingXR", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-xr.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "existing-xr.yaml"),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-xr.yaml", xpv1.Available()),
			)).
			Assess("CanDiffCompositionWithGetComposedResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunCompDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "updated-composition.yaml"))
				if err != nil {
					t.Fatalf("Error running comp diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "existing-xr.ansi"), log)

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

// TestDiffCompositionWithClaims tests the crossplane comp diff command with Claims.
func TestDiffCompositionWithClaims(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "comp-claim")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("DiffCompositionWithClaims").
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
			)).
			WithSetup("CreateExistingClaim", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-claim.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "existing-claim.yaml"),
				// Claims get their status from the backing XR, so wait for the claim to be available
				funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-claim.yaml", xpv1.Available()),
			)).
			Assess("CanDiffCompositionWithClaim", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunCompDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "updated-composition.yaml"))
				if err != nil {
					t.Fatalf("Error running comp diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "existing-claim.ansi"), log)

				return ctx
			}).
			WithTeardown("DeleteExistingClaim", funcs.AllOf(
				funcs.DeleteResources(manifests, "existing-claim.yaml"),
				funcs.ResourcesDeletedWithin(2*time.Minute, manifests, "existing-claim.yaml"),
			)).
			WithTeardown("DeletePrerequisites", funcs.ResourcesDeletedAfterListedAreGone(3*time.Minute, setupPath, "*.yaml", clusterNopList)).
			Feature(),
	)
}
