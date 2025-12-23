package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	gyaml "gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	xpextv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
)

// CrossplaneFieldManager is the field manager used for SSA when setting up Crossplane-managed resources.
// This simulates how Crossplane applies composed resources in production.
const CrossplaneFieldManager = "apiextensions.crossplane.io/composed/integration-test"

// Testing data for integration tests

// createTestCompositionWithExtraResources creates a test Composition with a function-extra-resources step.
func createTestCompositionWithExtraResources() (*xpextv1.Composition, error) {
	pipelineMode := xpextv1.CompositionModePipeline

	// Create the extra resources function input
	extraResourcesInput := map[string]any{
		"apiVersion": "function.crossplane.io/v1beta1",
		"kind":       "ExtraResources",
		"spec": map[string]any{
			"extraResources": []any{
				map[string]any{
					"apiVersion": "example.org/v1",
					"kind":       "ExtraResource",
					"selector": map[string]any{
						"matchLabels": map[string]any{
							"app": "test-app",
						},
					},
				},
			},
		},
	}

	extraResourcesRaw, err := json.Marshal(extraResourcesInput)
	if err != nil {
		return nil, err
	}

	// Create template function input to create composed resources
	templateInput := map[string]any{
		"apiVersion": "apiextensions.crossplane.io/v1",
		"kind":       "Composition",
		"spec": map[string]any{
			"resources": []any{
				map[string]any{
					"name": "composed-resource",
					"base": map[string]any{
						"apiVersion": "example.org/v1",
						"kind":       "ComposedResource",
						"metadata": map[string]any{
							"name": "test-composed-resource",
							"labels": map[string]any{
								"app": "crossplane",
							},
						},
						"spec": map[string]any{
							"coolParam": "{{ .observed.composite.spec.coolParam }}",
							"replicas":  "{{ .observed.composite.spec.replicas }}",
							"extraData": "{{ index .observed.resources \"extra-resource-0\" \"spec\" \"data\" }}",
						},
					},
				},
			},
		},
	}

	templateRaw, err := json.Marshal(templateInput)
	if err != nil {
		return nil, err
	}

	return &xpextv1.Composition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-composition",
		},
		Spec: xpextv1.CompositionSpec{
			CompositeTypeRef: xpextv1.TypeReference{
				APIVersion: "example.org/v1",
				Kind:       "XExampleResource",
			},
			Mode: pipelineMode,
			Pipeline: []xpextv1.PipelineStep{
				{
					Step:        "extra-resources",
					FunctionRef: xpextv1.FunctionReference{Name: "function-extra-resources"},
					Input:       &runtime.RawExtension{Raw: extraResourcesRaw},
				},
				{
					Step:        "templating",
					FunctionRef: xpextv1.FunctionReference{Name: "function-patch-and-transform"},
					Input:       &runtime.RawExtension{Raw: templateRaw},
				},
			},
		},
	}, nil
}

// createTestXRD creates a test XRD for the XR.
func createTestXRD() *xpextv1.CompositeResourceDefinition {
	return tu.NewXRD("xexampleresources.example.org", "example.org", "XExampleResource").
		WithPlural("xexampleresources").
		WithSingular("xexampleresource").
		WithRawSchema([]byte(`{
			"type": "object",
			"properties": {
				"spec": {
					"type": "object",
					"properties": {
						"coolParam": {
							"type": "string"
						},
						"replicas": {
							"type": "integer"
						}
					}
				},
				"status": {
					"type": "object",
					"properties": {
						"coolStatus": {
							"type": "string"
						}
					}
				}
			}
		}`)).
		Build()
}

// createExtraResource creates a test extra resource.
func createExtraResource() *un.Unstructured {
	return &un.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.org/v1",
			"kind":       "ExtraResource",
			"metadata": map[string]any{
				"name": "test-extra-resource",
				"labels": map[string]any{
					"app": "test-app",
				},
			},
			"spec": map[string]any{
				"data": "extra-resource-data",
			},
		},
	}
}

