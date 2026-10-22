package crossplane

import (
	"context"
	"strings"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	ucomposite "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"
)

var _ DefinitionClient = (*tu.MockDefinitionClient)(nil)

var (
	XRDv1GVK = schema.GroupVersionKind{Group: CrossplaneAPIExtGroup, Version: "v1", Kind: CompositeResourceDefinitionKind}
	XRDv2GVK = schema.GroupVersionKind{Group: CrossplaneAPIExtGroup, Version: "v2", Kind: CompositeResourceDefinitionKind}
)

func TestDefaultDefinitionClient_listXRDs(t *testing.T) {
	ctx := t.Context()

	// Create test XRDs
	xrd1 := tu.NewResource("apiextensions.crossplane.io/v1", CompositeResourceDefinitionKind, "xrd1").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
			"kind":     "XR1",
			"plural":   "xr1s",
			"singular": "xr1",
		}).
		Build()

	xrd2 := tu.NewResource("apiextensions.crossplane.io/v1", CompositeResourceDefinitionKind, "xrd2").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
			"kind":     "XR2",
			"plural":   "xr2s",
			"singular": "xr2",
		}).
		Build()

	type fields struct {
		xrds       []*un.Unstructured
		xrdsLoaded bool
		gvks       []schema.GroupVersionKind
	}

	tests := map[string]struct {
		reason       string
		mockResource tu.MockResourceClient
		fields       fields
		want         []*un.Unstructured
		wantErr      bool
		errSubstring string
	}{
		"NoXRDsFound": {
			reason: "Should return empty slice when no XRDs exist",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithListResources(func(_ context.Context, gvk schema.GroupVersionKind, _ string) ([]*un.Unstructured, error) {
					if gvk.Group == CrossplaneAPIExtGroup && gvk.Kind == CompositeResourceDefinitionKind {
						return []*un.Unstructured{}, nil
					}

					return nil, errors.New("unexpected GVK")
				}).
				Build(),
			fields: fields{
				xrds:       nil,
				xrdsLoaded: false,
			},
			want:    []*un.Unstructured{},
			wantErr: false,
		},
		// TODO:  test for v1 and v2 xrds
		"XRDsExist": {
			reason: "Should return all XRDs when they exist",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithListResources(func(_ context.Context, gvk schema.GroupVersionKind, _ string) ([]*un.Unstructured, error) {
					if gvk.Group == CrossplaneAPIExtGroup && gvk.Kind == CompositeResourceDefinitionKind && gvk.Version == "v1" {
						return []*un.Unstructured{xrd1, xrd2}, nil
					}

					return nil, errors.New("unexpected GVK")
				}).
				Build(),
			fields: fields{
				xrds:       nil,
				xrdsLoaded: false,
				gvks:       []schema.GroupVersionKind{XRDv1GVK},
			},
			want:    []*un.Unstructured{xrd1, xrd2},
			wantErr: false,
		},
		"ListError": {
			reason: "Should propagate errors from the Kubernetes API",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithListResourcesFailure("list error").
				Build(),
			fields: fields{
				xrds:       nil,
				xrdsLoaded: false,
				gvks:       []schema.GroupVersionKind{XRDv1GVK},
			},
			want:         nil,
			wantErr:      true,
			errSubstring: "cannot list XRDs",
		},
		"UsesCache": {
			reason: "Should use cached XRDs when already loaded",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				// This should never be called since cache is used
				WithListResources(func(context.Context, schema.GroupVersionKind, string) ([]*un.Unstructured, error) {
					t.Errorf("ListResources should not be called when cache is available")
					return nil, errors.New("should not be called")
				}).
				Build(),
			fields: fields{
				xrds:       []*un.Unstructured{xrd1, xrd2},
				xrdsLoaded: true,
			},
			want:    []*un.Unstructured{xrd1, xrd2},
			wantErr: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultDefinitionClient{
				resourceClient: &tt.mockResource,
				logger:         tu.TestLogger(t, false),
				xrds:           tt.fields.xrds,
				xrdsLoaded:     tt.fields.xrdsLoaded,
				gvks:           tt.fields.gvks,
			}

			got, err := c.listXRDs(ctx)

			if tt.wantErr {
				if err == nil {
					t.Errorf("\n%s\nlistXRDs(): expected error but got none", tt.reason)
					return
				}

				if tt.errSubstring != "" && !strings.Contains(err.Error(), tt.errSubstring) {
					t.Errorf("\n%s\nlistXRDs(): expected error containing %q, got %q", tt.reason, tt.errSubstring, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nlistXRDs(): unexpected error: %v", tt.reason, err)
				return
			}

			// Verify cache state
			if !c.xrdsLoaded {
				t.Errorf("\n%s\nlistXRDs(): cache not marked as loaded after call", tt.reason)
			}

			// Compare XRD count
			if diff := cmp.Diff(len(tt.want), len(got)); diff != "" {
				t.Errorf("\n%s\nlistXRDs(): -want count, +got count:\n%s", tt.reason, diff)
			}

			// Check if all expected XRDs are present by name
			wantNames := make(map[string]bool)
			gotNames := make(map[string]bool)

			for _, xrd := range tt.want {
				wantNames[xrd.GetName()] = true
			}

			for _, xrd := range got {
				gotNames[xrd.GetName()] = true
			}

			// Check missing XRDs
			for name := range wantNames {
				if !gotNames[name] {
					t.Errorf("\n%s\nlistXRDs(): missing expected XRD with name %s", tt.reason, name)
				}
			}

			// Check unexpected XRDs
			for name := range gotNames {
				if !wantNames[name] {
					t.Errorf("\n%s\nlistXRDs(): unexpected XRD with name %s", tt.reason, name)
				}
			}
		})
	}
}

