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
	"github.com/crossplane/crossplane/v2/cmd/crank/common/resource"
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
func testProcessorOptions(t *testing.T) []ProcessorOption {
	t.Helper()

	return []ProcessorOption{
		WithNamespace("default"),
		WithColorize(false),
		WithCompact(false),
		WithMaxNestedDepth(10),
		WithLogger(tu.TestLogger(t, false)),
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
						WithNoEnvironmentConfigs().
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			resources:     []*un.Unstructured{},
			processorOpts: testProcessorOptions(t),
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
						WithNoEnvironmentConfigs().
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			resources:     []*un.Unstructured{resource1},
			processorOpts: testProcessorOptions(t),
			verifyOutput: func(t *testing.T, output string) {
				t.Helper()
				// Verify that the error message was written to stdout
				if !strings.Contains(output, "ERROR: Failed to process XR1/my-xr-1") {
					t.Errorf("Expected stdout to contain error message, got: %s", output)
				}
				// Also verify it contains the composition not found error
				if !strings.Contains(output, "composition not found") {
					t.Errorf("Expected stdout to contain 'composition not found' error detail, got: %s", output)
				}
			},
			want: errors.New("unable to process resource XR1/my-xr-1: cannot get composition: composition not found"),
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
						WithNoEnvironmentConfigs().
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			resources:     []*un.Unstructured{resource1, resource2},
			processorOpts: testProcessorOptions(t),
			verifyOutput: func(t *testing.T, output string) {
				t.Helper()
				// Verify that error messages for both resources were written to stdout
				if !strings.Contains(output, "ERROR: Failed to process XR1/my-xr-1") {
					t.Errorf("Expected stdout to contain error message for my-xr-1, got: %s", output)
				}

				if !strings.Contains(output, "ERROR: Failed to process XR1/my-xr-2") {
					t.Errorf("Expected stdout to contain error message for my-xr-2, got: %s", output)
				}
				// Both should contain the composition not found error
				expectedCount := strings.Count(output, "composition not found")
				if expectedCount < 2 {
					t.Errorf("Expected stdout to contain 'composition not found' at least twice, found %d times in: %s", expectedCount, output)
				}
			},
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
						WithNoEnvironmentConfigs().
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			resources:     []*un.Unstructured{resource1},
			processorOpts: testProcessorOptions(t),
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
						WithNoEnvironmentConfigs().
						Build(),
					Function: tu.NewMockFunctionClient().
						WithFailedFunctionsFetch("function not found").
						Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			resources:     []*un.Unstructured{resource1},
			processorOpts: testProcessorOptions(t),
			want:          errors.New("unable to process resource XR1/my-xr-1: cannot get functions for composition: cannot get functions from pipeline: function not found"),
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
						WithEmptyXRDsFetch().
						WithXRDForGVK(schema.GroupVersionKind{Group: testGroup, Version: "v1", Kind: testKind}, mainXRD).
						WithXRDForGVK(schema.GroupVersionKind{Group: "cpd.org", Version: "v1", Kind: "ComposedResource"}, composedXRD).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithNoEnvironmentConfigs().
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
			processorOpts: append(testProcessorOptions(t),
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
						WithNoEnvironmentConfigs().
						Build(),
					Function: tu.NewMockFunctionClient().
						WithSuccessfulFunctionsFetch(functions).
						Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			resources: []*un.Unstructured{resource1},
			processorOpts: append(testProcessorOptions(t),
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

			// Check output if verification function is provided (do this first, before error checks)
			if tt.verifyOutput != nil {
				tt.verifyOutput(t, stdout.String())
			}

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
		})
	}
}

