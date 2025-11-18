package diffprocessor

import (
	"context"
	"fmt"
	"strings"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/ptr"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"

	"github.com/crossplane/crossplane/v2/cmd/crank/common/resource"
)

// ResourceManager handles resource-related operations like fetching, updating owner refs,
// and identifying resources to be removed.
type ResourceManager interface {
	// FetchCurrentObject retrieves the current state of an object from the cluster
	FetchCurrentObject(ctx context.Context, composite *un.Unstructured, desired *un.Unstructured) (*un.Unstructured, bool, error)

	// UpdateOwnerRefs ensures all OwnerReferences have valid UIDs
	UpdateOwnerRefs(ctx context.Context, parent *un.Unstructured, child *un.Unstructured)

	// FetchObservedResources fetches the observed composed resources for the given XR
	FetchObservedResources(ctx context.Context, xr *cmp.Unstructured) ([]cpd.Unstructured, error)
}

// DefaultResourceManager implements ResourceManager interface.
type DefaultResourceManager struct {
	client     k8.ResourceClient
	defClient  xp.DefinitionClient
	treeClient xp.ResourceTreeClient
	logger     logging.Logger
}

// NewResourceManager creates a new DefaultResourceManager.
func NewResourceManager(client k8.ResourceClient, defClient xp.DefinitionClient, treeClient xp.ResourceTreeClient, logger logging.Logger) ResourceManager {
	return &DefaultResourceManager{
		client:     client,
		defClient:  defClient,
		treeClient: treeClient,
		logger:     logger,
	}
}

// FetchCurrentObject retrieves the current state of the object from the cluster
// It returns the current object, a boolean indicating if it's a new object, and any error.
func (m *DefaultResourceManager) FetchCurrentObject(ctx context.Context, composite *un.Unstructured, desired *un.Unstructured) (*un.Unstructured, bool, error) {
	// Get the GroupVersionKind and name/namespace for lookup
	gvk := desired.GroupVersionKind()
	name := desired.GetName()
	generateName := desired.GetGenerateName()
	namespace := desired.GetNamespace()

	// Create a resource ID for logging
	resourceID := m.createResourceID(gvk, namespace, name, generateName)

	m.logger.Debug("Fetching current object state",
		"resource", resourceID,
		"hasName", name != "",
		"hasGenerateName", generateName != "")

	// Try direct lookup by name if available
	if name != "" {
		current, err := m.client.GetResource(ctx, gvk, namespace, name)
		if err == nil && current != nil {
			m.logger.Debug("Found resource by direct lookup",
				"resource", resourceID,
				"resourceVersion", current.GetResourceVersion())

			m.checkCompositeOwnership(current, composite)

			return current, false, nil
		}

		// If it's not a NotFound error, propagate it
		if err != nil && !apierrors.IsNotFound(err) {
			m.logger.Debug("Error getting resource",
				"resource", resourceID,
				"error", err)

			return nil, false, err
		}
	}

	// If direct lookup failed, try looking up by labels and annotations
	if composite != nil {
		current, found, err := m.lookupByComposite(ctx, composite, desired)
		if err != nil {
			// For resources that primarily use generateName, errors in label-based lookup
			// should result in a new resource rather than an error.
			// This matches the original behavior.
			if generateName != "" {
				m.logger.Debug("Error during label-based lookup for resource with generateName (treating as new)",
					"resource", resourceID,
					"error", err)

				return nil, true, nil
			}

			// For direct name lookups, propagate the error
			m.logger.Debug("Error during label-based lookup",
				"resource", resourceID,
				"error", err)

			return nil, false, err
		}

		if found {
			return current, false, nil
		}
	}

	// We didn't find a matching resource using any strategy
	m.logger.Debug("No matching resource found", "resource", resourceID)

	return nil, true, nil
}

