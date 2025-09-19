package kubernetes

import (
	"context"
	"strings"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	kt "k8s.io/client-go/testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

var _ SchemaClient = (*tu.MockSchemaClient)(nil)

func TestSchemaClient_IsCRDRequired(t *testing.T) {
	// Set up context for tests
	ctx := t.Context()

	tests := map[string]struct {
		reason         string
		setupConverter func() TypeConverter
		gvk            schema.GroupVersionKind
		want           bool
	}{
		"CoreResource": {
			reason: "Core API resources (group='') should not require a CRD",
			setupConverter: func() TypeConverter {
				// Just need a mock converter as it shouldn't be called for core resources
				return tu.NewMockTypeConverter().Build()
			},
			gvk: schema.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "Pod",
			},
			want: false, // Core API resource should not require a CRD
		},
		"KubernetesExtensionResource": {
			reason: "Kubernetes extension resources (like apps/v1) should not require a CRD",
			setupConverter: func() TypeConverter {
				// Just need a mock converter as it shouldn't be called for k8s resources
				return tu.NewMockTypeConverter().Build()
			},
			gvk: schema.GroupVersionKind{
				Group:   "apps",
				Version: "v1",
				Kind:    "Deployment",
			},
			want: false, // Kubernetes extension should not require a CRD
		},
		"CustomResource": {
			reason: "Custom resources (non-standard domain) should require a CRD",
			setupConverter: func() TypeConverter {
				// For custom resources, our converter should return a successful resource name
				return tu.NewMockTypeConverter().
					WithGetResourceNameForGVK(func(_ context.Context, gvk schema.GroupVersionKind) (string, error) {
						if gvk.Group == "example.org" && gvk.Version == "v1" && gvk.Kind == "XResource" {
							return "xresources", nil
						}
						return "", errors.New("unexpected GVK in test")
					}).Build()
			},
			gvk: schema.GroupVersionKind{
				Group:   "example.org",
				Version: "v1",
				Kind:    "XResource",
			},
			want: true, // Custom resource should require a CRD
		},
		"APIExtensionResource": {
			reason: "API Extensions resources like CRDs themselves should require special handling",
			setupConverter: func() TypeConverter {
				// For apiextensions resources, our converter should return a successful resource name
				return tu.NewMockTypeConverter().
					WithGetResourceNameForGVK(func(_ context.Context, gvk schema.GroupVersionKind) (string, error) {
						if gvk.Group == "apiextensions.k8s.io" && gvk.Version == "v1" && gvk.Kind == "CustomResourceDefinition" {
							return "customresourcedefinitions", nil
						}
						return "", errors.New("unexpected GVK in test")
					}).Build()
			},
			gvk: schema.GroupVersionKind{
				Group:   "apiextensions.k8s.io",
				Version: "v1",
				Kind:    "CustomResourceDefinition",
			},
			want: true, // APIExtensions resources are handled specially and require CRDs
		},
		"OtherK8sIOButNotAPIExtensions": {
			reason: "Other k8s.io resources that are not from apiextensions should not require a CRD",
			setupConverter: func() TypeConverter {
				// For networking.k8s.io resources, our converter should return a successful resource name
				return tu.NewMockTypeConverter().
					WithGetResourceNameForGVK(func(_ context.Context, gvk schema.GroupVersionKind) (string, error) {
						if gvk.Group == "networking.k8s.io" && gvk.Version == "v1" && gvk.Kind == "NetworkPolicy" {
							return "networkpolicies", nil
						}
						return "", errors.New("unexpected GVK in test")
					}).Build()
			},
			gvk: schema.GroupVersionKind{
				Group:   "networking.k8s.io",
				Version: "v1",
				Kind:    "NetworkPolicy",
			},
			want: false, // Other k8s.io resources should not require a CRD
		},
		"ConverterError": {
			reason: "If type conversion fails, should default to requiring a CRD",
			setupConverter: func() TypeConverter {
				// Create mock type converter that returns an error
				return tu.NewMockTypeConverter().
					WithGetResourceNameForGVK(func(context.Context, schema.GroupVersionKind) (string, error) {
						return "", errors.New("conversion error")
					}).Build()
			},
			gvk: schema.GroupVersionKind{
				Group:   "example.org",
				Version: "v1",
				Kind:    "XResource",
			},
			want: true, // Default to requiring CRD on conversion failure
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// Create a schema client with the test converter
			c := &DefaultSchemaClient{
				typeConverter:   tc.setupConverter(),
				logger:          tu.TestLogger(t, false),
				resourceTypeMap: make(map[schema.GroupVersionKind]bool),
			}

			// Call the method under test
			got := c.IsCRDRequired(ctx, tc.gvk)

			// Verify result
			if got != tc.want {
				t.Errorf("\n%s\nIsCRDRequired() = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

func TestSchemaClient_GetCRD(t *testing.T) {
	scheme := runtime.NewScheme()

	type args struct {
		ctx context.Context
		gvk schema.GroupVersionKind
	}

	type want struct {
		crd *extv1.CustomResourceDefinition
		err error
	}

	// Create a test CRD as typed object
	testCRD := &extv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CustomResourceDefinition",
			APIVersion: "apiextensions.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "xresources.example.org",
		},
		Spec: extv1.CustomResourceDefinitionSpec{
			Group: "example.org",
			Names: extv1.CustomResourceDefinitionNames{
				Kind:     "XResource",
				Plural:   "xresources",
				Singular: "xresource",
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

	// Create the same CRD as unstructured for the mock dynamic client
	testCRDUnstructured := &un.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apiextensions.k8s.io/v1",
			"kind":       "CustomResourceDefinition",
			"metadata": map[string]interface{}{
				"name": "xresources.example.org",
			},
			"spec": map[string]interface{}{
				"group": "example.org",
				"names": map[string]interface{}{
					"kind":     "XResource",
					"plural":   "xresources",
					"singular": "xresource",
				},
				"scope": "Namespaced",
				"versions": []interface{}{
					map[string]interface{}{
						"name":    "v1",
						"served":  true,
						"storage": true,
					},
				},
			},
		},
	}

	tests := map[string]struct {
		reason string
		setup  func() (dynamic.Interface, TypeConverter)
		args   args
		want   want
	}{
		"SuccessfulCRDRetrieval": {
			reason: "Should retrieve CRD when it exists",
			setup: func() (dynamic.Interface, TypeConverter) {
				// Set up the dynamic client to return our test CRD
				dynamicClient := fake.NewSimpleDynamicClient(scheme)
				dynamicClient.PrependReactor("get", "customresourcedefinitions", func(action kt.Action) (bool, runtime.Object, error) {
					getAction := action.(kt.GetAction)
					if getAction.GetName() == "xresources.example.org" {
						return true, testCRDUnstructured, nil
					}
					return false, nil, nil
				})

				// Create mock type converter that returns "xresources" for the given GVK
				mockConverter := tu.NewMockTypeConverter().
					WithGetResourceNameForGVK(func(_ context.Context, gvk schema.GroupVersionKind) (string, error) {
						if gvk.Group == "example.org" && gvk.Version == "v1" && gvk.Kind == "XResource" {
							return "xresources", nil
						}
						return "", errors.New("unexpected GVK in test")
					}).Build()

				return dynamicClient, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "XResource",
				},
			},
			want: want{
				crd: testCRD,
				err: nil,
			},
		},
		"CRDNotFound": {
			reason: "Should return error when CRD doesn't exist",
			setup: func() (dynamic.Interface, TypeConverter) {
				dynamicClient := fake.NewSimpleDynamicClient(scheme)
				dynamicClient.PrependReactor("get", "customresourcedefinitions", func(kt.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("CRD not found")
				})

				// Create mock type converter that returns "nonexistentresources"
				mockConverter := tu.NewMockTypeConverter().
					WithGetResourceNameForGVK(func(_ context.Context, gvk schema.GroupVersionKind) (string, error) {
						if gvk.Group == "example.org" && gvk.Version == "v1" && gvk.Kind == "NonexistentResource" {
							return "nonexistentresources", nil
						}
						return "", errors.New("unexpected GVK in test")
					}).Build()

				return dynamicClient, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "NonexistentResource",
				},
			},
			want: want{
				crd: nil,
				err: errors.New("cannot get CRD"),
			},
		},
		"TypeConverterError": {
			reason: "Should return error when type conversion fails",
			setup: func() (dynamic.Interface, TypeConverter) {
				dynamicClient := fake.NewSimpleDynamicClient(scheme)

				// Create mock type converter that returns an error
				mockConverter := tu.NewMockTypeConverter().
					WithGetResourceNameForGVK(func(context.Context, schema.GroupVersionKind) (string, error) {
						return "", errors.New("conversion error")
					}).Build()

				return dynamicClient, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "XResource",
				},
			},
			want: want{
				crd: nil,
				err: errors.New("cannot determine CRD name for"),
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dynamicClient, converter := tc.setup()

			c := &DefaultSchemaClient{
				dynamicClient:   dynamicClient,
				typeConverter:   converter,
				logger:          tu.TestLogger(t, false),
				resourceTypeMap: make(map[schema.GroupVersionKind]bool),
				crds:            []*extv1.CustomResourceDefinition{},
				crdByName:       make(map[string]*extv1.CustomResourceDefinition),
			}

			crd, err := c.GetCRD(tc.args.ctx, tc.args.gvk)

			if tc.want.err != nil {
				if err == nil {
					t.Errorf("\n%s\nGetCRD(...): expected error but got none", tc.reason)
					return
				}

				if !strings.Contains(err.Error(), tc.want.err.Error()) {
					t.Errorf("\n%s\nGetCRD(...): expected error containing %q, got %q",
						tc.reason, tc.want.err.Error(), err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nGetCRD(...): unexpected error: %v", tc.reason, err)
				return
			}

			if diff := cmp.Diff(tc.want.crd, crd); diff != "" {
				t.Errorf("\n%s\nGetCRD(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestSchemaClient_LoadCRDsFromXRDs(t *testing.T) {
	ctx := t.Context()

	// Create sample XRD with proper schema
	xrd := &un.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apiextensions.crossplane.io/v1",
			"kind":       "CompositeResourceDefinition",
			"metadata": map[string]interface{}{
				"name": "xresources.example.org",
			},
			"spec": map[string]interface{}{
				"group": "example.org",
				"names": map[string]interface{}{
					"kind":     "XResource",
					"plural":   "xresources",
					"singular": "xresource",
				},
				"versions": []interface{}{
					map[string]interface{}{
						"name":   "v1",
						"served": true,
						"schema": map[string]interface{}{
							"openAPIV3Schema": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"spec": map[string]interface{}{
										"type": "object",
										"properties": map[string]interface{}{
											"field": map[string]interface{}{
												"type": "string",
											},
										},
									},
									"status": map[string]interface{}{
										"type": "object",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	tests := map[string]struct {
		reason    string
		xrds      []*un.Unstructured
		expectErr bool
		errMsg    string
	}{
		"SuccessfulConversion": {
			reason:    "Should successfully convert XRDs to CRDs and cache them",
			xrds:      []*un.Unstructured{xrd},
			expectErr: false,
		},
		"EmptyXRDs": {
			reason:    "Should handle empty XRD list gracefully",
			xrds:      []*un.Unstructured{},
			expectErr: false,
		},
		"NilXRDs": {
			reason:    "Should handle nil XRD list gracefully",
			xrds:      nil,
			expectErr: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultSchemaClient{
				logger:          tu.TestLogger(t, false),
				resourceTypeMap: make(map[schema.GroupVersionKind]bool),
				crds:            []*extv1.CustomResourceDefinition{},
				crdByName:       make(map[string]*extv1.CustomResourceDefinition),
			}

			err := c.LoadCRDsFromXRDs(ctx, tc.xrds)

			if tc.expectErr {
				if err == nil {
					t.Errorf("\n%s\nLoadCRDsFromXRDs(): expected error but got none", tc.reason)
					return
				}

				if tc.errMsg != "" && !strings.Contains(err.Error(), tc.errMsg) {
					t.Errorf("\n%s\nLoadCRDsFromXRDs(): expected error containing %q, got %q",
						tc.reason, tc.errMsg, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nLoadCRDsFromXRDs(): unexpected error: %v", tc.reason, err)
				return
			}

			// Verify CRDs were loaded (for non-empty case)
			if len(tc.xrds) > 0 {
				crds := c.GetAllCRDs()
				if len(crds) == 0 {
					t.Errorf("\n%s\nLoadCRDsFromXRDs(): expected CRDs to be loaded but got none", tc.reason)
				}
			}
		})
	}
}

func TestSchemaClient_GetAllCRDs(t *testing.T) {
	// Create test CRDs
	crd1 := &extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "crd1.example.org"},
		Spec:       extv1.CustomResourceDefinitionSpec{Group: "example.org"},
	}
	crd2 := &extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "crd2.example.org"},
		Spec:       extv1.CustomResourceDefinitionSpec{Group: "example.org"},
	}

	tests := map[string]struct {
		reason    string
		setupCRDs []*extv1.CustomResourceDefinition
		expected  int
	}{
		"NoCRDs": {
			reason:    "Should return empty slice when no CRDs are cached",
			setupCRDs: []*extv1.CustomResourceDefinition{},
			expected:  0,
		},
		"MultipleCRDs": {
			reason:    "Should return all cached CRDs",
			setupCRDs: []*extv1.CustomResourceDefinition{crd1, crd2},
			expected:  2,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultSchemaClient{
				logger:          tu.TestLogger(t, false),
				resourceTypeMap: make(map[schema.GroupVersionKind]bool),
				crds:            tc.setupCRDs,
				crdByName:       make(map[string]*extv1.CustomResourceDefinition),
			}

			// Add CRDs to name lookup map
			for _, crd := range tc.setupCRDs {
				c.crdByName[crd.Name] = crd
			}

			crds := c.GetAllCRDs()

			if len(crds) != tc.expected {
				t.Errorf("\n%s\nGetAllCRDs(): expected %d CRDs, got %d", tc.reason, tc.expected, len(crds))
				return
			}

			// Verify it returns a copy (modifying result shouldn't affect internal state)
			if len(crds) > 0 {
				originalLen := len(c.crds)

				crds[0] = nil // Modify the returned slice
				if len(c.crds) != originalLen || c.crds[0] == nil {
					t.Errorf("\n%s\nGetAllCRDs(): should return a copy, not reference to internal slice", tc.reason)
				}
			}
		})
	}
}

func TestSchemaClient_CachingBehavior(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()

	// Create test CRD as unstructured for the mock dynamic client

	testCRDUnstructured := &un.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apiextensions.k8s.io/v1",
			"kind":       "CustomResourceDefinition",
			"metadata": map[string]interface{}{
				"name": "xresources.example.org",
			},
			"spec": map[string]interface{}{
				"group": "example.org",
				"names": map[string]interface{}{
					"kind":     "XResource",
					"plural":   "xresources",
					"singular": "xresource",
				},
			},
		},
	}

	// Track number of calls to the dynamic client
	callCount := 0
	dynamicClient := fake.NewSimpleDynamicClient(scheme)
	dynamicClient.PrependReactor("get", "customresourcedefinitions", func(action kt.Action) (bool, runtime.Object, error) {
		callCount++

		getAction := action.(kt.GetAction)
		if getAction.GetName() == "xresources.example.org" {
			return true, testCRDUnstructured, nil
		}

		return false, nil, nil
	})

	mockConverter := tu.NewMockTypeConverter().
		WithGetResourceNameForGVK(func(_ context.Context, gvk schema.GroupVersionKind) (string, error) {
			if gvk.Group == "example.org" && gvk.Version == "v1" && gvk.Kind == "XResource" {
				return "xresources", nil
			}
			return "", errors.New("unexpected GVK in test")
		}).Build()

	c := &DefaultSchemaClient{
		dynamicClient:   dynamicClient,
		typeConverter:   mockConverter,
		logger:          tu.TestLogger(t, false),
		resourceTypeMap: make(map[schema.GroupVersionKind]bool),
		crds:            []*extv1.CustomResourceDefinition{},
		crdByName:       make(map[string]*extv1.CustomResourceDefinition),
	}

	gvk := schema.GroupVersionKind{
		Group:   "example.org",
		Version: "v1",
		Kind:    "XResource",
	}

	// First call should fetch from dynamic client
	crd1, err := c.GetCRD(ctx, gvk)
	if err != nil {
		t.Fatalf("First GetCRD call failed: %v", err)
	}

	if callCount != 1 {
		t.Errorf("Expected 1 call to dynamic client, got %d", callCount)
	}

	// Second call should use cache
	crd2, err := c.GetCRD(ctx, gvk)
	if err != nil {
		t.Fatalf("Second GetCRD call failed: %v", err)
	}

	if callCount != 1 {
		t.Errorf("Expected cache to be used (1 call total), got %d calls", callCount)
	}

	// Both calls should return equivalent CRDs
	if diff := cmp.Diff(crd1, crd2); diff != "" {
		t.Errorf("Cached CRD differs from original: -want, +got:\n%s", diff)
	}

	// Verify CRD is in GetAllCRDs result
	allCRDs := c.GetAllCRDs()
	if len(allCRDs) != 1 {
		t.Errorf("Expected 1 CRD in GetAllCRDs(), got %d", len(allCRDs))
	}
}
