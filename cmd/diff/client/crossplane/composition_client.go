package crossplane

import (
	"context"
	"fmt"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
)

// CompositionClient handles operations related to Compositions.
type CompositionClient interface {
	core.Initializable

	// FindMatchingComposition finds a composition that matches the given XR or claim
	FindMatchingComposition(ctx context.Context, res *un.Unstructured) (*apiextensionsv1.Composition, error)

	// ListCompositions lists all compositions in the cluster
	ListCompositions(ctx context.Context) ([]*apiextensionsv1.Composition, error)

	// GetComposition gets a composition by name
	GetComposition(ctx context.Context, name string) (*apiextensionsv1.Composition, error)

	// FindXRsUsingComposition finds all XRs that use the specified composition
	FindXRsUsingComposition(ctx context.Context, compositionName string, namespace string) ([]*un.Unstructured, error)
}

// DefaultCompositionClient implements CompositionClient.
type DefaultCompositionClient struct {
	resourceClient   kubernetes.ResourceClient
	definitionClient DefinitionClient
	revisionClient   CompositionRevisionClient
	logger           logging.Logger

	// Cache of compositions
	compositions map[string]*apiextensionsv1.Composition
	gvks         []schema.GroupVersionKind
}

// NewCompositionClient creates a new DefaultCompositionClient.
func NewCompositionClient(resourceClient kubernetes.ResourceClient, definitionClient DefinitionClient, logger logging.Logger) CompositionClient {
	return &DefaultCompositionClient{
		resourceClient:   resourceClient,
		definitionClient: definitionClient,
		revisionClient:   NewCompositionRevisionClient(resourceClient, logger),
		logger:           logger,
		compositions:     make(map[string]*apiextensionsv1.Composition),
	}
}

// Initialize loads compositions into the cache.
func (c *DefaultCompositionClient) Initialize(ctx context.Context) error {
	c.logger.Debug("Initializing composition client")

	gvks, err := c.resourceClient.GetGVKsForGroupKind(ctx, "apiextensions.crossplane.io", "Composition")
	if err != nil {
		return errors.Wrap(err, "cannot get Composition GVKs")
	}

	c.gvks = gvks

	// Initialize revision client
	if err := c.revisionClient.Initialize(ctx); err != nil {
		return errors.Wrap(err, "cannot initialize composition revision client")
	}

	// List compositions to populate the cache
	comps, err := c.ListCompositions(ctx)
	if err != nil {
		return errors.Wrap(err, "cannot list compositions")
	}

	// Store in cache
	for _, comp := range comps {
		c.compositions[comp.GetName()] = comp
	}

	c.logger.Debug("Composition client initialized", "compositionsCount", len(c.compositions))

	return nil
}

// ListCompositions lists all compositions in the cluster.
func (c *DefaultCompositionClient) ListCompositions(ctx context.Context) ([]*apiextensionsv1.Composition, error) {
	c.logger.Debug("Listing compositions from cluster")

	// Define the composition GVK
	gvk := schema.GroupVersionKind{
		Group:   "apiextensions.crossplane.io",
		Version: "v1",
		Kind:    "Composition",
	}

	// TODO:  we don't actually use our cached GVKs here -- but there's only one version of Composition
	// and this part is strongly typed which will make a second version hard to handle

	// Get all compositions using the resource client
	unComps, err := c.resourceClient.ListResources(ctx, gvk, "")
	if err != nil {
		c.logger.Debug("Failed to list compositions", "error", err)
		return nil, errors.Wrap(err, "cannot list compositions from cluster")
	}

	// Convert unstructured to typed
	compositions := make([]*apiextensionsv1.Composition, 0, len(unComps))
	for _, obj := range unComps {
		comp := &apiextensionsv1.Composition{}

		err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, comp)
		if err != nil {
			c.logger.Debug("Failed to convert composition from unstructured",
				"name", obj.GetName(),
				"error", err)

			return nil, errors.Wrap(err, "cannot convert unstructured to Composition")
		}

		compositions = append(compositions, comp)
	}

	c.logger.Debug("Successfully retrieved compositions", "count", len(compositions))

	return compositions, nil
}

