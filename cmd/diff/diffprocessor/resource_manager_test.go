package diffprocessor

import (
	"context"
	"sort"
	"strings"
	"testing"

	gcmp "github.com/google/go-cmp/cmp"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"

	"github.com/crossplane/crossplane/v2/cmd/crank/common/resource"
)

const (
	testClaimName = "test-claim"
	testClaimKind = "TestClaim"
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
						"crossplane.io/claim-name":      testClaimName,
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
						if claimName, exists := sel.MatchLabels["crossplane.io/claim-name"]; exists && claimName == testClaimName {
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
					return resource.GetKind() == testClaimKind
				}).
				Build(),
			composite: tu.NewResource("example.org/v1", testClaimKind, testClaimName).
				InNamespace("test-namespace").
				Build(),
			desired: tu.NewResource("example.org/v1", "ComposedResource", "claim-managed-resource").
				WithLabels(map[string]string{
					"crossplane.io/claim-name":      testClaimName,
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

			rm := NewResourceManager(resourceClient, tt.defClient, tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

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
	ctx := t.Context()
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
			parent: tu.NewResource("example.org/v1", testClaimKind, testClaimName).
				InNamespace("test-namespace").
				Build(),
			child: tu.NewResource("example.org/v1", "Child", "child-resource").Build(),
			defClient: tu.NewMockDefinitionClient().
				WithIsClaimResource(func(_ context.Context, resource *un.Unstructured) bool {
					return resource.GetKind() == testClaimKind
				}).
				Build(),
			validate: func(t *testing.T, child *un.Unstructured) {
				t.Helper()

				labels := child.GetLabels()
				if labels == nil {
					t.Fatal("Expected labels to be set")
				}

				// Check claim-specific labels
				if claimName, exists := labels["crossplane.io/claim-name"]; !exists || claimName != testClaimName {
					t.Errorf("Expected crossplane.io/claim-name=%s, got %s", testClaimName, claimName)
				}

				if claimNS, exists := labels["crossplane.io/claim-namespace"]; !exists || claimNS != "test-namespace" {
					t.Errorf("Expected crossplane.io/claim-namespace=test-namespace, got %s", claimNS)
				}

				// Should have composite label pointing to claim for new resources
				if composite, exists := labels["crossplane.io/composite"]; !exists || composite != testClaimName {
					t.Errorf("Expected crossplane.io/composite=%s, got %s", testClaimName, composite)
				}
			},
		},
		"ClaimParent_PreservesExistingCompositeLabel": {
			parent: tu.NewResource("example.org/v1", testClaimKind, testClaimName).
				InNamespace("test-namespace").
				Build(),
			child: tu.NewResource("example.org/v1", "Child", "child-resource").
				WithLabels(map[string]string{
					"crossplane.io/composite": "test-claim-82crv", // Existing composite label
				}).
				Build(),
			defClient: tu.NewMockDefinitionClient().
				WithIsClaimResource(func(_ context.Context, resource *un.Unstructured) bool {
					return resource.GetKind() == testClaimKind
				}).
				Build(),
			validate: func(t *testing.T, child *un.Unstructured) {
				t.Helper()

				labels := child.GetLabels()
				if labels == nil {
					t.Fatal("Expected labels to be set")
				}

				// Check claim-specific labels
				if claimName, exists := labels["crossplane.io/claim-name"]; !exists || claimName != testClaimName {
					t.Errorf("Expected crossplane.io/claim-name=%s, got %s", testClaimName, claimName)
				}

				if claimNS, exists := labels["crossplane.io/claim-namespace"]; !exists || claimNS != "test-namespace" {
					t.Errorf("Expected crossplane.io/claim-namespace=test-namespace, got %s", claimNS)
				}

				// Should preserve existing composite label pointing to generated XR
				if composite, exists := labels["crossplane.io/composite"]; !exists || composite != "test-claim-82crv" {
					t.Errorf("Expected crossplane.io/composite=test-claim-82crv (preserved), got %s", composite)
				}
			},
		},
		"XRParent_SetsCompositeLabel": {
			parent: tu.NewResource("example.org/v1", "XR", "test-xr").Build(),
			child:  tu.NewResource("example.org/v1", "Child", "child-resource").Build(),
			defClient: tu.NewMockDefinitionClient().
				WithIsClaimResource(func(_ context.Context, resource *un.Unstructured) bool {
					return resource.GetKind() == testClaimKind
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
		"ClaimParent_SkipsOwnerRefUpdate": {
			parent: func() *un.Unstructured {
				u := tu.NewResource("example.org/v1", testClaimKind, testClaimName).
					InNamespace("test-namespace").
					Build()
				u.SetUID("claim-uid")

				return u
			}(),
			child: func() *un.Unstructured {
				u := tu.NewResource("example.org/v1", "Child", "child-resource").Build()
				u.SetOwnerReferences([]metav1.OwnerReference{
					{
						APIVersion:         "example.org/v1",
						Kind:               "XTestResource",
						Name:               "test-claim-82crv",
						UID:                "",
						Controller:         ptr.To(true),
						BlockOwnerDeletion: ptr.To(true),
					},
					{
						APIVersion:         "example.org/v1",
						Kind:               testClaimKind,
						Name:               testClaimName,
						UID:                "",
						Controller:         ptr.To(true),
						BlockOwnerDeletion: ptr.To(true),
					},
				})

				return u
			}(),
			defClient: tu.NewMockDefinitionClient().
				WithIsClaimResource(func(_ context.Context, resource *un.Unstructured) bool {
					return resource.GetKind() == testClaimKind
				}).
				Build(),
			validate: func(t *testing.T, child *un.Unstructured) {
				t.Helper()

				// When parent is a Claim, owner references should not be modified
				refs := child.GetOwnerReferences()
				if len(refs) != 2 {
					t.Fatalf("Expected 2 owner references (unchanged), got %d", len(refs))
				}

				// Find the references
				var xrRef, claimRef *metav1.OwnerReference

				for i := range refs {
					switch refs[i].Kind {
					case "XTestResource":
						xrRef = &refs[i]
					case testClaimKind:
						claimRef = &refs[i]
					}
				}

				if xrRef == nil {
					t.Fatal("Expected to find XTestResource owner reference")
				}

				if claimRef == nil {
					t.Fatal("Expected to find TestClaim owner reference")
				}

				// All refs should have UIDs now
				if xrRef.UID == "" {
					t.Error("Expected XTestResource owner reference to have a UID")
				}

				if claimRef.UID == "" {
					t.Error("Expected TestClaim owner reference to have a UID")
				}

				// The XR should keep Controller: true
				if xrRef.Controller == nil || !*xrRef.Controller {
					t.Error("Expected XTestResource owner reference to have Controller: true")
				}

				// The Claim should have Controller: false
				if claimRef.Controller == nil || *claimRef.Controller {
					t.Error("Expected TestClaim owner reference to have Controller: false, but it has Controller: true")
				}

				// But labels should still be updated
				labels := child.GetLabels()
				if labels == nil {
					t.Fatal("Expected labels to be set")
				}

				// Check claim-specific labels
				if claimName, exists := labels["crossplane.io/claim-name"]; !exists || claimName != testClaimName {
					t.Errorf("Expected crossplane.io/claim-name=%s, got %s", testClaimName, claimName)
				}

				if claimNS, exists := labels["crossplane.io/claim-namespace"]; !exists || claimNS != "test-namespace" {
					t.Errorf("Expected crossplane.io/claim-namespace=test-namespace, got %s", claimNS)
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Create the resource manager
			rm := NewResourceManager(tu.NewMockResourceClient().Build(), tt.defClient, tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

			// Need to create a copy of the child to avoid modifying test data
			child := tt.child.DeepCopy()

			// Call the method under test
			rm.UpdateOwnerRefs(ctx, tt.parent, child)

			// Validate the results
			tt.validate(t, child)
		})
	}
}

func TestDefaultResourceManager_getCompositionResourceName(t *testing.T) {
	rm := &DefaultResourceManager{
		logger: tu.TestLogger(t, false),
	}

	tests := map[string]struct {
		annotations map[string]string
		want        string
	}{
		"StandardAnnotation": {
			annotations: map[string]string{
				"crossplane.io/composition-resource-name": "my-resource",
			},
			want: "my-resource",
		},
		"FunctionSpecificAnnotation": {
			annotations: map[string]string{
				"function.crossplane.io/composition-resource-name": "function-resource",
			},
			want: "function-resource",
		},
		"BothAnnotations_StandardTakesPrecedence": {
			annotations: map[string]string{
				"crossplane.io/composition-resource-name":          "standard-resource",
				"function.crossplane.io/composition-resource-name": "function-resource",
			},
			want: "standard-resource",
		},
		"NoAnnotations": {
			annotations: map[string]string{
				"some-other-annotation": "value",
			},
			want: "",
		},
		"EmptyAnnotations": {
			annotations: map[string]string{},
			want:        "",
		},
		"NilAnnotations": {
			annotations: nil,
			want:        "",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := rm.getCompositionResourceName(tt.annotations)
			if got != tt.want {
				t.Errorf("getCompositionResourceName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultResourceManager_hasMatchingResourceName(t *testing.T) {
	rm := &DefaultResourceManager{
		logger: tu.TestLogger(t, false),
	}

	tests := map[string]struct {
		annotations      map[string]string
		compResourceName string
		want             bool
	}{
		"StandardAnnotationMatches": {
			annotations: map[string]string{
				"crossplane.io/composition-resource-name": "my-resource",
			},
			compResourceName: "my-resource",
			want:             true,
		},
		"StandardAnnotationDoesNotMatch": {
			annotations: map[string]string{
				"crossplane.io/composition-resource-name": "my-resource",
			},
			compResourceName: "different-resource",
			want:             false,
		},
		"FunctionSpecificAnnotationMatches": {
			annotations: map[string]string{
				"function.crossplane.io/composition-resource-name": "function-resource",
			},
			compResourceName: "function-resource",
			want:             true,
		},
		"FunctionSpecificAnnotationDoesNotMatch": {
			annotations: map[string]string{
				"function.crossplane.io/composition-resource-name": "function-resource",
			},
			compResourceName: "different-resource",
			want:             false,
		},
		"NoAnnotations": {
			annotations: map[string]string{
				"some-other-annotation": "value",
			},
			compResourceName: "my-resource",
			want:             false,
		},
		"EmptyAnnotations": {
			annotations:      map[string]string{},
			compResourceName: "my-resource",
			want:             false,
		},
		"NilAnnotations": {
			annotations:      nil,
			compResourceName: "my-resource",
			want:             false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := rm.hasMatchingResourceName(tt.annotations, tt.compResourceName)
			if got != tt.want {
				t.Errorf("hasMatchingResourceName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultResourceManager_createResourceID(t *testing.T) {
	rm := &DefaultResourceManager{
		logger: tu.TestLogger(t, false),
	}

	gvk := schema.GroupVersionKind{
		Group:   "example.org",
		Version: "v1",
		Kind:    "TestResource",
	}

	tests := map[string]struct {
		gvk          schema.GroupVersionKind
		namespace    string
		name         string
		generateName string
		want         string
	}{
		"NamedResourceWithNamespace": {
			gvk:       gvk,
			namespace: "default",
			name:      "my-resource",
			want:      "example.org/v1, Kind=TestResource/default/my-resource",
		},
		"NamedResourceWithoutNamespace": {
			gvk:  gvk,
			name: "my-resource",
			want: "example.org/v1, Kind=TestResource/my-resource",
		},
		"GenerateNameWithNamespace": {
			gvk:          gvk,
			namespace:    "default",
			generateName: "my-resource-",
			want:         "example.org/v1, Kind=TestResource/default/my-resource-*",
		},
		"GenerateNameWithoutNamespace": {
			gvk:          gvk,
			generateName: "my-resource-",
			want:         "example.org/v1, Kind=TestResource/my-resource-*",
		},
		"NoNameOrGenerateName": {
			gvk:  gvk,
			want: "example.org/v1, Kind=TestResource/<no-name>",
		},
		"BothNameAndGenerateName_NameTakesPrecedence": {
			gvk:          gvk,
			namespace:    "default",
			name:         "my-resource",
			generateName: "my-resource-",
			want:         "example.org/v1, Kind=TestResource/default/my-resource",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := rm.createResourceID(tt.gvk, tt.namespace, tt.name, tt.generateName)
			if got != tt.want {
				t.Errorf("createResourceID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultResourceManager_checkCompositeOwnership(t *testing.T) {
	tests := map[string]struct {
		current   *un.Unstructured
		composite *un.Unstructured
		wantLog   bool
	}{
		"NilComposite": {
			current: tu.NewResource("example.org/v1", "Resource", "my-resource").
				WithLabels(map[string]string{
					"crossplane.io/composite": "other-xr",
				}).
				Build(),
			composite: nil,
			wantLog:   false,
		},
		"NoCompositeLabel": {
			current: tu.NewResource("example.org/v1", "Resource", "my-resource").
				Build(),
			composite: tu.NewResource("example.org/v1", "XR", "my-xr").
				Build(),
			wantLog: false,
		},
		"MatchingCompositeLabel": {
			current: tu.NewResource("example.org/v1", "Resource", "my-resource").
				WithLabels(map[string]string{
					"crossplane.io/composite": "my-xr",
				}).
				Build(),
			composite: tu.NewResource("example.org/v1", "XR", "my-xr").
				Build(),
			wantLog: false,
		},
		"DifferentCompositeLabel": {
			current: tu.NewResource("example.org/v1", "Resource", "my-resource").
				WithLabels(map[string]string{
					"crossplane.io/composite": "other-xr",
				}).
				Build(),
			composite: tu.NewResource("example.org/v1", "XR", "my-xr").
				Build(),
			wantLog: true,
		},
		"NilLabels": {
			current: tu.NewResource("example.org/v1", "Resource", "my-resource").
				Build(),
			composite: tu.NewResource("example.org/v1", "XR", "my-xr").
				Build(),
			wantLog: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Create a test logger that captures log output
			var logCaptured bool

			logger := tu.TestLogger(t, false)

			// We can't easily intercept the logger, but we can test the function runs without error
			rm := &DefaultResourceManager{
				logger: logger,
			}

			// This should not panic or error
			rm.checkCompositeOwnership(tt.current, tt.composite)

			// The actual log checking would require a more sophisticated test logger
			// For now, we're just ensuring the function handles all cases correctly
			_ = logCaptured // Suppress unused variable warning
		})
	}
}

func TestDefaultResourceManager_updateCompositeOwnerLabel_EdgeCases(t *testing.T) {
	ctx := t.Context()

	tests := map[string]struct {
		parent    *un.Unstructured
		child     *un.Unstructured
		defClient *tu.MockDefinitionClient
		validate  func(t *testing.T, child *un.Unstructured)
	}{
		"ParentWithOnlyGenerateName": {
			parent: tu.NewResource("example.org/v1", "XR", "").
				WithGenerateName("my-xr-").
				Build(),
			child: tu.NewResource("example.org/v1", "Child", "child-resource").
				Build(),
			defClient: tu.NewMockDefinitionClient().
				WithIsClaimResource(func(_ context.Context, _ *un.Unstructured) bool {
					return false
				}).
				Build(),
			validate: func(t *testing.T, child *un.Unstructured) {
				t.Helper()

				labels := child.GetLabels()
				if labels == nil {
					t.Fatal("Expected labels to be set")
				}

				if composite, exists := labels["crossplane.io/composite"]; !exists || composite != "my-xr-" {
					t.Errorf("Expected crossplane.io/composite=my-xr-, got %s", composite)
				}
			},
		},
		"ParentWithNoNameOrGenerateName": {
			parent: tu.NewResource("example.org/v1", "XR", "").
				Build(),
			child: tu.NewResource("example.org/v1", "Child", "child-resource").
				Build(),
			defClient: tu.NewMockDefinitionClient().
				WithIsClaimResource(func(_ context.Context, _ *un.Unstructured) bool {
					return false
				}).
				Build(),
			validate: func(t *testing.T, child *un.Unstructured) {
				t.Helper()

				labels := child.GetLabels()
				// Should not set any composite label
				if labels != nil {
					if _, exists := labels["crossplane.io/composite"]; exists {
						t.Error("Should not set composite label when parent has no name")
					}
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rm := &DefaultResourceManager{
				defClient: tt.defClient,
				logger:    tu.TestLogger(t, false),
			}

			child := tt.child.DeepCopy()
			rm.updateCompositeOwnerLabel(ctx, tt.parent, child)
			tt.validate(t, child)
		})
	}
}

func TestDefaultResourceManager_FetchObservedResources(t *testing.T) {
	ctx := t.Context()

	// Create test XR
	testXR := tu.NewResource("example.org/v1", "XR", "test-xr").
		WithSpecField("field", "value").
		Build()

	// Create composed resources with composition-resource-name annotation
	composedResource1 := tu.NewResource("example.org/v1", "ManagedResource", "resource-1").
		WithSpecField("field", "value1").
		WithAnnotations(map[string]string{
			"crossplane.io/composition-resource-name": "db-instance",
		}).
		Build()

	composedResource2 := tu.NewResource("example.org/v1", "ManagedResource", "resource-2").
		WithSpecField("field", "value2").
		WithAnnotations(map[string]string{
			"crossplane.io/composition-resource-name": "db-subnet",
		}).
		Build()

	// Create a nested XR (child XR with composition-resource-name annotation)
	nestedXR := tu.NewResource("example.org/v1", "ChildXR", "nested-xr").
		WithSpecField("nested", "value").
		WithAnnotations(map[string]string{
			"crossplane.io/composition-resource-name": "child-xr",
		}).
		Build()

	// Create composed resources under the nested XR
	nestedComposedResource := tu.NewResource("example.org/v1", "NestedResource", "nested-resource-1").
		WithSpecField("field", "nested-value").
		WithAnnotations(map[string]string{
			"crossplane.io/composition-resource-name": "nested-db",
		}).
		Build()

	// Create a resource without the composition-resource-name annotation (should be filtered out)
	resourceWithoutAnnotation := tu.NewResource("example.org/v1", "OtherResource", "other-resource").
		WithSpecField("field", "other").
		Build()

	tests := map[string]struct {
		setupTreeClient func() *tu.MockResourceTreeClient
		xr              *un.Unstructured
		wantCount       int
		wantResourceIDs []string // Names of resources we expect to find
		wantErr         bool
	}{
		"SuccessfullyFetchesFlatComposedResources": {
			setupTreeClient: func() *tu.MockResourceTreeClient {
				return tu.NewMockResourceTreeClient().
					WithResourceTreeFromXRAndComposed(testXR, []*un.Unstructured{
						composedResource1,
						composedResource2,
					}).
					Build()
			},
			xr:              testXR,
			wantCount:       2,
			wantResourceIDs: []string{"resource-1", "resource-2"},
			wantErr:         false,
		},
		"SuccessfullyFetchesNestedResources": {
			setupTreeClient: func() *tu.MockResourceTreeClient {
				// Build a tree with nested structure:
				// XR
				//   -> composedResource1
				//   -> nestedXR
				//        -> nestedComposedResource
				return tu.NewMockResourceTreeClient().
					WithGetResourceTree(func(_ context.Context, _ *un.Unstructured) (*resource.Resource, error) {
						return &resource.Resource{
							Unstructured: *testXR.DeepCopy(),
							Children: []*resource.Resource{
								{
									Unstructured: *composedResource1.DeepCopy(),
									Children:     []*resource.Resource{},
								},
								{
									Unstructured: *nestedXR.DeepCopy(),
									Children: []*resource.Resource{
										{
											Unstructured: *nestedComposedResource.DeepCopy(),
											Children:     []*resource.Resource{},
										},
									},
								},
							},
						}, nil
					}).
					Build()
			},
			xr:              testXR,
			wantCount:       3, // composedResource1, nestedXR, nestedComposedResource
			wantResourceIDs: []string{"resource-1", "nested-xr", "nested-resource-1"},
			wantErr:         false,
		},
		"FiltersOutResourcesWithoutAnnotation": {
			setupTreeClient: func() *tu.MockResourceTreeClient {
				return tu.NewMockResourceTreeClient().
					WithGetResourceTree(func(_ context.Context, _ *un.Unstructured) (*resource.Resource, error) {
						return &resource.Resource{
							Unstructured: *testXR.DeepCopy(),
							Children: []*resource.Resource{
								{
									Unstructured: *composedResource1.DeepCopy(),
									Children:     []*resource.Resource{},
								},
								{
									// This resource lacks the annotation and should be filtered out
									Unstructured: *resourceWithoutAnnotation.DeepCopy(),
									Children:     []*resource.Resource{},
								},
								{
									Unstructured: *composedResource2.DeepCopy(),
									Children:     []*resource.Resource{},
								},
							},
						}, nil
					}).
					Build()
			},
			xr:              testXR,
			wantCount:       2, // Only composedResource1 and composedResource2
			wantResourceIDs: []string{"resource-1", "resource-2"},
			wantErr:         false,
		},
		"ReturnsEmptySliceWhenNoComposedResources": {
			setupTreeClient: func() *tu.MockResourceTreeClient {
				return tu.NewMockResourceTreeClient().
					WithEmptyResourceTree().
					Build()
			},
			xr:              testXR,
			wantCount:       0,
			wantResourceIDs: []string{},
			wantErr:         false,
		},
		"ReturnsEmptySliceWhenOnlyRootXRInTree": {
			setupTreeClient: func() *tu.MockResourceTreeClient {
				return tu.NewMockResourceTreeClient().
					WithGetResourceTree(func(_ context.Context, _ *un.Unstructured) (*resource.Resource, error) {
						// Tree with only root (XR itself has no composition-resource-name)
						return &resource.Resource{
							Unstructured: *testXR.DeepCopy(),
							Children:     []*resource.Resource{},
						}, nil
					}).
					Build()
			},
			xr:              testXR,
			wantCount:       0,
			wantResourceIDs: []string{},
			wantErr:         false,
		},
		"ReturnsErrorWhenTreeClientFails": {
			setupTreeClient: func() *tu.MockResourceTreeClient {
				return tu.NewMockResourceTreeClient().
					WithFailedResourceTreeFetch("failed to get tree").
					Build()
			},
			xr:      testXR,
			wantErr: true,
		},
		"HandlesDeepNestedStructure": {
			setupTreeClient: func() *tu.MockResourceTreeClient {
				// Build a deeply nested tree:
				// XR
				//   -> composedResource1
				//        -> nestedXR
				//             -> nestedComposedResource
				//                  -> composedResource2
				return tu.NewMockResourceTreeClient().
					WithGetResourceTree(func(_ context.Context, _ *un.Unstructured) (*resource.Resource, error) {
						return &resource.Resource{
							Unstructured: *testXR.DeepCopy(),
							Children: []*resource.Resource{
								{
									Unstructured: *composedResource1.DeepCopy(),
									Children: []*resource.Resource{
										{
											Unstructured: *nestedXR.DeepCopy(),
											Children: []*resource.Resource{
												{
													Unstructured: *nestedComposedResource.DeepCopy(),
													Children: []*resource.Resource{
														{
															Unstructured: *composedResource2.DeepCopy(),
															Children:     []*resource.Resource{},
														},
													},
												},
											},
										},
									},
								},
							},
						}, nil
					}).
					Build()
			},
			xr:              testXR,
			wantCount:       4, // All 4 resources have the annotation
			wantResourceIDs: []string{"resource-1", "nested-xr", "nested-resource-1", "resource-2"},
			wantErr:         false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Use the constructor and interface type to test via the public API
			rm := NewResourceManager(
				nil, // client not used by FetchObservedResources
				nil, // defClient not used by FetchObservedResources
				tt.setupTreeClient(),
				tu.TestLogger(t, false),
			)

			observed, err := rm.FetchObservedResources(ctx, &cmp.Unstructured{Unstructured: *tt.xr})

			if tt.wantErr {
				if err == nil {
					t.Errorf("FetchObservedResources() expected error, got nil")
				}

				return
			}

			if err != nil {
				t.Errorf("FetchObservedResources() unexpected error: %v", err)
				return
			}

			if len(observed) != tt.wantCount {
				t.Errorf("FetchObservedResources() got %d resources, want %d", len(observed), tt.wantCount)
			}

			// Verify we got the expected resources
			if len(tt.wantResourceIDs) > 0 {
				// Extract resource IDs from observed resources
				var gotResourceIDs []string
				for _, res := range observed {
					gotResourceIDs = append(gotResourceIDs, res.GetName())
				}
				sort.Strings(gotResourceIDs)

				wantResourceIDs := append([]string{}, tt.wantResourceIDs...)
				sort.Strings(wantResourceIDs)

				if diff := gcmp.Diff(wantResourceIDs, gotResourceIDs); diff != "" {
					t.Errorf("FetchObservedResources() resource IDs mismatch (-want +got):\n%s", diff)
				}

				// Verify all resources have the composition-resource-name annotation
				for _, res := range observed {
					if _, hasAnno := res.GetAnnotations()["crossplane.io/composition-resource-name"]; !hasAnno {
						t.Errorf("FetchObservedResources() returned resource %s without composition-resource-name annotation", res.GetName())
					}
				}
			}
		})
	}
}
