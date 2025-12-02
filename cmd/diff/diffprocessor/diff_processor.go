// Package diffprocessor contains the logic to calculate and render diffs.
package diffprocessor

import (
	"context"
	"fmt"
	"io"
	"maps"
	"reflect"
	"strings"

	"dario.cat/mergo"
	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/serial"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/types"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
	"github.com/crossplane/crossplane/v2/cmd/crank/render"
)

// RenderFunc defines the signature of a function that can render resources.
type RenderFunc func(ctx context.Context, log logging.Logger, in render.Inputs) (render.Outputs, error)

// DiffProcessor interface for processing resources.
type DiffProcessor interface {
	// PerformDiff processes resources using a composition provider function
	PerformDiff(ctx context.Context, stdout io.Writer, resources []*un.Unstructured, compositionProvider types.CompositionProvider) error

	// DiffSingleResource processes a single resource and returns its diffs
	DiffSingleResource(ctx context.Context, res *un.Unstructured, compositionProvider types.CompositionProvider) (map[string]*dt.ResourceDiff, error)

	// Initialize loads required resources like CRDs and environment configs
	Initialize(ctx context.Context) error
}

// DefaultDiffProcessor implements DiffProcessor with modular components.
type DefaultDiffProcessor struct {
	compClient           xp.CompositionClient
	defClient            xp.DefinitionClient
	schemaClient         k8.SchemaClient
	resourceManager      ResourceManager
	config               ProcessorConfig
	functionProvider     FunctionProvider
	schemaValidator      SchemaValidator
	diffCalculator       DiffCalculator
	diffRenderer         renderer.DiffRenderer
	requirementsProvider *RequirementsProvider
}

// NewDiffProcessor creates a new DefaultDiffProcessor with the provided options.
func NewDiffProcessor(k8cs k8.Clients, xpcs xp.Clients, opts ...ProcessorOption) DiffProcessor {
	// Create default configuration
	// Note: Behavior defaults (Namespace, Colorize, Compact, MaxNestedDepth) are intentionally
	// not set here. They should be provided via ProcessorOptions from the CLI layer.
	config := ProcessorConfig{
		Logger:     logging.NewNopLogger(),
		RenderFunc: render.Render,
	}

	// Apply all provided options
	for _, option := range opts {
		option(&config)
	}

	// Set default factory functions if not provided
	config.SetDefaultFactories()

	// Wrap the RenderFunc with serialization if a mutex was provided
	// This transparently handles serialization without requiring callers to worry about it
	if config.RenderMutex != nil {
		config.RenderFunc = serial.RenderFunc(config.RenderFunc, config.RenderMutex)
	}

	// Create the diff options based on configuration
	diffOpts := config.GetDiffOptions()

	// Create components using factories
	resourceManager := config.Factories.ResourceManager(k8cs.Resource, xpcs.Definition, xpcs.ResourceTree, config.Logger)
	schemaValidator := config.Factories.SchemaValidator(k8cs.Schema, xpcs.Definition, config.Logger)
	requirementsProvider := config.Factories.RequirementsProvider(k8cs.Resource, xpcs.Environment, config.RenderFunc, config.Logger)
	diffCalculator := config.Factories.DiffCalculator(k8cs.Apply, xpcs.ResourceTree, resourceManager, config.Logger, diffOpts)
	diffRenderer := config.Factories.DiffRenderer(config.Logger, diffOpts)
	functionProvider := config.Factories.FunctionProvider(xpcs.Function, config.Logger)

	processor := &DefaultDiffProcessor{
		compClient:           xpcs.Composition,
		defClient:            xpcs.Definition,
		schemaClient:         k8cs.Schema,
		resourceManager:      resourceManager,
		config:               config,
		functionProvider:     functionProvider,
		schemaValidator:      schemaValidator,
		diffCalculator:       diffCalculator,
		diffRenderer:         diffRenderer,
		requirementsProvider: requirementsProvider,
	}

	return processor
}

// Initialize loads required resources like CRDs and environment configs.
func (p *DefaultDiffProcessor) Initialize(ctx context.Context) error {
	p.config.Logger.Debug("Initializing diff processor")

	// Load CRDs (handled by the schema validator)
	err := p.initializeSchemaValidator(ctx)
	if err != nil {
		return err
	}

	// Init requirements provider
	err = p.requirementsProvider.Initialize(ctx)
	if err != nil {
		return err
	}

	p.config.Logger.Debug("Diff processor initialized")

	return nil
}

// initializeSchemaValidator initializes the schema validator with CRDs.
func (p *DefaultDiffProcessor) initializeSchemaValidator(ctx context.Context) error {
	// If the schema validator implements our interface with LoadCRDs, use it
	if validator, ok := p.schemaValidator.(*DefaultSchemaValidator); ok {
		err := validator.LoadCRDs(ctx)
		if err != nil {
			return errors.Wrap(err, "cannot load CRDs")
		}

		p.config.Logger.Debug("Schema validator initialized with CRDs",
			"crdCount", len(validator.GetCRDs()))
	}

	return nil
}

