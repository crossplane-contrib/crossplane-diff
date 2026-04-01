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

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
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

				_, jsonOutput, log, err := RunCompDiffJSON(t, c, "./crossplane-diff", exitCodeDiffDetected, filepath.Join(manifests, "updated-composition.yaml"))
				if err != nil {
					t.Fatalf("Error running comp diff command: %v\nLog output:\n%s", err, log)
				}

				// Verify composition diff has 1 XR with changes
				// Golden file shows composition changes:
				//   - Adds transforms to first patch (updated-%s)
				//   - Changes fmt in second patch from basic-%s to premium-%s
				// And downstream ClusterNopResource changes:
				//   config-data: existing-value → updated-existing-value
				//   resource-tier: basic-existing-value → premium-existing-value
				AssertStructuredCompDiff(t, jsonOutput, tu.ExpectCompDiff().
					WithComposition("xcompdiffresources.compdiff.example.org").
					WithCompositionModified(). // Composition itself is modified
					WithAffectedResources(1, 1, 0, 0).
					WithXRImpact("XCompDiffResource", "test-comp-resource", "", "changed").
					WithDownstreamSummary(0, 1, 0). // ClusterNopResource modified (downstream = composed resources only)
					WithDownstreamResource("modified", "ClusterNopResource", "", "").
					WithAnyName(). // Name is generated with random suffix
					WithFieldChange("metadata.annotations.config-data", "existing-value", "updated-existing-value").
					WithFieldChange("metadata.annotations.resource-tier", "basic-existing-value", "premium-existing-value").
					AndXR().
					AndComp().
					And())

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

				output, jsonOutput, log, err := RunCompDiffJSON(t, c, "./crossplane-diff", exitCodeDiffDetected, filepath.Join(manifests, "updated-composition.yaml"))
				if err != nil {
					t.Fatalf("Error running comp diff command: %v\nLog output:\n%s", err, log)
				}

				// With 29 XRs, each with changes, we should see 29 XRs with changes
				if len(output.Compositions) != 1 {
					t.Fatalf("Expected 1 composition, got %d", len(output.Compositions))
				}

				comp := output.Compositions[0]
				if comp.AffectedResources.Total != 29 {
					t.Errorf("Expected 29 affected XRs, got %d", comp.AffectedResources.Total)
				}

				if comp.AffectedResources.WithChanges != 29 {
					t.Errorf("Expected 29 XRs with changes, got %d", comp.AffectedResources.WithChanges)
				}

				// Verify composition is modified and field-level changes for one XR (all 29 follow the same pattern)
				// Golden file shows for test-fanout-resource-01:
				//   config-data: value-01 → updated-value-01
				//   resource-tier: basic-value-01 → premium-value-01
				AssertStructuredCompDiff(t, jsonOutput, tu.ExpectCompDiff().
					WithComposition("xcompdiffresources.fanout.example.org").
					WithCompositionModified(). // Composition itself is modified
					WithAffectedResources(29, 29, 0, 0).
					WithXRImpact("XCompDiffFanoutResource", "test-fanout-resource-01", "", "changed").
					WithDownstreamSummary(0, 1, 0).
					WithDownstreamResource("modified", "ClusterNopResource", "", "").
					WithAnyName(). // Name has generated suffix
					WithFieldChange("metadata.annotations.config-data", "value-01", "updated-value-01").
					WithFieldChange("metadata.annotations.resource-tier", "basic-value-01", "premium-value-01").
					AndXR().
					AndComp().
					And())

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

				_, jsonOutput, log, err := RunCompDiffJSON(t, c, "./crossplane-diff", exitCodeDiffDetected, filepath.Join(manifests, "updated-composition.yaml"))
				if err != nil {
					t.Fatalf("Error running comp diff command: %v\nLog output:\n%s", err, log)
				}

				// Verify composition is modified and XR has downstream changes
				// The updated composition uses getComposedResource to add an annotation
				// referencing another composed resource's name
				// Golden file shows for ClusterNopResource:
				//   metadata.annotations.getcomposed.example.org/source-bucket: <bucket-name> (added)
				AssertStructuredCompDiff(t, jsonOutput, tu.ExpectCompDiff().
					WithComposition("xgetcomposedresources.getcomposed.example.org").
					WithCompositionModified().         // Composition itself is modified (adds go-templating step)
					WithAffectedResources(1, 1, 0, 0). // total=1, changed=1, unchanged=0, errors=0
					WithXRImpact("XGetComposedResource", "test-getcomposed-resource", "", "changed").
					WithDownstreamSummary(0, 1, 0). // ClusterNopResource modified
					WithDownstreamResource("modified", "ClusterNopResource", "", "").
					WithAnyName(). // Name has generated suffix
					WithFieldValuePattern("metadata.annotations['getcomposed.example.org/source-bucket']", `test-getcomposed-resource-[a-z0-9]+`).
					AndXR().
					AndComp().
					And())

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

				_, jsonOutput, log, err := RunCompDiffJSON(t, c, "./crossplane-diff", exitCodeDiffDetected, filepath.Join(manifests, "updated-composition.yaml"))
				if err != nil {
					t.Fatalf("Error running comp diff command: %v\nLog output:\n%s", err, log)
				}

				// Verify composition diff has 2 affected resources:
				// 1. XNopClaimDiffResource (backing XR with generated name suffix, cluster-scoped)
				// 2. NopClaimDiffResource/test-comp-claim (the Claim, namespaced)
				//
				// Golden file shows composition changes:
				//   - configData: {{ .observed.composite.resource.spec.coolField }} → updated-{{ ... }}
				//   - resourceTier: basic → premium
				// Golden file shows for ClusterNopResource:
				//   spec.forProvider.fields.configData: claim-value-1 → updated-claim-value-1
				//   spec.forProvider.fields.resourceTier: basic → premium
				// Golden file shows for NopClaimDiffResource (the Claim):
				//   spec.compositionUpdatePolicy: Automatic (added field)
				AssertStructuredCompDiff(t, jsonOutput, tu.ExpectCompDiff().
					WithComposition("xnopclaimdiffresources.claimdiff.example.org").
					WithCompositionModified().         // Composition itself is modified
					WithAffectedResources(2, 2, 0, 0). // Both XR and Claim have changes
					// XR impact - backing XR with generated name
					WithXRImpact("XNopClaimDiffResource", "", "", "changed").
					WithAnyName().                  // XR name is generated (test-comp-claim-XXXXX)
					WithDownstreamSummary(0, 1, 0). // ClusterNopResource modified
					WithDownstreamResource("modified", "ClusterNopResource", "", "").
					WithAnyName(). // ClusterNopResource name matches XR name
					WithFieldChange("spec.forProvider.fields.configData", "claim-value-1", "updated-claim-value-1").
					WithFieldChange("spec.forProvider.fields.resourceTier", "basic", "premium").
					AndXR().
					AndComp().
					// Claim impact
					WithXRImpact("NopClaimDiffResource", "test-comp-claim", "default", "changed").
					WithDownstreamSummary(0, 1, 0). // The Claim itself is modified in downstream
					WithDownstreamResource("modified", "NopClaimDiffResource", "test-comp-claim", "default").
					WithFieldAdded("spec.compositionUpdatePolicy", "Automatic").
					AndXR().
					AndComp().
					And())

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