// GetComposition gets a composition by name.
func (c *DefaultCompositionClient) GetComposition(ctx context.Context, name string) (*apiextensionsv1.Composition, error) {
	// Check cache first
	if comp, ok := c.compositions[name]; ok {
		return comp, nil
	}

	// Not in cache, fetch from cluster
	gvk := schema.GroupVersionKind{
		Group:   "apiextensions.crossplane.io",
		Version: "v1",
		Kind:    "Composition",
	}

	unComp, err := c.resourceClient.GetResource(ctx, gvk, "" /* Compositions are cluster scoped */, name)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot get composition %s", name)
	}

	// Convert to typed # TODO:  troublesome because typed has a version
	comp := &apiextensionsv1.Composition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unComp.Object, comp); err != nil {
		return nil, errors.Wrap(err, "cannot convert unstructured to Composition")
	}

	// Update cache
	c.compositions[name] = comp

	return comp, nil
}

// getCompositionRevisionRef reads the compositionRevisionRef from an XR/Claim.
// Returns the revision name and whether it was found.
func (c *DefaultCompositionClient) getCompositionRevisionRef(xrd, res *un.Unstructured) (string, bool) {
	revisionRefName, found, _ := un.NestedString(res.Object, makeCrossplaneRefPath(xrd.GetAPIVersion(), "compositionRevisionRef", "name")...)
	return revisionRefName, found && revisionRefName != ""
}

// getCompositionUpdatePolicy reads the compositionUpdatePolicy from an XR/Claim.
// Returns the policy value and whether it was found. Defaults to "Automatic" if not found.
func (c *DefaultCompositionClient) getCompositionUpdatePolicy(xrd, res *un.Unstructured) string {
	policy, found, _ := un.NestedString(res.Object, makeCrossplaneRefPath(xrd.GetAPIVersion(), "compositionUpdatePolicy")...)
	if !found || policy == "" {
		return "Automatic" // Default policy
	}

	return policy
}

// resolveCompositionFromRevisions determines which composition to use based on revision logic.
// Returns a composition or nil if standard resolution should be used.
func (c *DefaultCompositionClient) resolveCompositionFromRevisions(
	ctx context.Context,
	xrd, res *un.Unstructured,
	compositionName string,
	resourceID string,
) (*apiextensionsv1.Composition, error) {
	// Check if there's a composition revision reference
	revisionRefName, hasRevisionRef := c.getCompositionRevisionRef(xrd, res)
	updatePolicy := c.getCompositionUpdatePolicy(xrd, res)

	c.logger.Debug("Checking revision resolution",
		"resource", resourceID,
		"hasRevisionRef", hasRevisionRef,
		"revisionRef", revisionRefName,
		"updatePolicy", updatePolicy)

	// Case 1: Automatic policy - always use latest revision
	if updatePolicy == "Automatic" {
		latest, err := c.revisionClient.GetLatestRevisionForComposition(ctx, compositionName)
		if err != nil {
			// If we can't find revisions, fall back to using the composition directly
			c.logger.Debug("Could not find latest revision, using composition directly",
				"compositionName", compositionName,
				"error", err)

			return nil, nil
		}

		comp := c.revisionClient.GetCompositionFromRevision(latest)
		c.logger.Debug("Using latest revision for Automatic policy",
			"resource", resourceID,
			"revisionName", latest.GetName(),
			"revisionNumber", latest.Spec.Revision)

		return comp, nil
	}

	// Case 2: Manual policy with revision reference - use that specific revision
	if updatePolicy == "Manual" && hasRevisionRef {
		revision, err := c.revisionClient.GetCompositionRevision(ctx, revisionRefName)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot get composition revision %s for %s", revisionRefName, resourceID)
		}

		comp := c.revisionClient.GetCompositionFromRevision(revision)
		c.logger.Debug("Using pinned revision for Manual policy",
			"resource", resourceID,
			"revisionName", revisionRefName,
			"revisionNumber", revision.Spec.Revision)

		return comp, nil
	}

	// Case 3: Manual policy without revision reference - use composition directly
	c.logger.Debug("Using composition directly (Manual policy without revision ref)",
		"resource", resourceID)

	return nil, nil
}