func TestDefaultDefinitionClient_GetXRDForClaim(t *testing.T) {

	// Create test XRDs
	xrdWithClaimKind := tu.NewResource("apiextensions.crossplane.io/v1", CompositeResourceDefinitionKind, "xexampleresources.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
			"kind":     "XExampleResource",
			"plural":   "xexampleresources",
			"singular": "xexampleresource",
		}).
		WithSpecField("claimNames", map[string]any{
			"kind":     "ExampleClaim",
			"plural":   "exampleclaims",
			"singular": "exampleclaim",
		}).
		WithSpecField("versions", []any{map[string]any{"name": "v1"}}).
		Build()

	xrdWithoutClaimKind := tu.NewResource("apiextensions.crossplane.io/v1", CompositeResourceDefinitionKind, "xotherresources.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
			"kind":     "XOtherResource",
			"plural":   "xotherresources",
			"singular": "xotherresource",
		}).
		// No claimNames field
		Build()

	type args struct {
		gvk schema.GroupVersionKind
	}

	tests := map[string]struct {
		reason       string
		cluster      xrdCluster
		args         args
		want         *un.Unstructured
		wantErr      bool
		errSubstring string
	}{
		"MatchingClaimFound": {
			reason:  "Should return the XRD that defines the claim kind",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdWithClaimKind, xrdWithoutClaimKind}, xrdGVKs: []schema.GroupVersionKind{XRDv1GVK}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "ExampleClaim",
				},
			},
			want:    xrdWithClaimKind,
			wantErr: false,
		},
		"NoMatchingClaim": {
			reason:  "Should return error when no XRD defines the claim kind",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdWithoutClaimKind}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "ExampleClaim",
				},
			},
			want:         nil,
			wantErr:      true,
			errSubstring: "no XRD found that defines claim type",
		},
		"GetError": {
			reason:  "Should propagate an error getting the definition from the cluster",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdWithClaimKind}, getErr: errors.New("get error")},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "ExampleClaim",
				},
			},
			want:         nil,
			wantErr:      true,
			errSubstring: "get error",
		},
		"DifferentGroup": {
			reason:  "Should not match XRD with different group",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdWithClaimKind}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "different.org", // Different group
					Version: "v1",
					Kind:    "ExampleClaim", // Same kind
				},
			},
			want:         nil,
			wantErr:      true,
			errSubstring: "no XRD found that defines claim type",
		},
		"DifferentKind": {
			reason:  "Should not match XRD with different claim kind",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdWithClaimKind}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org", // Same group
					Version: "v1",
					Kind:    "DifferentClaim", // Different kind
				},
			},
			want:         nil,
			wantErr:      true,
			errSubstring: "no XRD found that defines claim type",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			c := tt.cluster.client(ctx, t)

			got, err := c.GetXRDForClaim(ctx, tt.args.gvk)

			if tt.wantErr {
				if err == nil {
					t.Errorf("\n%s\nGetXRDForClaim(): expected error but got none", tt.reason)
					return
				}

				if tt.errSubstring != "" && !strings.Contains(err.Error(), tt.errSubstring) {
					t.Errorf("\n%s\nGetXRDForClaim(): expected error containing %q, got %q", tt.reason, tt.errSubstring, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nGetXRDForClaim(): unexpected error: %v", tt.reason, err)
				return
			}

			if diff := cmp.Diff(tt.want.GetName(), got.GetName()); diff != "" {
				t.Errorf("\n%s\nGetXRDForClaim(): -want name, +got name:\n%s", tt.reason, diff)
			}

			// Verify it's the right XRD by checking the claim kind
			claimNames, found, _ := un.NestedMap(got.Object, "spec", "claimNames")
			if !found {
				t.Errorf("\n%s\nGetXRDForClaim(): returned XRD missing spec.claimNames", tt.reason)
				return
			}

			claimKind, found, _ := un.NestedString(claimNames, "kind")
			if !found || claimKind != tt.args.gvk.Kind {
				t.Errorf("\n%s\nGetXRDForClaim(): returned XRD has wrong claim kind, want %s, got %s",
					tt.reason, tt.args.gvk.Kind, claimKind)
			}
		})
	}
}