// createExistingComposedResource creates an existing composed resource with different values.
func createExistingComposedResource() *un.Unstructured {
	return &un.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.org/v1",
			"kind":       "ComposedResource",
			"metadata": map[string]any{
				"name": "test-xr-composed-resource",
				"labels": map[string]any{
					"app":                     "crossplane",
					"crossplane.io/composite": "test-xr",
				},
				"annotations": map[string]any{
					"crossplane.io/composition-resource-name": "composed-resource",
				},
				"ownerReferences": []any{
					map[string]any{
						"apiVersion":         "example.org/v1",
						"kind":               "XExampleResource",
						"name":               "test-xr",
						"controller":         true,
						"blockOwnerDeletion": true,
					},
				},
			},
			"spec": map[string]any{
				"coolParam": "old-value", // Different from what will be rendered
				"replicas":  2,           // Different from what will be rendered
				"extraData": "old-data",  // Different from what will be rendered
			},
		},
	}
}

// createMatchingComposedResource creates a composed resource that matches what would be rendered.
func createMatchingComposedResource() *un.Unstructured {
	return &un.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.org/v1",
			"kind":       "ComposedResource",
			"metadata": map[string]any{
				"name": "test-xr-composed-resource",
				"labels": map[string]any{
					"app":                     "crossplane",
					"crossplane.io/composite": "test-xr",
				},
				"annotations": map[string]any{
					"crossplane.io/composition-resource-name": "composed-resource",
				},
				"ownerReferences": []any{
					map[string]any{
						"apiVersion":         "example.org/v1",
						"kind":               "XExampleResource",
						"name":               "test-xr",
						"controller":         true,
						"blockOwnerDeletion": true,
					},
				},
			},
			"spec": map[string]any{
				"coolParam": "test-value",          // Matches what would be rendered
				"replicas":  3,                     // Matches what would be rendered
				"extraData": "extra-resource-data", // Matches what would be rendered
			},
		},
	}
}

// HierarchicalOwnershipRelation represents an ownership tree structure.
type HierarchicalOwnershipRelation struct {
	OwnerFile  string                                    // The file containing the owner resource
	OwnedFiles map[string]*HierarchicalOwnershipRelation // Map of owned file paths to their own relationships
}

// setOwnerReference adds an owner reference to the resource.
func setOwnerReference(resource, owner *un.Unstructured) {
	// Create owner reference
	ownerRef := metav1.OwnerReference{
		APIVersion:         owner.GetAPIVersion(),
		Kind:               owner.GetKind(),
		Name:               owner.GetName(),
		UID:                owner.GetUID(),
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}

	// Set the owner reference
	resource.SetOwnerReferences([]metav1.OwnerReference{ownerRef})
}

// addResourceRef adds a reference to the child resource in the parent's resourceRefs array.
func addResourceRef(parent, child *un.Unstructured, xrAPIVersion XrdAPIVersion) error {
	// Create the resource reference
	ref := map[string]any{
		"apiVersion": child.GetAPIVersion(),
		"kind":       child.GetKind(),
		"name":       child.GetName(),
	}

	// If the child has a namespace, include it
	if ns := child.GetNamespace(); ns != "" {
		ref["namespace"] = ns
	}

	var resourceRefsPath []string

	switch xrAPIVersion {
	case V1:
		resourceRefsPath = []string{"spec", "resourceRefs"}
	case V2:
		resourceRefsPath = []string{"spec", "crossplane", "resourceRefs"}
	}

	// Get current resourceRefs or initialize if not present
	resourceRefs, found, err := un.NestedSlice(parent.Object, resourceRefsPath...)
	if err != nil {
		return errors.Wrap(err, "cannot get resourceRefs from parent")
	}

	if !found || resourceRefs == nil {
		resourceRefs = []any{}
	}

	// Add the new reference and update the parent
	resourceRefs = append(resourceRefs, ref)

	return un.SetNestedSlice(parent.Object, resourceRefs, resourceRefsPath...)
}