// FindMatchingComposition finds a composition matching the given resource.
func (c *DefaultCompositionClient) FindMatchingComposition(ctx context.Context, res *un.Unstructured) (*apiextensionsv1.Composition, error) {
	gvk := res.GroupVersionKind()
	resourceID := fmt.Sprintf("%s/%s", gvk.String(), res.GetName())

	c.logger.Debug("Finding matching composition",
		"resource_name", res.GetName(),
		"gvk", gvk.String())

	// First, check if this is a claim by looking for an XRD that defines this as a claim
	xrd, err := c.definitionClient.GetXRDForClaim(ctx, gvk)
	if err != nil {
		c.logger.Debug("Error checking if resource is claim type",
			"resource", resourceID,
			"error", err)
		// Continue as if not a claim - we'll try normal composition matching
	}

	// If it's a claim, we need to find compositions for the corresponding XR type
	var targetGVK schema.GroupVersionKind

	switch {
	case xrd != nil:
		targetGVK, err = c.getXRTypeFromXRD(xrd, resourceID)
		if err != nil {
			return nil, errors.Wrapf(err, "claim %s requires its XR type to find a composition", resourceID)
		}
	default:
		targetGVK = gvk
		c.logger.Debug("Resource is not a claim type, looking for XRD for XR",
			"resource", resourceID,
			"targetGVK", targetGVK.String())

		xrd, err = c.definitionClient.GetXRDForXR(ctx, gvk)
		if err != nil {
			return nil, errors.Wrapf(err, "resource %s requires its XR type to find a composition", resourceID)
		}
	}

	// Case 1: Check for direct composition reference in spec.compositionRef.name
	comp, err := c.findByDirectReference(ctx, xrd, res, targetGVK, resourceID)
	if err != nil || comp != nil {
		return comp, err
	}

	// Case 2: Check for selector-based composition reference
	comp, err = c.findByLabelSelector(ctx, xrd, res, targetGVK, resourceID)
	if err != nil || comp != nil {
		return comp, err
	}

	// Case 3: Look up by composite type reference (default behavior)
	return c.findByTypeReference(ctx, xrd, targetGVK, resourceID)
}

// getXRTypeFromXRD extracts the XR GroupVersionKind from an XRD.
func (c *DefaultCompositionClient) getXRTypeFromXRD(xrdForClaim *un.Unstructured, resourceID string) (schema.GroupVersionKind, error) {
	// Get the XR type from the XRD
	xrGroup, found, _ := un.NestedString(xrdForClaim.Object, "spec", "group")
	xrKind, kindFound, _ := un.NestedString(xrdForClaim.Object, "spec", "names", "kind")

	if !found || !kindFound {
		return schema.GroupVersionKind{}, errors.New("could not determine group or kind from XRD")
	}

	// Find the referenceable version - there should be exactly one
	xrVersion := ""

	versions, versionsFound, _ := un.NestedSlice(xrdForClaim.Object, "spec", "versions")
	if versionsFound && len(versions) > 0 {
		// Look for the one version that is marked referenceable
		for _, versionObj := range versions {
			if version, ok := versionObj.(map[string]interface{}); ok {
				ref, refFound, _ := un.NestedBool(version, "referenceable")
				if refFound && ref {
					name, nameFound, _ := un.NestedString(version, "name")
					if nameFound {
						xrVersion = name
						break
					}
				}
			}
		}
	}

	// If no referenceable version found, we shouldn't guess
	if xrVersion == "" {
		return schema.GroupVersionKind{}, errors.New("no referenceable version found in XRD")
	}

	targetGVK := schema.GroupVersionKind{
		Group:   xrGroup,
		Version: xrVersion,
		Kind:    xrKind,
	}

	c.logger.Debug("Claim resource detected - targeting XR type for composition matching",
		"claim", resourceID,
		"targetXR", targetGVK.String())

	return targetGVK, nil
}

// isCompositionCompatible checks if a composition is compatible with a GVK.
func (c *DefaultCompositionClient) isCompositionCompatible(comp *apiextensionsv1.Composition, xrGVK schema.GroupVersionKind) bool {
	return comp.Spec.CompositeTypeRef.APIVersion == xrGVK.GroupVersion().String() &&
		comp.Spec.CompositeTypeRef.Kind == xrGVK.Kind
}

// labelsMatch checks if a resource's labels match a selector.
func (c *DefaultCompositionClient) labelsMatch(labels, selector map[string]string) bool {
	// A resource matches a selector if all the selector's labels exist in the resource's labels
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}

	return true
}

