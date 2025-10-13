package diffprocessor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	gcmp "github.com/google/go-cmp/cmp"
	"github.com/sergi/go-diff/diffmatchpatch"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
	"github.com/crossplane/crossplane/v2/cmd/crank/render"
	v1 "github.com/crossplane/crossplane/v2/proto/fn/v1"
)

// Test constants to avoid duplication.
const (
	testGroup      = "example.org"
	testKind       = "XR1"
	testPlural     = "xr1s"
	testSingular   = "xr1"
	testCRDName    = testPlural + "." + testGroup
	testXRDName    = testCRDName
	testAPIVersion = "v1"
)

// Ensure MockDiffProcessor implements the DiffProcessor interface.
var _ DiffProcessor = &tu.MockDiffProcessor{}

// testProcessorOptions returns sensible default options for tests.
// Tests can append additional options or override these as needed.
func testProcessorOptions() []ProcessorOption {
	return []ProcessorOption{
		WithNamespace("default"),
		WithColorize(false),
		WithCompact(false),
		WithMaxNestedDepth(10),
		WithLogger(tu.TestLogger(nil, false)),
	}
}

func TestDefaultDiffProcessor_PerformDiff(t *testing.T) {
	// Setup test context
	ctx := t.Context()

	// Create test resources
	resource1 := tu.NewResource("example.org/v1", "XR1", "my-xr-1").
		WithSpecField("coolField", "test-value-1").
		Build()

	resource2 := tu.NewResource("example.org/v1", "XR1", "my-xr-2").
		WithSpecField("coolField", "test-value-2").
		Build()

	// Create a composition for testing
	composition := tu.NewComposition("test-comp").
		WithCompositeTypeRef("example.org/v1", "XR1").
		WithPipelineMode().
		WithPipelineStep("step1", "function-test", nil).
		Build()

	// Create a composed resource for testing
	composedResource := tu.NewResource("cpd.org/v1", "ComposedResource", "resource1").
		WithCompositeOwner("my-xr-1").
		WithCompositionResourceName("resA").
		WithSpecField("param", "value").
		Build()

	// Test cases
	tests := map[string]struct {
		setupMocks      func() (k8.Clients, xp.Clients)
		resources       []*un.Unstructured
		processorOpts   []ProcessorOption
		verifyOutput    func(t *testing.T, output string)
		want            error
		validationError bool
	}{
		"NoResources": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create Kubernetes client mocks
				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().Build(),
					Schema:   tu.NewMockSchemaClient().Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				// Create Crossplane client mocks
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().Build(),
					Definition:  tu.NewMockDefinitionClient().Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			resources:     []*un.Unstructured{},
			processorOpts: testProcessorOptions(),
			want:          nil,
		},
		"DiffSingleResourceError": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create Kubernetes client mocks
				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().Build(),
					Schema:   tu.NewMockSchemaClient().Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				// Create Crossplane client mocks
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithNoMatchingComposition().
						Build(),
					Definition: tu.NewMockDefinitionClient().Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			resources:     []*un.Unstructured{resource1},
			processorOpts: testProcessorOptions(),
			want:          errors.New("unable to process resource XR1/my-xr-1: cannot get composition: composition not found"),
		},
		"MultipleResourceErrors": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create Kubernetes client mocks
				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().Build(),
					Schema:   tu.NewMockSchemaClient().Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				// Create Crossplane client mocks
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithNoMatchingComposition().
						Build(),
					Definition: tu.NewMockDefinitionClient().Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			resources:     []*un.Unstructured{resource1, resource2},
			processorOpts: testProcessorOptions(),
			want: errors.New("[unable to process resource XR1/my-xr-1: cannot get composition: composition not found, " +
				"unable to process resource XR1/my-xr-2: cannot get composition: composition not found]"),
		},
		"CompositionNotFound": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create Kubernetes client mocks
				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().Build(),
					Schema:   tu.NewMockSchemaClient().Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				// Create Crossplane client mocks
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithNoMatchingComposition().
						Build(),
					Definition: tu.NewMockDefinitionClient().Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			resources:     []*un.Unstructured{resource1},
			processorOpts: testProcessorOptions(),
			want:          errors.New("unable to process resource XR1/my-xr-1: cannot get composition: composition not found"),
		},
		"GetFunctionsError": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create Kubernetes client mocks
				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().Build(),
					Schema:   tu.NewMockSchemaClient().Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				// Create Crossplane client mocks
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionMatch(composition).
						Build(),
					Definition: tu.NewMockDefinitionClient().Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
						Build(),
					Function: tu.NewMockFunctionClient().
						WithFailedFunctionsFetch("function not found").
						Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			resources:     []*un.Unstructured{resource1},
			processorOpts: testProcessorOptions(),
			want:          errors.New("unable to process resource XR1/my-xr-1: cannot get functions from pipeline: function not found"),
		},
		"SuccessfulDiff": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create mock functions that render will call successfully
				functions := []pkgv1.Function{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "function-test",
						},
					},
				}

				// Create CRDs upfront to avoid recreating them in closures
				mainCRD := makeTestCRD(testCRDName, testKind, testGroup, testAPIVersion)
				composedCRD := makeTestCRD("composedresources.cpd.org", "ComposedResource", "cpd.org", "v1")

				// Create Kubernetes client mocks
				k8sClients := k8.Clients{
					Apply: tu.NewMockApplyClient().
						WithSuccessfulDryRun().
						Build(),
					Resource: tu.NewMockResourceClient().
						WithResourcesExist(resource1, composedResource). // Add resources to existing resources
						WithResourcesFoundByLabel([]*un.Unstructured{composedResource}, "crossplane.io/composite", "test-xr").
						Build(),
					Schema: tu.NewMockSchemaClient().
						WithNoResourcesRequiringCRDs().
						WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
							if gvk.Group == testGroup && gvk.Kind == testKind {
								return mainCRD, nil
							}

							if gvk.Group == "cpd.org" && gvk.Kind == "ComposedResource" {
								return composedCRD, nil
							}

							return nil, errors.New("CRD not found")
						}).
						WithGetCRDByName(func(name string) (*extv1.CustomResourceDefinition, error) {
							if name == testCRDName {
								return mainCRD, nil
							}

							if name == "composedresources.cpd.org" {
								return composedCRD, nil
							}

							return nil, errors.Errorf("CRD with name %s not found", name)
						}).
						Build(),
					Type: tu.NewMockTypeConverter().Build(),
				}

				// Create XRD for composed resource
				composedXRD := tu.NewXRD("composedresources.cpd.org", "cpd.org", "ComposedResource").
					WithPlural("composedresources").
					WithSingular("composedresource").
					WithVersion("v1", true, true).
					WithSchema(&extv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]extv1.JSONSchemaProps{
									"param": {Type: "string"},
								},
							},
							"status": {Type: "object"},
						},
					}).
					BuildAsUnstructured()

				// Create main XRD
				mainXRD := tu.NewXRD(testXRDName, testGroup, testKind).
					WithPlural(testPlural).
					WithSingular(testSingular).
					WithVersion("v1", true, true).
					WithSchema(&extv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]extv1.JSONSchemaProps{
									"field": {Type: "string"},
								},
							},
							"status": {Type: "object"},
						},
					}).
					BuildAsUnstructured()

				// Create Crossplane client mocks
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionMatch(composition).
						Build(),
					Definition: tu.NewMockDefinitionClient().
						WithSuccessfulXRDsFetch([]*un.Unstructured{}).
						WithGetXRDForXR(func(_ context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
							// Return the appropriate XRD based on the GVK
							if gvk.Group == testGroup && gvk.Kind == testKind {
								return mainXRD, nil
							}

							if gvk.Group == "cpd.org" && gvk.Kind == "ComposedResource" {
								return composedXRD, nil
							}

							return nil, errors.Errorf("no XRD found that defines XR type %s", gvk.String())
						}).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
						Build(),
					Function: tu.NewMockFunctionClient().
						WithSuccessfulFunctionsFetch(functions).
						Build(),
					ResourceTree: tu.NewMockResourceTreeClient().
						WithEmptyResourceTree().
						Build(),
				}

				return k8sClients, xpClients
			},
			resources: []*un.Unstructured{resource1},
			processorOpts: append(testProcessorOptions(),
				WithLogger(tu.TestLogger(t, false)),
				WithRenderFunc(func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
					// Only return composed resources for the main XR, not for nested XRs
					// to avoid infinite recursion
					if in.CompositeResource.GetKind() == testKind {
						return render.Outputs{
							CompositeResource: in.CompositeResource,
							ComposedResources: []cpd.Unstructured{
								{
									Unstructured: un.Unstructured{
										Object: composedResource.Object,
									},
								},
							},
						}, nil
					}
					// For nested XRs, just return the XR itself with no composed resources
					return render.Outputs{
						CompositeResource: in.CompositeResource,
					}, nil
				}),
				// Override the schema validator factory to use a simple validator
				WithSchemaValidatorFactory(func(k8.SchemaClient, xp.DefinitionClient, logging.Logger) SchemaValidator {
					return &tu.MockSchemaValidator{
						ValidateResourcesFn: func(context.Context, *un.Unstructured, []cpd.Unstructured) error {
							return nil
						},
					}
				}),
				// Override the diff calculator factory to return actual diffs
				WithDiffCalculatorFactory(func(k8.ApplyClient, xp.ResourceTreeClient, ResourceManager, logging.Logger, renderer.DiffOptions) DiffCalculator {
					return &tu.MockDiffCalculator{
						CalculateDiffsFn: func(context.Context, *cmp.Unstructured, render.Outputs) (map[string]*dt.ResourceDiff, error) {
							diffs := make(map[string]*dt.ResourceDiff)

							// Add a modified diff (not just equal)
							lineDiffs := []diffmatchpatch.Diff{
								{Type: diffmatchpatch.DiffDelete, Text: "  field: old-value"},
								{Type: diffmatchpatch.DiffInsert, Text: "  field: new-value"},
							}

							diffs["example.org/v1/XR1/test-xr"] = &dt.ResourceDiff{
								Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "XR1"},
								ResourceName: "test-xr",
								DiffType:     dt.DiffTypeModified,
								LineDiffs:    lineDiffs, // Add line diffs
								Current:      resource1, // Set current for completeness
								Desired:      resource1, // Set desired for completeness
							}

							// Add a composed resource diff that's also modified
							diffs["example.org/v1/ComposedResource/resource-a"] = &dt.ResourceDiff{
								Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "ComposedResource"},
								ResourceName: "resource-a",
								DiffType:     dt.DiffTypeModified,
								LineDiffs:    lineDiffs,
								Current:      composedResource,
								Desired:      composedResource,
							}

							return diffs, nil
						},
					}
				}),
				// Override the diff renderer factory to produce actual output
				WithDiffRendererFactory(func(logging.Logger, renderer.DiffOptions) renderer.DiffRenderer {
					return &tu.MockDiffRenderer{
						RenderDiffsFn: func(w io.Writer, _ map[string]*dt.ResourceDiff) error {
							// Write a simple summary to the output
							_, err := fmt.Fprintln(w, "Changes will be applied to 2 resources:")
							if err != nil {
								return err
							}

							_, err = fmt.Fprintln(w, "- example.org/v1/XR1/test-xr will be modified")
							if err != nil {
								return err
							}

							_, err = fmt.Fprintln(w, "- example.org/v1/ComposedResource/resource-a will be modified")
							if err != nil {
								return err
							}

							_, err = fmt.Fprintln(w, "\nSummary: 0 to create, 2 to modify, 0 to delete")

							return err
						},
					}
				}),
			),
			verifyOutput: func(t *testing.T, output string) {
				t.Helper()
				// We should have some output from the diff
				if output == "" {
					t.Errorf("Expected non-empty diff output")
				}

				// Simple check for expected output format
				if !strings.Contains(output, "Summary:") {
					t.Errorf("Expected diff output to contain a Summary section")
				}
			},
			want: nil,
		},
		"ValidationError": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create mock functions that render will call successfully
				functions := []pkgv1.Function{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "function-test",
						},
					},
				}

				// Create Kubernetes client mocks
				k8sClients := k8.Clients{
					Apply: tu.NewMockApplyClient().
						WithSuccessfulDryRun().
						Build(),
					Resource: tu.NewMockResourceClient().
						WithResourcesExist(resource1).
						Build(),
					Schema: tu.NewMockSchemaClient().
						WithNoResourcesRequiringCRDs().
						WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
							if gvk.Group == testGroup && gvk.Kind == testKind {
								return makeTestCRD(testCRDName, testKind, testGroup, testAPIVersion), nil
							}

							if gvk.Group == "cpd.org" && gvk.Kind == "ComposedResource" {
								return makeTestCRD("composedresources.cpd.org", "ComposedResource", "cpd.org", "v1"), nil
							}

							return nil, errors.New("CRD not found")
						}).
						WithSuccessfulCRDByNameFetch(testCRDName, makeTestCRD(testCRDName, testKind, testGroup, testAPIVersion)).
						Build(),
					Type: tu.NewMockTypeConverter().Build(),
				}

				// Create Crossplane client mocks
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionMatch(composition).
						Build(),
					Definition: tu.NewMockDefinitionClient().
						WithXRDForXR(tu.NewXRD(testXRDName, testGroup, testKind).
							WithPlural(testPlural).
							WithSingular(testSingular).
							BuildAsUnstructured()).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
						Build(),
					Function: tu.NewMockFunctionClient().
						WithSuccessfulFunctionsFetch(functions).
						Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			resources: []*un.Unstructured{resource1},
			processorOpts: append(testProcessorOptions(),
				WithLogger(tu.TestLogger(t, false)),
				WithRenderFunc(func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
					// Return valid render outputs
					return render.Outputs{
						CompositeResource: in.CompositeResource,
						ComposedResources: []cpd.Unstructured{
							{
								Unstructured: un.Unstructured{
									Object: composedResource.Object,
								},
							},
						},
					}, nil
				}),
				// Override with a validator that fails
				WithSchemaValidatorFactory(func(_ k8.SchemaClient, _ xp.DefinitionClient, _ logging.Logger) SchemaValidator {
					return &tu.MockSchemaValidator{
						ValidateResourcesFn: func(context.Context, *un.Unstructured, []cpd.Unstructured) error {
							return errors.New("validation error")
						},
					}
				}),
			),
			want:            errors.New("unable to process resource XR1/my-xr-1: cannot validate resources: validation error"),
			validationError: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Create components for testing
			k8sClients, xpClients := tt.setupMocks()

			// Create the diff processor
			processor := NewDiffProcessor(k8sClients, xpClients, tt.processorOpts...)

			// Create a dummy writer for stdout
			var stdout bytes.Buffer

			// Create a mock composition provider that uses the same mock composition client
			compositionProvider := func(ctx context.Context, res *un.Unstructured) (*apiextensionsv1.Composition, error) {
				return xpClients.Composition.FindMatchingComposition(ctx, res)
			}
			err := processor.PerformDiff(ctx, &stdout, tt.resources, compositionProvider)

			if tt.want != nil {
				if err == nil {
					t.Errorf("PerformDiff(...): expected error but got none")
					return
				}

				if diff := gcmp.Diff(tt.want.Error(), err.Error()); diff != "" {
					t.Errorf("PerformDiff(...): -want error, +got error:\n%s", diff)
				}

				return
			}

			if err != nil {
				t.Errorf("PerformDiff(...): unexpected error: %v", err)
			}

			// Check output if verification function is provided
			if tt.verifyOutput != nil {
				tt.verifyOutput(t, stdout.String())
			}
		})
	}
}

