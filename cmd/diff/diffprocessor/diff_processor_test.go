package diffprocessor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/types"
	"github.com/crossplane/cli/v2/cmd/crossplane/common/resource"
	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	v1 "github.com/crossplane/function-sdk-go/proto/v1"
	gcmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/sergi/go-diff/diffmatchpatch"
	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
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

func TestDefaultDiffProcessor_removeNamespacesFromClusterScopedResources(t *testing.T) {
	secretGVK := schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	namespaceGVK := schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}

	clusterCRD := makeCRD("clusterthings.example.org", "ClusterThing", "example.org", "v1")
	clusterCRD.Spec.Scope = extv1.ClusterScoped

	// Built-in kinds have no CRD, so a GetCRD call means the discovery path
	// was skipped. Cases relying on discovery use this to catch that.
	noCRDs := func() *tu.MockSchemaClient {
		return tu.NewMockSchemaClient().
			WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
				return nil, errors.Errorf("GetCRD should not be called for %s when discovery resolves scope", gvk.String())
			}).
			Build()
	}

	tests := map[string]struct {
		reason        string
		setupResource func() *tu.MockResourceClient
		setupSchema   func() *tu.MockSchemaClient
		resources     []cpd.Unstructured
		wantNamespace []string
		wantErr       bool
		wantErrMsg    string
	}{
		"BuiltInNamespacedResourceKeepsNamespace": {
			reason: "A namespaced built-in has no CRD; discovery reports it namespaced so its namespace is preserved.",
			setupResource: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().WithNamespacedResource(secretGVK).Build()
			},
			setupSchema: noCRDs,
			resources: []cpd.Unstructured{
				*tu.NewResource("v1", "Secret", "creds").InNamespace("default").BuildUComposed(),
			},
			wantNamespace: []string{"default"},
		},
		"BuiltInClusterScopedResourceLosesNamespace": {
			reason: "A cluster-scoped built-in has no CRD; discovery reports it cluster-scoped so render's namespace is stripped.",
			setupResource: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().WithClusterScopedResource(namespaceGVK).Build()
			},
			setupSchema: noCRDs,
			resources: []cpd.Unstructured{
				*tu.NewResource("v1", "Namespace", "generated").InNamespace("default").BuildUComposed(),
			},
			wantNamespace: []string{""},
		},
		"MixedScopesResolveIndependently": {
			reason: "Each resource's scope is resolved on its own; a cluster-scoped sibling does not affect a namespaced one.",
			setupResource: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(secretGVK).
					WithClusterScopedResource(namespaceGVK).
					Build()
			},
			setupSchema: noCRDs,
			resources: []cpd.Unstructured{
				*tu.NewResource("v1", "Secret", "creds").InNamespace("default").BuildUComposed(),
				*tu.NewResource("v1", "Namespace", "generated").InNamespace("default").BuildUComposed(),
			},
			wantNamespace: []string{"default", ""},
		},
		"FallsBackToCRDWhenDiscoveryFails": {
			reason: "When discovery cannot resolve a custom kind, the CRD supplies the scope.",
			setupResource: func() *tu.MockResourceClient {
				// No scopes configured, so IsNamespacedResource errors.
				return tu.NewMockResourceClient().Build()
			},
			setupSchema: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithFoundCRD("example.org", "ClusterThing", clusterCRD).
					Build()
			},
			resources: []cpd.Unstructured{
				*tu.NewResource("example.org/v1", "ClusterThing", "thing").InNamespace("default").BuildUComposed(),
			},
			wantNamespace: []string{""},
		},
		"ErrorsWhenNeitherDiscoveryNorCRDResolvesScope": {
			reason: "Scope must be known to proceed; an unresolvable kind fails the diff rather than guessing.",
			setupResource: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().Build()
			},
			setupSchema: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithGetCRD(func(_ context.Context, _ schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
						return nil, errors.New("CRD not found")
					}).
					Build()
			},
			resources: []cpd.Unstructured{
				*tu.NewResource("example.org/v1", "ClusterThing", "thing").InNamespace("default").BuildUComposed(),
			},
			wantErr:    true,
			wantErrMsg: "cannot determine scope for resource ClusterThing/thing",
		},
		"ResourceWithoutNamespaceIsSkipped": {
			reason: "A resource render left unnamespaced needs no scope lookup at all.",
			setupResource: func() *tu.MockResourceClient {
				// Any scope lookup would error, proving none happened.
				return tu.NewMockResourceClient().Build()
			},
			setupSchema: noCRDs,
			resources: []cpd.Unstructured{
				*tu.NewResource("example.org/v1", "ClusterThing", "thing").BuildUComposed(),
			},
			wantNamespace: []string{""},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			processor := &DefaultDiffProcessor{
				resourceClient: tt.setupResource(),
				schemaClient:   tt.setupSchema(),
				config: ProcessorConfig{
					Logger: tu.TestLogger(t, false),
				},
			}

			err := processor.removeNamespacesFromClusterScopedResources(t.Context(), tt.resources)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("\n%s\nremoveNamespacesFromClusterScopedResources(): expected error but got none", tt.reason)
				}

				if tt.wantErrMsg != "" && !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("\n%s\nremoveNamespacesFromClusterScopedResources(): error %q doesn't contain %q",
						tt.reason, err.Error(), tt.wantErrMsg)
				}

				return
			}

			if err != nil {
				t.Fatalf("\n%s\nremoveNamespacesFromClusterScopedResources(): unexpected error: %v", tt.reason, err)
			}

			got := make([]string, len(tt.resources))
			for i := range tt.resources {
				got[i] = (&un.Unstructured{Object: tt.resources[i].UnstructuredContent()}).GetNamespace()
			}

			if diff := gcmp.Diff(tt.wantNamespace, got); diff != "" {
				t.Errorf("\n%s\nremoveNamespacesFromClusterScopedResources(): -want namespaces, +got:\n%s", tt.reason, diff)
			}
		})
	}
}

