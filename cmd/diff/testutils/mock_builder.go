package testutils

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"

	xpextv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
	"github.com/crossplane/crossplane/v2/cmd/crank/common/resource"
)

// MockBuilder provides a fluent API for building mock objects used in testing.
// This helps reduce duplication in test setup code while making the intent clearer.

// region Kubernetes API layer mock builders

// ======================================================================================
// Kubernetes API Layer Mock Builders
// ======================================================================================

// MockResourceClientBuilder helps build kubernetes.ResourceClient mocks.
type MockResourceClientBuilder struct {
	mock *MockResourceClient
}

// NewMockResourceClient creates a new MockResourceClientBuilder.
func NewMockResourceClient() *MockResourceClientBuilder {
	return &MockResourceClientBuilder{
		mock: &MockResourceClient{},
	}
}

// WithInitialize sets the Initialize behavior.
func (b *MockResourceClientBuilder) WithInitialize(fn func(context.Context) error) *MockResourceClientBuilder {
	b.mock.InitializeFn = fn
	return b
}

// WithSuccessfulInitialize sets a successful Initialize implementation.
func (b *MockResourceClientBuilder) WithSuccessfulInitialize() *MockResourceClientBuilder {
	return b.WithInitialize(func(context.Context) error {
		return nil
	})
}

// WithFoundGVKs sets the GetGVKsForGroupKind behavior.
func (b *MockResourceClientBuilder) WithFoundGVKs(gvks []schema.GroupVersionKind) *MockResourceClientBuilder {
	b.mock.GetGVKsForGroupKindFn = func(_ context.Context, _, _ string) ([]schema.GroupVersionKind, error) {
		return gvks, nil
	}

	return b
}

// WithoutFoundGVKs sets the GetGVKsForGroupKind behavior.
func (b *MockResourceClientBuilder) WithoutFoundGVKs(err string) *MockResourceClientBuilder {
	b.mock.GetGVKsForGroupKindFn = func(_ context.Context, _, _ string) ([]schema.GroupVersionKind, error) {
		return nil, errors.New(err)
	}

	return b
}

// WithGetResource sets the GetResource behavior.
func (b *MockResourceClientBuilder) WithGetResource(fn func(context.Context, schema.GroupVersionKind, string, string) (*un.Unstructured, error)) *MockResourceClientBuilder {
	b.mock.GetResourceFn = fn
	return b
}

// WithResourcesExist sets up GetResource to return resources from a map.
func (b *MockResourceClientBuilder) WithResourcesExist(resources ...*un.Unstructured) *MockResourceClientBuilder {
	resourceMap := make(map[string]*un.Unstructured)

	// Build a map for fast lookup
	for _, res := range resources {
		// Use name + kind as a unique key
		key := res.GetName() + "|" + res.GetKind()
		resourceMap[key] = res
	}

	return b.WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
		// Try to find the resource by name and kind
		key := name + "|" + gvk.Kind
		if res, found := resourceMap[key]; found {
			return res, nil
		}

		return nil, errors.Errorf("resource %q not found", name)
	})
}

// WithResourceNotFound sets GetResource to always return "not found".
func (b *MockResourceClientBuilder) WithResourceNotFound() *MockResourceClientBuilder {
	return b.WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
		// Create a proper Kubernetes "not found" error
		return nil, apierrors.NewNotFound(
			schema.GroupResource{
				Group:    gvk.Group,
				Resource: strings.ToLower(gvk.Kind) + "s", // Naive pluralization similar to the real code
			},
			name,
		)
	})
}

// WithListResources sets the ListResources behavior.
func (b *MockResourceClientBuilder) WithListResources(fn func(context.Context, schema.GroupVersionKind, string) ([]*un.Unstructured, error)) *MockResourceClientBuilder {
	b.mock.ListResourcesFn = fn
	return b
}

// WithEmptyListResources mimics an empty but successful response.
func (b *MockResourceClientBuilder) WithEmptyListResources() *MockResourceClientBuilder {
	return b.WithListResources(func(context.Context, schema.GroupVersionKind, string) ([]*un.Unstructured, error) {
		return []*un.Unstructured{}, nil
	})
}

// WithListResourcesFailure mimics a failed response.
func (b *MockResourceClientBuilder) WithListResourcesFailure(errorStr string) *MockResourceClientBuilder {
	return b.WithListResources(func(context.Context, schema.GroupVersionKind, string) ([]*un.Unstructured, error) {
		return nil, errors.New(errorStr)
	})
}

// WithGetResourcesByLabel sets the GetResourcesByLabel behavior.
func (b *MockResourceClientBuilder) WithGetResourcesByLabel(fn func(context.Context, schema.GroupVersionKind, string, metav1.LabelSelector) ([]*un.Unstructured, error)) *MockResourceClientBuilder {
	b.mock.GetResourcesByLabelFn = fn
	return b
}

// WithResourcesFoundByLabel sets GetResourcesByLabel to return resources for a specific label.
func (b *MockResourceClientBuilder) WithResourcesFoundByLabel(resources []*un.Unstructured, label, value string) *MockResourceClientBuilder {
	return b.WithGetResourcesByLabel(func(_ context.Context, _ schema.GroupVersionKind, _ string, selector metav1.LabelSelector) ([]*un.Unstructured, error) {
		// Check if the selector matches our expected label
		if labelValue, exists := selector.MatchLabels[label]; exists && labelValue == value {
			return resources, nil
		}

		return []*un.Unstructured{}, nil
	})
}

// WithGetAllResourcesByLabels sets the GetAllResourcesByLabels behavior.
func (b *MockResourceClientBuilder) WithGetAllResourcesByLabels(fn func(context.Context, []schema.GroupVersionKind, []metav1.LabelSelector) ([]*un.Unstructured, error)) *MockResourceClientBuilder {
	b.mock.GetAllResourcesByLabelsFn = fn
	return b
}

// WithIsNamespacedResource sets the IsNamespacedResource behavior.
func (b *MockResourceClientBuilder) WithIsNamespacedResource(fn func(context.Context, schema.GroupVersionKind) (bool, error)) *MockResourceClientBuilder {
	b.mock.IsNamespacedResourceFn = fn
	return b
}

// WithNamespacedResource sets specific GVKs to be namespaced.
func (b *MockResourceClientBuilder) WithNamespacedResource(gvks ...schema.GroupVersionKind) *MockResourceClientBuilder {
	namespacedGVKs := make(map[schema.GroupVersionKind]bool)
	for _, gvk := range gvks {
		namespacedGVKs[gvk] = true
	}

	return b.WithIsNamespacedResource(func(_ context.Context, gvk schema.GroupVersionKind) (bool, error) {
		if isNamespaced, exists := namespacedGVKs[gvk]; exists {
			return isNamespaced, nil
		}
		// Default to error for unconfigured resources to make tests explicit
		return false, errors.Errorf("IsNamespacedResource not configured for %s in mock", gvk.String())
	})
}

