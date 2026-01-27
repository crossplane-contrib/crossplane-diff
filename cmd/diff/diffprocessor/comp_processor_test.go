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
	"errors"
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

			// Create processor
			processor := &DefaultCompDiffProcessor{
				compositionClient: xpClients.Composition,
				xrProc:            mockXRProc,
				config: ProcessorConfig{
					Namespace: tt.namespace,
					Colorize:  false,
					Compact:   false,
					Logger:    tu.TestLogger(t, false),
					RenderFunc: func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
						return render.Outputs{
							CompositeResource: in.CompositeResource,
						}, nil
					},
				},
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

func Test_pluralize(t *testing.T) {
	tests := map[string]struct {
		count int
		want  string
	}{
		"Zero": {
			count: 0,
			want:  "s",
		},
		"One": {
			count: 1,
			want:  "",
		},
		"Two": {
			count: 2,
			want:  "s",
		},
		"Many": {
			count: 100,
			want:  "s",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := pluralize(tt.count)
			if got != tt.want {
				t.Errorf("pluralize(%d) = %q, want %q", tt.count, got, tt.want)
			}
		})
	}
}

func Test_formatXRStatusSummary(t *testing.T) {
	tests := map[string]struct {
		changedCount   int
		unchangedCount int
		errorCount     int
		want           string
	}{
		"NoResources": {
			changedCount:   0,
			unchangedCount: 0,
			errorCount:     0,
			want:           "\nSummary: \n",
		},
		"OneChanged_Only": {
			changedCount:   1,
			unchangedCount: 0,
			errorCount:     0,
			want:           "\nSummary: 1 resource with changes\n",
		},
		"OneUnchanged_Only": {
			changedCount:   0,
			unchangedCount: 1,
			errorCount:     0,
			want:           "\nSummary: 1 resource unchanged\n",
		},
		"OneError_Only": {
			changedCount:   0,
			unchangedCount: 0,
			errorCount:     1,
			want:           "\nSummary: 1 resource with errors\n",
		},
		"OneChanged_OneUnchanged": {
			changedCount:   1,
			unchangedCount: 1,
			errorCount:     0,
			want:           "\nSummary: 1 resource with changes, 1 resource unchanged\n",
		},
		"MultipleChanged_MultipleUnchanged": {
			changedCount:   5,
			unchangedCount: 3,
			errorCount:     0,
			want:           "\nSummary: 5 resources with changes, 3 resources unchanged\n",
		},
		"ManyChanged_Only": {
			changedCount:   100,
			unchangedCount: 0,
			errorCount:     0,
			want:           "\nSummary: 100 resources with changes\n",
		},
		"ManyUnchanged_Only": {
			changedCount:   0,
			unchangedCount: 50,
			errorCount:     0,
			want:           "\nSummary: 50 resources unchanged\n",
		},
		"AllThreeTypes": {
			changedCount:   2,
			unchangedCount: 3,
			errorCount:     1,
			want:           "\nSummary: 2 resources with changes, 3 resources unchanged, 1 resource with errors\n",
		},
		"MultipleErrors": {
			changedCount:   1,
			unchangedCount: 0,
			errorCount:     5,
			want:           "\nSummary: 1 resource with changes, 5 resources with errors\n",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := formatXRStatusSummary(tt.changedCount, tt.unchangedCount, tt.errorCount)
			if got != tt.want {
				t.Errorf("formatXRStatusSummary(%d, %d, %d) = %q, want %q",
					tt.changedCount, tt.unchangedCount, tt.errorCount, got, tt.want)
			}
		})
	}
}

