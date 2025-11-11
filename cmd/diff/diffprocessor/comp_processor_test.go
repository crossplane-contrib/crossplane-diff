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
	"strings"
	"testing"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	gcmp "github.com/google/go-cmp/cmp"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
	"github.com/crossplane/crossplane/v2/cmd/crank/render"
)

func TestDefaultCompDiffProcessor_findXRsUsingComposition(t *testing.T) {
	ctx := context.Background()

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
						WithXRsForComposition("test-composition", "default", []*un.Unstructured{xr1}).
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
						WithXRsForComposition("test-composition", "custom-ns", []*un.Unstructured{xr2}).
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
						WithFindXRsError("client error").
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
						WithXRsForComposition("test-composition", "default", []*un.Unstructured{}).
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

			got, err := processor.compositionClient.FindXRsUsingComposition(ctx, tt.compositionName, tt.namespace)

			if (err != nil) != tt.wantErr {
				t.Errorf("findXRsUsingComposition() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if diff := gcmp.Diff(tt.want, got); diff != "" {
				t.Errorf("findXRsUsingComposition() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDefaultCompDiffProcessor_DiffComposition(t *testing.T) {
	ctx := context.Background()

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
				// Need to create the XRD here so it can be captured in the closure
				testXRD := tu.NewXRD("xresources.example.org", "example.org", "XResource").BuildAsUnstructured()

				return xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionFetch(testComp).
						WithXRsForComposition("test-composition", "default", []*un.Unstructured{testXR}).
						Build(),
					Definition: tu.NewMockDefinitionClient().
						WithSuccessfulXRDsFetch([]*un.Unstructured{}).
						WithXRDForXR(testXRD).
						WithIsClaimResource(func(_ context.Context, _ *un.Unstructured) bool {
							return false // testXR is an XR, not a claim
						}).
						Build(),
					Environment: tu.NewMockEnvironmentClient().WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).Build(),
					Function: tu.NewMockFunctionClient().
						WithSuccessfulFunctionsFetch([]pkgv1.Function{}).
						Build(),
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
						WithXRsForComposition("test-composition-1", "default", []*un.Unstructured{}).
						WithXRsForComposition("test-composition-2", "default", []*un.Unstructured{}).
						Build(),
					Definition:   tu.NewMockDefinitionClient().WithSuccessfulXRDsFetch([]*un.Unstructured{}).Build(),
					Environment:  tu.NewMockEnvironmentClient().WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).Build(),
					Function:     tu.NewMockFunctionClient().WithSuccessfulFunctionsFetch([]pkgv1.Function{}).Build(),
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

			// Create mock k8s clients
			// Create test CRD for XResource
			testCRD := makeTestCRD("xresources.example.org", "XResource", "example.org", "v1")

			k8Clients := k8.Clients{
				Resource: tu.NewMockResourceClient().
					WithEmptyListResources().
					WithResourceNotFound().
					Build(),
				Apply: tu.NewMockApplyClient().
					WithSuccessfulDryRun().
					Build(),
				Schema: tu.NewMockSchemaClient().
					WithSuccessfulCRDByNameFetch("xresources.example.org", testCRD).
					WithFoundCRD("example.org", "XResource", testCRD).
					WithGetAllCRDs(func() []*extv1.CustomResourceDefinition { return []*extv1.CustomResourceDefinition{testCRD} }).
					Build(),
			}

			// Create processor options
			opts := []ProcessorOption{
				WithNamespace(tt.namespace),
				WithColorize(false),
				WithCompact(false),
				WithLogger(tu.TestLogger(t, true)), // Enable debug logging
				WithRenderFunc(func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
					return render.Outputs{
						CompositeResource: in.CompositeResource,
					}, nil
				}),
				WithMaxNestedDepth(1),
				WithDiffProcessorFactory(func(k8Clients k8.Clients, xpClients xp.Clients, processorOpts []ProcessorOption) DiffProcessor {
					return NewDiffProcessor(k8Clients, xpClients, processorOpts...)
				}),
			}

			// Create processor using constructor
			processor := NewCompDiffProcessor(k8Clients, xpClients, opts...)

			var stdout bytes.Buffer

			err := processor.DiffComposition(ctx, &stdout, tt.compositions, tt.namespace)

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

func TestDefaultCompDiffProcessor_filterXRsByUpdatePolicy(t *testing.T) {
	tests := map[string]struct {
		includeManual bool
		xrs           []*un.Unstructured
		want          []*un.Unstructured
	}{
		"IncludeManualTrue_ReturnsAllXRs": {
			includeManual: true,
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "manual-xr").
					WithNamespace("default").
					WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
				tu.NewResource("example.org/v1", "XResource", "auto-xr").
					WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
			},
			want: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "manual-xr").
					WithNamespace("default").
					WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
				tu.NewResource("example.org/v1", "XResource", "auto-xr").
					WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
			},
		},
		"IncludeManualFalse_FiltersManualXRs": {
			includeManual: false,
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "manual-xr").
					WithNamespace("default").
					WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
				tu.NewResource("example.org/v1", "XResource", "auto-xr").
					WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
			},
			want: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "auto-xr").
					WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
			},
		},
		"IncludeManualFalse_AllManualXRs_ReturnsEmpty": {
			includeManual: false,
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "manual-xr-1").
					WithNamespace("default").
					WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
				tu.NewResource("example.org/v1", "XResource", "manual-xr-2").
					WithNamespace("default").
					WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
			},
			want: []*un.Unstructured{},
		},
		"IncludeManualFalse_AllAutomaticXRs_ReturnsAll": {
			includeManual: false,
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "auto-xr-1").
					WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
				tu.NewResource("example.org/v1", "XResource", "auto-xr-2").
					WithNamespace("default").
					Build(), // No policy specified, defaults to Automatic
			},
			want: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "auto-xr-1").
					WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
				tu.NewResource("example.org/v1", "XResource", "auto-xr-2").
					WithNamespace("default").
					Build(),
			},
		},
		"IncludeManualFalse_EmptyList_ReturnsEmpty": {
			includeManual: false,
			xrs:           []*un.Unstructured{},
			want:          []*un.Unstructured{},
		},
		"IncludeManualFalse_V1PathManualXR_FiltersCorrectly": {
			includeManual: false,
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "legacy-manual-xr").
					WithNestedField("Manual", "spec", "compositionUpdatePolicy").
					Build(),
				tu.NewResource("example.org/v1", "XResource", "auto-xr").
					WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
			},
			want: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "auto-xr").
					WithNamespace("default").
					WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
			},
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

			got := processor.filterXRsByUpdatePolicy(tt.xrs)

			if len(got) != len(tt.want) {
				t.Errorf("filterXRsByUpdatePolicy() returned %d XRs, want %d", len(got), len(tt.want))
			}

			// Compare XR names to verify correct filtering
			gotNames := make([]string, len(got))
			for i, xr := range got {
				gotNames[i] = xr.GetName()
			}

			wantNames := make([]string, len(tt.want))
			for i, xr := range tt.want {
				wantNames[i] = xr.GetName()
			}

			if diff := gcmp.Diff(wantNames, gotNames); diff != "" {
				t.Errorf("filterXRsByUpdatePolicy() XR names mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDefaultCompDiffProcessor_getCompositionUpdatePolicy(t *testing.T) {
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
			processor := &DefaultCompDiffProcessor{
				config: ProcessorConfig{
					Logger: tu.TestLogger(t, false),
				},
			}

			got := processor.getCompositionUpdatePolicy(tt.xr)

			if got != tt.want {
				t.Errorf("getCompositionUpdatePolicy() = %v, want %v", got, tt.want)
			}
		})
	}
}
