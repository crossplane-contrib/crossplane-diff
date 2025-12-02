package kubernetes

import (
	"context"
	"strings"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	kt "k8s.io/client-go/testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

var _ ResourceClient = (*tu.MockResourceClient)(nil)

func TestResourceClient_GetResource(t *testing.T) {
	scheme := runtime.NewScheme()

	type args struct {
		ctx       context.Context
		gvk       schema.GroupVersionKind
		namespace string
		name      string
	}

	type want struct {
		resource *un.Unstructured
		err      error
	}

	tests := map[string]struct {
		reason string
		setup  func() (dynamic.Interface, TypeConverter)
		args   args
		want   want
	}{
		"NamespacedResourceFound": {
			reason: "Should return the resource when it exists in a namespace",
			setup: func() (dynamic.Interface, TypeConverter) {
				// Use the resource builder to create test objects
				objects := []runtime.Object{
					tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
						InNamespace("test-namespace").
						WithSpecField("property", "value").
						Build(),
				}

				dynamicClient := fake.NewSimpleDynamicClient(scheme, objects...)

				// Create mock type converter using the builder
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(_ context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{
							Group:    gvk.Group,
							Version:  gvk.Version,
							Resource: "exampleresources",
						}, nil
					}).Build()

				return dynamicClient, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "ExampleResource",
				},
				namespace: "test-namespace",
				name:      "test-resource",
			},
			want: want{
				resource: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
					InNamespace("test-namespace").
					WithSpecField("property", "value").
					Build(),
			},
		},
		"ClusterScopedResourceFound": {
			reason: "Should return the resource when it exists at cluster scope",
			setup: func() (dynamic.Interface, TypeConverter) {
				objects := []runtime.Object{
					tu.NewResource("example.org/v1", "ClusterResource", "test-cluster-resource").
						WithSpecField("property", "value").
						Build(),
				}

				dynamicClient := fake.NewSimpleDynamicClient(scheme, objects...)

				// Create mock type converter using the builder
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(_ context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{
							Group:    gvk.Group,
							Version:  gvk.Version,
							Resource: "clusterresources",
						}, nil
					}).Build()

				return dynamicClient, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "ClusterResource",
				},
				namespace: "", // Cluster-scoped
				name:      "test-cluster-resource",
			},
			want: want{
				resource: tu.NewResource("example.org/v1", "ClusterResource", "test-cluster-resource").
					WithSpecField("property", "value").
					Build(),
			},
		},
		"ResourceNotFound": {
			reason: "Should return an error when the resource doesn't exist",
			setup: func() (dynamic.Interface, TypeConverter) {
				dc := fake.NewSimpleDynamicClient(scheme)
				dc.PrependReactor("get", "*", func(kt.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("resource not found")
				})

				// Create mock type converter using the builder
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(_ context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{
							Group:    gvk.Group,
							Version:  gvk.Version,
							Resource: "exampleresources",
						}, nil
					}).Build()

				return dc, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "ExampleResource",
				},
				namespace: "test-namespace",
				name:      "nonexistent-resource",
			},
			want: want{
				resource: nil,
				err:      errors.New("cannot get resource test-namespace/nonexistent-resource of kind ExampleResource"),
			},
		},
		"ConverterError": {
			reason: "Should return an error when GVK to GVR conversion fails",
			setup: func() (dynamic.Interface, TypeConverter) {
				dynamicClient := fake.NewSimpleDynamicClient(scheme)

				// Create mock type converter that returns an error
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(context.Context, schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{}, errors.New("conversion error")
					}).Build()

				return dynamicClient, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "ExampleResource",
				},
				namespace: "test-namespace",
				name:      "test-resource",
			},
			want: want{
				resource: nil,
				err:      errors.New("cannot get resource test-namespace/test-resource of kind ExampleResource"),
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dynamicClient, converter := tc.setup()

			c := &DefaultResourceClient{
				dynamicClient: dynamicClient,
				converter:     converter,
				logger:        tu.TestLogger(t, false),
			}

			got, err := c.GetResource(tc.args.ctx, tc.args.gvk, tc.args.namespace, tc.args.name)

			if tc.want.err != nil {
				if err == nil {
					t.Errorf("\n%s\nGetResource(...): expected error but got none", tc.reason)
					return
				}

				if !strings.Contains(err.Error(), tc.want.err.Error()) {
					t.Errorf("\n%s\nGetResource(...): expected error containing %q, got %q",
						tc.reason, tc.want.err.Error(), err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nGetResource(...): unexpected error: %v", tc.reason, err)
				return
			}

			// Remove resourceVersion from comparison since it's added by the fake client
			gotCopy := got.DeepCopy()
			if gotCopy != nil && gotCopy.Object != nil {
				meta, found, _ := un.NestedMap(gotCopy.Object, "metadata")
				if found && meta != nil {
					delete(meta, "resourceVersion")
					_ = un.SetNestedMap(gotCopy.Object, meta, "metadata")
				}
			}

			if diff := cmp.Diff(tc.want.resource, gotCopy); diff != "" {
				t.Errorf("\n%s\nGetResource(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestResourceClient_GetResourcesByLabel(t *testing.T) {
	scheme := runtime.NewScheme()

	tests := map[string]struct {
		reason string
		setup  func() (dynamic.Interface, TypeConverter)
		args   struct {
			ctx       context.Context
			namespace string
			gvk       schema.GroupVersionKind
			selector  metav1.LabelSelector
		}
		want struct {
			resources []*un.Unstructured
			err       error
		}
	}{
		"NoMatchingResources": {
			reason: "Should return empty list when no resources match selector",
			setup: func() (dynamic.Interface, TypeConverter) {
				dc := fake.NewSimpleDynamicClientWithCustomListKinds(scheme,
					map[schema.GroupVersionResource]string{
						{Group: "example.org", Version: "v1", Resource: "resources"}: "ResourceList",
					})

				// Create mock type converter using the builder
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(_ context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{
							Group:    gvk.Group,
							Version:  gvk.Version,
							Resource: "resources",
						}, nil
					}).Build()

				return dc, mockConverter
			},
			args: struct {
				ctx       context.Context
				namespace string
				gvk       schema.GroupVersionKind
				selector  metav1.LabelSelector
			}{
				ctx:       t.Context(),
				namespace: "test-namespace",
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "Resource",
				},
				selector: metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
			},
			want: struct {
				resources []*un.Unstructured
				err       error
			}{
				resources: []*un.Unstructured{},
			},
		},
		"MatchingResources": {
			reason: "Should return resources matching label selector",
			setup: func() (dynamic.Interface, TypeConverter) {
				// Use resource builders for cleaner test objects
				objects := []runtime.Object{
					// Resource that matches our selector
					tu.NewResource("example.org/v1", "Resource", "matched-resource-1").
						InNamespace("test-namespace").
						WithLabels(map[string]string{
							"app": "test",
							"env": "dev",
						}).
						Build(),

					// Resource that matches our selector with different labels
					tu.NewResource("example.org/v1", "Resource", "matched-resource-2").
						InNamespace("test-namespace").
						WithLabels(map[string]string{
							"app": "test",
							"env": "prod",
						}).
						Build(),

					// Resource that doesn't match our selector
					tu.NewResource("example.org/v1", "Resource", "unmatched-resource").
						InNamespace("test-namespace").
						WithLabels(map[string]string{
							"app": "other",
						}).
						Build(),
				}

				dc := fake.NewSimpleDynamicClient(scheme, objects...)

				// Create mock type converter using the builder
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(_ context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{
							Group:    gvk.Group,
							Version:  gvk.Version,
							Resource: "resources",
						}, nil
					}).Build()

				return dc, mockConverter
			},
			args: struct {
				ctx       context.Context
				namespace string
				gvk       schema.GroupVersionKind
				selector  metav1.LabelSelector
			}{
				ctx:       t.Context(),
				namespace: "test-namespace",
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "Resource",
				},
				selector: metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
			},
			want: struct {
				resources []*un.Unstructured
				err       error
			}{
				resources: []*un.Unstructured{
					// Expected matching resources using builders
					tu.NewResource("example.org/v1", "Resource", "matched-resource-1").
						InNamespace("test-namespace").
						WithLabels(map[string]string{
							"app": "test",
							"env": "dev",
						}).
						Build(),
					tu.NewResource("example.org/v1", "Resource", "matched-resource-2").
						InNamespace("test-namespace").
						WithLabels(map[string]string{
							"app": "test",
							"env": "prod",
						}).
						Build(),
				},
			},
		},
		"ListError": {
			reason: "Should propagate errors from the Kubernetes API",
			setup: func() (dynamic.Interface, TypeConverter) {
				dc := fake.NewSimpleDynamicClientWithCustomListKinds(scheme,
					map[schema.GroupVersionResource]string{
						{Group: "example.org", Version: "v1", Resource: "resources"}: "ResourceList",
					})

				dc.PrependReactor("list", "resources", func(kt.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("list error")
				})

				// Create mock type converter using the builder
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(_ context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{
							Group:    gvk.Group,
							Version:  gvk.Version,
							Resource: "resources",
						}, nil
					}).Build()

				return dc, mockConverter
			},
			args: struct {
				ctx       context.Context
				namespace string
				gvk       schema.GroupVersionKind
				selector  metav1.LabelSelector
			}{
				ctx:       t.Context(),
				namespace: "test-namespace",
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "Resource",
				},
				selector: metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
			},
			want: struct {
				resources []*un.Unstructured
				err       error
			}{
				err: errors.New("cannot list resources for 'test-namespace/example.org/v1, Kind=Resource' matching"),
			},
		},
		"ConverterError": {
			reason: "Should propagate errors from the type converter",
			setup: func() (dynamic.Interface, TypeConverter) {
				dc := fake.NewSimpleDynamicClient(scheme)

				// Create mock type converter that returns an error
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(context.Context, schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{}, errors.New("conversion error")
					}).Build()

				return dc, mockConverter
			},
			args: struct {
				ctx       context.Context
				namespace string
				gvk       schema.GroupVersionKind
				selector  metav1.LabelSelector
			}{
				ctx:       t.Context(),
				namespace: "test-namespace",
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "Resource",
				},
				selector: metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
			},
			want: struct {
				resources []*un.Unstructured
				err       error
			}{
				err: errors.New("cannot list resources for 'example.org/v1, Kind=Resource' matching labels"),
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dynamicClient, converter := tc.setup()

			c := &DefaultResourceClient{
				dynamicClient: dynamicClient,
				converter:     converter,
				logger:        tu.TestLogger(t, false),
			}

			got, err := c.GetResourcesByLabel(tc.args.ctx, tc.args.gvk, tc.args.namespace, tc.args.selector)

			if tc.want.err != nil {
				if err == nil {
					t.Errorf("\n%s\nGetResourcesByLabel(...): expected error but got none", tc.reason)
					return
				}

				// Check that the error contains the expected message
				if !strings.Contains(err.Error(), tc.want.err.Error()) {
					t.Errorf("\n%s\nGetResourcesByLabel(...): expected error containing %q, got: %v",
						tc.reason, tc.want.err.Error(), err)
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nGetResourcesByLabel(...): unexpected error: %v", tc.reason, err)
				return
			}

			if diff := cmp.Diff(len(tc.want.resources), len(got)); diff != "" {
				t.Errorf("\n%s\nGetResourcesByLabel(...): -want resource count, +got resource count:\n%s", tc.reason, diff)
			}

			// Compare resources by name to handle ordering differences
			wantResources := make(map[string]bool)
			for _, res := range tc.want.resources {
				wantResources[res.GetName()] = true
			}

			for _, gotRes := range got {
				if !wantResources[gotRes.GetName()] {
					t.Errorf("\n%s\nGetResourcesByLabel(...): unexpected resource: %s", tc.reason, gotRes.GetName())
				}
			}

			// Also check if any expected resources are missing
			gotResources := make(map[string]bool)
			for _, res := range got {
				gotResources[res.GetName()] = true
			}

			for _, wantRes := range tc.want.resources {
				if !gotResources[wantRes.GetName()] {
					t.Errorf("\n%s\nGetResourcesByLabel(...): missing expected resource: %s", tc.reason, wantRes.GetName())
				}
			}
		})
	}
}

