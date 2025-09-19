package diffprocessor

import (
	"context"
	"strings"
	"testing"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
)

var _ SchemaValidator = (*tu.MockSchemaValidator)(nil)

func TestDefaultSchemaValidator_ValidateResources(t *testing.T) {
	ctx := t.Context()

	// Create a sample XR and cpd resources for validation
	xr := tu.NewResource("example.org/v1", "XR", "test-xr").
		InNamespace("default").
		WithSpecField("field", "value").
		Build()

	composedResource1 := tu.NewResource("cpd.org/v1", "ComposedResource", "resource1").
		InNamespace("default").
		WithCompositeOwner("test-xr").
		WithCompositionResourceName("resource1").
		WithSpecField("field", "value").
		BuildUComposed()

	composedResource2 := tu.NewResource("cpd.org/v1", "ComposedResource", "resource2").
		InNamespace("default").
		WithCompositeOwner("test-xr").
		WithCompositionResourceName("resource2").
		WithSpecField("field", "value").
		BuildUComposed()

	// Create sample CRDs for validation
	xrCRD := makeCRD("xrs.example.org", "XR", "example.org", "v1")
	composedCRD := makeCRD("composedresources.cpd.org", "ComposedResource", "cpd.org", "v1")

	tests := map[string]struct {
		setupClients   func() (*tu.MockSchemaClient, *tu.MockDefinitionClient)
		xr             *un.Unstructured
		composed       []cpd.Unstructured
		preloadedCRDs  []*extv1.CustomResourceDefinition
		expectedErr    bool
		expectedErrMsg string
	}{
		"SuccessfulValidationWithPreloadedCRDs": {
			setupClients: func() (*tu.MockSchemaClient, *tu.MockDefinitionClient) {
				return tu.NewMockSchemaClient().Build(), tu.NewMockDefinitionClient().Build()
			},
			xr:            xr,
			composed:      []cpd.Unstructured{*composedResource1, *composedResource2},
			preloadedCRDs: []*extv1.CustomResourceDefinition{xrCRD, composedCRD},
			expectedErr:   false,
		},
		"SuccessfulValidationWithFetchedCRDs": {
			setupClients: func() (*tu.MockSchemaClient, *tu.MockDefinitionClient) {
				// Convert CRDs to unstructured for the mock client
				xrCRDUn := &un.Unstructured{}
				_ = runtime.DefaultUnstructuredConverter.FromUnstructured(
					MustToUnstructured(xrCRD),
					xrCRDUn,
				)

				composedCRDUn := &un.Unstructured{}
				_ = runtime.DefaultUnstructuredConverter.FromUnstructured(
					MustToUnstructured(composedCRD),
					composedCRDUn,
				)

				sch := tu.NewMockSchemaClient().
					// Add GetCRD implementation
					WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
						if gvk.Group == "example.org" && gvk.Kind == "XR" {
							return xrCRDUn, nil
						}
						if gvk.Group == "cpd.org" && gvk.Kind == "ComposedResource" {
							return composedCRDUn, nil
						}
						return nil, errors.New("CRD not found")
					}).
					// Implement IsCRDRequired to return true for our test resources
					WithAllResourcesRequiringCRDs().
					Build()
				def := tu.NewMockDefinitionClient().
					WithSuccessfulXRDsFetch([]*un.Unstructured{}).
					Build()
				return sch, def
			},
			xr:            xr,
			composed:      []cpd.Unstructured{*composedResource1, *composedResource2},
			preloadedCRDs: []*extv1.CustomResourceDefinition{},
			expectedErr:   false,
		},
		"MissingCRD": {
			setupClients: func() (*tu.MockSchemaClient, *tu.MockDefinitionClient) {
				// Only provide the XR CRD, not the cpd resource CRD
				xrCRDUn := &un.Unstructured{}
				_ = runtime.DefaultUnstructuredConverter.FromUnstructured(
					MustToUnstructured(xrCRD),
					xrCRDUn,
				)

				sch := tu.NewMockSchemaClient().
					// Add GetCRD implementation
					WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
						if gvk.Group == "example.org" && gvk.Kind == "XR" {
							return xrCRDUn, nil
						}
						// Return not found for cpd resource CRD
						return nil, errors.New("CRD not found")
					}).
					// Add this line to make only Composed resources require CRDs:
					WithResourcesRequiringCRDs(
						schema.GroupVersionKind{Group: "cpd.org", Version: "v1", Kind: "ComposedResource"},
					).
					Build()
				def := tu.NewMockDefinitionClient().
					WithSuccessfulXRDsFetch([]*un.Unstructured{}).
					Build()
				return sch, def
			},
			xr:            xr,
			composed:      []cpd.Unstructured{*composedResource1, *composedResource2},
			preloadedCRDs: []*extv1.CustomResourceDefinition{},
			// Now we expect an error because we've configured it to require a CRD but can't find it
			expectedErr:    true,
			expectedErrMsg: "unable to find CRD for cpd.org/v1, Kind=ComposedResource",
		},
		"ValidationError": {
			setupClients: func() (*tu.MockSchemaClient, *tu.MockDefinitionClient) {
				// Convert CRDs to un for the mock
				composedCRDUn := &un.Unstructured{}
				_ = runtime.DefaultUnstructuredConverter.FromUnstructured(
					MustToUnstructured(createCRDWithStringField(composedCRD)),
					composedCRDUn,
				)

				sch := tu.NewMockSchemaClient().
					// Add GetCRD implementation
					WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
						if gvk.Group == "example.org" && gvk.Kind == "XR" {
							return nil, errors.New("CRD not found") // Force validation to use preloaded CRDs
						}
						if gvk.Group == "cpd.org" && gvk.Kind == "ComposedResource" {
							return composedCRDUn, nil
						}
						return nil, errors.New("CRD not found")
					}).
					// Setup IsCRDRequired to return true for our test resources
					WithAllResourcesRequiringCRDs().
					Build()

				def := tu.NewMockDefinitionClient().Build()
				return sch, def
			},
			xr: tu.NewResource("example.org/v1", "XR", "test-xr").
				InNamespace("default").
				WithSpecField("field", int64(123)).
				Build(),
			composed:       []cpd.Unstructured{*composedResource1, *composedResource2},
			preloadedCRDs:  []*extv1.CustomResourceDefinition{createCRDWithStringField(xrCRD)},
			expectedErr:    true,
			expectedErrMsg: "schema validation failed",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			schemaClient, defClient := tt.setupClients()
			logger := tu.TestLogger(t, false)

			// Create the schema validator
			validator := NewSchemaValidator(schemaClient, defClient, logger)

			// Set any preloaded CRDs
			if len(tt.preloadedCRDs) > 0 {
				validator.(*DefaultSchemaValidator).SetCRDs(tt.preloadedCRDs)
			}

			// Call the function under test
			err := validator.ValidateResources(ctx, tt.xr, tt.composed)

			// Check error expectations
			if tt.expectedErr {
				if err == nil {
					t.Errorf("ValidateResources() expected error but got none")
					return
				}

				if tt.expectedErrMsg != "" && !strings.Contains(err.Error(), tt.expectedErrMsg) {
					t.Errorf("ValidateResources() error %q doesn't contain expected message %q",
						err.Error(), tt.expectedErrMsg)
				}

				return
			}

			if err != nil {
				t.Errorf("ValidateResources() unexpected error: %v", err)
			}
		})
	}
}

