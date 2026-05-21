/*
Copyright 2025 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package diffprocessor

import (
	"context"
	"fmt"
	"os"
	"strings"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	dtypes "github.com/crossplane-contrib/crossplane-diff/cmd/diff/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
)

// XRDiffResult captures the result of processing a single XR against a composition.
type XRDiffResult struct {
	// Diffs contains the downstream resource diffs for this XR (keyed by resource ID).
	// Empty map means no changes detected.
	Diffs map[string]*dt.ResourceDiff
	// Error contains any error that occurred while processing this XR.
	// nil means processing was successful.
	Error error
}

// HasChanges returns true if this XR has downstream resource changes.
func (r *XRDiffResult) HasChanges() bool {
	// Count only non-equal diffs as "having changes".
	// The diffs map may contain DiffTypeEqual entries (e.g., XR stored for removal detection).
	for _, diff := range r.Diffs {
		if diff.DiffType != dt.DiffTypeEqual {
			return true
		}
	}

	return false
}

// HasError returns true if this XR encountered a processing error.
func (r *XRDiffResult) HasError() bool {
	return r.Error != nil
}

// CompDiffProcessor defines the interface for composition diffing.
type CompDiffProcessor interface {
	// DiffComposition processes composition changes and shows impact on existing XRs.
	// Returns (hasDiffs, error) where hasDiffs indicates if any differences were detected.
	// When `resources` is non-empty, impact analysis is restricted to the named composites:
	// each ref is resolved against every supplied composition's (XR GVK, claim GVK) pair via a
	// preflight pass. If any ref is relevant to no supplied composition, the call fails before
	// rendering any diffs (CLI input error). When `resources` is empty, behavior is unchanged.
	DiffComposition(ctx context.Context, compositions []*un.Unstructured, namespace string, resources []dtypes.ResourceRef) (bool, error)
	Initialize(ctx context.Context) error
	// Cleanup releases any resources held by the processor (e.g., Docker containers).
	Cleanup(ctx context.Context) error
}

// DefaultCompDiffProcessor implements CompDiffProcessor.
type DefaultCompDiffProcessor struct {
	compositionClient xp.CompositionClient
	config            ProcessorConfig
	xrProc            DiffProcessor
	compDiffRenderer  renderer.CompDiffRenderer
}

// NewCompDiffProcessor creates a new DefaultCompDiffProcessor.
func NewCompDiffProcessor(xrProc DiffProcessor, compositionClient xp.CompositionClient, opts ...ProcessorOption) CompDiffProcessor {
	// Create default configuration
	config := ProcessorConfig{
		Namespace: "",
		Colorize:  true,
		Compact:   false,
		Stderr:    os.Stderr,
		Logger:    logging.NewNopLogger(),
	}

	// Apply all provided options
	for _, option := range opts {
		option(&config)
	}

	// NOTE: We intentionally do NOT default config.RenderFunc here.
	//
	// WithRenderFunc is operation-scoped: the same opts slice flows into
	// NewDiffProcessor for our embedded xrProc, so a caller-supplied renderer is
	// already honored at the layer that actually drives rendering. Comp's copy of
	// config.RenderFunc just rides along unused, which is fine.
	//
	// What we must NOT do is *default* one here. NewEngineRenderFn allocates a
	// Docker bridge network and reserves function-runtime addresses on first use,
	// and DefaultCompDiffProcessor.Cleanup delegates to xrProc.Cleanup — it has no
	// hook for tearing down a comp-side engineRenderFn. Defaulting one would leak
	// the network for the lifetime of the process. The xr processor handles its
	// own default + cleanup in NewDiffProcessor, so comp piggybacking on
	// xrProc.Render is the correct (and only) path.

	// Set default factories if not provided
	config.SetDefaultFactories()

	// Create diff renderer first (needed by DefaultCompDiffRenderer for human-readable output)
	diffOpts := config.GetDiffOptions()
	diffRenderer := config.Factories.DiffRenderer(config.Logger, diffOpts)

	// Create comp diff renderer using factory
	compDiffRenderer := config.Factories.CompDiffRenderer(config.Logger, diffRenderer, diffOpts)

	return &DefaultCompDiffProcessor{
		compositionClient: compositionClient,
		config:            config,
		xrProc:            xrProc,
		compDiffRenderer:  compDiffRenderer,
	}
}

// Initialize loads required resources.
func (p *DefaultCompDiffProcessor) Initialize(ctx context.Context) error {
	p.config.Logger.Debug("Initializing composition diff processor")

	// Initialize the injected XR processor
	if err := p.xrProc.Initialize(ctx); err != nil {
		return errors.Wrap(err, "cannot initialize XR diff processor")
	}

	p.config.Logger.Debug("Composition diff processor initialized")

	return nil
}

// Cleanup releases any resources held by the processor.
// Delegates to the underlying XR processor for cleanup.
func (p *DefaultCompDiffProcessor) Cleanup(ctx context.Context) error {
	return p.xrProc.Cleanup(ctx)
}

// DiffComposition processes composition changes and shows impact on existing XRs.
// Returns (hasDiffs, error) where hasDiffs indicates if any differences were detected.
func (p *DefaultCompDiffProcessor) DiffComposition(ctx context.Context, compositions []*un.Unstructured, namespace string, resources []dtypes.ResourceRef) (bool, error) {
	p.config.Logger.Debug("Processing composition diff",
		"compositionCount", len(compositions),
		"namespace", namespace,
		"resourceCount", len(resources))

	if len(compositions) == 0 {
		return false, errors.New("no compositions provided")
	}

	// When --resource is set, run a preflight pass that resolves every ref against every supplied
	// composition. If any ref is relevant to no supplied composition, fail loudly BEFORE rendering
	// any diffs (this is a CLI input error, not a downstream processing failure).
	preflightMatches, err := p.preflightResourceRefs(ctx, compositions, resources)
	if err != nil {
		return false, err
	}

	output := &renderer.CompDiffOutput{
		Compositions: make([]renderer.CompositionDiff, 0, len(compositions)),
		Errors:       []dt.OutputError{},
	}

	var compositionErrors int

	hasDiffs := false

	// Process each composition, filtering out non-Composition objects
	for _, comp := range compositions {
		// Skip non-Composition objects (e.g., GoTemplate objects extracted from pipeline steps)
		if comp.GetKind() != "Composition" {
			p.config.Logger.Debug("Skipping non-Composition object", "kind", comp.GetKind(), "apiVersion", comp.GetAPIVersion())
			continue
		}

		compositionID := comp.GetName() // Use actual name from unstructured
		p.config.Logger.Debug("Processing composition", "name", compositionID)

		// In --resource mode, hand the per-composition matched set into processSingleComposition;
		// nil signals default-discovery mode.
		var preMatched []*un.Unstructured
		if len(resources) > 0 {
			preMatched = preflightMatches[compositionID]
		}

		// Process this single composition and build the result
		compResult, err := p.processSingleComposition(ctx, comp, namespace, preMatched, len(resources) > 0)
		if err != nil {
			p.config.Logger.Debug("Failed to process composition", "composition", compositionID, "error", err)

			compositionErrors++

			// Include failed composition with error instead of skipping
			output.Compositions = append(output.Compositions, renderer.CompositionDiff{
				Name:  compositionID,
				Error: err,
				AffectedResources: renderer.AffectedResourcesSummary{
					Total:       0,
					WithChanges: 0,
					Unchanged:   0,
					WithErrors:  0,
				},
				ImpactAnalysis: []renderer.XRImpact{},
			})
		} else {
			if compResult.HasChanges() {
				hasDiffs = true
			}

			output.Compositions = append(output.Compositions, *compResult)
		}
	}

	// Collect XR errors with their resource IDs for top-level errors
	for _, comp := range output.Compositions {
		for _, impact := range comp.ImpactAnalysis {
			if impact.Status == renderer.XRStatusError && impact.Error != nil {
				resourceID := fmt.Sprintf("%s/%s", impact.Kind, impact.Name)
				output.Errors = append(output.Errors, dt.OutputError{
					ResourceID: resourceID,
					Message:    impact.Error.Error(),
				})
			}
		}
	}

	// Always render output (even if all compositions failed) to ensure valid structured output
	// The renderer will include errors in the structured output and write them to stderr
	if err := p.compDiffRenderer.RenderCompDiff(output); err != nil {
		return hasDiffs, errors.Wrap(err, "failed to render composition diff")
	}

	// Check for XR processing errors after rendering (so users see the output first).
	// Return an error so CI/CD pipelines get a non-zero exit code when impact analysis failed.
	totalXRErrors := len(output.Errors)

	if totalXRErrors > 0 {
		return hasDiffs, errors.Errorf("impact analysis failed for %d XR(s)", totalXRErrors)
	}

	// Return error if all compositions failed
	if compositionErrors > 0 && compositionErrors == len(output.Compositions) {
		return hasDiffs, errors.New("failed to process all compositions")
	}

	return hasDiffs, nil
}

// preflightResourceRefs resolves user --resource refs against every supplied composition before
// any rendering happens. Returns the per-composition matched set keyed by composition name.
// If any ref is relevant to no supplied composition, it returns an error naming the unmatched
// refs (CLI input error). When `refs` is empty, returns (nil, nil) and the caller falls back to
// default-discovery mode.
func (p *DefaultCompDiffProcessor) preflightResourceRefs(ctx context.Context, compositions []*un.Unstructured, refs []dtypes.ResourceRef) (map[string][]*un.Unstructured, error) {
	if len(refs) == 0 {
		return nil, nil
	}

	perComp := make(map[string][]*un.Unstructured, len(compositions))
	matchedAtLeastOnce := make(map[string]bool, len(refs))

	for _, comp := range compositions {
		if comp.GetKind() != "Composition" {
			continue
		}

		typedComp := &apiextensionsv1.Composition{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(comp.Object, typedComp); err != nil {
			return nil, errors.Wrapf(err, "cannot convert composition %s to typed for preflight", comp.GetName())
		}

		matched, _, err := p.compositionClient.GetCompositesByName(ctx, typedComp, refs)
		if err != nil {
			return nil, errors.Wrapf(err, "preflight: cannot resolve --resource refs for composition %s", comp.GetName())
		}

		perComp[comp.GetName()] = matched

		// A ref is matched globally if any composition's matched-set contains a composite whose
		// (namespace, name) equals the ref.
		for _, m := range matched {
			for _, ref := range refs {
				if m.GetName() == ref.Name && m.GetNamespace() == ref.Namespace {
					matchedAtLeastOnce[ref.String()] = true
				}
			}
		}
	}

	var globallyUnmatched []dtypes.ResourceRef

	for _, ref := range refs {
		if !matchedAtLeastOnce[ref.String()] {
			globallyUnmatched = append(globallyUnmatched, ref)
		}
	}

	if len(globallyUnmatched) > 0 {
		names := make([]string, 0, len(globallyUnmatched))
		for _, r := range globallyUnmatched {
			names = append(names, r.String())
		}

		return nil, errors.Errorf("--resource ref(s) not relevant to any supplied composition: %s (resource not found, or it doesn't reference one of the supplied compositions)", joinRefs(names))
	}

	return perComp, nil
}

// joinRefs renders a list of human-readable refs joined by commas.
func joinRefs(refs []string) string {
	return strings.Join(refs, ", ")
}

// processSingleComposition processes a single composition and builds the result.
// Returns (*CompositionDiff, error). When `resourceMode` is true, the function uses the
// caller-supplied `preMatched` set instead of calling FindCompositesUsingComposition, and
// surfaces update-policy-filtered composites in ImpactAnalysis with XRStatusFilteredByPolicy.
func (p *DefaultCompDiffProcessor) processSingleComposition(ctx context.Context, newComp *un.Unstructured, namespace string, preMatched []*un.Unstructured, resourceMode bool) (*renderer.CompositionDiff, error) {
	result := &renderer.CompositionDiff{
		Name:           newComp.GetName(),
		ImpactAnalysis: []renderer.XRImpact{},
		AffectedResources: renderer.AffectedResourcesSummary{
			Total:            0,
			WithChanges:      0,
			Unchanged:        0,
			WithErrors:       0,
			FilteredByPolicy: 0,
		},
	}

	// First, calculate the composition diff itself
	compDiff, err := p.calculateCompositionDiff(ctx, newComp)
	if err != nil {
		return nil, errors.Wrap(err, "cannot calculate composition diff")
	}

	result.CompositionDiff = compDiff

	// Resolve the affected composite set. In --resource mode, the preflight pass already produced it.
	// In default-discovery mode, query the cluster.
	var affectedXRs []*un.Unstructured

	if resourceMode {
		affectedXRs = preMatched
	} else {
		discovered, err := p.compositionClient.FindCompositesUsingComposition(ctx, newComp.GetName(), namespace)
		if err != nil {
			// For net-new compositions, the composition won't exist in the cluster
			// so FindCompositesUsingComposition will fail. This is expected behavior.
			p.config.Logger.Debug("Cannot find composites using composition (likely net-new composition)",
				"composition", newComp.GetName(), "error", err)
			// Return result with empty impact analysis for net-new compositions
			return result, nil
		}

		affectedXRs = discovered
	}

	p.config.Logger.Debug("Found affected XRs", "composition", newComp.GetName(), "count", len(affectedXRs), "resourceMode", resourceMode)

	// Filter XRs based on IncludeManual flag
	keptXRs, droppedXRs := p.partitionXRsByUpdatePolicy(affectedXRs)
	filteredByPolicy := len(droppedXRs)

	p.config.Logger.Debug("Filtered XRs by update policy",
		"composition", newComp.GetName(),
		"originalCount", len(affectedXRs),
		"keptCount", len(keptXRs),
		"droppedCount", filteredByPolicy,
		"includeManual", p.config.IncludeManual)

	// In --resource mode, surface filtered composites in the impact analysis as
	// XRStatusFilteredByPolicy so users see what was matched-but-skipped. In default-discovery
	// mode, preserve the existing summary-only behavior.
	if resourceMode {
		for _, xr := range droppedXRs {
			result.ImpactAnalysis = append(result.ImpactAnalysis, renderer.XRImpact{
				ObjectReference: corev1.ObjectReference{
					APIVersion: xr.GetAPIVersion(),
					Kind:       xr.GetKind(),
					Name:       xr.GetName(),
					Namespace:  xr.GetNamespace(),
				},
				Status: renderer.XRStatusFilteredByPolicy,
			})
		}
	}

	if len(keptXRs) == 0 {
		// All XRs were filtered by policy
		result.AffectedResources.Total = len(affectedXRs)
		result.AffectedResources.FilteredByPolicy = filteredByPolicy

		return result, nil
	}

	// Process kept XRs and collect diffs to determine which ones have changes
	p.config.Logger.Debug("Processing XRs to collect diff information", "count", len(keptXRs))

	xrResults := p.collectXRDiffs(ctx, keptXRs, newComp)

	// Build impact analysis and counts from results for the kept set, then merge in any
	// already-appended filtered-by-policy entries.
	keptImpacts, keptSummary := p.buildImpactAnalysis(keptXRs, xrResults)
	result.ImpactAnalysis = append(result.ImpactAnalysis, keptImpacts...)
	// keptSummary.Total counts only kept; widen to include filtered so totals stay consistent.
	keptSummary.Total = len(affectedXRs)
	keptSummary.FilteredByPolicy = filteredByPolicy
	result.AffectedResources = keptSummary

	return result, nil
}

// collectXRDiffs processes XRs and collects their diffs, returning results for each XR.
func (p *DefaultCompDiffProcessor) collectXRDiffs(ctx context.Context, xrs []*un.Unstructured, newComp *un.Unstructured) map[string]*XRDiffResult {
	// Convert the CLI composition to typed once for reuse
	cliComp := &apiextensionsv1.Composition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(newComp.Object, cliComp); err != nil {
		// If we can't convert, return an error result for all XRs
		results := make(map[string]*XRDiffResult)

		for _, xr := range xrs {
			resourceID := dt.MakeDiffKeyFromResource(xr)
			results[resourceID] = &XRDiffResult{
				Diffs: make(map[string]*dt.ResourceDiff),
				Error: errors.Wrap(err, "cannot convert CLI composition to typed"),
			}
		}

		return results
	}

	// Extract the target GVK from the CLI composition's compositeTypeRef
	cliCompTargetAPIVersion := cliComp.Spec.CompositeTypeRef.APIVersion
	cliCompTargetKind := cliComp.Spec.CompositeTypeRef.Kind

	p.config.Logger.Debug("CLI composition targets",
		"apiVersion", cliCompTargetAPIVersion,
		"kind", cliCompTargetKind)

	// Build a set of root-level resource keys (apiVersion/kind/namespace/name) for quick lookup.
	// Root-level resources are XRs and Claims found by FindCompositesUsingComposition
	// that use the CLI composition. These should always use the CLI composition.
	// We include namespace to avoid collisions between resources with the same name
	// in different namespaces (e.g., two claims with the same name).
	rootResourceKeys := make(map[string]bool)

	for _, xr := range xrs {
		key := dt.MakeDiffKeyFromResource(xr)
		rootResourceKeys[key] = true
	}

	// Composition provider that returns CLI composition for:
	// 1. Root-level resources (XRs and Claims that use this composition)
	// 2. XRs whose type matches the CLI composition's compositeTypeRef
	//
	// For nested XRs with different types, looks up from the cluster.
	compositionProvider := func(ctx context.Context, res *un.Unstructured) (*apiextensionsv1.Composition, error) {
		resGVK := res.GroupVersionKind()
		resAPIVersion := resGVK.GroupVersion().String()
		resKind := resGVK.Kind
		resourceID := fmt.Sprintf("%s/%s", res.GetKind(), res.GetName())

		// Check 1: Is this a root-level resource (XR or Claim found by FindCompositesUsingComposition)?
		// Root-level resources always use the CLI composition, even claims whose GVK differs from the XR type.
		key := dt.MakeDiffKeyFromResource(res)
		if rootResourceKeys[key] {
			p.config.Logger.Debug("Resource is root-level (uses this composition), using CLI composition",
				"resource", resourceID,
				"composition", cliComp.GetName())

			return cliComp, nil
		}

		// Check 2: Does this resource's type match the CLI composition's target type?
		// This handles XRs encountered during rendering that match the composition's type.
		if resAPIVersion == cliCompTargetAPIVersion && resKind == cliCompTargetKind {
			p.config.Logger.Debug("Resource matches CLI composition type, using CLI composition",
				"resource", resourceID,
				"composition", cliComp.GetName())

			return cliComp, nil
		}

		// This is a nested XR with a different type - look up its composition from the cluster
		p.config.Logger.Debug("Resource does not match CLI composition type, looking up from cluster",
			"resource", resourceID,
			"resourceAPIVersion", resAPIVersion,
			"resourceKind", resKind,
			"cliCompTargetAPIVersion", cliCompTargetAPIVersion,
			"cliCompTargetKind", cliCompTargetKind)

		return p.compositionClient.FindMatchingComposition(ctx, res)
	}

	results := make(map[string]*XRDiffResult)

	for _, xr := range xrs {
		resourceID := dt.MakeDiffKeyFromResource(xr)

		diffs, err := p.xrProc.DiffSingleResource(ctx, xr, compositionProvider)
		if err != nil {
			p.config.Logger.Debug("Failed to process resource", "resource", resourceID, "error", err)

			// Store the error in the result
			results[resourceID] = &XRDiffResult{
				Diffs: make(map[string]*dt.ResourceDiff),
				Error: errors.Wrapf(err, "unable to process resource %s", resourceID),
			}
		} else {
			// Store successful result with diffs
			results[resourceID] = &XRDiffResult{
				Diffs: diffs,
				Error: nil,
			}
		}
	}

	return results
}

// calculateCompositionDiff calculates the diff between the cluster composition and the file composition.
// Returns the ResourceDiff (nil if no changes) and any error.
func (p *DefaultCompDiffProcessor) calculateCompositionDiff(ctx context.Context, newComp *un.Unstructured) (*dt.ResourceDiff, error) {
	p.config.Logger.Debug("Calculating composition diff", "composition", newComp.GetName())

	var originalCompUnstructured *un.Unstructured

	// Get the original composition from the cluster
	originalComp, err := p.compositionClient.GetComposition(ctx, newComp.GetName())
	if err != nil {
		p.config.Logger.Debug("Original composition not found in cluster, treating as new composition",
			"composition", newComp.GetName(), "error", err)
		// originalCompUnstructured remains nil for new compositions
	} else {
		p.config.Logger.Debug("Retrieved original composition from cluster", "name", originalComp.GetName(), "composition", originalComp)

		// Convert original composition to unstructured for comparison
		unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(originalComp)
		if err != nil {
			return nil, errors.Wrap(err, "cannot convert original composition to unstructured")
		}

		originalCompUnstructured = &un.Unstructured{Object: unstructuredObj}
	}

	newCompUnstructured := newComp

	// Clean up managed fields and other cluster metadata before diff calculation
	cleanupClusterMetadata := func(obj *un.Unstructured) {
		if obj == nil {
			return
		}

		obj.SetManagedFields(nil)
		obj.SetResourceVersion("")
		obj.SetUID("")
		obj.SetGeneration(0)
		obj.SetCreationTimestamp(metav1.Time{})
	}

	cleanupClusterMetadata(originalCompUnstructured)
	cleanupClusterMetadata(newCompUnstructured)

	// Calculate the composition diff directly without dry-run apply
	// (compositions are static YAML documents that don't need server-side processing)
	diffOptions := renderer.DefaultDiffOptions()
	diffOptions.UseColors = p.config.Colorize
	diffOptions.Compact = p.config.Compact
	diffOptions.IgnorePaths = p.config.IgnorePaths

	compDiff, err := renderer.GenerateDiffWithOptions(ctx, originalCompUnstructured, newCompUnstructured, p.config.Logger, diffOptions)
	if err != nil {
		return nil, errors.Wrap(err, "cannot calculate composition diff")
	}

	p.config.Logger.Debug("Calculated composition diff",
		"composition", newComp.GetName(),
		"hasChanges", compDiff != nil,
		"isNewComposition", originalCompUnstructured == nil)

	// Return nil if no changes
	if compDiff.DiffType == dt.DiffTypeEqual {
		p.config.Logger.Info("No changes detected in composition", "composition", newComp.GetName())
		return nil, nil
	}

	return compDiff, nil
}

// partitionXRsByUpdatePolicy splits XRs into a kept set (Automatic policy or default) and a
// dropped set (Manual policy). When IncludeManual is true, all XRs are kept.
func (p *DefaultCompDiffProcessor) partitionXRsByUpdatePolicy(xrs []*un.Unstructured) (kept, dropped []*un.Unstructured) {
	if p.config.IncludeManual {
		return xrs, nil
	}

	for _, xr := range xrs {
		policy := p.getCompositionUpdatePolicy(xr)

		p.config.Logger.Debug("Checking XR update policy",
			"xr", xr.GetName(),
			"kind", xr.GetKind(),
			"policy", policy)

		switch {
		case policy == compositionUpdatePolicyManual:
			dropped = append(dropped, xr)
		default:
			// Automatic or empty/default policy — keep.
			kept = append(kept, xr)
		}
	}

	return kept, dropped
}

// getCompositionUpdatePolicy retrieves the compositionUpdatePolicy from an XR.
// It checks both v2 (spec.crossplane.compositionUpdatePolicy) and v1 (spec.compositionUpdatePolicy) field paths.
// Returns "Automatic" as the default if not found (matching Crossplane behavior).
func (p *DefaultCompDiffProcessor) getCompositionUpdatePolicy(xr *un.Unstructured) string {
	// Try v2 path first: spec.crossplane.compositionUpdatePolicy
	policy, found, err := un.NestedString(xr.Object, "spec", "crossplane", "compositionUpdatePolicy")
	if err == nil && found && policy != "" {
		return policy
	}

	// Try v1 path: spec.compositionUpdatePolicy
	policy, found, err = un.NestedString(xr.Object, "spec", "compositionUpdatePolicy")
	if err == nil && found && policy != "" {
		return policy
	}

	// Default to Automatic if not found (matching Crossplane default behavior)
	return compositionUpdatePolicyAutomatic
}

// buildImpactAnalysis builds the impact analysis and summary from XR results.
func (p *DefaultCompDiffProcessor) buildImpactAnalysis(xrs []*un.Unstructured, results map[string]*XRDiffResult) ([]renderer.XRImpact, renderer.AffectedResourcesSummary) {
	impacts := make([]renderer.XRImpact, 0, len(xrs))
	summary := renderer.AffectedResourcesSummary{
		Total: len(xrs),
	}

	for _, xr := range xrs {
		resourceID := dt.MakeDiffKeyFromResource(xr)
		result := results[resourceID]

		impact := renderer.XRImpact{
			ObjectReference: corev1.ObjectReference{
				APIVersion: xr.GetAPIVersion(),
				Kind:       xr.GetKind(),
				Name:       xr.GetName(),
				Namespace:  xr.GetNamespace(),
			},
		}

		switch {
		case result != nil && result.HasError():
			impact.Status = renderer.XRStatusError
			impact.Error = result.Error
			summary.WithErrors++
		case result != nil && result.HasChanges():
			impact.Status = renderer.XRStatusChanged
			impact.Diffs = result.Diffs
			summary.WithChanges++
		default:
			impact.Status = renderer.XRStatusUnchanged
			summary.Unchanged++
		}

		impacts = append(impacts, impact)
	}

	return impacts, summary
}