// WithClusterScopedResource sets specific GVKs to be cluster-scoped.
func (b *MockResourceClientBuilder) WithClusterScopedResource(gvks ...schema.GroupVersionKind) *MockResourceClientBuilder {
	clusterGVKs := make(map[schema.GroupVersionKind]bool)
	for _, gvk := range gvks {
		clusterGVKs[gvk] = true
	}

	return b.WithIsNamespacedResource(func(_ context.Context, gvk schema.GroupVersionKind) (bool, error) {
		if _, exists := clusterGVKs[gvk]; exists {
			return false, nil
		}
		// Default to error for unconfigured resources to make tests explicit
		return false, errors.Errorf("IsNamespacedResource not configured for %s in mock", gvk.String())
	})
}

// Build returns the built mock.
func (b *MockResourceClientBuilder) Build() *MockResourceClient {
	return b.mock
}

// MockSchemaClientBuilder helps build kubernetes.SchemaClient mocks.
type MockSchemaClientBuilder struct {
	mock *MockSchemaClient
}

// NewMockSchemaClient creates a new MockSchemaClientBuilder.
func NewMockSchemaClient() *MockSchemaClientBuilder {
	return &MockSchemaClientBuilder{
		mock: &MockSchemaClient{},
	}
}

// WithInitialize sets the Initialize behavior.
func (b *MockSchemaClientBuilder) WithInitialize(fn func(context.Context) error) *MockSchemaClientBuilder {
	b.mock.InitializeFn = fn
	return b
}

// WithGetCRD sets the GetCRD behavior.
func (b *MockSchemaClientBuilder) WithGetCRD(fn func(context.Context, schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error)) *MockSchemaClientBuilder {
	b.mock.GetCRDFn = fn
	return b
}

// WithCRDNotFound sets GetCRD to return a not found error.
func (b *MockSchemaClientBuilder) WithCRDNotFound() *MockSchemaClientBuilder {
	return b.WithGetCRD(func(context.Context, schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
		return nil, errors.New("CRD not found")
	})
}

// WithSuccessfulCRDFetch sets GetCRD to return a specific CRD.
func (b *MockSchemaClientBuilder) WithSuccessfulCRDFetch(crd *extv1.CustomResourceDefinition) *MockSchemaClientBuilder {
	return b.WithGetCRD(func(context.Context, schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
		return crd, nil
	})
}

// WithFoundCRD sets GetCRD to return a specific CRD when the group and kind match.
func (b *MockSchemaClientBuilder) WithFoundCRD(group, kind string, crd *extv1.CustomResourceDefinition) *MockSchemaClientBuilder {
	// If we don't have an existing GetCRD function, create a new one
	if b.mock.GetCRDFn == nil {
		return b.WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
			if gvk.Group == group && gvk.Kind == kind {
				return crd, nil
			}

			return nil, errors.New("CRD not found")
		})
	}

	// If we already have a GetCRD function, wrap it to add this mapping
	originalFn := b.mock.GetCRDFn

	return b.WithGetCRD(func(ctx context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
		if gvk.Group == group && gvk.Kind == kind {
			return crd, nil
		}

		return originalFn(ctx, gvk)
	})
}

// WithFoundCRDs sets GetCRD to return specific CRDs when the group and kind match, with a fallback error.
func (b *MockSchemaClientBuilder) WithFoundCRDs(crdMappings map[schema.GroupKind]*extv1.CustomResourceDefinition) *MockSchemaClientBuilder {
	return b.WithGetCRD(func(_ context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
		groupKind := schema.GroupKind{Group: gvk.Group, Kind: gvk.Kind}
		if crd, found := crdMappings[groupKind]; found {
			return crd, nil
		}

		return nil, errors.New("CRD not found")
	})
}

// WithGetCRDByName sets the GetCRDByName behavior.
func (b *MockSchemaClientBuilder) WithGetCRDByName(fn func(string) (*extv1.CustomResourceDefinition, error)) *MockSchemaClientBuilder {
	b.mock.GetCRDByNameFn = fn
	return b
}

// WithSuccessfulCRDByNameFetch sets GetCRDByName to return a specific CRD for a given name.
func (b *MockSchemaClientBuilder) WithSuccessfulCRDByNameFetch(name string, crd *extv1.CustomResourceDefinition) *MockSchemaClientBuilder {
	return b.WithGetCRDByName(func(searchName string) (*extv1.CustomResourceDefinition, error) {
		if searchName == name {
			return crd, nil
		}

		return nil, errors.Errorf("CRD with name %s not found", searchName)
	})
}

// WithIsCRDRequired sets the IsCRDRequired behavior.
func (b *MockSchemaClientBuilder) WithIsCRDRequired(fn func(context.Context, schema.GroupVersionKind) bool) *MockSchemaClientBuilder {
	b.mock.IsCRDRequiredFn = fn
	return b
}

// WithResourcesRequiringCRDs sets only the specified GVKs to require CRDs.
func (b *MockSchemaClientBuilder) WithResourcesRequiringCRDs(crdsRequiredGVKs ...schema.GroupVersionKind) *MockSchemaClientBuilder {
	requiresCRD := make(map[schema.GroupVersionKind]bool)
	for _, gvk := range crdsRequiredGVKs {
		requiresCRD[gvk] = true
	}

	return b.WithIsCRDRequired(func(_ context.Context, gvk schema.GroupVersionKind) bool {
		// Only require CRDs for specified GVKs
		return requiresCRD[gvk]
	})
}

// WithAllResourcesRequiringCRDs sets all resources to require CRDs.
func (b *MockSchemaClientBuilder) WithAllResourcesRequiringCRDs() *MockSchemaClientBuilder {
	return b.WithIsCRDRequired(func(context.Context, schema.GroupVersionKind) bool {
		return true
	})
}

// WithNoResourcesRequiringCRDs sets all resources to not require CRDs.
func (b *MockSchemaClientBuilder) WithNoResourcesRequiringCRDs() *MockSchemaClientBuilder {
	return b.WithIsCRDRequired(func(context.Context, schema.GroupVersionKind) bool {
		return false
	})
}

// WithValidateResource sets the ValidateResource behavior.
func (b *MockSchemaClientBuilder) WithValidateResource(fn func(context.Context, *un.Unstructured) error) *MockSchemaClientBuilder {
	b.mock.ValidateResourceFn = fn
	return b
}

// WithGetAllCRDs sets the GetAllCRDs behavior.
func (b *MockSchemaClientBuilder) WithGetAllCRDs(fn func() []*extv1.CustomResourceDefinition) *MockSchemaClientBuilder {
	b.mock.GetAllCRDsFn = fn
	return b
}