// createResourceID generates a resource ID string for logging purposes.
func (m *DefaultResourceManager) createResourceID(gvk schema.GroupVersionKind, namespace, name, generateName string) string {
	// Handle case with a proper name
	if name != "" {
		if namespace != "" {
			return fmt.Sprintf("%s/%s/%s", gvk.String(), namespace, name)
		}

		return fmt.Sprintf("%s/%s", gvk.String(), name)
	}

	// Handle case with generateName
	if generateName != "" {
		if namespace != "" {
			return fmt.Sprintf("%s/%s/%s*", gvk.String(), namespace, generateName)
		}

		return fmt.Sprintf("%s/%s*", gvk.String(), generateName)
	}

	// Fallback case when neither name nor generateName is provided
	return fmt.Sprintf("%s/<no-name>", gvk.String())
}

// checkCompositeOwnership logs a warning if the resource is owned by a different composite.
func (m *DefaultResourceManager) checkCompositeOwnership(current *un.Unstructured, composite *un.Unstructured) {
	if composite == nil {
		return
	}

	if labels := current.GetLabels(); labels != nil {
		if owner, exists := labels["crossplane.io/composite"]; exists && owner != composite.GetName() {
			// Log a warning if the resource is owned by a different composite
			m.logger.Info(
				// TODO:  should we fail by default here?  maybe require a --force flag to proceed?
				"Warning: Resource already belongs to another composite.  Applying this diff will assume ownership!",
				"resource", fmt.Sprintf("%s/%s", current.GetKind(), current.GetName()),
				"currentOwner", owner,
				"newOwner", composite.GetName(),
			)
		}
	}
}

// lookupByComposite attempts to find a resource by looking at composite ownership and composition resource name.
func (m *DefaultResourceManager) lookupByComposite(ctx context.Context, composite *un.Unstructured, desired *un.Unstructured) (*un.Unstructured, bool, error) {
	// Derive parameters from the provided arguments
	gvk := desired.GroupVersionKind()
	namespace := desired.GetNamespace()
	generateName := desired.GetGenerateName()
	resourceID := m.createResourceID(gvk, namespace, desired.GetName(), generateName)

	// Check if we have annotations
	annotations := desired.GetAnnotations()
	if annotations == nil {
		m.logger.Debug("Resource has no annotations, creating new",
			"resource", resourceID)

		return nil, false, nil
	}

	// Extract the composition resource name from annotations
	compResourceName := m.getCompositionResourceName(annotations)
	if compResourceName == "" {
		m.logger.Debug("Resource has no composition-resource-name, creating new",
			"resource", resourceID)

		return nil, false, nil
	}

	m.logger.Debug("Looking up resource by labels and annotations",
		"resource", resourceID,
		"compositeName", composite.GetName(),
		"compositionResourceName", compResourceName,
		"hasGenerateName", generateName != "")

	// Only proceed if we have a composite name
	if composite.GetName() == "" {
		return nil, false, nil
	}

	// Determine the appropriate label selector based on whether the composite is a claim
	var (
		labelSelector metav1.LabelSelector
		lookupName    string
	)

	isCompositeAClaim := m.defClient.IsClaimResource(ctx, composite)

	if isCompositeAClaim {
		// For claims, we need to find the XR that was created from this claim
		// The downstream resources will have labels pointing to that XR
		// We'll use the claim labels to find downstream resources
		labelSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{
				"crossplane.io/claim-name":      composite.GetName(),
				"crossplane.io/claim-namespace": composite.GetNamespace(),
			},
		}
		lookupName = composite.GetName()
		m.logger.Debug("Using claim labels for resource lookup",
			"claim", composite.GetName(),
			"namespace", composite.GetNamespace())
	} else {
		// For XRs, use the composite label
		labelSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{
				"crossplane.io/composite": composite.GetName(),
			},
		}
		lookupName = composite.GetName()
		m.logger.Debug("Using composite label for resource lookup",
			"composite", composite.GetName())
	}

	// Look up resources with the appropriate label selector
	resources, err := m.client.GetResourcesByLabel(ctx, gvk, namespace, labelSelector)
	if err != nil {
		return nil, false, errors.Wrapf(err, "cannot list resources for %s %s",
			map[bool]string{true: "claim", false: "composite"}[isCompositeAClaim], lookupName)
	}

	if len(resources) == 0 {
		m.logger.Debug("No resources found with owner labels",
			"lookupName", lookupName,
			"isClaimLookup", isCompositeAClaim)

		return nil, false, nil
	}

	m.logger.Debug("Found potential matches by label",
		"resource", resourceID,
		"matchCount", len(resources))

	// Find a resource with matching composition-resource-name
	return m.findMatchingResource(resources, compResourceName, generateName)
}

