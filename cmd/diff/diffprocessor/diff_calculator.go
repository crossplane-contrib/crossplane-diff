package diffprocessor

import (
	"context"
	"fmt"
	"maps"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"

	"github.com/crossplane/crossplane/v2/cmd/crank/common/resource"
	"github.com/crossplane/crossplane/v2/cmd/crank/render"
)

// DiffCalculator calculates differences between resources.
type DiffCalculator interface {
	// CalculateDiff computes the diff for a single resource
	CalculateDiff(ctx context.Context, composite *un.Unstructured, desired *un.Unstructured) (*dt.ResourceDiff, error)

	// CalculateDiffs computes all diffs including removals for the rendered resources.
	// This is the primary method that most code should use.
	CalculateDiffs(ctx context.Context, xr *cmp.Unstructured, desired render.Outputs) (map[string]*dt.ResourceDiff, error)

	// CalculateNonRemovalDiffs computes diffs for modified/added resources and returns
	// the set of rendered resource keys. This is used by nested XR processing.
	// parentComposite should be nil for root XRs, and the parent XR for nested XRs.
	// Returns: (diffs map, rendered resource keys, error)
	CalculateNonRemovalDiffs(ctx context.Context, xr *cmp.Unstructured, parentComposite *un.Unstructured, desired render.Outputs) (map[string]*dt.ResourceDiff, map[string]bool, error)

	// CalculateRemovedResourceDiffs identifies resources that exist in the cluster but are not
	// in the rendered set. This is called after nested XR processing is complete.
	CalculateRemovedResourceDiffs(ctx context.Context, xr *un.Unstructured, renderedResources map[string]bool) (map[string]*dt.ResourceDiff, error)
}

// DefaultDiffCalculator implements the DiffCalculator interface.
type DefaultDiffCalculator struct {
	treeClient      xp.ResourceTreeClient
	applyClient     k8.ApplyClient
	resourceManager ResourceManager
	logger          logging.Logger
	diffOptions     renderer.DiffOptions
}

// SetDiffOptions updates the diff options used by the calculator.
func (c *DefaultDiffCalculator) SetDiffOptions(options renderer.DiffOptions) {
	c.diffOptions = options
}

// NewDiffCalculator creates a new DefaultDiffCalculator.
func NewDiffCalculator(apply k8.ApplyClient, tree xp.ResourceTreeClient, resourceManager ResourceManager, logger logging.Logger, diffOptions renderer.DiffOptions) DiffCalculator {
	return &DefaultDiffCalculator{
		treeClient:      tree,
		applyClient:     apply,
		resourceManager: resourceManager,
		logger:          logger,
		diffOptions:     diffOptions,
	}
}

// CalculateDiff calculates the diff for a single resource.
func (c *DefaultDiffCalculator) CalculateDiff(ctx context.Context, composite *un.Unstructured, desired *un.Unstructured) (*dt.ResourceDiff, error) {
	// Get resource identification information
	name := desired.GetName()
	generateName := desired.GetGenerateName()

	// Create a resource ID for logging purposes
	var resourceID string

	switch {
	case name != "":
		resourceID = fmt.Sprintf("%s/%s", desired.GetKind(), name)
	case generateName != "":
		resourceID = fmt.Sprintf("%s/%s(generated)", desired.GetKind(), generateName)
	default:
		resourceID = fmt.Sprintf("%s/<no-name>", desired.GetKind())
	}

	c.logger.Debug("Calculating diff", "resource", resourceID)

	// Fetch current object from cluster
	current, isNewObject, err := c.resourceManager.FetchCurrentObject(ctx, composite, desired)
	if err != nil {
		c.logger.Debug("Failed to fetch current object", "resource", resourceID, "error", err)
		return nil, errors.Wrap(err, "cannot fetch current object")
	}

	// Log the resource status
	if isNewObject {
		c.logger.Debug("Resource is new (not found in cluster)", "resource", resourceID)
	} else if current != nil {
		c.logger.Debug("Found existing resource",
			"resourceID", resourceID,
			"existingName", current.GetName(),
			"resourceVersion", current.GetResourceVersion(),
			"resource", current)
	}

	// Preserve existing resource identity for resources with generateName
	desired = c.preserveExistingResourceIdentity(current, desired, resourceID, name)

	// Preserve the composite label for ALL existing resources
	// This is critical because in Crossplane, all resources in a tree point to the ROOT composite,
	// not their immediate parent. We must never change this label.
	desired = c.preserveCompositeLabel(current, desired, resourceID)

	// Update owner references if needed (done after preserving existing labels)
	// IMPORTANT: For composed resources, the owner should be the XR, not a Claim.
	// When composite is the current XR from the cluster, we use it as the owner.
	// This ensures composed resources only have the XR as their controller owner.
	c.resourceManager.UpdateOwnerRefs(ctx, composite, desired)

	// Determine what the resource would look like after application
	wouldBeResult := desired
	if current != nil {
		// Extract the Crossplane field owner from the existing object's managedFields.
		// This ensures our dry-run apply uses the same field owner as Crossplane,
		// which correctly handles field removal detection (SSA removes fields that
		// are owned by this manager but not present in the apply request).
		fieldOwner := k8.GetComposedFieldOwner(current)

		// Perform a dry-run apply to get the result after we'd apply
		c.logger.Debug("Performing dry-run apply",
			"resource", resourceID,
			"name", desired.GetName(),
			"fieldOwner", fieldOwner,
			"desired", desired)

		wouldBeResult, err = c.applyClient.DryRunApply(ctx, desired, fieldOwner)
		if err != nil {
			c.logger.Debug("Dry-run apply failed", "resource", resourceID, "error", err)
			return nil, errors.Wrap(err, "cannot dry-run apply desired object")
		}

		c.logger.Debug("Dry-run apply succeeded", "resource", resourceID, "result", wouldBeResult)
	}

	// Generate diff with the configured options
	diff, err := renderer.GenerateDiffWithOptions(ctx, current, wouldBeResult, c.logger, c.diffOptions)
	if err != nil {
		c.logger.Debug("Failed to generate diff", "resource", resourceID, "error", err)
		return nil, err
	}

	// Log the outcome
	if diff != nil {
		c.logger.Debug("Diff generated",
			"resource", resourceID,
			"diffType", diff.DiffType,
			"hasChanges", diff.DiffType != dt.DiffTypeEqual)
	}

	return diff, nil
}