func TestDefaultSchemaValidator_EnsureComposedResourceCRDs(t *testing.T) {
	ctx := t.Context()

	// Create sample resources
	xr := tu.NewResource("example.org/v1", "XR", "test-xr").InNamespace("default").Build()
	cmpd := tu.NewResource("cpd.org/v1", "ComposedResource", "resource1").InNamespace("default").Build()

	// Create sample CRDs
	xrCRD := makeCRD("xrs.example.org", "XR", "example.org", "v1")
	composedCRD := makeCRD("composedresources.cpd.org", "ComposedResource", "cpd.org", "v1")

	tests := map[string]struct {
		setupClient    func() *tu.MockSchemaClient
		initialCRDs    []*extv1.CustomResourceDefinition
		resources      []*un.Unstructured
		expectedCRDLen int
	}{
		"AllCRDsAlreadyCached": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().Build()
			},
			initialCRDs:    []*extv1.CustomResourceDefinition{xrCRD, composedCRD},
			resources:      []*un.Unstructured{xr, cmpd},
			expectedCRDLen: 2, // No change, all CRDs already cached
		},
		"FetchMissingCRDs": {
			setupClient: func() *tu.MockSchemaClient {
				// Convert the cpd CRD to un for the mock
				composedCRDUn := &un.Unstructured{}
				_ = runtime.DefaultUnstructuredConverter.FromUnstructured(
					MustToUnstructured(composedCRD),
					composedCRDUn,
				)

				return tu.NewMockSchemaClient().
					// Use the new GetCRD method instead of GetResource
					WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
						if gvk.Group == "cpd.org" && gvk.Kind == "ComposedResource" {
							return composedCRDUn, nil
						}
						return nil, errors.New("CRD not found")
					}).
					// Make sure cpd resources require CRDs
					WithResourcesRequiringCRDs(
						schema.GroupVersionKind{Group: "cpd.org", Version: "v1", Kind: "ComposedResource"},
					).
					Build()
			},
			initialCRDs:    []*extv1.CustomResourceDefinition{xrCRD}, // Only XR CRD is cached
			resources:      []*un.Unstructured{xr, cmpd},
			expectedCRDLen: 2, // Should fetch the missing cpd CRD
		},
		"SomeCRDsMissing": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithCRDNotFound().
					Build()
			},
			initialCRDs:    []*extv1.CustomResourceDefinition{xrCRD}, // Only XR CRD is cached
			resources:      []*un.Unstructured{xr, cmpd},
			expectedCRDLen: 1, // Still only has the initial XR CRD
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			schemaClient := tt.setupClient()
			logger := tu.TestLogger(t, false)

			// Create the schema validator with initial CRDs
			validator := NewSchemaValidator(schemaClient, tu.NewMockDefinitionClient().Build(), logger)
			validator.(*DefaultSchemaValidator).SetCRDs(tt.initialCRDs)

			// Call the function under test
			_ = validator.(*DefaultSchemaValidator).EnsureComposedResourceCRDs(ctx, tt.resources)

			// Verify the CRD count
			crds := validator.(*DefaultSchemaValidator).GetCRDs()
			if len(crds) != tt.expectedCRDLen {
				t.Errorf("EnsureComposedResourceCRDs() resulted in %d CRDs, want %d",
					len(crds), tt.expectedCRDLen)
			}
		})
	}
}