// getCompositionResourceName extracts the composition resource name from annotations.
func (m *DefaultResourceManager) getCompositionResourceName(annotations map[string]string) string {
	// First check standard annotation
	if value, exists := annotations["crossplane.io/composition-resource-name"]; exists {
		return value
	}

	// Then check function-specific variations
	for key, value := range annotations {
		if strings.HasSuffix(key, "/composition-resource-name") {
			return value
		}
	}

	return ""
}

// findMatchingResource looks through resources to find one matching the composition resource name.
func (m *DefaultResourceManager) findMatchingResource(
	resources []*un.Unstructured,
	compResourceName string,
	generateName string,
) (*un.Unstructured, bool, error) {
	for _, res := range resources {
		resAnnotations := res.GetAnnotations()
		if resAnnotations == nil {
			continue
		}

		// Check if this resource has a matching composition resource name
		if !m.hasMatchingResourceName(resAnnotations, compResourceName) {
			continue
		}

		// TODO:  is this logic correct?  we should definitely match composition-resource-name, even if the generateName
		// doesn't match.  maybe as a fallback if we don't have the annotation?

		// If we have a generateName, verify the match has the right prefix
		if generateName != "" {
			resName := res.GetName()
			if !strings.HasPrefix(resName, generateName) {
				m.logger.Debug("Found resource with matching composition name but wrong generateName prefix",
					"expectedPrefix", generateName,
					"actualName", resName)

				continue
			}
		}

		// We found a match!
		m.logger.Debug("Found resource by label and annotation",
			"resource", res.GetName(),
			"compositionResourceName", compResourceName)

		return res, true, nil
	}

	m.logger.Debug("No matching resource found with composition resource name",
		"compositionResourceName", compResourceName)

	return nil, false, nil
}

// hasMatchingResourceName checks if annotations have a matching composition-resource-name.
func (m *DefaultResourceManager) hasMatchingResourceName(annotations map[string]string, compResourceName string) bool {
	// Check standard annotation
	if value, exists := annotations["crossplane.io/composition-resource-name"]; exists && value == compResourceName {
		return true
	}

	// Check function-specific variations
	for key, value := range annotations {
		if strings.HasSuffix(key, "/composition-resource-name") && value == compResourceName {
			return true
		}
	}

	return false
}

// UpdateOwnerRefs ensures all OwnerReferences have valid UIDs.
// It handles Claims and XRs differently according to Crossplane's ownership model:
// - Claims should never be controller owners of composed resources.
// - XRs should be controller owners and use their UID for matching references.
func (m *DefaultResourceManager) UpdateOwnerRefs(ctx context.Context, parent *un.Unstructured, child *un.Unstructured) {
	// if there's no parent, we are the parent.
	if parent == nil {
		m.logger.Debug("No parent provided for owner references update")
		return
	}

	// Check if the parent is a claim and dispatch to appropriate handler
	isParentAClaim := m.defClient.IsClaimResource(ctx, parent)

	switch {
	case isParentAClaim:
		m.updateOwnerRefsForClaim(parent, child)
	default:
		m.updateOwnerRefsForXR(parent, child)
	}

	// Update composite owner label (common for both Claims and XRs)
	m.updateCompositeOwnerLabel(ctx, parent, child)
}