func TestDefaultDiffProcessor_Initialize(t *testing.T) {
	// Setup test context
	ctx := t.Context()

	// Create test resources
	xrd1 := tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "xrd1").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]interface{}{
			"kind":     "XExampleResource",
			"plural":   "xexampleresources",
			"singular": "xexampleresource",
		}).
		Build()

	// Test cases
	tests := map[string]struct {
		setupMocks    func() (k8.Clients, xp.Clients)
		processorOpts []ProcessorOption
		want          error
	}{
		"XRDsError": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create Kubernetes client mocks
				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().Build(),
					Schema:   tu.NewMockSchemaClient().Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				// Create Crossplane client mocks with a failing Definition client
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().Build(),
					Definition: tu.NewMockDefinitionClient().
						WithFailedXRDsFetch("XRD not found").
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			processorOpts: testProcessorOptions(),
			want:          errors.Wrap(errors.Wrap(errors.New("XRD not found"), "cannot get XRDs"), "cannot load CRDs"),
		},
		"EnvConfigsError": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create Kubernetes client mocks
				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().Build(),
					Schema:   tu.NewMockSchemaClient().Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				// Create Crossplane client mocks with a failing Environment client
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().Build(),
					Definition: tu.NewMockDefinitionClient().
						WithSuccessfulXRDsFetch([]*un.Unstructured{}).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithGetEnvironmentConfigs(func(_ context.Context) ([]*un.Unstructured, error) {
							return nil, errors.New("env configs not found")
						}).
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			processorOpts: testProcessorOptions(),
			want:          errors.Wrap(errors.New("env configs not found"), "cannot get environment configs"),
		},
		"Success": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create Kubernetes client mocks
				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().Build(),
					Schema:   tu.NewMockSchemaClient().Build(),
					Type: tu.NewMockTypeConverter().
						WithDefaultGVKToGVR().
						Build(),
				}

				// Create Crossplane client mocks with successful initialization
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().Build(),
					Definition: tu.NewMockDefinitionClient().
						WithSuccessfulXRDsFetch([]*un.Unstructured{xrd1}).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			processorOpts: testProcessorOptions(),
			want:          nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// Get the clients for this test
			k8sClients, xpClients := tc.setupMocks()

			// Build processor options
			options := tc.processorOpts

			// Create the processor
			processor := NewDiffProcessor(k8sClients, xpClients, options...)

			// Call the Initialize method
			err := processor.Initialize(ctx)

			// Verify error expectations
			if tc.want != nil {
				if err == nil {
					t.Errorf("Initialize(...): expected error but got none")
					return
				}

				if diff := gcmp.Diff(tc.want.Error(), err.Error()); diff != "" {
					t.Errorf("Initialize(...): -want error, +got error:\n%s", diff)
				}

				return
			}

			if err != nil {
				t.Errorf("Initialize(...): unexpected error: %v", err)
			}
		})
	}
}