// applyResourcesFromFiles loads and applies resources from YAML files
// Under the assumption that no resource should already exist.
func applyResourcesFromFiles(ctx context.Context, c client.Client, paths []string) error {
	// Collect all resources from all files first
	var allResources []*un.Unstructured

	for _, path := range paths {
		resources, err := readResourcesFromFile(path)
		if err != nil {
			return fmt.Errorf("failed to read resources from %s: %w", path, err)
		}

		allResources = append(allResources, resources...)
	}

	// Apply all resources as new resources
	return createResources(ctx, c, allResources)
}

// readResourcesFromFile reads YAML resources from a file.
func readResourcesFromFile(path string) ([]*un.Unstructured, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", path, err)
	}

	// Use a YAML decoder to handle multiple documents
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	var resources []*un.Unstructured

	for {
		resource := &un.Unstructured{}

		err := decoder.Decode(resource)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, fmt.Errorf("failed to decode YAML document from %s: %w", path, err)
		}

		// Skip empty documents
		if len(resource.Object) == 0 {
			continue
		}

		resources = append(resources, resource)
	}

	return resources, nil
}

// createResources creates all resources in the cluster
// Assumes resources don't already exist - fails if they do.
func createResources(ctx context.Context, c client.Client, resources []*un.Unstructured) error {
	for _, resource := range resources {
		err := c.Create(ctx, resource.DeepCopy())
		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("resource %s/%s of kind %s already exists - test setup error",
					resource.GetNamespace(), resource.GetName(), resource.GetKind())
			}

			return fmt.Errorf("failed to create resource %s/%s: %w",
				resource.GetNamespace(), resource.GetName(), err)
		}
	}

	return nil
}

// applyResourceWithSSA applies a resource using Server-Side Apply with the specified field manager.
// This simulates how Crossplane applies composed resources, allowing us to test field ownership behavior.
func applyResourceWithSSA(ctx context.Context, c client.Client, resource *un.Unstructured, fieldManager string) error {
	// Use Patch with Apply to perform Server-Side Apply
	resourceCopy := resource.DeepCopy()

	// For SSA, we need to set the resource version to empty to indicate this is an apply
	resourceCopy.SetResourceVersion("")

	err := c.Patch(ctx, resourceCopy, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership)
	if err != nil {
		return fmt.Errorf("failed to apply resource %s/%s with field manager %s: %w",
			resource.GetNamespace(), resource.GetName(), fieldManager, err)
	}

	return nil
}

// applyHierarchicalOwnership applies a hierarchical ownership structure.
func applyHierarchicalOwnership(ctx context.Context, _ logging.Logger, c client.Client, xrdAPIVersion XrdAPIVersion, hierarchies []HierarchicalOwnershipRelation) error {
	// Map to store created resources by file path
	createdResources := make(map[string]*un.Unstructured)
	// Map to track parent-child relationships for establishing resourceRefs
	parentChildRelationships := make(map[string]string) // child file -> parent file

	// First pass: Create all resources and collect parent-child relationships
	err := createAllResourcesInHierarchy(ctx, c, hierarchies, createdResources, parentChildRelationships)
	if err != nil {
		return err
	}

	// Second pass: Apply all owner references and resource refs between parents and children
	err = applyAllRelationships(ctx, c, createdResources, parentChildRelationships, xrdAPIVersion)
	if err != nil {
		return err
	}

	// Debug logging disabled by default. Uncomment to debug hierarchical setup issues.
	// if err := LogResourcesAsYAML(ctx, log, c, createdResources); err != nil {
	// 	log.Info(fmt.Sprintf("Warning: Failed to log resources as YAML: %v\n", err))
	// }

	return nil
}

