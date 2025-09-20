package kubernetes

import (
	"context"
	"strings"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	kt "k8s.io/client-go/testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

const (
	testXResourceKind   = "XResource"
	testXResourcePlural = "xresources"
	testExampleOrgGroup = "example.org"
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
					WithResourceNameForGVK(schema.GroupVersionKind{
						Group:   testExampleOrgGroup,
						Version: "v1",
						Kind:    testXResourceKind,
					}, testXResourcePlural).Build()
			},
			gvk: schema.GroupVersionKind{
				Group:   testExampleOrgGroup,
				Version: "v1",
				Kind:    testXResourceKind,
			},
			want: true, // Custom resource should require a CRD
		},
		"APIExtensionResource": {
			reason: "API Extensions resources like CRDs themselves should require special handling",
			setupConverter: func() TypeConverter {
				// For apiextensions resources, our converter should return a successful resource name
				return tu.NewMockTypeConverter().
					WithResourceNameForGVK(schema.GroupVersionKind{
						Group:   "apiextensions.k8s.io",
						Version: "v1",
						Kind:    "CustomResourceDefinition",
					}, "customresourcedefinitions").Build()
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
					WithResourceNameForGVK(schema.GroupVersionKind{
						Group:   "networking.k8s.io",
						Version: "v1",
						Kind:    "NetworkPolicy",
					}, "networkpolicies").Build()
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
				Group:   testExampleOrgGroup,
				Version: "v1",
				Kind:    testXResourceKind,
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
				xrdToCRDName:    make(map[string]string),
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
	testCRD := tu.NewCRD(testXResourcePlural+"."+testExampleOrgGroup, testExampleOrgGroup, testXResourceKind).
		WithPlural(testXResourcePlural).
		WithSingular("xresource").
		Build()

	// Create the same CRD as unstructured for the mock dynamic client using CRD builder
	testCRDUnstructuredObj, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(
		tu.NewCRD(testXResourcePlural+".example.org", testExampleOrgGroup, testXResourceKind).
			WithPlural(testXResourcePlural).
			WithSingular("xresource").
			WithNamespaceScope().
			Build())
	testCRDUnstructured := &un.Unstructured{Object: testCRDUnstructuredObj}

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
					if getAction.GetName() == testXResourcePlural+".example.org" {
						return true, testCRDUnstructured, nil
					}
					return false, nil, nil
				})

				// Create mock type converter that returns "xresources" for the given GVK
				mockConverter := tu.NewMockTypeConverter().
					WithResourceNameForGVK(schema.GroupVersionKind{
						Group:   testExampleOrgGroup,
						Version: "v1",
						Kind:    testXResourceKind,
					}, testXResourcePlural).Build()

				return dynamicClient, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   testExampleOrgGroup,
					Version: "v1",
					Kind:    testXResourceKind,
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
					WithResourceNameForGVK(schema.GroupVersionKind{
						Group:   testExampleOrgGroup,
						Version: "v1",
						Kind:    "NonexistentResource",
					}, "nonexistentresources").Build()

				return dynamicClient, mockConverter
			},
			args: args{
				ctx: t.Context(),
				gvk: schema.GroupVersionKind{
					Group:   testExampleOrgGroup,
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
					Group:   testExampleOrgGroup,
					Version: "v1",
					Kind:    testXResourceKind,
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
				xrdToCRDName:    make(map[string]string),
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
	scheme := runtime.NewScheme()

	// Create sample XRD with proper schema using builder
	xrd := tu.NewXRD(testXResourcePlural+".example.org", testExampleOrgGroup, testXResourceKind).
		WithPlural(testXResourcePlural).
		WithSingular("xresource").
		WithRawSchema([]byte(`{
			"type": "object",
			"properties": {
				"spec": {
					"type": "object",
					"properties": {
						"field": {
							"type": "string"
						}
					}
				},
				"status": {
					"type": "object"
				}
			}
		}`)).
		BuildAsUnstructured()

	// Create corresponding CRD that should exist in cluster using CRD builder
	correspondingCRDObj, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(
		tu.NewCRD(testXResourcePlural+".example.org", testExampleOrgGroup, testXResourceKind).
			WithPlural(testXResourcePlural).
			WithSingular("xresource").
			Build())
	correspondingCRD := &un.Unstructured{Object: correspondingCRDObj}

	tests := map[string]struct {
		reason       string
		setupClient  func() *DefaultSchemaClient
		xrds         []*un.Unstructured
		expectErr    bool
		errMsg       string
		expectedCRDs int
	}{
		"SuccessfulFetchFromCluster": {
			reason: "Should successfully fetch CRDs from cluster for given XRDs",
			setupClient: func() *DefaultSchemaClient {
				// Setup dynamic client to return the corresponding CRD
				dynamicClient := fake.NewSimpleDynamicClient(scheme)
				dynamicClient.PrependReactor("get", "customresourcedefinitions", func(action kt.Action) (bool, runtime.Object, error) {
					getAction := action.(kt.GetAction)
					if getAction.GetName() == testXResourcePlural+".example.org" {
						return true, correspondingCRD, nil
					}
					return false, nil, nil
				})

				mockConverter := tu.NewMockTypeConverter().
					WithResourceNameForGVK(schema.GroupVersionKind{
						Group:   testExampleOrgGroup,
						Version: "v1",
						Kind:    testXResourceKind,
					}, testXResourcePlural).Build()

				return &DefaultSchemaClient{
					dynamicClient:   dynamicClient,
					typeConverter:   mockConverter,
					logger:          tu.TestLogger(t, false),
					resourceTypeMap: make(map[schema.GroupVersionKind]bool),
					crds:            []*extv1.CustomResourceDefinition{},
					crdByName:       make(map[string]*extv1.CustomResourceDefinition),
					xrdToCRDName:    make(map[string]string),
				}
			},
			xrds:         []*un.Unstructured{xrd},
			expectErr:    false,
			expectedCRDs: 1,
		},
		"EmptyXRDs": {
			reason: "Should handle empty XRD list gracefully",
			setupClient: func() *DefaultSchemaClient {
				return &DefaultSchemaClient{
					logger:          tu.TestLogger(t, false),
					resourceTypeMap: make(map[schema.GroupVersionKind]bool),
					crds:            []*extv1.CustomResourceDefinition{},
					crdByName:       make(map[string]*extv1.CustomResourceDefinition),
					xrdToCRDName:    make(map[string]string),
				}
			},
			xrds:         []*un.Unstructured{},
			expectErr:    false,
			expectedCRDs: 0,
		},
		"NilXRDs": {
			reason: "Should handle nil XRD list gracefully",
			setupClient: func() *DefaultSchemaClient {
				return &DefaultSchemaClient{
					logger:          tu.TestLogger(t, false),
					resourceTypeMap: make(map[schema.GroupVersionKind]bool),
					crds:            []*extv1.CustomResourceDefinition{},
					crdByName:       make(map[string]*extv1.CustomResourceDefinition),
					xrdToCRDName:    make(map[string]string),
				}
			},
			xrds:         nil,
			expectErr:    false,
			expectedCRDs: 0,
		},
		"CRDNotFoundInCluster": {
			reason: "Should fail fast when required CRD is not found in cluster",
			setupClient: func() *DefaultSchemaClient {
				// Setup dynamic client to return error for CRD requests
				dynamicClient := fake.NewSimpleDynamicClient(scheme)
				dynamicClient.PrependReactor("get", "customresourcedefinitions", func(kt.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("CRD not found")
				})

				mockConverter := tu.NewMockTypeConverter().
					WithResourceNameForGVK(schema.GroupVersionKind{
						Group:   testExampleOrgGroup,
						Version: "v1",
						Kind:    testXResourceKind,
					}, testXResourcePlural).Build()

				return &DefaultSchemaClient{
					dynamicClient:   dynamicClient,
					typeConverter:   mockConverter,
					logger:          tu.TestLogger(t, false),
					resourceTypeMap: make(map[schema.GroupVersionKind]bool),
					crds:            []*extv1.CustomResourceDefinition{},
					crdByName:       make(map[string]*extv1.CustomResourceDefinition),
					xrdToCRDName:    make(map[string]string),
				}
			},
			xrds:         []*un.Unstructured{xrd},
			expectErr:    true,
			errMsg:       "cannot fetch required CRD",
			expectedCRDs: 0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			client := tc.setupClient()

			err := client.LoadCRDsFromXRDs(ctx, tc.xrds)

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

			// Verify expected number of CRDs were cached
			crds := client.GetAllCRDs()
			if len(crds) != tc.expectedCRDs {
				t.Errorf("\n%s\nLoadCRDsFromXRDs(): expected %d CRDs to be cached, got %d",
					tc.reason, tc.expectedCRDs, len(crds))
			}
		})
	}
}

