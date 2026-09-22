package kubernetes

import (
	"context"
	"strings"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
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

func TestApplyClient_DryRunCreate(t *testing.T) {
	scheme := runtime.NewScheme()

	// exampleGVR is what the mock converter resolves every GVK to.
	exampleGVR := schema.GroupVersionResource{Group: "example.org", Version: "v1", Resource: "exampleresources"}

	newConverter := func() TypeConverter {
		return tu.NewMockTypeConverter().
			WithGVKToGVR(func(_ context.Context, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
				return schema.GroupVersionResource{
					Group:    gvk.Group,
					Version:  gvk.Version,
					Resource: "exampleresources",
				}, nil
			}).Build()
	}

	type want struct {
		result *un.Unstructured
		// errContains is matched against the error string.
		errContains string
		// errPredicate, when set, MUST hold for the returned error. This is how
		// we prove the wrapping preserves apierrors classification.
		errPredicate func(error) bool
		// action, when set, asserts on the create action the fake client recorded.
		action func(*testing.T, kt.CreateActionImpl)
	}

	tests := map[string]struct {
		reason string
		obj    *un.Unstructured
		// reaction is registered as a "create" reactor on exampleresources.
		reaction func(kt.Action) (bool, runtime.Object, error)
		// converter, when nil, resolves every GVK to exampleresources.
		converter TypeConverter
		want      want
	}{
		"SendsDryRunAllAndDefaultFieldManager": {
			reason: "Should send dryRun=All and the default field manager on the create request",
			obj: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
				InNamespace("test-namespace").
				WithSpecField("property", "new-value").
				Build(),
			reaction: func(action kt.Action) (bool, runtime.Object, error) {
				// The reactor is only registered for create actions.
				return true, action.(kt.CreateActionImpl).Object.DeepCopyObject(), nil
			},
			want: want{
				result: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
					InNamespace("test-namespace").
					WithSpecField("property", "new-value").
					Build(),
				action: func(t *testing.T, action kt.CreateActionImpl) {
					t.Helper()

					if diff := cmp.Diff([]string{metav1.DryRunAll}, action.CreateOptions.DryRun); diff != "" {
						t.Errorf("CreateOptions.DryRun: -want, +got:\n%s", diff)
					}

					if action.CreateOptions.FieldManager != FieldOwnerDefault {
						t.Errorf("CreateOptions.FieldManager: want %q, got %q", FieldOwnerDefault, action.CreateOptions.FieldManager)
					}

					if action.Namespace != "test-namespace" {
						t.Errorf("action namespace: want %q, got %q", "test-namespace", action.Namespace)
					}
				},
			},
		},
		"GenerateNameOnlyResourceAccepted": {
			reason: "Should accept an addition that carries only generateName, and send it unnamed",
			obj: tu.NewResource("example.org/v1", "ExampleResource", "").
				InNamespace("test-namespace").
				WithGenerateName("test-resource-").
				WithSpecField("property", "new-value").
				Build(),
			reaction: func(action kt.Action) (bool, runtime.Object, error) {
				// The server would invent a name here; mirror that so we also
				// prove the response is returned verbatim.
				// The reactor is only registered for create actions.
				result := action.(kt.CreateActionImpl).Object.DeepCopyObject().(*un.Unstructured)
				result.SetName("test-resource-abc12")

				return true, result, nil
			},
			want: want{
				result: tu.NewResource("example.org/v1", "ExampleResource", "test-resource-abc12").
					InNamespace("test-namespace").
					WithGenerateName("test-resource-").
					WithSpecField("property", "new-value").
					Build(),
				action: func(t *testing.T, action kt.CreateActionImpl) {
					t.Helper()

					sent, ok := action.Object.(*un.Unstructured)
					if !ok {
						t.Fatalf("create action object: want *unstructured.Unstructured, got %T", action.Object)
					}

					if sent.GetName() != "" {
						t.Errorf("sent name: want empty, got %q", sent.GetName())
					}

					if sent.GetGenerateName() != "test-resource-" {
						t.Errorf("sent generateName: want %q, got %q", "test-resource-", sent.GetGenerateName())
					}

					// A named path would have surfaced here; create must not use one.
					if action.Name != "" {
						t.Errorf("create action name: want empty, got %q", action.Name)
					}
				},
			},
		},
		"ClusterScopedResourceCreated": {
			reason: "Should create a cluster-scoped resource without a namespace",
			obj: tu.NewResource("example.org/v1", "ExampleResource", "test-cluster-resource").
				WithSpecField("property", "new-value").
				Build(),
			reaction: func(action kt.Action) (bool, runtime.Object, error) {
				// The reactor is only registered for create actions.
				return true, action.(kt.CreateActionImpl).Object.DeepCopyObject(), nil
			},
			want: want{
				result: tu.NewResource("example.org/v1", "ExampleResource", "test-cluster-resource").
					WithSpecField("property", "new-value").
					Build(),
				action: func(t *testing.T, action kt.CreateActionImpl) {
					t.Helper()

					if action.Namespace != "" {
						t.Errorf("action namespace: want empty, got %q", action.Namespace)
					}
				},
			},
		},
		"ForbiddenErrorStaysClassifiable": {
			reason: "A Forbidden returned by the apiserver must still satisfy apierrors.IsForbidden after wrapping",
			obj: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
				InNamespace("test-namespace").
				Build(),
			reaction: func(kt.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(
					exampleGVR.GroupResource(),
					"test-resource",
					errors.New("exceeded quota: compute-resources"),
				)
			},
			want: want{
				errContains:  "failed to dry-run create resource ExampleResource/test-resource",
				errPredicate: apierrors.IsForbidden,
			},
		},
		"InvalidErrorStaysClassifiable": {
			reason: "An Invalid returned by the apiserver must still satisfy apierrors.IsInvalid after wrapping",
			obj: tu.NewResource("example.org/v1", "ExampleResource", "").
				InNamespace("test-namespace").
				WithGenerateName("test-resource-").
				Build(),
			reaction: func(kt.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewInvalid(
					schema.GroupKind{Group: "example.org", Kind: "ExampleResource"},
					"",
					field.ErrorList{field.Required(field.NewPath("spec", "property"), "must be set")},
				)
			},
			want: want{
				// The identifier must not degenerate to "ExampleResource/" for a
				// generateName-only object.
				errContains:  "failed to dry-run create resource ExampleResource (generateName test-resource-)",
				errPredicate: apierrors.IsInvalid,
			},
		},
		"ConverterErrorCarriesTheUnresolvableGVKSentinel": {
			// The sentinel is the whole point of this case, not decoration. The request never reached the
			// apiserver, yet a real discovery failure for an unserved group/version is apierrors-NotFound
			// — identical in shape to NamespaceLifecycle refusing a resource whose type is perfectly
			// fine. Callers classify on this sentinel precisely so they cannot confuse the two; if it
			// stops being attached, an unknown type gets reported as a missing namespace.
			reason: "A GVK that cannot be resolved is distinguishable from an admission-time rejection",
			obj: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
				InNamespace("test-namespace").
				Build(),
			converter: tu.NewMockTypeConverter().
				WithGVKToGVR(func(context.Context, schema.GroupVersionKind) (schema.GroupVersionResource, error) {
					// Shaped like the real thing: discovery 404 for an unserved group/version.
					return schema.GroupVersionResource{}, apierrors.NewNotFound(schema.GroupResource{Group: "example.org", Resource: "exampleresources"}, "")
				}).Build(),
			want: want{
				// The GVK is named so the message points at the type, not at the namespace.
				errContains:  "cannot perform dry-run create for ExampleResource/test-resource (example.org/v1, Kind=ExampleResource)",
				errPredicate: func(err error) bool { return errors.Is(err, ErrUnresolvableGVK) },
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dynamicClient := fake.NewSimpleDynamicClient(scheme)
			if tc.reaction != nil {
				dynamicClient.PrependReactor("create", exampleGVR.Resource, tc.reaction)
			}

			converter := tc.converter
			if converter == nil {
				converter = newConverter()
			}

			c := &DefaultApplyClient{
				dynamicClient: dynamicClient,
				typeConverter: converter,
				logger:        tu.TestLogger(t, false),
			}

			got, err := c.DryRunCreate(t.Context(), tc.obj)

			if tc.want.errContains != "" {
				if err == nil {
					t.Fatalf("\n%s\nDryRunCreate(...): expected error but got none", tc.reason)
				}

				if !strings.Contains(err.Error(), tc.want.errContains) {
					t.Errorf("\n%s\nDryRunCreate(...): expected error containing %q, got %q",
						tc.reason, tc.want.errContains, err.Error())
				}

				if tc.want.errPredicate != nil && !tc.want.errPredicate(err) {
					t.Errorf("\n%s\nDryRunCreate(...): wrapped error lost its apierrors classification: %v",
						tc.reason, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("\n%s\nDryRunCreate(...): unexpected error: %v", tc.reason, err)
			}

			if diff := cmp.Diff(tc.want.result, got); diff != "" {
				t.Errorf("\n%s\nDryRunCreate(...): -want, +got:\n%s", tc.reason, diff)
			}

			if tc.want.action != nil {
				createAction, ok := findCreateAction(dynamicClient.Actions())
				if !ok {
					t.Fatalf("\n%s\nDryRunCreate(...): no create action recorded", tc.reason)
				}

				tc.want.action(t, createAction)
			}
		})
	}
}

// findCreateAction returns the first recorded create action, if any.
func findCreateAction(actions []kt.Action) (kt.CreateActionImpl, bool) {
	for _, a := range actions {
		if create, ok := a.(kt.CreateActionImpl); ok && a.GetVerb() == "create" {
			return create, true
		}
	}

	return kt.CreateActionImpl{}, false
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
			obj: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
				WithFieldManagers("kubectl-client-side-apply", "other-controller").
				Build(),
			want: "",
		},
		"ManagedFieldsWithCrossplaneComposedPrefix": {
			reason: "Should return the Crossplane composed field owner when present",
			obj: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
				WithFieldManagers(
					"kubectl-client-side-apply",
					"apiextensions.crossplane.io/composed/abc123def456",
					"other-controller",
				).
				Build(),
			want: "apiextensions.crossplane.io/composed/abc123def456",
		},
		"MultipleCrossplanePrefixes": {
			reason: "Should return the first Crossplane composed field owner when multiple present",
			obj: tu.NewResource("example.org/v1", "ExampleResource", "test-resource").
				WithFieldManagers(
					"apiextensions.crossplane.io/composed/first-hash",
					"apiextensions.crossplane.io/composed/second-hash",
				).
				Build(),
			want: "apiextensions.crossplane.io/composed/first-hash",
		},
		"RealWorldCrossplaneFieldOwner": {
			reason: "Should correctly extract a real-world Crossplane field owner hash",
			// This simulates a real composed resource from Crossplane
			obj: tu.NewResource("nop.crossplane.io/v1alpha1", "ClusterNopResource", "test-xr-abc123").
				WithFieldManagers(
					"crossplane",
					"apiextensions.crossplane.io/composed/e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				).
				Build(),
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