func TestDefaultDiffProcessor_RenderWithRequirements(t *testing.T) {
	ctx := t.Context()

	// Create test resources
	xr := tu.NewResource("example.org/v1", "XR", "test-xr").BuildUComposite()

	// Create a composition with pipeline mode
	pipelineMode := apiextensionsv1.CompositionModePipeline
	composition := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-composition",
		},
		Spec: apiextensionsv1.CompositionSpec{
			Mode: pipelineMode,
		},
	}

	// Create test functions
	functions := []pkgv1.Function{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-function",
			},
		},
	}

	// Create test resources for requirements
	const (
		ConfigMap     = "ConfigMap"
		ConfigMapName = "config1"
	)

	configMap := tu.NewResource("v1", ConfigMap, ConfigMapName).Build()
	secret := tu.NewResource("v1", "Secret", "secret1").Build()

	tests := map[string]struct {
		xr                     *cmp.Unstructured
		composition            *apiextensionsv1.Composition
		functions              []pkgv1.Function
		resourceID             string
		setupResourceClient    func() *tu.MockResourceClient
		setupEnvironmentClient func() *tu.MockEnvironmentClient
		setupRenderFunc        func() RenderFunc
		wantComposedCount      int
		wantRenderIterations   int
		wantErr                bool
	}{
		"NoRequirements": {
			xr:          xr,
			composition: composition,
			functions:   functions,
			resourceID:  "XR/test-xr",
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
					Build()
			},
			setupRenderFunc: func() RenderFunc {
				iteration := 0

				return func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
					iteration++
					// Return a simple output with no requirements
					return render.Outputs{
						CompositeResource: in.CompositeResource,
						ComposedResources: []cpd.Unstructured{
							{Unstructured: un.Unstructured{Object: map[string]interface{}{
								"apiVersion": "example.org/v1",
								"kind":       "ComposedResource",
								"metadata": map[string]interface{}{
									"name": "composed1",
								},
							}}},
						},
						Requirements: map[string]v1.Requirements{},
					}, nil
				}
			},
			wantComposedCount:    1,
			wantRenderIterations: 1, // Only renders once when no requirements
			wantErr:              false,
		},
		"SingleIterationWithRequirements": {
			xr:          xr,
			composition: composition,
			functions:   functions,
			resourceID:  "XR/test-xr",
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
					).
					WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
						if gvk.Kind == ConfigMap && name == ConfigMapName {
							return configMap, nil
						}

						return nil, errors.New("resource not found")
					}).
					Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
					Build()
			},
			setupRenderFunc: func() RenderFunc {
				iteration := 0

				return func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
					iteration++

					// First render includes requirements, second should have no requirements
					var reqs map[string]v1.Requirements
					if iteration == 1 {
						reqs = map[string]v1.Requirements{
							"step1": {
								Resources: map[string]*v1.ResourceSelector{
									"config": {
										ApiVersion: "v1",
										Kind:       ConfigMap,
										Match: &v1.ResourceSelector_MatchName{
											MatchName: ConfigMapName,
										},
									},
								},
							},
						}
					} else {
						reqs = map[string]v1.Requirements{}
					}

					// Return a simple output
					return render.Outputs{
						CompositeResource: in.CompositeResource,
						ComposedResources: []cpd.Unstructured{
							{Unstructured: un.Unstructured{Object: map[string]interface{}{
								"apiVersion": "example.org/v1",
								"kind":       "ComposedResource",
								"metadata": map[string]interface{}{
									"name": "composed1",
								},
							}}},
						},
						Requirements: reqs,
					}, nil
				}
			},
			wantComposedCount:    1,
			wantRenderIterations: 2, // Renders once with requirements, then once more to confirm no new requirements
			wantErr:              false,
		},
		"MultipleIterationsWithRequirements": {
			xr:          xr,
			composition: composition,
			functions:   functions,
			resourceID:  "XR/test-xr",
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"},
					).
					WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
						if gvk.Kind == ConfigMap && name == ConfigMapName {
							return configMap, nil
						}

						if gvk.Kind == "Secret" && name == "secret1" {
							return secret, nil
						}

						return nil, errors.New("resource not found")
					}).
					Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
					Build()
			},
			setupRenderFunc: func() RenderFunc {
				iteration := 0

				return func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
					iteration++

					// Track existing resources to simulate dependencies
					hasConfig := false
					hasSecret := false

					for _, res := range in.RequiredResources {
						if res.GetKind() == ConfigMap && res.GetName() == ConfigMapName {
							hasConfig = true
						}

						if res.GetKind() == "Secret" && res.GetName() == "secret1" {
							hasSecret = true
						}
					}

					// Build requirements based on what we already have
					var reqs map[string]*v1.ResourceSelector

					if !hasConfig {
						// First iteration - request ConfigMap
						reqs = map[string]*v1.ResourceSelector{
							"config": {
								ApiVersion: "v1",
								Kind:       ConfigMap,
								Match: &v1.ResourceSelector_MatchName{
									MatchName: ConfigMapName,
								},
							},
						}
					} else if !hasSecret {
						// Second iteration - request Secret
						reqs = map[string]*v1.ResourceSelector{
							"secret": {
								ApiVersion: "v1",
								Kind:       "Secret",
								Match: &v1.ResourceSelector_MatchName{
									MatchName: "secret1",
								},
							},
						}
					}

					requirements := map[string]v1.Requirements{}
					if len(reqs) > 0 {
						requirements["step1"] = v1.Requirements{
							Resources: reqs,
						}
					}

					// Return a simple output
					return render.Outputs{
						CompositeResource: in.CompositeResource,
						ComposedResources: []cpd.Unstructured{
							{Unstructured: un.Unstructured{Object: map[string]interface{}{
								"apiVersion": "example.org/v1",
								"kind":       "ComposedResource",
								"metadata": map[string]interface{}{
									"name": "composed1",
								},
							}}},
						},
						Requirements: requirements,
					}, nil
				}
			},
			wantComposedCount:    1,
			wantRenderIterations: 3, // Iterations: 1. Request ConfigMap, 2. Request Secret, 3. No more requirements
			wantErr:              false,
		},
		"RenderError": {
			xr:          xr,
			composition: composition,
			functions:   functions,
			resourceID:  "XR/test-xr",
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
					Build()
			},
			setupRenderFunc: func() RenderFunc {
				return func(context.Context, logging.Logger, render.Inputs) (render.Outputs, error) {
					return render.Outputs{}, errors.New("render error")
				}
			},
			wantComposedCount:    0,
			wantRenderIterations: 1,
			wantErr:              true,
		},
		"RenderErrorWithRequirements": {
			xr:          xr,
			composition: composition,
			functions:   functions,
			resourceID:  "XR/test-xr",
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
					).
					WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
						if gvk.Kind == ConfigMap && name == ConfigMapName {
							return configMap, nil
						}

						return nil, errors.New("resource not found")
					}).
					Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
					Build()
			},
			setupRenderFunc: func() RenderFunc {
				iteration := 0

				return func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
					iteration++

					// First render has requirements but errors
					if iteration == 1 {
						reqs := map[string]v1.Requirements{
							"step1": {
								Resources: map[string]*v1.ResourceSelector{
									"config": {
										ApiVersion: "v1",
										Kind:       ConfigMap,
										Match: &v1.ResourceSelector_MatchName{
											MatchName: ConfigMapName,
										},
									},
								},
							},
						}

						return render.Outputs{
							Requirements: reqs,
						}, errors.New("render error with requirements")
					}

					// Second render succeeds
					return render.Outputs{
						CompositeResource: in.CompositeResource,
						ComposedResources: []cpd.Unstructured{
							{Unstructured: un.Unstructured{Object: map[string]interface{}{
								"apiVersion": "example.org/v1",
								"kind":       "ComposedResource",
								"metadata": map[string]interface{}{
									"name": "composed1",
								},
							}}},
						},
					}, nil
				}
			},
			wantComposedCount:    1,
			wantRenderIterations: 2,     // Renders once with error but requirements, then once more successfully
			wantErr:              false, // Should not error as the second render succeeds
		},
		"RequirementsProcessingError": {
			xr:          xr,
			composition: composition,
			functions:   functions,
			resourceID:  "XR/test-xr",
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
					).
					WithResourceNotFound().
					Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
					Build()
			},
			setupRenderFunc: func() RenderFunc {
				return func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
					reqs := map[string]v1.Requirements{
						"step1": {
							Resources: map[string]*v1.ResourceSelector{
								"config": {
									ApiVersion: "v1",
									Kind:       ConfigMap,
									Match: &v1.ResourceSelector_MatchName{
										MatchName: "missing-config",
									},
								},
							},
						},
					}

					return render.Outputs{
						CompositeResource: in.CompositeResource,
						Requirements:      reqs,
					}, nil
				}
			},
			wantComposedCount:    0,
			wantRenderIterations: 1,
			wantErr:              true, // Should error because requirements processing fails
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Set up mock clients
			resourceClient := tt.setupResourceClient()
			environmentClient := tt.setupEnvironmentClient()

			// Create a logger
			logger := tu.TestLogger(t, false)
			renderFunc := tt.setupRenderFunc()

			// Create a render iteration counter to verify
			renderCount := 0
			countingRenderFunc := func(ctx context.Context, log logging.Logger, in render.Inputs) (render.Outputs, error) {
				renderCount++
				return renderFunc(ctx, log, in)
			}

			// Create the requirements provider
			requirementsProvider := NewRequirementsProvider(
				resourceClient,
				environmentClient,
				countingRenderFunc,
				logger,
			)

			// Build processor options
			baseOpts := testProcessorOptions()
			customOpts := []ProcessorOption{
				WithLogger(logger),
				WithRenderFunc(countingRenderFunc),
				WithRequirementsProviderFactory(func(k8.ResourceClient, xp.EnvironmentClient, RenderFunc, logging.Logger) *RequirementsProvider {
					return requirementsProvider
				}),
			}
			baseOpts = append(baseOpts, customOpts...)
			processor := NewDiffProcessor(k8.Clients{}, xp.Clients{}, baseOpts...)

			// Call the method under test
			output, err := processor.(*DefaultDiffProcessor).RenderWithRequirements(ctx, tt.xr, tt.composition, tt.functions, tt.resourceID)

			// Check error expectations
			if tt.wantErr {
				if err == nil {
					t.Errorf("RenderWithRequirements() expected error but got none")
				}

				return
			}

			if err != nil {
				t.Errorf("RenderWithRequirements() unexpected error: %v", err)
				return
			}

			// Check render iterations
			if renderCount != tt.wantRenderIterations {
				t.Errorf("RenderWithRequirements() called render func %d times, want %d",
					renderCount, tt.wantRenderIterations)
			}

			// Check composed resource count
			if len(output.ComposedResources) != tt.wantComposedCount {
				t.Errorf("RenderWithRequirements() returned %d composed resources, want %d",
					len(output.ComposedResources), tt.wantComposedCount)
			}
		})
	}
}