// WithCachingBehavior creates a mock that simulates caching CRDs when GetCRD or LoadCRDsFromXRDs is called.
func (b *MockSchemaClientBuilder) WithCachingBehavior() *MockSchemaClientBuilder {
	cachedCRDs := make(map[string]*extv1.CustomResourceDefinition)

	// Override GetCRD to track cached CRDs
	originalGetCRD := b.mock.GetCRDFn
	b.mock.GetCRDFn = func(ctx context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
		// Check cache first
		key := gvk.String()
		if crd, ok := cachedCRDs[key]; ok {
			return crd, nil
		}

		// Call original function
		if originalGetCRD != nil {
			crd, err := originalGetCRD(ctx, gvk)
			if err == nil && crd != nil {
				// Cache the CRD
				cachedCRDs[key] = crd
			}

			return crd, err
		}

		return nil, errors.New("GetCRD not implemented")
	}

	// Override LoadCRDsFromXRDs to simulate caching CRDs converted from XRDs
	originalLoadCRDs := b.mock.LoadCRDsFromXRDsFn
	b.mock.LoadCRDsFromXRDsFn = func(ctx context.Context, xrds []*un.Unstructured) error {
		// Call original function first
		if originalLoadCRDs != nil {
			err := originalLoadCRDs(ctx, xrds)
			if err != nil {
				return err
			}
		}

		// Simulate caching CRDs converted from XRDs
		// For test purposes, create a simple CRD from each XRD
		for _, xrd := range xrds {
			group, _, _ := un.NestedString(xrd.Object, "spec", "group")
			if group == "" {
				continue // Skip invalid XRDs
			}

			names, ok, _ := un.NestedMap(xrd.Object, "spec", "names")
			if !ok {
				continue
			}

			kind, _ := names["kind"].(string)
			if kind == "" {
				continue
			}

			// Create a simple CRD for this XRD
			crd := &extv1.CustomResourceDefinition{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "apiextensions.k8s.io/v1",
					Kind:       "CustomResourceDefinition",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: xrd.GetName(),
				},
				Spec: extv1.CustomResourceDefinitionSpec{
					Group: group,
					Names: extv1.CustomResourceDefinitionNames{
						Kind: kind,
					},
					Scope: extv1.NamespaceScoped,
					Versions: []extv1.CustomResourceDefinitionVersion{
						{
							Name:    "v1",
							Served:  true,
							Storage: true,
						},
					},
				},
			}

			gvk := schema.GroupVersionKind{Group: group, Version: "v1", Kind: kind}
			cachedCRDs[gvk.String()] = crd
		}

		return nil
	}

	// Override GetAllCRDs to return cached CRDs
	b.mock.GetAllCRDsFn = func() []*extv1.CustomResourceDefinition {
		var result []*extv1.CustomResourceDefinition
		for _, crd := range cachedCRDs {
			result = append(result, crd)
		}

		return result
	}

	return b
}

// Build returns the built mock.
func (b *MockSchemaClientBuilder) Build() *MockSchemaClient {
	return b.mock
}

// MockApplyClientBuilder helps build kubernetes.ApplyClient mocks.
type MockApplyClientBuilder struct {
	mock *MockApplyClient
}

// NewMockApplyClient creates a new MockApplyClientBuilder.
func NewMockApplyClient() *MockApplyClientBuilder {
	return &MockApplyClientBuilder{
		mock: &MockApplyClient{},
	}
}

// WithInitialize sets the Initialize behavior.
func (b *MockApplyClientBuilder) WithInitialize(fn func(context.Context) error) *MockApplyClientBuilder {
	b.mock.InitializeFn = fn
	return b
}

// WithApply sets the Apply behavior.
func (b *MockApplyClientBuilder) WithApply(fn func(context.Context, *un.Unstructured) (*un.Unstructured, error)) *MockApplyClientBuilder {
	b.mock.ApplyFn = fn
	return b
}

// WithDryRunApply sets the DryRunApply behavior.
func (b *MockApplyClientBuilder) WithDryRunApply(fn func(context.Context, *un.Unstructured) (*un.Unstructured, error)) *MockApplyClientBuilder {
	b.mock.DryRunApplyFn = fn
	return b
}

// WithSuccessfulDryRun sets DryRunApply to return the input resource.
func (b *MockApplyClientBuilder) WithSuccessfulDryRun() *MockApplyClientBuilder {
	return b.WithDryRunApply(func(_ context.Context, obj *un.Unstructured) (*un.Unstructured, error) {
		return obj, nil
	})
}

// WithFailedDryRun sets DryRunApply to return an error.
func (b *MockApplyClientBuilder) WithFailedDryRun(errMsg string) *MockApplyClientBuilder {
	return b.WithDryRunApply(func(context.Context, *un.Unstructured) (*un.Unstructured, error) {
		return nil, errors.New(errMsg)
	})
}

// Build returns the built mock.
func (b *MockApplyClientBuilder) Build() *MockApplyClient {
	return b.mock
}

// MockTypeConverterBuilder helps build kubernetes.TypeConverter mocks.
type MockTypeConverterBuilder struct {
	mock *MockTypeConverter
}

// NewMockTypeConverter creates a new MockTypeConverterBuilder.
func NewMockTypeConverter() *MockTypeConverterBuilder {
	return &MockTypeConverterBuilder{
		mock: &MockTypeConverter{},
	}
}

// WithGVKToGVR sets the GVKToGVR behavior.
func (b *MockTypeConverterBuilder) WithGVKToGVR(fn func(context.Context, schema.GroupVersionKind) (schema.GroupVersionResource, error)) *MockTypeConverterBuilder {
	b.mock.GVKToGVRFn = fn
	return b
}

// WithDefaultGVKToGVR sets a default implementation for GVKToGVR.
func (b *MockTypeConverterBuilder) WithDefaultGVKToGVR() *MockTypeConverterBuilder {
	return b.WithGVKToGVR(func(_ context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
		// Simple default implementation that converts Kind to lowercase and adds 's'
		return schema.GroupVersionResource{
			Group:    gvk.Group,
			Version:  gvk.Version,
			Resource: strings.ToLower(gvk.Kind) + "s",
		}, nil
	})
}

// WithGetResourceNameForGVK sets the GetResourceNameForGVK behavior.
func (b *MockTypeConverterBuilder) WithGetResourceNameForGVK(fn func(context.Context, schema.GroupVersionKind) (string, error)) *MockTypeConverterBuilder {
	b.mock.GetResourceNameForGVKFn = fn
	return b
}

// WithDefaultGetResourceNameForGVK sets a default implementation for GetResourceNameForGVK.
func (b *MockTypeConverterBuilder) WithDefaultGetResourceNameForGVK() *MockTypeConverterBuilder {
	return b.WithGetResourceNameForGVK(func(_ context.Context, gvk schema.GroupVersionKind) (string, error) {
		// Simple default implementation that converts Kind to lowercase and adds 's'
		return strings.ToLower(gvk.Kind) + "s", nil
	})
}

// Build returns the built mock.
func (b *MockTypeConverterBuilder) Build() *MockTypeConverter {
	return b.mock
}

// endregion

// region Crossplane API layer mock builders

// ======================================================================================
// Crossplane API Layer Mock Builders
// ======================================================================================