func TestDefaultDefinitionClient_GetXRDForXR(t *testing.T) {

	// Create test XRDs
	xrdForXR1 := tu.NewResource("apiextensions.crossplane.io/v1", CompositeResourceDefinitionKind, "xr1s.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
			"kind":     "XR1",
			"plural":   "xr1s",
			"singular": "xr1",
		}).
		WithSpecField("versions", []any{
			map[string]any{
				"name":    "v1",
				"served":  true,
				"storage": true,
			},
			map[string]any{
				"name":    "v2",
				"served":  true,
				"storage": false,
			},
		}).
		Build()

	xrdForXR2 := tu.NewResource("apiextensions.crossplane.io/v1", CompositeResourceDefinitionKind, "xr2s.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
			"kind":     "XR2",
			"plural":   "xr2s",
			"singular": "xr2",
		}).
		WithSpecField("versions", []any{
			map[string]any{
				"name":    "v1alpha1",
				"served":  true,
				"storage": true,
			},
		}).
		Build()

	type args struct {
		gvk schema.GroupVersionKind
	}

	tests := map[string]struct {
		reason       string
		cluster      xrdCluster
		args         args
		want         *un.Unstructured
		wantErr      bool
		errSubstring string
	}{
		"MatchingXRFound": {
			reason:  "Should return the XRD that defines the XR kind",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdForXR1, xrdForXR2}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "XR1",
				},
			},
			want:    xrdForXR1,
			wantErr: false,
		},
		"MatchingXRWithDifferentVersion": {
			reason:  "Should return the XRD that defines the XR kind with matching version",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdForXR1, xrdForXR2}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v2", // Using v2 version
					Kind:    "XR1",
				},
			},
			want:    xrdForXR1,
			wantErr: false,
		},
		"NoMatchingXR": {
			reason:  "Should return error when no XRD defines the XR kind",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdForXR1, xrdForXR2}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "NonExistentXR", // No XRD defines this
				},
			},
			want:         nil,
			wantErr:      true,
			errSubstring: "no XRD found that defines XR type",
		},
		"GetError": {
			reason:  "Should propagate an error getting the definition from the cluster",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdForXR1}, getErr: errors.New("get error")},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1",
					Kind:    "XR1",
				},
			},
			want:         nil,
			wantErr:      true,
			errSubstring: "get error",
		},
		"DifferentGroup": {
			reason:  "Should not match XRD with different group",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdForXR1}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "different.org", // Different group
					Version: "v1",
					Kind:    "XR1", // Same kind
				},
			},
			want:         nil,
			wantErr:      true,
			errSubstring: "no XRD found that defines XR type",
		},
		"VersionNotFound": {
			reason:  "Should not match XRD if version doesn't exist",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdForXR1}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v3", // Version doesn't exist in XRD
					Kind:    "XR1",
				},
			},
			want:         nil,
			wantErr:      true,
			errSubstring: "no XRD found that defines XR type",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			c := tt.cluster.client(ctx, t)

			got, err := c.GetXRDForXR(ctx, tt.args.gvk)

			if tt.wantErr {
				if err == nil {
					t.Errorf("\n%s\nGetXRDForXR(): expected error but got none", tt.reason)
					return
				}

				if tt.errSubstring != "" && !strings.Contains(err.Error(), tt.errSubstring) {
					t.Errorf("\n%s\nGetXRDForXR(): expected error containing %q, got %q", tt.reason, tt.errSubstring, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nGetXRDForXR(): unexpected error: %v", tt.reason, err)
				return
			}

			if diff := cmp.Diff(tt.want.GetName(), got.GetName()); diff != "" {
				t.Errorf("\n%s\nGetXRDForXR(): -want name, +got name:\n%s", tt.reason, diff)
			}

			// Verify it's the right XRD by checking the XR kind
			xrKind, found, _ := un.NestedString(got.Object, "spec", "names", "kind")
			if !found || xrKind != tt.args.gvk.Kind {
				t.Errorf("\n%s\nGetXRDForXR(): returned XRD has wrong XR kind, want %s, got %s",
					tt.reason, tt.args.gvk.Kind, xrKind)
			}
		})
	}
}

func TestDefaultDefinitionClient_Initialize(t *testing.T) {
	tests := map[string]struct {
		reason       string
		mockResource *tu.MockResourceClient
		wantErr      string
	}{
		"XRDListForbidden": {
			reason: "Should initialize though the credentials cannot list XRDs, since XRDs are fetched on demand",
			mockResource: tu.NewMockResourceClient().
				WithFoundGVKs([]schema.GroupVersionKind{XRDv1GVK, XRDv2GVK}).
				WithErrorOnListOf(XRDv1GVK, XRDv2GVK).
				Build(),
		},
		"DiscoveryError": {
			reason: "Should return an error when the served XRD versions cannot be discovered",
			mockResource: tu.NewMockResourceClient().
				WithoutFoundGVKs("discovery error").
				Build(),
			wantErr: "cannot get XRD GVKs: discovery error",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := NewDefinitionClient(tt.mockResource, tu.NewMockTypeConverter().Build(), tu.TestLogger(t, false))

			gotErr := ""
			if err := c.Initialize(t.Context()); err != nil {
				gotErr = err.Error()
			}

			if diff := cmp.Diff(tt.wantErr, gotErr); diff != "" {
				t.Errorf("\n%s\nInitialize(): -want error, +got error:\n%s", tt.reason, diff)
			}
		})
	}
}