// Helper function to create a test CRD for the given GVK.
func makeTestCRD(name string, kind string, group string, version string) *extv1.CustomResourceDefinition {
	return tu.NewCRD(name, group, kind).
		WithListKind(kind+"List").
		WithPlural(strings.ToLower(kind)+"s").
		WithSingular(strings.ToLower(kind)).
		WithVersion(version, true, true).
		WithStandardSchema("coolField").
		Build()
}

func TestDefaultDiffProcessor_isCompositeResource(t *testing.T) {
	ctx := t.Context()

	// Create test XRD for parent resources
	parentXRD := tu.NewXRD("xparentresources.nested.example.org", "nested.example.org", "XParentResource").
		WithVersion("v1alpha1", true, true).
		WithSchema(&extv1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]extv1.JSONSchemaProps{
				"spec": {
					Type: "object",
					Properties: map[string]extv1.JSONSchemaProps{
						"parentField": {Type: "string"},
					},
				},
				"status": {Type: "object"},
			},
		}).
		Build()

	// Create test XRD for child resources
	childXRD := tu.NewXRD("xchildresources.nested.example.org", "nested.example.org", "XChildResource").
		WithVersion("v1alpha1", true, true).
		WithSchema(&extv1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]extv1.JSONSchemaProps{
				"spec": {
					Type: "object",
					Properties: map[string]extv1.JSONSchemaProps{
						"childField": {Type: "string"},
					},
				},
				"status": {Type: "object"},
			},
		}).
		Build()

	tests := map[string]struct {
		defClient   xp.DefinitionClient
		resource    *un.Unstructured
		wantIsXR    bool
		wantXRDName string
	}{
		"Managed resource is not an XR": {
			defClient: tu.NewMockDefinitionClient().
				WithXRDForXRNotFound().
				Build(),
			resource: tu.NewResource("nop.example.org/v1alpha1", "NopResource", "test-managed").
				WithSpecField("forProvider", map[string]interface{}{
					"configData": "test-value",
				}).
				Build(),
			wantIsXR:    false,
			wantXRDName: "",
		},
		"Parent XR is correctly identified": {
			defClient: tu.NewMockDefinitionClient().
				WithXRD(parentXRD).
				Build(),
			resource: tu.NewResource("nested.example.org/v1alpha1", "XParentResource", "test-parent").
				WithSpecField("parentField", "parent-value").
				Build(),
			wantIsXR:    true,
			wantXRDName: "xparentresources.nested.example.org",
		},
		"Child XR is correctly identified": {
			defClient: tu.NewMockDefinitionClient().
				WithXRD(childXRD).
				Build(),
			resource: tu.NewResource("nested.example.org/v1alpha1", "XChildResource", "test-child").
				WithSpecField("childField", "child-value").
				Build(),
			wantIsXR:    true,
			wantXRDName: "xchildresources.nested.example.org",
		},
		"Error from definition client is handled": {
			defClient: tu.NewMockDefinitionClient().
				WithXRDForXRError(errors.New("cluster connection error")).
				Build(),
			resource: tu.NewResource("nested.example.org/v1alpha1", "XParentResource", "test-parent").
				WithSpecField("parentField", "parent-value").
				Build(),
			wantIsXR:    false,
			wantXRDName: "",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Create processor with mocked definition client
			defClient := tt.defClient

			processor := &DefaultDiffProcessor{
				defClient: defClient,
				config: ProcessorConfig{
					Logger: tu.TestLogger(t, false),
				},
			}

			// Call the method under test
			isXR, xrd := processor.isCompositeResource(ctx, tt.resource)

			// Check isXR result
			if diff := gcmp.Diff(tt.wantIsXR, isXR); diff != "" {
				t.Errorf("isCompositeResource() isXR mismatch (-want +got):\n%s", diff)
			}

			// Check XRD result
			var gotXRDName string
			if xrd != nil {
				gotXRDName = xrd.GetName()
			}

			if diff := gcmp.Diff(tt.wantXRDName, gotXRDName); diff != "" {
				t.Errorf("isCompositeResource() XRD name mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDefaultDiffProcessor_ProcessNestedXRs(t *testing.T) {
	ctx := t.Context()

	// Create test resources
	childXR := tu.NewResource("nested.example.org/v1alpha1", "XChildResource", "test-parent-child").
		WithSpecField("childField", "parent-value").
		WithCompositionResourceName("child-xr").
		Build()

	managedResource := tu.NewResource("nop.example.org/v1alpha1", "NopResource", "test-managed").
		WithSpecField("forProvider", map[string]interface{}{
			"configData": "test-value",
		}).
		WithCompositionResourceName("managed-resource").
		Build()

	childXRD := tu.NewXRD("xchildresources.nested.example.org", "nested.example.org", "XChildResource").
		WithVersion("v1alpha1", true, true).
		WithSchema(&extv1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]extv1.JSONSchemaProps{
				"spec": {
					Type: "object",
					Properties: map[string]extv1.JSONSchemaProps{
						"childField": {Type: "string"},
					},
				},
				"status": {Type: "object"},
			},
		}).
		Build()

	childComposition := tu.NewComposition("child-composition").
		WithCompositeTypeRef("nested.example.org/v1alpha1", "XChildResource").
		WithPipelineMode().
		WithPipelineStep("generate-managed", "function-go-templating", map[string]interface{}{
			"apiVersion": "template.fn.crossplane.io/v1beta1",
			"kind":       "GoTemplate",
			"source":     "Inline",
			"inline": map[string]interface{}{
				"template": "apiVersion: nop.example.org/v1alpha1\nkind: NopResource\nmetadata:\n  name: test\n  annotations:\n    gotemplating.fn.crossplane.io/composition-resource-name: managed-resource\nspec:\n  forProvider:\n    configData: test",
			},
		}).
		Build()

	tests := map[string]struct {
		setupMocks        func() (xp.Clients, k8.Clients)
		composedResources []cpd.Unstructured
		parentResourceID  string
		depth             int
		wantDiffCount     int
		wantErr           bool
		wantErrContain    string
	}{
		"No composed resources returns empty": {
			setupMocks: func() (xp.Clients, k8.Clients) {
				xpClients := xp.Clients{
					Definition: tu.NewMockDefinitionClient().Build(),
				}
				k8sClients := k8.Clients{}

				return xpClients, k8sClients
			},
			composedResources: []cpd.Unstructured{},
			parentResourceID:  "XParentResource/test-parent",
			depth:             1,
			wantDiffCount:     0,
			wantErr:           false,
		},
		"Only managed resources returns empty": {
			setupMocks: func() (xp.Clients, k8.Clients) {
				xpClients := xp.Clients{
					Definition: tu.NewMockDefinitionClient().
						WithXRDForXRNotFound().
						Build(),
				}
				k8sClients := k8.Clients{}

				return xpClients, k8sClients
			},
			composedResources: []cpd.Unstructured{
				{Unstructured: *managedResource},
			},
			parentResourceID: "XParentResource/test-parent",
			depth:            1,
			wantDiffCount:    0,
			wantErr:          false,
		},
		"Child XR is processed recursively": {
			setupMocks: func() (xp.Clients, k8.Clients) {
				// Create functions that the composition references
				functions := []pkgv1.Function{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "function-go-templating",
						},
						Spec: pkgv1.FunctionSpec{
							PackageSpec: pkgv1.PackageSpec{
								Package: "xpkg.upbound.io/crossplane-contrib/function-go-templating:v0.11.0",
							},
						},
					},
				}

				xpClients := xp.Clients{
					Definition: tu.NewMockDefinitionClient().
						WithXRD(childXRD).
						Build(),
					Composition: tu.NewMockCompositionClient().
						WithComposition(childComposition).
						Build(),
					Function: tu.NewMockFunctionClient().
						WithSuccessfulFunctionsFetch(functions).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
						Build(),
				}

				// Create a child CRD with proper schema for childField
				childCRD := tu.NewCRD("xchildresources.nested.example.org", "nested.example.org", "XChildResource").
					WithListKind("XChildResourceList").
					WithPlural("xchildresources").
					WithSingular("xchildresource").
					WithVersion("v1alpha1", true, true).
					WithStandardSchema("childField").
					Build()

				// Create a CRD for the managed NopResource that the composition creates
				nopCRD := tu.NewCRD("nopresources.nop.example.org", "nop.example.org", "NopResource").
					WithListKind("NopResourceList").
					WithPlural("nopresources").
					WithSingular("nopresource").
					WithVersion("v1alpha1", true, true).
					WithStandardSchema("configData").
					Build()

				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().Build(),
					Schema: tu.NewMockSchemaClient().
						WithFoundCRD("nested.example.org", "XChildResource", childCRD).
						WithFoundCRD("nop.example.org", "NopResource", nopCRD).
						WithSuccessfulCRDByNameFetch("xchildresources.nested.example.org", childCRD).
						Build(),
					Type: tu.NewMockTypeConverter().Build(),
				}

				return xpClients, k8sClients
			},
			composedResources: []cpd.Unstructured{
				{Unstructured: *childXR},
			},
			parentResourceID: "XParentResource/test-parent",
			depth:            1,
			wantDiffCount:    1, // Should have diff for the child XR itself
			wantErr:          false,
		},
		"Max depth exceeded returns error": {
			setupMocks: func() (xp.Clients, k8.Clients) {
				xpClients := xp.Clients{
					Definition: tu.NewMockDefinitionClient().
						WithXRD(childXRD).
						Build(),
				}
				k8sClients := k8.Clients{}

				return xpClients, k8sClients
			},
			composedResources: []cpd.Unstructured{
				{Unstructured: *childXR},
			},
			parentResourceID: "XParentResource/test-parent",
			depth:            11, // Exceeds default maxDepth of 10
			wantDiffCount:    0,
			wantErr:          true,
			wantErrContain:   "maximum nesting depth exceeded",
		},
		"Mixed XR and managed resources processes only XRs": {
			setupMocks: func() (xp.Clients, k8.Clients) {
				// Create functions that the composition references
				functions := []pkgv1.Function{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "function-go-templating",
						},
						Spec: pkgv1.FunctionSpec{
							PackageSpec: pkgv1.PackageSpec{
								Package: "xpkg.upbound.io/crossplane-contrib/function-go-templating:v0.11.0",
							},
						},
					},
				}

				xpClients := xp.Clients{
					Definition: tu.NewMockDefinitionClient().
						WithXRD(childXRD).
						WithXRDForXRNotFoundForGVK(schema.GroupVersionKind{
							Group:   "nop.example.org",
							Version: "v1alpha1",
							Kind:    "NopResource",
						}).
						Build(),
					Composition: tu.NewMockCompositionClient().
						WithComposition(childComposition).
						Build(),
					Function: tu.NewMockFunctionClient().
						WithSuccessfulFunctionsFetch(functions).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{}).
						Build(),
				}

				// Create a child CRD with proper schema for childField
				childCRD := tu.NewCRD("xchildresources.nested.example.org", "nested.example.org", "XChildResource").
					WithListKind("XChildResourceList").
					WithPlural("xchildresources").
					WithSingular("xchildresource").
					WithVersion("v1alpha1", true, true).
					WithStandardSchema("childField").
					Build()

				// Create a CRD for the managed NopResource that the composition creates
				nopCRD := tu.NewCRD("nopresources.nop.example.org", "nop.example.org", "NopResource").
					WithListKind("NopResourceList").
					WithPlural("nopresources").
					WithSingular("nopresource").
					WithVersion("v1alpha1", true, true).
					WithStandardSchema("configData").
					Build()

				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().Build(),
					Schema: tu.NewMockSchemaClient().
						WithFoundCRD("nested.example.org", "XChildResource", childCRD).
						WithFoundCRD("nop.example.org", "NopResource", nopCRD).
						WithSuccessfulCRDByNameFetch("xchildresources.nested.example.org", childCRD).
						Build(),
					Type: tu.NewMockTypeConverter().Build(),
				}

				return xpClients, k8sClients
			},
			composedResources: []cpd.Unstructured{
				{Unstructured: *childXR},
				{Unstructured: *managedResource},
			},
			parentResourceID: "XParentResource/test-parent",
			depth:            1,
			wantDiffCount:    1, // Only the child XR should be processed
			wantErr:          false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Setup mocks
			xpClients, k8sClients := tt.setupMocks()

			// Create processor with behavior defaults + custom options
			baseOpts := testProcessorOptions()
			customOpts := []ProcessorOption{
				WithSchemaValidatorFactory(func(k8.SchemaClient, xp.DefinitionClient, logging.Logger) SchemaValidator {
					return &tu.MockSchemaValidator{
						ValidateResourcesFn: func(context.Context, *un.Unstructured, []cpd.Unstructured) error {
							return nil
						},
					}
				}),
				WithDiffCalculatorFactory(func(k8.ApplyClient, xp.ResourceTreeClient, ResourceManager, logging.Logger, renderer.DiffOptions) DiffCalculator {
					return &tu.MockDiffCalculator{
						CalculateDiffsFn: func(_ context.Context, xr *cmp.Unstructured, _ render.Outputs) (map[string]*dt.ResourceDiff, error) {
							// Return a simple diff for the XR to make the test pass
							diffs := make(map[string]*dt.ResourceDiff)
							gvk := xr.GroupVersionKind()
							resourceID := gvk.Kind + "/" + xr.GetName()
							diffs[resourceID] = &dt.ResourceDiff{
								Gvk:          gvk,
								ResourceName: xr.GetName(),
								DiffType:     dt.DiffTypeAdded,
							}

							return diffs, nil
						},
					}
				}),
			}
			baseOpts = append(baseOpts, customOpts...)
			processor := NewDiffProcessor(k8sClients, xpClients, baseOpts...).(*DefaultDiffProcessor)

			// Initialize if needed
			if len(tt.composedResources) > 0 {
				// Mock composition provider that returns a composition
				compositionProvider := func(ctx context.Context, res *un.Unstructured) (*apiextensionsv1.Composition, error) {
					return xpClients.Composition.FindMatchingComposition(ctx, res)
				}

				// Call the method under test
				diffs, err := processor.ProcessNestedXRs(ctx, tt.composedResources, compositionProvider, tt.parentResourceID, tt.depth)

				// Check error
				if (err != nil) != tt.wantErr {
					t.Errorf("ProcessNestedXRs() error = %v, wantErr %v", err, tt.wantErr)
					return
				}

				if tt.wantErr && tt.wantErrContain != "" && !strings.Contains(err.Error(), tt.wantErrContain) {
					t.Errorf("ProcessNestedXRs() error = %v, want error containing %v", err, tt.wantErrContain)
					return
				}

				// Check diff count
				if diff := gcmp.Diff(tt.wantDiffCount, len(diffs)); diff != "" {
					t.Errorf("ProcessNestedXRs() diff count mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}