// CalculateNonRemovalDiffs computes diffs for modified/added resources and returns
// the set of rendered resource keys for removal detection.
//
// TWO-PHASE DIFF ALGORITHM:
// This method implements Phase 1 of our two-phase diff calculation. The two-phase
// approach is necessary to correctly handle nested XRs (Composite Resources that
// themselves create other Composite Resources).
//
// WHY TWO PHASES?
// When processing nested XRs, we must:
//  1. Phase 1 (this method): Calculate diffs for all rendered resources (adds/modifications)
//     and build a set of "rendered resource keys" that tracks what was generated
//  2. Phase 2 (CalculateRemovedResourceDiffs): Compare cluster state against rendered
//     resources to identify removals
//
// The separation is critical because:
//   - Nested XRs are processed recursively BETWEEN these phases
//   - Nested XRs generate additional composed resources that must be added to the
//     "rendered resources" set before removal detection
//   - If we detected removals too early, we'd falsely identify nested XR resources
//     as "to be removed" before they've been processed
//
// EXAMPLE SCENARIO:
//
//	Parent XR renders: [Resource-A, NestedXR-B]
//	NestedXR-B renders: [Resource-C, Resource-D]
//
//	Without two phases:
//	  - We'd see cluster has [Resource-A, Resource-C, Resource-D] from prior render
//	  - We'd see new render has [Resource-A, NestedXR-B]
//	  - We'd INCORRECTLY mark Resource-C and Resource-D as removed
//
//	With two phases:
//	  Phase 1: Calculate diffs for [Resource-A, NestedXR-B], track as rendered
//	  Process nested: Recurse into NestedXR-B, add Resource-C and Resource-D to rendered set
//	  Phase 2: Now see [Resource-A, NestedXR-B, Resource-C, Resource-D] as rendered
//	           No false removal detection!
//
// Returns: (diffs map, rendered resource keys, error).
func (c *DefaultDiffCalculator) CalculateNonRemovalDiffs(ctx context.Context, xr *cmp.Unstructured, parentComposite *un.Unstructured, desired render.Outputs) (map[string]*dt.ResourceDiff, map[string]bool, error) {
	xrName := xr.GetName()
	c.logger.Debug("Calculating diffs",
		"xr", xrName,
		"composedCount", len(desired.ComposedResources))

	diffs := make(map[string]*dt.ResourceDiff)

	var errs []error

	renderedResources := make(map[string]bool)

	// Determine if this is a nested XR or root XR, and select the appropriate XR to diff
	renderedXR := desired.CompositeResource.GetUnstructured()

	var (
		desiredXR       *un.Unstructured
		compositeParent *un.Unstructured
	)

	if renderedXR.GetAnnotations()["crossplane.io/composition-resource-name"] != "" {
		// NESTED XR: Use rendered XR (it's a composed resource from parent's composition)
		c.logger.Debug("Processing nested XR", "xr", xrName, "hasParent", parentComposite != nil)

		desiredXR = renderedXR
		compositeParent = parentComposite
	} else {
		// ROOT XR: Use input XR as-is (source of truth, don't use rendered metadata)
		c.logger.Debug("Processing root XR", "xr", xrName)

		desiredXR = xr.GetUnstructured()
		compositeParent = nil
	}

	// Calculate diff for the XR
	xrDiff, err := c.CalculateDiff(ctx, compositeParent, desiredXR)
	if err != nil || xrDiff == nil {
		return nil, nil, errors.Wrap(err, "cannot calculate diff for XR")
	} else if xrDiff.DiffType != dt.DiffTypeEqual {
		key := xrDiff.GetDiffKey()
		diffs[key] = xrDiff
	}

	// Then calculate diffs for all composed resources
	for _, d := range desired.ComposedResources {
		un := &un.Unstructured{Object: d.UnstructuredContent()}

		// Generate a key to identify this resource
		apiVersion := un.GetAPIVersion()
		kind := un.GetKind()
		name := un.GetName()
		generateName := un.GetGenerateName()

		// For logging purposes - create a resource ID that might use generateName
		resourceID := fmt.Sprintf("%s/%s", kind, name)
		if name == "" && generateName != "" {
			resourceID = fmt.Sprintf("%s/%s*", kind, generateName)
		}

		// For resources using generateName, the name will be empty but we shouldn't skip them
		// Only skip if both name and generateName are empty (likely a template issue)
		if name == "" && generateName == "" {
			c.logger.Debug("Skipping resource with empty name and generateName",
				"kind", kind,
				"apiVersion", apiVersion)

			continue
		}

		diff, err := c.CalculateDiff(ctx, xrDiff.Current, un)
		if err != nil {
			c.logger.Debug("Error calculating diff for composed resource", "resource", resourceID, "error", err)
			errs = append(errs, errors.Wrapf(err, "cannot calculate diff for %s", resourceID))

			continue
		}

		diffKey := diff.GetDiffKey()
		if diff.DiffType != dt.DiffTypeEqual {
			diffs[diffKey] = diff
		}

		renderedResources[diffKey] = true
		c.logger.Debug("Added resource to renderedResources",
			"xr", xrName,
			"diffKey", diffKey,
			"diffType", diff.DiffType)
	}

	// Log a summary
	c.logger.Debug("Diff calculation complete",
		"totalDiffs", len(diffs),
		"renderedResourcesCount", len(renderedResources),
		"errors", len(errs),
		"xr", xrName)

	if len(errs) > 0 {
		return diffs, renderedResources, errors.Join(errs...)
	}

	return diffs, renderedResources, nil
}

