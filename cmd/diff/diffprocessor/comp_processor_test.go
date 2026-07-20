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

package diffprocessor

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/types"
	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	gcmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
)

func TestDefaultCompDiffProcessor_findResourcesUsingComposition(t *testing.T) {
	ctx := t.Context()

	// Create test XR data
	xr1 := tu.NewResource("example.org/v1", "XResource", "xr-1").
		WithNamespace("default").
		Build()
	xr2 := tu.NewResource("example.org/v1", "XResource", "xr-2").
		WithNamespace("custom-ns").
		Build()

	tests := map[string]struct {
		compositionName string
		namespace       string
		setupMocks      func() xp.Clients
		want            []*un.Unstructured
		wantErr         bool
	}{
		"SuccessfulFind": {
			compositionName: "test-composition",
			namespace:       "default",
			setupMocks: func() xp.Clients {
				return xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithResourcesForComposition("test-composition", "default", []*un.Unstructured{xr1}).
						Build(),
				}
			},
			want:    []*un.Unstructured{xr1},
			wantErr: false,
		},
		"DifferentNamespace": {
			compositionName: "test-composition",
			namespace:       "custom-ns",
			setupMocks: func() xp.Clients {
				return xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithResourcesForComposition("test-composition", "custom-ns", []*un.Unstructured{xr2}).
						Build(),
				}
			},
			want:    []*un.Unstructured{xr2},
			wantErr: false,
		},
		"ClientError": {
			compositionName: "test-composition",
			namespace:       "default",
			setupMocks: func() xp.Clients {
				return xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithFindResourcesError("client error").
						Build(),
				}
			},
			want:    nil,
			wantErr: true,
		},
		"NoXRsFound": {
			compositionName: "test-composition",
			namespace:       "default",
			setupMocks: func() xp.Clients {
				return xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithResourcesForComposition("test-composition", "default", []*un.Unstructured{}).
						Build(),
				}
			},
			want:    []*un.Unstructured{},
			wantErr: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Create processor
			mocks := tt.setupMocks()
			processor := &DefaultCompDiffProcessor{
				compositionClient: mocks.Composition,
				config: ProcessorConfig{
					Logger: tu.TestLogger(t, false),
				},
			}

			comp := &un.Unstructured{}
			comp.SetName(tt.compositionName)
			got, err := processor.compositionClient.FindComposites(ctx, comp, types.FindCompositesOptions{Namespace: tt.namespace})

			if (err != nil) != tt.wantErr {
				t.Errorf("findResourcesUsingComposition() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if diff := gcmp.Diff(tt.want, got); diff != "" {
				t.Errorf("findResourcesUsingComposition() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDefaultCompDiffProcessor_DiffComposition(t *testing.T) {
	ctx := t.Context()

	// Create test composition
	testComp := tu.NewComposition("test-composition").
		WithCompositeTypeRef("example.org/v1", "XResource").
		WithPipelineMode().
		Build()

	// Create test XR
	testXR := tu.NewResource("example.org/v1", "XResource", "test-xr").
		WithNamespace("default").
		Build()

	tests := map[string]struct {
		compositions []*un.Unstructured
		namespace    string
		setupMocks   func() xp.Clients
		verifyOutput func(t *testing.T, output string)
		wantErr      bool
	}{
		"SuccessfulDiff": {
			namespace: "default",
			compositions: []*un.Unstructured{
				tu.NewComposition("test-composition").
					WithCompositeTypeRef("example.org/v1", "XResource").
					WithPipelineMode().
					BuildAsUnstructured(),
			},
			setupMocks: func() xp.Clients {
				return xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionFetch(testComp).
						WithResourcesForComposition("test-composition", "default", []*un.Unstructured{testXR}).
						Build(),
					Definition:   tu.NewMockDefinitionClient().Build(),
					Environment:  tu.NewMockEnvironmentClient().Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}
			},
			verifyOutput: func(t *testing.T, output string) {
				t.Helper()
				// Should contain composition changes section
				if !strings.Contains(output, "=== Composition Changes ===") {
					t.Errorf("Expected output to contain composition changes section")
				}
				// Should contain affected XRs section
				if !strings.Contains(output, "=== Affected Composite Resources ===") {
					t.Errorf("Expected output to contain affected XRs section")
				}
			},
			wantErr: false,
		},
		"NoCompositions": {
			namespace:    "default",
			compositions: []*un.Unstructured{},
			setupMocks: func() xp.Clients {
				return xp.Clients{}
			},
			wantErr: true,
		},
		"MultipleCompositions": {
			namespace: "default",
			compositions: []*un.Unstructured{
				tu.NewComposition("test-composition-1").
					WithCompositeTypeRef("example.org/v1", "XResource").
					WithPipelineMode().
					BuildAsUnstructured(),
				tu.NewComposition("test-composition-2").
					WithCompositeTypeRef("example.org/v1", "XResource").
					WithPipelineMode().
					BuildAsUnstructured(),
			},
			setupMocks: func() xp.Clients {
				// Create test compositions for the multi-composition test
				testComp1 := tu.NewComposition("test-composition-1").
					WithCompositeTypeRef("example.org/v1", "XResource").
					WithPipelineMode().
					Build()

				testComp2 := tu.NewComposition("test-composition-2").
					WithCompositeTypeRef("example.org/v1", "XResource").
					WithPipelineMode().
					Build()

				return xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionFetches([]*apiextensionsv1.Composition{testComp1, testComp2}).
						WithResourcesForComposition("test-composition-1", "default", []*un.Unstructured{}).
						WithResourcesForComposition("test-composition-2", "default", []*un.Unstructured{}).
						Build(),
					Definition:   tu.NewMockDefinitionClient().Build(),
					Environment:  tu.NewMockEnvironmentClient().Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}
			},
			verifyOutput: func(t *testing.T, output string) {
				t.Helper()
				// Should contain composition changes sections for both compositions
				compositionChangesSections := strings.Count(output, "=== Composition Changes ===")
				if compositionChangesSections != 2 {
					t.Errorf("Expected 2 composition changes sections, got %d", compositionChangesSections)
				}
				// Should contain separator between compositions
				if !strings.Contains(output, strings.Repeat("=", 80)) {
					t.Errorf("Expected output to contain composition separator")
				}
			},
			wantErr: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			xpClients := tt.setupMocks()

			// Create mock XR processor
			mockXRProc := &tu.MockDiffProcessor{
				PerformDiffFn: func(_ context.Context, _ []*un.Unstructured, _ types.CompositionProvider) (bool, error) {
					return true, nil
				},
			}

			// Create processor using constructor to ensure all fields are initialized
			logger := tu.TestLogger(t, false)

			// Create stdout buffer first so it can be set in config
			var stdout bytes.Buffer

			config := ProcessorConfig{
				Colorize: false,
				Compact:  false,
				Logger:   logger,
				Stdout:   &stdout,         // Set stdout in config so renderers can access it
				Stderr:   &bytes.Buffer{}, // Discard stderr for tests
				RenderFunc: func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					return render.CompositionOutputs{
						CompositeResource: in.CompositeResource,
					}, nil
				},
			}
			config.SetDefaultFactories()
			diffOpts := config.GetDiffOptions()
			diffRenderer := config.Factories.DiffRenderer(logger, diffOpts)
			compDiffRenderer := config.Factories.CompDiffRenderer(logger, diffRenderer, diffOpts)

			processor := &DefaultCompDiffProcessor{
				compositionClient: xpClients.Composition,
				xrProc:            mockXRProc,
				config:            config,
				compDiffRenderer:  compDiffRenderer,
			}

			_, err := processor.DiffComposition(ctx, tt.compositions, tt.namespace, nil)

			if (err != nil) != tt.wantErr {
				t.Errorf("DiffComposition() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			if tt.verifyOutput != nil {
				tt.verifyOutput(t, stdout.String())
			}
		})
	}
}