// MockCompositionClientBuilder helps build crossplane.CompositionClient mocks.
type MockCompositionClientBuilder struct {
	mock *MockCompositionClient
}

// NewMockCompositionClient creates a new MockCompositionClientBuilder.
func NewMockCompositionClient() *MockCompositionClientBuilder {
	return &MockCompositionClientBuilder{
		mock: &MockCompositionClient{},
	}
}

// WithInitialize sets the Initialize behavior.
func (b *MockCompositionClientBuilder) WithInitialize(fn func(context.Context) error) *MockCompositionClientBuilder {
	b.mock.InitializeFn = fn
	return b
}

// WithSuccessfulInitialize mocks a successful call to Initialize.
func (b *MockCompositionClientBuilder) WithSuccessfulInitialize() *MockCompositionClientBuilder {
	b.mock.InitializeFn = func(context.Context) error {
		return nil
	}

	return b
}

// WithFailedInitialize mocks a failed call to Initialize.
func (b *MockCompositionClientBuilder) WithFailedInitialize(errMsg string) *MockCompositionClientBuilder {
	b.mock.InitializeFn = func(context.Context) error {
		return errors.New(errMsg)
	}

	return b
}

// WithFindMatchingComposition sets the FindMatchingComposition behavior.
func (b *MockCompositionClientBuilder) WithFindMatchingComposition(fn func(context.Context, *un.Unstructured) (*xpextv1.Composition, error)) *MockCompositionClientBuilder {
	b.mock.FindMatchingCompositionFn = fn
	return b
}

// WithSuccessfulCompositionMatch sets FindMatchingComposition to return a specific composition.
func (b *MockCompositionClientBuilder) WithSuccessfulCompositionMatch(comp *xpextv1.Composition) *MockCompositionClientBuilder {
	return b.WithFindMatchingComposition(func(context.Context, *un.Unstructured) (*xpextv1.Composition, error) {
		return comp, nil
	})
}

// WithNoMatchingComposition sets FindMatchingComposition to return "not found".
func (b *MockCompositionClientBuilder) WithNoMatchingComposition() *MockCompositionClientBuilder {
	return b.WithFindMatchingComposition(func(context.Context, *un.Unstructured) (*xpextv1.Composition, error) {
		return nil, errors.New("composition not found")
	})
}

// WithListCompositions sets the ListCompositions behavior.
func (b *MockCompositionClientBuilder) WithListCompositions(fn func(context.Context) ([]*xpextv1.Composition, error)) *MockCompositionClientBuilder {
	b.mock.ListCompositionsFn = fn
	return b
}

// WithGetComposition sets the GetComposition behavior.
func (b *MockCompositionClientBuilder) WithGetComposition(fn func(context.Context, string) (*xpextv1.Composition, error)) *MockCompositionClientBuilder {
	b.mock.GetCompositionFn = fn
	return b
}

// Build returns the built mock.
func (b *MockCompositionClientBuilder) Build() *MockCompositionClient {
	return b.mock
}

// MockFunctionClientBuilder helps build crossplane.FunctionClient mocks.
type MockFunctionClientBuilder struct {
	mock *MockFunctionClient
}

// NewMockFunctionClient creates a new MockFunctionClientBuilder.
func NewMockFunctionClient() *MockFunctionClientBuilder {
	return &MockFunctionClientBuilder{
		mock: &MockFunctionClient{},
	}
}

// WithInitialize sets the Initialize behavior.
func (b *MockFunctionClientBuilder) WithInitialize(fn func(context.Context) error) *MockFunctionClientBuilder {
	b.mock.InitializeFn = fn
	return b
}

// WithSuccessfulInitialize mocks a successful call to Initialize.
func (b *MockFunctionClientBuilder) WithSuccessfulInitialize() *MockFunctionClientBuilder {
	b.mock.InitializeFn = func(context.Context) error {
		return nil
	}

	return b
}

// WithFailedInitialize mocks a failed call to Initialize.
func (b *MockFunctionClientBuilder) WithFailedInitialize(errMsg string) *MockFunctionClientBuilder {
	b.mock.InitializeFn = func(context.Context) error {
		return errors.New(errMsg)
	}

	return b
}

// WithGetFunctionsFromPipeline sets the GetFunctionsFromPipeline behavior.
func (b *MockFunctionClientBuilder) WithGetFunctionsFromPipeline(fn func(*xpextv1.Composition) ([]pkgv1.Function, error)) *MockFunctionClientBuilder {
	b.mock.GetFunctionsFromPipelineFn = fn
	return b
}

// WithSuccessfulFunctionsFetch sets GetFunctionsFromPipeline to return specific functions.
func (b *MockFunctionClientBuilder) WithSuccessfulFunctionsFetch(functions []pkgv1.Function) *MockFunctionClientBuilder {
	return b.WithGetFunctionsFromPipeline(func(*xpextv1.Composition) ([]pkgv1.Function, error) {
		return functions, nil
	})
}

// WithFailedFunctionsFetch sets GetFunctionsFromPipeline to return an error.
func (b *MockFunctionClientBuilder) WithFailedFunctionsFetch(errMsg string) *MockFunctionClientBuilder {
	return b.WithGetFunctionsFromPipeline(func(*xpextv1.Composition) ([]pkgv1.Function, error) {
		return nil, errors.New(errMsg)
	})
}

// WithListFunctions sets the ListFunctions behavior.
func (b *MockFunctionClientBuilder) WithListFunctions(fn func(context.Context) ([]pkgv1.Function, error)) *MockFunctionClientBuilder {
	b.mock.ListFunctionsFn = fn
	return b
}

// Build returns the built mock.
func (b *MockFunctionClientBuilder) Build() *MockFunctionClient {
	return b.mock
}

// MockEnvironmentClientBuilder helps build crossplane.EnvironmentClient mocks.
type MockEnvironmentClientBuilder struct {
	mock *MockEnvironmentClient
}

// NewMockEnvironmentClient creates a new MockEnvironmentClientBuilder.
func NewMockEnvironmentClient() *MockEnvironmentClientBuilder {
	return &MockEnvironmentClientBuilder{
		mock: &MockEnvironmentClient{},
	}
}

// WithInitialize sets the Initialize behavior.
func (b *MockEnvironmentClientBuilder) WithInitialize(fn func(context.Context) error) *MockEnvironmentClientBuilder {
	b.mock.InitializeFn = fn
	return b
}

// WithSuccessfulInitialize mocks a successful call to Initialize.
func (b *MockEnvironmentClientBuilder) WithSuccessfulInitialize() *MockEnvironmentClientBuilder {
	b.mock.InitializeFn = func(context.Context) error {
		return nil
	}

	return b
}

// WithFailedInitialize mocks a failed call to Initialize.
func (b *MockEnvironmentClientBuilder) WithFailedInitialize(errMsg string) *MockEnvironmentClientBuilder {
	b.mock.InitializeFn = func(context.Context) error {
		return errors.New(errMsg)
	}

	return b
}

