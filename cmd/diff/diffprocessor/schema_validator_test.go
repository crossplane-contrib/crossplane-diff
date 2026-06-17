package diffprocessor

import (
	"context"
	"strings"
	"testing"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	pkgvalidate "github.com/crossplane/cli/v2/pkg/validate"
	"github.com/google/go-cmp/cmp"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
)

var _ SchemaValidator = (*tu.MockSchemaValidator)(nil)

const (
	testExampleOrg = "example.org"
	testCpdOrg     = "cpd.org"
)

func TestDefaultSchemaValidator_ValidateResources(t *testing.T) {
	ctx := t.Context()

	// Create a sample XR and cpd resources for validation
	xr := tu.NewResource(testExampleOrg+"/v1", "XR", "test-xr").
		InNamespace("default").
		WithSpecField("field", "value").
		Build()

	composedResource1 := tu.NewResource(testCpdOrg+"/v1", "testComposedResource", "resource1").
		InNamespace("default").
		WithCompositeOwner("test-xr").
		WithCompositionResourceName("resource1").
		WithSpecField("field", "value").
		BuildUComposed()

	composedResource2 := tu.NewResource(testCpdOrg+"/v1", "testComposedResource", "resource2").
		InNamespace("default").
		WithCompositeOwner("test-xr").
		WithCompositionResourceName("resource2").
		WithSpecField("field", "value").
		BuildUComposed()

	// Create sample CRDs for validation
	xrCRD := makeCRD("xrs."+testExampleOrg, "XR", testExampleOrg, "v1")
	composedCRD := makeCRD("testComposedResources."+testCpdOrg, "testComposedResource", testCpdOrg, "v1")

	tests := map[string]struct {
		setupClients   func() (*tu.MockSchemaClient, *tu.MockDefinitionClient)
		xr             *un.Unstructured
		composed       []cpd.Unstructured
		expectedErr    bool
		expectedErrMsg string
	}{
		"SuccessfulValidationWithPreloadedCRDs": {
			setupClients: func() (*tu.MockSchemaClient, *tu.MockDefinitionClient) {
				sch := tu.NewMockSchemaClient().
					WithFoundCRDs(map[schema.GroupKind]*extv1.CustomResourceDefinition{
						{Group: testExampleOrg, Kind: "XR"}:               xrCRD,
						{Group: testCpdOrg, Kind: "testComposedResource"}: composedCRD,
					}).
					WithAllResourcesRequiringCRDs().
					WithCachingBehavior().
					Build()

				return sch, tu.NewMockDefinitionClient().Build()
			},
			xr:          xr,
			composed:    []cpd.Unstructured{*composedResource1, *composedResource2},
			expectedErr: false,
		},
		"SuccessfulValidationWithFetchedCRDs": {
			setupClients: func() (*tu.MockSchemaClient, *tu.MockDefinitionClient) {
				sch := tu.NewMockSchemaClient().
					// Add GetCRD implementation for typed CRDs
					WithFoundCRDs(map[schema.GroupKind]*extv1.CustomResourceDefinition{
						{Group: testExampleOrg, Kind: "XR"}:               xrCRD,
						{Group: testCpdOrg, Kind: "testComposedResource"}: composedCRD,
					}).
					// Implement IsCRDRequired to return true for our test resources
					WithAllResourcesRequiringCRDs().
					WithCachingBehavior().
					Build()
				def := tu.NewMockDefinitionClient().
					WithEmptyXRDsFetch().
					Build()

				return sch, def
			},
			xr:          xr,
			composed:    []cpd.Unstructured{*composedResource1, *composedResource2},
			expectedErr: false,
		},
		"MissingCRD": {
			setupClients: func() (*tu.MockSchemaClient, *tu.MockDefinitionClient) {
				sch := tu.NewMockSchemaClient().
					// Add GetCRD implementation for typed CRDs
					WithFoundCRD(testExampleOrg, "XR", xrCRD).
					// Add this line to make only Composed resources require CRDs:
					WithResourcesRequiringCRDs(
						schema.GroupVersionKind{Group: testCpdOrg, Version: "v1", Kind: "testComposedResource"},
					).
					WithCachingBehavior().
					Build()
				def := tu.NewMockDefinitionClient().
					WithEmptyXRDsFetch().
					Build()

				return sch, def
			},
			xr:       xr,
			composed: []cpd.Unstructured{*composedResource1, *composedResource2},
			// Now we expect an error because we've configured it to require a CRD but can't find it
			expectedErr:    true,
			expectedErrMsg: "unable to find CRDs for",
		},
		"ValidationError": {
			setupClients: func() (*tu.MockSchemaClient, *tu.MockDefinitionClient) {
				sch := tu.NewMockSchemaClient().
					// Add GetCRD implementation for typed CRDs
					WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
						if gvk.Group == testExampleOrg && gvk.Kind == "XR" {
							return createCRDWithStringField(xrCRD), nil
						}

						if gvk.Group == testCpdOrg && gvk.Kind == "testComposedResource" {
							return composedCRD, nil
						}

						return nil, errors.New("CRD not found")
					}).
					// Setup IsCRDRequired to return true for our test resources
					WithAllResourcesRequiringCRDs().
					Build()

				def := tu.NewMockDefinitionClient().Build()

				return sch, def
			},
			xr: tu.NewResource(testExampleOrg+"/v1", "XR", "test-xr").
				InNamespace("default").
				WithSpecField("field", int64(123)).
				Build(),
			composed:       []cpd.Unstructured{*composedResource1, *composedResource2},
			expectedErr:    true,
			expectedErrMsg: "missing schema",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			schemaClient, defClient := tt.setupClients()
			logger := tu.TestLogger(t, false)

			// Create the schema validator
			validator := NewSchemaValidator(schemaClient, defClient, logger)

			// CRDs are now provided via mock SchemaClient

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
	xr := tu.NewResource(testExampleOrg+"/v1", "XR", "test-xr").InNamespace("default").Build()
	cmpd := tu.NewResource(testCpdOrg+"/v1", "testComposedResource", "resource1").InNamespace("default").Build()

	// Create sample CRDs
	xrCRD := makeCRD("xrs."+testExampleOrg, "XR", testExampleOrg, "v1")
	composedCRD := makeCRD("testComposedResources."+testCpdOrg, "testComposedResource", testCpdOrg, "v1")

	tests := map[string]struct {
		setupClient    func() *tu.MockSchemaClient
		initialCRDs    []*extv1.CustomResourceDefinition
		resources      []*un.Unstructured
		expectedCRDLen int
	}{
		"AllCRDsAlreadyCached": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithNoResourcesRequiringCRDs().
					WithCachingBehavior().
					Build()
			},
			initialCRDs:    []*extv1.CustomResourceDefinition{xrCRD, composedCRD},
			resources:      []*un.Unstructured{xr, cmpd},
			expectedCRDLen: 0, // No CRDs should be cached since no resources require CRDs
		},
		"FetchMissingCRDs": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					// Use the new GetCRD method with typed CRDs
					WithFoundCRD(testCpdOrg, "testComposedResource", composedCRD).
					// Make sure cpd resources require CRDs
					WithResourcesRequiringCRDs(
						schema.GroupVersionKind{Group: testCpdOrg, Version: "v1", Kind: "testComposedResource"},
					).
					WithCachingBehavior().
					Build()
			},
			initialCRDs:    []*extv1.CustomResourceDefinition{xrCRD}, // Only XR CRD is cached
			resources:      []*un.Unstructured{xr, cmpd},
			expectedCRDLen: 1, // Should fetch the missing cpd CRD (only cpd resource requires CRD)
		},
		"SomeCRDsMissing": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithCRDNotFound().
					WithResourcesRequiringCRDs(
						schema.GroupVersionKind{Group: testCpdOrg, Version: "v1", Kind: "testComposedResource"},
					).
					WithCachingBehavior().
					Build()
			},
			initialCRDs:    []*extv1.CustomResourceDefinition{xrCRD}, // Only XR CRD is cached
			resources:      []*un.Unstructured{xr, cmpd},
			expectedCRDLen: 0, // No CRDs should be fetched successfully since GetCRD returns not found
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			schemaClient := tt.setupClient()
			logger := tu.TestLogger(t, false)

			// Create the schema validator - CRDs provided via mock SchemaClient
			validator := NewSchemaValidator(schemaClient, tu.NewMockDefinitionClient().Build(), logger)

			// Call the function under test
			_ = validator.(*DefaultSchemaValidator).EnsureComposedResourceCRDs(ctx, tt.resources)

			// Verify the CRD count
			crds := validator.(*DefaultSchemaValidator).GetCRDs()
			if diff := cmp.Diff(tt.expectedCRDLen, len(crds)); diff != "" {
				t.Errorf("EnsureComposedResourceCRDs() CRD count mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDefaultSchemaValidator_LoadCRDs(t *testing.T) {
	ctx := t.Context()

	// Create sample CRDs as un
	xrdUn := tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "xrd1").
		WithSpecField("group", "testExampleOrg").
		WithSpecField("names", map[string]any{
			"kind":     "XR",
			"plural":   "xrs",
			"singular": "xr",
		}).
		Build()

	tests := map[string]struct {
		setupClient    func() xp.DefinitionClient
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
			expectedErr:    false,
			callTwice:      true, // Make two calls to LoadCRDs
			expectXRDCalls: 1,    // GetXRDs should only be called once due to caching
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defClient := tt.setupClient()
			logger := tu.TestLogger(t, false)

			// Create the schema validator with caching behavior
			validator := NewSchemaValidator(tu.NewMockSchemaClient().WithCachingBehavior().Build(), defClient, logger)

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
	return tu.NewCRD(name, group, kind).
		WithListKind(kind+"List").
		WithPlural(strings.ToLower(kind)+"s").
		WithSingular(strings.ToLower(kind)).
		WithVersion(version, true, true).
		WithStringFieldSchema("field").
		Build()
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

func TestDefaultSchemaValidator_ValidateResources_AppliesDefaults(t *testing.T) {
	ctx := t.Context()

	// Create a simple managed resource
	managedResource := tu.NewResource("provider.example.org/v1", "ManagedResource", "test-managed").
		InNamespace("default").
		WithCompositeOwner("test-xr").
		WithCompositionResourceName("managed-resource").
		WithSpecField("field", "value").
		BuildUComposed()

	// Manually add compositionRevisionRef to spec.crossplane to ensure validation can handle it
	_ = un.SetNestedMap(managedResource.Object, map[string]any{
		"compositionRevisionRef": map[string]any{
			"name": "some-revision-abc123",
		},
	}, "spec", "crossplane")

	// Create XR
	xr := tu.NewResource("example.org/v2", "XCompositeResource", "test-xr").
		InNamespace("default").
		WithSpecField("field", "value").
		Build()

	// Create CRD with defaults for the managed resource using OpenAPIV3Schema
	managedCRD := makeCRD("managedresources.provider.example.org", "ManagedResource", "provider.example.org", "v1")
	// Set a permissive schema with defaults
	managedCRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties = map[string]extv1.JSONSchemaProps{
		"spec": {
			Type:                   "object",
			XPreserveUnknownFields: func() *bool { b := true; return &b }(), // Allow all fields
			Properties: map[string]extv1.JSONSchemaProps{
				"deletionPolicy": {
					Type:    "string",
					Default: &extv1.JSON{Raw: []byte(`"Delete"`)},
				},
				"managementPolicies": {
					Type: "array",
					Items: &extv1.JSONSchemaPropsOrArray{
						Schema: &extv1.JSONSchemaProps{Type: "string"},
					},
					Default: &extv1.JSON{Raw: []byte(`["*"]`)},
				},
				"providerConfigRef": {
					Type:                   "object",
					XPreserveUnknownFields: func() *bool { b := true; return &b }(),
					Default:                &extv1.JSON{Raw: []byte(`{"name":"default"}`)},
				},
			},
		},
	}

	xrCRD := makeCRD("xcompositeresources.example.org", "XCompositeResource", "example.org", "v2")
	preserveUnknown := true
	specProps := xrCRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	specProps.XPreserveUnknownFields = &preserveUnknown
	xrCRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = specProps

	schemaClient := tu.NewMockSchemaClient().
		WithFoundCRDs(map[schema.GroupKind]*extv1.CustomResourceDefinition{
			{Group: "provider.example.org", Kind: "ManagedResource"}: managedCRD,
			{Group: "example.org", Kind: "XCompositeResource"}:       xrCRD,
		}).
		WithAllResourcesRequiringCRDs().
		WithCachingBehavior().
		Build()

	defClient := tu.NewMockDefinitionClient().Build()
	logger := tu.TestLogger(t, false)

	validator := NewSchemaValidator(schemaClient, defClient, logger)

	// Verify compositionRevisionRef exists before validation
	crossplane, found, _ := un.NestedMap(managedResource.Object, "spec", "crossplane")
	if !found || crossplane["compositionRevisionRef"] == nil {
		t.Fatal("Test setup failed: compositionRevisionRef not found in managed resource before validation")
	}

	// Call ValidateResources
	// This should succeed even with compositionRevisionRef present because the validator
	// strips Crossplane-managed fields internally before scope validation
	err := validator.ValidateResources(ctx, xr, []cpd.Unstructured{*managedResource})
	if err != nil {
		t.Fatalf("ValidateResources() unexpected error: %v", err)
	}

	// Verify defaults were applied to the ORIGINAL resource
	// The defaults are applied in-place by applyCRDDefaults before validation, so they persist
	deletionPolicy, found, err := un.NestedString(managedResource.Object, "spec", "deletionPolicy")
	if err != nil {
		t.Fatalf("Failed to get deletionPolicy: %v", err)
	}

	if !found || deletionPolicy != "Delete" {
		t.Errorf("Expected deletionPolicy default 'Delete' to be applied, got found=%v, value=%q", found, deletionPolicy)
	}

	managementPolicies, found, err := un.NestedStringSlice(managedResource.Object, "spec", "managementPolicies")
	if err != nil {
		t.Fatalf("Failed to get managementPolicies: %v", err)
	}

	if !found || len(managementPolicies) != 1 || managementPolicies[0] != "*" {
		t.Errorf("Expected managementPolicies default ['*'] to be applied, got found=%v, value=%v", found, managementPolicies)
	}

	providerConfigRef, found, err := un.NestedMap(managedResource.Object, "spec", "providerConfigRef")
	if err != nil {
		t.Fatalf("Failed to get providerConfigRef: %v", err)
	}

	if !found || providerConfigRef["name"] != "default" {
		t.Errorf("Expected providerConfigRef.name default 'default' to be applied, got found=%v, value=%v", found, providerConfigRef)
	}

	// Note: We do NOT verify that compositionRevisionRef is stripped from the original resource,
	// because the stripping only happens on temporary copies used for scope validation.
	// The compositionRevisionRef remains in the original resource, which is correct behavior.
}

func TestFormatValidationErrors(t *testing.T) {
	tests := map[string]struct {
		reason   string
		result   *pkgvalidate.ValidationResult
		expected string
	}{
		"SingleSchemaError": {
			reason: "A single field-level schema error renders as '<gvk> <name>:' header plus an indented '<message> [schema]' line.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{{
						Type:    pkgvalidate.FieldErrorTypeSchema,
						Field:   "spec.region",
						Message: "spec.region: Required value",
					}},
				}},
			},
			expected: "example.org/v1/XR my-xr:\n  spec.region: Required value [schema]",
		},
		"SingleMissingSchema": {
			reason: "A missing-schema resource collapses to a single '<gvk> <name>: missing schema' line (no errors[] to expand).",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "other.org/v1",
					Kind:       "SomeResource",
					Name:       "thing",
					Status:     pkgvalidate.ValidationStatusMissingSchema,
				}},
			},
			expected: "other.org/v1/SomeResource thing: missing schema",
		},
		"MissingSchemaWithoutName": {
			reason: "A resource without metadata.name collapses to just the GVK in the header rather than producing a trailing space.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "other.org/v1",
					Kind:       "SomeResource",
					Status:     pkgvalidate.ValidationStatusMissingSchema,
				}},
			},
			expected: "other.org/v1/SomeResource: missing schema",
		},
		"NamespacedResource": {
			reason: "Namespaced resources render '<ns>/<name>' in the header so cluster-vs-namespaced scope is visible.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "Thing",
					Namespace:  "production",
					Name:       "my-thing",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{{
						Type:    pkgvalidate.FieldErrorTypeSchema,
						Message: "spec.size: Required value",
					}},
				}},
			},
			expected: "example.org/v1/Thing production/my-thing:\n  spec.size: Required value [schema]",
		},
		"NamespaceWithoutNameCollapsesToGVK": {
			reason: "When namespace is set but name is empty, the header collapses to just the GVK rather than rendering '<ns>/' with a stray trailing slash.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "Thing",
					Namespace:  "production",
					Status:     pkgvalidate.ValidationStatusMissingSchema,
				}},
			},
			expected: "example.org/v1/Thing: missing schema",
		},
		"MultipleErrorsGroupedUnderResource": {
			reason: "Multiple errors on the same resource share a single header rather than producing a header per error.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{
						{Type: pkgvalidate.FieldErrorTypeSchema, Message: "spec.region: Required value"},
						{Type: pkgvalidate.FieldErrorTypeCEL, Message: "spec.replicas: must be > 0"},
					},
				}},
			},
			expected: "example.org/v1/XR my-xr:\n  spec.region: Required value [schema]\n  spec.replicas: must be > 0 [cel]",
		},
		"BadValueAppendedWhenAbsentFromMessage": {
			reason: "When Value is set and the message doesn't already include it (typical for CEL errors), '(got <value>)' is appended so users see what was rejected.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{{
						Type:    pkgvalidate.FieldErrorTypeCEL,
						Message: "spec.replicas: must be > 0",
						Value:   -1,
					}},
				}},
			},
			expected: "example.org/v1/XR my-xr:\n  spec.replicas: must be > 0 (got -1) [cel]",
		},
		"BadValueOmittedWhenAlreadyInMessage": {
			reason: "k8s field errors typically embed the bad value as a quoted string in the message text; the '(got <value>)' tail is omitted to avoid duplication.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{{
						Type:    pkgvalidate.FieldErrorTypeSchema,
						Message: `spec.replicas: Invalid value: "five": must be integer`,
						Value:   "five",
					}},
				}},
			},
			expected: `example.org/v1/XR my-xr:` + "\n" + `  spec.replicas: Invalid value: "five": must be integer [schema]`,
		},
		"BadValueNotSuppressedByIncidentalSubstring": {
			reason: "Regression: the duplication check matches the quoted form for strings (\"k\") so an unquoted incidental substring (\"k\" inside \"kind\") doesn't suppress the tail.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{{
						Type:    pkgvalidate.FieldErrorTypeCEL,
						Message: "spec.kind: Required value",
						Value:   "k",
					}},
				}},
			},
			expected: `example.org/v1/XR my-xr:` + "\n" + `  spec.kind: Required value (got "k") [cel]`,
		},
		"NumericBadValueRendersUnquoted": {
			reason: "Numbers / bools / structs aren't quoted in k8s messages and shouldn't be quoted in our display either.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{{
						Type:    pkgvalidate.FieldErrorTypeCEL,
						Message: "spec.replicas: must be > 0",
						Value:   42,
					}},
				}},
			},
			expected: "example.org/v1/XR my-xr:\n  spec.replicas: must be > 0 (got 42) [cel]",
		},
		"NumericBadValueNotSuppressedByDigitPrefix": {
			reason: "Regression: a raw substring search would suppress the tail when the rendered number is a digit-prefix of another token (Value=42 vs message containing '420'). Word-boundary matching keeps the tail rendered so the user sees the actual rejected value.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{{
						Type:    pkgvalidate.FieldErrorTypeCEL,
						Message: "spec.size: must be at most 420",
						Value:   42,
					}},
				}},
			},
			expected: "example.org/v1/XR my-xr:\n  spec.size: must be at most 420 (got 42) [cel]",
		},
		"NumericBadValueSuppressedAsToken": {
			reason: "When the rendered number does appear as a standalone token in the message (delimited by non-word chars), the tail is correctly suppressed to avoid duplication.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{{
						Type:    pkgvalidate.FieldErrorTypeSchema,
						Message: "spec.replicas: Invalid value: 42: must be at most 10",
						Value:   42,
					}},
				}},
			},
			expected: "example.org/v1/XR my-xr:\n  spec.replicas: Invalid value: 42: must be at most 10 [schema]",
		},
		"MultipleResourcesEachOnTheirOwnBlock": {
			reason: "Failures from distinct resources produce distinct blocks joined by newlines, in input order.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{
					{
						APIVersion: "example.org/v1",
						Kind:       "XR",
						Name:       "my-xr",
						Status:     pkgvalidate.ValidationStatusInvalid,
						Errors: []pkgvalidate.FieldValidationError{{
							Type:    pkgvalidate.FieldErrorTypeSchema,
							Message: "spec.region: Required value",
						}},
					},
					{
						APIVersion: "other.org/v1",
						Kind:       "SomeResource",
						Name:       "thing",
						Status:     pkgvalidate.ValidationStatusMissingSchema,
					},
				},
			},
			expected: "example.org/v1/XR my-xr:\n  spec.region: Required value [schema]\nother.org/v1/SomeResource thing: missing schema",
		},
		"ValidResourcesProduceNoBlock": {
			reason: "Only failing resources contribute blocks; valid resources are filtered out so the output is the failure summary, not an audit log.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{
					{
						APIVersion: "example.org/v1",
						Kind:       "XR",
						Name:       "good-xr",
						Status:     pkgvalidate.ValidationStatusValid,
					},
					{
						APIVersion: "example.org/v1",
						Kind:       "XR",
						Name:       "bad-xr",
						Status:     pkgvalidate.ValidationStatusInvalid,
						Errors: []pkgvalidate.FieldValidationError{{
							Type:    pkgvalidate.FieldErrorTypeSchema,
							Message: "spec.field: Invalid value",
						}},
					},
				},
			},
			expected: "example.org/v1/XR bad-xr:\n  spec.field: Invalid value [schema]",
		},
		"NoApplicableEntriesReturnsEmpty": {
			reason: "formatValidationErrors is only invoked after ResultError has flagged a failure; an all-valid input is unreachable in production. The contract is empty string (not a generic fallback) so the wrapping ResultError carries the message.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusValid,
				}},
			},
			expected: "",
		},
		"DefaultingErrorAccompanyingSchemaError": {
			reason: "A defaulting entry that co-occurs with a schema-class error is suppressed: the schema entry already conveys the failure, and the defaulting line would just be noise.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{
						{Type: pkgvalidate.FieldErrorTypeDefaulting, Message: "cannot apply defaults"},
						{Type: pkgvalidate.FieldErrorTypeSchema, Message: "spec.field: Required value"},
					},
				}},
			},
			expected: "example.org/v1/XR my-xr:\n  spec.field: Required value [schema]",
		},
		"InvalidWithOnlyDefaultingErrorsStillEmitted": {
			reason: "Defensive: upstream's statusFromErrors won't produce Invalid+only-defaulting today, but if it ever did we surface the defaulting message rather than emit an empty block.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{
						{Type: pkgvalidate.FieldErrorTypeDefaulting, Message: "cannot apply defaults"},
					},
				}},
			},
			expected: "example.org/v1/XR my-xr:\n  cannot apply defaults [defaulting]",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := formatValidationErrors(tt.result)
			if got != tt.expected {
				t.Errorf("\n%s\nformatValidationErrors() = %q, want %q", tt.reason, got, tt.expected)
			}
		})
	}
}

