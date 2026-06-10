package diffprocessor

import (
	"context"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	v1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// TestRequirementsProvider_ResolveSelectors covers the selector-flat entry
// point used by the new render.CompositionOutputs.RequiredResources shape.
func TestRequirementsProvider_ResolveSelectors(t *testing.T) {
	ctx := t.Context()

	configMap := tu.NewResource("v1", "ConfigMap", "config1").WithNamespace("default").Build()
	secret := tu.NewResource("v1", "Secret", "secret1").WithNamespace("default").Build()

	selFor := func(kind, name string) *v1.ResourceSelector {
		return &v1.ResourceSelector{
			ApiVersion: "v1",
			Kind:       kind,
			Match:      &v1.ResourceSelector_MatchName{MatchName: name},
		}
	}

	tests := map[string]struct {
		selectors []*v1.ResourceSelector
		setupRes  func() *tu.MockResourceClient
		wantCount int
		wantNames []string
		wantErr   bool
	}{
		"Nil": {
			selectors: nil,
			setupRes: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().Build()
			},
			wantCount: 0,
		},
		"Empty": {
			selectors: []*v1.ResourceSelector{},
			setupRes: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().Build()
			},
			wantCount: 0,
		},
		"SingleMatchName": {
			selectors: []*v1.ResourceSelector{selFor("ConfigMap", "config1")},
			setupRes: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}).
					WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
						if gvk.Kind == "ConfigMap" && name == "config1" {
							return configMap, nil
						}

						return nil, errors.New("not found")
					}).
					Build()
			},
			wantCount: 1,
			wantNames: []string{"config1"},
		},
		"TwoSelectorsDistinctKinds": {
			selectors: []*v1.ResourceSelector{
				selFor("ConfigMap", "config1"),
				selFor("Secret", "secret1"),
			},
			setupRes: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"},
					).
					WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
						switch {
						case gvk.Kind == "ConfigMap" && name == "config1":
							return configMap, nil
						case gvk.Kind == "Secret" && name == "secret1":
							return secret, nil
						}

						return nil, errors.New("not found")
					}).
					Build()
			},
			wantCount: 2,
			wantNames: []string{"config1", "secret1"},
		},
		"FetchError": {
			selectors: []*v1.ResourceSelector{selFor("ConfigMap", "missing")},
			setupRes: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}).
					WithGetResource(func(_ context.Context, _ schema.GroupVersionKind, _, _ string) (*un.Unstructured, error) {
						return nil, errors.New("boom")
					}).
					Build()
			},
			wantErr: true,
		},
		"MatchLabels": {
			// processSelector → processLabelSelector → GetResourcesByLabel.
			// Two ConfigMaps both labelled tier=cache; selector requests
			// tier=cache and we expect both back. Different code path from
			// MatchName (no GetResource, no namespace-aware cache).
			selectors: []*v1.ResourceSelector{
				{
					ApiVersion: "v1",
					Kind:       "ConfigMap",
					Match: &v1.ResourceSelector_MatchLabels{
						MatchLabels: &v1.MatchLabels{Labels: map[string]string{"tier": "cache"}},
					},
				},
			},
			setupRes: func() *tu.MockResourceClient {
				cacheA := tu.NewResource("v1", "ConfigMap", "cache-a").WithNamespace("default").Build()
				cacheB := tu.NewResource("v1", "ConfigMap", "cache-b").WithNamespace("default").Build()

				return tu.NewMockResourceClient().
					WithNamespacedResource(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}).
					WithGetResourcesByLabel(func(_ context.Context, gvk schema.GroupVersionKind, _ string, sel metav1.LabelSelector) ([]*un.Unstructured, error) {
						if gvk.Kind == "ConfigMap" && sel.MatchLabels["tier"] == "cache" {
							return []*un.Unstructured{cacheA, cacheB}, nil
						}

						return nil, nil
					}).
					Build()
			},
			wantCount: 2,
			wantNames: []string{"cache-a", "cache-b"},
		},
		"MatchLabelsFetchError": {
			// Error path for the label-selector branch, parallel to FetchError.
			selectors: []*v1.ResourceSelector{
				{
					ApiVersion: "v1",
					Kind:       "ConfigMap",
					Match: &v1.ResourceSelector_MatchLabels{
						MatchLabels: &v1.MatchLabels{Labels: map[string]string{"tier": "cache"}},
					},
				},
			},
			setupRes: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}).
					WithGetResourcesByLabel(func(context.Context, schema.GroupVersionKind, string, metav1.LabelSelector) ([]*un.Unstructured, error) {
						return nil, errors.New("boom")
					}).
					Build()
			},
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			provider := NewRequirementsProvider(
				tt.setupRes(),
				tu.NewMockEnvironmentClient().WithNoEnvironmentConfigs().Build(),
				nil, // renderFn unused by ResolveSelectors
				tu.TestLogger(t, false),
			)
			if err := provider.Initialize(ctx); err != nil {
				t.Fatalf("Initialize: %v", err)
			}

			got, err := provider.ResolveSelectors(ctx, tt.selectors, "default")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveSelectors: expected error, got nil")
				}

				return
			}

			if err != nil {
				t.Fatalf("ResolveSelectors: unexpected error: %v", err)
			}

			if diff := cmp.Diff(tt.wantCount, len(got)); diff != "" {
				t.Errorf("resource count mismatch (-want +got):\n%s", diff)
			}

			if tt.wantNames != nil {
				names := make(map[string]bool, len(got))
				for _, r := range got {
					names[r.GetName()] = true
				}

				for _, want := range tt.wantNames {
					if !names[want] {
						t.Errorf("expected resource %q not found in result", want)
					}
				}
			}
		})
	}
}