// Unused but useful for debugging; leave it here.
// LogResourcesAsYAML fetches the latest version of each resource and logs it as YAML.
func LogResourcesAsYAML(ctx context.Context, log logging.Logger, c client.Client, createdResources map[string]*un.Unstructured) error {
	log.Info("\n===== FINAL STATE OF CREATED RESOURCES =====\n\n")

	// Sort the file paths for consistent output order
	filePaths := make([]string, 0, len(createdResources))
	for filePath := range createdResources {
		filePaths = append(filePaths, filePath)
	}

	sort.Strings(filePaths)

	for _, filePath := range filePaths {
		resource := createdResources[filePath]

		// Fetch the latest version of the resource
		latest := &un.Unstructured{}
		latest.SetGroupVersionKind(resource.GroupVersionKind())

		if err := c.Get(ctx, client.ObjectKey{
			Name:      resource.GetName(),
			Namespace: resource.GetNamespace(),
		}, latest); err != nil {
			return fmt.Errorf("failed to get latest version of resource %s/%s: %w",
				resource.GetNamespace(), resource.GetName(), err)
		}

		// Convert to YAML
		yamlData, err := gyaml.Marshal(latest.Object)
		if err != nil {
			return fmt.Errorf("failed to marshal resource to YAML: %w", err)
		}

		// Print the resource file path and its YAML representation
		log.Info(fmt.Sprintf("--- Source: %s\nResourceName: %s/%s\n%s\n\n",
			filePath, latest.GetKind(), latest.GetName(), string(yamlData)))
	}

	log.Info("===== END OF RESOURCES =====\n\n")

	return nil
}