// WithGetEnvironmentConfigs sets the GetEnvironmentConfigs behavior.
func (b *MockEnvironmentClientBuilder) WithGetEnvironmentConfigs(fn func(context.Context) ([]*un.Unstructured, error)) *MockEnvironmentClientBuilder {
	b.mock.GetEnvironmentConfigsFn = fn
	return b
}

// WithSuccessfulEnvironmentConfigsFetch sets GetEnvironmentConfigs to return specific configs.
func (b *MockEnvironmentClientBuilder) WithSuccessfulEnvironmentConfigsFetch(configs []*un.Unstructured) *MockEnvironmentClientBuilder {
	return b.WithGetEnvironmentConfigs(func(context.Context) ([]*un.Unstructured, error) {
		return configs, nil
	})
}

// WithGetEnvironmentConfig sets the GetEnvironmentConfig behavior.
func (b *MockEnvironmentClientBuilder) WithGetEnvironmentConfig(fn func(context.Context, string) (*un.Unstructured, error)) *MockEnvironmentClientBuilder {
	b.mock.GetEnvironmentConfigFn = fn
	return b
}

// Build returns the built mock.
func (b *MockEnvironmentClientBuilder) Build() *MockEnvironmentClient {
	return b.mock
}

// MockDefinitionClientBuilder helps build crossplane.DefinitionClient mocks.
type MockDefinitionClientBuilder struct {
	mock *MockDefinitionClient
}

// NewMockDefinitionClient creates a new MockDefinitionClientBuilder.
func NewMockDefinitionClient() *MockDefinitionClientBuilder {
	return &MockDefinitionClientBuilder{
		mock: &MockDefinitionClient{},
	}
}

// WithInitialize sets the Initialize behavior.
func (b *MockDefinitionClientBuilder) WithInitialize(fn func(context.Context) error) *MockDefinitionClientBuilder {
	b.mock.InitializeFn = fn
	return b
}

// WithSuccessfulInitialize mocks a successful call to Initialize.
func (b *MockDefinitionClientBuilder) WithSuccessfulInitialize() *MockDefinitionClientBuilder {
	b.mock.InitializeFn = func(context.Context) error {
		return nil
	}

	return b
}

// WithFailedInitialize mocks a failed call to Initialize.
func (b *MockDefinitionClientBuilder) WithFailedInitialize(errMsg string) *MockDefinitionClientBuilder {
	b.mock.InitializeFn = func(context.Context) error {
		return errors.New(errMsg)
	}

	return b
}

// WithGetXRDs sets the GetXRDs behavior.
func (b *MockDefinitionClientBuilder) WithGetXRDs(fn func(context.Context) ([]*un.Unstructured, error)) *MockDefinitionClientBuilder {
	b.mock.GetXRDsFn = fn
	return b
}

// WithSuccessfulXRDsFetch sets GetXRDs to return specific XRDs.
func (b *MockDefinitionClientBuilder) WithSuccessfulXRDsFetch(xrds []*un.Unstructured) *MockDefinitionClientBuilder {
	return b.WithGetXRDs(func(context.Context) ([]*un.Unstructured, error) {
		return xrds, nil
	})
}

// WithEmptyXRDsFetch sets GetXRDs to return an empty set of XRDs.
func (b *MockDefinitionClientBuilder) WithEmptyXRDsFetch() *MockDefinitionClientBuilder {
	return b.WithGetXRDs(func(context.Context) ([]*un.Unstructured, error) {
		return []*un.Unstructured{}, nil
	})
}

// WithFailedXRDsFetch sets GetXRDs to return an error.
func (b *MockDefinitionClientBuilder) WithFailedXRDsFetch(errMsg string) *MockDefinitionClientBuilder {
	return b.WithGetXRDs(func(context.Context) ([]*un.Unstructured, error) {
		return nil, errors.New(errMsg)
	})
}

// WithGetXRDForClaim sets the GetXRDForClaim behavior.
func (b *MockDefinitionClientBuilder) WithGetXRDForClaim(fn func(context.Context, schema.GroupVersionKind) (*un.Unstructured, error)) *MockDefinitionClientBuilder {
	b.mock.GetXRDForClaimFn = fn
	return b
}

// WithGetXRDForXR sets the GetXRDForXR behavior.
func (b *MockDefinitionClientBuilder) WithGetXRDForXR(fn func(context.Context, schema.GroupVersionKind) (*un.Unstructured, error)) *MockDefinitionClientBuilder {
	b.mock.GetXRDForXRFn = fn
	return b
}

// WithV1XRDForXR sets the GetXRDForXR behavior to return a v1 XRD.
func (b *MockDefinitionClientBuilder) WithV1XRDForXR() *MockDefinitionClientBuilder {
	return b.WithGetXRDForXR(func(_ context.Context, _ schema.GroupVersionKind) (*un.Unstructured, error) {
		return &un.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "apiextensions.crossplane.io/v1",
			},
		}, nil
	})
}

// WithV2XRDForXR sets the GetXRDForXR behavior to return a v1 XRD.
func (b *MockDefinitionClientBuilder) WithV2XRDForXR() *MockDefinitionClientBuilder {
	return b.WithGetXRDForXR(func(_ context.Context, _ schema.GroupVersionKind) (*un.Unstructured, error) {
		return &un.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "apiextensions.crossplane.io/v2",
			},
		}, nil
	})
}

// WithXRDForXR sets the GetXRDForXR behavior to return the specified XR.
func (b *MockDefinitionClientBuilder) WithXRDForXR(unstructured *un.Unstructured) *MockDefinitionClientBuilder {
	return b.WithGetXRDForXR(func(_ context.Context, _ schema.GroupVersionKind) (*un.Unstructured, error) {
		return unstructured, nil
	})
}

// WithXRDForClaim sets the GetXRDForXR behavior to return the specified XR.
func (b *MockDefinitionClientBuilder) WithXRDForClaim(unstructured *un.Unstructured) *MockDefinitionClientBuilder {
	return b.WithGetXRDForClaim(func(_ context.Context, _ schema.GroupVersionKind) (*un.Unstructured, error) {
		return unstructured, nil
	})
}

// WithIsClaimResource sets the IsClaimResource behavior.
func (b *MockDefinitionClientBuilder) WithIsClaimResource(fn func(context.Context, *un.Unstructured) bool) *MockDefinitionClientBuilder {
	b.mock.IsClaimResourceFn = fn
	return b
}

// Build returns the built mock.
func (b *MockDefinitionClientBuilder) Build() *MockDefinitionClient {
	return b.mock
}

// MockResourceTreeClientBuilder helps build crossplane.ResourceTreeClient mocks.
type MockResourceTreeClientBuilder struct {
	mock *MockResourceTreeClient
}

// NewMockResourceTreeClient creates a new MockResourceTreeClientBuilder.
func NewMockResourceTreeClient() *MockResourceTreeClientBuilder {
	return &MockResourceTreeClientBuilder{
		mock: &MockResourceTreeClient{},
	}
}