func TestDefaultCompDiffProcessor_partitionXRsByUpdatePolicy(t *testing.T) {
	// compLabels are the labels of the edited composition being diffed (which the resulting
	// CompositionRevision would inherit). An Automatic XR's compositionRevisionSelector is evaluated
	// against these to decide whether the XR would adopt the new revision.
	type droppedWant struct {
		name   string
		reason renderer.FilterReason
	}

	tests := map[string]struct {
		includeManual bool
		compName      string // defaults to "test-comp" when empty
		compLabels    map[string]string
		xrs           []*un.Unstructured
		wantKept      []string
		wantDropped   []droppedWant
		wantErr       bool
	}{
		// AC2.5 (Manual side): --include-manual keeps Manual XRs...
		"IncludeManualTrue_KeepsManualXRs": {
			includeManual: true,
			compLabels:    map[string]string{"version": "0.0.2"},
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "manual-xr").WithNamespace("default").
					WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").Build(),
				tu.NewResource("example.org/v1", "XResource", "auto-xr").WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").Build(),
			},
			wantKept:    []string{"manual-xr", "auto-xr"},
			wantDropped: nil,
		},
		// AC2.5 (selector side): ...but --include-manual does NOT re-include selector-mismatched
		// Automatic XRs — they would never adopt the new revision.
		"IncludeManualTrue_StillDropsSelectorMismatchedAutomaticXRs": {
			includeManual: true,
			compLabels:    map[string]string{"version": "0.0.2"},
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "manual-xr").WithNamespace("default").
					WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").Build(),
				tu.NewResource("example.org/v1", "XResource", "auto-mismatch").WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					WithCompositionRevisionSelector(xp.CrossplaneAPIExtGroupV2, map[string]string{"version": "0.0.1"}, nil).Build(),
			},
			wantKept:    []string{"manual-xr"},
			wantDropped: []droppedWant{{name: "auto-mismatch", reason: renderer.FilterReasonRevisionSelectorMismatch}},
		},
		"IncludeManualFalse_FiltersManualXRs": {
			includeManual: false,
			compLabels:    map[string]string{"version": "0.0.2"},
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "manual-xr").WithNamespace("default").
					WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").Build(),
				tu.NewResource("example.org/v1", "XResource", "auto-xr").WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").Build(),
			},
			wantKept:    []string{"auto-xr"},
			wantDropped: []droppedWant{{name: "manual-xr", reason: renderer.FilterReasonManualPolicy}},
		},
		// AC2.1: Automatic XR whose selector does not match the composition labels is dropped.
		"AutomaticSelectorMismatch_Dropped": {
			includeManual: false,
			compLabels:    map[string]string{"version": "0.0.2"},
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "selector-old").WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					WithCompositionRevisionSelector(xp.CrossplaneAPIExtGroupV2, map[string]string{"version": "0.0.1"}, nil).Build(),
			},
			wantKept:    nil,
			wantDropped: []droppedWant{{name: "selector-old", reason: renderer.FilterReasonRevisionSelectorMismatch}},
		},
		// AC2.2: Automatic XR whose selector matches the composition labels is kept.
		"AutomaticSelectorMatch_Kept": {
			includeManual: false,
			compLabels:    map[string]string{"version": "0.0.2"},
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "selector-new").WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					WithCompositionRevisionSelector(xp.CrossplaneAPIExtGroupV2, map[string]string{"version": "0.0.2"}, nil).Build(),
			},
			wantKept:    []string{"selector-new"},
			wantDropped: nil,
		},
		// A selector that keys on crossplane.io/composition-name (via Exists) matches because the
		// composition's predicted revision labels include that stamped label (added by
		// predictedRevisionLabels from the composition's name). Guards that augmentation.
		"AutomaticSelectorOnCompositionName_Kept": {
			includeManual: false,
			compName:      "xnopresources.example.org",
			compLabels:    map[string]string{"version": "0.0.2"},
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "name-selector").WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					WithCompositionRevisionSelector(xp.CrossplaneAPIExtGroupV2, nil, []map[string]any{
						{"key": xp.LabelCompositionName, "operator": "Exists"},
					}).Build(),
			},
			wantKept:    []string{"name-selector"},
			wantDropped: nil,
		},
		// AC2.3: Automatic XR with no selector is kept (unchanged from prior behavior).
		"AutomaticNoSelector_Kept": {
			includeManual: false,
			compLabels:    map[string]string{"version": "0.0.2"},
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "no-selector").WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").Build(),
				tu.NewResource("example.org/v1", "XResource", "default-policy").WithNamespace("default").Build(),
			},
			wantKept:    []string{"no-selector", "default-policy"},
			wantDropped: nil,
		},
		// AC2.4: Manual XR with a matching selector is still dropped by policy (reason manual_policy),
		// not rescued by the selector match.
		"ManualWithMatchingSelector_DroppedByPolicy": {
			includeManual: false,
			compLabels:    map[string]string{"version": "0.0.2"},
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "manual-match").WithNamespace("default").
					WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
					WithCompositionRevisionSelector(xp.CrossplaneAPIExtGroupV2, map[string]string{"version": "0.0.2"}, nil).Build(),
			},
			wantKept:    nil,
			wantDropped: []droppedWant{{name: "manual-match", reason: renderer.FilterReasonManualPolicy}},
		},
		"EmptyList_ReturnsEmpty": {
			includeManual: false,
			compLabels:    map[string]string{"version": "0.0.2"},
			xrs:           []*un.Unstructured{},
			wantKept:      nil,
			wantDropped:   nil,
		},
		// Composition with no labels: an Automatic XR with a non-empty selector cannot match, so it
		// is dropped as a selector mismatch.
		"NoCompositionLabels_SelectorMismatch_Dropped": {
			includeManual: false,
			compLabels:    nil,
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "selector-xr").WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					WithCompositionRevisionSelector(xp.CrossplaneAPIExtGroupV2, map[string]string{"version": "0.0.2"}, nil).Build(),
			},
			wantKept:    nil,
			wantDropped: []droppedWant{{name: "selector-xr", reason: renderer.FilterReasonRevisionSelectorMismatch}},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			processor := &DefaultCompDiffProcessor{
				config: ProcessorConfig{
					IncludeManual: tt.includeManual,
					Logger:        tu.TestLogger(t, false),
				},
			}

			compName := tt.compName
			if compName == "" {
				compName = "test-comp"
			}

			newComp := tu.NewComposition(compName).
				WithCompositeTypeRef("example.org/v1", "XResource").
				WithLabels(tt.compLabels).
				BuildAsUnstructured()

			kept, dropped, err := processor.partitionXRsByUpdatePolicy(tt.xrs, newComp)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("partitionXRsByUpdatePolicy() expected error, got nil")
				}

				return
			}

			if err != nil {
				t.Fatalf("partitionXRsByUpdatePolicy() unexpected error: %v", err)
			}

			gotKept := make([]string, len(kept))
			for i, xr := range kept {
				gotKept[i] = xr.GetName()
			}

			if diff := gcmp.Diff(tt.wantKept, gotKept, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("kept XR names mismatch (-want +got):\n%s", diff)
			}

			gotDropped := make([]droppedWant, len(dropped))
			for i, d := range dropped {
				gotDropped[i] = droppedWant{name: d.xr.GetName(), reason: d.reason}
			}

			if diff := gcmp.Diff(tt.wantDropped, gotDropped, cmpopts.EquateEmpty(), gcmp.AllowUnexported(droppedWant{})); diff != "" {
				t.Errorf("dropped XRs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPredictedRevisionLabels verifies the label set used to evaluate an XR's
// compositionRevisionSelector mirrors what a real CompositionRevision would carry: the composition's
// own labels plus crossplane.io/composition-name. The source composition must not be mutated.
func TestPredictedRevisionLabels(t *testing.T) {
	newComp := tu.NewComposition("xnopresources.example.org").
		WithCompositeTypeRef("example.org/v1", "XR").
		WithLabels(map[string]string{"version": "0.0.2"}).
		BuildAsUnstructured()

	got := predictedRevisionLabels(newComp)

	want := map[string]string{
		"version":               "0.0.2",
		xp.LabelCompositionName: "xnopresources.example.org",
	}

	if diff := gcmp.Diff(want, got); diff != "" {
		t.Errorf("predictedRevisionLabels() mismatch (-want +got):\n%s", diff)
	}

	// The source composition's own labels must be untouched (no composition-name injected).
	if _, injected := newComp.GetLabels()[xp.LabelCompositionName]; injected {
		t.Errorf("predictedRevisionLabels mutated the source composition's labels")
	}
}

// TestXRUpdatePolicy exercises the shared xp.XRUpdatePolicy reader (v2/v1 path precedence and the
// Automatic default) through the diffprocessor package that depends on it.
func TestXRUpdatePolicy(t *testing.T) {
	tests := map[string]struct {
		xr   *un.Unstructured
		want string
	}{
		"V2Path_Manual": {
			xr: tu.NewResource("example.org/v1", "XResource", "test-xr").
				WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
				Build(),
			want: "Manual",
		},
		"V2Path_Automatic": {
			xr: tu.NewResource("example.org/v1", "XResource", "test-xr").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				Build(),
			want: "Automatic",
		},
		"V1Path_Manual": {
			xr: tu.NewResource("example.org/v1", "XResource", "test-xr").
				WithNestedField("Manual", "spec", "compositionUpdatePolicy").
				Build(),
			want: "Manual",
		},
		"V1Path_Automatic": {
			xr: tu.NewResource("example.org/v1", "XResource", "test-xr").
				WithNestedField("Automatic", "spec", "compositionUpdatePolicy").
				Build(),
			want: "Automatic",
		},
		"NoPolicy_DefaultsToAutomatic": {
			xr:   tu.NewResource("example.org/v1", "XResource", "test-xr").Build(),
			want: "Automatic",
		},
		"EmptyPolicy_DefaultsToAutomatic": {
			xr: tu.NewResource("example.org/v1", "XResource", "test-xr").
				WithNestedField("", "spec", "compositionUpdatePolicy").
				Build(),
			want: "Automatic",
		},
		"V2PathTakesPrecedenceOverV1Path": {
			xr: tu.NewResource("example.org/v1", "XResource", "test-xr").
				WithNestedField("Automatic", "spec", "compositionUpdatePolicy").
				WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
				Build(),
			want: "Manual", // v2 path value should be used
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := xp.XRUpdatePolicy(tt.xr.Object, tt.xr.GetAPIVersion())
			if err != nil {
				t.Fatalf("XRUpdatePolicy() unexpected error: %v", err)
			}

			if got != tt.want {
				t.Errorf("XRUpdatePolicy() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDefaultCompDiffProcessor_collectXRDiffs_NestedXRCompositionLookup verifies that
// when processing nested XRs, the composition provider correctly distinguishes between:
// - Root XR type (matching the CLI composition's compositeTypeRef): returns CLI composition
// - Nested XR types (different GVK): should look up from cluster, not return CLI composition
//
// This test documents a bug where the composition provider always returns the CLI composition
// regardless of the resource type, causing nested XRs to be rendered with the wrong composition.
func TestDefaultCompDiffProcessor_collectXRDiffs_NestedXRCompositionLookup(t *testing.T) {
	ctx := t.Context()

	// Create the CLI composition targeting XParentResource
	parentComposition := tu.NewComposition("parent-composition").
		WithCompositeTypeRef("parent.example.org/v1", "XParentResource").
		WithPipelineMode().
		Build()

	parentCompUnstructured := tu.NewComposition("parent-composition").
		WithCompositeTypeRef("parent.example.org/v1", "XParentResource").
		WithPipelineMode().
		BuildAsUnstructured()

	// Create a composition for the nested XR (different type)
	childComposition := tu.NewComposition("child-composition").
		WithCompositeTypeRef("child.example.org/v1", "XChildResource").
		WithPipelineMode().
		Build()

	// Create the root XR (matches CLI composition type)
	// Root XR is identified by GVK+name+namespace, which matches what's in the xrs slice
	rootXR := tu.NewResource("parent.example.org/v1", "XParentResource", "test-parent").
		Build()

	// Create a nested XR (different type - does NOT match CLI composition)
	// Nested XR has different GVK, so it should look up its composition from cluster
	nestedXR := tu.NewResource("child.example.org/v1", "XChildResource", "test-child").
		Build()

	tests := map[string]struct {
		description        string
		setupMocks         func() (xp.CompositionClient, *tu.MockDiffProcessor, *[]string)
		xrs                []*un.Unstructured
		cliComposition     *un.Unstructured
		wantRootCompName   string // Expected composition name for root XR
		wantNestedCompName string // Expected composition name for nested XR (if any)
	}{
		"RootXR_ReceivesCLIComposition_NestedXR_ReceivesClusterComposition": {
			description: "Verifies correct behavior: root XR gets CLI composition, nested XR gets its own composition from cluster",
			setupMocks: func() (xp.CompositionClient, *tu.MockDiffProcessor, *[]string) {
				// Track which compositions are returned for which resources
				compositionRequests := make([]string, 0)

				mockXRProc := &tu.MockDiffProcessor{
					DiffSingleResourceFn: func(ctx context.Context, res *un.Unstructured, compositionProvider types.CompositionProvider) (map[string]*dt.ResourceDiff, error) {
						// For the root XR, test what composition is returned
						comp, err := compositionProvider(ctx, res)
						if err != nil {
							return nil, err
						}

						compositionRequests = append(compositionRequests, fmt.Sprintf("%s/%s->%s", res.GetKind(), res.GetName(), comp.GetName()))

						// Simulate processing a nested XR by calling the provider with a different resource type
						// This is what ProcessNestedXRs does when it encounters nested XRs
						nestedComp, err := compositionProvider(ctx, nestedXR)
						if err != nil {
							return nil, err
						}

						compositionRequests = append(compositionRequests, fmt.Sprintf("%s/%s->%s", nestedXR.GetKind(), nestedXR.GetName(), nestedComp.GetName()))

						return make(map[string]*dt.ResourceDiff), nil
					},
				}

				mockCompClient := tu.NewMockCompositionClient().
					WithSuccessfulCompositionFetch(parentComposition).
					WithResourcesForComposition("parent-composition", "", []*un.Unstructured{rootXR}).
					// This will be called when looking up composition for nested XR
					WithSuccessfulCompositionMatch(childComposition).
					Build()

				return mockCompClient, mockXRProc, &compositionRequests
			},
			xrs:            []*un.Unstructured{rootXR},
			cliComposition: parentCompUnstructured,
			// Correct behavior: root XR gets CLI composition, nested XR gets its own from cluster
			wantRootCompName:   "parent-composition",
			wantNestedCompName: "child-composition",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			mockCompClient, mockXRProc, compositionRequests := tt.setupMocks()

			processor := &DefaultCompDiffProcessor{
				compositionClient: mockCompClient,
				xrProc:            mockXRProc,
				config: ProcessorConfig{
					Logger: tu.TestLogger(t, false),
				},
			}

			_ = processor.collectXRDiffs(ctx, tt.xrs, tt.cliComposition)

			// Verify the composition requests
			if len(*compositionRequests) < 2 {
				t.Fatalf("Expected at least 2 composition requests, got %d: %v", len(*compositionRequests), *compositionRequests)
			}

			// Check root XR composition
			rootRequest := (*compositionRequests)[0]

			expectedRoot := fmt.Sprintf("XParentResource/test-parent->%s", tt.wantRootCompName)
			if rootRequest != expectedRoot {
				t.Errorf("Root XR composition request mismatch: got %q, want %q", rootRequest, expectedRoot)
			}

			// Check nested XR composition - nested XR should get its own composition from cluster
			nestedRequest := (*compositionRequests)[1]
			expectedNested := fmt.Sprintf("XChildResource/test-child->%s", tt.wantNestedCompName)

			if nestedRequest != expectedNested {
				t.Errorf("Nested XR composition request mismatch: got %q, want %q", nestedRequest, expectedNested)
			}
		})
	}
}

// TestDefaultCompDiffProcessor_DiffComposition_StderrErrorOutput verifies that when
// XR processing fails, detailed errors are written to stderr for human visibility.
// This tests the WithStderr option and the stderr error output path.
func TestDefaultCompDiffProcessor_DiffComposition_StderrErrorOutput(t *testing.T) {
	ctx := t.Context()

	// Create test composition
	testComp := tu.NewComposition("test-composition").
		WithCompositeTypeRef("example.org/v1", "XResource").
		WithPipelineMode().
		Build()

	// Create test XRs - one will succeed, one will fail
	successXR := tu.NewResource("example.org/v1", "XResource", "success-xr").
		WithNamespace("default").
		Build()
	failXR := tu.NewResource("example.org/v1", "XResource", "fail-xr").
		WithNamespace("default").
		Build()

	// Setup mocks
	xpClients := xp.Clients{
		Composition: tu.NewMockCompositionClient().
			WithSuccessfulCompositionFetch(testComp).
			WithResourcesForComposition("test-composition", "default", []*un.Unstructured{successXR, failXR}).
			Build(),
		Definition:   tu.NewMockDefinitionClient().Build(),
		Environment:  tu.NewMockEnvironmentClient().Build(),
		Function:     tu.NewMockFunctionClient().Build(),
		ResourceTree: tu.NewMockResourceTreeClient().Build(),
	}

	// Create mock XR processor that fails for one XR
	mockXRProc := &tu.MockDiffProcessor{
		DiffSingleResourceFn: func(_ context.Context, res *un.Unstructured, _ types.CompositionProvider) (map[string]*dt.ResourceDiff, error) {
			if res.GetName() == "fail-xr" {
				return nil, fmt.Errorf("render pipeline failed: function timeout")
			}

			return make(map[string]*dt.ResourceDiff), nil
		},
	}

	// Create stderr buffer to capture error output
	var stderrBuf bytes.Buffer

	// Create processor with custom stderr
	logger := tu.TestLogger(t, false)
	processor := NewCompDiffProcessor(
		mockXRProc,
		xpClients.Composition,
		WithLogger(logger),
		WithColorize(false),
		WithCompact(false),
		WithStderr(&stderrBuf), // Use WithStderr to inject test buffer
	)

	// Run the diff - should succeed but report XR errors
	_, err := processor.DiffComposition(ctx, []*un.Unstructured{
		tu.NewComposition("test-composition").
			WithCompositeTypeRef("example.org/v1", "XResource").
			WithPipelineMode().
			BuildAsUnstructured(),
	}, "default", nil)

	// Should return error because one XR failed
	if err == nil {
		t.Error("Expected error due to XR processing failure, got nil")
	}

	// Verify stderr contains the error details
	stderrOutput := stderrBuf.String()
	if !strings.Contains(stderrOutput, "ERROR:") {
		t.Errorf("Expected stderr to contain 'ERROR:', got: %q", stderrOutput)
	}

	if !strings.Contains(stderrOutput, "XResource/fail-xr") {
		t.Errorf("Expected stderr to contain resource ID 'XResource/fail-xr', got: %q", stderrOutput)
	}

	if !strings.Contains(stderrOutput, "render pipeline failed") {
		t.Errorf("Expected stderr to contain error message 'render pipeline failed', got: %q", stderrOutput)
	}
}

// newCompProcessorForTest builds a DefaultCompDiffProcessor wrapping the given composition client
// for use by the --resource preflight tests below.
func newCompProcessorForTest(t *testing.T, compClient xp.CompositionClient, includeManual bool) (*DefaultCompDiffProcessor, *bytes.Buffer) {
	t.Helper()

	logger := tu.TestLogger(t, false)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	config := ProcessorConfig{
		IncludeManual: includeManual,
		Logger:        logger,
		Stdout:        stdout,
		Stderr:        stderr,
		RenderFunc: func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
			return render.CompositionOutputs{CompositeResource: in.CompositeResource}, nil
		},
	}
	config.SetDefaultFactories()

	diffOpts := config.GetDiffOptions()
	diffRenderer := config.Factories.DiffRenderer(logger, diffOpts)
	compRenderer := config.Factories.CompDiffRenderer(logger, diffRenderer, diffOpts)

	mockXR := &tu.MockDiffProcessor{
		DiffSingleResourceFn: func(context.Context, *un.Unstructured, types.CompositionProvider) (map[string]*dt.ResourceDiff, error) {
			return map[string]*dt.ResourceDiff{}, nil
		},
	}

	return &DefaultCompDiffProcessor{
		compositionClient: compClient,
		config:            config,
		xrProc:            mockXR,
		compDiffRenderer:  compRenderer,
	}, stdout
}

// TestDefaultCompDiffProcessor_DiffComposition_ResourceMode covers the --resource code path:
// preflight, fail-fast on globally-unmatched refs, and surfacing of policy-filtered composites.
func TestDefaultCompDiffProcessor_DiffComposition_ResourceMode(t *testing.T) {
	comp := tu.NewComposition("test-comp").
		WithCompositeTypeRef("example.org/v1", "XR").
		WithPipelineMode().
		BuildAsUnstructured()

	xr1 := tu.NewResource("example.org/v1", "XR", "xr-1").
		InNamespace("ns").
		WithSpecField("compositionRef", map[string]any{"name": "test-comp"}).
		Build()

	xr2 := tu.NewResource("example.org/v1", "XR", "xr-2").
		InNamespace("ns").
		WithSpecField("compositionRef", map[string]any{"name": "test-comp"}).
		Build()

	// Manual-policy XR (v2 path).
	manualXR := tu.NewResource("example.org/v1", "XR", "manual-xr").
		InNamespace("ns").
		WithSpecField("compositionRef", map[string]any{"name": "test-comp"}).
		WithSpecField("crossplane", map[string]any{"compositionUpdatePolicy": "Manual"}).
		Build()

	// DispatchesToCorrectFindMode: DiffComposition routes to ref-lookup vs default-discovery
	// based on whether `resources` is non-empty. Verification is implicit via mode-specific
	// helpers — WithResourcesForComposition errors on refs-mode, WithCompositesByRef errors on
	// default-discovery, so wrong-mode dispatch surfaces as a non-nil DiffComposition error.
	t.Run("DispatchesToCorrectFindMode", func(t *testing.T) {
		dispatchTests := map[string]struct {
			resources         []k8stypes.NamespacedName
			namespace         string
			compositionClient xp.CompositionClient
		}{
			"EmptyResources_DefaultDiscovery": {
				resources: nil,
				namespace: "ns",
				compositionClient: tu.NewMockCompositionClient().
					WithSuccessfulCompositionFetch(&apiextensionsv1.Composition{}).
					WithResourcesForComposition("test-comp", "ns", []*un.Unstructured{xr1}).
					Build(),
			},
			"WithResources_RefLookup": {
				resources: []k8stypes.NamespacedName{{Namespace: "ns", Name: "xr-1"}, {Namespace: "ns", Name: "xr-2"}},
				namespace: "",
				compositionClient: tu.NewMockCompositionClient().
					WithSuccessfulCompositionFetch(&apiextensionsv1.Composition{}).
					WithCompositesByRef(xr1, xr2).
					Build(),
			},
		}

		for name, tt := range dispatchTests {
			t.Run(name, func(t *testing.T) {
				proc, _ := newCompProcessorForTest(t, tt.compositionClient, false)
				if _, err := proc.DiffComposition(t.Context(), []*un.Unstructured{comp}, tt.namespace, tt.resources); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			})
		}
	})

	t.Run("ResourceMode_GloballyUnmatched_FailsFastNoRender", func(t *testing.T) {
		client := tu.NewMockCompositionClient().
			WithSuccessfulCompositionFetch(&apiextensionsv1.Composition{}).
			WithNoMatchingComposites().
			Build()

		proc, stdout := newCompProcessorForTest(t, client, false)

		_, err := proc.DiffComposition(t.Context(), []*un.Unstructured{comp}, "",
			[]k8stypes.NamespacedName{{Namespace: "ns", Name: "ghost"}})
		if err == nil {
			t.Fatal("expected error from globally-unmatched preflight, got nil")
		}

		if !strings.Contains(err.Error(), "ns/ghost") {
			t.Errorf("error message should name the unmatched ref, got: %v", err)
		}

		if stdout.Len() != 0 {
			t.Errorf("expected no output before fail-fast, got: %q", stdout.String())
		}
	})

	// SurfaceFilteredControlsImpactAnalysis: when surfaceFiltered=true (resource mode),
	// Manual-policy XRs are surfaced as XRStatusFiltered impacts (reason manual_policy); when false
	// (default-discovery), they're counted in the summary but absent from ImpactAnalysis.
	t.Run("SurfaceFilteredControlsImpactAnalysis", func(t *testing.T) {
		surfaceTests := map[string]struct {
			surfaceFiltered bool
			wantImpactCount int
			wantStatus      renderer.XRStatus     // empty when wantImpactCount == 0
			wantReason      renderer.FilterReason // empty when wantImpactCount == 0
		}{
			"ResourceMode_SurfacesFilteredXRs": {
				surfaceFiltered: true,
				wantImpactCount: 1,
				wantStatus:      renderer.XRStatusFiltered,
				wantReason:      renderer.FilterReasonManualPolicy,
			},
			"DefaultDiscovery_OmitsFilteredFromImpactAnalysis": {
				surfaceFiltered: false,
				wantImpactCount: 0,
			},
		}

		for name, tt := range surfaceTests {
			t.Run(name, func(t *testing.T) {
				client := tu.NewMockCompositionClient().
					WithSuccessfulCompositionFetch(&apiextensionsv1.Composition{}).
					Build()

				proc, _ := newCompProcessorForTest(t, client, false /* IncludeManual */)

				got, err := proc.processSingleComposition(t.Context(), comp, []*un.Unstructured{manualXR}, tt.surfaceFiltered)
				if err != nil {
					t.Fatalf("processSingleComposition: %v", err)
				}

				if len(got.ImpactAnalysis) != tt.wantImpactCount {
					t.Fatalf("ImpactAnalysis: got %d entries, want %d (%+v)", len(got.ImpactAnalysis), tt.wantImpactCount, got.ImpactAnalysis)
				}

				if tt.wantImpactCount > 0 && got.ImpactAnalysis[0].Status != tt.wantStatus {
					t.Errorf("ImpactAnalysis[0].Status: got %q, want %q", got.ImpactAnalysis[0].Status, tt.wantStatus)
				}

				if tt.wantImpactCount > 0 && got.ImpactAnalysis[0].FilterReason != tt.wantReason {
					t.Errorf("ImpactAnalysis[0].FilterReason: got %q, want %q", got.ImpactAnalysis[0].FilterReason, tt.wantReason)
				}

				if got.AffectedResources.FilteredByPolicy != 1 {
					t.Errorf("FilteredByPolicy: got %d, want 1", got.AffectedResources.FilteredByPolicy)
				}

				if got.AffectedResources.Total != 1 {
					t.Errorf("Total: got %d, want 1", got.AffectedResources.Total)
				}
			})
		}
	})

	// RevisionSelectorMismatchSurfacedWithReasonAndCounts: an Automatic XR whose
	// compositionRevisionSelector does not match the diffed composition's labels is surfaced as
	// filtered with reason revision_selector_mismatch (and a detail hint), and counted in
	// FilteredBySelector rather than FilteredByPolicy. (T3 / AC2.6, AC2.7, AC4.6)
	t.Run("RevisionSelectorMismatchSurfacedWithReasonAndCounts", func(t *testing.T) {
		labeledComp := tu.NewComposition("test-comp").
			WithCompositeTypeRef("example.org/v1", "XR").
			WithPipelineMode().
			WithLabels(map[string]string{"version": "0.0.2"}).
			BuildAsUnstructured()

		selectorMismatchXR := tu.NewResource("example.org/v1", "XR", "selector-xr").
			InNamespace("ns").
			WithSpecField("compositionRef", map[string]any{"name": "test-comp"}).
			WithSpecField("crossplane", map[string]any{"compositionUpdatePolicy": "Automatic"}).
			WithCompositionRevisionSelector(xp.CrossplaneAPIExtGroupV2, map[string]string{"version": "0.0.1"}, nil).
			Build()

		client := tu.NewMockCompositionClient().
			WithSuccessfulCompositionFetch(&apiextensionsv1.Composition{}).
			Build()

		proc, _ := newCompProcessorForTest(t, client, false /* IncludeManual */)

		got, err := proc.processSingleComposition(t.Context(), labeledComp, []*un.Unstructured{selectorMismatchXR}, true /* surfaceFiltered */)
		if err != nil {
			t.Fatalf("processSingleComposition: %v", err)
		}

		if len(got.ImpactAnalysis) != 1 {
			t.Fatalf("ImpactAnalysis: got %d entries, want 1 (%+v)", len(got.ImpactAnalysis), got.ImpactAnalysis)
		}

		impact := got.ImpactAnalysis[0]
		if impact.Status != renderer.XRStatusFiltered {
			t.Errorf("Status: got %q, want %q", impact.Status, renderer.XRStatusFiltered)
		}

		if impact.FilterReason != renderer.FilterReasonRevisionSelectorMismatch {
			t.Errorf("FilterReason: got %q, want %q", impact.FilterReason, renderer.FilterReasonRevisionSelectorMismatch)
		}

		if !strings.Contains(impact.FilterDetail, "does not match composition labels") {
			t.Errorf("FilterDetail: got %q, want it to explain the mismatch", impact.FilterDetail)
		}

		if got.AffectedResources.FilteredBySelector != 1 {
			t.Errorf("FilteredBySelector: got %d, want 1", got.AffectedResources.FilteredBySelector)
		}

		if got.AffectedResources.FilteredByPolicy != 0 {
			t.Errorf("FilteredByPolicy: got %d, want 0", got.AffectedResources.FilteredByPolicy)
		}

		if got.AffectedResources.Total != 1 {
			t.Errorf("Total: got %d, want 1", got.AffectedResources.Total)
		}
	})
}