func makeCrossplaneRefPath(apiVersion string, path ...string) []string {
	var specCrossplane []string

	switch apiVersion {
	case "apiextensions.crossplane.io/v1":
		// Crossplane v1 keeps these under spec.x
		specCrossplane = []string{"spec"}
	default:
		// Crossplane v2 keeps these under spec.crossplane.x
		specCrossplane = []string{"spec", "crossplane"}
	}

	return append(specCrossplane, path...)
}

// findByDirectReference attempts to find a composition directly referenced by name.
func (c *DefaultCompositionClient) findByDirectReference(ctx context.Context, xrd, res *un.Unstructured, targetGVK schema.GroupVersionKind, resourceID string) (*apiextensionsv1.Composition, error) {
	compositionRefName, compositionRefFound, err := un.NestedString(res.Object, makeCrossplaneRefPath(xrd.GetAPIVersion(), "compositionRef", "name")...)
	if err == nil && compositionRefFound && compositionRefName != "" {
		c.logger.Debug("Found direct composition reference",
			"resource", resourceID,
			"compositionName", compositionRefName)

		// Check if we should use a revision instead
		comp, err := c.resolveCompositionFromRevisions(ctx, xrd, res, compositionRefName, resourceID)
		if err != nil {
			return nil, err
		}

		if comp != nil {
			// Validate that the composition's compositeTypeRef matches the target GVK
			if !c.isCompositionCompatible(comp, targetGVK) {
				return nil, errors.Errorf("composition from revision is not compatible with %s", targetGVK.String())
			}

			return comp, nil
		}

		// No revision-based resolution, use composition directly
		comp, err = c.GetComposition(ctx, compositionRefName)
		if err != nil {
			return nil, errors.Errorf("composition %s referenced in %s not found",
				compositionRefName, resourceID)
		}

		// Validate that the composition's compositeTypeRef matches the target GVK
		if !c.isCompositionCompatible(comp, targetGVK) {
			return nil, errors.Errorf("composition %s is not compatible with %s",
				compositionRefName, targetGVK.String())
		}

		c.logger.Debug("Found composition by direct reference",
			"resource", resourceID,
			"composition", comp.GetName())

		return comp, nil
	}

	return nil, nil // No direct reference found
}

// findByLabelSelector attempts to find compositions that match label selectors.
func (c *DefaultCompositionClient) findByLabelSelector(ctx context.Context, xrd, res *un.Unstructured, targetGVK schema.GroupVersionKind, resourceID string) (*apiextensionsv1.Composition, error) {
	matchLabels, selectorFound, err := un.NestedMap(res.Object, makeCrossplaneRefPath(xrd.GetAPIVersion(), "compositionSelector", "matchLabels")...)
	if err == nil && selectorFound && len(matchLabels) > 0 {
		c.logger.Debug("Found composition selector",
			"resource", resourceID,
			"matchLabels", matchLabels)

		// Convert matchLabels to string map for comparison
		stringLabels := make(map[string]string)
		for k, v := range matchLabels {
			if strVal, ok := v.(string); ok {
				stringLabels[k] = strVal
			}
		}

		// Find compositions matching the labels
		var matchingCompositions []*apiextensionsv1.Composition

		// Get all compositions if we haven't loaded them yet
		if len(c.compositions) == 0 {
			if _, err := c.ListCompositions(ctx); err != nil {
				return nil, errors.Wrap(err, "cannot list compositions to match selector")
			}
		}

		// Search through all compositions looking for compatible ones with matching labels
		for _, comp := range c.compositions {
			// Check if this composition is for the right XR type
			if c.isCompositionCompatible(comp, targetGVK) {
				// Check if labels match
				if c.labelsMatch(comp.GetLabels(), stringLabels) {
					matchingCompositions = append(matchingCompositions, comp)
				}
			}
		}

		// Handle matching results
		switch len(matchingCompositions) {
		case 0:
			return nil, errors.Errorf("no compatible composition found matching labels %v for %s",
				stringLabels, resourceID)
		case 1:
			c.logger.Debug("Found composition by label selector",
				"resource", resourceID,
				"composition", matchingCompositions[0].GetName())

			return matchingCompositions[0], nil
		default:
			// Multiple matches - this is ambiguous and should fail
			names := make([]string, len(matchingCompositions))
			for i, comp := range matchingCompositions {
				names[i] = comp.GetName()
			}

			return nil, errors.New("ambiguous composition selection: multiple compositions match")
		}
	}

	return nil, nil // No label selector found or no matches
}