// updateOwnerRefsForClaim handles owner reference updates when the parent is a Claim.
// Claims should not be controller owners of composed resources per Crossplane's model:
// Claim -> owns -> XR -> owns -> Composed Resources.
func (m *DefaultResourceManager) updateOwnerRefsForClaim(claim *un.Unstructured, child *un.Unstructured) {
	m.logger.Debug("Processing owner references with claim parent",
		"parentKind", claim.GetKind(),
		"parentName", claim.GetName(),
		"childKind", child.GetKind(),
		"childName", child.GetName())

	// Get the current owner references
	refs := child.GetOwnerReferences()
	updatedRefs := make([]metav1.OwnerReference, 0, len(refs))

	for _, ref := range refs {
		// Generate UIDs for any owner references that don't have them
		// For Claims, we never use the Claim's UID even for matching refs
		if ref.UID == "" {
			ref.UID = uuid.NewUUID()
			m.logger.Debug("Generated UID for owner reference",
				"refKind", ref.Kind,
				"refName", ref.Name,
				"newUID", ref.UID)
		}

		// Ensure Claims are never controller owners
		if ref.Kind == claim.GetKind() && ref.Name == claim.GetName() {
			if ref.Controller != nil && *ref.Controller {
				ref.Controller = ptr.To(false)
				m.logger.Debug("Set Controller to false for claim owner reference",
					"refKind", ref.Kind,
					"refName", ref.Name)
			}
		}

		updatedRefs = append(updatedRefs, ref)
	}

	child.SetOwnerReferences(updatedRefs)
}

// updateOwnerRefsForXR handles owner reference updates when the parent is an XR.
// XRs should be controller owners of their composed resources and use their UID
// for matching owner references.
func (m *DefaultResourceManager) updateOwnerRefsForXR(xr *un.Unstructured, child *un.Unstructured) {
	uid := xr.GetUID()
	m.logger.Debug("Updating owner references",
		"parentKind", xr.GetKind(),
		"parentName", xr.GetName(),
		"parentUID", uid,
		"childKind", child.GetKind(),
		"childName", child.GetName())

	// Get the current owner references
	refs := child.GetOwnerReferences()
	m.logger.Debug("Current owner references", "count", len(refs))

	// Create new slice to hold the updated references
	updatedRefs := make([]metav1.OwnerReference, 0, len(refs))

	// Set a valid UID for each reference
	for _, ref := range refs {
		originalUID := ref.UID

		// If there is an owner ref on the dependent that matches the parent XR,
		// use the parent's UID
		if ref.Name == xr.GetName() &&
			ref.APIVersion == xr.GetAPIVersion() &&
			ref.Kind == xr.GetKind() &&
			ref.UID == "" {
			ref.UID = uid
			m.logger.Debug("Updated matching owner reference with parent UID",
				"refName", ref.Name,
				"oldUID", originalUID,
				"newUID", ref.UID)
		}

		// For non-matching owner refs, generate a random UID
		if ref.UID == "" {
			ref.UID = uuid.NewUUID()
			m.logger.Debug("Generated new random UID for owner reference",
				"refName", ref.Name,
				"oldUID", originalUID,
				"newUID", ref.UID)
		}

		updatedRefs = append(updatedRefs, ref)
	}

	// Update the object with the modified owner references
	child.SetOwnerReferences(updatedRefs)

	m.logger.Debug("Updated owner references and labels",
		"newCount", len(updatedRefs))
}

