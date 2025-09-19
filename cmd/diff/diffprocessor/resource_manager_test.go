package diffprocessor

import (
	"context"
	"strings"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

func TestDefaultResourceManager_FetchCurrentObject(t *testing.T) {
	ctx := t.Context()

	// Create test resources
	existingResource := tu.NewResource("example.org/v1", "TestResource", "existing-resource").
		WithSpecField("field", "value").
		Build()

	// Resource with generateName instead of name
	resourceWithGenerateName := tu.NewResource("example.org/v1", "TestResource", "").
		WithSpecField("field", "value").
		Build()
	resourceWithGenerateName.SetGenerateName("test-resource-")

	// Existing resource that matches generateName pattern
	existingGeneratedResource := tu.NewResource("example.org/v1", "TestResource", "test-resource-abc123").
		WithSpecField("field", "value").
		WithLabels(map[string]string{
			"crossplane.io/composite": "parent-xr",
		}).
		WithAnnotations(map[string]string{
			"crossplane.io/composition-resource-name": "resource-a",
		}).
		Build()

	// Existing resource that matches generateName pattern but has different resource name
	existingGeneratedResourceWithDifferentResName := tu.NewResource("example.org/v1", "TestResource", "test-resource-abc123").
		WithSpecField("field", "value").
		WithLabels(map[string]string{
			"crossplane.io/composite": "parent-xr",
		}).
		WithAnnotations(map[string]string{
			"crossplane.io/composition-resource-name": "resource-b",
		}).
		Build()

	// Composed resource with annotations
	composedResource := tu.NewResource("example.org/v1", "ComposedResource", "composed-resource").
		WithSpecField("field", "value").
		WithLabels(map[string]string{
			"crossplane.io/composite": "parent-xr",
		}).
		WithAnnotations(map[string]string{
			"crossplane.io/composition-resource-name": "resource-a",
		}).
		Build()

	// Parent XR
	parentXR := tu.NewResource("example.org/v1", "XR", "parent-xr").
		WithSpecField("field", "value").
		Build()

	tests := map[string]struct {
		setupResourceClient func() *tu.MockResourceClient
		defClient           *tu.MockDefinitionClient
		composite           *un.Unstructured
		desired             *un.Unstructured
		wantIsNew           bool
		wantResourceID      string
		wantErr             bool
	}{
		"ExistingResourceFoundDirectly": {
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithResourcesExist(existingResource).
					Build()
			},
			defClient:      tu.NewMockDefinitionClient().Build(),
			composite:      nil,
			desired:        existingResource.DeepCopy(),
			wantIsNew:      false,
			wantResourceID: "existing-resource",
			wantErr:        false,
		},
		"ResourceNotFound": {
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithResourceNotFound().
					Build()
			},
			defClient:      tu.NewMockDefinitionClient().Build(),
			composite:      nil,
			desired:        tu.NewResource("example.org/v1", "TestResource", "non-existent").Build(),
			wantIsNew:      true,
			wantResourceID: "",
			wantErr:        false,
		},
		"CompositeIsNil_NewXR": {
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithResourceNotFound().
					Build()
			},
			defClient:      tu.NewMockDefinitionClient().Build(),
			composite:      nil,
			desired:        tu.NewResource("example.org/v1", "XR", "new-xr").Build(),
			wantIsNew:      true,
			wantResourceID: "",
			wantErr:        false,
		},
		"ResourceWithGenerateName_NotFound": {
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithResourceNotFound().
					Build()
			},
			defClient:      tu.NewMockDefinitionClient().Build(),
			composite:      nil,
			desired:        resourceWithGenerateName,
			wantIsNew:      true,
			wantResourceID: "",
			wantErr:        false,
		},
		"ResourceWithGenerateName_FoundByLabelAndAnnotation": {
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					// Return "not found" for direct name lookup
					WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
						return nil, apierrors.NewNotFound(
							schema.GroupResource{
								Group:    gvk.Group,
								Resource: strings.ToLower(gvk.Kind) + "s",
							},
							name,
						)
					}).
					// Return existing resource when looking up by label AND check the composition-resource-name annotation
					WithGetResourcesByLabel(func(_ context.Context, _ schema.GroupVersionKind, _ string, sel metav1.LabelSelector) ([]*un.Unstructured, error) {
						if owner, exists := sel.MatchLabels["crossplane.io/composite"]; exists && owner == "parent-xr" {
							return []*un.Unstructured{existingGeneratedResource, existingGeneratedResourceWithDifferentResName}, nil
						}
						return []*un.Unstructured{}, nil
					}).
					Build()
			},
			defClient: tu.NewMockDefinitionClient().Build(),
			composite: parentXR,
			desired: tu.NewResource("example.org/v1", "TestResource", "").
				WithLabels(map[string]string{
					"crossplane.io/composite": "parent-xr",
				}).
				WithAnnotations(map[string]string{
					"crossplane.io/composition-resource-name": "resource-a",
				}).
				WithGenerateName("test-resource-").
				Build(),
			wantIsNew:      false,
			wantResourceID: "test-resource-abc123",
			wantErr:        false,
		},
		"ComposedResource_FoundByLabelAndAnnotation": {
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					// Return "not found" for direct name lookup to force label lookup
					WithResourceNotFound().
					// Return our existing resource when looking up by label AND check the composition-resource-name annotation
					WithGetResourcesByLabel(func(_ context.Context, _ schema.GroupVersionKind, _ string, sel metav1.LabelSelector) ([]*un.Unstructured, error) {
						if owner, exists := sel.MatchLabels["crossplane.io/composite"]; exists && owner == "parent-xr" {
							return []*un.Unstructured{composedResource}, nil
						}
						return []*un.Unstructured{}, nil
					}).
					Build()
			},
			defClient: tu.NewMockDefinitionClient().Build(),
			composite: parentXR,
			desired: tu.NewResource("example.org/v1", "ComposedResource", "composed-resource").
				WithLabels(map[string]string{
					"crossplane.io/composite": "parent-xr",
				}).
				WithAnnotations(map[string]string{
					"crossplane.io/composition-resource-name": "resource-a",
				}).
				Build(),
			wantIsNew:      false,
			wantResourceID: "composed-resource",
			wantErr:        false,
		},
		"NoAnnotations_NewResource": {
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithResourceNotFound().
					Build()
			},
			defClient: tu.NewMockDefinitionClient().Build(),
			composite: parentXR,
			desired: tu.NewResource("example.org/v1", "Resource", "resource-name").
				WithLabels(map[string]string{
					"crossplane.io/composite": "parent-xr",
				}).
				// No composition-resource-name annotation
				Build(),
			wantIsNew:      true,
			wantResourceID: "",
			wantErr:        false,
		},
		"GenerateNameMismatch": {
			setupResourceClient: func() *tu.MockResourceClient {
				mismatchedResource := tu.NewResource("example.org/v1", "TestResource", "different-prefix-abc123").
					WithLabels(map[string]string{
						"crossplane.io/composite": "parent-xr",
					}).
					WithAnnotations(map[string]string{
						"crossplane.io/composition-resource-name": "resource-a",
					}).
					Build()

				return tu.NewMockResourceClient().
					WithResourceNotFound().
					WithGetResourcesByLabel(func(_ context.Context, _ schema.GroupVersionKind, _ string, sel metav1.LabelSelector) ([]*un.Unstructured, error) {
						if owner, exists := sel.MatchLabels["crossplane.io/composite"]; exists && owner == "parent-xr" {
							return []*un.Unstructured{mismatchedResource}, nil
						}
						return []*un.Unstructured{}, nil
					}).
					Build()
			},
			defClient: tu.NewMockDefinitionClient().Build(),
			composite: parentXR,
			desired: tu.NewResource("example.org/v1", "TestResource", "").
				WithLabels(map[string]string{
					"crossplane.io/composite": "parent-xr",
				}).
				WithAnnotations(map[string]string{
					"crossplane.io/composition-resource-name": "resource-a",
				}).
				WithGenerateName("test-resource-").
				Build(),
			wantIsNew:      true, // Should be treated as new because generateName prefix doesn't match
			wantResourceID: "",
			wantErr:        false,
		},
		"ErrorLookingUpResources": {
			setupResourceClient: func() *tu.MockResourceClient {
				return tu.NewMockResourceClient().
					WithResourceNotFound().
					WithGetResourcesByLabel(func(context.Context, schema.GroupVersionKind, string, metav1.LabelSelector) ([]*un.Unstructured, error) {
						return nil, errors.New("error looking up resources")
					}).
					Build()
			},
			defClient: tu.NewMockDefinitionClient().Build(),
			composite: parentXR,
			desired: tu.NewResource("example.org/v1", "ComposedResource", "").
				WithLabels(map[string]string{
					"crossplane.io/composite": "parent-xr",
				}).
				WithAnnotations(map[string]string{
					"crossplane.io/composition-resource-name": "resource-a",
				}).
				WithGenerateName("test-resource-").
				Build(),
			wantIsNew: true,  // Fall back to creating a new resource
			wantErr:   false, // We handle the error gracefully
		},
		"ClaimResource_FoundByClaimLabels": {
			setupResourceClient: func() *tu.MockResourceClient {
				// Create an existing resource with claim labels
				existingClaimResource := tu.NewResource("example.org/v1", "ComposedResource", "claim-managed-resource").
					WithSpecField("field", "value").
					WithLabels(map[string]string{
						"crossplane.io/claim-name":      "test-claim",
						"crossplane.io/claim-namespace": "test-namespace",
					}).
					WithAnnotations(map[string]string{
						"crossplane.io/composition-resource-name": "resource-a",
					}).
					Build()

				return tu.NewMockResourceClient().
					WithResourceNotFound(). // Direct lookup fails
					WithGetResourcesByLabel(func(_ context.Context, _ schema.GroupVersionKind, _ string, sel metav1.LabelSelector) ([]*un.Unstructured, error) {
						// Check if looking up by claim labels
						if claimName, exists := sel.MatchLabels["crossplane.io/claim-name"]; exists && claimName == "test-claim" {
							if claimNS, exists := sel.MatchLabels["crossplane.io/claim-namespace"]; exists && claimNS == "test-namespace" {
								return []*un.Unstructured{existingClaimResource}, nil
							}
						}
						return []*un.Unstructured{}, nil
					}).
					Build()
			},
			defClient: tu.NewMockDefinitionClient().
				WithIsClaimResource(func(_ context.Context, resource *un.Unstructured) bool {
					return resource.GetKind() == "TestClaim"
				}).
				Build(),
			composite: tu.NewResource("example.org/v1", "TestClaim", "test-claim").
				InNamespace("test-namespace").
				Build(),
			desired: tu.NewResource("example.org/v1", "ComposedResource", "claim-managed-resource").
				WithLabels(map[string]string{
					"crossplane.io/claim-name":      "test-claim",
					"crossplane.io/claim-namespace": "test-namespace",
				}).
				WithAnnotations(map[string]string{
					"crossplane.io/composition-resource-name": "resource-a",
				}).
				Build(),
			wantIsNew:      false,
			wantResourceID: "claim-managed-resource",
			wantErr:        false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Create the resource manager
			resourceClient := tt.setupResourceClient()

			rm := NewResourceManager(resourceClient, tt.defClient, tu.TestLogger(t, false))

			// Call the method under test
			current, isNew, err := rm.FetchCurrentObject(ctx, tt.composite, tt.desired)

			// Check error expectations
			if tt.wantErr {
				if err == nil {
					t.Errorf("FetchCurrentObject() expected error but got none")
				}

				return
			}

			if err != nil {
				t.Fatalf("FetchCurrentObject() unexpected error: %v", err)
			}

			// Check if isNew flag matches expectations
			if isNew != tt.wantIsNew {
				t.Errorf("FetchCurrentObject() isNew = %v, want %v", isNew, tt.wantIsNew)
			}

			// For new resources, current should be nil
			if isNew && current != nil {
				t.Errorf("FetchCurrentObject() returned non-nil current for new resource")
			}

			// For existing resources, check the resource ID
			if !isNew && tt.wantResourceID != "" {
				if current == nil {
					t.Fatalf("FetchCurrentObject() returned nil current for existing resource")
				}

				if current.GetName() != tt.wantResourceID {
					t.Errorf("FetchCurrentObject() current.GetName() = %v, want %v",
						current.GetName(), tt.wantResourceID)
				}
			}
		})
	}
}

