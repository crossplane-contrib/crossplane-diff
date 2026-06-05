package diffprocessor

import (
	"context"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	v1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/google/go-cmp/cmp"
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
// This test demonstrates a bug where cache keys didn't include namespace, causing
// collisions when same-named resources existed in different namespaces.