// findByTypeReference attempts to find a composition by matching the type reference.
func (c *DefaultCompositionClient) findByTypeReference(ctx context.Context, _ *un.Unstructured, targetGVK schema.GroupVersionKind, resourceID string) (*apiextensionsv1.Composition, error) {
	// Get all compositions if we haven't loaded them yet
	if len(c.compositions) == 0 {
		if _, err := c.ListCompositions(ctx); err != nil {
			return nil, errors.Wrap(err, "cannot list compositions to match type")
		}
	}

	// Find all compositions that match this target type
	var compatibleCompositions []*apiextensionsv1.Composition

	for _, comp := range c.compositions {
		if c.isCompositionCompatible(comp, targetGVK) {
			compatibleCompositions = append(compatibleCompositions, comp)
		}
	}

	if len(compatibleCompositions) == 0 {
		c.logger.Debug("No matching composition found",
			"targetGVK", targetGVK.String())

		return nil, errors.Errorf("no composition found for %s", targetGVK.String())
	}

	if len(compatibleCompositions) > 1 {
		// Multiple compositions match, but no selection criteria was provided
		// This is an ambiguous situation
		names := make([]string, len(compatibleCompositions))
		for i, comp := range compatibleCompositions {
			names[i] = comp.GetName()
		}

		return nil, errors.Errorf("ambiguous composition selection: multiple compositions exist for %s", targetGVK.String())
	}

	// We have exactly one matching composition
	c.logger.Debug("Found matching composition by type reference",
		"resource_name", resourceID,
		"composition_name", compatibleCompositions[0].GetName())

	return compatibleCompositions[0], nil
}

// FindXRsUsingComposition finds all XRs that use the specified composition.
func (c *DefaultCompositionClient) FindXRsUsingComposition(ctx context.Context, compositionName string, namespace string) ([]*un.Unstructured, error) {
	c.logger.Debug("Finding XRs using composition",
		"compositionName", compositionName,
		"namespace", namespace)

	// First, get the composition to understand what XR type it targets
	comp, err := c.GetComposition(ctx, compositionName)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot get composition %s", compositionName)
	}

	// Get the XR type this composition targets
	xrAPIVersion := comp.Spec.CompositeTypeRef.APIVersion
	xrKind := comp.Spec.CompositeTypeRef.Kind

	// Parse the GVK
	gv, err := schema.ParseGroupVersion(xrAPIVersion)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot parse API version %s", xrAPIVersion)
	}

	xrGVK := schema.GroupVersionKind{
		Group:   gv.Group,
		Version: gv.Version,
		Kind:    xrKind,
	}

	c.logger.Debug("Composition targets XR type",
		"gvk", xrGVK.String())

	// List all resources of this XR type in the specified namespace
	xrs, err := c.resourceClient.ListResources(ctx, xrGVK, namespace)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot list XRs of type %s in namespace %s", xrGVK.String(), namespace)
	}

	c.logger.Debug("Found XRs of target type", "count", len(xrs))

	// Filter XRs that use this specific composition
	var matchingXRs []*un.Unstructured

	for _, xr := range xrs {
		if c.xrUsesComposition(xr, compositionName) {
			matchingXRs = append(matchingXRs, xr)
		}
	}

	c.logger.Debug("Found XRs using composition",
		"compositionName", compositionName,
		"count", len(matchingXRs))

	return matchingXRs, nil
}

// xrUsesComposition checks if an XR uses the specified composition.
func (c *DefaultCompositionClient) xrUsesComposition(xr *un.Unstructured, compositionName string) bool {
	// Check direct composition reference in spec.compositionRef.name or spec.crossplane.compositionRef.name
	apiVersion := xr.GetAPIVersion()

	// Try both v1 and v2 paths
	paths := [][]string{
		makeCrossplaneRefPath(apiVersion, "compositionRef", "name"),
		{"spec", "compositionRef", "name"}, // fallback for v1
	}

	for _, path := range paths {
		if refName, found, _ := un.NestedString(xr.Object, path...); found && refName == compositionName {
			return true
		}
	}

	return false
}
