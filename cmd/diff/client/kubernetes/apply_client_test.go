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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	kt "k8s.io/client-go/testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

var _ ApplyClient = (*tu.MockApplyClient)(nil)

func TestApplyClient_DryRunApply(t *testing.T) {
	scheme := runtime.NewScheme()

	type args struct {
		ctx context.Context
		obj *un.Unstructured
	}

	type want struct {
		result *un.Unstructured
		err    error
	}

	tests := map[string]struct {
		reason string
		setup  func() (dynamic.Interface, TypeConverter)
		args   args
		want   want
	}{
		"NamespacedResourceApplied": {
			reason: "Should successfully apply a namespaced resource",
			setup: func() (dynamic.Interface, TypeConverter) {
				obj := tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
					InNamespace("test-namespace").
					WithSpecField("property", "new-value").
					Build()

				// Create dynamic client that returns the object with a resource version
				dynamicClient := fake.NewSimpleDynamicClient(scheme)
				// Add reactor to handle apply operation
				dynamicClient.PrependReactor("patch", "exampleresources", func(kt.Action) (bool, runtime.Object, error) {
					// For apply, we'd return the "server-modified" version
					result := obj.DeepCopy()
					result.SetResourceVersion("1000") // Server would set this

					return true, result, nil
				})

				// Create type converter
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
				obj: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
					InNamespace("test-namespace").
					WithSpecField("property", "new-value").
					Build(),
			},
			want: want{
				result: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
					InNamespace("test-namespace").
					WithSpecField("property", "new-value").
					Build(),
			},
		},
		"ClusterScopedResourceApplied": {
			reason: "Should successfully apply a cluster-scoped resource",
			setup: func() (dynamic.Interface, TypeConverter) {
				obj := tu.NewResource("example.org/v1", "ClusterResource", "test-cluster-resource").
					WithSpecField("property", "new-value").
					Build()

				// Create dynamic client that returns the object with a resource version
				dynamicClient := fake.NewSimpleDynamicClient(scheme)
				// Add reactor to handle apply operation
				dynamicClient.PrependReactor("patch", "clusterresources", func(kt.Action) (bool, runtime.Object, error) {
					// For apply, we'd return the "server-modified" version
					result := obj.DeepCopy()
					result.SetResourceVersion("1000") // Server would set this

					return true, result, nil
				})

				// Create type converter
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
				obj: tu.NewResource("example.org/v1", "ClusterResource", "test-cluster-resource").
					WithSpecField("property", "new-value").
					Build(),
			},
			want: want{
				result: tu.NewResource("example.org/v1", "ClusterResource", "test-cluster-resource").
					WithSpecField("property", "new-value").
					Build(),
			},
		},
		"ConverterError": {
			reason: "Should return error when GVK to GVR conversion fails",
			setup: func() (dynamic.Interface, TypeConverter) {
				dynamicClient := fake.NewSimpleDynamicClient(scheme)

				// Create type converter that returns an error
				mockConverter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(context.Context, schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{}, errors.New("conversion error")
					}).Build()

				return dynamicClient, mockConverter
			},
			args: args{
				ctx: t.Context(),
				obj: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
					InNamespace("test-namespace").
					WithSpecField("property", "new-value").
					Build(),
			},
			want: want{
				err: errors.New("cannot perform dry-run apply for ExampleResource/test-resource"),
			},
		},
		"ApplyError": {
			reason: "Should return error when apply fails",
			setup: func() (dynamic.Interface, TypeConverter) {
				dynamicClient := fake.NewSimpleDynamicClient(scheme)
				// Add reactor to make apply fail
				dynamicClient.PrependReactor("patch", "exampleresources", func(kt.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("apply failed")
				})

				// Create type converter
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
				obj: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
					InNamespace("test-namespace").
					WithSpecField("property", "new-value").
					Build(),
			},
			want: want{
				err: errors.New("failed to apply resource"),
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dynamicClient, converter := tc.setup()

			c := &DefaultApplyClient{
				dynamicClient: dynamicClient,
				typeConverter: converter,
				logger:        tu.TestLogger(t, false),
			}

			got, err := c.DryRunApply(tc.args.ctx, tc.args.obj, "")

			if tc.want.err != nil {
				if err == nil {
					t.Errorf("\n%s\nDryRunApply(...): expected error but got none", tc.reason)
					return
				}

				if !strings.Contains(err.Error(), tc.want.err.Error()) {
					t.Errorf("\n%s\nDryRunApply(...): expected error containing %q, got %q",
						tc.reason, tc.want.err.Error(), err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nDryRunApply(...): unexpected error: %v", tc.reason, err)
				return
			}

			// For successful cases, compare the original parts of results
			// We remove the resourceVersion before comparing since we set it in our test
			gotCopy := got.DeepCopy()
			if _, exists, _ := un.NestedString(gotCopy.Object, "metadata", "resourceVersion"); exists {
				un.RemoveNestedField(gotCopy.Object, "metadata", "resourceVersion")
			}

			wantCopy := tc.want.result.DeepCopy()
			if _, exists, _ := un.NestedString(wantCopy.Object, "metadata", "resourceVersion"); exists {
				un.RemoveNestedField(wantCopy.Object, "metadata", "resourceVersion")
			}

			if diff := cmp.Diff(wantCopy, gotCopy); diff != "" {
				t.Errorf("\n%s\nDryRunApply(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestGetComposedFieldOwner(t *testing.T) {
	tests := map[string]struct {
		reason string
		obj    *un.Unstructured
		want   string
	}{
		"NilObject": {
			reason: "Should return empty string for nil object",
			obj:    nil,
			want:   "",
		},
		"NoManagedFields": {
			reason: "Should return empty string when object has no managed fields",
			obj: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
				Build(),
			want: "",
		},
		"ManagedFieldsWithoutCrossplanePrefix": {
			reason: "Should return empty string when managed fields don't contain Crossplane composed prefix",
			obj: func() *un.Unstructured {
				obj := tu.NewResource("example.org/v1", "ExampleResource", "test-resource").Build()
				obj.SetManagedFields([]metav1.ManagedFieldsEntry{
					{Manager: "kubectl-client-side-apply"},
					{Manager: "other-controller"},
				})
				return obj
			}(),
			want: "",
		},
		"ManagedFieldsWithCrossplaneComposedPrefix": {
			reason: "Should return the Crossplane composed field owner when present",
			obj: func() *un.Unstructured {
				obj := tu.NewResource("example.org/v1", "ExampleResource", "test-resource").Build()
				obj.SetManagedFields([]metav1.ManagedFieldsEntry{
					{Manager: "kubectl-client-side-apply"},
					{Manager: "apiextensions.crossplane.io/composed/abc123def456"},
					{Manager: "other-controller"},
				})
				return obj
			}(),
			want: "apiextensions.crossplane.io/composed/abc123def456",
		},
		"MultipleCrossplanePrefixes": {
			reason: "Should return the first Crossplane composed field owner when multiple present",
			obj: func() *un.Unstructured {
				obj := tu.NewResource("example.org/v1", "ExampleResource", "test-resource").Build()
				obj.SetManagedFields([]metav1.ManagedFieldsEntry{
					{Manager: "apiextensions.crossplane.io/composed/first-hash"},
					{Manager: "apiextensions.crossplane.io/composed/second-hash"},
				})
				return obj
			}(),
			want: "apiextensions.crossplane.io/composed/first-hash",
		},
		"RealWorldCrossplaneFieldOwner": {
			reason: "Should correctly extract a real-world Crossplane field owner hash",
			obj: func() *un.Unstructured {
				// This simulates a real composed resource from Crossplane
				obj := tu.NewResource("nop.crossplane.io/v1alpha1", "ClusterNopResource", "test-xr-abc123").Build()
				obj.SetManagedFields([]metav1.ManagedFieldsEntry{
					{Manager: "crossplane", Operation: "Apply"},
					{Manager: "apiextensions.crossplane.io/composed/e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
				})
				return obj
			}(),
			want: "apiextensions.crossplane.io/composed/e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := GetComposedFieldOwner(tc.obj)

			if got != tc.want {
				t.Errorf("\n%s\nGetComposedFieldOwner(...): want %q, got %q", tc.reason, tc.want, got)
			}
		})
	}
}