func TestDefaultSchemaValidator_LoadCRDs(t *testing.T) {
	ctx := t.Context()

	// Create sample CRDs as un
	xrdUn := tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "xrd1").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]interface{}{
			"kind":     "XR",
			"plural":   "xrs",
			"singular": "xr",
		}).
		Build()

	tests := map[string]struct {
		setupClient    func() xp.DefinitionClient
		preloadedCRDs  []*extv1.CustomResourceDefinition
		expectedErr    bool
		expectedErrMsg string
		// for caching tests
		callTwice      bool // Test making two calls to LoadCRDs
		expectXRDCalls int  // Expected number of calls to GetXRDs
	}{
		"SuccessfulLoad": {
			setupClient: func() xp.DefinitionClient {
				return tu.NewMockDefinitionClient().
					WithSuccessfulXRDsFetch([]*un.Unstructured{xrdUn}).
					Build()
			},
			expectedErr: false,
		},
		"XRDFetchError": {
			setupClient: func() xp.DefinitionClient {
				return tu.NewMockDefinitionClient().
					WithFailedXRDsFetch("failed to fetch XRDs").
					Build()
			},
			expectedErr: true,
		},
		"UsesCachedXRDs": {
			setupClient: func() xp.DefinitionClient {
				// Create a tracking client that counts GetXRDs calls
				return &xrdCountingClient{
					MockDefinitionClient: *tu.NewMockDefinitionClient().
						WithSuccessfulXRDsFetch([]*un.Unstructured{xrdUn}).
						Build(),
				}
			},
			preloadedCRDs:  nil, // No preloaded CRDs
			expectedErr:    false,
			callTwice:      true, // Make two calls to LoadCRDs
			expectXRDCalls: 1,    // GetXRDs should only be called once due to caching
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defClient := tt.setupClient()
			logger := tu.TestLogger(t, false)

			// Create the schema validator
			validator := NewSchemaValidator(tu.NewMockSchemaClient().Build(), defClient, logger)

			// Call the function under test
			err := validator.(*DefaultSchemaValidator).LoadCRDs(ctx)

			// Check error expectations
			if tt.expectedErr {
				if err == nil {
					t.Errorf("LoadCRDs() expected error but got none")
				}

				return
			}

			if err != nil {
				t.Errorf("LoadCRDs() unexpected error: %v", err)
				return
			}

			// Verify CRDs were loaded (for successful case)
			crds := validator.(*DefaultSchemaValidator).GetCRDs()
			if len(crds) == 0 {
				t.Errorf("LoadCRDs() did not load any CRDs")
			}
		})
	}
}

