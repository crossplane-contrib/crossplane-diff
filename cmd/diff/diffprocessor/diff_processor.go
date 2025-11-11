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

	// Initialize loads required resources like CRDs and environment configs
	Initialize(ctx context.Context) error
}

// DefaultDiffProcessor implements DiffProcessor with modular components.
type DefaultDiffProcessor struct {
	functionProvider     FunctionProvider
	compClient           xp.CompositionClient
	defClient            xp.DefinitionClient
	schemaClient         k8.SchemaClient
	config               ProcessorConfig
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
	resourceManager := config.Factories.ResourceManager(k8cs.Resource, xpcs.Definition, config.Logger)
	schemaValidator := config.Factories.SchemaValidator(k8cs.Schema, xpcs.Definition, config.Logger)
	requirementsProvider := config.Factories.RequirementsProvider(k8cs.Resource, xpcs.Environment, config.RenderFunc, config.Logger)
	diffCalculator := config.Factories.DiffCalculator(k8cs.Apply, xpcs.ResourceTree, resourceManager, config.Logger, diffOpts)
	diffRenderer := config.Factories.DiffRenderer(config.Logger, diffOpts)
	functionProvider := config.Factories.FunctionProvider(xpcs.Function, config.Logger)

	processor := &DefaultDiffProcessor{
		functionProvider:     functionProvider,
		compClient:           xpcs.Composition,
		defClient:            xpcs.Definition,
		schemaClient:         k8cs.Schema,
		config:               config,
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
func (p *DefaultDiffProcessor) DiffSingleResource(ctx context.Context, res *un.Unstructured, compositionProvider types.CompositionProvider) (map[string]*dt.ResourceDiff, error) {
	resourceID := fmt.Sprintf("%s/%s", res.GetKind(), res.GetName())
	p.config.Logger.Debug("Processing resource", "resource", resourceID)

	xr, done, err := p.SanitizeXR(res, resourceID)
	if done {
		return nil, err
	}

	// Get the composition using the provided function
	comp, err := compositionProvider(ctx, res)
	if err != nil {
		p.config.Logger.Debug("Failed to get composition", "resource", resourceID, "error", err)
		return nil, errors.Wrap(err, "cannot get composition")
	}

	p.config.Logger.Debug("Resource setup complete", "resource", resourceID, "composition", comp.GetName())

	// Get functions for rendering
	fns, err := p.functionProvider.GetFunctionsForComposition(comp)
	if err != nil {
		p.config.Logger.Debug("Failed to get functions", "resource", resourceID, "error", err)
		return nil, errors.Wrap(err, "cannot get functions for composition")
	}

	// Note: Serialization mutex prevents concurrent Docker operations.
	// In e2e tests, named Docker containers (via annotations) reuse containers across renders.

	// Apply XRD defaults before rendering
	err = p.applyXRDDefaults(ctx, xr, resourceID)
	if err != nil {
		p.config.Logger.Debug("Failed to apply XRD defaults", "resource", resourceID, "error", err)
		return nil, errors.Wrap(err, "cannot apply XRD defaults")
	}

	// Perform iterative rendering and requirements reconciliation
	desired, err := p.RenderWithRequirements(ctx, xr, comp, fns, resourceID)
	if err != nil {
		p.config.Logger.Debug("Resource rendering failed", "resource", resourceID, "error", err)
		return nil, errors.Wrap(err, "cannot render resources with requirements")
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
		return nil, errors.Wrap(err, "cannot merge input XR with result of rendered XR")
	}

	// Clean up namespaces from cluster-scoped resources
	// Crossplane PR #6812 fixed issue #6782 by making render propagate namespaces from XR to all
	// composed resources, but it doesn't check if resources are cluster-scoped. This cleanup
	// removes namespaces from cluster-scoped resources. See removeNamespacesFromClusterScopedResources
	// for details on the upstream fix needed.
	if err := p.removeNamespacesFromClusterScopedResources(ctx, desired.ComposedResources); err != nil {
		p.config.Logger.Debug("Failed to clean up namespaces from cluster-scoped resources", "resource", resourceID, "error", err)
		return nil, errors.Wrap(err, "cannot clean up namespaces from cluster-scoped resources")
	}

	// Validate the resources
	if err := p.schemaValidator.ValidateResources(ctx, xrUnstructured, desired.ComposedResources); err != nil {
		p.config.Logger.Debug("Resource validation failed", "resource", resourceID, "error", err)
		return nil, errors.Wrap(err, "cannot validate resources")
	}

	// Calculate diffs
	p.config.Logger.Debug("Calculating diffs", "resource", resourceID)

	// Clean the XR for diff calculation - remove managed fields that can cause apply issues
	cleanXR := xr.DeepCopy()
	cleanXR.SetManagedFields(nil)
	cleanXR.SetResourceVersion("")

	diffs, err := p.diffCalculator.CalculateDiffs(ctx, cleanXR, desired)
	if err != nil {
		// We don't fail completely if some diffs couldn't be calculated
		p.config.Logger.Debug("Partial error calculating diffs", "resource", resourceID, "error", err)
	}

	// Check for nested XRs in the composed resources and process them recursively
	p.config.Logger.Debug("Checking for nested XRs", "resource", resourceID, "composedCount", len(desired.ComposedResources))

	nestedDiffs, err := p.ProcessNestedXRs(ctx, desired.ComposedResources, compositionProvider, resourceID, 1)
	if err != nil {
		p.config.Logger.Debug("Error processing nested XRs", "resource", resourceID, "error", err)
		return nil, errors.Wrap(err, "cannot process nested XRs")
	}

	// Merge nested diffs into our result
	maps.Copy(diffs, nestedDiffs)

	p.config.Logger.Debug("Resource processing complete",
		"resource", resourceID,
		"diffCount", len(diffs),
		"nestedDiffCount", len(nestedDiffs),
		"hasErrors", err != nil)

	return diffs, err
}

// ProcessNestedXRs recursively processes composed resources that are themselves XRs.
// It checks each composed resource to see if it's an XR, and if so, processes it through
// its own composition pipeline to get the full tree of diffs.
func (p *DefaultDiffProcessor) ProcessNestedXRs(
	ctx context.Context,
	composedResources []cpd.Unstructured,
	compositionProvider types.CompositionProvider,
	parentResourceID string,
	depth int,
) (map[string]*dt.ResourceDiff, error) {
	if depth > p.config.MaxNestedDepth {
		p.config.Logger.Debug("Maximum nesting depth exceeded",
			"parentResource", parentResourceID,
			"depth", depth,
			"maxDepth", p.config.MaxNestedDepth)

		return nil, errors.New("maximum nesting depth exceeded")
	}

	p.config.Logger.Debug("Processing nested XRs",
		"parentResource", parentResourceID,
		"composedResourceCount", len(composedResources),
		"depth", depth)

	allDiffs := make(map[string]*dt.ResourceDiff)

	for _, composed := range composedResources {
		un := &un.Unstructured{Object: composed.UnstructuredContent()}

		// Check if this composed resource is itself an XR
		isXR, _ := p.getCompositeResourceXRD(ctx, un)

		if !isXR {
			// Skip non-XR resources
			continue
		}

		nestedResourceID := fmt.Sprintf("%s/%s (nested depth %d)", un.GetKind(), un.GetName(), depth)
		p.config.Logger.Debug("Found nested XR, processing recursively",
			"nestedXR", nestedResourceID,
			"parentXR", parentResourceID,
			"depth", depth)

		// Recursively process this nested XR
		nestedDiffs, err := p.DiffSingleResource(ctx, un, compositionProvider)
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
					"gvk", un.GroupVersionKind().String())
				// Continue processing other nested XRs
				continue
			}

			// For other errors, fail per Guiding Principles: "never silently continue in the face of failures"
			p.config.Logger.Debug("Error processing nested XR",
				"nestedXR", nestedResourceID,
				"parentXR", parentResourceID,
				"error", err)

			return nil, errors.Wrapf(err, "cannot process nested XR %s", nestedResourceID)
		}

		// Merge diffs from nested XR
		maps.Copy(allDiffs, nestedDiffs)

		p.config.Logger.Debug("Nested XR processed successfully",
			"nestedXR", nestedResourceID,
			"diffCount", len(nestedDiffs))
	}

	p.config.Logger.Debug("Finished processing nested XRs",
		"parentResource", parentResourceID,
		"totalNestedDiffs", len(allDiffs),
		"depth", depth)

	return allDiffs, nil
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

	err := mergo.Merge(&result.Object, src.Object, mergo.WithOverride)
	if err != nil {
		return nil, errors.Wrap(err, "cannot merge unstructured objects")
	}

	// WORKAROUND for https://github.com/crossplane/crossplane/issues/6782
	// Crossplane render strips namespace from XRs - restore it from the original
	if src.GetNamespace() != "" && result.GetNamespace() == "" {
		result.SetNamespace(src.GetNamespace())
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
