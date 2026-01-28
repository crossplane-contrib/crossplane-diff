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
			)).
			Assess("CanDiffNewResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", exitCodeDiffDetected, filepath.Join(manifests, "new-xr.yaml"))
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
			)).
			WithSetup("CreateExistingXR", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-xr.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "existing-xr.yaml"),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-xr.yaml", xpv1.Available()),
			)).
			Assess("CanDiffExistingResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", exitCodeDiffDetected, filepath.Join(manifests, "modified-xr.yaml"))
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
			)).
			Assess("CanDiffNewResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", exitCodeDiffDetected, filepath.Join(manifests, "new-xr.yaml"))
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
			)).
			WithSetup("CreateExistingXR", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-xr.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "existing-xr.yaml"),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-xr.yaml", xpv1.Available()),
			)).
			Assess("CanDiffExistingResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", exitCodeDiffDetected, filepath.Join(manifests, "modified-xr.yaml"))
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
			)).
			Assess("CanDiffNewResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", exitCodeDiffDetected, filepath.Join(manifests, "new-xr.yaml"))
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
			)).
			WithSetup("CreateXR", funcs.AllOf(
				funcs.ApplyResources(e2e.FieldManager, manifests, "existing-xr.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "existing-xr.yaml"),
				funcs.ResourcesHaveConditionWithin(2*time.Minute, manifests, "existing-xr.yaml", xpv1.Available()),
			)).
			Assess("CanDiffExistingResource", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				t.Helper()

				output, log, err := RunXRDiff(t, c, "./crossplane-diff", exitCodeDiffDetected, filepath.Join(manifests, "modified-xr.yaml"))
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