func TestResourceClient_ListResources(t *testing.T) {
	scheme := runtime.NewScheme()

	type args struct {
		ctx       context.Context
		gvk       schema.GroupVersionKind
		namespace string
	}

	type want struct {
		resources []*un.Unstructured
		err       error
	}

	tests := map[string]struct {
		reason string
		setup  func() (dynamic.Interface, TypeConverter)
		args   args
		want   want
	}{
		"NoResources": {
			reason: "Should return empty list when no resources exist",
			setup: func() (dynamic.Interface, TypeConverter) {
				dc := fake.NewSimpleDynamicClientWithCustomListKinds(scheme,
					map[schema.GroupVersionResource]string{
						{Group: "example.org", Version: "v1", Resource: "resources"}: "ResourceList",
					})

				// Create mock type converter
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(_ context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{
							Group:    gvk.Group,
							Version:  gvk.Version,
							Resource: "resources",
						}, nil
					}).Build()

				return dc, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "Resource",
				},
				namespace: "",
			},
			want: want{
				resources: []*un.Unstructured{},
			},
		},
		"ResourcesExist": {
			reason: "Should return all resources when they exist",
			setup: func() (dynamic.Interface, TypeConverter) {
				objects := []runtime.Object{
					tu.NewResource("example.org/v1", "Resource", "res1").
						InNamespace("test-namespace").
						WithSpecField("field1", "value1").
						Build(),
					tu.NewResource("example.org/v1", "Resource", "res2").
						InNamespace("test-namespace").
						WithSpecField("field2", "value2").
						Build(),
				}

				dc := fake.NewSimpleDynamicClient(scheme, objects...)

				// Create mock type converter
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(_ context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{
							Group:    gvk.Group,
							Version:  gvk.Version,
							Resource: "resources",
						}, nil
					}).Build()

				return dc, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "Resource",
				},
				namespace: "test-namespace",
			},
			want: want{
				resources: []*un.Unstructured{
					tu.NewResource("example.org/v1", "Resource", "res1").
						InNamespace("test-namespace").
						WithSpecField("field1", "value1").
						Build(),
					tu.NewResource("example.org/v1", "Resource", "res2").
						InNamespace("test-namespace").
						WithSpecField("field2", "value2").
						Build(),
				},
			},
		},
		"ListError": {
			reason: "Should propagate errors from API server",
			setup: func() (dynamic.Interface, TypeConverter) {
				dc := fake.NewSimpleDynamicClientWithCustomListKinds(scheme,
					map[schema.GroupVersionResource]string{
						{Group: "example.org", Version: "v1", Resource: "resources"}: "ResourceList",
					})

				dc.PrependReactor("list", "resources", func(kt.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("list error")
				})

				// Create mock type converter
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(_ context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{
							Group:    gvk.Group,
							Version:  gvk.Version,
							Resource: "resources",
						}, nil
					}).Build()

				return dc, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "Resource",
				},
				namespace: "test-namespace",
			},
			want: want{
				err: errors.New("cannot list resources for 'example.org/v1, Kind=Resource'"),
			},
		},
		"ConverterError": {
			reason: "Should propagate errors from type converter",
			setup: func() (dynamic.Interface, TypeConverter) {
				dc := fake.NewSimpleDynamicClient(scheme)

				// Create mock type converter that returns an error
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(context.Context, schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{}, errors.New("conversion error")
					}).Build()

				return dc, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "Resource",
				},
				namespace: "test-namespace",
			},
			want: want{
				err: errors.New("cannot list resources for 'example.org/v1, Kind=Resource'"),
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dynamicClient, converter := tc.setup()

			c := &DefaultResourceClient{
				dynamicClient: dynamicClient,
				converter:     converter,
				logger:        tu.TestLogger(t, false),
			}

			got, err := c.ListResources(tc.args.ctx, tc.args.gvk, tc.args.namespace)

			if tc.want.err != nil {
				if err == nil {
					t.Errorf("\n%s\nListResources(...): expected error but got none", tc.reason)
					return
				}

				if !strings.Contains(err.Error(), tc.want.err.Error()) {
					t.Errorf("\n%s\nListResources(...): expected error containing %q, got %q",
						tc.reason, tc.want.err.Error(), err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nListResources(...): unexpected error: %v", tc.reason, err)
				return
			}

			if diff := cmp.Diff(len(tc.want.resources), len(got)); diff != "" {
				t.Errorf("\n%s\nListResources(...): -want resource count, +got resource count:\n%s", tc.reason, diff)
			}

			// Create maps of resource names for easier comparison
			wantResources := make(map[string]bool)
			gotResources := make(map[string]bool)

			for _, res := range tc.want.resources {
				wantResources[res.GetName()] = true
			}

			for _, res := range got {
				gotResources[res.GetName()] = true
			}

			// Check for missing resources
			for name := range wantResources {
				if !gotResources[name] {
					t.Errorf("\n%s\nListResources(...): missing expected resource: %s", tc.reason, name)
				}
			}

			// Check for unexpected resources
			for name := range gotResources {
				if !wantResources[name] {
					t.Errorf("\n%s\nListResources(...): unexpected resource: %s", tc.reason, name)
				}
			}
		})
	}
}