// createAllResourcesInHierarchy creates all resources in a hierarchy and tracks relationships.
func createAllResourcesInHierarchy(ctx context.Context, c client.Client,
	hierarchies []HierarchicalOwnershipRelation,
	createdResources map[string]*un.Unstructured,
	parentChildRelationships map[string]string,
) error {
	for _, hierarchy := range hierarchies {
		// Create the owner resource first
		_, err := createResourceFromFile(ctx, c, hierarchy.OwnerFile, createdResources)
		if err != nil {
			return err
		}

		// Create all owned resources without setting references yet
		for ownedFile, childHierarchy := range hierarchy.OwnedFiles {
			// Track the parent-child relationship
			parentChildRelationships[ownedFile] = hierarchy.OwnerFile

			// Create the owned resource without setting references
			_, err := createResourceFromFile(ctx, c, ownedFile, createdResources)
			if err != nil {
				return err
			}

			// Process nested hierarchies recursively
			if childHierarchy != nil && len(childHierarchy.OwnedFiles) > 0 {
				childHierarchies := []HierarchicalOwnershipRelation{
					{
						OwnerFile:  ownedFile,
						OwnedFiles: childHierarchy.OwnedFiles,
					},
				}

				err := createAllResourcesInHierarchy(ctx, c, childHierarchies,
					createdResources, parentChildRelationships)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// createResourceFromFile creates a resource from a file using Server-Side Apply (SSA) with the
// CrossplaneFieldManager. This more closely simulates how Crossplane applies composed resources
// in production, enabling tests to accurately detect SSA-related behaviors like field removal.
func createResourceFromFile(ctx context.Context, c client.Client, path string,
	createdResources map[string]*un.Unstructured,
) (*un.Unstructured, error) {
	// Check if we've already processed this resource
	if resource, exists := createdResources[path]; exists {
		return resource, nil
	}

	// Read the resource from file
	resources, err := readResourcesFromFile(path)
	if err != nil {
		return nil, err
	}

	if len(resources) == 0 {
		return nil, fmt.Errorf("no resources found in file %s", path)
	}

	resource := resources[0]

	// Apply the resource using SSA with Crossplane field manager.
	// This simulates how Crossplane applies composed resources in production.
	if err := applyResourceWithSSA(ctx, c, resource, CrossplaneFieldManager); err != nil {
		return nil, fmt.Errorf("failed to apply resource with SSA: %w", err)
	}

	// Get the resource back from the server
	serverResource := &un.Unstructured{}
	serverResource.SetGroupVersionKind(resource.GroupVersionKind())

	if err := c.Get(ctx, client.ObjectKey{
		Name:      resource.GetName(),
		Namespace: resource.GetNamespace(),
	}, serverResource); err != nil {
		return nil, fmt.Errorf("failed to get created resource: %w", err)
	}

	// Store and return the created resource
	createdResources[path] = serverResource

	return serverResource, nil
}

// applyAllRelationships applies all owner references and resource refs.
func applyAllRelationships(ctx context.Context, c client.Client,
	createdResources map[string]*un.Unstructured,
	parentChildRelationships map[string]string,
	xrdAPIVersion XrdAPIVersion,
) error {
	// Process all parent-child relationships
	for childFile, parentFile := range parentChildRelationships {
		childResource := createdResources[childFile]
		parentResource := createdResources[parentFile]

		if childResource == nil || parentResource == nil {
			return fmt.Errorf("missing resource in relationship: parent=%s, child=%s",
				parentFile, childFile)
		}

		// 1. Set the owner reference in the child's metadata
		err := setOwnerReferenceAndUpdate(ctx, c, parentResource, childResource)
		if err != nil {
			return err
		}

		// 2. Add the child resource reference to the parent
		err = addResourceRefAndUpdate(ctx, c, parentResource, childResource, xrdAPIVersion)
		if err != nil {
			return err
		}
	}

	return nil
}

// setOwnerReferenceAndUpdate sets the owner reference in the child and updates it.
// Uses a separate field manager for owner references to avoid conflicts with SSA-created resources.
func setOwnerReferenceAndUpdate(ctx context.Context, c client.Client,
	owner *un.Unstructured, child *un.Unstructured,
) error {
	// Get the latest version of the child
	latestChild := &un.Unstructured{}
	latestChild.SetGroupVersionKind(child.GroupVersionKind())

	err := c.Get(ctx, client.ObjectKey{
		Name:      child.GetName(),
		Namespace: child.GetNamespace(),
	}, latestChild)
	if err != nil {
		return fmt.Errorf("failed to get child resource: %w", err)
	}

	// Set the owner reference
	setOwnerReference(latestChild, owner)

	// Use a separate field manager for owner references to avoid conflicts with the
	// Crossplane field manager that created the resource via SSA.
	err = c.Update(ctx, latestChild, client.FieldOwner("diff.test"))
	if err != nil {
		return fmt.Errorf("failed to update child with owner reference: %w", err)
	}

	return nil
}

// addResourceRefAndUpdate adds a resource reference to the owner and updates it.
func addResourceRefAndUpdate(ctx context.Context, c client.Client,
	owner *un.Unstructured, owned *un.Unstructured, xrdAPIVersion XrdAPIVersion,
) error {
	// Get the latest version of the owner
	latestOwner := &un.Unstructured{}
	latestOwner.SetGroupVersionKind(owner.GroupVersionKind())

	err := c.Get(ctx, client.ObjectKey{
		Name:      owner.GetName(),
		Namespace: owner.GetNamespace(),
	}, latestOwner)
	if err != nil {
		return fmt.Errorf("failed to get owner for updating references: %w", err)
	}

	// Add the resource reference
	err = addResourceRef(latestOwner, owned, xrdAPIVersion)
	if err != nil {
		return fmt.Errorf("unable to add resource ref: %w", err)
	}

	// Use a separate field manager for resource refs to avoid conflicts with the
	// Crossplane field manager that created the resource via SSA.
	err = c.Update(ctx, latestOwner, client.FieldOwner("diff.test"))
	if err != nil {
		return fmt.Errorf("failed to update owner with resource reference: %w", err)
	}

	return nil
}
