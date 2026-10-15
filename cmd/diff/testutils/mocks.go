package testutils

import (
	"context"
	"io"
	"slices"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/types"
	"github.com/crossplane/cli/v2/cmd/crossplane/common/resource"
	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"

	xpextv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
)

// duplicate these interfaces to avoid cyclical dependency:

// DiffProcessor defines the interface for processing resources for diffing.
type DiffProcessor interface {
	Initialize(ctx context.Context) error
	ProcessAll(stdout io.Writer, ctx context.Context, resources []*un.Unstructured) error
	ProcessResource(stdout io.Writer, ctx context.Context, res *un.Unstructured) error
}

// region MockDynamicClient

// MockDynamicClient mocks the dynamic.Interface.
type MockDynamicClient struct {
	ResourceFn func(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface
}

// Resource implements the dynamic.Interface method.
func (m *MockDynamicClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return m.ResourceFn(gvr)
}

// endregion

// region MockNamespaceableResourceInterface

// MockNamespaceableResourceInterface implements dynamic.NamespaceableResourceInterface.
type MockNamespaceableResourceInterface struct {
	NamespaceFn func(namespace string) dynamic.ResourceInterface
	GetFn       func(ctx context.Context, name string, options metav1.GetOptions, subresources ...string) (*un.Unstructured, error)
	ListFn      func(ctx context.Context, opts metav1.ListOptions) (*un.UnstructuredList, error)
	CreateFn    func(ctx context.Context, obj *un.Unstructured, options metav1.CreateOptions, subresources ...string) (*un.Unstructured, error)
	UpdateFn    func(ctx context.Context, obj *un.Unstructured, options metav1.UpdateOptions, subresources ...string) (*un.Unstructured, error)
	DeleteFn    func(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error
	PatchFn     func(ctx context.Context, name string, pt k8stypes.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*un.Unstructured, error)
}

// Namespace implements dynamic.NamespaceableResourceInterface.
func (m *MockNamespaceableResourceInterface) Namespace(namespace string) dynamic.ResourceInterface {
	if m.NamespaceFn != nil {
		return m.NamespaceFn(namespace)
	}

	return &MockResourceInterface{
		GetFn:    m.GetFn,
		ListFn:   m.ListFn,
		CreateFn: m.CreateFn,
		UpdateFn: m.UpdateFn,
		DeleteFn: m.DeleteFn,
		PatchFn:  m.PatchFn,
	}
}

// Create implements dynamic.ResourceInterface.
func (m *MockNamespaceableResourceInterface) Create(ctx context.Context, obj *un.Unstructured, options metav1.CreateOptions, subresources ...string) (*un.Unstructured, error) {
	if m.CreateFn != nil {
		return m.CreateFn(ctx, obj, options, subresources...)
	}

	return nil, nil
}

// Update implements dynamic.ResourceInterface.
func (m *MockNamespaceableResourceInterface) Update(ctx context.Context, obj *un.Unstructured, options metav1.UpdateOptions, subresources ...string) (*un.Unstructured, error) {
	if m.UpdateFn != nil {
		return m.UpdateFn(ctx, obj, options, subresources...)
	}

	return nil, nil
}

// UpdateStatus implements dynamic.ResourceInterface.
func (m *MockNamespaceableResourceInterface) UpdateStatus(_ context.Context, _ *un.Unstructured, _ metav1.UpdateOptions) (*un.Unstructured, error) {
	return nil, nil
}

// Delete implements dynamic.ResourceInterface.
func (m *MockNamespaceableResourceInterface) Delete(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error {
	if m.DeleteFn != nil {
		return m.DeleteFn(ctx, name, options, subresources...)
	}

	return nil
}

// DeleteCollection implements dynamic.ResourceInterface.
func (m *MockNamespaceableResourceInterface) DeleteCollection(_ context.Context, _ metav1.DeleteOptions, _ metav1.ListOptions) error {
	return nil
}

// Get implements dynamic.ResourceInterface.
func (m *MockNamespaceableResourceInterface) Get(ctx context.Context, name string, options metav1.GetOptions, subresources ...string) (*un.Unstructured, error) {
	if m.GetFn != nil {
		return m.GetFn(ctx, name, options, subresources...)
	}

	return nil, nil
}

// List implements dynamic.ResourceInterface.
func (m *MockNamespaceableResourceInterface) List(ctx context.Context, opts metav1.ListOptions) (*un.UnstructuredList, error) {
	if m.ListFn != nil {
		return m.ListFn(ctx, opts)
	}

	return nil, nil
}

// Watch implements dynamic.ResourceInterface.
func (m *MockNamespaceableResourceInterface) Watch(_ context.Context, _ metav1.ListOptions) (watch.Interface, error) {
	return nil, nil
}

// Patch implements dynamic.ResourceInterface.
func (m *MockNamespaceableResourceInterface) Patch(ctx context.Context, name string, pt k8stypes.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*un.Unstructured, error) {
	if m.PatchFn != nil {
		return m.PatchFn(ctx, name, pt, data, options, subresources...)
	}

	return nil, nil
}

// Apply implements dynamic.ResourceInterface.
func (m *MockNamespaceableResourceInterface) Apply(_ context.Context, _ string, _ *un.Unstructured, _ metav1.ApplyOptions, _ ...string) (*un.Unstructured, error) {
	return nil, nil
}

// ApplyStatus implements dynamic.ResourceInterface.
func (m *MockNamespaceableResourceInterface) ApplyStatus(_ context.Context, _ string, _ *un.Unstructured, _ metav1.ApplyOptions) (*un.Unstructured, error) {
	return nil, nil
}

// endregion

// region MockResourceInterface

// MockResourceInterface mocks dynamic.ResourceInterface for namespaced resources.
type MockResourceInterface struct {
	GetFn    func(ctx context.Context, name string, options metav1.GetOptions, subresources ...string) (*un.Unstructured, error)
	ListFn   func(ctx context.Context, opts metav1.ListOptions) (*un.UnstructuredList, error)
	CreateFn func(ctx context.Context, obj *un.Unstructured, options metav1.CreateOptions, subresources ...string) (*un.Unstructured, error)
	UpdateFn func(ctx context.Context, obj *un.Unstructured, options metav1.UpdateOptions, subresources ...string) (*un.Unstructured, error)
	DeleteFn func(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error
	PatchFn  func(ctx context.Context, name string, pt k8stypes.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*un.Unstructured, error)
}

// Create implements dynamic.ResourceInterface.
func (m *MockResourceInterface) Create(ctx context.Context, obj *un.Unstructured, options metav1.CreateOptions, subresources ...string) (*un.Unstructured, error) {
	if m.CreateFn != nil {
		return m.CreateFn(ctx, obj, options, subresources...)
	}

	return nil, nil
}

// Update implements dynamic.ResourceInterface.
func (m *MockResourceInterface) Update(ctx context.Context, obj *un.Unstructured, options metav1.UpdateOptions, subresources ...string) (*un.Unstructured, error) {
	if m.UpdateFn != nil {
		return m.UpdateFn(ctx, obj, options, subresources...)
	}

	return nil, nil
}

// UpdateStatus implements dynamic.ResourceInterface.
func (m *MockResourceInterface) UpdateStatus(_ context.Context, _ *un.Unstructured, _ metav1.UpdateOptions) (*un.Unstructured, error) {
	return nil, nil
}

// Delete implements dynamic.ResourceInterface.
func (m *MockResourceInterface) Delete(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error {
	if m.DeleteFn != nil {
		return m.DeleteFn(ctx, name, options, subresources...)
	}

	return nil
}

// DeleteCollection implements dynamic.ResourceInterface.
func (m *MockResourceInterface) DeleteCollection(_ context.Context, _ metav1.DeleteOptions, _ metav1.ListOptions) error {
	return nil
}

// Get implements dynamic.ResourceInterface.
func (m *MockResourceInterface) Get(ctx context.Context, name string, options metav1.GetOptions, subresources ...string) (*un.Unstructured, error) {
	if m.GetFn != nil {
		return m.GetFn(ctx, name, options, subresources...)
	}

	return nil, nil
}

// List implements dynamic.ResourceInterface.
func (m *MockResourceInterface) List(ctx context.Context, opts metav1.ListOptions) (*un.UnstructuredList, error) {
	if m.ListFn != nil {
		return m.ListFn(ctx, opts)
	}

	return nil, nil
}

// Watch implements dynamic.ResourceInterface.
func (m *MockResourceInterface) Watch(_ context.Context, _ metav1.ListOptions) (watch.Interface, error) {
	return nil, nil
}

// Patch implements dynamic.ResourceInterface.
func (m *MockResourceInterface) Patch(ctx context.Context, name string, pt k8stypes.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*un.Unstructured, error) {
	if m.PatchFn != nil {
		return m.PatchFn(ctx, name, pt, data, options, subresources...)
	}

	return nil, nil
}

// Apply implements dynamic.ResourceInterface.
func (m *MockResourceInterface) Apply(_ context.Context, _ string, _ *un.Unstructured, _ metav1.ApplyOptions, _ ...string) (*un.Unstructured, error) {
	return nil, nil
}

// ApplyStatus implements dynamic.ResourceInterface.
func (m *MockResourceInterface) ApplyStatus(_ context.Context, _ string, _ *un.Unstructured, _ metav1.ApplyOptions) (*un.Unstructured, error) {
	return nil, nil
}

// endregion

// region MockDiffProcessor

// MockDiffProcessor implements the DiffProcessor interface for testing.
type MockDiffProcessor struct {
	// Function fields for mocking behavior
	InitializeFn         func(ctx context.Context) error
	PerformDiffFn        func(ctx context.Context, resources []*un.Unstructured, compositionProvider types.CompositionProvider) (bool, error)
	DiffSingleResourceFn func(ctx context.Context, res *un.Unstructured, compositionProvider types.CompositionProvider) (map[string]*dt.ResourceDiff, error)
	CleanupFn            func(ctx context.Context) error
}

// Initialize implements the DiffProcessor interface.
func (m *MockDiffProcessor) Initialize(ctx context.Context) error {
	if m.InitializeFn != nil {
		return m.InitializeFn(ctx)
	}

	return nil
}

// PerformDiff implements the DiffProcessor.PerformDiff method.
func (m *MockDiffProcessor) PerformDiff(ctx context.Context, resources []*un.Unstructured, compositionProvider types.CompositionProvider) (bool, error) {
	if m.PerformDiffFn != nil {
		return m.PerformDiffFn(ctx, resources, compositionProvider)
	}

	return false, nil
}

// DiffSingleResource implements the DiffProcessor.DiffSingleResource method.
func (m *MockDiffProcessor) DiffSingleResource(ctx context.Context, res *un.Unstructured, compositionProvider types.CompositionProvider) (map[string]*dt.ResourceDiff, error) {
	if m.DiffSingleResourceFn != nil {
		return m.DiffSingleResourceFn(ctx, res, compositionProvider)
	}

	return make(map[string]*dt.ResourceDiff), nil
}

// Cleanup implements the DiffProcessor.Cleanup method.
func (m *MockDiffProcessor) Cleanup(ctx context.Context) error {
	if m.CleanupFn != nil {
		return m.CleanupFn(ctx)
	}

	return nil
}

// endregion

// region MockSchemaValidator

// MockSchemaValidator Mock schema validator.
type MockSchemaValidator struct {
	ValidateResourcesFn        func(ctx context.Context, xr *un.Unstructured, composed []cpd.Unstructured) error
	ValidateScopeConstraintsFn func(ctx context.Context, resource *un.Unstructured, expectedNamespace string, isClaimRoot bool) error
}

// ValidateResources validates a set of resources against schemas from the cluster.
func (m *MockSchemaValidator) ValidateResources(ctx context.Context, xr *un.Unstructured, composed []cpd.Unstructured) error {
	if m.ValidateResourcesFn != nil {
		return m.ValidateResourcesFn(ctx, xr, composed)
	}

	return nil
}

// EnsureComposedResourceCRDs Implement other required methods of the SchemaValidator interface.
func (m *MockSchemaValidator) EnsureComposedResourceCRDs(_ context.Context, _ []*un.Unstructured) error {
	return nil
}

// ValidateScopeConstraints validates that a resource has the appropriate namespace for its scope.
func (m *MockSchemaValidator) ValidateScopeConstraints(ctx context.Context, resource *un.Unstructured, expectedNamespace string, isClaimRoot bool) error {
	if m.ValidateScopeConstraintsFn != nil {
		return m.ValidateScopeConstraintsFn(ctx, resource, expectedNamespace, isClaimRoot)
	}

	return nil
}

// endregion

// region MockDefaulter

// MockDefaulter Mock defaulter. With no DefaultFn it applies no defaults, returning a copy of its
// input unchanged.
type MockDefaulter struct {
	DefaultFn func(ctx context.Context, obj *un.Unstructured) (*un.Unstructured, string, error)
}

// Default returns a copy of obj with predicted CRD defaults applied, and the storage version of its
// CRD when that is not obj's own (none, without a DefaultFn).
func (m *MockDefaulter) Default(ctx context.Context, obj *un.Unstructured) (*un.Unstructured, string, error) {
	if m.DefaultFn != nil {
		return m.DefaultFn(ctx, obj)
	}

	return obj.DeepCopy(), "", nil
}

// endregion

// region MockInputValidator

// MockInputValidator Mock input validator, for driving PerformDiff's wiring directly.
type MockInputValidator struct {
	ToRenderFn       func() []types.ValidatedInput
	RecordRenderFn   func(i int, rendered map[string]bool, err error)
	VerdictsFn       func(groups []dt.XRDiffGroup) []error
	RenderOverlapsFn func(groups []dt.XRDiffGroup) []error
}

// ToRender returns the inputs to render.
func (m *MockInputValidator) ToRender() []types.ValidatedInput {
	if m.ToRenderFn != nil {
		return m.ToRenderFn()
	}

	return nil
}

// RecordRender records one render.
func (m *MockInputValidator) RecordRender(i int, rendered map[string]bool, err error) {
	if m.RecordRenderFn != nil {
		m.RecordRenderFn(i, rendered, err)
	}
}

// Verdicts returns each group's final error; by default, none.
func (m *MockInputValidator) Verdicts(groups []dt.XRDiffGroup) []error {
	if m.VerdictsFn != nil {
		return m.VerdictsFn(groups)
	}

	return make([]error, len(groups))
}

// RenderOverlaps returns the render-overlap errors; by default, none.
func (m *MockInputValidator) RenderOverlaps(groups []dt.XRDiffGroup) []error {
	if m.RenderOverlapsFn != nil {
		return m.RenderOverlapsFn(groups)
	}

	return nil
}

// endregion

// region Kubernetes client mocks

// ==============================================================================
// Kubernetes API Layer Mocks
// ==============================================================================

// MockResourceClient implements the kubernetes.ResourceClient interface.
type MockResourceClient struct {
	InitializeFn              func(ctx context.Context) error
	GetResourceFn             func(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*un.Unstructured, error)
	ListResourcesFn           func(ctx context.Context, gvk schema.GroupVersionKind, namespace string) ([]*un.Unstructured, error)
	GetResourcesByLabelFn     func(ctx context.Context, gvk schema.GroupVersionKind, namespace string, sel metav1.LabelSelector) ([]*un.Unstructured, error)
	GetAllResourcesByLabelsFn func(ctx context.Context, gvks []schema.GroupVersionKind, selectors []metav1.LabelSelector) ([]*un.Unstructured, error)
	GetGVKsForGroupKindFn     func(ctx context.Context, group, kind string) ([]schema.GroupVersionKind, error)
	IsNamespacedResourceFn    func(ctx context.Context, gvk schema.GroupVersionKind) (bool, error)
}

// Initialize implements kubernetes.ResourceClient.
func (m *MockResourceClient) Initialize(ctx context.Context) error {
	if m.InitializeFn != nil {
		return m.InitializeFn(ctx)
	}

	return nil
}

// GetResource implements kubernetes.ResourceClient.
func (m *MockResourceClient) GetResource(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*un.Unstructured, error) {
	if m.GetResourceFn != nil {
		return m.GetResourceFn(ctx, gvk, namespace, name)
	}

	return nil, errors.New("GetResource not implemented")
}

// ListResources implements kubernetes.ResourceClient.
func (m *MockResourceClient) ListResources(ctx context.Context, gvk schema.GroupVersionKind, namespace string) ([]*un.Unstructured, error) {
	if m.ListResourcesFn != nil {
		return m.ListResourcesFn(ctx, gvk, namespace)
	}

	return nil, errors.New("ListResources not implemented")
}

// GetResourcesByLabel implements kubernetes.ResourceClient.
func (m *MockResourceClient) GetResourcesByLabel(ctx context.Context, gvk schema.GroupVersionKind, namespace string, sel metav1.LabelSelector) ([]*un.Unstructured, error) {
	if m.GetResourcesByLabelFn != nil {
		return m.GetResourcesByLabelFn(ctx, gvk, namespace, sel)
	}

	return nil, errors.New("GetResourcesByLabel not implemented")
}

// GetAllResourcesByLabels implements kubernetes.ResourceClient.
func (m *MockResourceClient) GetAllResourcesByLabels(ctx context.Context, gvks []schema.GroupVersionKind, selectors []metav1.LabelSelector) ([]*un.Unstructured, error) {
	if m.GetAllResourcesByLabelsFn != nil {
		return m.GetAllResourcesByLabelsFn(ctx, gvks, selectors)
	}

	return nil, errors.New("GetAllResourcesByLabels not implemented")
}

// GetGVKsForGroupKind implements kubernetes.ResourceClient.
func (m *MockResourceClient) GetGVKsForGroupKind(ctx context.Context, group, kind string) ([]schema.GroupVersionKind, error) {
	if m.GetGVKsForGroupKindFn != nil {
		return m.GetGVKsForGroupKindFn(ctx, group, kind)
	}

	return nil, errors.New("GetGVKsForGroupKind not implemented")
}

// IsNamespacedResource implements kubernetes.ResourceClient.
func (m *MockResourceClient) IsNamespacedResource(ctx context.Context, gvk schema.GroupVersionKind) (bool, error) {
	if m.IsNamespacedResourceFn != nil {
		return m.IsNamespacedResourceFn(ctx, gvk)
	}

	return false, errors.Errorf("IsNamespacedResource not implemented for %s in mock", gvk.String())
}

// MockSchemaClient implements the kubernetes.SchemaClient interface.
type MockSchemaClient struct {
	InitializeFn       func(ctx context.Context) error
	GetCRDFn           func(ctx context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error)
	GetCRDByNameFn     func(name string) (*extv1.CustomResourceDefinition, error)
	IsCRDRequiredFn    func(ctx context.Context, gvk schema.GroupVersionKind) bool
	ValidateResourceFn func(ctx context.Context, resource *un.Unstructured) error
	LoadCRDsFromXRDsFn func(ctx context.Context, xrds []*un.Unstructured) error
	GetAllCRDsFn       func() []*extv1.CustomResourceDefinition
}

// Initialize implements kubernetes.SchemaClient.
func (m *MockSchemaClient) Initialize(ctx context.Context) error {
	if m.InitializeFn != nil {
		return m.InitializeFn(ctx)
	}

	return nil
}

// GetCRD implements kubernetes.SchemaClient.
func (m *MockSchemaClient) GetCRD(ctx context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
	if m.GetCRDFn != nil {
		return m.GetCRDFn(ctx, gvk)
	}

	return nil, errors.New("GetCRD not implemented")
}

// GetCRDByName implements kubernetes.SchemaClient.
func (m *MockSchemaClient) GetCRDByName(name string) (*extv1.CustomResourceDefinition, error) {
	if m.GetCRDByNameFn != nil {
		return m.GetCRDByNameFn(name)
	}

	return nil, errors.New("GetCRDByName not implemented")
}

// IsCRDRequired implements kubernetes.SchemaClient.
func (m *MockSchemaClient) IsCRDRequired(ctx context.Context, gvk schema.GroupVersionKind) bool {
	if m.IsCRDRequiredFn != nil {
		return m.IsCRDRequiredFn(ctx, gvk)
	}

	return true // Default to true
}

// ValidateResource implements kubernetes.SchemaClient.
func (m *MockSchemaClient) ValidateResource(ctx context.Context, resource *un.Unstructured) error {
	if m.ValidateResourceFn != nil {
		return m.ValidateResourceFn(ctx, resource)
	}

	return nil
}

// LoadCRDsFromXRDs implements kubernetes.SchemaClient.
func (m *MockSchemaClient) LoadCRDsFromXRDs(ctx context.Context, xrds []*un.Unstructured) error {
	if m.LoadCRDsFromXRDsFn != nil {
		return m.LoadCRDsFromXRDsFn(ctx, xrds)
	}

	return nil
}

// GetAllCRDs implements kubernetes.SchemaClient.
func (m *MockSchemaClient) GetAllCRDs() []*extv1.CustomResourceDefinition {
	if m.GetAllCRDsFn != nil {
		return m.GetAllCRDsFn()
	}

	return []*extv1.CustomResourceDefinition{}
}

// MockApplyClient implements the kubernetes.ApplyClient interface.
type MockApplyClient struct {
	InitializeFn   func(ctx context.Context) error
	ApplyFn        func(ctx context.Context, obj *un.Unstructured) (*un.Unstructured, error)
	DryRunApplyFn  func(ctx context.Context, obj *un.Unstructured, fieldOwner string) (*un.Unstructured, error)
	DryRunCreateFn func(ctx context.Context, obj *un.Unstructured) (*un.Unstructured, error)
}

// Initialize implements kubernetes.ApplyClient.
func (m *MockApplyClient) Initialize(ctx context.Context) error {
	if m.InitializeFn != nil {
		return m.InitializeFn(ctx)
	}

	return nil
}

// Apply implements kubernetes.ApplyClient.
func (m *MockApplyClient) Apply(ctx context.Context, obj *un.Unstructured) (*un.Unstructured, error) {
	if m.ApplyFn != nil {
		return m.ApplyFn(ctx, obj)
	}

	return nil, errors.New("Apply not implemented")
}

// DryRunApply implements kubernetes.ApplyClient.
func (m *MockApplyClient) DryRunApply(ctx context.Context, obj *un.Unstructured, fieldOwner string) (*un.Unstructured, error) {
	if m.DryRunApplyFn != nil {
		return m.DryRunApplyFn(ctx, obj, fieldOwner)
	}

	return nil, errors.New("DryRunApply not implemented")
}

// DryRunCreate implements kubernetes.ApplyClient.
func (m *MockApplyClient) DryRunCreate(ctx context.Context, obj *un.Unstructured) (*un.Unstructured, error) {
	if m.DryRunCreateFn != nil {
		return m.DryRunCreateFn(ctx, obj)
	}

	return nil, errors.New("DryRunCreate not implemented")
}

// MockAccessChecker implements the kubernetes.AccessChecker interface.
type MockAccessChecker struct {
	CanFn func(ctx context.Context, gvk schema.GroupVersionKind, namespace string, verb types.Verb) (bool, string, error)
}

// Can implements kubernetes.AccessChecker.
//
// The unset default is "allowed", not an error, and that is deliberate. The production code only
// consults the authorizer after a dry run has already come back Forbidden, and "allowed" is the
// answer that routes such a 403 to the cluster-rejection path. So a test that wires a Forbidden
// without saying anything about authorization gets the rejection behaviour rather than a confusing
// "Can not implemented" failure from a collaborator it never meant to exercise.
func (m *MockAccessChecker) Can(ctx context.Context, gvk schema.GroupVersionKind, namespace string, verb types.Verb) (bool, string, error) {
	if m.CanFn != nil {
		return m.CanFn(ctx, gvk, namespace, verb)
	}

	return true, "", nil
}

// MockTypeConverter implements the kubernetes.TypeConverter interface.
type MockTypeConverter struct {
	GVKToGVRFn              func(ctx context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error)
	GetResourceNameForGVKFn func(ctx context.Context, gvk schema.GroupVersionKind) (string, error)
}

// GVKToGVR implements kubernetes.TypeConverter.
func (m *MockTypeConverter) GVKToGVR(ctx context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	if m.GVKToGVRFn != nil {
		return m.GVKToGVRFn(ctx, gvk)
	}

	return schema.GroupVersionResource{}, errors.New("GVKToGVR not implemented")
}

// GetResourceNameForGVK implements kubernetes.TypeConverter.
func (m *MockTypeConverter) GetResourceNameForGVK(ctx context.Context, gvk schema.GroupVersionKind) (string, error) {
	if m.GetResourceNameForGVKFn != nil {
		return m.GetResourceNameForGVKFn(ctx, gvk)
	}

	return "", errors.New("GetResourceNameForGVK not implemented")
}

// endregion

// region Crossplane client mocks

// ==============================================================================
// Crossplane API Layer Mocks
// ==============================================================================

// MockCompositionClient implements the crossplane.CompositionClient interface.
type MockCompositionClient struct {
	InitializeFn              func(ctx context.Context) error
	FindMatchingCompositionFn func(ctx context.Context, res *un.Unstructured) (types.ResolvedComposition, error)
	ListCompositionsFn        func(ctx context.Context) ([]*xpextv1.Composition, error)
	GetCompositionFn          func(ctx context.Context, name string) (*xpextv1.Composition, error)
	FindCompositesFn          func(ctx context.Context, comp *un.Unstructured, opts types.FindCompositesOptions) ([]*un.Unstructured, error)
}

// Initialize implements crossplane.CompositionClient.
func (m *MockCompositionClient) Initialize(ctx context.Context) error {
	if m.InitializeFn != nil {
		return m.InitializeFn(ctx)
	}

	return nil
}

// FindMatchingComposition implements crossplane.CompositionClient.
func (m *MockCompositionClient) FindMatchingComposition(ctx context.Context, res *un.Unstructured) (types.ResolvedComposition, error) {
	if m.FindMatchingCompositionFn != nil {
		return m.FindMatchingCompositionFn(ctx, res)
	}

	return types.ResolvedComposition{}, errors.New("FindMatchingComposition not implemented")
}

// ListCompositions implements crossplane.CompositionClient.
func (m *MockCompositionClient) ListCompositions(ctx context.Context) ([]*xpextv1.Composition, error) {
	if m.ListCompositionsFn != nil {
		return m.ListCompositionsFn(ctx)
	}

	return nil, errors.New("ListCompositions not implemented")
}

// GetComposition implements crossplane.CompositionClient.
func (m *MockCompositionClient) GetComposition(ctx context.Context, name string) (*xpextv1.Composition, error) {
	if m.GetCompositionFn != nil {
		return m.GetCompositionFn(ctx, name)
	}

	return nil, errors.New("GetComposition not implemented")
}

// FindComposites implements crossplane.CompositionClient.
func (m *MockCompositionClient) FindComposites(ctx context.Context, comp *un.Unstructured, opts types.FindCompositesOptions) ([]*un.Unstructured, error) {
	if m.FindCompositesFn != nil {
		return m.FindCompositesFn(ctx, comp, opts)
	}

	return nil, errors.New("FindComposites not implemented")
}

// MockFunctionClient implements the crossplane.FunctionClient interface.
type MockFunctionClient struct {
	InitializeFn               func(ctx context.Context) error
	GetFunctionsFromPipelineFn func(comp *xpextv1.Composition) ([]pkgv1.Function, error)
	ListFunctionsFn            func(ctx context.Context) ([]pkgv1.Function, error)
}

// Initialize implements crossplane.FunctionClient.
func (m *MockFunctionClient) Initialize(ctx context.Context) error {
	if m.InitializeFn != nil {
		return m.InitializeFn(ctx)
	}

	return nil
}

// GetFunctionsFromPipeline implements crossplane.FunctionClient.
func (m *MockFunctionClient) GetFunctionsFromPipeline(comp *xpextv1.Composition) ([]pkgv1.Function, error) {
	if m.GetFunctionsFromPipelineFn != nil {
		return m.GetFunctionsFromPipelineFn(comp)
	}

	return nil, errors.New("GetFunctionsFromPipeline not implemented")
}

// ListFunctions implements crossplane.FunctionClient.
func (m *MockFunctionClient) ListFunctions(ctx context.Context) ([]pkgv1.Function, error) {
	if m.ListFunctionsFn != nil {
		return m.ListFunctionsFn(ctx)
	}

	return nil, errors.New("ListFunctions not implemented")
}

// MockEnvironmentClient implements the crossplane.EnvironmentClient interface.
type MockEnvironmentClient struct {
	InitializeFn            func(ctx context.Context) error
	GetEnvironmentConfigsFn func(ctx context.Context) ([]*un.Unstructured, error)
	GetEnvironmentConfigFn  func(ctx context.Context, name string) (*un.Unstructured, error)
}

// Initialize implements crossplane.EnvironmentClient.
func (m *MockEnvironmentClient) Initialize(ctx context.Context) error {
	if m.InitializeFn != nil {
		return m.InitializeFn(ctx)
	}

	return nil
}

// GetEnvironmentConfigs implements crossplane.EnvironmentClient.
func (m *MockEnvironmentClient) GetEnvironmentConfigs(ctx context.Context) ([]*un.Unstructured, error) {
	if m.GetEnvironmentConfigsFn != nil {
		return m.GetEnvironmentConfigsFn(ctx)
	}

	return nil, errors.New("GetEnvironmentConfigs not implemented")
}

// GetEnvironmentConfig implements crossplane.EnvironmentClient.
func (m *MockEnvironmentClient) GetEnvironmentConfig(ctx context.Context, name string) (*un.Unstructured, error) {
	if m.GetEnvironmentConfigFn != nil {
		return m.GetEnvironmentConfigFn(ctx, name)
	}

	return nil, errors.New("GetEnvironmentConfig not implemented")
}

// MockDefinitionClient implements the crossplane.DefinitionClient interface.
type MockDefinitionClient struct {
	InitializeFn         func(ctx context.Context) error
	GetXRDsFn            func(ctx context.Context) ([]*un.Unstructured, error)
	GetXRDForClaimFn     func(ctx context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error)
	GetXRDForXRFn        func(ctx context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error)
	IsClaimResourceFn    func(ctx context.Context, resource *un.Unstructured) bool
	GetCompositeSchemaFn func(ctx context.Context, gvk schema.GroupVersionKind) (cmp.Schema, error)
}

// Initialize implements crossplane.DefinitionClient.
func (m *MockDefinitionClient) Initialize(ctx context.Context) error {
	if m.InitializeFn != nil {
		return m.InitializeFn(ctx)
	}

	return nil
}

// GetXRDs implements crossplane.DefinitionClient.
func (m *MockDefinitionClient) GetXRDs(ctx context.Context) ([]*un.Unstructured, error) {
	if m.GetXRDsFn != nil {
		return m.GetXRDsFn(ctx)
	}

	return nil, errors.New("GetXRDs not implemented")
}

// GetXRDForClaim implements crossplane.DefinitionClient.
func (m *MockDefinitionClient) GetXRDForClaim(ctx context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
	if m.GetXRDForClaimFn != nil {
		return m.GetXRDForClaimFn(ctx, gvk)
	}

	return nil, errors.New("GetXRDForClaim not implemented")
}

// GetXRDForXR implements crossplane.DefinitionClient.
func (m *MockDefinitionClient) GetXRDForXR(ctx context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
	if m.GetXRDForXRFn != nil {
		return m.GetXRDForXRFn(ctx, gvk)
	}

	return nil, errors.New("GetXRDForXR not implemented")
}

// IsClaimResource implements crossplane.DefinitionClient.
func (m *MockDefinitionClient) IsClaimResource(ctx context.Context, resource *un.Unstructured) bool {
	if m.IsClaimResourceFn != nil {
		return m.IsClaimResourceFn(ctx, resource)
	}

	return false
}

// GetCompositeSchema implements crossplane.DefinitionClient.
func (m *MockDefinitionClient) GetCompositeSchema(ctx context.Context, gvk schema.GroupVersionKind) (cmp.Schema, error) {
	if m.GetCompositeSchemaFn != nil {
		return m.GetCompositeSchemaFn(ctx, gvk)
	}

	return cmp.SchemaModern, errors.New("GetCompositeSchema not implemented")
}

// MockResourceTreeClient implements the crossplane.ResourceTreeClient interface.
type MockResourceTreeClient struct {
	InitializeFn      func(ctx context.Context) error
	GetResourceTreeFn func(ctx context.Context, root *un.Unstructured) (*resource.Resource, error)
}

// Initialize implements crossplane.ResourceTreeClient.
func (m *MockResourceTreeClient) Initialize(ctx context.Context) error {
	if m.InitializeFn != nil {
		return m.InitializeFn(ctx)
	}

	return nil
}

// GetResourceTree implements crossplane.ResourceTreeClient.
func (m *MockResourceTreeClient) GetResourceTree(ctx context.Context, root *un.Unstructured) (*resource.Resource, error) {
	if m.GetResourceTreeFn != nil {
		return m.GetResourceTreeFn(ctx, root)
	}

	return nil, errors.New("GetResourceTree not implemented")
}

// MockCredentialClient implements the crossplane.CredentialClient interface.
type MockCredentialClient struct {
	FetchCompositionCredentialsFn func(ctx context.Context, comp *xpextv1.Composition) (types.CredentialFetchResult, error)
}

// FetchCompositionCredentials implements crossplane.CredentialClient.
func (m *MockCredentialClient) FetchCompositionCredentials(ctx context.Context, comp *xpextv1.Composition) (types.CredentialFetchResult, error) {
	if m.FetchCompositionCredentialsFn != nil {
		return m.FetchCompositionCredentialsFn(ctx, comp)
	}

	return types.CredentialFetchResult{}, nil
}

// endregion

// MockDiffCalculator is a mock implementation of DiffCalculator for testing.
type MockDiffCalculator struct {
	CalculateDiffFn                 func(context.Context, *un.Unstructured, *un.Unstructured) (*dt.ResourceDiff, error)
	CalculateDiffsFn                func(context.Context, *cmp.Unstructured, render.CompositionOutputs) (map[string]*dt.ResourceDiff, error)
	CalculateNonRemovalDiffsFn      func(context.Context, *cmp.Unstructured, render.CompositionOutputs) (map[string]*dt.ResourceDiff, types.RenderedResources, error)
	CalculateRemovedResourceDiffsFn func(context.Context, *un.Unstructured, types.RenderedResources) (map[string]*dt.ResourceDiff, error)
}

// CalculateDiff implements DiffCalculator.
func (m *MockDiffCalculator) CalculateDiff(ctx context.Context, composite *un.Unstructured, desired *un.Unstructured) (*dt.ResourceDiff, error) {
	if m.CalculateDiffFn != nil {
		return m.CalculateDiffFn(ctx, composite, desired)
	}

	return nil, nil
}

// CalculateDiffs implements DiffCalculator.
func (m *MockDiffCalculator) CalculateDiffs(ctx context.Context, xr *cmp.Unstructured, desired render.CompositionOutputs) (map[string]*dt.ResourceDiff, error) {
	if m.CalculateDiffsFn != nil {
		return m.CalculateDiffsFn(ctx, xr, desired)
	}

	return nil, nil
}

// CalculateNonRemovalDiffs implements DiffCalculator.
func (m *MockDiffCalculator) CalculateNonRemovalDiffs(ctx context.Context, xr *cmp.Unstructured, desired render.CompositionOutputs) (map[string]*dt.ResourceDiff, types.RenderedResources, error) {
	if m.CalculateNonRemovalDiffsFn != nil {
		return m.CalculateNonRemovalDiffsFn(ctx, xr, desired)
	}

	return nil, types.RenderedResources{}, nil
}

// CalculateRemovedResourceDiffs implements DiffCalculator.
func (m *MockDiffCalculator) CalculateRemovedResourceDiffs(ctx context.Context, xr *un.Unstructured, rendered types.RenderedResources) (map[string]*dt.ResourceDiff, error) {
	if m.CalculateRemovedResourceDiffsFn != nil {
		return m.CalculateRemovedResourceDiffsFn(ctx, xr, rendered)
	}

	return nil, nil
}

// MockDiffRenderer provides a mock implementation for DiffRenderer.
type MockDiffRenderer struct {
	RenderDiffsFn func([]dt.XRDiffGroup, []dt.OutputError, []dt.OutputWarning) error
}

// RenderDiffs implements RenderDiffs from the DiffRenderer interface.
func (m *MockDiffRenderer) RenderDiffs(groups []dt.XRDiffGroup, errs []dt.OutputError, warnings []dt.OutputWarning) error {
	if m.RenderDiffsFn != nil {
		return m.RenderDiffsFn(groups, errs, warnings)
	}

	return nil
}

// MockCompositionProvider provides a mock implementation for CompositionProvider.
type MockCompositionProvider struct {
	GetCompositionFn func() (*xpextv1.Composition, error)
}

// GetComposition implements GetComposition from the CompositionProvider interface.
func (m *MockCompositionProvider) GetComposition() (*xpextv1.Composition, error) {
	if m.GetCompositionFn != nil {
		return m.GetCompositionFn()
	}

	return &xpextv1.Composition{}, nil
}

// MockFunctionProvider provides a mock implementation for FunctionProvider.
type MockFunctionProvider struct {
	GetFunctionsForCompositionFn func(comp *xpextv1.Composition) ([]pkgv1.Function, error)
	CleanupFn                    func(ctx context.Context) error
}

// GetFunctionsForComposition implements FunctionProvider.
func (m *MockFunctionProvider) GetFunctionsForComposition(comp *xpextv1.Composition) ([]pkgv1.Function, error) {
	if m.GetFunctionsForCompositionFn != nil {
		return m.GetFunctionsForCompositionFn(comp)
	}

	return nil, nil
}

// Cleanup implements FunctionProvider.
func (m *MockFunctionProvider) Cleanup(ctx context.Context) error {
	if m.CleanupFn != nil {
		return m.CleanupFn(ctx)
	}

	return nil
}

// region Docker daemon fake

// FakeDocker is an in-memory Docker daemon: it holds containers and networks, and implements the subset
// of the Docker API client that dockerrun.Docker names. It behaves as the daemon does wherever the
// difference would change which resources are left: listing applies label filters (a resource matches
// only if it carries every one) and, without All, lists only running containers; a running container
// is only removed with Force; and a network is not removed while a container is attached to it. A test
// sets up the host, runs the code, and asserts what is left.
type FakeDocker struct {
	Containers []container.Summary
	Networks   []network.Summary

	// ListErr and NetworkListErr fail every container and network list.
	ListErr, NetworkListErr error
	// RemoveErr fails removing the container or network with that ID, which stays.
	RemoveErr map[string]error
	// Vanished holds the IDs of containers and networks that are listed but gone by the time they are
	// removed, as when a concurrent run removed them first.
	Vanished map[string]bool
}

// DockerSession returns the labels the upstream render engine puts on what it creates for session id.
func DockerSession(id string) map[string]string {
	return map[string]string{render.LabelKeyManagedBy: render.LabelValueManagedByCrossplane, render.LabelKeySession: id}
}

// DockerContainer returns a container in state, carrying labels and attached to the named networks.
func DockerContainer(id string, state container.ContainerState, labels map[string]string, networks ...string) container.Summary {
	c := container.Summary{ID: id, State: state, Labels: labels, NetworkSettings: &container.NetworkSettingsSummary{}}
	if len(networks) > 0 {
		c.NetworkSettings.Networks = make(map[string]*network.EndpointSettings, len(networks))
		for _, n := range networks {
			c.NetworkSettings.Networks[n] = &network.EndpointSettings{}
		}
	}

	return c
}

// DockerNetwork returns a network carrying labels. Its ID is its name.
func DockerNetwork(name string, labels map[string]string) network.Summary {
	return network.Summary{Network: network.Network{ID: name, Name: name, Labels: labels}}
}

// ContainerIDs returns the IDs of the containers on the host, sorted.
func (f *FakeDocker) ContainerIDs() []string {
	ids := make([]string, 0, len(f.Containers))
	for _, c := range f.Containers {
		ids = append(ids, c.ID)
	}

	slices.Sort(ids)

	return ids
}

// NetworkNames returns the names of the networks on the host, sorted.
func (f *FakeDocker) NetworkNames() []string {
	names := make([]string, 0, len(f.Networks))
	for _, n := range f.Networks {
		names = append(names, n.Name)
	}

	slices.Sort(names)

	return names
}

// ContainerList lists the containers matching opts' filters; without opts.All, only running ones.
func (f *FakeDocker) ContainerList(_ context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error) {
	var res client.ContainerListResult

	if f.ListErr != nil {
		return res, f.ListErr
	}

	for _, c := range f.Containers {
		ok, err := matchesDockerFilters(opts.Filters, c.Labels)
		if err != nil {
			return client.ContainerListResult{}, err
		}

		if ok && (opts.All || c.State == container.StateRunning) {
			res.Items = append(res.Items, c)
		}
	}

	return res, nil
}

// ContainerRemove removes the container with ID id.
func (f *FakeDocker) ContainerRemove(_ context.Context, id string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	i := slices.IndexFunc(f.Containers, func(c container.Summary) bool { return c.ID == id })

	switch {
	case i < 0:
		return client.ContainerRemoveResult{}, errors.Wrapf(cerrdefs.ErrNotFound, "no such container: %s", id)
	case f.Vanished[id]:
		f.Containers = slices.Delete(f.Containers, i, i+1)
		return client.ContainerRemoveResult{}, errors.Wrapf(cerrdefs.ErrNotFound, "no such container: %s", id)
	case f.RemoveErr[id] != nil:
		return client.ContainerRemoveResult{}, f.RemoveErr[id]
	case f.Containers[i].State == container.StateRunning && !opts.Force:
		return client.ContainerRemoveResult{}, errors.Wrapf(cerrdefs.ErrConflict, "container %s is running: stop it or force the removal", id)
	}

	f.Containers = slices.Delete(f.Containers, i, i+1)

	return client.ContainerRemoveResult{}, nil
}

// NetworkList lists the networks matching opts' filters.
func (f *FakeDocker) NetworkList(_ context.Context, opts client.NetworkListOptions) (client.NetworkListResult, error) {
	var res client.NetworkListResult

	if f.NetworkListErr != nil {
		return res, f.NetworkListErr
	}

	for _, n := range f.Networks {
		ok, err := matchesDockerFilters(opts.Filters, n.Labels)
		if err != nil {
			return client.NetworkListResult{}, err
		}

		if ok {
			res.Items = append(res.Items, n)
		}
	}

	return res, nil
}

// NetworkRemove removes the network with ID or name id, unless a container is attached to it.
func (f *FakeDocker) NetworkRemove(_ context.Context, id string, _ client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	i := slices.IndexFunc(f.Networks, func(n network.Summary) bool { return n.ID == id || n.Name == id })
	if i < 0 {
		return client.NetworkRemoveResult{}, errors.Wrapf(cerrdefs.ErrNotFound, "network %s not found", id)
	}

	n := f.Networks[i]

	switch {
	case f.Vanished[n.ID]:
		f.Networks = slices.Delete(f.Networks, i, i+1)
		return client.NetworkRemoveResult{}, errors.Wrapf(cerrdefs.ErrNotFound, "network %s not found", id)
	case f.RemoveErr[n.ID] != nil:
		return client.NetworkRemoveResult{}, f.RemoveErr[n.ID]
	case slices.ContainsFunc(f.Containers, func(c container.Summary) bool {
		if c.NetworkSettings == nil {
			return false
		}

		_, attached := c.NetworkSettings.Networks[n.Name]

		return attached
	}):
		return client.NetworkRemoveResult{}, errors.Wrapf(cerrdefs.ErrConflict, "error while removing network: network %s has active endpoints", n.Name)
	}

	f.Networks = slices.Delete(f.Networks, i, i+1)

	return client.NetworkRemoveResult{}, nil
}

// Close does nothing: the fake holds no connection.
func (f *FakeDocker) Close() error {
	return nil
}

// matchesDockerFilters reports whether labels satisfy filters as the daemon's label filter does. The
// fake supports no other filter term, so it fails a list that uses one rather than ignore it.
func matchesDockerFilters(filters client.Filters, labels map[string]string) (bool, error) {
	for term, values := range filters {
		if term != "label" {
			return false, errors.Errorf("fake Docker daemon does not support the %q list filter", term)
		}

		for v := range values {
			k, want, hasValue := strings.Cut(v, "=")
			if got, ok := labels[k]; !ok || (hasValue && got != want) {
				return false, nil
			}
		}
	}

	return true, nil
}

// endregion