// CalculateDiffs computes all diffs including removals for the rendered resources.
// This is the primary method that most code should use.
func (c *DefaultDiffCalculator) CalculateDiffs(ctx context.Context, xr *cmp.Unstructured, desired render.Outputs) (map[string]*dt.ResourceDiff, error) {
	// First calculate diffs for modified/added resources
	// parentComposite is nil because CalculateDiffs is only called for root XRs
	diffs, renderedResources, err := c.CalculateNonRemovalDiffs(ctx, xr, nil, desired)
	if err != nil {
		return nil, err
	}

	// Then detect removed resources
	removedDiffs, err := c.CalculateRemovedResourceDiffs(ctx, xr.GetUnstructured(), renderedResources)
	if err != nil {
		return nil, err
	}

	// Merge removed diffs into the main diffs map
	maps.Copy(diffs, removedDiffs)

	return diffs, nil
}

// CalculateRemovedResourceDiffs identifies resources that would be removed and calculates their diffs.
func (c *DefaultDiffCalculator) CalculateRemovedResourceDiffs(ctx context.Context, xr *un.Unstructured, renderedResources map[string]bool) (map[string]*dt.ResourceDiff, error) {
	xrName := xr.GetName()
	c.logger.Debug("Checking for resources to be removed",
		"xr", xrName,
		"renderedResourceCount", len(renderedResources))

	removedDiffs := make(map[string]*dt.ResourceDiff)

	// Try to get the resource tree
	resourceTree, err := c.treeClient.GetResourceTree(ctx, xr)
	if err != nil {
		// Log the error but continue - we just won't detect removed resources
		c.logger.Debug("Cannot get resource tree; aborting", "error", err)
		return nil, errors.New("cannot get resource tree")
	}

	// Create a handler function to recursively traverse the tree and find composed resources
	var findRemovedResources func(node *resource.Resource)

	findRemovedResources = func(node *resource.Resource) {
		// Skip the root (XR) node
		if _, hasAnno := node.Unstructured.GetAnnotations()["crossplane.io/composition-resource-name"]; hasAnno {
			apiVersion := node.Unstructured.GetAPIVersion()
			kind := node.Unstructured.GetKind()
			name := node.Unstructured.GetName()
			resourceID := fmt.Sprintf("%s/%s", kind, name)

			// Use the same key format as in CalculateDiffs to check if this resource was rendered
			key := dt.MakeDiffKey(apiVersion, kind, name)

			if !renderedResources[key] {
				// This resource exists but wasn't rendered - it will be removed
				c.logger.Debug("Resource will be removed", "resource", resourceID)

				diff, err := renderer.GenerateDiffWithOptions(ctx, &node.Unstructured, nil, c.logger, c.diffOptions)
				if err != nil {
					c.logger.Debug("Cannot calculate removal diff (continuing)",
						"resource", resourceID,
						"error", err)

					return
				}

				if diff != nil {
					diffKey := diff.GetDiffKey()
					removedDiffs[diffKey] = diff
				}
			}
		}

		// Continue recursively traversing children
		for _, child := range node.Children {
			findRemovedResources(child)
		}
	}

	// Start the traversal from the root's children to skip the XR itself
	for _, child := range resourceTree.Children {
		findRemovedResources(child)
	}

	c.logger.Debug("Found resources to be removed", "count", len(removedDiffs))

	return removedDiffs, nil
}