func TestSchemaClient_GetCRDByName(t *testing.T) {
	// Create test CRDs
	testCRDName := testXResourcePlural + "." + testExampleOrgGroup
	testCRD := tu.NewCRD(testCRDName, testExampleOrgGroup, testXResourceKind).
		WithPlural(testXResourcePlural).
		WithSingular("xresource").
		Build()

	tests := map[string]struct {
		reason      string
		setupCRDs   []*extv1.CustomResourceDefinition
		searchName  string
		expectError bool
		expectedCRD *extv1.CustomResourceDefinition
	}{
		"CRDFound": {
			reason:      "Should return CRD when it exists in cache",
			setupCRDs:   []*extv1.CustomResourceDefinition{testCRD},
			searchName:  testCRDName,
			expectError: false,
			expectedCRD: testCRD,
		},
		"CRDNotFound": {
			reason:      "Should return error when CRD doesn't exist in cache",
			setupCRDs:   []*extv1.CustomResourceDefinition{},
			searchName:  testCRDName,
			expectError: true,
			expectedCRD: nil,
		},
		"DifferentCRDInCache": {
			reason: "Should return error when searching for CRD that doesn't exist",
			setupCRDs: []*extv1.CustomResourceDefinition{
				tu.NewCRD("other."+testExampleOrgGroup, testExampleOrgGroup, "OtherKind").Build(),
			},
			searchName:  testCRDName,
			expectError: true,
			expectedCRD: nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// Create schema client with test CRDs
			client := &DefaultSchemaClient{
				logger:          tu.TestLogger(t, false),
				crds:            make([]*extv1.CustomResourceDefinition, 0),
				crdByName:       make(map[string]*extv1.CustomResourceDefinition),
				resourceTypeMap: make(map[schema.GroupVersionKind]bool),
				xrdToCRDName:    make(map[string]string),
			}

			// Pre-populate cache with test CRDs
			for _, crd := range tc.setupCRDs {
				client.addCRD(crd)
			}

			// Call GetCRDByName
			crd, err := client.GetCRDByName(tc.searchName)

			// Check error expectations
			if tc.expectError {
				if err == nil {
					t.Errorf("\n%s\nGetCRDByName(): expected error but got none", tc.reason)
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nGetCRDByName(): unexpected error: %v", tc.reason, err)
				return
			}

			// Verify returned CRD
			if crd == nil {
				t.Errorf("\n%s\nGetCRDByName(): expected CRD but got nil", tc.reason)
				return
			}

			if crd.Name != tc.expectedCRD.Name {
				t.Errorf("\n%s\nGetCRDByName(): expected CRD name %s, got %s",
					tc.reason, tc.expectedCRD.Name, crd.Name)
			}

			if crd.Spec.Group != tc.expectedCRD.Spec.Group {
				t.Errorf("\n%s\nGetCRDByName(): expected CRD group %s, got %s",
					tc.reason, tc.expectedCRD.Spec.Group, crd.Spec.Group)
			}
		})
	}
}