func TestDefaultDiffProcessor_Initialize(t *testing.T) {
	// Setup test context
	ctx := t.Context()

	// Create test resources
	xrd1 := tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "xrd1").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
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
						WithNoEnvironmentConfigs().
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			processorOpts: testProcessorOptions(t),
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
						WithEmptyXRDsFetch().
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
			processorOpts: testProcessorOptions(t),
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
						WithNoEnvironmentConfigs().
						Build(),
					Function:     tu.NewMockFunctionClient().Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
				}

				return k8sClients, xpClients
			},
			processorOpts: testProcessorOptions(t),
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
		observedResources      []cpd.Unstructured
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
					WithNoEnvironmentConfigs().
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
							{Unstructured: un.Unstructured{Object: map[string]any{
								"apiVersion": "example.org/v1",
								"kind":       "ComposedResource",
								"metadata": map[string]any{
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
					WithNoEnvironmentConfigs().
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
							{Unstructured: un.Unstructured{Object: map[string]any{
								"apiVersion": "example.org/v1",
								"kind":       "ComposedResource",
								"metadata": map[string]any{
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
					WithNoEnvironmentConfigs().
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
							{Unstructured: un.Unstructured{Object: map[string]any{
								"apiVersion": "example.org/v1",
								"kind":       "ComposedResource",
								"metadata": map[string]any{
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
					WithNoEnvironmentConfigs().
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
					WithNoEnvironmentConfigs().
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
							{Unstructured: un.Unstructured{Object: map[string]any{
								"apiVersion": "example.org/v1",
								"kind":       "ComposedResource",
								"metadata": map[string]any{
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
					WithNoEnvironmentConfigs().
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
		"ObservedResourcesPassedToRenderFunc": {
			xr:          xr,
			composition: composition,
			functions:   functions,
			resourceID:  "XR/test-xr",
			observedResources: []cpd.Unstructured{
				{Unstructured: un.Unstructured{Object: map[string]any{
					"apiVersion": "s3.aws.crossplane.io/v1",
					"kind":       "Bucket",
					"metadata": map[string]any{
						"name": "observed-bucket",
						"annotations": map[string]any{
							"crossplane.io/composition-resource-name": "bucket",
						},
					},
				}}},
				{Unstructured: un.Unstructured{Object: map[string]any{
					"apiVersion": "iam.aws.crossplane.io/v1",
					"kind":       "User",
					"metadata": map[string]any{
						"name": "observed-user",
						"annotations": map[string]any{
							"crossplane.io/composition-resource-name": "user",
						},
					},
				}}},
			},
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithNoEnvironmentConfigs().
					Build()
			},
			setupRenderFunc: func() RenderFunc {
				return func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
					// Verify observed resources were passed through
					if len(in.ObservedResources) != 2 {
						return render.Outputs{}, errors.Errorf("expected 2 observed resources, got %d", len(in.ObservedResources))
					}

					// Verify the observed resources have the expected kinds
					observedKinds := make(map[string]bool)
					for _, obs := range in.ObservedResources {
						observedKinds[obs.GetKind()] = true
					}

					if !observedKinds["Bucket"] || !observedKinds["User"] {
						return render.Outputs{}, errors.New("expected observed resources to include Bucket and User")
					}

					return render.Outputs{
						CompositeResource: in.CompositeResource,
						ComposedResources: []cpd.Unstructured{
							{Unstructured: un.Unstructured{Object: map[string]any{
								"apiVersion": "example.org/v1",
								"kind":       "ComposedResource",
								"metadata": map[string]any{
									"name": "composed1",
								},
							}}},
						},
					}, nil
				}
			},
			wantComposedCount:    1,
			wantRenderIterations: 1,
			wantErr:              false,
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
			baseOpts := testProcessorOptions(t)
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
			output, err := processor.(*DefaultDiffProcessor).RenderWithRequirements(ctx, tt.xr, tt.composition, tt.functions, tt.resourceID, tt.observedResources)

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

func TestDefaultDiffProcessor_getCompositeResourceXRD(t *testing.T) {
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
		"ManagedResourceIsNotXR": {
			defClient: tu.NewMockDefinitionClient().
				WithXRDForXRNotFound().
				Build(),
			resource: tu.NewResource("nop.example.org/v1alpha1", "NopResource", "test-managed").
				WithSpecField("forProvider", map[string]any{
					"configData": "test-value",
				}).
				Build(),
			wantIsXR:    false,
			wantXRDName: "",
		},
		"ParentXRCorrectlyIdentified": {
			defClient: tu.NewMockDefinitionClient().
				WithXRD(parentXRD).
				Build(),
			resource: tu.NewResource("nested.example.org/v1alpha1", "XParentResource", "test-parent").
				WithSpecField("parentField", "parent-value").
				Build(),
			wantIsXR:    true,
			wantXRDName: "xparentresources.nested.example.org",
		},
		"ChildXRCorrectlyIdentified": {
			defClient: tu.NewMockDefinitionClient().
				WithXRD(childXRD).
				Build(),
			resource: tu.NewResource("nested.example.org/v1alpha1", "XChildResource", "test-child").
				WithSpecField("childField", "child-value").
				Build(),
			wantIsXR:    true,
			wantXRDName: "xchildresources.nested.example.org",
		},
		"ErrorFromDefinitionClientHandled": {
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
			isXR, xrd := processor.getCompositeResourceXRD(ctx, tt.resource)

			// Check isXR result
			if diff := gcmp.Diff(tt.wantIsXR, isXR); diff != "" {
				t.Errorf("getCompositeResourceXRD() isXR mismatch (-want +got):\n%s", diff)
			}

			// Check XRD result
			var gotXRDName string
			if xrd != nil {
				gotXRDName = xrd.GetName()
			}

			if diff := gcmp.Diff(tt.wantXRDName, gotXRDName); diff != "" {
				t.Errorf("getCompositeResourceXRD() XRD name mismatch (-want +got):\n%s", diff)
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
		WithSpecField("forProvider", map[string]any{
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
		WithPipelineStep("generate-managed", "function-go-templating", map[string]any{
			"apiVersion": "template.fn.crossplane.io/v1beta1",
			"kind":       "GoTemplate",
			"source":     "Inline",
			"inline": map[string]any{
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
		"NoComposedResourcesReturnsEmpty": {
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
		"OnlyManagedResourcesReturnsEmpty": {
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
		"ChildXRProcessedRecursively": {
			setupMocks: func() (xp.Clients, k8.Clients) {
				// Create functions that the composition references
				functions := []pkgv1.Function{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "function-go-templating",
						},
						Spec: pkgv1.FunctionSpec{
							PackageSpec: pkgv1.PackageSpec{
								Package: "xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.11.0",
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
						WithNoEnvironmentConfigs().
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
		"MaxDepthExceededReturnsError": {
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
		"MixedXRAndManagedResourcesProcessesOnlyXRs": {
			setupMocks: func() (xp.Clients, k8.Clients) {
				// Create functions that the composition references
				functions := []pkgv1.Function{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "function-go-templating",
						},
						Spec: pkgv1.FunctionSpec{
							PackageSpec: pkgv1.PackageSpec{
								Package: "xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.11.0",
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
						WithNoEnvironmentConfigs().
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
		"NestedXRWithExistingResourcesPreservesIdentity": {
			setupMocks: func() (xp.Clients, k8.Clients) {
				// This test reproduces the bug where existing nested XR identity is not preserved
				// resulting in all managed resources showing as removed/added instead of modified

				// Create an EXISTING nested XR with actual cluster name (not generateName)
				existingChildXR := tu.NewResource("nested.example.org/v1alpha1", "XChildResource", "parent-xr-child-abc123").
					WithGenerateName("parent-xr-").
					WithSpecField("childField", "existing-value").
					WithCompositionResourceName("child-xr").
					WithLabels(map[string]string{
						"crossplane.io/composite": "parent-xr-abc",  // Existing composite label
					}).
					Build()

				// Create an existing managed resource owned by the nested XR
				existingManagedResource := tu.NewResource("nop.example.org/v1alpha1", "NopResource", "parent-xr-child-abc123-managed-xyz").
					WithGenerateName("parent-xr-child-abc123-").
					WithSpecField("forProvider", map[string]any{
						"configData": "existing-data",
					}).
					WithCompositionResourceName("managed-resource").
					WithLabels(map[string]string{
						"crossplane.io/composite": "parent-xr-child-abc123",  // Points to existing nested XR
					}).
					Build()

				// Create a parent XR that owns the nested XR
				parentXR := tu.NewResource("parent.example.org/v1alpha1", "XParentResource", "parent-xr-abc").
					WithGenerateName("parent-xr-").
					Build()

				// Create functions
				functions := []pkgv1.Function{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "function-go-templating",
						},
						Spec: pkgv1.FunctionSpec{
							PackageSpec: pkgv1.PackageSpec{
								Package: "xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.11.0",
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
						WithNoEnvironmentConfigs().
						Build(),
					// Mock resource tree to return existing nested XR and its managed resources
					ResourceTree: tu.NewMockResourceTreeClient().
						WithResourceTreeFromXRAndComposed(
							parentXR,
							[]*un.Unstructured{existingChildXR, existingManagedResource},
						).
						Build(),
				}

				// Create CRDs
				childCRD := tu.NewCRD("xchildresources.nested.example.org", "nested.example.org", "XChildResource").
					WithListKind("XChildResourceList").
					WithPlural("xchildresources").
					WithSingular("xchildresource").
					WithVersion("v1alpha1", true, true).
					WithStandardSchema("childField").
					Build()

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
				// The RENDERED nested XR (from parent composition) with generateName but no name
				{Unstructured: *childXR},
			},
			parentResourceID: "XParentResource/parent-xr-abc",
			depth:            1,
			// TODO: This will currently fail because the nested XR gets "(generated)" name
			// After fix, should NOT show managed resources as removed/added
			wantDiffCount: 1, // Just the nested XR diff, not its managed resources as separate remove/add
			wantErr:       false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Setup mocks
			xpClients, k8sClients := tt.setupMocks()

			// Create processor with behavior defaults + custom options
			baseOpts := testProcessorOptions(t)
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

				// Create a mock parent XR (nil is acceptable for tests that don't need observed resources)
				var parentXR *cmp.Unstructured

				// Call the method under test
				diffs, err := processor.ProcessNestedXRs(ctx, tt.composedResources, compositionProvider, tt.parentResourceID, parentXR, tt.depth)

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

func TestDefaultDiffProcessor_DiffSingleResource_WithObservedResources(t *testing.T) {
	ctx := t.Context()

	// Create test XR
	xr := tu.NewResource("example.org/v1", "XR", "test-xr").
		WithCompositionResourceName("xr-test").
		Build()

	// Create test observed composed resources
	observedBucket := tu.NewResource("s3.aws.crossplane.io/v1", "Bucket", "observed-bucket").
		WithAnnotations(map[string]string{
			"crossplane.io/composition-resource-name": "bucket",
		}).
		WithSpecField("bucketName", "my-bucket").
		Build()

	observedUser := tu.NewResource("iam.aws.crossplane.io/v1", "User", "observed-user").
		WithAnnotations(map[string]string{
			"crossplane.io/composition-resource-name": "user",
		}).
		WithSpecField("userName", "my-user").
		Build()

	// Create a composition with pipeline mode
	composition := tu.NewComposition("test-composition").
		WithCompositeTypeRef("example.org/v1", "XR").
		WithPipelineMode().
		WithPipelineStep("step1", "function-test", nil).
		Build()

	// Create test functions
	functions := []pkgv1.Function{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "function-test",
			},
		},
	}

	tests := map[string]struct {
		setupMocks           func() (k8.Clients, xp.Clients)
		wantObservedInRender bool
		wantObservedCount    int
		wantErr              bool
		wantErrContain       string
		verifyObservedPassed bool
	}{
		"ObservedResourcesFetchedAndPassedToRender": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create resource tree with observed composed resources
				resourceTree := &resource.Resource{
					Unstructured: *xr,
					Children: []*resource.Resource{
						{Unstructured: *observedBucket},
						{Unstructured: *observedUser},
					},
				}

				// Create XRD
				xrdUnstructured := tu.NewXRD("xrs.example.org", "example.org", "XR").
					WithPlural("xrs").
					WithSingular("xr").
					WithVersion("v1", true, true).
					WithSchema(&extv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"spec":   {Type: "object"},
							"status": {Type: "object"},
						},
					}).
					BuildAsUnstructured()

				// Create CRDs
				xrCRD := tu.NewCRD("xrs.example.org", "example.org", "XR").
					WithListKind("XRList").
					WithPlural("xrs").
					WithSingular("xr").
					WithVersion("v1", true, true).
					WithStandardSchema("field").
					Build()

				bucketCRD := tu.NewCRD("buckets.s3.aws.crossplane.io", "s3.aws.crossplane.io", "Bucket").
					WithListKind("BucketList").
					WithPlural("buckets").
					WithSingular("bucket").
					WithVersion("v1", true, true).
					WithStandardSchema("bucketName").
					Build()

				userCRD := tu.NewCRD("users.iam.aws.crossplane.io", "iam.aws.crossplane.io", "User").
					WithListKind("UserList").
					WithPlural("users").
					WithSingular("user").
					WithVersion("v1", true, true).
					WithStandardSchema("userName").
					Build()

				k8sClients := k8.Clients{
					Apply: tu.NewMockApplyClient().
						WithSuccessfulDryRun().
						Build(),
					Resource: tu.NewMockResourceClient().
						WithResourcesExist(xr).
						Build(),
					Schema: tu.NewMockSchemaClient().
						WithNoResourcesRequiringCRDs().
						WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
							switch {
							case gvk.Group == "example.org" && gvk.Kind == "XR":
								return xrCRD, nil
							case gvk.Group == "s3.aws.crossplane.io" && gvk.Kind == "Bucket":
								return bucketCRD, nil
							case gvk.Group == "iam.aws.crossplane.io" && gvk.Kind == "User":
								return userCRD, nil
							default:
								return nil, errors.Errorf("CRD not found for %v", gvk)
							}
						}).
						WithSuccessfulCRDByNameFetch("xrs.example.org", xrCRD).
						Build(),
					Type: tu.NewMockTypeConverter().Build(),
				}

				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionMatch(composition).
						Build(),
					Definition: tu.NewMockDefinitionClient().
						WithXRDForXR(xrdUnstructured).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithNoEnvironmentConfigs().
						Build(),
					Function: tu.NewMockFunctionClient().
						WithSuccessfulFunctionsFetch(functions).
						Build(),
					ResourceTree: tu.NewMockResourceTreeClient().
						WithGetResourceTree(func(_ context.Context, _ *un.Unstructured) (*resource.Resource, error) {
							return resourceTree, nil
						}).
						Build(),
				}

				return k8sClients, xpClients
			},
			wantObservedInRender: true,
			wantObservedCount:    2,
			verifyObservedPassed: true,
			wantErr:              false,
		},
		"EmptyObservedResourcesWhenTreeEmpty": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create empty resource tree
				emptyTree := &resource.Resource{
					Unstructured: *xr,
					Children:     []*resource.Resource{},
				}

				// Create XRD
				xrdUnstructured := tu.NewXRD("xrs.example.org", "example.org", "XR").
					WithPlural("xrs").
					WithSingular("xr").
					WithVersion("v1", true, true).
					WithSchema(&extv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"spec":   {Type: "object"},
							"status": {Type: "object"},
						},
					}).
					BuildAsUnstructured()

				// Create CRD
				xrCRD := tu.NewCRD("xrs.example.org", "example.org", "XR").
					WithListKind("XRList").
					WithPlural("xrs").
					WithSingular("xr").
					WithVersion("v1", true, true).
					WithStandardSchema("field").
					Build()

				k8sClients := k8.Clients{
					Apply: tu.NewMockApplyClient().
						WithSuccessfulDryRun().
						Build(),
					Resource: tu.NewMockResourceClient().
						WithResourcesExist(xr).
						Build(),
					Schema: tu.NewMockSchemaClient().
						WithNoResourcesRequiringCRDs().
						WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
							if gvk.Group == "example.org" && gvk.Kind == "XR" {
								return xrCRD, nil
							}

							return nil, errors.Errorf("CRD not found for %v", gvk)
						}).
						WithSuccessfulCRDByNameFetch("xrs.example.org", xrCRD).
						Build(),
					Type: tu.NewMockTypeConverter().Build(),
				}

				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionMatch(composition).
						Build(),
					Definition: tu.NewMockDefinitionClient().
						WithXRDForXR(xrdUnstructured).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithNoEnvironmentConfigs().
						Build(),
					Function: tu.NewMockFunctionClient().
						WithSuccessfulFunctionsFetch(functions).
						Build(),
					ResourceTree: tu.NewMockResourceTreeClient().
						WithGetResourceTree(func(_ context.Context, _ *un.Unstructured) (*resource.Resource, error) {
							return emptyTree, nil
						}).
						Build(),
				}

				return k8sClients, xpClients
			},
			wantObservedInRender: true,
			wantObservedCount:    0,
			verifyObservedPassed: true,
			wantErr:              false,
		},
		"ContinuesWhenFetchObservedResourcesFails": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create XRD
				xrdUnstructured := tu.NewXRD("xrs.example.org", "example.org", "XR").
					WithPlural("xrs").
					WithSingular("xr").
					WithVersion("v1", true, true).
					WithSchema(&extv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"spec":   {Type: "object"},
							"status": {Type: "object"},
						},
					}).
					BuildAsUnstructured()

				// Create CRD
				xrCRD := tu.NewCRD("xrs.example.org", "example.org", "XR").
					WithListKind("XRList").
					WithPlural("xrs").
					WithSingular("xr").
					WithVersion("v1", true, true).
					WithStandardSchema("field").
					Build()

				k8sClients := k8.Clients{
					Apply: tu.NewMockApplyClient().
						WithSuccessfulDryRun().
						Build(),
					Resource: tu.NewMockResourceClient().
						WithResourcesExist(xr).
						Build(),
					Schema: tu.NewMockSchemaClient().
						WithNoResourcesRequiringCRDs().
						WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
							if gvk.Group == "example.org" && gvk.Kind == "XR" {
								return xrCRD, nil
							}

							return nil, errors.Errorf("CRD not found for %v", gvk)
						}).
						WithSuccessfulCRDByNameFetch("xrs.example.org", xrCRD).
						Build(),
					Type: tu.NewMockTypeConverter().Build(),
				}

				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionMatch(composition).
						Build(),
					Definition: tu.NewMockDefinitionClient().
						WithXRDForXR(xrdUnstructured).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithNoEnvironmentConfigs().
						Build(),
					Function: tu.NewMockFunctionClient().
						WithSuccessfulFunctionsFetch(functions).
						Build(),
					ResourceTree: tu.NewMockResourceTreeClient().
						WithGetResourceTree(func(_ context.Context, _ *un.Unstructured) (*resource.Resource, error) {
							return nil, errors.New("failed to get resource tree")
						}).
						Build(),
				}

				return k8sClients, xpClients
			},
			wantObservedInRender: true,
			wantObservedCount:    0, // Should pass empty list when fetch fails
			verifyObservedPassed: true,
			wantErr:              false, // Should not error, just log and continue
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			k8sClients, xpClients := tt.setupMocks()

			// Track whether observed resources were passed to render
			var (
				capturedObservedCount int
				capturedObserved      []cpd.Unstructured
			)

			// Create processor with custom render function that captures observed resources
			baseOpts := testProcessorOptions(t)
			customOpts := []ProcessorOption{
				WithRenderFunc(func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
					capturedObserved = in.ObservedResources
					capturedObservedCount = len(in.ObservedResources)

					return render.Outputs{
						CompositeResource: in.CompositeResource,
						ComposedResources: []cpd.Unstructured{},
					}, nil
				}),
				WithSchemaValidatorFactory(func(k8.SchemaClient, xp.DefinitionClient, logging.Logger) SchemaValidator {
					return &tu.MockSchemaValidator{
						ValidateResourcesFn: func(context.Context, *un.Unstructured, []cpd.Unstructured) error {
							return nil
						},
					}
				}),
				WithDiffCalculatorFactory(NewDiffCalculator),
			}
			baseOpts = append(baseOpts, customOpts...)
			processor := NewDiffProcessor(k8sClients, xpClients, baseOpts...)

			// Initialize processor
			err := processor.Initialize(ctx)
			if err != nil {
				t.Fatalf("Failed to initialize processor: %v", err)
			}

			// Call DiffSingleResource
			compositionProvider := func(ctx context.Context, res *un.Unstructured) (*apiextensionsv1.Composition, error) {
				return xpClients.Composition.FindMatchingComposition(ctx, res)
			}

			diffs, err := processor.(*DefaultDiffProcessor).DiffSingleResource(ctx, xr, compositionProvider)

			// Check error expectations
			if (err != nil) != tt.wantErr {
				t.Errorf("DiffSingleResource() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && tt.wantErrContain != "" && !strings.Contains(err.Error(), tt.wantErrContain) {
				t.Errorf("DiffSingleResource() error = %v, want error containing %v", err, tt.wantErrContain)
				return
			}

			if err != nil {
				return
			}

			// Verify observed resources were passed to render if expected
			if tt.verifyObservedPassed {
				if capturedObservedCount != tt.wantObservedCount {
					t.Errorf("DiffSingleResource() passed %d observed resources to render, want %d",
						capturedObservedCount, tt.wantObservedCount)
				}

				// If we expected observed resources, verify they have the composition annotation
				if tt.wantObservedCount > 0 {
					for i, obs := range capturedObserved {
						if _, hasAnno := obs.GetAnnotations()["crossplane.io/composition-resource-name"]; !hasAnno {
							t.Errorf("Observed resource %d missing composition-resource-name annotation", i)
						}
					}
				}
			}

			// Verify diffs were returned (even if empty)
			if diffs == nil {
				t.Errorf("DiffSingleResource() returned nil diffs, expected non-nil map")
			}
		})
	}
}