func TestDefaultDefinitionClient_IsClaimResource(t *testing.T) {

	tests := map[string]struct {
		reason   string
		cluster  xrdCluster
		resource *un.Unstructured
		expected bool
	}{
		"ResourceIsClaim": {
			reason: "Should return true when resource is a claim type",
			cluster: xrdCluster{xrds: []*un.Unstructured{
				// Create a mock XRD that defines this as a claim
				tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "xtestresources.example.org").
					WithSpecField("group", "example.org").
					WithSpecField("names", map[string]any{
						"kind":     "XTestResource",
						"plural":   "xtestresources",
						"singular": "xtestresource",
					}).
					WithSpecField("claimNames", map[string]any{
						"kind":     "TestClaim",
						"plural":   "testclaims",
						"singular": "testclaim",
					}).
					WithSpecField("versions", []any{map[string]any{"name": "v1"}}).
					Build(),
			}},
			resource: tu.NewResource("example.org/v1", "TestClaim", "test-claim").
				InNamespace("default").
				Build(),
			expected: true,
		},
		"ResourceIsNotClaim": {
			reason: "Should return false when resource is not a claim type",
			cluster: xrdCluster{xrds: []*un.Unstructured{
				// Create a mock XRD that defines this as an XR (no claimNames)
				tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "testxrs.example.org").
					WithSpecField("group", "example.org").
					WithSpecField("names", map[string]any{
						"kind":     "TestXR",
						"plural":   "testxrs",
						"singular": "testxr",
					}).
					// No claimNames field
					Build(),
			}},
			resource: tu.NewResource("example.org/v1", "TestXR", "test-xr").
				InNamespace("default").
				Build(),
			expected: false,
		},
		"GetError": {
			reason:  "Should return false when getting the definition from the cluster fails",
			cluster: xrdCluster{getErr: errors.New("get error")},
			resource: tu.NewResource("example.org/v1", "TestResource", "test-resource").
				Build(),
			expected: false, // Should return false on error
		},
		"NoMatchingXRD": {
			reason: "Should return false when no matching XRD exists",
			cluster: xrdCluster{xrds: []*un.Unstructured{
				// Create an XRD that doesn't match our resource
				tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "xotherresources.example.org").
					WithSpecField("group", "example.org").
					WithSpecField("names", map[string]any{
						"kind":     "XOtherResource",
						"plural":   "xotherresources",
						"singular": "xotherresource",
					}).
					WithSpecField("claimNames", map[string]any{
						"kind":     "OtherClaim",
						"plural":   "otherclaims",
						"singular": "otherclaim",
					}).
					Build(),
			}},
			resource: tu.NewResource("example.org/v1", "TestClaim", "test-claim").
				Build(),
			expected: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			c := tt.cluster.client(ctx, t)

			// Call the function under test
			result := c.IsClaimResource(ctx, tt.resource)

			// Check the result
			if result != tt.expected {
				t.Errorf("\n%s\nIsClaimResource() = %v, want %v", tt.reason, result, tt.expected)
			}
		})
	}
}