// updateCompositeOwnerLabel updates the ownership labels on the child resource.
// For Claims, it sets claim-name and claim-namespace labels.
// For XRs, it sets the composite label.
func (m *DefaultResourceManager) updateCompositeOwnerLabel(ctx context.Context, parent, child *un.Unstructured) {
	if parent == nil {
		return
	}

	// Get current labels or create a new map
	labels := child.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	parentName := parent.GetName()
	if parentName == "" && parent.GetGenerateName() != "" {
		// For XRs with only generateName, use the generateName prefix
		parentName = parent.GetGenerateName()
	}

	if parentName == "" {
		return
	}

	// Check if the parent is a claim
	isParentAClaim := m.defClient.IsClaimResource(ctx, parent)

	switch {
	case isParentAClaim:
		// For claims, set claim-specific labels (all claims are namespaced)
		labels["crossplane.io/claim-name"] = parentName
		labels["crossplane.io/claim-namespace"] = parent.GetNamespace()

		// For claims, only set the composite label if it doesn't already exist
		// If it exists, it likely points to a generated XR name which we should preserve
		existingComposite, hasComposite := labels["crossplane.io/composite"]
		if !hasComposite {
			// No existing composite label, set it to the claim name
			labels["crossplane.io/composite"] = parentName
			m.logger.Debug("Set composite label for new claim resource",
				"claimName", parentName,
				"claimNamespace", parent.GetNamespace(),
				"child", child.GetName())
		} else {
			// Preserve existing composite label (likely a generated XR name)
			m.logger.Debug("Preserved existing composite label for claim resource",
				"claimName", parentName,
				"claimNamespace", parent.GetNamespace(),
				"existingComposite", existingComposite,
				"child", child.GetName())
		}
	default:
		// For XRs, only set the composite label if it doesn't already exist
		// If it exists, preserve it (e.g., for managed resources that already have correct ownership)
		existingComposite, hasComposite := labels["crossplane.io/composite"]
		if !hasComposite {
			// No existing composite label, set it to the parent XR name
			labels["crossplane.io/composite"] = parentName
			m.logger.Debug("Set composite label for new XR resource",
				"xrName", parentName,
				"child", child.GetName())
		} else {
			// Preserve existing composite label
			m.logger.Debug("Preserved existing composite label for XR resource",
				"xrName", parentName,
				"existingComposite", existingComposite,
				"child", child.GetName())
		}
	}

	child.SetLabels(labels)
}

// FetchObservedResources fetches the observed composed resources for the given XR.
// Returns a flat slice of composed resources suitable for render.Inputs.ObservedResources.
func (m *DefaultResourceManager) FetchObservedResources(ctx context.Context, xr *cmp.Unstructured) ([]cpd.Unstructured, error) {
	m.logger.Debug("Fetching observed resources for XR",
		"xr_kind", xr.GetKind(),
		"xr_name", xr.GetName())

	// Get the resource tree from the cluster
	tree, err := m.treeClient.GetResourceTree(ctx, &xr.Unstructured)
	if err != nil {
		m.logger.Debug("Failed to get resource tree for XR",
			"xr", xr.GetName(),
			"error", err)

		return nil, errors.Wrap(err, "cannot get resource tree")
	}

	// Extract composed resources from the tree
	observed := extractComposedResourcesFromTree(tree)

	m.logger.Debug("Fetched observed composed resources",
		"xr", xr.GetName(),
		"count", len(observed))

	return observed, nil
}

// extractComposedResourcesFromTree recursively extracts all composed resources from a resource tree.
// It returns a flat slice of composed resources, suitable for render.Inputs.ObservedResources.
// Only includes resources with the crossplane.io/composition-resource-name annotation.
func extractComposedResourcesFromTree(tree *resource.Resource) []cpd.Unstructured {
	var resources []cpd.Unstructured

	// Recursively collect composed resources from the tree
	var collectResources func(node *resource.Resource)

	collectResources = func(node *resource.Resource) {
		// Only include resources that have the composition-resource-name annotation
		// (this filters out the root XR and non-composed resources)
		if _, hasAnno := node.Unstructured.GetAnnotations()["crossplane.io/composition-resource-name"]; hasAnno {
			// Convert to cpd.Unstructured (composed resource)
			resources = append(resources, cpd.Unstructured{
				Unstructured: node.Unstructured,
			})
		}

		// Recursively process children
		for _, child := range node.Children {
			collectResources(child)
		}
	}

	// Start from root's children to avoid including the XR itself
	for _, child := range tree.Children {
		collectResources(child)
	}

	return resources
}