// createStaleDiscoveryClient creates a fake discovery client that simulates stale discovery
// for external.metrics.k8s.io but works correctly for Crossplane API groups.
// This reproduces the exact scenario from issue #153.
func createStaleDiscoveryClient() discovery.DiscoveryInterface {
	// Create resources for Crossplane API groups
	resources := map[string][]metav1.APIResource{
		"apiextensions.crossplane.io/v1": {
			{Name: "compositeresourcedefinitions", Kind: "CompositeResourceDefinition"},
			{Name: "compositions", Kind: "Composition"},
		},
		// Include the stale group in the resources list so ServerGroups sees it,
		// but we'll make ServerResourcesForGroupVersion fail for it
		"external.metrics.k8s.io/v1beta1": {
			{Name: "externalmetrics", Kind: "ExternalMetric"},
		},
	}

	// Use the standard CreateFakeDiscoveryClient which sets up groups from resources
	fakeDiscovery := tu.CreateFakeDiscoveryClient(resources)

	return fakeDiscovery
}

// TestResourceClient_GetGVKsForGroupKind_StaleDiscoveryResilience validates that the fix
// for issue #153 works correctly - we can query Crossplane API groups even when unrelated
// API groups (like external.metrics.k8s.io) exist in the cluster.
//
// The key behavior validated here:
// - The new implementation queries only the specific API group needed (apiextensions.crossplane.io)
// - It does NOT query unrelated groups (external.metrics.k8s.io) which could have stale discovery
// - This is achieved by using ServerGroups() + ServerResourcesForGroupVersion() instead of ServerPreferredResources()
func TestResourceClient_GetGVKsForGroupKind_StaleDiscoveryResilience(t *testing.T) {
	// Create a fake discovery client with multiple API groups including external.metrics.k8s.io
	// In a real cluster, if external.metrics.k8s.io had stale discovery, ServerPreferredResources()
	// would fail. Our fix avoids calling ServerPreferredResources() entirely.
	discoveryClient := createStaleDiscoveryClient()

	c := &DefaultResourceClient{
		discoveryClient: discoveryClient,
		logger:          tu.TestLogger(t, false),
	}

	// Test that we can query Crossplane API groups successfully
	// The old implementation would call ServerPreferredResources() which queries ALL groups
	// The new implementation only queries apiextensions.crossplane.io
	got, err := c.GetGVKsForGroupKind(t.Context(), "apiextensions.crossplane.io", "CompositeResourceDefinition")
	if err != nil {
		t.Errorf("GetGVKsForGroupKind() failed unexpectedly: %v\n"+
			"This error indicates the fix for issue #153 may not be working correctly.", err)
		return
	}

	// Verify we got the expected GVK
	if len(got) != 1 {
		t.Errorf("GetGVKsForGroupKind() returned %d GVKs, want 1", len(got))
		return
	}

	expectedGVK := schema.GroupVersionKind{
		Group:   "apiextensions.crossplane.io",
		Version: "v1",
		Kind:    "CompositeResourceDefinition",
	}
	if got[0] != expectedGVK {
		t.Errorf("GetGVKsForGroupKind() = %v, want %v", got[0], expectedGVK)
	}

	// Also verify we can query Composition kind in the same group
	got2, err := c.GetGVKsForGroupKind(t.Context(), "apiextensions.crossplane.io", "Composition")
	if err != nil {
		t.Errorf("GetGVKsForGroupKind(Composition) failed unexpectedly: %v", err)
		return
	}

	if len(got2) != 1 || got2[0].Kind != "Composition" {
		t.Errorf("GetGVKsForGroupKind(Composition) = %v, want single Composition GVK", got2)
	}
}