func TestDefaultDefinitionClient_GetCompositeSchema(t *testing.T) {

	// v1 XRD: defines an XR (no claimNames here for simplicity).
	xrdV1 := tu.NewResource("apiextensions.crossplane.io/v1", CompositeResourceDefinitionKind, "xlegacyresources.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
			"kind":     "XLegacyResource",
			"plural":   "xlegacyresources",
			"singular": "xlegacyresource",
		}).
		WithSpecField("versions", []any{
			map[string]any{"name": "v1alpha1"},
		}).
		Build()

	// v2 XRD: declares a non-legacy scope so the scope-based detector
	// returns SchemaModern. (A v2 XRD without scope, or with
	// scope=LegacyCluster, would correctly resolve as SchemaLegacy — see
	// the V2XRD_LegacyScope_LegacySchema case below.)
	xrdV2 := tu.NewResource("apiextensions.crossplane.io/v2", CompositeResourceDefinitionKind, "xmodernresources.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
			"kind":     "XModernResource",
			"plural":   "xmodernresources",
			"singular": "xmodernresource",
		}).
		WithSpecField("scope", "Cluster").
		WithSpecField("versions", []any{
			map[string]any{"name": "v1alpha1"},
		}).
		Build()

	// v2 XRD whose scope is "LegacyCluster". This shape can occur in real
	// clusters when a user-posted v1 XRD round-trips through the apiserver's
	// conversion to v2 storage form: the converted v2 object preserves
	// scope="LegacyCluster" verbatim. Our scope-based detector should treat
	// this as Legacy regardless of the apiVersion stamp.
	xrdV2LegacyScope := tu.NewResource("apiextensions.crossplane.io/v2", CompositeResourceDefinitionKind, "xlegacyscopedresources.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
			"kind":     "XLegacyScopedResource",
			"plural":   "xlegacyscopedresources",
			"singular": "xlegacyscopedresource",
		}).
		WithSpecField("scope", "LegacyCluster").
		WithSpecField("versions", []any{
			map[string]any{"name": "v1alpha1"},
		}).
		Build()

	// v1 XRD that also publishes a claim. Used to verify the helper resolves
	// via the claim path when given a claim GVK (the XR/claim distinction is
	// a Crossplane CompositeResourceDefinition feature, not a Go type — the
	// claim kind here is just the user-supplied "ClaimedResource" below).
	xrdV1WithClaim := tu.NewResource("apiextensions.crossplane.io/v1", CompositeResourceDefinitionKind, "xclaimedresources.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
			"kind":     "XClaimedResource",
			"plural":   "xclaimedresources",
			"singular": "xclaimedresource",
		}).
		WithSpecField("claimNames", map[string]any{
			"kind":     "ClaimedResource",
			"plural":   "claimedresources",
			"singular": "claimedresource",
		}).
		WithSpecField("versions", []any{
			map[string]any{"name": "v1alpha1"},
		}).
		Build()

	type args struct {
		gvk schema.GroupVersionKind
	}

	tests := map[string]struct {
		reason       string
		cluster      xrdCluster
		args         args
		want         ucomposite.Schema
		wantErr      bool
		errSubstring string
	}{
		"V1XRD_LegacySchema": {
			reason:  "Should return SchemaLegacy for an XR defined by a v1 XRD",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdV1}, xrdGVKs: []schema.GroupVersionKind{XRDv1GVK}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1alpha1",
					Kind:    "XLegacyResource",
				},
			},
			want: ucomposite.SchemaLegacy,
		},
		"V2XRD_ModernSchema": {
			reason:  "Should return SchemaModern for a v2 XRD with non-legacy scope",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdV2}, xrdGVKs: []schema.GroupVersionKind{XRDv2GVK}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1alpha1",
					Kind:    "XModernResource",
				},
			},
			want: ucomposite.SchemaModern,
		},
		"V2XRD_LegacyScope_LegacySchema": {
			reason:  "Should return SchemaLegacy when a v2-form XRD's scope is LegacyCluster (e.g. v1 XRD round-tripped through conversion)",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdV2LegacyScope}, xrdGVKs: []schema.GroupVersionKind{XRDv2GVK}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1alpha1",
					Kind:    "XLegacyScopedResource",
				},
			},
			want: ucomposite.SchemaLegacy,
		},
		"ClaimGVK_ResolvesViaClaimPath": {
			reason:  "Should return the schema of the XRD that publishes a claim, given the claim GVK",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdV1WithClaim}, xrdGVKs: []schema.GroupVersionKind{XRDv1GVK}},
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1alpha1",
					Kind:    "ClaimedResource", // claim kind, not XR kind
				},
			},
			want: ucomposite.SchemaLegacy,
		},
		"NoXRDFound_Error": {
			reason:  "Should return an error when no XRD defines the GVK",
			cluster: xrdCluster{xrds: []*un.Unstructured{xrdV2}, xrdGVKs: []schema.GroupVersionKind{XRDv2GVK}}, // only v2 XRD known
			args: args{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1alpha1",
					Kind:    "Unknown",
				},
			},
			wantErr:      true,
			errSubstring: "no XRD",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			c := tt.cluster.client(ctx, t)

			got, err := c.GetCompositeSchema(ctx, tt.args.gvk)

			if tt.wantErr {
				if err == nil {
					t.Errorf("\n%s\nGetCompositeSchema(): expected error but got none", tt.reason)
					return
				}

				if tt.errSubstring != "" && !strings.Contains(err.Error(), tt.errSubstring) {
					t.Errorf("\n%s\nGetCompositeSchema(): expected error containing %q, got %q", tt.reason, tt.errSubstring, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nGetCompositeSchema(): unexpected error: %v", tt.reason, err)
				return
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("\n%s\nGetCompositeSchema(): -want, +got:\n%s", tt.reason, diff)
			}
		})
	}
}

