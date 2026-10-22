package kubernetes

import (
	"context"
	"strings"
	"testing"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
				missingCRDs:     make(map[string]error),
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

func TestGetCRDByNameOnMiss(t *testing.T) {
	crdName := testXResourcePlural + "." + testExampleOrgGroup
	crd := tu.NewCRD(crdName, testExampleOrgGroup, testXResourceKind).
		WithPlural(testXResourcePlural).
		WithSingular("xresource").
		WithDefaultVersion().
		Build()
	crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

	asUnstructured := func(t *testing.T) *un.Unstructured {
		t.Helper()

		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(crd)
		if err != nil {
			t.Fatalf("cannot convert CRD: %v", err)
		}

		u := &un.Unstructured{Object: obj}
		u.SetGroupVersionKind(extv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))

		return u
	}

	// A result of one GetCRDByName call: the name of the CRD it returned, or the kind of error.
	type result struct {
		Name     string
		NotFound bool
		Failed   bool
	}

	found := result{Name: crdName}
	notFound := result{NotFound: true, Failed: true}

	tests := map[string]struct {
		reason string
		// inCluster says whether the cluster holds the CRD before the first call.
		inCluster bool
		// failFirstGet makes the first GET fail with an error other than NotFound.
		failFirstGet bool
		// between changes the cluster after the first call.
		between func(t *testing.T, c *fake.FakeDynamicClient)
		want    []result
	}{
		"FetchesFromClusterOnMiss": {
			reason:    "A CRD not yet cached is fetched from the cluster",
			inCluster: true,
			want:      []result{found},
		},
		"MissingCRDIsNotFound": {
			reason: "A CRD the cluster does not hold is reported NotFound",
			want:   []result{notFound},
		},
		"FoundCRDIsCached": {
			reason:    "A CRD once found is returned from the cache, even after the cluster drops it",
			inCluster: true,
			between: func(t *testing.T, c *fake.FakeDynamicClient) {
				t.Helper()

				if err := c.Resource(crdGVR).Delete(t.Context(), crdName, metav1.DeleteOptions{}); err != nil {
					t.Fatalf("cannot delete CRD: %v", err)
				}
			},
			want: []result{found, found},
		},
		"MissingCRDIsCached": {
			reason: "A CRD once found missing stays missing for the run, even after the cluster gains it",
			between: func(t *testing.T, c *fake.FakeDynamicClient) {
				t.Helper()

				if _, err := c.Resource(crdGVR).Create(t.Context(), asUnstructured(t), metav1.CreateOptions{}); err != nil {
					t.Fatalf("cannot create CRD: %v", err)
				}
			},
			want: []result{notFound, notFound},
		},
		"OtherErrorIsNotCached": {
			reason:       "A failure other than NotFound says nothing about the CRD, so the next call fetches again",
			inCluster:    true,
			failFirstGet: true,
			want:         []result{{Failed: true}, found},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var objs []runtime.Object
			if tc.inCluster {
				objs = append(objs, asUnstructured(t))
			}

			dc := fake.NewSimpleDynamicClient(runtime.NewScheme(), objs...)
			if tc.failFirstGet {
				failed := false
				dc.PrependReactor("get", "customresourcedefinitions", func(kt.Action) (bool, runtime.Object, error) {
					if failed {
						return false, nil, nil
					}

					failed = true

					return true, nil, errors.New("connection refused")
				})
			}

			c := NewSchemaClient(&core.Clients{Dynamic: dc}, tu.NewMockTypeConverter().Build(), tu.TestLogger(t, false))

			got := make([]result, 0, len(tc.want))

			for i := range tc.want {
				if i == 1 && tc.between != nil {
					tc.between(t, dc)
				}

				crd, err := c.GetCRDByName(t.Context(), crdName)

				r := result{Failed: err != nil, NotFound: apierrors.IsNotFound(err)}
				if crd != nil {
					r.Name = crd.GetName()
				}

				got = append(got, r)
			}

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("\n%s\nGetCRDByName(...) results: -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestSchemaClient_GetAllCRDs(t *testing.T) {
	// Create test CRDs
	crd1 := tu.NewCRD("crd1."+testExampleOrgGroup, testExampleOrgGroup, "TestKind1").WithDefaultVersion().Build()
	crd2 := tu.NewCRD("crd2."+testExampleOrgGroup, testExampleOrgGroup, "TestKind2").WithDefaultVersion().Build()

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
				missingCRDs:     make(map[string]error),
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
					missingCRDs:     make(map[string]error),
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
					missingCRDs:     make(map[string]error),
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