// WithInitialize sets the Initialize behavior.
func (b *MockResourceTreeClientBuilder) WithInitialize(fn func(context.Context) error) *MockResourceTreeClientBuilder {
	b.mock.InitializeFn = fn
	return b
}

// WithSuccessfulInitialize mocks a successful call to Initialize.
func (b *MockResourceTreeClientBuilder) WithSuccessfulInitialize() *MockResourceTreeClientBuilder {
	b.mock.InitializeFn = func(context.Context) error {
		return nil
	}

	return b
}

// WithFailedInitialize mocks a failed call to Initialize.
func (b *MockResourceTreeClientBuilder) WithFailedInitialize(errMsg string) *MockResourceTreeClientBuilder {
	b.mock.InitializeFn = func(context.Context) error {
		return errors.New(errMsg)
	}

	return b
}

// WithGetResourceTree sets the GetResourceTree behavior.
func (b *MockResourceTreeClientBuilder) WithGetResourceTree(fn func(context.Context, *un.Unstructured) (*resource.Resource, error)) *MockResourceTreeClientBuilder {
	b.mock.GetResourceTreeFn = fn
	return b
}

// WithSuccessfulResourceTreeFetch sets GetResourceTree to return a specific tree.
func (b *MockResourceTreeClientBuilder) WithSuccessfulResourceTreeFetch(resourceTree *resource.Resource) *MockResourceTreeClientBuilder {
	return b.WithGetResourceTree(func(context.Context, *un.Unstructured) (*resource.Resource, error) {
		return resourceTree, nil
	})
}

// WithEmptyResourceTree sets GetResourceTree to return just the root with no children.
func (b *MockResourceTreeClientBuilder) WithEmptyResourceTree() *MockResourceTreeClientBuilder {
	return b.WithGetResourceTree(func(_ context.Context, root *un.Unstructured) (*resource.Resource, error) {
		return &resource.Resource{
			Unstructured: *root.DeepCopy(),
			Children:     []*resource.Resource{},
		}, nil
	})
}

// WithFailedResourceTreeFetch sets GetResourceTree to return an error.
func (b *MockResourceTreeClientBuilder) WithFailedResourceTreeFetch(errMsg string) *MockResourceTreeClientBuilder {
	return b.WithGetResourceTree(func(context.Context, *un.Unstructured) (*resource.Resource, error) {
		return nil, errors.New(errMsg)
	})
}

// WithResourceTreeFromXRAndComposed creates a basic resource tree from an XR and composed resources.
func (b *MockResourceTreeClientBuilder) WithResourceTreeFromXRAndComposed(xr *un.Unstructured, composed []*un.Unstructured) *MockResourceTreeClientBuilder {
	return b.WithGetResourceTree(func(_ context.Context, root *un.Unstructured) (*resource.Resource, error) {
		// Make sure we're looking for the right XR
		if root.GetName() != xr.GetName() || root.GetKind() != xr.GetKind() {
			return nil, errors.Errorf("unexpected resource %s/%s", root.GetKind(), root.GetName())
		}

		// Create the resource tree with the XR as root
		resourceTree := &resource.Resource{
			Unstructured: *xr.DeepCopy(),
			Children:     make([]*resource.Resource, 0, len(composed)),
		}

		// Add composed resources as children
		for _, comp := range composed {
			resourceTree.Children = append(resourceTree.Children, &resource.Resource{
				Unstructured: *comp.DeepCopy(),
				Children:     []*resource.Resource{},
			})
		}

		return resourceTree, nil
	})
}

// Build returns the built mock.
func (b *MockResourceTreeClientBuilder) Build() *MockResourceTreeClient {
	return b.mock
}

// endregion

// region DiffProcessor mock builder

// ======================================================================================
// DiffProcessor Mock Builder
// ======================================================================================

// DiffProcessorBuilder helps build mock DiffProcessor instances.
type DiffProcessorBuilder struct {
	mock *MockDiffProcessor
}

// NewMockDiffProcessor creates a new DiffProcessorBuilder.
func NewMockDiffProcessor() *DiffProcessorBuilder {
	return &DiffProcessorBuilder{
		mock: &MockDiffProcessor{},
	}
}

// WithInitialize adds an implementation for the Initialize method.
func (b *DiffProcessorBuilder) WithInitialize(fn func(context.Context) error) *DiffProcessorBuilder {
	b.mock.InitializeFn = fn
	return b
}

// WithSuccessfulInitialize sets a successful Initialize implementation.
func (b *DiffProcessorBuilder) WithSuccessfulInitialize() *DiffProcessorBuilder {
	return b.WithInitialize(func(context.Context) error {
		return nil
	})
}

// WithFailedInitialize sets a failing Initialize implementation.
func (b *DiffProcessorBuilder) WithFailedInitialize(errMsg string) *DiffProcessorBuilder {
	return b.WithInitialize(func(context.Context) error {
		return errors.New(errMsg)
	})
}

// WithPerformDiff adds an implementation for the PerformDiff method.
func (b *DiffProcessorBuilder) WithPerformDiff(fn func(io.Writer, context.Context, []*un.Unstructured) error) *DiffProcessorBuilder {
	b.mock.PerformDiffFn = fn
	return b
}

// WithSuccessfulPerformDiff sets a successful PerformDiff implementation.
func (b *DiffProcessorBuilder) WithSuccessfulPerformDiff() *DiffProcessorBuilder {
	return b.WithPerformDiff(func(io.Writer, context.Context, []*un.Unstructured) error {
		return nil
	})
}

// WithDiffOutput sets a PerformDiff implementation that writes a specific output.
func (b *DiffProcessorBuilder) WithDiffOutput(output string) *DiffProcessorBuilder {
	return b.WithPerformDiff(func(stdout io.Writer, _ context.Context, _ []*un.Unstructured) error {
		if stdout != nil {
			_, _ = io.WriteString(stdout, output)
		}

		return nil
	})
}

// WithFailedPerformDiff sets a failing PerformDiff implementation.
func (b *DiffProcessorBuilder) WithFailedPerformDiff(errMsg string) *DiffProcessorBuilder {
	return b.WithPerformDiff(func(io.Writer, context.Context, []*un.Unstructured) error {
		return errors.New(errMsg)
	})
}

// Build creates and returns the configured mock DiffProcessor.
func (b *DiffProcessorBuilder) Build() *MockDiffProcessor {
	return b.mock
}

// endregion

// region Resource builders

// ======================================================================================
// Resource Building Helpers
// ======================================================================================

// ResourceBuilder helps construct unstructured resources for testing.
type ResourceBuilder struct {
	resource *un.Unstructured
}

// NewResource creates a new ResourceBuilder.
func NewResource(apiVersion, kind, name string) *ResourceBuilder {
	return &ResourceBuilder{
		resource: &un.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": apiVersion,
				"kind":       kind,
				"metadata": map[string]interface{}{
					"name": name,
				},
			},
		},
	}
}