// xrdCluster is a cluster holding XRDs and the CRDs Crossplane generates for them, with discovery
// serving each XR and claim type at every version its XRD lists.
type xrdCluster struct {
	xrds []*un.Unstructured
	// xrdGVKs are the XRD versions discovery serves, each serving every XRD. Default: v1 only.
	xrdGVKs []schema.GroupVersionKind
	// unestablished are XRDs whose types are not served, so they have no CRD of their own.
	unestablished []*un.Unstructured
	// managed are served types no XRD defines, such as managed resources.
	managed []schema.GroupVersionKind
	// unownedClaimCRDs drops the controller reference from claim CRDs, as on a CRD applied by hand.
	unownedClaimCRDs bool
	// listForbidden denies listing XRDs, as for credentials without that permission.
	listForbidden bool
	// getErr, if set, is what getting any XRD or CRD fails with.
	getErr error
}

// client returns an initialized DefaultDefinitionClient over the cluster.
func (x xrdCluster) client(ctx context.Context, t *testing.T) *DefaultDefinitionClient {
	t.Helper()

	xrdGVKs := x.xrdGVKs
	if len(xrdGVKs) == 0 {
		xrdGVKs = []schema.GroupVersionKind{XRDv1GVK}
	}

	served := make([]*un.Unstructured, 0, len(xrdGVKs)*(len(x.xrds)+len(x.unestablished)))

	resources := make([]*un.Unstructured, 0, 3*len(x.xrds))
	plurals := map[schema.GroupVersionKind]string{}

	for _, gvk := range x.managed {
		plurals[gvk] = strings.ToLower(gvk.Kind) + "s"
		// A provider's CRD, which its ProviderRevision controls.
		resources = append(resources, tu.NewResource("apiextensions.k8s.io/v1", "CustomResourceDefinition", plurals[gvk]+"."+gvk.Group).
			WithControllerReference("ProviderRevision", "provider-nop-abc123", "pkg.crossplane.io/v1", "rev-uid").
			Build())
	}

	for _, xrd := range append(append([]*un.Unstructured{}, x.xrds...), x.unestablished...) {
		// The apiserver serves an XRD at every XRD version, converting it.
		for _, gvk := range xrdGVKs {
			v := xrd.DeepCopy()
			v.SetAPIVersion(gvk.GroupVersion().String())
			served = append(served, v)
		}
	}

	for _, xrd := range x.xrds {
		group, _, _ := un.NestedString(xrd.Object, "spec", "group")
		versions, _, _ := un.NestedSlice(xrd.Object, "spec", "versions")

		serve := func(kind, plural string, owned bool) {
			crd := tu.NewResource("apiextensions.k8s.io/v1", "CustomResourceDefinition", plural+"."+group)
			if owned {
				crd = crd.WithControllerReference(CompositeResourceDefinitionKind, xrd.GetName(), xrd.GetAPIVersion(), "xrd-uid")
			}

			resources = append(resources, crd.Build())

			for _, v := range versions {
				name, _, _ := un.NestedString(v.(map[string]any), "name")
				plurals[schema.GroupVersionKind{Group: group, Version: name, Kind: kind}] = plural
			}
		}

		kind, _, _ := un.NestedString(xrd.Object, "spec", "names", "kind")
		plural, _, _ := un.NestedString(xrd.Object, "spec", "names", "plural")
		serve(kind, plural, true)

		if claimKind, ok, _ := un.NestedString(xrd.Object, "spec", "claimNames", "kind"); ok {
			claimPlural, _, _ := un.NestedString(xrd.Object, "spec", "claimNames", "plural")
			serve(claimKind, claimPlural, !x.unownedClaimCRDs)
		}
	}

	rb := tu.NewMockResourceClient().
		WithFoundGVKs(xrdGVKs).
		WithResourcesExist(append(resources, served...)...).
		WithListableResources(served...)
	if x.getErr != nil {
		rb = rb.WithGetResourceError(x.getErr)
	}

	if x.listForbidden {
		rb = rb.WithErrorOnListOf(xrdGVKs...)
	}

	tc := tu.NewMockTypeConverter().
		WithGetResourceNameForGVK(func(_ context.Context, gvk schema.GroupVersionKind) (string, error) {
			if p, ok := plurals[gvk]; ok {
				return p, nil
			}

			return "", errors.Errorf("no resource found for kind %s in group version %s", gvk.Kind, gvk.GroupVersion())
		}).
		Build()

	c, ok := NewDefinitionClient(rb.Build(), tc, tu.TestLogger(t, false)).(*DefaultDefinitionClient)
	if !ok {
		t.Fatal("NewDefinitionClient did not return a *DefaultDefinitionClient")
	}

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize(): unexpected error: %v", err)
	}

	return c
}

// lookupXRDTwice calls lookup twice, as concurrent or repeated diffs do, and returns the name of the
// XRD each call found ("" for none) and its error. It renames what the first call returns, so the
// second shows whether a caller's change to an XRD it was given reaches later callers.
func lookupXRDTwice(lookup func() (*un.Unstructured, error)) (names, errs []string) {
	for range 2 {
		xrd, err := lookup()

		name, msg := "", ""
		if xrd != nil {
			name = xrd.GetName()
			xrd.SetName("modified-by-caller")
		}

		if err != nil {
			msg = err.Error()
		}

		names = append(names, name)
		errs = append(errs, msg)
	}

	return names, errs
}

