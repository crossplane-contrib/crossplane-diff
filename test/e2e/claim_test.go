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
	"slices"
	"strings"
	"testing"
	"time"

	k8sapiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	xpv1 "github.com/crossplane/crossplane-runtime/v2/apis/common/v1"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/v2/test/e2e"
	"github.com/crossplane/crossplane/v2/test/e2e/config"
	"github.com/crossplane/crossplane/v2/test/e2e/funcs"
)

// TestDiffNewClaim tests the crossplane diff command against net-new claims with v1 XRDs.
func TestDiffNewClaim(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v1-claim")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("DiffNewClaim").
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
			)).
			Assess("CanDiffNewClaim", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "new-claim.yaml"))
				if err != nil {
					t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "new-claim.ansi"), log)

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

// TestDiffExistingClaim tests the crossplane diff command against existing claims with v1 XRDs.
func TestDiffExistingClaim(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v1-claim")
	setupPath := filepath.Join(manifests, "setup")
	expectPath := filepath.Join(manifests, "expect")

	environment.Test(t,
		features.New("DiffExistingClaim").
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
			)).
			WithSetup("CreateClaim", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-claim.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "existing-claim.yaml"),
				// Claims get their status from the backing XR, so wait for the XR to be available
				funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-claim.yaml", xpv1.Available()),
			)).
			Assess("CanDiffExistingClaim", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "modified-claim.yaml"))
				if err != nil {
					t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
				}

				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "existing-claim.ansi"), log)

				return ctx
			}).
			WithTeardown("DeleteClaim", funcs.AllOf(
				funcs.DeleteResources(manifests, "existing-claim.yaml"),
				funcs.ResourcesDeletedWithin(2*time.Minute, manifests, "existing-claim.yaml"),
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

// TestDiffNewClaimWithNestedXRs tests the crossplane diff command against new claims that create nested XRs.
// This covers the case where a claim creates an XR that itself creates child XRs (nested composition).
func TestDiffNewClaimWithNestedXRs(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v1-claim-nested")
	setupPath := filepath.Join(manifests, "setup")

	environment.Test(t,
		features.New("DiffNewClaimWithNestedXRs").
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
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "parent-definition.yaml", apiextensionsv1.WatchingComposite()),
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "child-definition.yaml", apiextensionsv1.WatchingComposite()),
			)).
			Assess("CanDiffNewClaimWithNestedXRs", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "new-claim.yaml"))
				if err != nil {
					t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
				}

				expectPath := filepath.Join(manifests, "expect")
				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "new-claim.ansi"), log)

				return ctx
			}).
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
			)).
			Feature(),
	)
}

// TestDiffExistingClaimWithNestedXRs tests the crossplane diff command against existing claims that create nested XRs.
// This test verifies that nested XR identity is preserved when diffing modified claims.
func TestDiffExistingClaimWithNestedXRs(t *testing.T) {
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]
	manifests := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "v1-claim-nested")
	setupPath := filepath.Join(manifests, "setup")

	environment.Test(t,
		features.New("DiffExistingClaimWithNestedXRs").
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
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "parent-definition.yaml", apiextensionsv1.WatchingComposite()),
				funcs.ResourcesHaveConditionWithin(1*time.Minute, setupPath, "child-definition.yaml", apiextensionsv1.WatchingComposite()),
			)).
			WithSetup("CreateClaim", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-claim.yaml"),
				funcs.ResourcesCreatedWithin(1*time.Minute, manifests, "existing-claim.yaml"),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-claim.yaml", xpv1.Available()),
			)).
			Assess("CanDiffExistingClaimWithNestedXRs", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", filepath.Join(manifests, "modified-claim.yaml"))
				if err != nil {
					t.Fatalf("Error running diff command: %v\nLog output:\n%s", err, log)
				}

				expectPath := filepath.Join(manifests, "expect")
				assertDiffMatchesFile(t, output, filepath.Join(expectPath, "existing-claim.ansi"), log)

				return ctx
			}).
			WithTeardown("DeleteClaim", funcs.AllOf(
				funcs.DeleteResources(manifests, "existing-claim.yaml"),
				funcs.ResourcesDeletedWithin(2*time.Minute, manifests, "existing-claim.yaml"),
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
			)).
			Feature(),
	)
}