func TestResourceClient_GetGVKsForGroupKind(t *testing.T) {
	tests := map[string]struct {
		reason    string
		resources map[string][]metav1.APIResource
		group     string
		kind      string
		want      []schema.GroupVersionKind
		wantErr   bool
		errMsg    string
	}{
		"SingleVersion": {
			reason: "Should return single GVK when kind exists in one version",
			resources: map[string][]metav1.APIResource{
				"apiextensions.crossplane.io/v1": {
					{
						Name: "compositeresourcedefinitions",
						Kind: "CompositeResourceDefinition",
					},
				},
			},
			group: "apiextensions.crossplane.io",
			kind:  "CompositeResourceDefinition",
			want: []schema.GroupVersionKind{
				{
					Group:   "apiextensions.crossplane.io",
					Version: "v1",
					Kind:    "CompositeResourceDefinition",
				},
			},
		},
		"MultipleVersions": {
			reason: "Should return GVKs for all versions when kind exists in multiple versions",
			resources: map[string][]metav1.APIResource{
				"apiextensions.crossplane.io/v1": {
					{
						Name: "compositeresourcedefinitions",
						Kind: "CompositeResourceDefinition",
					},
				},
				"apiextensions.crossplane.io/v1beta1": {
					{
						Name: "compositeresourcedefinitions",
						Kind: "CompositeResourceDefinition",
					},
				},
			},
			group: "apiextensions.crossplane.io",
			kind:  "CompositeResourceDefinition",
			want: []schema.GroupVersionKind{
				{
					Group:   "apiextensions.crossplane.io",
					Version: "v1",
					Kind:    "CompositeResourceDefinition",
				},
				{
					Group:   "apiextensions.crossplane.io",
					Version: "v1beta1",
					Kind:    "CompositeResourceDefinition",
				},
			},
		},
		"KindNotFound": {
			reason: "Should return empty list when kind doesn't exist in group",
			resources: map[string][]metav1.APIResource{
				"apiextensions.crossplane.io/v1": {
					{
						Name: "compositeresourcedefinitions",
						Kind: "CompositeResourceDefinition",
					},
				},
			},
			group: "apiextensions.crossplane.io",
			kind:  "NonExistentKind",
			want:  nil,
		},
		"GroupNotFound": {
			reason: "Should return error when group doesn't exist",
			resources: map[string][]metav1.APIResource{
				"other.io/v1": {
					{
						Name: "resources",
						Kind: "Resource",
					},
				},
			},
			group:   "nonexistent.io",
			kind:    "SomeKind",
			wantErr: true,
			errMsg:  "API group \"nonexistent.io\" not found on server",
		},
		"IgnoresOtherGroups": {
			reason: "Should only return GVKs from the specified group, ignoring others",
			resources: map[string][]metav1.APIResource{
				"apiextensions.crossplane.io/v1": {
					{
						Name: "compositeresourcedefinitions",
						Kind: "CompositeResourceDefinition",
					},
				},
				"external.metrics.k8s.io/v1beta1": {
					{
						Name: "externalmetrics",
						Kind: "ExternalMetric",
					},
				},
			},
			group: "apiextensions.crossplane.io",
			kind:  "CompositeResourceDefinition",
			want: []schema.GroupVersionKind{
				{
					Group:   "apiextensions.crossplane.io",
					Version: "v1",
					Kind:    "CompositeResourceDefinition",
				},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			discoveryClient := tu.CreateFakeDiscoveryClient(tc.resources)

			c := &DefaultResourceClient{
				discoveryClient: discoveryClient,
				logger:          tu.TestLogger(t, false),
			}

			got, err := c.GetGVKsForGroupKind(t.Context(), tc.group, tc.kind)

			if tc.wantErr {
				if err == nil {
					t.Errorf("\n%s\nGetGVKsForGroupKind(...): expected error but got none", tc.reason)
					return
				}

				if tc.errMsg != "" && !strings.Contains(err.Error(), tc.errMsg) {
					t.Errorf("\n%s\nGetGVKsForGroupKind(...): expected error containing %q, got %q",
						tc.reason, tc.errMsg, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nGetGVKsForGroupKind(...): unexpected error: %v", tc.reason, err)
				return
			}

			// Compare by converting both to maps for order-independent comparison
			wantMap := make(map[string]bool)
			for _, gvk := range tc.want {
				wantMap[gvk.String()] = true
			}

			gotMap := make(map[string]bool)
			for _, gvk := range got {
				gotMap[gvk.String()] = true
			}

			if len(wantMap) != len(gotMap) {
				t.Errorf("\n%s\nGetGVKsForGroupKind(...): got %d GVKs, want %d GVKs\ngot: %v\nwant: %v",
					tc.reason, len(got), len(tc.want), got, tc.want)
				return
			}

			for gvkStr := range wantMap {
				if !gotMap[gvkStr] {
					t.Errorf("\n%s\nGetGVKsForGroupKind(...): missing expected GVK: %s", tc.reason, gvkStr)
				}
			}
		})
	}
}

func TestResourceClient_IsNamespacedResource(t *testing.T) {
	tests := map[string]struct {
		reason    string
		resources map[string][]metav1.APIResource
		gvk       schema.GroupVersionKind
		want      bool
		wantErr   bool
		errMsg    string
	}{
		"NamespacedResource": {
			reason: "Should return true for namespaced resources",
			resources: map[string][]metav1.APIResource{
				"example.org/v1": {
					{
						Name:       "testresources",
						Kind:       "TestResource",
						Namespaced: true,
					},
				},
			},
			gvk: schema.GroupVersionKind{
				Group:   "example.org",
				Version: "v1",
				Kind:    "TestResource",
			},
			want:    true,
			wantErr: false,
		},
		"ClusterScopedResource": {
			reason: "Should return false for cluster-scoped resources",
			resources: map[string][]metav1.APIResource{
				"example.org/v1": {
					{
						Name:       "clusterresources",
						Kind:       "ClusterResource",
						Namespaced: false,
					},
				},
			},
			gvk: schema.GroupVersionKind{
				Group:   "example.org",
				Version: "v1",
				Kind:    "ClusterResource",
			},
			want:    false,
			wantErr: false,
		},
		"BuiltInNamespacedResource": {
			reason: "Should return true for built-in namespaced resources like ConfigMap",
			resources: map[string][]metav1.APIResource{
				"v1": {
					{
						Name:       "configmaps",
						Kind:       "ConfigMap",
						Namespaced: true,
					},
				},
			},
			gvk: schema.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "ConfigMap",
			},
			want:    true,
			wantErr: false,
		},
		"BuiltInClusterScopedResource": {
			reason: "Should return false for built-in cluster-scoped resources like Node",
			resources: map[string][]metav1.APIResource{
				"v1": {
					{
						Name:       "nodes",
						Kind:       "Node",
						Namespaced: false,
					},
				},
			},
			gvk: schema.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "Node",
			},
			want:    false,
			wantErr: false,
		},
		"ResourceNotFound": {
			reason: "Should return error when resource kind is not found in API resources",
			resources: map[string][]metav1.APIResource{
				"example.org/v1": {
					{
						Name:       "otherresources",
						Kind:       "OtherResource",
						Namespaced: true,
					},
				},
			},
			gvk: schema.GroupVersionKind{
				Group:   "example.org",
				Version: "v1",
				Kind:    "NonExistentResource",
			},
			want:    false,
			wantErr: true,
			errMsg:  "resource kind NonExistentResource not found in discovery API for group version example.org/v1",
		},
		"MultipleResourcesInGroup": {
			reason: "Should find the correct resource when multiple resources exist in same group",
			resources: map[string][]metav1.APIResource{
				"example.org/v1": {
					{
						Name:       "clusterresources",
						Kind:       "ClusterResource",
						Namespaced: false,
					},
					{
						Name:       "namespacedresources",
						Kind:       "NamespacedResource",
						Namespaced: true,
					},
					{
						Name:       "otherresources",
						Kind:       "OtherResource",
						Namespaced: true,
					},
				},
			},
			gvk: schema.GroupVersionKind{
				Group:   "example.org",
				Version: "v1",
				Kind:    "NamespacedResource",
			},
			want:    true,
			wantErr: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			discoveryClient := tu.CreateFakeDiscoveryClient(tc.resources)
			converter := tu.NewMockTypeConverter().Build()

			c := &DefaultResourceClient{
				dynamicClient:   nil, // Not needed for this test
				converter:       converter,
				discoveryClient: discoveryClient,
				logger:          tu.TestLogger(t, false),
			}

			got, err := c.IsNamespacedResource(t.Context(), tc.gvk)

			if tc.wantErr {
				if err == nil {
					t.Errorf("\n%s\nIsNamespacedResource(...): expected error but got none", tc.reason)
					return
				}

				if tc.errMsg != "" && !strings.Contains(err.Error(), tc.errMsg) {
					t.Errorf("\n%s\nIsNamespacedResource(...): expected error containing %q, got %q",
						tc.reason, tc.errMsg, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nIsNamespacedResource(...): unexpected error: %v", tc.reason, err)
				return
			}

			if got != tc.want {
				t.Errorf("\n%s\nIsNamespacedResource(...): got %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}