func TestGetXRDForXRWithoutListPermission(t *testing.T) {
	// A v1 XRD serving its XR at two versions.
	v1XRD := tu.NewResource(CrossplaneAPIExtGroupV1, CompositeResourceDefinitionKind, "xbuckets.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{"kind": "XBucket", "plural": "xbuckets"}).
		WithSpecField("versions", []any{map[string]any{"name": "v1alpha1"}, map[string]any{"name": "v1"}}).
		Build()

	// A v2 XRD of a namespaced XR, in the same group as the v1 XRD.
	v2XRD := tu.NewResource(CrossplaneAPIExtGroupV2, CompositeResourceDefinitionKind, "databases.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("scope", "Namespaced").
		WithSpecField("names", map[string]any{"kind": "Database", "plural": "databases"}).
		WithSpecField("versions", []any{map[string]any{"name": "v1"}}).
		Build()

	// An XRD that never established: a provider's CRD already has its name, so discovery
	// serves the provider's kind under its plural, not the XRD's kind.
	collidingXRD := tu.NewResource(CrossplaneAPIExtGroupV1, CompositeResourceDefinitionKind, "buckets.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{"kind": "XStorage", "plural": "buckets"}).
		WithSpecField("versions", []any{map[string]any{"name": "v1"}}).
		Build()

	bothXRDVersions := []schema.GroupVersionKind{XRDv1GVK, XRDv2GVK}
	cluster := xrdCluster{
		xrds:          []*un.Unstructured{v1XRD, v2XRD},
		xrdGVKs:       bothXRDVersions,
		unestablished: []*un.Unstructured{collidingXRD},
		managed:       []schema.GroupVersionKind{{Group: "example.org", Version: "v1", Kind: "Bucket"}},
		listForbidden: true,
	}

	type want struct {
		names []string
		errs  []string
	}

	tests := map[string]struct {
		reason string
		gvk    schema.GroupVersionKind
		want   want
	}{
		"V1XRDFirstVersion": {
			reason: "Should get the v1 XRD for its XR at the first version it serves",
			gvk:    schema.GroupVersionKind{Group: "example.org", Version: "v1alpha1", Kind: "XBucket"},
			want:   want{names: []string{"xbuckets.example.org", "xbuckets.example.org"}, errs: []string{"", ""}},
		},
		"V1XRDSecondVersion": {
			reason: "Should get the v1 XRD for its XR at another version it serves",
			gvk:    schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "XBucket"},
			want:   want{names: []string{"xbuckets.example.org", "xbuckets.example.org"}, errs: []string{"", ""}},
		},
		"V2XRD": {
			reason: "Should get the v2 XRD for its XR",
			gvk:    schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Database"},
			want:   want{names: []string{"databases.example.org", "databases.example.org"}, errs: []string{"", ""}},
		},
		"VersionNotServed": {
			reason: "Should find no XRD for an XR kind at a version its XRD does not serve",
			gvk:    schema.GroupVersionKind{Group: "example.org", Version: "v2", Kind: "XBucket"},
			want: want{names: []string{"", ""}, errs: []string{
				"no XRD found that defines XR type example.org/v2, Kind=XBucket: no resource found for kind XBucket in group version example.org/v2",
				"no XRD found that defines XR type example.org/v2, Kind=XBucket: no resource found for kind XBucket in group version example.org/v2",
			}},
		},
		"CoreType": {
			reason: "Should find no XRD for a core type",
			gvk:    schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
			want: want{names: []string{"", ""}, errs: []string{
				"no XRD found that defines XR type /v1, Kind=ConfigMap",
				"no XRD found that defines XR type /v1, Kind=ConfigMap",
			}},
		},
		"ManagedResource": {
			reason: "Should find no XRD for a provider's type, though an XRD that never established has the name its plural gives",
			gvk:    schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Bucket"},
			want: want{names: []string{"", ""}, errs: []string{
				"no XRD found that defines XR type example.org/v1, Kind=Bucket",
				"no XRD found that defines XR type example.org/v1, Kind=Bucket",
			}},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			c := cluster.client(ctx, t)

			names, errs := lookupXRDTwice(func() (*un.Unstructured, error) { return c.GetXRDForXR(t.Context(), tt.gvk) })

			if diff := cmp.Diff(tt.want, want{names: names, errs: errs}, cmp.AllowUnexported(want{})); diff != "" {
				t.Errorf("\n%s\nGetXRDForXR(...): -want, +got:\n%s", tt.reason, diff)
			}
		})
	}
}

func TestGetXRDForClaim(t *testing.T) {
	// A v1 XRD offering a claim at two versions.
	v1XRD := tu.NewResource(CrossplaneAPIExtGroupV1, CompositeResourceDefinitionKind, "xbuckets.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{"kind": "XBucket", "plural": "xbuckets"}).
		WithSpecField("claimNames", map[string]any{"kind": "Bucket", "plural": "buckets"}).
		WithSpecField("versions", []any{map[string]any{"name": "v1alpha1"}, map[string]any{"name": "v1"}}).
		Build()

	// A v1 XRD offering no claim, in the same group.
	noClaimXRD := tu.NewResource(CrossplaneAPIExtGroupV1, CompositeResourceDefinitionKind, "xqueues.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{"kind": "XQueue", "plural": "xqueues"}).
		WithSpecField("versions", []any{map[string]any{"name": "v1"}}).
		Build()

	xrds := []*un.Unstructured{v1XRD, noClaimXRD}
	bothXRDVersions := []schema.GroupVersionKind{XRDv1GVK, XRDv2GVK}
	managed := []schema.GroupVersionKind{{Group: "example.org", Version: "v1", Kind: "Instance"}}

	bucketV1 := schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Bucket"}
	found := []string{"xbuckets.example.org", "xbuckets.example.org"}
	noErrs := []string{"", ""}
	notAClaim := func(gvk schema.GroupVersionKind) []string {
		msg := "no XRD found that defines claim type " + gvk.String()
		return []string{msg, msg}
	}

	type want struct {
		names []string
		errs  []string
	}

	tests := map[string]struct {
		reason  string
		cluster xrdCluster
		gvk     schema.GroupVersionKind
		want    want
	}{
		"ClaimFoundThroughCRDOwner": {
			reason:  "Should get the XRD that controls the claim's CRD, though the credentials cannot list XRDs",
			cluster: xrdCluster{xrds: xrds, xrdGVKs: bothXRDVersions, listForbidden: true},
			gvk:     bucketV1,
			want:    want{names: found, errs: noErrs},
		},
		"ClaimAtAnotherVersion": {
			reason:  "Should get the XRD for its claim at another version it serves",
			cluster: xrdCluster{xrds: xrds, xrdGVKs: bothXRDVersions, listForbidden: true},
			gvk:     schema.GroupVersionKind{Group: "example.org", Version: "v1alpha1", Kind: "Bucket"},
			want:    want{names: found, errs: noErrs},
		},
		"XRKindIsNotAClaim": {
			reason:  "Should find no XRD defining an XR kind as a claim, though an XRD controls its CRD",
			cluster: xrdCluster{xrds: xrds, listForbidden: true},
			gvk:     schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "XBucket"},
			want:    want{names: []string{"", ""}, errs: notAClaim(schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "XBucket"})},
		},
		"ManagedResourceIsNotAClaim": {
			reason:  "Should find no XRD for a type whose CRD a provider controls",
			cluster: xrdCluster{xrds: xrds, managed: managed, listForbidden: true},
			gvk:     managed[0],
			want:    want{names: []string{"", ""}, errs: notAClaim(managed[0])},
		},
		"CRDWithoutOwnerFallsBackToList": {
			reason:  "Should find the XRD among all XRDs when the claim's CRD has no controller",
			cluster: xrdCluster{xrds: xrds, xrdGVKs: bothXRDVersions, unownedClaimCRDs: true},
			gvk:     bucketV1,
			want:    want{names: found, errs: noErrs},
		},
		"CRDWithoutOwnerAndNoMatchingXRD": {
			reason:  "Should find no XRD when the CRD has no controller and no XRD offers the claim",
			cluster: xrdCluster{xrds: []*un.Unstructured{noClaimXRD}, managed: managed, unownedClaimCRDs: true},
			gvk:     managed[0],
			want:    want{names: []string{"", ""}, errs: notAClaim(managed[0])},
		},
		"CRDWithoutOwnerAndListForbidden": {
			reason:  "Should report that the XRDs cannot be listed when the fallback needs them",
			cluster: xrdCluster{xrds: xrds, unownedClaimCRDs: true, listForbidden: true},
			gvk:     bucketV1,
			want: want{names: []string{"", ""}, errs: []string{
				`cannot get XRDs: cannot list XRDs: cannot list items for GVK apiextensions.crossplane.io/v1, Kind=CompositeResourceDefinition: compositeresourcedefinitions.apiextensions.crossplane.io is forbidden: list is not permitted`,
				`cannot get XRDs: cannot list XRDs: cannot list items for GVK apiextensions.crossplane.io/v1, Kind=CompositeResourceDefinition: compositeresourcedefinitions.apiextensions.crossplane.io is forbidden: list is not permitted`,
			}},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			c := tt.cluster.client(ctx, t)

			names, errs := lookupXRDTwice(func() (*un.Unstructured, error) { return c.GetXRDForClaim(t.Context(), tt.gvk) })

			if diff := cmp.Diff(tt.want, want{names: names, errs: errs}, cmp.AllowUnexported(want{})); diff != "" {
				t.Errorf("\n%s\nGetXRDForClaim(...): -want, +got:\n%s", tt.reason, diff)
			}
		})
	}
}