// TestRequirementsProvider_NamespaceCollision tests that resources with the same name
// but different namespaces are correctly distinguished in the cache.
//
// Pairs with the (currently skipped) E2E TestCompDiffIntegration/CrossNamespaceResourceCollision,
// which is blocked on function-extra-resources#106 (the function emits Selector{Namespace:""}
// for Reference-typed extras that omit ref.namespace). This unit test exercises the same
// defaulting + cache-keying behavior at our layer without depending on the function, so
// regressions in resolveNamespace / namespace-aware cache keys are caught immediately.
func TestRequirementsProvider_NamespaceCollision(t *testing.T) {
	ctx := t.Context()

	// Two ConfigMaps with the SAME name in DIFFERENT namespaces.
	configInNsA := tu.NewResource("v1", "ConfigMap", "my-config").
		WithNamespace("ns-a").
		WithSpecField("data", "value-a").
		Build()

	configInNsB := tu.NewResource("v1", "ConfigMap", "my-config").
		WithNamespace("ns-b").
		WithSpecField("data", "value-b").
		Build()

	resourceClient := tu.NewMockResourceClient().
		WithNamespacedResource(
			schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		).
		WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, ns, name string) (*un.Unstructured, error) {
			t.Logf("GetResource called for %s/%s in namespace %s - cache miss", gvk.Kind, name, ns)

			if ns == "ns-a" {
				return configInNsA, nil
			}

			if ns == "ns-b" {
				return configInNsB, nil
			}

			return nil, errors.New("resource not found")
		}).
		Build()

	environmentClient := tu.NewMockEnvironmentClient().
		WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{configInNsA, configInNsB}).
		Build()

	provider := NewRequirementsProvider(
		resourceClient,
		environmentClient,
		nil,
		tu.TestLogger(t, true),
	)

	if err := provider.Initialize(ctx); err != nil {
		t.Fatalf("Failed to initialize provider: %v", err)
	}

	// Empty Namespace on the selector — should default to xrNamespace ("ns-a").
	selectors := []*v1.ResourceSelector{
		{
			ApiVersion: "v1",
			Kind:       "ConfigMap",
			Match:      &v1.ResourceSelector_MatchName{MatchName: "my-config"},
		},
	}

	resources, err := provider.ResolveSelectors(ctx, selectors, "ns-a")
	if err != nil {
		t.Fatalf("ResolveSelectors() unexpected error: %v", err)
	}

	if len(resources) != 1 {
		t.Fatalf("Expected 1 resource, got %d", len(resources))
	}

	gotResource := resources[0]
	gotNamespace := gotResource.GetNamespace()
	gotData, _, _ := un.NestedString(gotResource.Object, "spec", "data")

	t.Logf("Got resource: namespace=%s, data=%s (expected: namespace=ns-a, data=value-a)", gotNamespace, gotData)

	if gotNamespace != "ns-a" {
		t.Errorf("Namespace collision bug: expected resource from namespace 'ns-a', got %q", gotNamespace)
	}

	if gotData != "value-a" {
		t.Errorf("Namespace collision bug: expected data 'value-a', got %q (got resource from wrong namespace)", gotData)
	}
}
