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
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/types"
	gcmp "github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
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
						WithFindXRsUsingComposition(func(_ context.Context, compositionName, namespace string) ([]*un.Unstructured, error) {
							if compositionName == "test-composition" && namespace == "default" {
								return []*un.Unstructured{xr1}, nil
							}
							return nil, errors.New("not found")
						}).
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
						WithFindXRsUsingComposition(func(_ context.Context, compositionName, namespace string) ([]*un.Unstructured, error) {
							if compositionName == "test-composition" && namespace == "custom-ns" {
								return []*un.Unstructured{xr2}, nil
							}
							return nil, errors.New("not found")
						}).
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
						WithFindXRsUsingComposition(func(_ context.Context, _, _ string) ([]*un.Unstructured, error) {
							return nil, errors.New("client error")
						}).
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
						WithFindXRsUsingComposition(func(_ context.Context, _, _ string) ([]*un.Unstructured, error) {
							return []*un.Unstructured{}, nil
						}).
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
			processor := &DefaultCompDiffProcessor{
				xpClients: tt.setupMocks(),
				config: ProcessorConfig{
					Logger: tu.TestLogger(t, false),
				},
			}

			got, err := processor.findXRsUsingComposition(ctx, tt.compositionName, tt.namespace)

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

func TestDefaultCompDiffProcessor_unstructuredToComposition(t *testing.T) {
	tests := map[string]struct {
		unstructured *un.Unstructured
		want         *apiextensionsv1.Composition
		wantErr      bool
	}{
		"ValidUnstructured": {
			unstructured: &un.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "apiextensions.crossplane.io/v1",
					"kind":       "Composition",
					"metadata": map[string]interface{}{
						"name": "test-composition",
					},
					"spec": map[string]interface{}{
						"compositeTypeRef": map[string]interface{}{
							"apiVersion": "example.org/v1",
							"kind":       "XResource",
						},
						"mode": "Pipeline",
					},
				},
			},
			want: &apiextensionsv1.Composition{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "apiextensions.crossplane.io/v1",
					Kind:       "Composition",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-composition",
				},
				Spec: apiextensionsv1.CompositionSpec{
					CompositeTypeRef: apiextensionsv1.TypeReference{
						APIVersion: "example.org/v1",
						Kind:       "XResource",
					},
					Mode: apiextensionsv1.CompositionModePipeline,
				},
			},
			wantErr: false,
		},
		"InvalidUnstructured": {
			unstructured: &un.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod", // Wrong kind
				},
			},
			want: &apiextensionsv1.Composition{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Pod",
				},
			},
			wantErr: false, // Runtime converter doesn't validate, it just converts
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Create processor
			processor := &DefaultCompDiffProcessor{
				config: ProcessorConfig{
					Logger: tu.TestLogger(t, false),
				},
			}

			got, err := processor.unstructuredToComposition(tt.unstructured)

			if (err != nil) != tt.wantErr {
				t.Errorf("unstructuredToComposition() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			if diff := gcmp.Diff(tt.want, got); diff != "" {
				t.Errorf("unstructuredToComposition() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDefaultCompDiffProcessor_compositionToUnstructured(t *testing.T) {
	tests := map[string]struct {
		composition *apiextensionsv1.Composition
		wantErr     bool
	}{
		"ValidComposition": {
			composition: &apiextensionsv1.Composition{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "apiextensions.crossplane.io/v1",
					Kind:       "Composition",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-composition",
				},
				Spec: apiextensionsv1.CompositionSpec{
					CompositeTypeRef: apiextensionsv1.TypeReference{
						APIVersion: "example.org/v1",
						Kind:       "XResource",
					},
					Mode: apiextensionsv1.CompositionModePipeline,
				},
			},
			wantErr: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Create processor
			processor := &DefaultCompDiffProcessor{
				config: ProcessorConfig{
					Logger: tu.TestLogger(t, false),
				},
			}

			got, err := processor.compositionToUnstructured(tt.composition)

			if (err != nil) != tt.wantErr {
				t.Errorf("compositionToUnstructured() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			// Basic checks
			if got == nil {
				t.Errorf("compositionToUnstructured() returned nil")
				return
			}

			if got.GetKind() != "Composition" {
				t.Errorf("compositionToUnstructured() kind = %v, want Composition", got.GetKind())
			}

			if got.GetName() != tt.composition.GetName() {
				t.Errorf("compositionToUnstructured() name = %v, want %v", got.GetName(), tt.composition.GetName())
			}
		})
	}
}

func TestDefaultCompDiffProcessor_DiffComposition(t *testing.T) {
	ctx := context.Background()

	// Create test composition
	testComp := &apiextensionsv1.Composition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.crossplane.io/v1",
			Kind:       "Composition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-composition",
		},
		Spec: apiextensionsv1.CompositionSpec{
			CompositeTypeRef: apiextensionsv1.TypeReference{
				APIVersion: "example.org/v1",
				Kind:       "XResource",
			},
			Mode: apiextensionsv1.CompositionModePipeline,
		},
	}

	// Create test XR
	testXR := tu.NewResource("example.org/v1", "XResource", "test-xr").
		WithNamespace("default").
		Build()

	tests := map[string]struct {
		compositions []*un.Unstructured
		namespace    string
		setupMocks   func() (k8.Clients, xp.Clients)
		verifyOutput func(t *testing.T, output string)
		wantErr      bool
	}{
		"SuccessfulDiff": {
			namespace: "default",
			compositions: []*un.Unstructured{
				{
					Object: map[string]interface{}{
						"apiVersion": "apiextensions.crossplane.io/v1",
						"kind":       "Composition",
						"metadata": map[string]interface{}{
							"name": "test-composition",
						},
						"spec": map[string]interface{}{
							"compositeTypeRef": map[string]interface{}{
								"apiVersion": "example.org/v1",
								"kind":       "XResource",
							},
							"mode": "Pipeline",
						},
					},
				},
			},
			setupMocks: func() (k8.Clients, xp.Clients) {
				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().Build(),
					Schema:   tu.NewMockSchemaClient().Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionFetch(testComp).
						WithFindXRsUsingComposition(func(_ context.Context, _, _ string) ([]*un.Unstructured, error) {
							return []*un.Unstructured{testXR}, nil
						}).
						Build(),
					Definition:   tu.NewMockDefinitionClient().Build(),
					Environment:  tu.NewMockEnvironmentClient().Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
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
			setupMocks: func() (k8.Clients, xp.Clients) {
				return k8.Clients{}, xp.Clients{}
			},
			wantErr: true,
		},
		"MultipleCompositions": {
			namespace: "default",
			compositions: []*un.Unstructured{
				{
					Object: map[string]interface{}{
						"apiVersion": "apiextensions.crossplane.io/v1",
						"kind":       "Composition",
						"metadata": map[string]interface{}{
							"name": "test-composition-1",
						},
						"spec": map[string]interface{}{
							"compositeTypeRef": map[string]interface{}{
								"apiVersion": "example.org/v1",
								"kind":       "XResource",
							},
							"mode": "Pipeline",
						},
					},
				},
				{
					Object: map[string]interface{}{
						"apiVersion": "apiextensions.crossplane.io/v1",
						"kind":       "Composition",
						"metadata": map[string]interface{}{
							"name": "test-composition-2",
						},
						"spec": map[string]interface{}{
							"compositeTypeRef": map[string]interface{}{
								"apiVersion": "example.org/v1",
								"kind":       "XResource",
							},
							"mode": "Pipeline",
						},
					},
				},
			},
			setupMocks: func() (k8.Clients, xp.Clients) {
				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().Build(),
					Schema:   tu.NewMockSchemaClient().Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				// Create test compositions for the multi-composition test
				testComp1 := &apiextensionsv1.Composition{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "apiextensions.crossplane.io/v1",
						Kind:       "Composition",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-composition-1",
					},
					Spec: apiextensionsv1.CompositionSpec{
						CompositeTypeRef: apiextensionsv1.TypeReference{
							APIVersion: "example.org/v1",
							Kind:       "XResource",
						},
						Mode: apiextensionsv1.CompositionModePipeline,
					},
				}

				testComp2 := &apiextensionsv1.Composition{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "apiextensions.crossplane.io/v1",
						Kind:       "Composition",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-composition-2",
					},
					Spec: apiextensionsv1.CompositionSpec{
						CompositeTypeRef: apiextensionsv1.TypeReference{
							APIVersion: "example.org/v1",
							Kind:       "XResource",
						},
						Mode: apiextensionsv1.CompositionModePipeline,
					},
				}

				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithGetComposition(func(_ context.Context, name string) (*apiextensionsv1.Composition, error) {
							switch name {
							case "test-composition-1":
								return testComp1, nil
							case "test-composition-2":
								return testComp2, nil
							default:
								return nil, errors.New("composition not found")
							}
						}).
						WithFindXRsUsingComposition(func(_ context.Context, _, _ string) ([]*un.Unstructured, error) {
							// Return no XRs for simplicity - just testing that multiple compositions are processed
							return []*un.Unstructured{}, nil
						}).
						Build(),
					Definition:   tu.NewMockDefinitionClient().Build(),
					Environment:  tu.NewMockEnvironmentClient().Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
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
			k8sClients, xpClients := tt.setupMocks()

			// Create mock XR processor
			mockXRProc := &tu.MockDiffProcessor{
				PerformDiffFn: func(_ context.Context, stdout io.Writer, _ []*un.Unstructured, _ types.CompositionProvider) error {
					_, err := stdout.Write([]byte("Mock XR diff output"))
					return err
				},
			}

			// Create processor
			processor := &DefaultCompDiffProcessor{
				k8sClients: k8sClients,
				xpClients:  xpClients,
				xrProc:     mockXRProc,
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
