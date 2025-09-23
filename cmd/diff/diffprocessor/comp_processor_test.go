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
	"io"
	"strings"
	"testing"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/types"
	gcmp "github.com/google/go-cmp/cmp"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
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
				return xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionFetch(testComp).
						WithXRsForComposition("test-composition", "default", []*un.Unstructured{testXR}).
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
						WithXRsForComposition("test-composition-1", "default", []*un.Unstructured{}).
						WithXRsForComposition("test-composition-2", "default", []*un.Unstructured{}).
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
				PerformDiffFn: func(_ context.Context, stdout io.Writer, _ []*un.Unstructured, _ types.CompositionProvider) error {
					_, err := stdout.Write([]byte("Mock XR diff output"))
					return err
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