// preserveExistingResourceIdentity preserves the identity (name, generateName, labels) from an existing
// resource with generateName to ensure dry-run apply works on the correct resource identity.
// This is critical for claim scenarios where the rendered name differs from the generated name.
func (c *DefaultDiffCalculator) preserveExistingResourceIdentity(current, desired *un.Unstructured, resourceID, renderedName string) *un.Unstructured {
	// Only preserve identity for existing resources with both generateName and name
	if current == nil || current.GetGenerateName() == "" || current.GetName() == "" {
		return desired
	}

	currentName := current.GetName()
	currentGenerateName := current.GetGenerateName()

	c.logger.Debug("Using existing resource identity for dry-run apply",
		"resource", resourceID,
		"renderedName", renderedName,
		"currentName", currentName,
		"currentGenerateName", currentGenerateName)

	// Create a copy of the desired object and set the identity from the current resource
	// This ensures dry-run apply works on the correct resource identity
	desiredCopy := desired.DeepCopy()
	desiredCopy.SetName(currentName)
	desiredCopy.SetGenerateName(currentGenerateName)

	// Preserve important labels from the existing resource, particularly the composite label
	// This ensures the dry-run apply gets the right labels and preserves them correctly
	currentLabels := current.GetLabels()
	if currentLabels != nil {
		desiredLabels := desiredCopy.GetLabels()
		if desiredLabels == nil {
			desiredLabels = make(map[string]string)
		}

		// Preserve the composite label if it exists (critical for claims)
		if compositeLabel, exists := currentLabels["crossplane.io/composite"]; exists {
			desiredLabels["crossplane.io/composite"] = compositeLabel
			c.logger.Debug("Preserved composite label for dry-run apply",
				"resource", resourceID,
				"compositeLabel", compositeLabel)
		}

		desiredCopy.SetLabels(desiredLabels)
	}

	return desiredCopy
}

// preserveCompositeLabel preserves the crossplane.io/composite label from an existing composed resource.
// This is critical because in Crossplane, all composed resources (including nested XRs) point to the ROOT composite.
// We must never change this label for existing composed resources.
// Root XRs don't have the composition-resource-name annotation and shouldn't have this label.
func (c *DefaultDiffCalculator) preserveCompositeLabel(current, desired *un.Unstructured, resourceID string) *un.Unstructured {
	// Only preserve for composed resources (those with composition-resource-name annotation)
	if current == nil || desired.GetAnnotations()["crossplane.io/composition-resource-name"] == "" {
		return desired
	}

	compositeLabel, exists := current.GetLabels()["crossplane.io/composite"]
	if !exists {
		return desired
	}

	// Preserve the composite label
	desiredCopy := desired.DeepCopy()

	desiredLabels := desiredCopy.GetLabels()
	if desiredLabels == nil {
		desiredLabels = make(map[string]string)
	}

	desiredLabels["crossplane.io/composite"] = compositeLabel
	desiredCopy.SetLabels(desiredLabels)

	c.logger.Debug("Preserved composite label from existing composed resource",
		"resource", resourceID,
		"compositeLabel", compositeLabel)

	return desiredCopy
}