// testProcessorOptions returns sensible default options for tests.
// Tests can append additional options or override these as needed.
//
// The default WithRenderFunc is a no-op that returns an empty CompositionOutputs.
// The production default would spin up a Docker engine, which unit tests cannot rely on.
// Tests that need specific render behavior should override via WithRenderFunc.
func testProcessorOptions(t *testing.T) []ProcessorOption {
	t.Helper()

	return []ProcessorOption{
		WithColorize(false),
		WithCompact(false),
		WithMaxNestedDepth(10),
		WithMaxRenderIterations(DefaultMaxRenderIterations),
		WithLogger(tu.TestLogger(t, false)),
		WithRenderFunc(func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
			return render.CompositionOutputs{CompositeResource: in.CompositeResource}, nil
		}),
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

	// The XR's XRD and CRD. The effective XR is defaulted before its composition is resolved, so even
	// the cases that fail resolving it need them.
	testXRD := tu.NewXRD(testXRDName, testGroup, testKind).WithVersion("v1", true, true).BuildAsUnstructured()
	testCRD := makeTestCRD(testCRDName, testKind, testGroup, testAPIVersion)

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
					Credential:  &tu.MockCredentialClient{},
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
					Resource: tu.NewMockResourceClient().WithResourceNotFound().Build(),
					Schema:   tu.NewMockSchemaClient().WithSuccessfulCRDByNameFetch(testCRDName, testCRD).Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				// Create Crossplane client mocks
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithNoMatchingComposition().
						Build(),
					Credential: &tu.MockCredentialClient{},
					Definition: tu.NewMockDefinitionClient().WithXRDForXR(testXRD).Build(),
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
			// Note: Error output now goes to stderr (see TestDefaultDiffProcessor_PerformDiff_StderrErrorOutput)
			// This test verifies the error return value, not stderr output
			verifyOutput: nil,
			want:         errors.New("unable to process resource XR1/my-xr-1: cannot get composition: composition not found"),
		},
		"MultipleResourceErrors": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create Kubernetes client mocks
				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().WithResourceNotFound().Build(),
					Schema:   tu.NewMockSchemaClient().WithSuccessfulCRDByNameFetch(testCRDName, testCRD).Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				// Create Crossplane client mocks
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithNoMatchingComposition().
						Build(),
					Credential: &tu.MockCredentialClient{},
					Definition: tu.NewMockDefinitionClient().WithXRDForXR(testXRD).Build(),
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
			// Note: Error output now goes to stderr (see TestDefaultDiffProcessor_PerformDiff_StderrErrorOutput)
			// This test verifies the error return value, not stderr output
			verifyOutput: nil,
			want: errors.New("[unable to process resource XR1/my-xr-1: cannot get composition: composition not found, " +
				"unable to process resource XR1/my-xr-2: cannot get composition: composition not found]"),
		},
		"CompositionNotFound": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				// Create Kubernetes client mocks
				k8sClients := k8.Clients{
					Apply:    tu.NewMockApplyClient().Build(),
					Resource: tu.NewMockResourceClient().WithResourceNotFound().Build(),
					Schema:   tu.NewMockSchemaClient().WithSuccessfulCRDByNameFetch(testCRDName, testCRD).Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				// Create Crossplane client mocks
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithNoMatchingComposition().
						Build(),
					Credential: &tu.MockCredentialClient{},
					Definition: tu.NewMockDefinitionClient().WithXRDForXR(testXRD).Build(),
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
					Resource: tu.NewMockResourceClient().WithResourceNotFound().Build(),
					Schema:   tu.NewMockSchemaClient().WithSuccessfulCRDByNameFetch(testCRDName, testCRD).Build(),
					Type:     tu.NewMockTypeConverter().Build(),
				}

				// Create Crossplane client mocks
				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionMatch(composition).
						Build(),
					Credential: &tu.MockCredentialClient{},
					Definition: tu.NewMockDefinitionClient().WithXRDForXR(testXRD).Build(),
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
					Credential: &tu.MockCredentialClient{},
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
				WithRenderFunc(func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					// Only return composed resources for the main XR, not for nested XRs
					// to avoid infinite recursion
					if in.CompositeResource.GetKind() == testKind {
						return render.CompositionOutputs{
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
					return render.CompositionOutputs{
						CompositeResource: in.CompositeResource,
					}, nil
				}),
				// Override the schema validator factory to use a simple validator
				WithSchemaValidatorFactory(func(k8.SchemaClient, k8.ResourceClient, xp.DefinitionClient, logging.Logger) SchemaValidator {
					return &tu.MockSchemaValidator{
						ValidateResourcesFn: func(context.Context, *un.Unstructured, []cpd.Unstructured) error {
							return nil
						},
					}
				}),
				// Override the diff calculator factory to return actual diffs
				WithDiffCalculatorFactory(func(k8.ApplyClient, k8.AccessChecker, xp.ResourceTreeClient, ResourceManager, logging.Logger, renderer.DiffOptions, DryRunOn, Defaulter) DiffCalculator {
					return &tu.MockDiffCalculator{
						CalculateNonRemovalDiffsFn: func(context.Context, *cmp.Unstructured, render.CompositionOutputs) (map[string]*dt.ResourceDiff, map[string]bool, error) {
							diffs := make(map[string]*dt.ResourceDiff)
							rendered := make(map[string]bool)

							// Add a modified diff (not just equal)
							lineDiffs := []diffmatchpatch.Diff{
								{Type: diffmatchpatch.DiffDelete, Text: "  field: old-value"},
								{Type: diffmatchpatch.DiffInsert, Text: "  field: new-value"},
							}

							diffKey1 := "example.org/v1/XR1/test-xr"
							diffs[diffKey1] = &dt.ResourceDiff{
								Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "XR1"},
								ResourceName: "test-xr",
								DiffType:     dt.DiffTypeModified,
								LineDiffs:    lineDiffs,                                          // Add line diffs
								Current:      dt.ResourceViews{Raw: resource1, Clean: resource1}, // for completeness
								Desired:      dt.ResourceViews{Raw: resource1, Clean: resource1}, // for completeness
							}
							rendered[diffKey1] = true

							// Add a composed resource diff that's also modified
							diffKey2 := "example.org/v1/ComposedResource/resource-a"
							diffs[diffKey2] = &dt.ResourceDiff{
								Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "ComposedResource"},
								ResourceName: "resource-a",
								DiffType:     dt.DiffTypeModified,
								LineDiffs:    lineDiffs,
								Current:      dt.ResourceViews{Raw: composedResource, Clean: composedResource},
								Desired:      dt.ResourceViews{Raw: composedResource, Clean: composedResource},
							}
							rendered[diffKey2] = true

							return diffs, rendered, nil
						},
					}
				}),
				// Override the diff renderer factory to produce actual output
				// The factory receives DiffOptions which contains Stdout where output should be written
				WithDiffRendererFactory(func(_ logging.Logger, opts renderer.DiffOptions) renderer.DiffRenderer {
					return &tu.MockDiffRenderer{
						RenderDiffsFn: func(_ []dt.XRDiffGroup, _ []dt.OutputError, _ []dt.OutputWarning) error {
							// Write a simple summary to the output via opts.Stdout
							w := opts.Stdout

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
					Credential: &tu.MockCredentialClient{},
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
					// resource1 exists in the cluster, so its observed resources
					// are fetched before validation runs. That fetch has to
					// succeed for this case to reach the validation failure it is
					// actually about.
					ResourceTree: tu.NewMockResourceTreeClient().
						WithEmptyResourceTree().
						Build(),
				}

				return k8sClients, xpClients
			},
			resources: []*un.Unstructured{resource1},
			processorOpts: append(testProcessorOptions(t),
				WithRenderFunc(func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					// Return valid render outputs
					return render.CompositionOutputs{
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
				WithSchemaValidatorFactory(func(_ k8.SchemaClient, _ k8.ResourceClient, _ xp.DefinitionClient, _ logging.Logger) SchemaValidator {
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

			// Create stdout buffer and add it to processor options so renderers can access it
			var stdout bytes.Buffer

			opts := append([]ProcessorOption{}, tt.processorOpts...)
			opts = append(opts, WithStdout(&stdout))

			// Create the diff processor
			processor := NewDiffProcessor(k8sClients, xpClients, opts...)

			// Create a mock composition provider that uses the same mock composition client
			compositionProvider := func(ctx context.Context, res *un.Unstructured) (types.ResolvedComposition, error) {
				return xpClients.Composition.FindMatchingComposition(ctx, res)
			}
			_, err := processor.PerformDiff(ctx, tt.resources, compositionProvider)

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

// Note: PerformDiff's per-XR grouping structure is verified end-to-end (real
// renderer, real JSON) by TestDiffIntegration/MultipleXRsGroupedByInputXR, and
// the per-group xrs[] shape — including errored groups — by
// TestStructuredDiffRenderer_GroupsByXR. The identity each group carries, and
// how the input validator's verdicts land, are asserted on the intermediate
// []XRDiffGroup handoff by TestDefaultDiffProcessor_PerformDiff_Groups.

// TestDefaultDiffProcessor_PerformDiff_Groups pins how PerformDiff hands its groups to the renderer. With
// a mock InputValidator it pins the wiring: only what ToRender returns is rendered, and an input it
// rejected is not; every render is recorded; and each Verdicts and RenderOverlaps error lands as a group
// error (replacing that group's diffs), in errors[], and on the returned error. With the real default
// validator it pins what PerformDiff itself decides: the group identity of a generateName-only XR (issue
// #477), and the NameGenerated mark the overlap check relies on. The validation rules themselves are
// tested directly in input_validator_test.go and end to end in diff_integration_test.go; add rule cases
// there, not here.
func TestDefaultDiffProcessor_PerformDiff_Groups(t *testing.T) {
	ctx := t.Context()

	composition := tu.NewComposition("test-comp").
		WithCompositeTypeRef(testGroup+"/"+testAPIVersion, testKind).
		WithPipelineMode().
		WithPipelineStep("step1", "function-test", nil).
		Build()

	functions := []pkgv1.Function{{ObjectMeta: metav1.ObjectMeta{Name: "function-test"}}}

	xrd := tu.NewXRD(testXRDName, testGroup, testKind).
		WithPlural(testPlural).
		WithSingular(testSingular).
		BuildAsUnstructured()

	// sharedKey is returned by the diff calculator for every input XR, so two
	// inputs collide on it.
	const sharedKey = "example.org/v1/Bucket/default/shared"

	// bucket is the shared resource as one input XR's render produces it, controlled by the XR named
	// controller, with spec.value set to value. renderedBy only varies the controller reference's UID,
	// the way a render synthesizes a fresh UID for an XR that does not exist yet. Clean mirrors
	// cleanupForDiff, which strips ownerReferences (and uid) before anything is compared or displayed.
	bucket := func(controller, value, renderedBy string) *dt.ResourceDiff {
		desired := tu.NewResource("example.org/v1", "Bucket", "shared").
			InNamespace("default").
			WithSpecField("value", value).
			WithControllerReference(testKind, controller, testGroup+"/"+testAPIVersion, "uid-rendered-by-"+renderedBy).
			Build()
		clean := desired.DeepCopy()
		clean.SetOwnerReferences(nil)

		return &dt.ResourceDiff{
			Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Bucket"},
			Namespace:    "default",
			ResourceName: "shared",
			DiffType:     dt.DiffTypeModified,
			Desired:      dt.ResourceViews{Raw: desired, Clean: clean},
		}
	}

	xr := func(name string) *un.Unstructured {
		return tu.NewResource(testGroup+"/"+testAPIVersion, testKind, name).WithSpecField("coolField", name).Build()
	}

	ref := func(name string) corev1.ObjectReference {
		return corev1.ObjectReference{APIVersion: testGroup + "/" + testAPIVersion, Kind: testKind, Name: name}
	}

	// recorded is one RecordRender call.
	type recorded struct {
		I        int
		Rendered map[string]bool
		Err      string
	}

	// got is the whole handoff, asserted as one value: the group identities in
	// input order, each errored group's message by name, the global (union)
	// error list the renderer was given, the error PerformDiff returned, and
	// (with a mock validator) the renders it recorded.
	type got struct {
		XRs        []corev1.ObjectReference
		GroupErrs  map[string]string
		GlobalErrs []string
		Err        string
		Recorded   []recorded
	}

	const (
		generatedErr = `cannot combine diffs: inputs XR1/gen-xr-(generated), XR1/gen-xr-(generated) both produce resource ` +
			`"example.org/v1/Bucket/default/shared" only because crossplane-diff gives XRs that share a generateName the ` +
			`same placeholder name; the API server would name them differently, so they cannot be told apart here — diff ` +
			`them separately`

		// renderFailedErr is how a render failure reaches RecordRender: wrapped by the render path.
		renderFailedErr = "cannot calculate diffs for composed resources: render failed"
	)

	genXR := tu.NewResource(testGroup+"/"+testAPIVersion, testKind, "").
		WithGenerateName("gen-xr-").
		WithSpecField("coolField", "value").
		Build()

	// validation is what a mock validator returns: ToRender's list, and Verdicts and RenderOverlaps
	// errors. A nil validation means the real default validator.
	type validation struct {
		toRender []types.ValidatedInput
		verdicts []error
		overlaps []error
	}

	tests := map[string]struct {
		resources []*un.Unstructured
		validator *validation
		// failRender names an input XR whose render fails.
		failRender string
		want       got
	}{
		// An input ToRender already rejected is not rendered; its verdict is a group error, in errors[],
		// and on the returned error.
		"InputRejectedBeforeRenderingIsNotRendered": {
			resources: []*un.Unstructured{xr("my-xr-1"), xr("my-xr-2")},
			validator: &validation{
				toRender: []types.ValidatedInput{{Resource: xr("my-xr-1")}, {Resource: xr("my-xr-2"), Err: errors.New("rejected")}},
				verdicts: []error{nil, errors.New("rejected")},
			},
			want: got{
				XRs:        []corev1.ObjectReference{ref("my-xr-1"), ref("my-xr-2")},
				GroupErrs:  map[string]string{"my-xr-2": "rejected"},
				GlobalErrs: []string{"rejected"},
				Err:        "unable to process resource XR1/my-xr-2: rejected",
				Recorded:   []recorded{{I: 0, Rendered: map[string]bool{sharedKey: true}}},
			},
		},
		// A Verdicts error on a group that rendered fine replaces its diffs.
		"VerdictReplacesAGroupsDiffs": {
			resources: []*un.Unstructured{xr("my-xr-1"), xr("my-xr-2")},
			validator: &validation{
				toRender: []types.ValidatedInput{{Resource: xr("my-xr-1")}, {Resource: xr("my-xr-2")}},
				verdicts: []error{nil, errors.New("verdict")},
			},
			want: got{
				XRs:        []corev1.ObjectReference{ref("my-xr-1"), ref("my-xr-2")},
				GroupErrs:  map[string]string{"my-xr-2": "verdict"},
				GlobalErrs: []string{"verdict"},
				Err:        "unable to process resource XR1/my-xr-2: verdict",
				Recorded: []recorded{
					{I: 0, Rendered: map[string]bool{sharedKey: true}},
					{I: 1, Rendered: map[string]bool{sharedKey: true}},
				},
			},
		},
		"RenderOverlapsReachErrorsAndTheReturnedError": {
			resources: []*un.Unstructured{xr("my-xr-1")},
			validator: &validation{
				toRender: []types.ValidatedInput{{Resource: xr("my-xr-1")}},
				overlaps: []error{errors.New("overlap")},
			},
			want: got{
				XRs:        []corev1.ObjectReference{ref("my-xr-1")},
				GlobalErrs: []string{"overlap"},
				Err:        "overlap",
				Recorded:   []recorded{{I: 0, Rendered: map[string]bool{sharedKey: true}}},
			},
		},
		// Only what ToRender returns is rendered: a duplicate it dropped yields no group.
		"OnlyWhatToRenderReturnsIsRendered": {
			resources: []*un.Unstructured{xr("my-xr-1"), xr("my-xr-1")},
			validator: &validation{toRender: []types.ValidatedInput{{Resource: xr("my-xr-1")}}},
			want: got{
				XRs:      []corev1.ObjectReference{ref("my-xr-1")},
				Recorded: []recorded{{I: 0, Rendered: map[string]bool{sharedKey: true}}},
			},
		},
		// RecordRender sees each render's keys and its error; the error is reported only if Verdicts
		// returns it, which this mock does not.
		"RecordRenderReceivesEachRender": {
			resources:  []*un.Unstructured{xr("my-xr-1"), xr("my-xr-2")},
			validator:  &validation{toRender: []types.ValidatedInput{{Resource: xr("my-xr-1")}, {Resource: xr("my-xr-2")}}},
			failRender: "my-xr-2",
			want: got{
				XRs: []corev1.ObjectReference{ref("my-xr-1"), ref("my-xr-2")},
				Recorded: []recorded{
					{I: 0, Rendered: map[string]bool{sharedKey: true}},
					{I: 1, Err: renderFailedErr},
				},
			},
		},
		// Issue #477: the name used for rendering is synthesized from
		// generateName inside SanitizeXR, so the identity must carry that
		// effective name rather than the input's empty metadata.name.
		"GenerateNameOnlyXR": {
			resources: []*un.Unstructured{genXR},
			want:      got{XRs: []corev1.ObjectReference{ref("gen-xr-(generated)")}},
		},
		// PerformDiff marks a group whose name it synthesized, which is the only way the overlap check
		// can tell two generateName-only inputs apart from one XR reached twice. Without it these
		// identical renderings would merge, reporting one XR's changes for two.
		"SharedGenerateNameReachesTheOverlapCheck": {
			resources: []*un.Unstructured{genXR, genXR.DeepCopy()},
			want: got{
				XRs:        []corev1.ObjectReference{ref("gen-xr-(generated)"), ref("gen-xr-(generated)")},
				GlobalErrs: []string{generatedErr},
				Err:        generatedErr,
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			k8sClients := k8.Clients{
				Apply:    tu.NewMockApplyClient().WithSuccessfulDryRun().Build(),
				Resource: tu.NewMockResourceClient().WithResourceNotFound().Build(),
				Schema: tu.NewMockSchemaClient().
					WithNoResourcesRequiringCRDs().
					WithSuccessfulCRDByNameFetch(testCRDName, makeTestCRD(testCRDName, testKind, testGroup, testAPIVersion)).
					Build(),
				Type: tu.NewMockTypeConverter().Build(),
			}
			xpClients := xp.Clients{
				Composition:  tu.NewMockCompositionClient().WithSuccessfulCompositionMatch(composition).Build(),
				Credential:   &tu.MockCredentialClient{},
				Definition:   tu.NewMockDefinitionClient().WithXRDForXR(xrd).Build(),
				Environment:  tu.NewMockEnvironmentClient().WithNoEnvironmentConfigs().Build(),
				Function:     tu.NewMockFunctionClient().WithSuccessfulFunctionsFetch(functions).Build(),
				ResourceTree: tu.NewMockResourceTreeClient().WithEmptyResourceTree().Build(),
			}

			var (
				gotGroups   []dt.XRDiffGroup
				gotErrs     []dt.OutputError
				gotRecorded []recorded
			)

			opts := append(testProcessorOptions(t),
				WithSchemaValidatorFactory(func(k8.SchemaClient, k8.ResourceClient, xp.DefinitionClient, logging.Logger) SchemaValidator {
					return &tu.MockSchemaValidator{
						ValidateResourcesFn: func(context.Context, *un.Unstructured, []cpd.Unstructured) error {
							return nil
						},
					}
				}),
				WithDiffCalculatorFactory(func(k8.ApplyClient, k8.AccessChecker, xp.ResourceTreeClient, ResourceManager, logging.Logger, renderer.DiffOptions, DryRunOn, Defaulter) DiffCalculator {
					return &tu.MockDiffCalculator{
						CalculateNonRemovalDiffsFn: func(_ context.Context, rendered *cmp.Unstructured, _ render.CompositionOutputs) (map[string]*dt.ResourceDiff, map[string]bool, error) {
							if rendered.GetName() == tt.failRender {
								return nil, nil, errors.New("render failed")
							}

							diff := bucket("parent", "same", rendered.GetName())

							return map[string]*dt.ResourceDiff{sharedKey: diff}, map[string]bool{sharedKey: true}, nil
						},
					}
				}),
				WithDiffRendererFactory(func(logging.Logger, renderer.DiffOptions) renderer.DiffRenderer {
					return &tu.MockDiffRenderer{
						RenderDiffsFn: func(groups []dt.XRDiffGroup, errs []dt.OutputError, _ []dt.OutputWarning) error {
							gotGroups = groups
							gotErrs = errs

							return nil
						},
					}
				}),
			)

			if v := tt.validator; v != nil {
				opts = append(opts, WithInputValidatorFactory(func(logging.Logger, []*un.Unstructured) InputValidator {
					return &tu.MockInputValidator{
						ToRenderFn: func() []types.ValidatedInput { return v.toRender },
						RecordRenderFn: func(i int, rendered map[string]bool, err error) {
							gotRecorded = append(gotRecorded, recorded{I: i, Rendered: rendered, Err: errMessage(err)})
						},
						VerdictsFn: func(groups []dt.XRDiffGroup) []error {
							if v.verdicts == nil {
								return make([]error, len(groups))
							}

							return v.verdicts
						},
						RenderOverlapsFn: func([]dt.XRDiffGroup) []error { return v.overlaps },
					}
				}))
			}

			processor := NewDiffProcessor(k8sClients, xpClients, opts...)

			_, err := processor.PerformDiff(ctx, tt.resources, func(ctx context.Context, res *un.Unstructured) (types.ResolvedComposition, error) {
				return xpClients.Composition.FindMatchingComposition(ctx, res)
			})

			result := got{Recorded: gotRecorded}

			for _, g := range gotGroups {
				result.XRs = append(result.XRs, g.XR)

				if g.Err == nil {
					continue
				}

				if g.Diffs != nil {
					t.Errorf("group %s/%s carries an error and diffs; an errored input must emit no partial result", g.XR.Kind, g.XR.Name)
				}

				if result.GroupErrs == nil {
					result.GroupErrs = map[string]string{}
				}

				result.GroupErrs[g.XR.Name] = g.Err.Message
			}

			for _, e := range gotErrs {
				result.GlobalErrs = append(result.GlobalErrs, e.Message)
			}

			if err != nil {
				result.Err = err.Error()
			}

			if diff := gcmp.Diff(tt.want, result); diff != "" {
				t.Errorf("PerformDiff(...) group handoff: -want, +got:\n%s", diff)
			}
		})
	}
}

// TestDefaultDiffProcessor_PerformDiff_LateWarningsReachStructuredOutput pins the warning channel's
// contract for advisories that can only be discovered during teardown. The leftover-function-container
// advisory is raised from FunctionProvider.Cleanup, which used to run in the command layer's defer —
// i.e. after the renderer had already consumed and serialized the warning slice — so it reached stderr
// but was structurally absent from warnings[], contradicting the README. PerformDiff therefore tears
// down before rendering, and this test fails if that ordering is lost.
func TestDefaultDiffProcessor_PerformDiff_LateWarningsReachStructuredOutput(t *testing.T) {
	ctx := t.Context()

	const advisory = "Some containers could not be cleaned up"

	var stdout, stderr bytes.Buffer

	warnings := NewWarningLogger(tu.TestLogger(t, false), &stderr)

	k8sClients := k8.Clients{
		Apply:    tu.NewMockApplyClient().Build(),
		Resource: tu.NewMockResourceClient().Build(),
		Schema:   tu.NewMockSchemaClient().Build(),
		Type:     tu.NewMockTypeConverter().Build(),
	}

	// The XR's own diff fails (no matching composition), which additionally pins the README's claim
	// that a warning is still reported when a later step fails — the advisory has to survive alongside
	// errors[], not instead of it.
	xpClients := xp.Clients{
		Composition:  tu.NewMockCompositionClient().WithNoMatchingComposition().Build(),
		Credential:   &tu.MockCredentialClient{},
		Definition:   tu.NewMockDefinitionClient().Build(),
		Environment:  tu.NewMockEnvironmentClient().WithNoEnvironmentConfigs().Build(),
		Function:     tu.NewMockFunctionClient().Build(),
		ResourceTree: tu.NewMockResourceTreeClient().Build(),
	}

	processor := NewDiffProcessor(k8sClients, xpClients,
		append(testProcessorOptions(t),
			WithLogger(warnings),
			WithWarnings(warnings),
			WithOutputFormat(renderer.OutputFormatJSON),
			WithStdout(&stdout),
			WithStderr(&stderr),
			// Stands in for CachedFunctionProvider.Cleanup raising its advisory when a container could
			// not be removed; the point under test is WHEN cleanup runs, not how it detects leftovers.
			WithFunctionProviderFactory(func(_ xp.FunctionClient, logger logging.Logger) FunctionProvider {
				return &tu.MockFunctionProvider{
					CleanupFn: func(context.Context) error {
						logger.Info(advisory, "errors", 1)
						return nil
					},
				}
			}),
		)...,
	)

	resource := tu.NewResource("example.org/v1", "XR1", "my-xr-1").
		WithSpecField("coolField", "test-value-1").
		Build()

	if _, err := processor.PerformDiff(ctx, []*un.Unstructured{resource}, xpClients.Composition.FindMatchingComposition); err == nil {
		t.Fatal("PerformDiff(): expected the XR's diff to fail, got nil")
	}

	if !strings.Contains(stderr.String(), advisory) {
		t.Errorf("teardown advisory should reach stderr, got:\n%s", stderr.String())
	}

	// Decoded via the wire contract rather than the renderer's own struct: what matters is that a
	// machine consumer reading warnings[] out of the JSON sees the advisory.
	var got struct {
		Warnings []dt.OutputWarning `json:"warnings"`
		Errors   []dt.OutputError   `json:"errors"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("cannot parse structured output %q: %v", stdout.String(), err)
	}

	want := []dt.OutputWarning{{Message: advisory, Context: map[string]string{"errors": "1"}}}
	if diff := gcmp.Diff(want, got.Warnings); diff != "" {
		t.Errorf("structured warnings[] mismatch (-want +got):\n%s", diff)
	}

	if len(got.Errors) != 1 {
		t.Errorf("expected the XR's failure to still be reported in errors[], got %v", got.Errors)
	}
}

// TestDefaultDiffProcessor_PerformDiff_StderrErrorOutput verifies that when
// resource processing fails, detailed errors are written to stderr for human visibility.
// This tests the WithStderr option and the stderr error output path.
func TestDefaultDiffProcessor_PerformDiff_StderrErrorOutput(t *testing.T) {
	ctx := t.Context()

	// Create test resource
	resource := tu.NewResource("example.org/v1", "XR1", "my-xr-1").
		WithSpecField("coolField", "test-value-1").
		Build()

	// Create stderr buffer to capture error output
	var stderrBuf bytes.Buffer

	// Create Kubernetes client mocks. The XR's CRD is needed because the effective XR is defaulted
	// before its composition is resolved.
	k8sClients := k8.Clients{
		Apply:    tu.NewMockApplyClient().Build(),
		Resource: tu.NewMockResourceClient().WithResourceNotFound().Build(),
		Schema: tu.NewMockSchemaClient().
			WithSuccessfulCRDByNameFetch(testCRDName, makeTestCRD(testCRDName, testKind, testGroup, testAPIVersion)).
			Build(),
		Type: tu.NewMockTypeConverter().Build(),
	}

	// Create Crossplane client mocks with a failing composition match
	xpClients := xp.Clients{
		Composition: tu.NewMockCompositionClient().
			WithNoMatchingComposition().
			Build(),
		Credential: &tu.MockCredentialClient{},
		Definition: tu.NewMockDefinitionClient().
			WithXRDForXR(tu.NewXRD(testXRDName, testGroup, testKind).WithVersion("v1", true, true).BuildAsUnstructured()).
			Build(),
		Environment: tu.NewMockEnvironmentClient().
			WithNoEnvironmentConfigs().
			Build(),
		Function:     tu.NewMockFunctionClient().Build(),
		ResourceTree: tu.NewMockResourceTreeClient().Build(),
	}

	// Create processor with custom stderr buffer
	processor := NewDiffProcessor(k8sClients, xpClients,
		append(testProcessorOptions(t),
			WithStderr(&stderrBuf), // Inject test buffer to capture stderr
		)...,
	)

	// Create composition provider using mock client
	compositionProvider := func(ctx context.Context, res *un.Unstructured) (types.ResolvedComposition, error) {
		return xpClients.Composition.FindMatchingComposition(ctx, res)
	}

	// Run the diff
	_, err := processor.PerformDiff(ctx, []*un.Unstructured{resource}, compositionProvider)

	// Should return an error
	if err == nil {
		t.Fatal("PerformDiff() expected error but got none")
	}

	// Verify stderr contains the error output
	stderrOutput := stderrBuf.String()

	// The error should be formatted using FormatError() which produces:
	// "ERROR: {ResourceID}: {Message}"
	if !strings.Contains(stderrOutput, "ERROR: XR1/my-xr-1:") {
		t.Errorf("Expected stderr to contain 'ERROR: XR1/my-xr-1:', got: %q", stderrOutput)
	}

	if !strings.Contains(stderrOutput, "composition not found") {
		t.Errorf("Expected stderr to contain 'composition not found' error detail, got: %q", stderrOutput)
	}
}

// TestDefaultDiffProcessor_warnIfDeleting covers the `xr`-side counterpart to comp's deleting-XR
// exclusion (issue #452). `xr` diffs exactly the resource the user named, so a deleting cluster copy
// is warned about rather than skipped. The warning travels through the WarningLogger channel, so this
// asserts both halves of the dual emission: the stderr line and the collected structured warning.
// Only top-level XRs are flagged, and an explicitly-null deletionTimestamp is not mistaken for a
// deleting resource.
func TestDefaultDiffProcessor_warnIfDeleting(t *testing.T) {
	deleting := tu.NewResource("example.org/v1", "XR1", "my-xr").
		WithDeletionTimestamp("2026-09-07T11:25:03Z").Build()

	tests := map[string]struct {
		existing     *un.Unstructured
		parentXR     *cmp.Unstructured
		wantStderr   string
		wantWarnings []dt.OutputWarning
	}{
		"NotInCluster_NoWarning": {
			existing: nil,
		},
		"NotDeleting_NoWarning": {
			existing: tu.NewResource("example.org/v1", "XR1", "my-xr").Build(),
		},
		"NullDeletionTimestamp_NoWarning": {
			existing: tu.NewResource("example.org/v1", "XR1", "my-xr").WithDeletionTimestamp(nil).Build(),
		},
		"Deleting_Warns": {
			existing: deleting,
			wantStderr: "WARNING: The resource being diffed is being deleted in the cluster; the diff compares against a resource that is going away " +
				"(deletionTimestamp=2026-09-07T11:25:03Z, resource=XR1/my-xr)\n",
			wantWarnings: []dt.OutputWarning{{
				Message: "The resource being diffed is being deleted in the cluster; the diff compares against a resource that is going away",
				Context: map[string]string{
					"resource":          "XR1/my-xr",
					"deletionTimestamp": "2026-09-07T11:25:03Z",
				},
			}},
		},
		// Composed resources of a live XR churn through deletion routinely; warning per nested XR
		// would be noise, so only the top-level XR (parentXR == nil) is flagged.
		"DeletingNestedXR_NoWarning": {
			existing: deleting,
			parentXR: cmp.New(),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stderrBuf bytes.Buffer

			warnings := NewWarningLogger(tu.TestLogger(t, false), &stderrBuf)

			processor := &DefaultDiffProcessor{
				config: ProcessorConfig{
					Stderr:   &stderrBuf,
					Logger:   warnings,
					Warnings: warnings,
				},
			}

			processor.warnIfDeleting(tt.existing, tt.parentXR, "XR1/my-xr")

			if diff := gcmp.Diff(tt.wantStderr, stderrBuf.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}

			if diff := gcmp.Diff(tt.wantWarnings, processor.collectedWarnings()); diff != "" {
				t.Errorf("collected warnings mismatch (-want +got):\n%s", diff)
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
					Credential:  &tu.MockCredentialClient{},
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
					Credential:  &tu.MockCredentialClient{},
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
					Credential:  &tu.MockCredentialClient{},
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

func TestDefaultDiffProcessor_RenderToStableState(t *testing.T) {
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
		setupRenderFunc        func() RenderFn
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
			setupRenderFunc: func() RenderFn {
				iteration := 0

				return func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					iteration++
					// Return a simple output with no requirements
					return render.CompositionOutputs{
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
			setupRenderFunc: func() RenderFn {
				iteration := 0

				return func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					iteration++

					// First render includes requirements, second should have no requirements
					var reqs []*v1.ResourceSelector
					if iteration == 1 {
						reqs = []*v1.ResourceSelector{
							{
								ApiVersion: "v1",
								Kind:       ConfigMap,
								Match: &v1.ResourceSelector_MatchName{
									MatchName: ConfigMapName,
								},
							},
						}
					}

					// Return a simple output
					return render.CompositionOutputs{
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
						RequiredResources: reqs,
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
			setupRenderFunc: func() RenderFn {
				iteration := 0

				return func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
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
					var requirements []*v1.ResourceSelector

					if !hasConfig {
						// First iteration - request ConfigMap
						requirements = []*v1.ResourceSelector{
							{
								ApiVersion: "v1",
								Kind:       ConfigMap,
								Match: &v1.ResourceSelector_MatchName{
									MatchName: ConfigMapName,
								},
							},
						}
					} else if !hasSecret {
						// Second iteration - request Secret
						requirements = []*v1.ResourceSelector{
							{
								ApiVersion: "v1",
								Kind:       "Secret",
								Match: &v1.ResourceSelector_MatchName{
									MatchName: "secret1",
								},
							},
						}
					}

					// Return a simple output
					return render.CompositionOutputs{
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
						RequiredResources: requirements,
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
			setupRenderFunc: func() RenderFn {
				return func(context.Context, logging.Logger, RenderInputs) (render.CompositionOutputs, error) {
					return render.CompositionOutputs{}, errors.New("render error")
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
			setupRenderFunc: func() RenderFn {
				iteration := 0

				return func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					iteration++

					// First render has requirements but errors
					if iteration == 1 {
						reqs := []*v1.ResourceSelector{
							{
								ApiVersion: "v1",
								Kind:       ConfigMap,
								Match: &v1.ResourceSelector_MatchName{
									MatchName: ConfigMapName,
								},
							},
						}

						return render.CompositionOutputs{
							RequiredResources: reqs,
						}, errors.New("render error with requirements")
					}

					// Second render succeeds
					return render.CompositionOutputs{
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
		"RenderErrorWithCachedRequirements": {
			// Regression test: render fails with a fatal error and returns requirements,
			// but all requirements are already cached (newReqCount==0). The error must be
			// returned instead of silently dropped. Previously the condition also required
			// len(output.Requirements)==0, which let errors fall through to checkStability
			// and return an output with nil CompositeResource, causing a SIGSEGV.
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
			setupRenderFunc: func() RenderFn {
				reqs := []*v1.ResourceSelector{
					{
						ApiVersion: "v1",
						Kind:       ConfigMap,
						Match: &v1.ResourceSelector_MatchName{
							MatchName: ConfigMapName,
						},
					},
				}

				iteration := 0

				return func(_ context.Context, _ logging.Logger, _ RenderInputs) (render.CompositionOutputs, error) {
					iteration++

					// Every iteration returns the same requirements AND the same error.
					// After iteration 1, the requirement is already cached so newReqCount==0.
					return render.CompositionOutputs{
						RequiredResources: reqs,
					}, errors.New("fatal template error: assignment to entry in nil map")
				}
			},
			wantComposedCount:    0,
			wantRenderIterations: 2,    // First render resolves the requirement, second sees no new requirements
			wantErr:              true, // Must return error, not silently swallow it
		},
		"RequirementsProcessingError": {
			// A transport-level / non-NotFound failure during requirement
			// resolution must still propagate (e.g. RBAC denial, API server
			// unreachable). The NotFound case is exercised in
			// RequirementsNotFoundConverges below.
			xr:          xr,
			composition: composition,
			functions:   functions,
			resourceID:  "XR/test-xr",
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
					).
					WithGetResourceError(errors.New("forbidden: user cannot get configmaps")).
					Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithNoEnvironmentConfigs().
					Build()
			},
			setupRenderFunc: func() RenderFn {
				return func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					reqs := []*v1.ResourceSelector{
						{
							ApiVersion: "v1",
							Kind:       ConfigMap,
							Match: &v1.ResourceSelector_MatchName{
								MatchName: "missing-config",
							},
						},
					}

					return render.CompositionOutputs{
						CompositeResource: in.CompositeResource,
						RequiredResources: reqs,
					}, nil
				}
			},
			wantComposedCount:    0,
			wantRenderIterations: 1,
			wantErr:              true, // Non-NotFound errors still surface.
		},
		"RequirementsNotFoundConverges": {
			// Regression test for crossplane-contrib/crossplane-diff#355: a
			// matchName selector whose target does not exist must NOT abort
			// the diff. The render loop converges on iteration 1 — render
			// emits the selector, ResolveSelectors silently skips the
			// NotFound (mirrors upstream xfn.required_resources.go), no new
			// requirements are accumulated, the stability check fires.
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
			setupRenderFunc: func() RenderFn {
				return func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					reqs := []*v1.ResourceSelector{
						{
							ApiVersion: "v1",
							Kind:       ConfigMap,
							Match: &v1.ResourceSelector_MatchName{
								MatchName: "missing-config",
							},
						},
					}

					return render.CompositionOutputs{
						CompositeResource: in.CompositeResource,
						RequiredResources: reqs,
					}, nil
				}
			},
			wantComposedCount:    0,
			wantRenderIterations: 1,
			wantErr:              false,
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
			setupRenderFunc: func() RenderFn {
				return func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					// Verify observed resources were passed through
					if len(in.ObservedResources) != 2 {
						return render.CompositionOutputs{}, errors.Errorf("expected 2 observed resources, got %d", len(in.ObservedResources))
					}

					// Verify the observed resources have the expected kinds
					observedKinds := make(map[string]bool)
					for _, obs := range in.ObservedResources {
						observedKinds[obs.GetKind()] = true
					}

					if !observedKinds["Bucket"] || !observedKinds["User"] {
						return render.CompositionOutputs{}, errors.New("expected observed resources to include Bucket and User")
					}

					return render.CompositionOutputs{
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
			countingRenderFunc := func(ctx context.Context, log logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
				renderCount++
				return renderFunc(ctx, log, in)
			}

			// Create the requirements provider
			requirementsProvider := NewRequirementsProvider(
				resourceClient,
				environmentClient,
				logger,
			)

			// Build processor options
			baseOpts := testProcessorOptions(t)
			customOpts := []ProcessorOption{
				WithLogger(logger),
				WithRenderFunc(countingRenderFunc),
				WithRequirementsProviderFactory(func(k8.ResourceClient, xp.EnvironmentClient, logging.Logger) *RequirementsProvider {
					return requirementsProvider
				}),
			}
			baseOpts = append(baseOpts, customOpts...)
			processor := NewDiffProcessor(k8.Clients{}, xp.Clients{Definition: tu.NewMockDefinitionClient().Build()}, baseOpts...)

			// Call the method under test
			output, err := processor.(*DefaultDiffProcessor).RenderToStableState(ctx, tt.xr, tt.composition, tt.functions, tt.resourceID, tt.observedResources, false)

			// Check error expectations
			if tt.wantErr {
				if err == nil {
					t.Errorf("RenderToStableState() expected error but got none")
				}

				return
			}

			if err != nil {
				t.Errorf("RenderToStableState() unexpected error: %v", err)
				return
			}

			// Check render iterations
			if renderCount != tt.wantRenderIterations {
				t.Errorf("RenderToStableState() called render func %d times, want %d",
					renderCount, tt.wantRenderIterations)
			}

			// Check composed resource count
			if len(output.ComposedResources) != tt.wantComposedCount {
				t.Errorf("RenderToStableState() returned %d composed resources, want %d",
					len(output.ComposedResources), tt.wantComposedCount)
			}
		})
	}
}

func TestDefaultDiffProcessor_RenderToStableState_SynthesizeReady(t *testing.T) {
	ctx := t.Context()

	// Create test resources
	xr := tu.NewResource("example.org/v1", "XR", "test-xr").BuildUComposite()

	// Create a composition with pipeline mode
	pipelineMode := apiextensionsv1.CompositionModePipeline
	composition := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "test-composition"},
		Spec:       apiextensionsv1.CompositionSpec{Mode: pipelineMode},
	}

	functions := []pkgv1.Function{{ObjectMeta: metav1.ObjectMeta{Name: "test-function"}}}

	tests := map[string]struct {
		setupRenderFunc      func() RenderFn
		wantComposedCount    int
		wantRenderIterations int
		wantErr              bool
		wantErrContains      string
	}{
		"AlreadyStable": {
			setupRenderFunc: func() RenderFn {
				return func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					// Return same resource every time - already stable
					return render.CompositionOutputs{
						CompositeResource: in.CompositeResource,
						ComposedResources: []cpd.Unstructured{{
							Unstructured: un.Unstructured{Object: map[string]any{
								"apiVersion": "example.org/v1",
								"kind":       "ComposedResource",
								"metadata":   map[string]any{"name": "composed1"},
							}},
						}},
					}, nil
				}
			},
			wantComposedCount:    1,
			wantRenderIterations: 2, // First render + verification render
			wantErr:              false,
		},
		"MultiStageProgression": {
			setupRenderFunc: func() RenderFn {
				iteration := 0

				return func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					iteration++

					// Count how many observed resources have Ready=True
					readyCount := 0

					for _, obs := range in.ObservedResources {
						if hasReadyCondition(&obs.Unstructured) {
							readyCount++
						}
					}

					// Stage 1: No ready resources -> produce resource1
					// Stage 2: resource1 is ready -> produce resource1 + resource2
					// Stage 3: resource1,2 ready -> produce resource1 + resource2 + resource3
					// Stage 4: All ready, stable
					resources := []cpd.Unstructured{{
						Unstructured: un.Unstructured{Object: map[string]any{
							"apiVersion": "example.org/v1",
							"kind":       "Stage1Resource",
							"metadata":   map[string]any{"name": "resource1"},
						}},
					}}

					if readyCount >= 1 {
						resources = append(resources, cpd.Unstructured{
							Unstructured: un.Unstructured{Object: map[string]any{
								"apiVersion": "example.org/v1",
								"kind":       "Stage2Resource",
								"metadata":   map[string]any{"name": "resource2"},
							}},
						})
					}

					if readyCount >= 2 {
						resources = append(resources, cpd.Unstructured{
							Unstructured: un.Unstructured{Object: map[string]any{
								"apiVersion": "example.org/v1",
								"kind":       "Stage3Resource",
								"metadata":   map[string]any{"name": "resource3"},
							}},
						})
					}

					return render.CompositionOutputs{
						CompositeResource: in.CompositeResource,
						ComposedResources: resources,
					}, nil
				}
			},
			wantComposedCount:    3, // All three stages rendered
			wantRenderIterations: 4, // Stage1 -> Stage2 -> Stage3 -> verify stable
			wantErr:              false,
		},
		"MaxIterationsExceeded": {
			setupRenderFunc: func() RenderFn {
				resourceNum := 0

				return func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					// Always produce a new resource - never stabilizes
					// Key uses crossplane.io/composition-resource-name annotation
					resourceNum++

					return render.CompositionOutputs{
						CompositeResource: in.CompositeResource,
						ComposedResources: []cpd.Unstructured{{
							Unstructured: un.Unstructured{Object: map[string]any{
								"apiVersion": "example.org/v1",
								"kind":       "InfiniteResource",
								"metadata": map[string]any{
									"name": fmt.Sprintf("resource%d", resourceNum),
									"annotations": map[string]any{
										"crossplane.io/composition-resource-name": fmt.Sprintf("res%d", resourceNum),
									},
								},
							}},
						}},
					}, nil
				}
			},
			wantComposedCount:    0,
			wantRenderIterations: 20, // maxIterations
			wantErr:              true,
			wantErrContains:      "did not stabilize",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger := tu.TestLogger(t, false)
			renderFunc := tt.setupRenderFunc()

			renderCount := 0
			countingRenderFunc := func(ctx context.Context, log logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
				renderCount++
				return renderFunc(ctx, log, in)
			}

			resourceClient := tu.NewMockResourceClient().Build()
			environmentClient := tu.NewMockEnvironmentClient().WithNoEnvironmentConfigs().Build()

			requirementsProvider := NewRequirementsProvider(resourceClient, environmentClient, logger)

			baseOpts := testProcessorOptions(t)
			customOpts := []ProcessorOption{
				WithLogger(logger),
				WithRenderFunc(countingRenderFunc),
				WithRequirementsProviderFactory(func(k8.ResourceClient, xp.EnvironmentClient, logging.Logger) *RequirementsProvider {
					return requirementsProvider
				}),
			}
			baseOpts = append(baseOpts, customOpts...)
			processor := NewDiffProcessor(k8.Clients{}, xp.Clients{Definition: tu.NewMockDefinitionClient().Build()}, baseOpts...)

			// Call with synthesizeReady=true
			output, err := processor.(*DefaultDiffProcessor).RenderToStableState(ctx, xr, composition, functions, "XR/test-xr", nil, true)

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error but got none")
					return
				}

				if tt.wantErrContains != "" && !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("error %q should contain %q", err.Error(), tt.wantErrContains)
				}

				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if renderCount != tt.wantRenderIterations {
				t.Errorf("render called %d times, want %d", renderCount, tt.wantRenderIterations)
			}

			if len(output.ComposedResources) != tt.wantComposedCount {
				t.Errorf("got %d composed resources, want %d", len(output.ComposedResources), tt.wantComposedCount)
			}
		})
	}
}

// hasReadyCondition checks if a resource has a Ready=True condition.
func hasReadyCondition(res *un.Unstructured) bool {
	conditions, found, _ := un.NestedSlice(res.Object, "status", "conditions")
	if !found {
		return false
	}

	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}

		if cond["type"] == "Ready" && cond["status"] == "True" {
			return true
		}
	}

	return false
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

	// Fixtures for the nesting-depth cases below. They form a composition cycle:
	// an XChildResource composes an XCycleResource, which composes an
	// XChildResource, and so on without end.
	const cycleGroup = "nested.example.org"

	cycleBXR := tu.NewResource(cycleGroup+"/v1alpha1", "XCycleResource", "test-cycle-b").
		WithSpecField("childField", "cycle-value").
		WithCompositionResourceName("cycle-xr").
		Build()

	cycleAGVK := schema.GroupVersionKind{Group: cycleGroup, Version: "v1alpha1", Kind: "XChildResource"}
	cycleBGVK := schema.GroupVersionKind{Group: cycleGroup, Version: "v1alpha1", Kind: "XCycleResource"}

	cycleAXRDUn := tu.NewXRD("xchildresources."+cycleGroup, cycleGroup, "XChildResource").
		WithVersion("v1alpha1", true, true).
		BuildAsUnstructured()
	cycleBXRDUn := tu.NewXRD("xcycleresources."+cycleGroup, cycleGroup, "XCycleResource").
		WithVersion("v1alpha1", true, true).
		BuildAsUnstructured()

	cycleACRD := tu.NewCRD("xchildresources."+cycleGroup, cycleGroup, "XChildResource").
		WithListKind("XChildResourceList").
		WithPlural("xchildresources").
		WithSingular("xchildresource").
		WithVersion("v1alpha1", true, true).
		WithStandardSchema("childField").
		Build()
	cycleBCRD := tu.NewCRD("xcycleresources."+cycleGroup, cycleGroup, "XCycleResource").
		WithListKind("XCycleResourceList").
		WithPlural("xcycleresources").
		WithSingular("xcycleresource").
		WithVersion("v1alpha1", true, true).
		WithStandardSchema("childField").
		Build()

	// cycleRenderFunc renders the cycle described above. renderCap breaks the
	// cycle after that many renders so that a processor which fails to bound
	// recursion reports a failed assertion instead of exhausting the goroutine
	// stack and taking down the whole test binary. Passing a renderCap of 0
	// yields a tree exactly one level deep (the XR renders no nested XR).
	cycleRenderFunc := func(renderCap int) func(context.Context, logging.Logger, RenderInputs) (render.CompositionOutputs, error) {
		renders := 0

		return func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
			renders++
			out := render.CompositionOutputs{CompositeResource: in.CompositeResource}

			if renders > renderCap {
				return out, nil
			}

			next := cycleBXR
			if in.CompositeResource.GetKind() == cycleBGVK.Kind {
				next = childXR
			}

			out.ComposedResources = []cpd.Unstructured{{Unstructured: *next}}

			return out, nil
		}
	}

	// cycleClients wires both XR kinds of the cycle so each is recognised as an
	// XR and can have its XRD defaults applied.
	cycleClients := func() (xp.Clients, k8.Clients) {
		functions := []pkgv1.Function{
			{ObjectMeta: metav1.ObjectMeta{Name: "function-go-templating"}},
		}

		xpClients := xp.Clients{
			Composition: tu.NewMockCompositionClient().
				WithComposition(childComposition).
				Build(),
			Credential: &tu.MockCredentialClient{},
			Definition: tu.NewMockDefinitionClient().
				WithXRDForGVK(cycleAGVK, cycleAXRDUn).
				WithXRDForGVK(cycleBGVK, cycleBXRDUn).
				Build(),
			Environment: tu.NewMockEnvironmentClient().
				WithNoEnvironmentConfigs().
				Build(),
			Function: tu.NewMockFunctionClient().
				WithSuccessfulFunctionsFetch(functions).
				Build(),
			ResourceTree: tu.NewMockResourceTreeClient().Build(),
		}

		k8sClients := k8.Clients{
			Apply:    tu.NewMockApplyClient().Build(),
			Resource: tu.NewMockResourceClient().WithResourceNotFound().Build(),
			Schema: tu.NewMockSchemaClient().
				WithFoundCRD(cycleGroup, cycleAGVK.Kind, cycleACRD).
				WithFoundCRD(cycleGroup, cycleBGVK.Kind, cycleBCRD).
				WithGetCRDByName(func(name string) (*extv1.CustomResourceDefinition, error) {
					switch name {
					case cycleACRD.Name:
						return cycleACRD, nil
					case cycleBCRD.Name:
						return cycleBCRD, nil
					default:
						return nil, errors.Errorf("CRD with name %s not found", name)
					}
				}).
				Build(),
			Type: tu.NewMockTypeConverter().Build(),
		}

		return xpClients, k8sClients
	}

	tests := map[string]struct {
		setupMocks        func() (xp.Clients, k8.Clients)
		extraOpts         []ProcessorOption
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
					Credential: &tu.MockCredentialClient{},
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
					Credential: &tu.MockCredentialClient{},
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
					Composition: tu.NewMockCompositionClient().
						WithComposition(childComposition).
						Build(),
					Credential: &tu.MockCredentialClient{},
					Definition: tu.NewMockDefinitionClient().
						WithXRD(childXRD).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithNoEnvironmentConfigs().
						Build(),
					Function: tu.NewMockFunctionClient().
						WithSuccessfulFunctionsFetch(functions).
						Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
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
					Resource: tu.NewMockResourceClient().WithResourceNotFound().Build(),
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
					Credential: &tu.MockCredentialClient{},
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
		// A composition cycle must be bounded by MaxNestedDepth rather than by
		// running out of nested XRs (which never happens). Regression test for
		// the depth counter never being incremented across the recursive
		// re-entry through diffSingleResourceInternal.
		"CyclicCompositionTerminatesWithDepthError": {
			setupMocks: cycleClients,
			extraOpts: []ProcessorOption{
				WithRenderFunc(cycleRenderFunc(64)),
			},
			composedResources: []cpd.Unstructured{
				{Unstructured: *childXR},
			},
			parentResourceID: "XParentResource/test-parent",
			depth:            1,
			wantDiffCount:    0,
			wantErr:          true,
			wantErrContain:   "maximum nesting depth exceeded",
		},
		// --max-nested-depth N means "N levels of nesting below the root", so
		// with N=1 a second level of nesting must be refused.
		"MaxNestedDepthOneRefusesSecondLevel": {
			setupMocks: cycleClients,
			extraOpts: []ProcessorOption{
				WithMaxNestedDepth(1),
				WithRenderFunc(cycleRenderFunc(64)),
			},
			composedResources: []cpd.Unstructured{
				{Unstructured: *childXR},
			},
			parentResourceID: "XParentResource/test-parent",
			depth:            1,
			wantDiffCount:    0,
			wantErr:          true,
			wantErrContain:   "maximum nesting depth exceeded",
		},
		// ...and the boundary is inclusive: N=1 must still process the first
		// level of nesting without complaining that the bound was exceeded.
		"MaxNestedDepthOneAllowsFirstLevel": {
			setupMocks: cycleClients,
			extraOpts: []ProcessorOption{
				WithMaxNestedDepth(1),
				WithRenderFunc(cycleRenderFunc(0)),
			},
			composedResources: []cpd.Unstructured{
				{Unstructured: *childXR},
			},
			parentResourceID: "XParentResource/test-parent",
			depth:            1,
			wantDiffCount:    1,
			wantErr:          false,
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
					Composition: tu.NewMockCompositionClient().
						WithComposition(childComposition).
						Build(),
					Credential: &tu.MockCredentialClient{},
					Definition: tu.NewMockDefinitionClient().
						WithXRD(childXRD).
						WithXRDForXRNotFoundForGVK(schema.GroupVersionKind{
							Group:   "nop.example.org",
							Version: "v1alpha1",
							Kind:    "NopResource",
						}).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithNoEnvironmentConfigs().
						Build(),
					Function: tu.NewMockFunctionClient().
						WithSuccessfulFunctionsFetch(functions).
						Build(),
					ResourceTree: tu.NewMockResourceTreeClient().Build(),
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
					Resource: tu.NewMockResourceClient().WithResourceNotFound().Build(),
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
						"crossplane.io/composite": "parent-xr-abc", // Existing composite label
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
						"crossplane.io/composite": "parent-xr-child-abc123", // Points to existing nested XR
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
					Composition: tu.NewMockCompositionClient().
						WithComposition(childComposition).
						Build(),
					Credential: &tu.MockCredentialClient{},
					Definition: tu.NewMockDefinitionClient().
						WithXRD(childXRD).
						Build(),
					Environment: tu.NewMockEnvironmentClient().
						WithNoEnvironmentConfigs().
						Build(),
					Function: tu.NewMockFunctionClient().
						WithSuccessfulFunctionsFetch(functions).
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
					Resource: tu.NewMockResourceClient().WithResourceNotFound().Build(),
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
			// With identity preservation fix, nested XR maintains its cluster identity
			// so managed resources show as modified rather than removed/added
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
				WithSchemaValidatorFactory(func(k8.SchemaClient, k8.ResourceClient, xp.DefinitionClient, logging.Logger) SchemaValidator {
					return &tu.MockSchemaValidator{
						ValidateResourcesFn: func(context.Context, *un.Unstructured, []cpd.Unstructured) error {
							return nil
						},
					}
				}),
				WithDiffCalculatorFactory(func(k8.ApplyClient, k8.AccessChecker, xp.ResourceTreeClient, ResourceManager, logging.Logger, renderer.DiffOptions, DryRunOn, Defaulter) DiffCalculator {
					return &tu.MockDiffCalculator{
						CalculateNonRemovalDiffsFn: func(_ context.Context, xr *cmp.Unstructured, _ render.CompositionOutputs) (map[string]*dt.ResourceDiff, map[string]bool, error) {
							// Return a simple diff for the XR to make the test pass
							diffs := make(map[string]*dt.ResourceDiff)
							rendered := make(map[string]bool)
							gvk := xr.GroupVersionKind()
							resourceID := gvk.Kind + "/" + xr.GetName()
							diffs[resourceID] = &dt.ResourceDiff{
								Gvk:          gvk,
								ResourceName: xr.GetName(),
								DiffType:     dt.DiffTypeAdded,
							}
							rendered[resourceID] = true

							return diffs, rendered, nil
						},
					}
				}),
			}
			baseOpts = append(baseOpts, customOpts...)
			baseOpts = append(baseOpts, tt.extraOpts...)
			processor := NewDiffProcessor(k8sClients, xpClients, baseOpts...).(*DefaultDiffProcessor)

			// Initialize if needed
			if len(tt.composedResources) > 0 {
				// Mock composition provider that returns a composition
				compositionProvider := func(ctx context.Context, res *un.Unstructured) (types.ResolvedComposition, error) {
					return xpClients.Composition.FindMatchingComposition(ctx, res)
				}

				// Create a mock parent XR (nil is acceptable for tests that don't need observed resources)
				var parentXR *cmp.Unstructured

				// Call the method under test
				var observedResources []cpd.Unstructured

				diffs, _, err := processor.ProcessNestedXRs(ctx, tt.composedResources, compositionProvider, tt.parentResourceID, parentXR, observedResources, tt.depth)

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
	xr := tu.NewResource("example.org/v1", "XR", "test-xr").Build()

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
				resourceTree := tu.NewTreeNode(xr).WithChildren(
					tu.NewTreeNode(observedBucket),
					tu.NewTreeNode(observedUser),
				).Build()

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
					Credential: &tu.MockCredentialClient{},
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
				emptyTree := tu.NewTreeNode(xr).Build()

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
					Credential: &tu.MockCredentialClient{},
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
		"FailedObservedResourceFetchIsFatal": {
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
					Credential: &tu.MockCredentialClient{},
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
			wantObservedCount:    0,
			verifyObservedPassed: true,
			wantErr:              true,
			// The observed-resource fetch is the first thing to touch the tree
			// client, so it is what surfaces the failure.
			wantErrContain: "cannot fetch observed resources for XR",
		},
		// A *transient* failure must be fatal too. Removal detection queries the
		// resource tree a second time, so a failure confined to the observed
		// fetch used to be swallowed entirely: the diff was computed as though
		// the cluster held nothing, reporting every existing composed resource
		// as an addition, with no error and no warning.
		"TransientObservedResourceFetchFailureIsFatal": {
			setupMocks: func() (k8.Clients, xp.Clients) {
				resourceTree := tu.NewTreeNode(xr).WithChildren(
					tu.NewTreeNode(observedBucket),
					tu.NewTreeNode(observedUser),
				).Build()

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

				// Fail only the first tree lookup (the observed-resource fetch);
				// every later one succeeds, as a transient API error would.
				treeCalls := 0

				xpClients := xp.Clients{
					Composition: tu.NewMockCompositionClient().
						WithSuccessfulCompositionMatch(composition).
						Build(),
					Credential: &tu.MockCredentialClient{},
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
							treeCalls++
							if treeCalls == 1 {
								return nil, errors.New("etcdserver: request timed out")
							}

							return resourceTree, nil
						}).
						Build(),
				}

				return k8sClients, xpClients
			},
			wantObservedInRender: true,
			wantObservedCount:    0,
			verifyObservedPassed: true,
			wantErr:              true,
			wantErrContain:       "cannot fetch observed resources for XR",
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
				WithRenderFunc(func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					capturedObserved = in.ObservedResources
					capturedObservedCount = len(in.ObservedResources)

					return render.CompositionOutputs{
						CompositeResource: in.CompositeResource,
						ComposedResources: []cpd.Unstructured{},
					}, nil
				}),
				WithSchemaValidatorFactory(func(k8.SchemaClient, k8.ResourceClient, xp.DefinitionClient, logging.Logger) SchemaValidator {
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
			compositionProvider := func(ctx context.Context, res *un.Unstructured) (types.ResolvedComposition, error) {
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

// TestDefaultDiffProcessor_DiffSingleResource_RevisionRef covers how the revision the composition
// provider resolves reaches render. Crossplane's composite reconciler writes the revision it selects to
// compositionRevisionRef before it composes, creating the ref when there is none, so the render input
// carries that name, at the path the composite's schema puts it. An unknown revision leaves the ref as
// the composite has it.
func TestDefaultDiffProcessor_DiffSingleResource_RevisionRef(t *testing.T) {
	composition := tu.NewComposition("test-composition").
		WithCompositeTypeRef("example.org/v1", "XR").
		WithPipelineMode().
		WithPipelineStep("step1", "function-test", nil).
		Build()

	functions := []pkgv1.Function{{ObjectMeta: metav1.ObjectMeta{Name: "function-test"}}}

	staleRef := map[string]any{"name": "test-composition-0ld0001"}

	tests := map[string]struct {
		reason string
		// scope is the XRD's spec.scope, which decides the composite's schema.
		scope string
		// spec is the composite's spec, in the cluster and as supplied.
		spec         map[string]any
		revisionName string
		// wantSpec is the spec of the composite render is handed.
		wantSpec map[string]any
	}{
		"ModernRefIsOverwritten": {
			reason:       "A ref naming another revision is pointed at the resolved one.",
			scope:        "Namespaced",
			spec:         map[string]any{"crossplane": map[string]any{"compositionRevisionRef": staleRef}},
			revisionName: "test-composition-abc1234",
			wantSpec: map[string]any{"crossplane": map[string]any{
				"compositionRevisionRef": map[string]any{"name": "test-composition-abc1234"},
			}},
		},
		"ModernRefIsCreated": {
			reason:       "A composite with no ref gets one, as the reconciler writes it, under spec.crossplane.",
			scope:        "Cluster",
			spec:         map[string]any{"field": "value"},
			revisionName: "test-composition-abc1234",
			wantSpec: map[string]any{
				"field":      "value",
				"crossplane": map[string]any{"compositionRevisionRef": map[string]any{"name": "test-composition-abc1234"}},
			},
		},
		"LegacyRefIsCreated": {
			reason:       "A legacy composite keeps its Crossplane fields directly under spec, so the ref is created there.",
			scope:        "LegacyCluster",
			spec:         map[string]any{"field": "value"},
			revisionName: "test-composition-abc1234",
			wantSpec: map[string]any{
				"field":                  "value",
				"compositionRevisionRef": map[string]any{"name": "test-composition-abc1234"},
			},
		},
		"UnknownRevisionLeavesTheRefAsItIs": {
			reason:   "With no revision resolved, the composite keeps the ref it has; no name is made up.",
			scope:    "Namespaced",
			spec:     map[string]any{"crossplane": map[string]any{"compositionRevisionRef": staleRef}},
			wantSpec: map[string]any{"crossplane": map[string]any{"compositionRevisionRef": staleRef}},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			xr := tu.NewResource("example.org/v1", "XR", "test-xr").WithSpec(tt.spec).Build()

			xrd := tu.NewXRD("xrs.example.org", "example.org", "XR").
				WithPlural("xrs").
				WithSingular("xr").
				WithVersion("v1", true, true).
				BuildAsUnstructured()
			_ = un.SetNestedField(xrd.Object, tt.scope, "spec", "scope")

			xrCRD := tu.NewCRD("xrs.example.org", "example.org", "XR").
				WithListKind("XRList").
				WithPlural("xrs").
				WithSingular("xr").
				WithVersion("v1", true, true).
				WithStandardSchema("field").
				Build()

			k8sClients := k8.Clients{
				Apply:    tu.NewMockApplyClient().WithSuccessfulDryRun().Build(),
				Resource: tu.NewMockResourceClient().WithResourcesExist(xr).Build(),
				Schema: tu.NewMockSchemaClient().
					WithNoResourcesRequiringCRDs().
					WithSuccessfulCRDFetch(xrCRD).
					WithSuccessfulCRDByNameFetch("xrs.example.org", xrCRD).
					Build(),
				Type: tu.NewMockTypeConverter().Build(),
			}

			xpClients := xp.Clients{
				Composition: tu.NewMockCompositionClient().Build(),
				Credential:  &tu.MockCredentialClient{},
				Definition:  tu.NewMockDefinitionClient().WithXRDForXR(xrd).Build(),
				Environment: tu.NewMockEnvironmentClient().WithNoEnvironmentConfigs().Build(),
				Function:    tu.NewMockFunctionClient().WithSuccessfulFunctionsFetch(functions).Build(),
				ResourceTree: tu.NewMockResourceTreeClient().
					WithGetResourceTree(func(context.Context, *un.Unstructured) (*resource.Resource, error) {
						return tu.NewTreeNode(xr).Build(), nil
					}).
					Build(),
			}

			var rendered map[string]any

			opts := append(testProcessorOptions(t),
				WithRenderFunc(func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
					rendered, _, _ = un.NestedMap(in.CompositeResource.Object, "spec")
					return render.CompositionOutputs{CompositeResource: in.CompositeResource}, nil
				}),
				WithSchemaValidatorFactory(func(k8.SchemaClient, k8.ResourceClient, xp.DefinitionClient, logging.Logger) SchemaValidator {
					return &tu.MockSchemaValidator{
						ValidateResourcesFn: func(context.Context, *un.Unstructured, []cpd.Unstructured) error { return nil },
					}
				}),
				WithDiffCalculatorFactory(NewDiffCalculator),
			)
			processor := NewDiffProcessor(k8sClients, xpClients, opts...)

			if err := processor.Initialize(t.Context()); err != nil {
				t.Fatalf("Initialize(): %v", err)
			}

			provider := func(context.Context, *un.Unstructured) (types.ResolvedComposition, error) {
				return types.ResolvedComposition{Composition: composition, RevisionName: tt.revisionName}, nil
			}

			if _, err := processor.DiffSingleResource(t.Context(), xr, provider); err != nil {
				t.Fatalf("%s\nDiffSingleResource(): unexpected error: %v", tt.reason, err)
			}

			if diff := gcmp.Diff(tt.wantSpec, rendered); diff != "" {
				t.Errorf("%s\nDiffSingleResource(): render input spec (-want +got):\n%s", tt.reason, diff)
			}
		})
	}
}

func TestMergeCredentials(t *testing.T) {
	// Define common test secrets
	var secret1NS1 corev1.Secret
	tu.NewResource("v1", "Secret", "secret1").InNamespace("ns1").BuildTyped(&secret1NS1)

	var secret2NS2 corev1.Secret
	tu.NewResource("v1", "Secret", "secret2").InNamespace("ns2").BuildTyped(&secret2NS2)

	var secret1CLIValue corev1.Secret
	tu.NewResource("v1", "Secret", "secret1").InNamespace("ns1").
		WithData(map[string][]byte{"key": []byte("cli-value")}).
		BuildTyped(&secret1CLIValue)

	var secret1AutoValue corev1.Secret
	tu.NewResource("v1", "Secret", "secret1").InNamespace("ns1").
		WithData(map[string][]byte{"key": []byte("auto-value")}).
		BuildTyped(&secret1AutoValue)

	var cliSecretNS1 corev1.Secret
	tu.NewResource("v1", "Secret", "cli-secret").InNamespace("ns1").BuildTyped(&cliSecretNS1)

	var autoSecretNS2 corev1.Secret
	tu.NewResource("v1", "Secret", "auto-secret").InNamespace("ns2").BuildTyped(&autoSecretNS2)

	var secret1First corev1.Secret
	tu.NewResource("v1", "Secret", "secret1").InNamespace("ns1").
		WithData(map[string][]byte{"key": []byte("first")}).
		BuildTyped(&secret1First)

	var secret1Second corev1.Secret
	tu.NewResource("v1", "Secret", "secret1").InNamespace("ns1").
		WithData(map[string][]byte{"key": []byte("second")}).
		BuildTyped(&secret1Second)

	tests := map[string]struct {
		cliCredentials         []corev1.Secret
		autoFetchedCredentials []corev1.Secret
		want                   map[string]bool // expected namespace/name keys in result
		wantCount              int
	}{
		"EmptyBoth": {
			cliCredentials:         nil,
			autoFetchedCredentials: nil,
			want:                   map[string]bool{},
			wantCount:              0,
		},
		"OnlyCLI": {
			cliCredentials:         []corev1.Secret{secret1NS1},
			autoFetchedCredentials: nil,
			want:                   map[string]bool{"ns1/secret1": true},
			wantCount:              1,
		},
		"OnlyAutoFetched": {
			cliCredentials:         nil,
			autoFetchedCredentials: []corev1.Secret{secret2NS2},
			want:                   map[string]bool{"ns2/secret2": true},
			wantCount:              1,
		},
		"CLIOverridesAutoFetched": {
			cliCredentials:         []corev1.Secret{secret1CLIValue},
			autoFetchedCredentials: []corev1.Secret{secret1AutoValue},
			want:                   map[string]bool{"ns1/secret1": true},
			wantCount:              1,
		},
		"MergesDifferentSecrets": {
			cliCredentials:         []corev1.Secret{cliSecretNS1},
			autoFetchedCredentials: []corev1.Secret{autoSecretNS2},
			want:                   map[string]bool{"ns1/cli-secret": true, "ns2/auto-secret": true},
			wantCount:              2,
		},
		"DuplicatesInCLIInputLastWins": {
			cliCredentials:         []corev1.Secret{secret1First, secret1Second},
			autoFetchedCredentials: nil,
			want:                   map[string]bool{"ns1/secret1": true},
			wantCount:              1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := mergeCredentials(tc.cliCredentials, tc.autoFetchedCredentials)

			if len(got) != tc.wantCount {
				t.Errorf("mergeCredentials() returned %d secrets, want %d", len(got), tc.wantCount)
			}

			for _, secret := range got {
				key := fmt.Sprintf("%s/%s", secret.Namespace, secret.Name)
				if !tc.want[key] {
					t.Errorf("mergeCredentials() unexpected secret %s", key)
				}
			}

			// Test CLI override behavior specifically
			if name == "CLIOverridesAutoFetched" && len(got) == 1 {
				if string(got[0].Data["key"]) != "cli-value" {
					t.Errorf("mergeCredentials() CLI credentials should override auto-fetched, got value %q", string(got[0].Data["key"]))
				}
			}

			// Test that duplicates in CLI input use last-write-wins
			if name == "DuplicatesInCLIInputLastWins" && len(got) == 1 {
				if string(got[0].Data["key"]) != "second" {
					t.Errorf("mergeCredentials() duplicate CLI credentials should use last value, got %q want %q", string(got[0].Data["key"]), "second")
				}
			}
		})
	}
}

// TestResolveFunctionCredentials covers the credential resolution seam: delegation to the
// CredentialClient (whose own fetch logic is tested in credential_client_test.go), the merge with
// CLI-supplied credentials, and — the part that matters to a user — exactly when the shortfall advisory
// fires. The advisory must describe the state AFTER the merge: a user who supplied the absent secret
// with --function-credentials has already solved the problem and must not be told to solve it.
func TestResolveFunctionCredentials(t *testing.T) {
	var clusterSecret corev1.Secret
	tu.NewResource("v1", "Secret", "cluster-credentials").
		InNamespace("crossplane-system").
		BuildTyped(&clusterSecret)

	var suppliedSecret corev1.Secret
	tu.NewResource("v1", "Secret", "absent-credentials").
		InNamespace("crossplane-system").
		BuildTyped(&suppliedSecret)

	absentRef := k8stypes.NamespacedName{Namespace: "crossplane-system", Name: "absent-credentials"}
	otherAbsentRef := k8stypes.NamespacedName{Namespace: "ns2", Name: "other-credentials"}

	composition := tu.NewComposition("test-comp").
		WithCompositeTypeRef("example.org/v1", "XR1").
		WithPipelineMode().
		WithPipelineStep("step1", "function-msgraph", nil,
			tu.WithCredentials("azure-creds", "crossplane-system", "cluster-credentials")).
		Build()

	const (
		advisoryMsg  = "Some function credential secrets could not be fetched from cluster"
		advisoryHint = "Use --function-credentials to provide secrets that don't exist on cluster"
	)

	tests := map[string]struct {
		reason string
		// composition is passed straight through; nil exercises the delegation guard.
		composition *apiextensionsv1.Composition
		// calls is how many times resolveFunctionCredentials runs, standing in for the per-XR repeat:
		// RenderToStableState resolves credentials once per XR, so a composition affecting many XRs
		// resolves many times against identical inputs.
		calls          int
		cliCredentials []corev1.Secret
		fetchResult    types.CredentialFetchResult
		fetchErr       error
		wantMerged     []string
		wantAdvisories []dt.OutputWarning
		wantErr        string
	}{
		"NilCompositionResolvesNothing": {
			reason:      "A nil composition has no pipeline to inspect, so nothing is fetched or warned about",
			composition: nil,
		},
		"DelegatesToCredentialClient": {
			reason:      "Secrets the client fetched from the cluster are returned for rendering",
			composition: composition,
			fetchResult: types.CredentialFetchResult{Secrets: []corev1.Secret{clusterSecret}},
			wantMerged:  []string{"crossplane-system/cluster-credentials"},
		},
		"AbsentAndNotSuppliedWarns": {
			reason:      "A shortfall the user has not covered is exactly what the advisory exists to report",
			composition: composition,
			fetchResult: types.CredentialFetchResult{
				Secrets: []corev1.Secret{clusterSecret},
				Absent:  []k8stypes.NamespacedName{absentRef},
			},
			wantMerged: []string{"crossplane-system/cluster-credentials"},
			wantAdvisories: []dt.OutputWarning{{
				Message: advisoryMsg,
				Context: map[string]string{
					"composition": "test-comp",
					"missing":     "crossplane-system/absent-credentials",
					"hint":        advisoryHint,
				},
			}},
		},
		"AbsentButSuppliedViaCLIDoesNotWarn": {
			reason:         "The user passed --function-credentials for the very secret the cluster lacks; warning would tell them to do what they just did",
			composition:    composition,
			cliCredentials: []corev1.Secret{suppliedSecret},
			fetchResult: types.CredentialFetchResult{
				Secrets: []corev1.Secret{clusterSecret},
				Absent:  []k8stypes.NamespacedName{absentRef},
			},
			wantMerged: []string{"crossplane-system/absent-credentials", "crossplane-system/cluster-credentials"},
		},
		"PartiallySuppliedWarnsOnlyAboutTheRemainder": {
			reason:         "Naming the secrets that are still missing is what makes the advisory actionable",
			composition:    composition,
			cliCredentials: []corev1.Secret{suppliedSecret},
			fetchResult: types.CredentialFetchResult{
				Absent: []k8stypes.NamespacedName{otherAbsentRef, absentRef},
			},
			wantMerged: []string{"crossplane-system/absent-credentials"},
			wantAdvisories: []dt.OutputWarning{{
				Message: advisoryMsg,
				Context: map[string]string{
					"composition": "test-comp",
					"missing":     "ns2/other-credentials",
					"hint":        advisoryHint,
				},
			}},
		},
		"RepeatedResolutionWarnsOnce": {
			reason:      "The shortfall is a property of the composition, so thirty affected XRs must not produce thirty identical warnings",
			composition: composition,
			calls:       3,
			fetchResult: types.CredentialFetchResult{
				Absent: []k8stypes.NamespacedName{absentRef},
			},
			wantAdvisories: []dt.OutputWarning{{
				Message: advisoryMsg,
				Context: map[string]string{
					"composition": "test-comp",
					"missing":     "crossplane-system/absent-credentials",
					"hint":        advisoryHint,
				},
			}},
		},
		"FetchErrorPropagates": {
			reason:      "A credential that could not be read leaves the diff possibly wrong, so the failure must not be degraded to an advisory",
			composition: composition,
			fetchErr:    errors.New("secrets is forbidden"),
			wantErr:     "secrets is forbidden",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			credentialClient := &tu.MockCredentialClient{
				FetchCompositionCredentialsFn: func(_ context.Context, _ *apiextensionsv1.Composition) (types.CredentialFetchResult, error) {
					return tc.fetchResult, tc.fetchErr
				},
			}

			// A real WarningLogger, so the assertion covers the whole advisory channel — message,
			// machine-readable context, and the sink's dedup — rather than just "an Info happened".
			var stderr bytes.Buffer

			warnings := NewWarningLogger(tu.TestLogger(t, false), &stderr)

			processor := &DefaultDiffProcessor{
				credentialClient: credentialClient,
				config: ProcessorConfig{
					Logger:              warnings,
					FunctionCredentials: tc.cliCredentials,
				},
			}

			calls := max(tc.calls, 1)

			var (
				merged []corev1.Secret
				err    error
			)

			for range calls {
				merged, err = processor.resolveFunctionCredentials(t.Context(), tc.composition, "XR1/my-xr")
			}

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("\n%s\nresolveFunctionCredentials(): expected error containing %q, got nil", tc.reason, tc.wantErr)
				}

				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("\n%s\nresolveFunctionCredentials(): error %q does not contain %q", tc.reason, err.Error(), tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("\n%s\nresolveFunctionCredentials(): unexpected error: %v", tc.reason, err)
			}

			gotMerged := make([]string, 0, len(merged))
			for _, s := range merged {
				gotMerged = append(gotMerged, s.Namespace+"/"+s.Name)
			}

			if diff := gcmp.Diff(tc.wantMerged, gotMerged, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("\n%s\nmerged credentials mismatch (-want +got):\n%s", tc.reason, diff)
			}

			if diff := gcmp.Diff(tc.wantAdvisories, warnings.Warnings()); diff != "" {
				t.Errorf("\n%s\nadvisories mismatch (-want +got):\n%s", tc.reason, diff)
			}
		})
	}
}

// TestDefaultDiffProcessor_RenderToStableState_SchemaPlumbing asserts that
// resolveSchemaAndXRDForRender pins the right composite.Schema on the input
// *cmp.Unstructured the render function receives. Schema selection follows
// the XRD's spec.scope: LegacyCluster -> SchemaLegacy (canonical fields at
// spec.*); anything else -> SchemaModern (canonical fields at
// spec.crossplane.*). Pinning here is required so the renderer writes
// canonical fields at the path the cluster CRD declares.
func TestDefaultDiffProcessor_RenderToStableState_SchemaPlumbing(t *testing.T) {
	ctx := t.Context()

	xr := tu.NewResource("example.org/v1", "XLegacy", "test-xr").BuildUComposite()
	composition := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "test-composition"},
		Spec:       apiextensionsv1.CompositionSpec{Mode: apiextensionsv1.CompositionModePipeline},
	}

	// resolveSchemaAndXRDForRender derives the schema from the XRD's
	// spec.scope (LegacyCluster -> SchemaLegacy; anything else ->
	// SchemaModern), so this test drives the schema decision via the
	// XRD's scope field rather than mocking GetCompositeSchema.
	tests := map[string]struct {
		scope      string
		wantSchema cmp.Schema
	}{
		"LegacyXRD_SchemaLegacy": {
			scope:      "LegacyCluster",
			wantSchema: cmp.SchemaLegacy,
		},
		"ModernXRD_SchemaModern": {
			scope:      "Cluster",
			wantSchema: cmp.SchemaModern,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var capturedSchema cmp.Schema

			renderFn := func(_ context.Context, _ logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
				capturedSchema = in.CompositeResource.Schema
				return render.CompositionOutputs{CompositeResource: in.CompositeResource}, nil
			}

			xrd := tu.NewResource("apiextensions.crossplane.io/v2", "CompositeResourceDefinition", "xrd-test").
				WithSpecField("scope", tt.scope).
				Build()

			defClient := tu.NewMockDefinitionClient().Build()
			defClient.GetXRDForXRFn = func(_ context.Context, _ schema.GroupVersionKind) (*un.Unstructured, error) {
				return xrd, nil
			}

			opts := append(testProcessorOptions(t),
				WithRenderFunc(renderFn),
			)
			processor := NewDiffProcessor(k8.Clients{}, xp.Clients{Definition: defClient}, opts...)

			out, err := processor.(*DefaultDiffProcessor).RenderToStableState(
				ctx, xr, composition, nil, "XR/test-xr", nil, false,
			)
			if err != nil {
				t.Fatalf("RenderToStableState() unexpected error: %v", err)
			}

			if capturedSchema != tt.wantSchema {
				t.Errorf("input.CompositeResource.Schema = %v, want %v", capturedSchema, tt.wantSchema)
			}

			if out.CompositeResource == nil {
				t.Fatal("output CompositeResource is nil")
			}

			if out.CompositeResource.Schema != tt.wantSchema {
				t.Errorf("output.CompositeResource.Schema = %v, want %v",
					out.CompositeResource.Schema, tt.wantSchema)
			}
		})
	}
}

// recordingCleaner records the context Cleanup was called with, and that context's Err() at call time.
type recordingCleaner struct {
	err       error
	called    bool
	ctx       context.Context
	ctxErrNow error
}

func (r *recordingCleaner) Cleanup(ctx context.Context) error {
	r.called = true
	r.ctx = ctx
	r.ctxErrNow = ctx.Err()

	return r.err
}

type cleanupDetachedCtxKey struct{}

func TestCleanupDetached(t *testing.T) {
	type want struct {
		CtxErr error
		Value  any
	}

	tests := map[string]struct {
		reason        string
		parent        func() (context.Context, context.CancelFunc)
		cleanErr      error
		want          want
		checkDeadline bool
	}{
		"ParentCancelled": {
			reason: "Cleanup must receive a live context even when the run context is already cancelled.",
			parent: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				return ctx, cancel
			},
		},
		"ParentDeadlineExpired": {
			reason: "Cleanup must receive a live context even when the run's --timeout has expired.",
			parent: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
		},
		"KeepsParentValues": {
			reason: "Cleanup's context must keep the run context's values.",
			parent: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.WithValue(context.Background(), cleanupDetachedCtxKey{}, "v"))
			},
			want: want{Value: "v"},
		},
		"BoundedByCleanupTimeout": {
			reason: "Cleanup's context must carry a deadline no later than CleanupTimeout from now.",
			parent: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			checkDeadline: true,
		},
		"ErrorIsSwallowed": {
			reason: "A cleanup error must be logged, not panic or propagate.",
			parent: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			cleanErr: errors.New("boom"),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			parent, cancel := tt.parent()
			defer cancel()

			c := &recordingCleaner{err: tt.cleanErr}

			CleanupDetached(parent, c, logging.NewNopLogger())

			if !c.called {
				t.Fatalf("\n%s\nCleanupDetached(...): Cleanup was not called", tt.reason)
			}

			got := want{CtxErr: c.ctxErrNow, Value: c.ctx.Value(cleanupDetachedCtxKey{})}
			if diff := gcmp.Diff(tt.want, got, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("\n%s\nCleanupDetached(...): -want, +got:\n%s", tt.reason, diff)
			}

			if tt.checkDeadline {
				dl, ok := c.ctx.Deadline()
				if !ok {
					t.Fatalf("\n%s\nCleanupDetached(...): cleanup context has no deadline", tt.reason)
				}

				if limit := time.Now().Add(CleanupTimeout); dl.After(limit) {
					t.Errorf("\n%s\nCleanupDetached(...): deadline %v is later than %v", tt.reason, dl, limit)
				}
			}
		})
	}
}