// Helper function to create a simple CRD.
func makeCRD(name string, kind string, group string, version string) *extv1.CustomResourceDefinition {
	return &extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: extv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: extv1.CustomResourceDefinitionNames{
				Kind:     kind,
				ListKind: kind + "List",
				Plural:   strings.ToLower(kind) + "s",
				Singular: strings.ToLower(kind),
			},
			Scope: extv1.NamespaceScoped,
			Versions: []extv1.CustomResourceDefinitionVersion{
				{
					Name:    version,
					Served:  true,
					Storage: true,
					Schema: &extv1.CustomResourceValidation{
						OpenAPIV3Schema: &extv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]extv1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]extv1.JSONSchemaProps{
										"field": {
											Type: "string",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// Create a CRD with a string field validation.
func createCRDWithStringField(baseCRD *extv1.CustomResourceDefinition) *extv1.CustomResourceDefinition {
	crd := baseCRD.DeepCopy()
	// Ensure the schema requires 'field' to be a string
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["field"] = extv1.JSONSchemaProps{
		Type: "string",
	}

	return crd
}

// Helper function to convert to un.
func MustToUnstructured(obj interface{}) map[string]interface{} {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		panic(err)
	}

	return u
}

// Helper type to track GetXRDs calls.
type xrdCountingClient struct {
	tu.MockDefinitionClient

	getXRDsCallCount int
}

// Override GetXRDs to count calls.
func (c *xrdCountingClient) GetXRDs(ctx context.Context) ([]*un.Unstructured, error) {
	c.getXRDsCallCount++
	return c.MockDefinitionClient.GetXRDs(ctx)
}

func TestDefaultSchemaValidator_ValidateScopeConstraints(t *testing.T) {
	ctx := t.Context()

	// Create CRDs with different scopes
	namespacedCRD := makeCRD("namespacedresources.example.org", "NamespacedResource", "example.org", "v1")
	namespacedCRD.Spec.Scope = extv1.NamespaceScoped

	clusterCRD := makeCRD("clusterresources.example.org", "ClusterResource", "example.org", "v1")
	clusterCRD.Spec.Scope = extv1.ClusterScoped

	tests := map[string]struct {
		setupClient       func() *tu.MockSchemaClient
		preloadedCRDs     []*extv1.CustomResourceDefinition
		resource          *un.Unstructured
		expectedNamespace string
		isClaimRoot       bool
		expectedErr       bool
		expectedErrMsg    string
	}{
		"NamespacedResourceValidNamespace": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().Build()
			},
			preloadedCRDs: []*extv1.CustomResourceDefinition{namespacedCRD},
			resource: tu.NewResource("example.org/v1", "NamespacedResource", "test-resource").
				InNamespace("default").
				Build(),
			expectedNamespace: "default",
			isClaimRoot:       false,
			expectedErr:       false,
		},
		"NamespacedResourceMissingNamespace": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().Build()
			},
			preloadedCRDs: []*extv1.CustomResourceDefinition{namespacedCRD},
			resource: tu.NewResource("example.org/v1", "NamespacedResource", "test-resource").
				Build(), // No namespace
			expectedNamespace: "default",
			isClaimRoot:       false,
			expectedErr:       true,
			expectedErrMsg:    "namespaced resource NamespacedResource/test-resource must have a namespace",
		},
		"NamespacedResourceWrongNamespace": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().Build()
			},
			preloadedCRDs: []*extv1.CustomResourceDefinition{namespacedCRD},
			resource: tu.NewResource("example.org/v1", "NamespacedResource", "test-resource").
				InNamespace("wrong").
				Build(),
			expectedNamespace: "default",
			isClaimRoot:       false,
			expectedErr:       true,
			expectedErrMsg:    "cross-namespace references not supported",
		},
		"ClusterResourceValidNoNamespace": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().Build()
			},
			preloadedCRDs: []*extv1.CustomResourceDefinition{clusterCRD},
			resource: tu.NewResource("example.org/v1", "ClusterResource", "test-resource").
				Build(), // No namespace - correct for cluster-scoped
			expectedNamespace: "",
			isClaimRoot:       false,
			expectedErr:       false,
		},
		"ClusterResourceInvalidNamespace": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().Build()
			},
			preloadedCRDs: []*extv1.CustomResourceDefinition{clusterCRD},
			resource: tu.NewResource("example.org/v1", "ClusterResource", "test-resource").
				InNamespace("default").
				Build(),
			expectedNamespace: "",
			isClaimRoot:       false,
			expectedErr:       true,
			expectedErrMsg:    "cluster-scoped resource ClusterResource/test-resource cannot have a namespace",
		},
		"ClusterResourceFromNamespacedXR": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().Build()
			},
			preloadedCRDs: []*extv1.CustomResourceDefinition{clusterCRD},
			resource: tu.NewResource("example.org/v1", "ClusterResource", "test-resource").
				Build(),
			expectedNamespace: "default", // XR is namespaced
			isClaimRoot:       false,
			expectedErr:       true,
			expectedErrMsg:    "namespaced XR cannot own cluster-scoped managed resource",
		},
		"ClusterResourceFromNamespacedClaim": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().Build()
			},
			preloadedCRDs: []*extv1.CustomResourceDefinition{clusterCRD},
			resource: tu.NewResource("example.org/v1", "ClusterResource", "test-resource").
				Build(),
			expectedNamespace: "default", // Claim is namespaced
			isClaimRoot:       true,      // But it's a claim, so allowed
			expectedErr:       false,
		},
		"CRDNotFound": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithGetCRD(func(_ context.Context, _ schema.GroupVersionKind) (*un.Unstructured, error) {
						return nil, errors.New("CRD not found")
					}).
					Build()
			},
			preloadedCRDs: []*extv1.CustomResourceDefinition{},
			resource: tu.NewResource("example.org/v1", "UnknownResource", "test-resource").
				Build(),
			expectedNamespace: "default",
			isClaimRoot:       false,
			expectedErr:       true,
			expectedErrMsg:    "cannot determine scope",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			schemaClient := tt.setupClient()
			logger := tu.TestLogger(t, false)

			// Create the schema validator
			validator := NewSchemaValidator(schemaClient, tu.NewMockDefinitionClient().Build(), logger)
			validator.(*DefaultSchemaValidator).SetCRDs(tt.preloadedCRDs)

			// Call the function under test
			err := validator.ValidateScopeConstraints(ctx, tt.resource, tt.expectedNamespace, tt.isClaimRoot)

			// Check error expectations
			if tt.expectedErr {
				if err == nil {
					t.Errorf("ValidateScopeConstraints() expected error but got none")
					return
				}

				if tt.expectedErrMsg != "" && !strings.Contains(err.Error(), tt.expectedErrMsg) {
					t.Errorf("ValidateScopeConstraints() error %q doesn't contain expected message %q",
						err.Error(), tt.expectedErrMsg)
				}

				return
			}

			if err != nil {
				t.Errorf("ValidateScopeConstraints() unexpected error: %v", err)
			}
		})
	}
}