// PerformDiff processes resources using a composition provider function.
func (p *DefaultDiffProcessor) PerformDiff(ctx context.Context, stdout io.Writer, resources []*un.Unstructured, compositionProvider types.CompositionProvider) error {
	p.config.Logger.Debug("Processing resources with composition provider", "count", len(resources))

	if len(resources) == 0 {
		p.config.Logger.Debug("No resources to process")
		return nil
	}

	// Collect all diffs across all resources
	allDiffs := make(map[string]*dt.ResourceDiff)

	var errs []error

	for _, res := range resources {
		resourceID := fmt.Sprintf("%s/%s", res.GetKind(), res.GetName())

		diffs, err := p.DiffSingleResource(ctx, res, compositionProvider)
		if err != nil {
			p.config.Logger.Debug("Failed to process resource", "resource", resourceID, "error", err)
			errs = append(errs, errors.Wrapf(err, "unable to process resource %s", resourceID))

			// Write error message to stdout so user can see it
			errMsg := fmt.Sprintf("ERROR: Failed to process %s: %v\n\n", resourceID, err)
			if _, writeErr := fmt.Fprint(stdout, errMsg); writeErr != nil {
				p.config.Logger.Debug("Failed to write error message", "error", writeErr)
			}
		} else {
			// Merge the diffs into our combined map
			maps.Copy(allDiffs, diffs)
		}
	}

	// Only render diffs if we found some
	if len(allDiffs) > 0 {
		// Render all diffs in a single pass
		err := p.diffRenderer.RenderDiffs(stdout, allDiffs)
		if err != nil {
			p.config.Logger.Debug("Failed to render diffs", "error", err)
			errs = append(errs, errors.Wrap(err, "failed to render diffs"))
		}
	}

	p.config.Logger.Debug("Processing complete",
		"resourceCount", len(resources),
		"totalDiffs", len(allDiffs),
		"errorCount", len(errs))

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

// DiffSingleResource handles one resource at a time and returns its diffs.
// The compositionProvider function is called to obtain the composition to use for rendering.
// This is the public method for top-level XR diffing, which enables removal detection.
func (p *DefaultDiffProcessor) DiffSingleResource(ctx context.Context, res *un.Unstructured, compositionProvider types.CompositionProvider) (map[string]*dt.ResourceDiff, error) {
	diffs, _, err := p.diffSingleResourceInternal(ctx, res, compositionProvider, nil, true)
	return diffs, err
}

// diffSingleResourceInternal is the internal implementation that allows control over removal detection.
// parentXR should be nil for root XRs, and the parent XR for nested XRs.
// detectRemovals should be true for top-level XRs and false for nested XRs (which don't own their composed resources).
func (p *DefaultDiffProcessor) diffSingleResourceInternal(ctx context.Context, res *un.Unstructured, compositionProvider types.CompositionProvider, parentXR *cmp.Unstructured, detectRemovals bool) (map[string]*dt.ResourceDiff, map[string]bool, error) {
	resourceID := fmt.Sprintf("%s/%s", res.GetKind(), res.GetName())
	p.config.Logger.Debug("Processing resource", "resource", resourceID)

	xr, done, err := p.SanitizeXR(res, resourceID)
	if done {
		return nil, nil, err
	}

	// Get the composition using the provided function
	comp, err := compositionProvider(ctx, res)
	if err != nil {
		p.config.Logger.Debug("Failed to get composition", "resource", resourceID, "error", err)
		return nil, nil, errors.Wrap(err, "cannot get composition")
	}

	p.config.Logger.Debug("Resource setup complete", "resource", resourceID, "composition", comp.GetName())

	// Get functions for this composition (provider handles caching internally)
	fns, err := p.functionProvider.GetFunctionsForComposition(comp)
	if err != nil {
		p.config.Logger.Debug("Failed to get functions", "resource", resourceID, "error", err)
		return nil, nil, errors.Wrap(err, "cannot get functions for composition")
	}

	// Note: Serialization mutex prevents concurrent Docker operations.
	// In e2e tests, named Docker containers (via annotations) reuse containers across renders.

	// Apply XRD defaults before rendering
	err = p.applyXRDDefaults(ctx, xr, resourceID)
	if err != nil {
		p.config.Logger.Debug("Failed to apply XRD defaults", "resource", resourceID, "error", err)
		return nil, nil, errors.Wrap(err, "cannot apply XRD defaults")
	}

	// Fetch the existing XR from the cluster to populate UID and other cluster-specific fields.
	// This ensures that when composition functions set owner references on nested resources,
	// they use the correct UID from the cluster, preventing duplicate owner reference errors.
	existingXRFromCluster, isNew, err := p.resourceManager.FetchCurrentObject(ctx, nil, xr.GetUnstructured())
	switch {
	case err == nil && !isNew && existingXRFromCluster != nil:
		// Preserve cluster-specific fields from the existing XR
		xr.SetUID(existingXRFromCluster.GetUID())
		xr.SetResourceVersion(existingXRFromCluster.GetResourceVersion())
		p.config.Logger.Debug("Populated XR with cluster UID before rendering",
			"resource", resourceID,
			"uid", existingXRFromCluster.GetUID())
	case isNew:
		p.config.Logger.Debug("XR is new (will render without UID)", "resource", resourceID)
	default:
		// Error fetching
		p.config.Logger.Debug("Error fetching XR from cluster (will render without UID)",
			"resource", resourceID,
			"error", err)
	}

	// If the input was a Claim, we need to fetch observed resources for the backing XR (not the Claim)
	// to get nested XRs. Check if the existing resource in cluster (if any) has a resourceRef field.
	// TODO: Verify assumption that Claims don't have resource trees pointing to composed resources,
	// and that we need to fetch the backing XR's tree instead. Test against real Crossplane behavior.
	var observedResources []cpd.Unstructured
	var backingXRName string
	var backingXRAPIVersion string
	var backingXRKind string
	var existingBackingXRUn *un.Unstructured

	if existingXRFromCluster != nil {
		resourceRefRaw, hasResourceRef, _ := un.NestedFieldCopy(existingXRFromCluster.Object, "spec", "resourceRef")
		if hasResourceRef && resourceRefRaw != nil {
			// This is a Claim - extract the backing XR details from resourceRef
			resourceRefMap, ok := resourceRefRaw.(map[string]interface{})
			if ok {
				name, nameFound, _ := un.NestedString(resourceRefMap, "name")
				apiVersion, apiVersionFound, _ := un.NestedString(resourceRefMap, "apiVersion")
				kind, kindFound, _ := un.NestedString(resourceRefMap, "kind")
				if nameFound && apiVersionFound && kindFound {
					backingXRName = name
					backingXRAPIVersion = apiVersion
					backingXRKind = kind
					p.config.Logger.Debug("Found resourceRef in existing Claim",
						"claim", xr.GetName(),
						"backingXR", backingXRName,
						"apiVersion", backingXRAPIVersion,
						"kind", backingXRKind)
				}
			}
		}
	}

	if backingXRName != "" && backingXRAPIVersion != "" && backingXRKind != "" {
		p.config.Logger.Debug("Input is a Claim, fetching observed resources for backing XR",
			"claim", xr.GetName(),
			"backingXR", backingXRName)

		// Fetch the backing XR from the cluster to get its UID and other metadata
		backingXR := cmp.New()
		backingXR.SetAPIVersion(backingXRAPIVersion)
		backingXR.SetKind(backingXRKind)
		backingXR.SetName(backingXRName)

		// Convert to unstructured for FetchCurrentObject
		backingXRUnstructured := backingXR.GetUnstructured()

		// Fetch the actual backing XR from cluster to get its UID (no parent composite)
		// Note: FetchCurrentObject returns (resource, isNew, error) where isNew=true means resource doesn't exist
		var fetchErr error
		existingBackingXRUn, _, fetchErr = p.resourceManager.FetchCurrentObject(ctx, nil, backingXRUnstructured)
		err = fetchErr
		if err != nil {
			p.config.Logger.Debug("Could not fetch backing XR from cluster (continuing with Claim's observed resources)",
				"backingXR", backingXRName,
				"error", err)
		} else if existingBackingXRUn != nil {
			// Convert back to composite for FetchObservedResources
			existingBackingXR := cmp.New()
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(existingBackingXRUn.UnstructuredContent(), existingBackingXR); err != nil {
				p.config.Logger.Debug("Could not convert backing XR to composite (continuing with Claim's observed resources)",
					"backingXR", backingXRName,
					"error", err)
			} else {
				// Use the existing backing XR with its UID to fetch observed resources
				p.config.Logger.Debug("Fetched backing XR from cluster",
					"backingXR", backingXRName,
					"uid", existingBackingXR.GetUID())

				// Fetch observed resources for the backing XR
				backingXRObservedResources, err := p.resourceManager.FetchObservedResources(ctx, existingBackingXR)
				if err != nil {
					p.config.Logger.Debug("Could not fetch observed resources for backing XR (continuing with empty list)",
						"backingXR", backingXRName,
						"error", err)
				} else {
					// Use the backing XR's observed resources for nested XR processing
					observedResources = backingXRObservedResources
					p.config.Logger.Debug("Using observed resources from backing XR",
						"backingXR", backingXRName,
						"count", len(observedResources))
				}
			}
		}
	}

	// Fetch observed resources for use in rendering (needed for getComposedResource template function)
	// For new XRs that don't exist in the cluster yet, this will return an empty list
	// Skip if we already fetched backing XR's observed resources for a Claim
	if observedResources == nil {
		observedResources, err = p.resourceManager.FetchObservedResources(ctx, xr)
		if err != nil {
			// Log the error but continue with empty observed resources
			// This handles the case where the XR doesn't exist in the cluster yet
			p.config.Logger.Debug("Could not fetch observed resources (continuing with empty list)",
				"resource", resourceID,
				"error", err)

			observedResources = nil
		}
	}

	// Perform iterative rendering and requirements reconciliation
	desired, err := p.RenderWithRequirements(ctx, xr, comp, fns, resourceID, observedResources)
	if err != nil {
		p.config.Logger.Debug("Resource rendering failed", "resource", resourceID, "error", err)
		return nil, nil, errors.Wrap(err, "cannot render resources with requirements")
	}

	// If we rendered from a Claim, fix the labels in nested XRs to point to the backing XR
	// instead of the Claim. This ensures FetchCurrentObject can match them to existing resources.
	if existingBackingXRUn != nil {
		p.config.Logger.Debug("Fixing nested XR labels to point to backing XR",
			"claim", xr.GetName(),
			"backingXR", existingBackingXRUn.GetName(),
			"composedResourcesCount", len(desired.ComposedResources))

		for i := range desired.ComposedResources {
			resource := &desired.ComposedResources[i]

			p.config.Logger.Debug("Checking composed resource",
				"kind", resource.GetKind(),
				"name", resource.GetName(),
				"generateName", resource.GetGenerateName(),
				"hasCompositionResourceNameAnnotation", resource.GetAnnotations()["crossplane.io/composition-resource-name"] != "")

			// Check if this is a nested XR (has composition-resource-name annotation)
			if _, hasAnno := resource.GetAnnotations()["crossplane.io/composition-resource-name"]; hasAnno {
				// Check if there's an XRD for this Kind - if so, it's an XR
				gvk := resource.GroupVersionKind()
				xrd, err := p.defClient.GetXRDForXR(ctx, gvk)
				isXR := (err == nil && xrd != nil)

				p.config.Logger.Debug("Checking if resource is XR",
					"resource", resource.GetKind()+"/"+resource.GetName(),
					"gvk", gvk.String(),
					"isXR", isXR)

				// If it's an XR, fix its labels
				if isXR {
					// This is a nested XR - fix its labels and generateName
					labels := resource.GetLabels()
					if labels == nil {
						labels = make(map[string]string)
					}

					oldComposite := labels["crossplane.io/composite"]
					// Update composite label to point to backing XR
					labels["crossplane.io/composite"] = existingBackingXRUn.GetName()
					resource.SetLabels(labels)

					// Update generateName to use backing XR's name as prefix
					oldGenerateName := resource.GetGenerateName()
					if oldGenerateName != "" && backingXRName != "" {
						// Replace Claim's name prefix with backing XR's name prefix
						newGenerateName := backingXRName + "-"
						resource.SetGenerateName(newGenerateName)

						p.config.Logger.Debug("Fixed nested XR identity",
							"resource", resource.GetKind()+"/"+resource.GetName(),
							"oldGenerateName", oldGenerateName,
							"newGenerateName", newGenerateName,
							"oldComposite", oldComposite,
							"newComposite", existingBackingXRUn.GetName())
					}

					// Fix owner references to point to the backing XR instead of the Claim
					// The rendered nested XR has an owner ref to the Claim, but Crossplane
					// actually sets the owner ref to the backing XR.
					ownerRefs := resource.GetOwnerReferences()
					for j := range ownerRefs {
						// Replace owner ref if it points to the Claim
						if ownerRefs[j].Name == xr.GetName() && ownerRefs[j].Kind == xr.GetKind() {
							ownerRefs[j].Name = existingBackingXRUn.GetName()
							ownerRefs[j].Kind = existingBackingXRUn.GetKind()
							ownerRefs[j].UID = existingBackingXRUn.GetUID()
							p.config.Logger.Debug("Fixed nested XR owner reference",
								"resource", resource.GetKind()+"/"+resource.GetName(),
								"oldOwner", xr.GetName(),
								"newOwner", existingBackingXRUn.GetName())
						}
					}
					resource.SetOwnerReferences(ownerRefs)
				}
			}
		}
	}

	// For nested XRs in a CLAIM context, propagate the composite label to all composed resources.
	// In Crossplane, when rendering from a Claim, ALL composed resources (at all nesting levels)
	// get the BACKING XR's name in their composite label. This is different from non-Claim XR trees
	// where each resource gets its immediate parent XR's name.
	//
	// We detect Claim context by checking for claim labels (crossplane.io/claim-name).
	// Only propagate when:
	// 1. We're in a Claim context (XR has claim-name label)
	// 2. The XR has a composite label different from its name (set by preserveNestedXRIdentity)
	//
	// WHY NON-CLAIM XRs DON'T NEED THIS:
	// For standalone XR trees (no Claim), Crossplane's render pipeline correctly sets the
	// crossplane.io/composite label on each composed resource to its immediate parent XR's name.
	// The DiffCalculator.preserveCompositeLabel() method then preserves this label from existing
	// cluster resources, ensuring no spurious diffs. No manual propagation is needed.
	xrCompositeLabel := xr.GetLabels()["crossplane.io/composite"]
	xrClaimName := xr.GetLabels()["crossplane.io/claim-name"]
	isClaimContext := xrClaimName != ""
	if isClaimContext && xrCompositeLabel != "" && xrCompositeLabel != xr.GetName() {
		p.config.Logger.Debug("Propagating composite label to composed resources",
			"xr", xr.GetName(),
			"compositeLabel", xrCompositeLabel,
			"composedCount", len(desired.ComposedResources))

		for i := range desired.ComposedResources {
			resource := &desired.ComposedResources[i]
			labels := resource.GetLabels()
			if labels == nil {
				labels = make(map[string]string)
			}

			oldLabel := labels["crossplane.io/composite"]
			labels["crossplane.io/composite"] = xrCompositeLabel
			resource.SetLabels(labels)

			// Also fix generateName to use the root XR's name as prefix
			oldGenerateName := resource.GetGenerateName()
			if oldGenerateName != "" {
				newGenerateName := xrCompositeLabel + "-"
				resource.SetGenerateName(newGenerateName)

				p.config.Logger.Debug("Fixed composed resource identity for nested XR",
					"resource", resource.GetKind()+"/"+resource.GetName(),
					"oldComposite", oldLabel,
					"newComposite", xrCompositeLabel,
					"oldGenerateName", oldGenerateName,
					"newGenerateName", newGenerateName)
			}
		}
	}

	// Merge the result of the render together with the input XR
	p.config.Logger.Debug("Merging and validating rendered resources",
		"resource", resourceID,
		"composedCount", len(desired.ComposedResources))


	xrUnstructured, err := mergeUnstructured(
		desired.CompositeResource.GetUnstructured(),
		xr.GetUnstructured(),
	)
	if err != nil {
		p.config.Logger.Debug("Failed to merge XR", "resource", resourceID, "error", err)
		return nil, nil, errors.Wrap(err, "cannot merge input XR with result of rendered XR")
	}

	// Clean up namespaces from cluster-scoped resources
	// Crossplane PR #6812 fixed issue #6782 by making render propagate namespaces from XR to all
	// composed resources, but it doesn't check if resources are cluster-scoped. This cleanup
	// removes namespaces from cluster-scoped resources. See removeNamespacesFromClusterScopedResources
	// for details on the upstream fix needed.
	if err := p.removeNamespacesFromClusterScopedResources(ctx, desired.ComposedResources); err != nil {
		p.config.Logger.Debug("Failed to clean up namespaces from cluster-scoped resources", "resource", resourceID, "error", err)
		return nil, nil, errors.Wrap(err, "cannot clean up namespaces from cluster-scoped resources")
	}

	// Validate the resources
	if err := p.schemaValidator.ValidateResources(ctx, xrUnstructured, desired.ComposedResources); err != nil {
		p.config.Logger.Debug("Resource validation failed", "resource", resourceID, "error", err)
		return nil, nil, errors.Wrap(err, "cannot validate resources")
	}

	// Calculate diffs (without removal detection)
	p.config.Logger.Debug("Calculating diffs", "resource", resourceID)

	// Use the merged XR (input + rendered metadata) for diff calculation
	// This ensures Claims get the generated XR name and other metadata from rendering
	mergedXR := cmp.New()
	if err = runtime.DefaultUnstructuredConverter.FromUnstructured(xrUnstructured.UnstructuredContent(), mergedXR); err != nil {
		p.config.Logger.Debug("Failed to convert merged XR", "resource", resourceID, "error", err)
		return nil, nil, errors.Wrap(err, "cannot convert merged XR")
	}

	// Clean the merged XR for diff calculation - remove managed fields that can cause apply issues
	mergedXR.SetManagedFields(nil)
	mergedXR.SetResourceVersion("")

	// Convert parentXR to unstructured for the diff calculator
	var parentComposite *un.Unstructured
	if parentXR != nil {
		parentComposite = parentXR.GetUnstructured()
	}

	diffs, renderedResources, err := p.diffCalculator.CalculateNonRemovalDiffs(ctx, mergedXR, parentComposite, desired)
	if err != nil {
		// We don't fail completely if some diffs couldn't be calculated
		p.config.Logger.Debug("Partial error calculating diffs", "resource", resourceID, "error", err)
	}

	// Check for nested XRs in the composed resources and process them recursively
	p.config.Logger.Debug("Checking for nested XRs", "resource", resourceID, "composedCount", len(desired.ComposedResources))

	// Extract the existing XR from the cluster (if it exists) to pass to ProcessNestedXRs
	// This is needed because ProcessNestedXRs fetches observed resources using the parent XR's UID,
	// which is only available on the existing cluster XR, not the modified XR from the input file.
	var existingXR *cmp.Unstructured

	xrDiffKey := dt.MakeDiffKey(xr.GetAPIVersion(), xr.GetKind(), xr.GetName())
	if xrDiff, ok := diffs[xrDiffKey]; ok && xrDiff.Current != nil {
		// Convert from unstructured.Unstructured to composite.Unstructured
		existingXR = cmp.New()

		err := runtime.DefaultUnstructuredConverter.FromUnstructured(xrDiff.Current.Object, existingXR)
		if err != nil {
			p.config.Logger.Debug("Failed to convert existing XR to composite unstructured",
				"resource", resourceID,
				"error", err)
			// Continue with nil existingXR - ProcessNestedXRs will handle this case
		}
	}

	nestedDiffs, nestedRenderedResources, err := p.ProcessNestedXRs(ctx, desired.ComposedResources, compositionProvider, resourceID, existingXR, observedResources, 1)
	if err != nil {
		p.config.Logger.Debug("Error processing nested XRs", "resource", resourceID, "error", err)
		return nil, nil, errors.Wrap(err, "cannot process nested XRs")
	}

	p.config.Logger.Debug("Before merging nested resources",
		"resource", resourceID,
		"renderedResourcesCount", len(renderedResources),
		"nestedRenderedResourcesCount", len(nestedRenderedResources))

	// Merge nested diffs into our result
	maps.Copy(diffs, nestedDiffs)

	// Merge nested rendered resources into our tracking map
	// This ensures that resources from nested XRs (including unchanged ones) are not flagged as removed
	maps.Copy(renderedResources, nestedRenderedResources)

	p.config.Logger.Debug("After merging nested resources",
		"resource", resourceID,
		"renderedResourcesCount", len(renderedResources))

	// Now detect removals if requested (only for top-level XRs)
	// This must happen after nested XR processing to avoid false positives
	if detectRemovals && existingXR != nil {
		p.config.Logger.Debug("Detecting removed resources", "resource", resourceID, "renderedCount", len(renderedResources))

		removedDiffs, err := p.diffCalculator.CalculateRemovedResourceDiffs(ctx, existingXR.GetUnstructured(), renderedResources)
		if err != nil {
			p.config.Logger.Debug("Error detecting removed resources (continuing)", "resource", resourceID, "error", err)
		} else if len(removedDiffs) > 0 {
			maps.Copy(diffs, removedDiffs)
			p.config.Logger.Debug("Found removed resources", "resource", resourceID, "removedCount", len(removedDiffs))
		}
	}

	p.config.Logger.Debug("Resource processing complete",
		"resource", resourceID,
		"diffCount", len(diffs),
		"nestedDiffCount", len(nestedDiffs),
		"hasErrors", err != nil)

	return diffs, renderedResources, err
}

// findExistingNestedXR locates an existing nested XR in the observed resources by matching
// the composition-resource-name annotation and kind.
func findExistingNestedXR(nestedXR *un.Unstructured, observedResources []cpd.Unstructured) *un.Unstructured {
	compositionResourceName := nestedXR.GetAnnotations()["crossplane.io/composition-resource-name"]
	if compositionResourceName == "" {
		return nil
	}

	for _, obs := range observedResources {
		obsUnstructured := &un.Unstructured{Object: obs.UnstructuredContent()}
		obsCompResName := obsUnstructured.GetAnnotations()["crossplane.io/composition-resource-name"]

		// Match by composition-resource-name annotation and kind
		if obsCompResName == compositionResourceName && obsUnstructured.GetKind() == nestedXR.GetKind() {
			return obsUnstructured
		}
	}

	return nil
}

// preserveNestedXRIdentity updates the nested XR to preserve the identity of an existing XR
// by copying its name, generateName, UID, and composite label.
func preserveNestedXRIdentity(nestedXR, existingNestedXR *un.Unstructured) {
	// Preserve the actual cluster name and UID
	nestedXR.SetName(existingNestedXR.GetName())
	nestedXR.SetGenerateName(existingNestedXR.GetGenerateName())
	nestedXR.SetUID(existingNestedXR.GetUID())

	// Preserve Crossplane labels so child resources get matched correctly
	// These labels are needed for:
	// - crossplane.io/composite: matching composed resources to their owner
	// - crossplane.io/claim-name/namespace: detecting Claim context for label propagation
	if labels := existingNestedXR.GetLabels(); labels != nil {
		nestedXRLabels := nestedXR.GetLabels()
		if nestedXRLabels == nil {
			nestedXRLabels = make(map[string]string)
		}

		// Copy composite label
		if compositeLabel, exists := labels["crossplane.io/composite"]; exists {
			nestedXRLabels["crossplane.io/composite"] = compositeLabel
		}

		// Copy claim labels (needed to detect Claim context during nested XR processing)
		if claimName, exists := labels["crossplane.io/claim-name"]; exists {
			nestedXRLabels["crossplane.io/claim-name"] = claimName
		}
		if claimNamespace, exists := labels["crossplane.io/claim-namespace"]; exists {
			nestedXRLabels["crossplane.io/claim-namespace"] = claimNamespace
		}

		nestedXR.SetLabels(nestedXRLabels)
	}
}

// ProcessNestedXRs recursively processes composed resources that are themselves XRs.
// It checks each composed resource to see if it's an XR, and if so, processes it through
// its own composition pipeline to get the full tree of diffs. It preserves the identity
// of existing nested XRs to ensure accurate diff calculation.
// observedResources should contain the observed resources from the parent XR's resource tree.
func (p *DefaultDiffProcessor) ProcessNestedXRs(
	ctx context.Context,
	composedResources []cpd.Unstructured,
	compositionProvider types.CompositionProvider,
	parentResourceID string,
	parentXR *cmp.Unstructured,
	observedResources []cpd.Unstructured,
	depth int,
) (map[string]*dt.ResourceDiff, map[string]bool, error) {
	if depth > p.config.MaxNestedDepth {
		p.config.Logger.Debug("Maximum nesting depth exceeded",
			"parentResource", parentResourceID,
			"depth", depth,
			"maxDepth", p.config.MaxNestedDepth)

		return nil, nil, errors.New("maximum nesting depth exceeded")
	}

	p.config.Logger.Debug("Processing nested XRs",
		"parentResource", parentResourceID,
		"composedResourceCount", len(composedResources),
		"observedResourcesCount", len(observedResources),
		"depth", depth)

	allDiffs := make(map[string]*dt.ResourceDiff)
	allRenderedResources := make(map[string]bool)

	for _, composed := range composedResources {
		nestedXR := &un.Unstructured{Object: composed.UnstructuredContent()}

		// Check if this composed resource is itself an XR
		isXR, _ := p.getCompositeResourceXRD(ctx, nestedXR)

		if !isXR {
			// Skip non-XR resources
			continue
		}

		nestedResourceID := fmt.Sprintf("%s/%s (nested depth %d)", nestedXR.GetKind(), nestedXR.GetName(), depth)
		p.config.Logger.Debug("Found nested XR, processing recursively",
			"nestedXR", nestedResourceID,
			"parentXR", parentResourceID,
			"depth", depth)

		// Find the matching existing nested XR in observed resources (if it exists)
		// Match by composition-resource-name annotation to find the correct existing resource
		existingNestedXR := findExistingNestedXR(nestedXR, observedResources)

		// If not found in observedResources (e.g., tree client returned empty), try FetchCurrentObject
		// This handles cases where the tree client doesn't work (e.g., envtest) or the ownership model
		// doesn't allow tree traversal from intermediate XRs.
		if existingNestedXR == nil && parentXR != nil {
			p.config.Logger.Debug("Nested XR not found in observedResources, trying FetchCurrentObject",
				"nestedXR", nestedResourceID)

			// Use the composite label from the nested XR to build a lookup
			existingNestedXRUn, isNew, fetchErr := p.resourceManager.FetchCurrentObject(ctx, parentXR.GetUnstructured(), nestedXR)
			if fetchErr == nil && existingNestedXRUn != nil && !isNew {
				existingNestedXR = existingNestedXRUn
				p.config.Logger.Debug("Found existing nested XR via FetchCurrentObject",
					"nestedXR", nestedResourceID,
					"existingName", existingNestedXR.GetName())
			}
		}

		if existingNestedXR != nil {
			p.config.Logger.Debug("Found existing nested XR in cluster",
				"nestedXR", nestedResourceID,
				"existingName", existingNestedXR.GetName(),
				"compositionResourceName", nestedXR.GetAnnotations()["crossplane.io/composition-resource-name"])

			// Preserve its identity (name, composite label) so its managed resources can be matched correctly
			preserveNestedXRIdentity(nestedXR, existingNestedXR)

			p.config.Logger.Debug("Preserved nested XR identity",
				"nestedXR", nestedResourceID,
				"preservedName", nestedXR.GetName())
		}

		// Recursively process this nested XR
		// Pass parentXR so nested XR can have correct composite label
		// Use detectRemovals=false for nested XRs since they don't own their composed resources
		// (resources are owned by the top-level parent XR in Crossplane's ownership model)
		nestedDiffs, nestedRenderedResources, err := p.diffSingleResourceInternal(ctx, nestedXR, compositionProvider, parentXR, false)
		if err != nil {
			// Check if the error is due to missing composition
			// Note: It's valid to have an XRD in Crossplane without a composition attached to it.
			// In such cases, we skip recursive processing of the nested XR but allow the overall
			// diff operation to continue. The diff for the nested XR itself will still be shown.
			errMsg := err.Error()
			if strings.Contains(errMsg, "cannot get composition") && strings.Contains(errMsg, "no composition found") {
				p.config.Logger.Info("Skipping nested XR processing due to missing composition",
					"nestedXR", nestedResourceID,
					"parentXR", parentResourceID,
					"gvk", nestedXR.GroupVersionKind().String())
				// Continue processing other nested XRs
				continue
			}

			// For other errors, fail per Guiding Principles: "never silently continue in the face of failures"
			p.config.Logger.Debug("Error processing nested XR",
				"nestedXR", nestedResourceID,
				"parentXR", parentResourceID,
				"error", err)

			return nil, nil, errors.Wrapf(err, "cannot process nested XR %s", nestedResourceID)
		}

		// Merge diffs from nested XR
		maps.Copy(allDiffs, nestedDiffs)
		// Merge rendered resources from nested XR
		maps.Copy(allRenderedResources, nestedRenderedResources)

		p.config.Logger.Debug("Nested XR processed successfully",
			"nestedXR", nestedResourceID,
			"diffCount", len(nestedDiffs),
			"nestedRenderedResourcesCount", len(nestedRenderedResources),
			"allRenderedResourcesCount", len(allRenderedResources))
	}

	p.config.Logger.Debug("Finished processing nested XRs",
		"parentResource", parentResourceID,
		"totalNestedDiffs", len(allDiffs),
		"totalRenderedResourcesCount", len(allRenderedResources),
		"depth", depth)

	return allDiffs, allRenderedResources, nil
}

// SanitizeXR makes an XR into a valid unstructured object that we can use in a dry-run apply.
func (p *DefaultDiffProcessor) SanitizeXR(res *un.Unstructured, resourceID string) (*cmp.Unstructured, bool, error) {
	// Convert the unstructured resource to a composite unstructured for rendering
	xr := cmp.New()

	err := runtime.DefaultUnstructuredConverter.FromUnstructured(res.UnstructuredContent(), xr)
	if err != nil {
		p.config.Logger.Debug("Failed to convert resource", "resource", resourceID, "error", err)
		return nil, true, errors.Wrap(err, "cannot convert XR to composite unstructured")
	}

	// Handle XRs with generateName but no name
	if xr.GetName() == "" && xr.GetGenerateName() != "" {
		// Create a display name for the diff
		displayName := xr.GetGenerateName() + "(generated)"
		p.config.Logger.Debug("Setting display name for XR with generateName",
			"generateName", xr.GetGenerateName(),
			"displayName", displayName)

		// Set this display name on the XR for rendering
		xrCopy := xr.DeepCopy()
		xrCopy.SetName(displayName)
		xr = xrCopy
	}

	return xr, false, nil
}

// mergeUnstructured merges two unstructured objects.
func mergeUnstructured(dest *un.Unstructured, src *un.Unstructured) (*un.Unstructured, error) {
	// Start with a deep copy of the rendered resource
	result := dest.DeepCopy()

	// Save the rendered name before merging, in case it was generated (e.g., for Claims)
	renderedName := dest.GetName()

	err := mergo.Merge(&result.Object, src.Object, mergo.WithOverride)
	if err != nil {
		return nil, errors.Wrap(err, "cannot merge unstructured objects")
	}

	// WORKAROUND for https://github.com/crossplane/crossplane/issues/6782
	// Crossplane render strips namespace from XRs - restore it from the original
	if src.GetNamespace() != "" && result.GetNamespace() == "" {
		result.SetNamespace(src.GetNamespace())
	}

	// If the rendered XR had a generated name (different from input), preserve it
	// This is critical for Claims where the input has the Claim name but rendering
	// generates the XR name with a suffix (e.g., "my-claim-abc123")
	if renderedName != "" && renderedName != src.GetName() {
		result.SetName(renderedName)
	}

	return result, nil
}

// RenderWithRequirements performs an iterative rendering process that discovers and fulfills requirements.
func (p *DefaultDiffProcessor) RenderWithRequirements(
	ctx context.Context,
	xr *cmp.Unstructured,
	comp *apiextensionsv1.Composition,
	fns []pkgv1.Function,
	resourceID string,
	observedResources []cpd.Unstructured,
) (render.Outputs, error) {
	// Start with environment configs as baseline extra resources
	var renderResources []un.Unstructured

	// Track all discovered extra resources to return at the end
	var discoveredResources []*un.Unstructured

	// Track resources we've already discovered to detect when we're done
	discoveredResourcesMap := make(map[string]bool)

	// Set up for iterative discovery
	const maxIterations = 10 // Prevent infinite loops

	var (
		lastOutput    render.Outputs
		lastRenderErr error
	)

	// Track the number of iterations for logging
	iteration := 0

	// Iteratively discover and fetch resources until we have all requirements
	// or until we hit the max iterations limit
	for iteration < maxIterations {
		iteration++
		p.config.Logger.Debug("Performing render iteration to identify requirements",
			"resource", resourceID,
			"iteration", iteration,
			"resourceCount", len(renderResources))

		// Perform render to get requirements
		output, renderErr := p.config.RenderFunc(ctx, p.config.Logger, render.Inputs{
			CompositeResource: xr,
			Composition:       comp,
			Functions:         fns,
			RequiredResources: renderResources,
			ObservedResources: observedResources,
		})

		lastOutput = output
		lastRenderErr = renderErr

		// Handle the case where rendering failed but we still have requirements
		var hasRequirements bool

		// Use reflection to safely check if output is non-nil and has Requirements
		if v := reflect.ValueOf(output); v.IsValid() {
			// Check if it has a Requirements field
			if requirements := v.FieldByName("Requirements"); requirements.IsValid() && !requirements.IsNil() {
				hasRequirements = true
			}
		}

		// If we got an error and there are no usable requirements, fail
		if renderErr != nil && !hasRequirements {
			p.config.Logger.Debug("Resource rendering failed completely",
				"resource", resourceID,
				"iteration", iteration,
				"error", renderErr)

			return render.Outputs{}, errors.Wrap(renderErr, "cannot render resources")
		}

		// Log if we're continuing despite render errors
		if renderErr != nil { // && hasRequirements {
			p.config.Logger.Debug("Resource rendering had errors but returned requirements",
				"resource", resourceID,
				"iteration", iteration,
				"error", renderErr,
				"requirementCount", len(output.Requirements))
		}

		// If no requirements, we're done
		if len(output.Requirements) == 0 {
			p.config.Logger.Debug("No more requirements found, discovery complete",
				"iteration", iteration)

			break
		}

		// Process requirements from the render output
		p.config.Logger.Debug("Processing requirements from render output",
			"iteration", iteration,
			"requirementCount", len(output.Requirements))

		additionalResources, err := p.requirementsProvider.ProvideRequirements(ctx, output.Requirements, xr.GetNamespace())
		if err != nil {
			return render.Outputs{}, errors.Wrap(err, "failed to process requirements")
		}

		// If no new resources were found, we're done
		if len(additionalResources) == 0 {
			p.config.Logger.Debug("No new resources found from requirements, discovery complete",
				"iteration", iteration)

			break
		}

		// Check if we've already discovered these resources
		newResourcesFound := false

		for _, res := range additionalResources {
			resourceKey := fmt.Sprintf("%s/%s", res.GetAPIVersion(), res.GetName())
			if !discoveredResourcesMap[resourceKey] {
				discoveredResourcesMap[resourceKey] = true
				newResourcesFound = true

				// Add to our collection of extra resources
				discoveredResources = append(discoveredResources, res)

				// Add to render resources for next iteration
				renderResources = append(renderResources, *res)
			}
		}

		// If no new resources were found, we've reached a stable state
		if !newResourcesFound {
			p.config.Logger.Debug("No new unique resources found, discovery complete",
				"iteration", iteration)

			break
		}

		p.config.Logger.Debug("Found additional resources to incorporate",
			"resource", resourceID,
			"iteration", iteration,
			"additionalCount", len(additionalResources),
			"totalResourcesNow", len(discoveredResources))
	}

	// Log if we hit the iteration limit
	if iteration >= maxIterations {
		p.config.Logger.Info("Reached maximum iteration limit for resource discovery",
			"resource", resourceID,
			"maxIterations", maxIterations)
	}

	// If we exited the loop with a render error but still found resources,
	// log it but don't fail the process
	if lastRenderErr != nil {
		p.config.Logger.Debug("Completed resource discovery with render errors",
			"resource", resourceID,
			"iterations", iteration,
			"error", lastRenderErr)
	}

	p.config.Logger.Debug("Finished discovering and rendering resources",
		"totalExtraResources", len(discoveredResources),
		"iterations", iteration)

	return lastOutput, lastRenderErr
}

// removeNamespacesFromClusterScopedResources removes namespaces from cluster-scoped resources.
//
// TEMPORARY WORKAROUND: This function exists because Crossplane's render command blindly propagates
// namespaces from the XR to ALL composed resources without checking if they are cluster-scoped.
// This was introduced in PR #6812 (https://github.com/crossplane/crossplane/pull/6812) which fixed
// issue #6782 by adding namespace propagation to SetComposedResourceMetadata.
//
// UPSTREAM FIX NEEDED in github.com/crossplane/crossplane/v2:
// File: cmd/crank/render/render.go
// Function: SetComposedResourceMetadata (around line 445)
// Issue: Lines 455-457 blindly set cd.SetNamespace(xr.GetNamespace()) without checking resource scope
//
// Proposed Solution:
// 1. Extend render.Inputs to accept XRDs (similar to how RequiredResources is passed)
// 2. Pass XRDs through to SetComposedResourceMetadata (modify function signature)
// 3. Look up the composed resource's GVK in the XRDs to determine if it's cluster-scoped
// 4. Only call cd.SetNamespace(xr.GetNamespace()) if the resource is namespaced
//
// Example fix in SetComposedResourceMetadata:
//
//	if xr.GetNamespace() != "" {
//	    // Look up cd's GVK in XRDs to check scope
//	    if isNamespaced(cd.GetObjectKind().GroupVersionKind(), xrds) {
//	        cd.SetNamespace(xr.GetNamespace())
//	    }
//	}
//
// Once upstream is fixed, this function can be removed along with its call site at line 270.
//
// NOTE: render is an offline tool with no cluster access, so it needs XRDs passed explicitly.
// ExtraResources/RequiredResources are only available to composition functions, not to the
// core render logic, so a new mechanism is needed to pass schema information.
func (p *DefaultDiffProcessor) removeNamespacesFromClusterScopedResources(ctx context.Context, composedResources []cpd.Unstructured) error {
	for i := range composedResources {
		resource := &un.Unstructured{Object: composedResources[i].UnstructuredContent()}

		// Skip if resource has no namespace
		if resource.GetNamespace() == "" {
			continue
		}

		resourceID := fmt.Sprintf("%s/%s", resource.GetKind(), resource.GetName())
		if resource.GetName() == "" && resource.GetGenerateName() != "" {
			resourceID = fmt.Sprintf("%s/%s*", resource.GetKind(), resource.GetGenerateName())
		}

		// Check if resource is cluster-scoped
		// We must be able to determine scope to proceed - if we can't get the CRD,
		// validation will fail anyway, so fail fast with a clear error message.
		gvk := resource.GroupVersionKind()

		crd, err := p.schemaClient.GetCRD(ctx, gvk)
		if err != nil {
			return errors.Wrapf(err, "cannot determine scope for resource %s (GVK %s): CRD not found", resourceID, gvk.String())
		}

		if crd.Spec.Scope == "Cluster" {
			p.config.Logger.Debug("Removing namespace from cluster-scoped resource",
				"resource", resourceID,
				"gvk", gvk.String(),
				"namespace", resource.GetNamespace())
			resource.SetNamespace("")

			// Update the composed resource with the modified content
			composedResources[i].SetUnstructuredContent(resource.Object)
		}
	}

	return nil
}

// getCompositeResourceXRD checks if a resource is a Composite Resource (XR) by looking it up in XRDs.
// Returns true if the resource is an XR, along with its XRD.
// Returns false if it's not an XR or if there's an error (errors are logged but not returned).
func (p *DefaultDiffProcessor) getCompositeResourceXRD(ctx context.Context, resource *un.Unstructured) (bool, *un.Unstructured) {
	gvk := resource.GroupVersionKind()

	p.config.Logger.Debug("Checking if resource is a composite resource",
		"resource", fmt.Sprintf("%s/%s", resource.GetKind(), resource.GetName()),
		"gvk", gvk.String())

	// Check if there's an XRD that defines this GVK as an XR
	xrd, err := p.defClient.GetXRDForXR(ctx, gvk)
	if err == nil && xrd != nil {
		p.config.Logger.Debug("Resource is a composite resource (XR)",
			"resource", fmt.Sprintf("%s/%s", resource.GetKind(), resource.GetName()),
			"xrd", xrd.GetName())

		return true, xrd
	}

	// Check if there's an XRD that defines this GVK as a claim
	xrd, err = p.defClient.GetXRDForClaim(ctx, gvk)
	if err == nil && xrd != nil {
		p.config.Logger.Debug("Resource is a composite resource (Claim)",
			"resource", fmt.Sprintf("%s/%s", resource.GetKind(), resource.GetName()),
			"xrd", xrd.GetName())

		return true, xrd
	}

	// Not a composite resource
	p.config.Logger.Debug("Resource is not a composite resource",
		"resource", fmt.Sprintf("%s/%s", resource.GetKind(), resource.GetName()))

	return false, nil
}

// applyXRDDefaults applies default values from the XRD schema to the XR.
func (p *DefaultDiffProcessor) applyXRDDefaults(ctx context.Context, xr *cmp.Unstructured, resourceID string) error {
	p.config.Logger.Debug("Applying XRD defaults", "resource", resourceID)

	// Get the XR's GVK
	gvk := xr.GroupVersionKind()

	// Find the XRD that defines this XR
	var (
		xrd *un.Unstructured
		err error
	)

	// Check if this is a claim or an XR
	if p.defClient.IsClaimResource(ctx, xr.GetUnstructured()) {
		xrd, err = p.defClient.GetXRDForClaim(ctx, gvk)
	} else {
		xrd, err = p.defClient.GetXRDForXR(ctx, gvk)
	}

	if err != nil {
		return errors.Wrapf(err, "cannot find XRD for resource %s with GVK %s", resourceID, gvk.String())
	}

	// Get the CRD that corresponds to this XRD using the XRD name
	xrdName := xrd.GetName()

	p.config.Logger.Debug("Looking for CRD matching XRD in applyXRDDefaults", "resource", resourceID, "xrdName", xrdName)

	// Use the new GetCRDByName method to directly get the CRD
	crdForDefaults, err := p.schemaClient.GetCRDByName(xrdName)
	if err != nil {
		return errors.Wrapf(err, "cannot find CRD for XRD %s (resource %s)", xrdName, resourceID)
	}

	// Apply defaults using the render.DefaultValues function
	apiVersion := xr.GetAPIVersion()
	xrContent := xr.UnstructuredContent()

	p.config.Logger.Debug("Applying defaults to XR in applyXRDDefaults",
		"resource", resourceID,
		"apiVersion", apiVersion,
		"crdName", crdForDefaults.Name)

	err = render.DefaultValues(xrContent, apiVersion, *crdForDefaults)
	if err != nil {
		return errors.Wrapf(err, "cannot apply default values for XR %s", resourceID)
	}

	// Update the XR with the defaulted content
	xr.SetUnstructuredContent(xrContent)

	p.config.Logger.Debug("Successfully applied XRD defaults", "resource", resourceID)

	return nil
}