func TestDefaultSchemaValidator_ValidateScopeConstraints(t *testing.T) {
	ctx := t.Context()

	// Create CRDs with different scopes
	namespacedCRD := makeCRD("namespacedresources."+testExampleOrg, "NamespacedResource", testExampleOrg, "v1")
	namespacedCRD.Spec.Scope = extv1.NamespaceScoped

	clusterCRD := makeCRD("clusterresources."+testExampleOrg, "ClusterResource", testExampleOrg, "v1")
	clusterCRD.Spec.Scope = extv1.ClusterScoped

	tests := map[string]struct {
		setupClient       func() *tu.MockSchemaClient
		resource          *un.Unstructured
		expectedNamespace string
		isClaimRoot       bool
		expectedErr       bool
		expectedErrMsg    string
	}{
		"NamespacedResourceValidNamespace": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithFoundCRD(testExampleOrg, "NamespacedResource", namespacedCRD).
					Build()
			},
			resource: tu.NewResource(testExampleOrg+"/v1", "NamespacedResource", "test-resource").
				InNamespace("default").
				Build(),
			expectedNamespace: "default",
			isClaimRoot:       false,
			expectedErr:       false,
		},
		"NamespacedResourceMissingNamespace": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithFoundCRD(testExampleOrg, "NamespacedResource", namespacedCRD).
					Build()
			},
			resource: tu.NewResource(testExampleOrg+"/v1", "NamespacedResource", "test-resource").
				Build(), // No namespace
			expectedNamespace: "default",
			isClaimRoot:       false,
			expectedErr:       true,
			expectedErrMsg:    "namespaced resource NamespacedResource/test-resource must have a namespace",
		},
		"NamespacedResourceWrongNamespace": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithFoundCRD(testExampleOrg, "NamespacedResource", namespacedCRD).
					Build()
			},
			resource: tu.NewResource(testExampleOrg+"/v1", "NamespacedResource", "test-resource").
				InNamespace("wrong").
				Build(),
			expectedNamespace: "default",
			isClaimRoot:       false,
			expectedErr:       true,
			expectedErrMsg:    "cross-namespace references not supported",
		},
		"ClusterResourceValidNoNamespace": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithFoundCRD(testExampleOrg, "ClusterResource", clusterCRD).
					Build()
			},
			resource: tu.NewResource(testExampleOrg+"/v1", "ClusterResource", "test-resource").
				Build(), // No namespace - correct for cluster-scoped
			expectedNamespace: "",
			isClaimRoot:       false,
			expectedErr:       false,
		},
		"ClusterResourceInvalidNamespace": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithFoundCRD(testExampleOrg, "ClusterResource", clusterCRD).
					Build()
			},
			resource: tu.NewResource(testExampleOrg+"/v1", "ClusterResource", "test-resource").
				InNamespace("default").
				Build(),
			expectedNamespace: "",
			isClaimRoot:       false,
			expectedErr:       true,
			expectedErrMsg:    "cluster-scoped resource ClusterResource/test-resource cannot have a namespace",
		},
		"ClusterResourceFromNamespacedXR": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithFoundCRD(testExampleOrg, "ClusterResource", clusterCRD).
					Build()
			},
			resource: tu.NewResource(testExampleOrg+"/v1", "ClusterResource", "test-resource").
				Build(),
			expectedNamespace: "default", // XR is namespaced
			isClaimRoot:       false,
			expectedErr:       true,
			expectedErrMsg:    "namespaced XR cannot own cluster-scoped managed resource",
		},
		"ClusterResourceFromNamespacedClaim": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithFoundCRD(testExampleOrg, "ClusterResource", clusterCRD).
					Build()
			},
			resource: tu.NewResource(testExampleOrg+"/v1", "ClusterResource", "test-resource").
				Build(),
			expectedNamespace: "default", // Claim is namespaced
			isClaimRoot:       true,      // But it's a claim, so allowed
			expectedErr:       false,
		},
		"CRDNotFound": {
			setupClient: func() *tu.MockSchemaClient {
				return tu.NewMockSchemaClient().
					WithGetCRD(func(_ context.Context, _ schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
						return nil, errors.New("CRD not found")
					}).
					Build()
			},
			resource: tu.NewResource(testExampleOrg+"/v1", "UnknownResource", "test-resource").
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

			// Create the schema validator - CRDs provided via mock SchemaClient
			validator := NewSchemaValidator(schemaClient, tu.NewMockDefinitionClient().Build(), logger)

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