func TestDefaultResourceManager_UpdateOwnerRefs(t *testing.T) {
	// Create test resources
	parentXR := tu.NewResource("example.org/v1", "XR", "parent-xr").Build()

	const ParentUID = "parent-uid"
	parentXR.SetUID(ParentUID)

	tests := map[string]struct {
		parent    *un.Unstructured
		child     *un.Unstructured
		defClient *tu.MockDefinitionClient
		validate  func(t *testing.T, child *un.Unstructured)
	}{
		"NilParent_NoChange": {
			parent: nil,
			child: tu.NewResource("example.org/v1", "Child", "child-resource").
				WithOwnerReference("some-api-version", "SomeKind", "some-name", "foobar").
				Build(),
			defClient: tu.NewMockDefinitionClient().Build(),
			validate: func(t *testing.T, child *un.Unstructured) {
				t.Helper()
				// Owner refs should be unchanged
				ownerRefs := child.GetOwnerReferences()
				if len(ownerRefs) != 1 {
					t.Fatalf("Expected 1 owner reference, got %d", len(ownerRefs))
				}
				// UID should be generated but not parent's UID
				if ownerRefs[0].UID == ParentUID {
					t.Errorf("UID should not be parent's UID when parent is nil")
				}
				if ownerRefs[0].UID == "" {
					t.Errorf("UID should not be empty")
				}
			},
		},
		"MatchingOwnerRef_UpdatedWithParentUID": {
			parent: parentXR,
			child: tu.NewResource("example.org/v1", "Child", "child-resource").
				WithOwnerReference("XR", "parent-xr", "example.org/v1", "").
				Build(),
			defClient: tu.NewMockDefinitionClient().Build(),
			validate: func(t *testing.T, child *un.Unstructured) {
				t.Helper()
				// Owner reference should be updated with parent's UID
				ownerRefs := child.GetOwnerReferences()
				if len(ownerRefs) != 1 {
					t.Fatalf("Expected 1 owner reference, got %d", len(ownerRefs))
				}
				if ownerRefs[0].UID != ParentUID {
					t.Errorf("UID = %s, want %s", ownerRefs[0].UID, ParentUID)
				}
			},
		},
		"NonMatchingOwnerRef_GenerateRandomUID": {
			parent: parentXR,
			child: tu.NewResource("example.org/v1", "Child", "child-resource").
				WithOwnerReference("other-api-version", "OtherKind", "other-name", "").
				Build(),
			defClient: tu.NewMockDefinitionClient().Build(),
			validate: func(t *testing.T, child *un.Unstructured) {
				t.Helper()
				// Owner reference should have a UID, but not parent's UID
				ownerRefs := child.GetOwnerReferences()
				if len(ownerRefs) != 1 {
					t.Fatalf("Expected 1 owner reference, got %d", len(ownerRefs))
				}
				if ownerRefs[0].UID == ParentUID {
					t.Errorf("UID should not be parent's UID for non-matching owner ref")
				}
				if ownerRefs[0].UID == "" {
					t.Errorf("UID should not be empty")
				}
			},
		},
		"MultipleOwnerRefs_OnlyUpdateMatching": {
			parent: parentXR,
			child: func() *un.Unstructured {
				child := tu.NewResource("example.org/v1", "Child", "child-resource").Build()

				// Add multiple owner references
				child.SetOwnerReferences([]metav1.OwnerReference{
					{
						APIVersion: "example.org/v1",
						Kind:       "XR",
						Name:       "parent-xr",
						UID:        "", // Empty UID should be updated
					},
					{
						APIVersion: "other.org/v1",
						Kind:       "OtherKind",
						Name:       "other-name",
						UID:        "", // Empty UID should be generated
					},
					{
						APIVersion: "example.org/v1",
						Kind:       "XR",
						Name:       "different-parent",
						UID:        "", // Empty UID should be generated
					},
				})

				return child
			}(),
			defClient: tu.NewMockDefinitionClient().Build(),
			validate: func(t *testing.T, child *un.Unstructured) {
				t.Helper()
				ownerRefs := child.GetOwnerReferences()
				if len(ownerRefs) != 3 {
					t.Fatalf("Expected 3 owner references, got %d", len(ownerRefs))
				}

				// Check each owner ref
				for _, ref := range ownerRefs {
					// All UIDs should be filled
					if ref.UID == "" {
						t.Errorf("UID should not be empty for any owner reference")
					}

					// Only the matching reference should have parent's UID
					if ref.APIVersion == "example.org/v1" && ref.Kind == "XR" && ref.Name == "parent-xr" {
						if ref.UID != ParentUID {
							t.Errorf("Matching owner ref has UID = %s, want %s", ref.UID, ParentUID)
						}
					} else {
						if ref.UID == ParentUID {
							t.Errorf("Non-matching owner ref should not have parent's UID")
						}
					}
				}
			},
		},
		"ClaimParent_SetsClaimLabels": {
			parent: tu.NewResource("example.org/v1", "TestClaim", "test-claim").
				InNamespace("test-namespace").
				Build(),
			child: tu.NewResource("example.org/v1", "Child", "child-resource").Build(),
			defClient: tu.NewMockDefinitionClient().
				WithIsClaimResource(func(_ context.Context, resource *un.Unstructured) bool {
					return resource.GetKind() == "TestClaim"
				}).
				Build(),
			validate: func(t *testing.T, child *un.Unstructured) {
				t.Helper()
				labels := child.GetLabels()
				if labels == nil {
					t.Fatal("Expected labels to be set")
				}

				// Check claim-specific labels
				if claimName, exists := labels["crossplane.io/claim-name"]; !exists || claimName != "test-claim" {
					t.Errorf("Expected crossplane.io/claim-name=test-claim, got %s", claimName)
				}
				if claimNS, exists := labels["crossplane.io/claim-namespace"]; !exists || claimNS != "test-namespace" {
					t.Errorf("Expected crossplane.io/claim-namespace=test-namespace, got %s", claimNS)
				}

				// Should not have composite label for claims
				if composite, exists := labels["crossplane.io/composite"]; exists {
					t.Errorf("Should not have crossplane.io/composite label for claim, but got %s", composite)
				}
			},
		},
		"XRParent_SetsCompositeLabel": {
			parent: tu.NewResource("example.org/v1", "XR", "test-xr").Build(),
			child:  tu.NewResource("example.org/v1", "Child", "child-resource").Build(),
			defClient: tu.NewMockDefinitionClient().
				WithIsClaimResource(func(_ context.Context, resource *un.Unstructured) bool {
					return resource.GetKind() == "TestClaim"
				}).
				Build(),
			validate: func(t *testing.T, child *un.Unstructured) {
				t.Helper()
				labels := child.GetLabels()
				if labels == nil {
					t.Fatal("Expected labels to be set")
				}

				// Check composite label
				if composite, exists := labels["crossplane.io/composite"]; !exists || composite != "test-xr" {
					t.Errorf("Expected crossplane.io/composite=test-xr, got %s", composite)
				}

				// Should not have claim-specific labels for XRs
				if claimName, exists := labels["crossplane.io/claim-name"]; exists {
					t.Errorf("Should not have crossplane.io/claim-name label for XR, but got %s", claimName)
				}
				if claimNS, exists := labels["crossplane.io/claim-namespace"]; exists {
					t.Errorf("Should not have crossplane.io/claim-namespace label for XR, but got %s", claimNS)
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Create the resource manager
			rm := NewResourceManager(tu.NewMockResourceClient().Build(), tt.defClient, tu.TestLogger(t, false))

			// Need to create a copy of the child to avoid modifying test data
			child := tt.child.DeepCopy()

			// Call the method under test
			rm.UpdateOwnerRefs(tt.parent, child)

			// Validate the results
			tt.validate(t, child)
		})
	}
}