func Test_buildXRStatusList(t *testing.T) {
	tests := map[string]struct {
		xrs           []*un.Unstructured
		results       map[string]*XRDiffResult
		colorize      bool
		wantChanged   int
		wantUnchanged int
		wantError     int
		validateList  func(t *testing.T, list string)
	}{
		"EmptyList": {
			xrs:           []*un.Unstructured{},
			results:       map[string]*XRDiffResult{},
			colorize:      false,
			wantChanged:   0,
			wantUnchanged: 0,
			wantError:     0,
			validateList: func(t *testing.T, list string) {
				t.Helper()

				if list != "" {
					t.Errorf("Expected empty list, got: %q", list)
				}
			},
		},
		"SingleUnchangedResource_NoColor": {
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "test-xr").
					WithNamespace("default").
					Build(),
			},
			results: map[string]*XRDiffResult{
				"XResource/test-xr": {
					Diffs: make(map[string]*dt.ResourceDiff),
					Error: nil,
				},
			},
			colorize:      false,
			wantChanged:   0,
			wantUnchanged: 1,
			wantError:     0,
			validateList: func(t *testing.T, list string) {
				t.Helper()

				if !strings.Contains(list, "✓ XResource/test-xr") {
					t.Errorf("Expected checkmark for unchanged resource, got: %q", list)
				}

				if !strings.Contains(list, "namespace: default") {
					t.Errorf("Expected namespace info, got: %q", list)
				}
			},
		},
		"SingleChangedResource_NoColor": {
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "test-xr").
					WithNamespace("default").
					Build(),
			},
			results: map[string]*XRDiffResult{
				"XResource/test-xr": {
					Diffs: map[string]*dt.ResourceDiff{"some-resource": {}},
					Error: nil,
				},
			},
			colorize:      false,
			wantChanged:   1,
			wantUnchanged: 0,
			wantError:     0,
			validateList: func(t *testing.T, list string) {
				t.Helper()

				if !strings.Contains(list, "⚠ XResource/test-xr") {
					t.Errorf("Expected warning mark for changed resource, got: %q", list)
				}
			},
		},
		"MixedResources_NoColor": {
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "unchanged-xr").
					WithNamespace("default").
					Build(),
				tu.NewResource("example.org/v1", "XResource", "changed-xr").
					WithNamespace("default").
					Build(),
			},
			results: map[string]*XRDiffResult{
				"XResource/unchanged-xr": {
					Diffs: make(map[string]*dt.ResourceDiff),
					Error: nil,
				},
				"XResource/changed-xr": {
					Diffs: map[string]*dt.ResourceDiff{"some-resource": {}},
					Error: nil,
				},
			},
			colorize:      false,
			wantChanged:   1,
			wantUnchanged: 1,
			wantError:     0,
			validateList: func(t *testing.T, list string) {
				t.Helper()

				if !strings.Contains(list, "✓ XResource/unchanged-xr") {
					t.Errorf("Expected checkmark for unchanged resource")
				}

				if !strings.Contains(list, "⚠ XResource/changed-xr") {
					t.Errorf("Expected warning mark for changed resource")
				}
			},
		},
		"ClusterScopedResource_NoColor": {
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "ClusterXResource", "cluster-xr").
					Build(), // No namespace = cluster-scoped
			},
			results: map[string]*XRDiffResult{
				"ClusterXResource/cluster-xr": {
					Diffs: make(map[string]*dt.ResourceDiff),
					Error: nil,
				},
			},
			colorize:      false,
			wantChanged:   0,
			wantUnchanged: 1,
			wantError:     0,
			validateList: func(t *testing.T, list string) {
				t.Helper()

				if !strings.Contains(list, "cluster-scoped") {
					t.Errorf("Expected cluster-scoped indicator, got: %q", list)
				}
			},
		},
		"SingleResource_WithColor": {
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "test-xr").
					WithNamespace("default").
					Build(),
			},
			results: map[string]*XRDiffResult{
				"XResource/test-xr": {
					Diffs: make(map[string]*dt.ResourceDiff),
					Error: nil,
				},
			},
			colorize:      true,
			wantChanged:   0,
			wantUnchanged: 1,
			wantError:     0,
			validateList: func(t *testing.T, list string) {
				t.Helper()
				// Should contain green ANSI code for unchanged resource
				if !strings.Contains(list, "\x1b[32m") {
					t.Errorf("Expected green ANSI color code, got: %q", list)
				}
				// Should contain reset code
				if !strings.Contains(list, "\x1b[0m") {
					t.Errorf("Expected ANSI reset code, got: %q", list)
				}
			},
		},
		"ChangedResource_WithColor": {
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "test-xr").
					WithNamespace("default").
					Build(),
			},
			results: map[string]*XRDiffResult{
				"XResource/test-xr": {
					Diffs: map[string]*dt.ResourceDiff{"some-resource": {}},
					Error: nil,
				},
			},
			colorize:      true,
			wantChanged:   1,
			wantUnchanged: 0,
			wantError:     0,
			validateList: func(t *testing.T, list string) {
				t.Helper()
				// Should contain yellow ANSI code for changed resource
				if !strings.Contains(list, "\x1b[33m") {
					t.Errorf("Expected yellow ANSI color code, got: %q", list)
				}
				// Should contain reset code
				if !strings.Contains(list, "\x1b[0m") {
					t.Errorf("Expected ANSI reset code, got: %q", list)
				}
			},
		},
		"MultipleNamespaces": {
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "xr-1").
					WithNamespace("namespace-a").
					Build(),
				tu.NewResource("example.org/v1", "XResource", "xr-2").
					WithNamespace("namespace-b").
					Build(),
			},
			results: map[string]*XRDiffResult{
				"XResource/xr-1": {
					Diffs: make(map[string]*dt.ResourceDiff),
					Error: nil,
				},
				"XResource/xr-2": {
					Diffs: map[string]*dt.ResourceDiff{"some-resource": {}},
					Error: nil,
				},
			},
			colorize:      false,
			wantChanged:   1,
			wantUnchanged: 1,
			wantError:     0,
			validateList: func(t *testing.T, list string) {
				t.Helper()

				if !strings.Contains(list, "namespace: namespace-a") {
					t.Errorf("Expected namespace-a in output")
				}

				if !strings.Contains(list, "namespace: namespace-b") {
					t.Errorf("Expected namespace-b in output")
				}
			},
		},
		"ResourceWithError_NoColor": {
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "error-xr").
					WithNamespace("default").
					Build(),
			},
			results: map[string]*XRDiffResult{
				"XResource/error-xr": {
					Diffs: make(map[string]*dt.ResourceDiff),
					Error: errors.New("processing failed"),
				},
			},
			colorize:      false,
			wantChanged:   0,
			wantUnchanged: 0,
			wantError:     1,
			validateList: func(t *testing.T, list string) {
				t.Helper()

				if !strings.Contains(list, "✗ XResource/error-xr") {
					t.Errorf("Expected error mark for resource with error, got: %q", list)
				}
			},
		},
		"ResourceWithError_WithColor": {
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "error-xr").
					WithNamespace("default").
					Build(),
			},
			results: map[string]*XRDiffResult{
				"XResource/error-xr": {
					Diffs: make(map[string]*dt.ResourceDiff),
					Error: errors.New("processing failed"),
				},
			},
			colorize:      true,
			wantChanged:   0,
			wantUnchanged: 0,
			wantError:     1,
			validateList: func(t *testing.T, list string) {
				t.Helper()
				// Should contain red ANSI code for error resource
				if !strings.Contains(list, "\x1b[31m") {
					t.Errorf("Expected red ANSI color code, got: %q", list)
				}
				// Should contain reset code
				if !strings.Contains(list, "\x1b[0m") {
					t.Errorf("Expected ANSI reset code, got: %q", list)
				}
			},
		},
		"MixedResources_WithErrors": {
			xrs: []*un.Unstructured{
				tu.NewResource("example.org/v1", "XResource", "unchanged-xr").
					WithNamespace("default").
					Build(),
				tu.NewResource("example.org/v1", "XResource", "changed-xr").
					WithNamespace("default").
					Build(),
				tu.NewResource("example.org/v1", "XResource", "error-xr").
					WithNamespace("default").
					Build(),
			},
			results: map[string]*XRDiffResult{
				"XResource/unchanged-xr": {
					Diffs: make(map[string]*dt.ResourceDiff),
					Error: nil,
				},
				"XResource/changed-xr": {
					Diffs: map[string]*dt.ResourceDiff{"some-resource": {}},
					Error: nil,
				},
				"XResource/error-xr": {
					Diffs: make(map[string]*dt.ResourceDiff),
					Error: errors.New("processing failed"),
				},
			},
			colorize:      false,
			wantChanged:   1,
			wantUnchanged: 1,
			wantError:     1,
			validateList: func(t *testing.T, list string) {
				t.Helper()

				if !strings.Contains(list, "✓ XResource/unchanged-xr") {
					t.Errorf("Expected checkmark for unchanged resource")
				}

				if !strings.Contains(list, "⚠ XResource/changed-xr") {
					t.Errorf("Expected warning mark for changed resource")
				}

				if !strings.Contains(list, "✗ XResource/error-xr") {
					t.Errorf("Expected error mark for resource with error")
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			gotList, gotChanged, gotUnchanged, gotError := buildXRStatusList(tt.xrs, tt.results, tt.colorize)

			if gotChanged != tt.wantChanged {
				t.Errorf("buildXRStatusList() changed count = %d, want %d", gotChanged, tt.wantChanged)
			}

			if gotUnchanged != tt.wantUnchanged {
				t.Errorf("buildXRStatusList() unchanged count = %d, want %d", gotUnchanged, tt.wantUnchanged)
			}

			if gotError != tt.wantError {
				t.Errorf("buildXRStatusList() error count = %d, want %d", gotError, tt.wantError)
			}

			if tt.validateList != nil {
				tt.validateList(t, gotList)
			}
		})
	}
}
