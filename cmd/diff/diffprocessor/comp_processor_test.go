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
	"io"
	"strings"
	"testing"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/types"
	gcmp "github.com/google/go-cmp/cmp"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/v2/cmd/crank/render"
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

			got, err := processor.compositionClient.FindCompositesUsingComposition(ctx, tt.compositionName, tt.namespace)

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
				PerformDiffFn: func(_ context.Context, stdout io.Writer, _ []*un.Unstructured, _ types.CompositionProvider) (bool, error) {
					_, err := stdout.Write([]byte("Mock XR diff output"))
					return true, err
				},
			}

			// Create processor using constructor to ensure all fields are initialized
			logger := tu.TestLogger(t, false)
			config := ProcessorConfig{
				Namespace: tt.namespace,
				Colorize:  false,
				Compact:   false,
				Logger:    logger,
				RenderFunc: func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
					return render.Outputs{
						CompositeResource: in.CompositeResource,
					}, nil
				},
			}
			config.SetDefaultFactories()
			diffRenderer := config.Factories.DiffRenderer(logger, config.GetDiffOptions())
			compDiffRenderer := config.Factories.CompDiffRenderer(logger, diffRenderer, config.Colorize)

			processor := &DefaultCompDiffProcessor{
				compositionClient: xpClients.Composition,
				xrProc:            mockXRProc,
				config:            config,
				compDiffRenderer:  compDiffRenderer,
			}

			var stdout bytes.Buffer

			_, err := processor.DiffComposition(ctx, &stdout, tt.compositions, tt.namespace)

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

// Note: Tests for pluralize and formatXRStatusSummary have been moved to
// renderer/comp_diff_renderer_test.go since the functions are now in the renderer package.

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

// Note: Test_buildXRStatusList has been moved to renderer/comp_diff_renderer_test.go
// since the function is now in the renderer package.

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

	var stdout bytes.Buffer

	// Run the diff - should succeed but report XR errors
	_, err := processor.DiffComposition(ctx, &stdout, []*un.Unstructured{
		tu.NewComposition("test-composition").
			WithCompositeTypeRef("example.org/v1", "XResource").
			WithPipelineMode().
			BuildAsUnstructured(),
	}, "default")

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