func TestSchemaClient_GetAllCRDs(t *testing.T) {
	// Create test CRDs
	crd1 := tu.NewCRD("crd1."+testExampleOrgGroup, testExampleOrgGroup, "TestKind1").Build()
	crd2 := tu.NewCRD("crd2."+testExampleOrgGroup, testExampleOrgGroup, "TestKind2").Build()

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
				xrdToCRDName:    make(map[string]string),
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

func TestExtractGVKsFromXRD(t *testing.T) {
	tests := map[string]struct {
		reason       string
		xrd          *un.Unstructured
		expectedGVKs []schema.GroupVersionKind
		expectErr    bool
		errMsg       string
	}{
		"ValidV1XRD": {
			reason: "Should extract GVKs from a valid v1 XRD with multiple versions",
			xrd: tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", testXResourcePlural+".example.org").
				WithSpec(map[string]interface{}{
					"group": testExampleOrgGroup,
					"names": map[string]interface{}{
						"kind":     testXResourceKind,
						"plural":   testXResourcePlural,
						"singular": "xresource",
					},
					"versions": []interface{}{
						map[string]interface{}{
							"name":   "v1alpha1",
							"served": true,
						},
						map[string]interface{}{
							"name":          "v1",
							"served":        true,
							"referenceable": true,
						},
					},
				}).Build(),
			expectedGVKs: []schema.GroupVersionKind{
				{Group: testExampleOrgGroup, Version: "v1alpha1", Kind: testXResourceKind},
				{Group: testExampleOrgGroup, Version: "v1", Kind: testXResourceKind},
			},
			expectErr: false,
		},
		"ValidV2XRD": {
			reason: "Should extract GVKs from a valid v2 XRD with multiple versions",
			xrd: tu.NewResource("apiextensions.crossplane.io/v2", "CompositeResourceDefinition", testXResourcePlural+".example.org").
				WithSpec(map[string]interface{}{
					"group": testExampleOrgGroup,
					"names": map[string]interface{}{
						"kind":     testXResourceKind,
						"plural":   testXResourcePlural,
						"singular": "xresource",
					},
					"versions": []interface{}{
						map[string]interface{}{
							"name":   "v1beta1",
							"served": true,
						},
						map[string]interface{}{
							"name":          "v1",
							"served":        true,
							"referenceable": true,
						},
					},
				}).Build(),
			expectedGVKs: []schema.GroupVersionKind{
				{Group: testExampleOrgGroup, Version: "v1beta1", Kind: testXResourceKind},
				{Group: testExampleOrgGroup, Version: "v1", Kind: testXResourceKind},
			},
			expectErr: false,
		},

		"UnsupportedAPIVersion": {
			reason: "Should fail when XRD has unsupported apiVersion",
			xrd: tu.NewResource("apiextensions.crossplane.io/v3", "CompositeResourceDefinition", "invalid-xrd"). // Unsupported version
																WithSpec(map[string]interface{}{
					"group": testExampleOrgGroup,
					"names": map[string]interface{}{
						"kind": testXResourceKind,
					},
					"versions": []interface{}{
						map[string]interface{}{
							"name":   "v1",
							"served": true,
						},
					},
				}).Build(),
			expectErr: true,
			errMsg:    "unsupported XRD apiVersion",
		},
		"ConversionError": {
			reason: "Should fail when XRD cannot be converted to typed object",
			xrd: tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "invalid-xrd").
				WithSpec(map[string]interface{}{
					"group": testExampleOrgGroup,
					"names": "invalid-names-should-be-object", // Invalid structure
					"versions": []interface{}{
						map[string]interface{}{
							"name":   "v1",
							"served": true,
						},
					},
				}).Build(),
			expectErr: true,
			errMsg:    "cannot convert XRD",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			gvks, err := extractGVKsFromXRD(tc.xrd)

			if tc.expectErr {
				if err == nil {
					t.Errorf("\n%s\nextractGVKsFromXRD(): expected error but got none", tc.reason)
					return
				}

				if tc.errMsg != "" && !strings.Contains(err.Error(), tc.errMsg) {
					t.Errorf("\n%s\nextractGVKsFromXRD(): expected error containing %q, got %q",
						tc.reason, tc.errMsg, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nextractGVKsFromXRD(): unexpected error: %v", tc.reason, err)

				return
			}

			if diff := cmp.Diff(tc.expectedGVKs, gvks); diff != "" {
				t.Errorf("\n%s\nextractGVKsFromXRD(): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestSchemaClient_CachingBehavior(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()

	// Create test CRD as unstructured for the mock dynamic client using CRD builder
	testCRDUnstructuredObj, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(
		tu.NewCRD(testXResourcePlural+".example.org", testExampleOrgGroup, testXResourceKind).
			WithPlural(testXResourcePlural).
			WithSingular("xresource").
			Build())
	testCRDUnstructured := &un.Unstructured{Object: testCRDUnstructuredObj}

	tests := map[string]struct {
		reason          string
		setupClient     func() (*DefaultSchemaClient, *int)
		gvk             schema.GroupVersionKind
		expectedCalls   int
		expectCRDCached bool
		expectError     bool
	}{
		"FirstCallFetchesFromCluster": {
			reason: "First GetCRD call should fetch from dynamic client",
			setupClient: func() (*DefaultSchemaClient, *int) {
				callCount := 0
				dynamicClient := fake.NewSimpleDynamicClient(scheme)
				dynamicClient.PrependReactor("get", "customresourcedefinitions", func(action kt.Action) (bool, runtime.Object, error) {
					callCount++
					getAction := action.(kt.GetAction)
					if getAction.GetName() == testXResourcePlural+".example.org" {
						return true, testCRDUnstructured, nil
					}
					return false, nil, nil
				})

				mockConverter := tu.NewMockTypeConverter().
					WithResourceNameForGVK(schema.GroupVersionKind{
						Group:   testExampleOrgGroup,
						Version: "v1",
						Kind:    testXResourceKind,
					}, testXResourcePlural).Build()

				client := &DefaultSchemaClient{
					dynamicClient:   dynamicClient,
					typeConverter:   mockConverter,
					logger:          tu.TestLogger(t, false),
					resourceTypeMap: make(map[schema.GroupVersionKind]bool),
					crds:            []*extv1.CustomResourceDefinition{},
					crdByName:       make(map[string]*extv1.CustomResourceDefinition),
					xrdToCRDName:    make(map[string]string),
				}

				return client, &callCount
			},
			gvk: schema.GroupVersionKind{
				Group:   testExampleOrgGroup,
				Version: "v1",
				Kind:    testXResourceKind,
			},
			expectedCalls:   1,
			expectCRDCached: true,
			expectError:     false,
		},
		"SecondCallUsesCache": {
			reason: "Second GetCRD call should use cache without calling dynamic client",
			setupClient: func() (*DefaultSchemaClient, *int) {
				callCount := 0
				dynamicClient := fake.NewSimpleDynamicClient(scheme)
				dynamicClient.PrependReactor("get", "customresourcedefinitions", func(action kt.Action) (bool, runtime.Object, error) {
					callCount++
					getAction := action.(kt.GetAction)
					if getAction.GetName() == testXResourcePlural+".example.org" {
						return true, testCRDUnstructured, nil
					}
					return false, nil, nil
				})

				mockConverter := tu.NewMockTypeConverter().
					WithResourceNameForGVK(schema.GroupVersionKind{
						Group:   testExampleOrgGroup,
						Version: "v1",
						Kind:    testXResourceKind,
					}, testXResourcePlural).Build()

				client := &DefaultSchemaClient{
					dynamicClient:   dynamicClient,
					typeConverter:   mockConverter,
					logger:          tu.TestLogger(t, false),
					resourceTypeMap: make(map[schema.GroupVersionKind]bool),
					crds:            []*extv1.CustomResourceDefinition{},
					crdByName:       make(map[string]*extv1.CustomResourceDefinition),
					xrdToCRDName:    make(map[string]string),
				}

				// Pre-populate cache by making first call
				gvk := schema.GroupVersionKind{Group: testExampleOrgGroup, Version: "v1", Kind: testXResourceKind}
				_, _ = client.GetCRD(ctx, gvk)
				callCount = 0 // Reset counter after pre-population

				return client, &callCount
			},
			gvk: schema.GroupVersionKind{
				Group:   testExampleOrgGroup,
				Version: "v1",
				Kind:    testXResourceKind,
			},
			expectedCalls:   0, // Should use cache, no additional calls
			expectCRDCached: true,
			expectError:     false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			client, callCountPtr := tc.setupClient()

			// Call GetCRD
			crd, err := client.GetCRD(ctx, tc.gvk)

			// Check error expectations
			if tc.expectError {
				if err == nil {
					t.Errorf("\n%s\nGetCRD(): expected error but got none", tc.reason)
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nGetCRD(): unexpected error: %v", tc.reason, err)
				return
			}

			// Verify call count
			if *callCountPtr != tc.expectedCalls {
				t.Errorf("\n%s\nExpected %d calls to dynamic client, got %d",
					tc.reason, tc.expectedCalls, *callCountPtr)
			}

			// Verify CRD was returned
			if crd == nil {
				t.Errorf("\n%s\nGetCRD(): expected CRD but got nil", tc.reason)
				return
			}

			// Verify caching behavior
			if tc.expectCRDCached {
				allCRDs := client.GetAllCRDs()
				if len(allCRDs) == 0 {
					t.Errorf("\n%s\nExpected CRD to be cached in GetAllCRDs(), got empty slice", tc.reason)
				}

				// Verify consistency between calls
				crd2, err2 := client.GetCRD(ctx, tc.gvk)
				if err2 != nil {
					t.Errorf("\n%s\nSecond GetCRD() call failed: %v", tc.reason, err2)
					return
				}

				if diff := cmp.Diff(crd, crd2); diff != "" {
					t.Errorf("\n%s\nCached CRD differs from original: -want, +got:\n%s", tc.reason, diff)
				}
			}
		})
	}
}