// InNamespace sets the namespace for the resource.
func (b *ResourceBuilder) InNamespace(namespace string) *ResourceBuilder {
	if namespace != "" {
		b.resource.SetNamespace(namespace)
	}

	return b
}

// WithGenerateName sets the namespace for the resource.
func (b *ResourceBuilder) WithGenerateName(generateName string) *ResourceBuilder {
	if generateName != "" {
		b.resource.SetGenerateName(generateName)
	}

	return b
}

// WithLabels adds labels to the resource.
func (b *ResourceBuilder) WithLabels(labels map[string]string) *ResourceBuilder {
	if len(labels) > 0 {
		b.resource.SetLabels(labels)
	}

	return b
}

// WithAnnotations adds annotations to the resource.
func (b *ResourceBuilder) WithAnnotations(annotations map[string]string) *ResourceBuilder {
	if len(annotations) > 0 {
		b.resource.SetAnnotations(annotations)
	}

	return b
}

// WithSpec sets the spec field of the resource.
func (b *ResourceBuilder) WithSpec(spec map[string]interface{}) *ResourceBuilder {
	if len(spec) > 0 {
		_ = un.SetNestedMap(b.resource.Object, spec, "spec")
	}

	return b
}

// WithSpecField sets a specific field in the spec.
func (b *ResourceBuilder) WithSpecField(name string, value interface{}) *ResourceBuilder {
	spec, _, _ := un.NestedMap(b.resource.Object, "spec")
	if spec == nil {
		spec = map[string]interface{}{}
	}

	spec[name] = value
	_ = un.SetNestedMap(b.resource.Object, spec, "spec")

	return b
}

// WithStatus sets the status field of the resource.
func (b *ResourceBuilder) WithStatus(status map[string]interface{}) *ResourceBuilder {
	if len(status) > 0 {
		_ = un.SetNestedMap(b.resource.Object, status, "status")
	}

	return b
}

// WithStatusField sets a specific field in the status.
func (b *ResourceBuilder) WithStatusField(name string, value interface{}) *ResourceBuilder {
	status, _, _ := un.NestedMap(b.resource.Object, "status")
	if status == nil {
		status = map[string]interface{}{}
	}

	status[name] = value
	_ = un.SetNestedMap(b.resource.Object, status, "status")

	return b
}

// WithOwnerReference appends an owner ref to a resource.
func (b *ResourceBuilder) WithOwnerReference(kind, name, apiVersion, uid string) *ResourceBuilder {
	// Get existing owner references, or create an empty slice if none exist
	ownerRefs := b.resource.GetOwnerReferences()

	// Create the new owner reference
	newOwnerRef := metav1.OwnerReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		UID:        types.UID(uid),
	}

	// Append the new owner reference
	ownerRefs = append(ownerRefs, newOwnerRef)

	// Set the updated owner references on the resource
	b.resource.SetOwnerReferences(ownerRefs)

	return b
}

// WithCompositeOwner sets up the resource as a cpd resource with the given composite owner.
func (b *ResourceBuilder) WithCompositeOwner(owner string) *ResourceBuilder {
	// Add standard Crossplane labels and annotations for a cpd resource
	labels := b.resource.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}

	labels["crossplane.io/composite"] = owner
	b.resource.SetLabels(labels)

	return b
}

// WithCompositionResourceName sets the composition resource name annotation.
func (b *ResourceBuilder) WithCompositionResourceName(name string) *ResourceBuilder {
	annotations := b.resource.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations["crossplane.io/composition-resource-name"] = name
	b.resource.SetAnnotations(annotations)

	return b
}

// Build returns the built unstructured resource.
func (b *ResourceBuilder) Build() *un.Unstructured {
	return b.resource.DeepCopy()
}

// BuildUComposite returns the built unstructured resource as a *cmp.Unstructured.
func (b *ResourceBuilder) BuildUComposite() *cmp.Unstructured {
	built := &cmp.Unstructured{}
	built.SetUnstructuredContent(b.Build().UnstructuredContent())

	return built
}

// BuildUComposed returns the built unstructured resource as a *cpd.Unstructured.
func (b *ResourceBuilder) BuildUComposed() *cpd.Unstructured {
	built := &cpd.Unstructured{}
	built.SetUnstructuredContent(b.Build().UnstructuredContent())

	return built
}

// endregion

// region CRD builders

// ======================================================================================
// CRD Building Helpers
// ======================================================================================

// CRDBuilder helps construct CRD resources for testing.
type CRDBuilder struct {
	crd *extv1.CustomResourceDefinition
}

// NewCRD creates a new CRDBuilder.
func NewCRD(name, group, kind string) *CRDBuilder {
	return &CRDBuilder{
		crd: &extv1.CustomResourceDefinition{
			TypeMeta: metav1.TypeMeta{
				Kind:       "CustomResourceDefinition",
				APIVersion: "apiextensions.k8s.io/v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: extv1.CustomResourceDefinitionSpec{
				Group: group,
				Names: extv1.CustomResourceDefinitionNames{
					Kind: kind,
				},
				Scope: extv1.NamespaceScoped,
				Versions: []extv1.CustomResourceDefinitionVersion{
					{
						Name:    "v1",
						Served:  true,
						Storage: true,
					},
				},
			},
		},
	}
}

// WithPlural sets the plural name for the CRD.
func (b *CRDBuilder) WithPlural(plural string) *CRDBuilder {
	b.crd.Spec.Names.Plural = plural
	return b
}

// WithSingular sets the singular name for the CRD.
func (b *CRDBuilder) WithSingular(singular string) *CRDBuilder {
	b.crd.Spec.Names.Singular = singular
	return b
}

// WithScope sets the scope (Namespaced or Cluster) for the CRD.
func (b *CRDBuilder) WithScope(scope extv1.ResourceScope) *CRDBuilder {
	b.crd.Spec.Scope = scope
	return b
}

// WithClusterScope sets the CRD to be cluster-scoped.
func (b *CRDBuilder) WithClusterScope() *CRDBuilder {
	return b.WithScope(extv1.ClusterScoped)
}

// WithNamespaceScope sets the CRD to be namespace-scoped.
func (b *CRDBuilder) WithNamespaceScope() *CRDBuilder {
	return b.WithScope(extv1.NamespaceScoped)
}

// WithVersion adds a version to the CRD.
func (b *CRDBuilder) WithVersion(name string, served, storage bool) *CRDBuilder {
	version := extv1.CustomResourceDefinitionVersion{
		Name:    name,
		Served:  served,
		Storage: storage,
	}
	b.crd.Spec.Versions = append(b.crd.Spec.Versions, version)

	return b
}

// WithSchema adds an OpenAPI v3 schema to the first version.
func (b *CRDBuilder) WithSchema(schema *extv1.JSONSchemaProps) *CRDBuilder {
	if len(b.crd.Spec.Versions) > 0 {
		b.crd.Spec.Versions[0].Schema = &extv1.CustomResourceValidation{
			OpenAPIV3Schema: schema,
		}
	}

	return b
}

