package diffprocessor

import (
	"context"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"

	v1 "github.com/crossplane/crossplane/v2/proto/fn/v1"
)

func TestRequirementsProvider_ProvideRequirements(t *testing.T) {
	ctx := t.Context()

	// Create resources for testing
	configMap := tu.NewResource("v1", "ConfigMap", "config1").Build()
	secret := tu.NewResource("v1", "Secret", "secret1").Build()

	tests := map[string]struct {
		requirements           map[string]v1.Requirements
		setupResourceClient    func() *tu.MockResourceClient
		setupEnvironmentClient func() *tu.MockEnvironmentClient
		wantCount              int
		wantNames              []string
		wantErr                bool
	}{
		"EmptyRequirements": {
			requirements: map[string]v1.Requirements{},
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"},
					).
					Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithNoEnvironmentConfigs().
					Build()
			},
			wantCount: 0,
			wantErr:   false,
		},
		"NameSelector": {
			requirements: map[string]v1.Requirements{
				"step1": {
					Resources: map[string]*v1.ResourceSelector{
						"config": {
							ApiVersion: "v1",
							Kind:       "ConfigMap",
							Match: &v1.ResourceSelector_MatchName{
								MatchName: "config1",
							},
						},
					},
				},
			},
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
					).
					WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
						if gvk.Kind == "ConfigMap" && name == "config1" {
							return configMap, nil
						}

						return nil, errors.New("resource not found")
					}).
					Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithNoEnvironmentConfigs().
					Build()
			},
			wantCount: 1,
			wantNames: []string{"config1"},
			wantErr:   false,
		},
		"LabelSelector": {
			requirements: map[string]v1.Requirements{
				"step1": {
					Resources: map[string]*v1.ResourceSelector{
						"config": {
							ApiVersion: "v1",
							Kind:       "ConfigMap",
							Match: &v1.ResourceSelector_MatchLabels{
								MatchLabels: &v1.MatchLabels{
									Labels: map[string]string{
										"app": "test-app",
									},
								},
							},
						},
					},
				},
			},
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
					).
					WithGetResourcesByLabel(func(_ context.Context, _ schema.GroupVersionKind, _ string, sel metav1.LabelSelector) ([]*un.Unstructured, error) {
						// Return resources for label-based selectors
						if sel.MatchLabels["app"] == "test-app" {
							return []*un.Unstructured{configMap}, nil
						}

						return []*un.Unstructured{}, nil
					}).
					Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithNoEnvironmentConfigs().
					Build()
			},
			wantCount: 1,
			wantNames: []string{"config1"},
			wantErr:   false,
		},
		"MultipleSelectors": {
			requirements: map[string]v1.Requirements{
				"step1": {
					Resources: map[string]*v1.ResourceSelector{
						"config": {
							ApiVersion: "v1",
							Kind:       "ConfigMap",
							Match: &v1.ResourceSelector_MatchName{
								MatchName: "config1",
							},
						},
						"secret": {
							ApiVersion: "v1",
							Kind:       "Secret",
							Match: &v1.ResourceSelector_MatchName{
								MatchName: "secret1",
							},
						},
					},
				},
			},
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"},
					).
					WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
						if gvk.Kind == "ConfigMap" && name == "config1" {
							return configMap, nil
						}

						if gvk.Kind == "Secret" && name == "secret1" {
							return secret, nil
						}

						return nil, errors.New("resource not found")
					}).
					Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithNoEnvironmentConfigs().
					Build()
			},
			wantCount: 2,
			wantNames: []string{"config1", "secret1"},
			wantErr:   false,
		},
		"ResourceNotFound": {
			requirements: map[string]v1.Requirements{
				"step1": {
					Resources: map[string]*v1.ResourceSelector{
						"missing": {
							ApiVersion: "v1",
							Kind:       "ConfigMap",
							Match: &v1.ResourceSelector_MatchName{
								MatchName: "missing-resource",
							},
						},
					},
				},
			},
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithNamespacedResource(
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
					).
					WithResourceNotFound().
					Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithNoEnvironmentConfigs().
					Build()
			},
			wantErr: true,
		},
		"EnvironmentConfigsAvailable": {
			requirements: map[string]v1.Requirements{
				"step1": {
					Resources: map[string]*v1.ResourceSelector{
						"config": {
							ApiVersion: "v1",
							Kind:       "ConfigMap",
							Match: &v1.ResourceSelector_MatchName{
								MatchName: "config1",
							},
						},
					},
				},
			},
			setupResourceClient: func() *tu.MockResourceClient {
				// This resource client should not be called because the resource is in the env configs
				return tu.NewMockResourceClient().
					WithNamespacedResource(
						schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
					).
					WithGetResource(func(_ context.Context, _ schema.GroupVersionKind, _, _ string) (*un.Unstructured, error) {
						return nil, errors.New("should not be called")
					}).
					Build()
			},
			setupEnvironmentClient: func() *tu.MockEnvironmentClient {
				return tu.NewMockEnvironmentClient().
					WithSuccessfulEnvironmentConfigsFetch([]*un.Unstructured{configMap}).
					Build()
			},
			wantCount: 1,
			wantNames: []string{"config1"},
			wantErr:   false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Set up clients
			resourceClient := tt.setupResourceClient()
			environmentClient := tt.setupEnvironmentClient()

			// Create the requirements provider
			provider := NewRequirementsProvider(
				resourceClient,
				environmentClient,
				nil, // renderFn not needed for this test
				tu.TestLogger(t, false),
			)

			// Initialize the provider to cache any environment configs
			if err := provider.Initialize(ctx); err != nil {
				t.Fatalf("Failed to initialize provider: %v", err)
			}

			// Call the method being tested
			resources, err := provider.ProvideRequirements(ctx, tt.requirements, "default")

			// Check error cases
			if tt.wantErr {
				if err == nil {
					t.Errorf("ProvideRequirements() expected error but got none")
				}

				return
			}

			if err != nil {
				t.Fatalf("ProvideRequirements() unexpected error: %v", err)
			}

			// Check resource count
			if diff := cmp.Diff(tt.wantCount, len(resources)); diff != "" {
				t.Errorf("ProvideRequirements() resource count mismatch (-want +got):\n%s", diff)
			}

			// Verify expected resource names if specified
			if tt.wantNames != nil {
				foundNames := make(map[string]bool)
				for _, res := range resources {
					foundNames[res.GetName()] = true
				}

				for _, name := range tt.wantNames {
					if !foundNames[name] {
						t.Errorf("Expected resource %q not found in result", name)
					}
				}
			}
		})
	}
}