// WithStringFieldSchema adds a simple string field schema to the CRD.
func (b *CRDBuilder) WithStringFieldSchema(fieldName string) *CRDBuilder {
	schema := &extv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]extv1.JSONSchemaProps{
			"spec": {
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					fieldName: {
						Type: "string",
					},
				},
			},
			"status": {
				Type: "object",
			},
		},
	}

	return b.WithSchema(schema)
}

// WithStandardSchema adds a standard schema with common Kubernetes fields and a spec field.
func (b *CRDBuilder) WithStandardSchema(specFieldName string) *CRDBuilder {
	schema := &extv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]extv1.JSONSchemaProps{
			"apiVersion": {Type: "string"},
			"kind":       {Type: "string"},
			"metadata":   {Type: "object"},
			"spec": {
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					specFieldName: {Type: "string"},
				},
			},
			"status": {Type: "object"},
		},
	}

	return b.WithSchema(schema)
}

// WithListKind sets the ListKind name for the CRD.
func (b *CRDBuilder) WithListKind(listKind string) *CRDBuilder {
	b.crd.Spec.Names.ListKind = listKind
	return b
}

// Build returns the built CRD.
func (b *CRDBuilder) Build() *extv1.CustomResourceDefinition {
	return b.crd.DeepCopy()
}

// endregion

// region XRD builders

// ======================================================================================
// XRD Building Helpers
// ======================================================================================

// XRDBuilder helps construct XRD resources for testing.
type XRDBuilder struct {
	xrd *xpextv1.CompositeResourceDefinition
}

// NewXRD creates a new XRDBuilder.
func NewXRD(name, group, kind string) *XRDBuilder {
	return &XRDBuilder{
		xrd: &xpextv1.CompositeResourceDefinition{
			TypeMeta: metav1.TypeMeta{
				Kind:       "CompositeResourceDefinition",
				APIVersion: "apiextensions.crossplane.io/v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: xpextv1.CompositeResourceDefinitionSpec{
				Group: group,
				Names: extv1.CustomResourceDefinitionNames{
					Kind: kind,
				},
				Versions: []xpextv1.CompositeResourceDefinitionVersion{
					{
						Name:          "v1",
						Served:        true,
						Referenceable: true,
					},
				},
			},
		},
	}
}

// WithPlural sets the plural name for the XRD.
func (b *XRDBuilder) WithPlural(plural string) *XRDBuilder {
	b.xrd.Spec.Names.Plural = plural
	return b
}

// WithSingular sets the singular name for the XRD.
func (b *XRDBuilder) WithSingular(singular string) *XRDBuilder {
	b.xrd.Spec.Names.Singular = singular
	return b
}

// WithClaimNames sets the claim names for the XRD.
func (b *XRDBuilder) WithClaimNames(kind, plural string) *XRDBuilder {
	b.xrd.Spec.ClaimNames = &extv1.CustomResourceDefinitionNames{
		Kind:   kind,
		Plural: plural,
	}

	return b
}

// WithVersion adds a version to the XRD.
func (b *XRDBuilder) WithVersion(name string, served, referenceable bool) *XRDBuilder {
	version := xpextv1.CompositeResourceDefinitionVersion{
		Name:          name,
		Served:        served,
		Referenceable: referenceable,
	}
	b.xrd.Spec.Versions = append(b.xrd.Spec.Versions, version)

	return b
}

// WithSchema adds an OpenAPI v3 schema to the first version.
func (b *XRDBuilder) WithSchema(schema *extv1.JSONSchemaProps) *XRDBuilder {
	if len(b.xrd.Spec.Versions) > 0 {
		// Convert JSONSchemaProps to RawExtension
		rawBytes, err := json.Marshal(schema)
		if err != nil {
			// In tests, this should not happen, but if it does, we'll just skip the schema
			return b
		}

		b.xrd.Spec.Versions[0].Schema = &xpextv1.CompositeResourceValidation{
			OpenAPIV3Schema: runtime.RawExtension{
				Raw: rawBytes,
			},
		}
	}

	return b
}

// WithRawSchema adds a raw JSON schema to the first version.
func (b *XRDBuilder) WithRawSchema(rawJSON []byte) *XRDBuilder {
	if len(b.xrd.Spec.Versions) > 0 {
		b.xrd.Spec.Versions[0].Schema = &xpextv1.CompositeResourceValidation{
			OpenAPIV3Schema: runtime.RawExtension{
				Raw: rawJSON,
			},
		}
	}

	return b
}

// Build returns the built XRD.
func (b *XRDBuilder) Build() *xpextv1.CompositeResourceDefinition {
	return b.xrd.DeepCopy()
}

// BuildAsUnstructured returns the built XRD as an unstructured object.
func (b *XRDBuilder) BuildAsUnstructured() *un.Unstructured {
	xrd := b.Build()

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(xrd)
	if err != nil {
		// This should not happen in tests, but if it does, we'll return an empty unstructured
		return &un.Unstructured{}
	}

	return &un.Unstructured{Object: obj}
}

// endregion

// region Composition builders

// ======================================================================================
// Composition Building Helpers
// ======================================================================================

// CompositionBuilder helps construct Composition objects for testing.
type CompositionBuilder struct {
	composition *xpextv1.Composition
}

// NewComposition creates a new CompositionBuilder.
func NewComposition(name string) *CompositionBuilder {
	return &CompositionBuilder{
		composition: &xpextv1.Composition{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "apiextensions.crossplane.io/v1",
				Kind:       "Composition",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: xpextv1.CompositionSpec{},
		},
	}
}

// WithCompositeTypeRef sets the composite type reference.
func (b *CompositionBuilder) WithCompositeTypeRef(apiVersion, kind string) *CompositionBuilder {
	b.composition.Spec.CompositeTypeRef = xpextv1.TypeReference{
		APIVersion: apiVersion,
		Kind:       kind,
	}

	return b
}

// WithPipelineMode sets the composition mode to pipeline.
func (b *CompositionBuilder) WithPipelineMode() *CompositionBuilder {
	b.composition.Spec.Mode = xpextv1.CompositionModePipeline
	return b
}

// WithPipelineStep adds a pipeline step to the composition.
func (b *CompositionBuilder) WithPipelineStep(step, functionName string, input map[string]interface{}) *CompositionBuilder {
	var rawInput *runtime.RawExtension

	if input != nil {
		// Properly serialize the map to JSON bytes
		jsonBytes, err := json.Marshal(input)
		if err == nil {
			rawInput = &runtime.RawExtension{
				Raw: jsonBytes,
			}
		}
	}

	b.composition.Spec.Pipeline = append(b.composition.Spec.Pipeline, xpextv1.PipelineStep{
		Step:        step,
		FunctionRef: xpextv1.FunctionReference{Name: functionName},
		Input:       rawInput,
	})

	return b
}

// Build returns the built Composition.
func (b *CompositionBuilder) Build() *xpextv1.Composition {
	return b.composition.DeepCopy()
}

// endregion
