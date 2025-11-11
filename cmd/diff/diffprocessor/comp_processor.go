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
	"io"
	"strings"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/v2/cmd/crank/render"
)

// CompDiffProcessor defines the interface for composition diffing.
type CompDiffProcessor interface {
	DiffComposition(ctx context.Context, stdout io.Writer, compositions []*un.Unstructured, namespace string) error
	Initialize(ctx context.Context) error
}

// DefaultCompDiffProcessor implements CompDiffProcessor.
type DefaultCompDiffProcessor struct {
	compositionClient xp.CompositionClient
	fnClient          xp.FunctionClient
	k8Clients         k8.Clients
	xpClients         xp.Clients
	config            ProcessorConfig
	processorOpts     []ProcessorOption
}

// NewCompDiffProcessor creates a new DefaultCompDiffProcessor.
func NewCompDiffProcessor(k8cs k8.Clients, xpcs xp.Clients, opts ...ProcessorOption) CompDiffProcessor {
	// Create default configuration
	config := ProcessorConfig{
		Namespace:  "",
		Colorize:   true,
		Compact:    false,
		Logger:     logging.NewNopLogger(),
		RenderFunc: render.Render,
	}

	// Apply all provided options
	for _, option := range opts {
		option(&config)
	}

	return &DefaultCompDiffProcessor{
		compositionClient: xpcs.Composition,
		fnClient:          xpcs.Function,
		k8Clients:         k8cs,
		xpClients:         xpcs,
		config:            config,
		processorOpts:     opts,
	}
}

// Initialize loads required resources.
func (p *DefaultCompDiffProcessor) Initialize(_ context.Context) error {
	p.config.Logger.Debug("Initializing composition diff processor")
	p.config.Logger.Debug("Composition diff processor initialized")

	return nil
}

// DiffComposition processes composition changes and shows impact on existing XRs.
func (p *DefaultCompDiffProcessor) DiffComposition(ctx context.Context, stdout io.Writer, compositions []*un.Unstructured, namespace string) error {
	p.config.Logger.Debug("Processing composition diff", "compositionCount", len(compositions), "namespace", namespace)

	if len(compositions) == 0 {
		return errors.New("no compositions provided")
	}

	var errs []error

	// Process each composition, filtering out non-Composition objects
	for i, comp := range compositions {
		// Skip non-Composition objects (e.g., GoTemplate objects extracted from pipeline steps)
		if comp.GetKind() != "Composition" {
			p.config.Logger.Debug("Skipping non-Composition object", "kind", comp.GetKind(), "apiVersion", comp.GetAPIVersion())
			continue
		}

		compositionID := comp.GetName() // Use actual name from unstructured
		p.config.Logger.Debug("Processing composition", "name", compositionID)

		// Process this single composition
		if err := p.processSingleComposition(ctx, stdout, comp, namespace); err != nil {
			p.config.Logger.Debug("Failed to process composition", "composition", compositionID, "error", err)
			errs = append(errs, errors.Wrapf(err, "cannot process composition %s", compositionID))

			continue
		}

		// Add separator between compositions if processing multiple
		if len(compositions) > 1 && i < len(compositions)-1 {
			separator := "\n" + strings.Repeat("=", 80) + "\n\n"
			if _, err := fmt.Fprint(stdout, separator); err != nil {
				return errors.Wrap(err, "cannot write composition separator")
			}
		}
	}

	// Handle any errors that occurred during processing
	if len(errs) > 0 {
		if len(errs) == len(compositions) {
			// All compositions failed - this is a complete failure
			return errors.New("failed to process all compositions")
		}
		// Some compositions failed - log the errors but don't fail completely
		p.config.Logger.Info("Some compositions failed to process", "failedCount", len(errs), "totalCount", len(compositions))

		for _, err := range errs {
			p.config.Logger.Debug("Composition processing error", "error", err)
		}
	}

	return nil
}

// processSingleComposition processes a single composition and shows its impact on existing XRs.
func (p *DefaultCompDiffProcessor) processSingleComposition(ctx context.Context, stdout io.Writer, newComp *un.Unstructured, namespace string) error {
	// First, show the composition diff itself
	if err := p.displayCompositionDiff(ctx, stdout, newComp); err != nil {
		return errors.Wrap(err, "cannot display composition diff")
	}

	// Find all XRs that use this composition
	affectedXRs, err := p.compositionClient.FindXRsUsingComposition(ctx, newComp.GetName(), namespace)
	if err != nil {
		// For net-new compositions, the composition won't exist in the cluster
		// so findXRsUsingComposition will fail. This is expected behavior.
		p.config.Logger.Debug("Cannot find XRs using composition (likely net-new composition)",
			"composition", newComp.GetName(), "error", err)

		// Display the "no XRs found" message for net-new compositions
		if _, err := fmt.Fprintf(stdout, "No XRs found using composition %s\n", newComp.GetName()); err != nil {
			return errors.Wrap(err, "cannot write no XRs message")
		}

		return nil
	}

	p.config.Logger.Debug("Found affected XRs", "composition", newComp.GetName(), "count", len(affectedXRs))

	// Filter XRs based on IncludeManual flag
	filteredXRs := p.filterXRsByUpdatePolicy(affectedXRs)

	p.config.Logger.Debug("Filtered XRs by update policy",
		"composition", newComp.GetName(),
		"originalCount", len(affectedXRs),
		"filteredCount", len(filteredXRs),
		"includeManual", p.config.IncludeManual)

	if len(filteredXRs) == 0 {
		if !p.config.IncludeManual && len(affectedXRs) > 0 {
			// XRs exist but were filtered out due to Manual policy
			p.config.Logger.Info("All XRs using composition have Manual update policy (use --include-manual to see them)",
				"composition", newComp.GetName(),
				"filteredCount", len(affectedXRs))

			if _, err := fmt.Fprintf(stdout, "All %d XR(s) using composition %s have Manual update policy (use --include-manual to see them)\n",
				len(affectedXRs), newComp.GetName()); err != nil {
				return errors.Wrap(err, "cannot write filtered XRs message")
			}
		} else {
			// No XRs found at all
			p.config.Logger.Info("No XRs found using composition", "composition", newComp.GetName())

			if _, err := fmt.Fprintf(stdout, "No XRs found using composition %s\n", newComp.GetName()); err != nil {
				return errors.Wrap(err, "cannot write no XRs message")
			}
		}

		return nil
	}

	// Use filtered XRs for the rest of the processing
	affectedXRs = filteredXRs

	// Process affected XRs using the existing XR processor with composition override
	// List the affected XRs so users can understand the scope of impact
	var xrList strings.Builder

	for _, xr := range affectedXRs {
		// Format namespace/scope information
		scope := fmt.Sprintf("namespace: %s", xr.GetNamespace())
		if xr.GetNamespace() == "" {
			scope = "cluster-scoped"
		}

		xrList.WriteString(fmt.Sprintf("- %s/%s (%s)\n", xr.GetKind(), xr.GetName(), scope))
	}

	if _, err := fmt.Fprintf(stdout, "=== Affected Composite Resources ===\n\n%s\n=== Impact Analysis ===\n\n", xrList.String()); err != nil {
		return errors.Wrap(err, "cannot write XR list and headers")
	}

	// Convert unstructured to structured composition
	comp := &apiextensionsv1.Composition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(newComp.Object, comp); err != nil {
		return errors.Wrap(err, "cannot convert unstructured to Composition")
	}

	// Create a CachedFunctionProvider factory for this specific composition
	cachedFnProviderFactory := func(fnClient xp.FunctionClient, logger logging.Logger) FunctionProvider {
		provider, err := NewCachedFunctionProvider(fnClient, comp, logger)
		if err != nil {
			// Log error and fall back to default provider
			logger.Debug("Failed to create cached function provider, using default", "error", err)
			return NewDefaultFunctionProvider(fnClient, logger)
		}

		return provider
	}

	// Create options with the cached function provider factory
	// Note: We must append to a copy to avoid mutating the original slice
	optsWithCache := make([]ProcessorOption, len(p.processorOpts), len(p.processorOpts)+1)
	copy(optsWithCache, p.processorOpts)
	optsWithCache = append(optsWithCache, WithFunctionProviderFactory(cachedFnProviderFactory))

	// Create a new XR processor with cached function provider for this composition
	xrProc := NewDiffProcessor(p.k8Clients, p.xpClients, optsWithCache...)

	// Initialize the processor
	if err := xrProc.Initialize(ctx); err != nil {
		return errors.Wrap(err, "cannot initialize XR processor")
	}

	// Process affected XRs using the new processor with composition override
	if err := xrProc.PerformDiff(ctx, stdout, affectedXRs, func(context.Context, *un.Unstructured) (*apiextensionsv1.Composition, error) {
		return comp, nil
	}); err != nil {
		return errors.Wrap(err, "cannot process XRs with composition override")
	}

	return nil
}

// displayCompositionDiff shows the diff between the cluster composition and the file composition.
// If the composition doesn't exist in the cluster, it shows it as a new composition.
func (p *DefaultCompDiffProcessor) displayCompositionDiff(ctx context.Context, stdout io.Writer, newComp *un.Unstructured) error {
	p.config.Logger.Debug("Displaying composition diff", "composition", newComp.GetName())

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
			return errors.Wrap(err, "cannot convert original composition to unstructured")
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

	compDiff, err := renderer.GenerateDiffWithOptions(ctx, originalCompUnstructured, newCompUnstructured, p.config.Logger, diffOptions)
	if err != nil {
		return errors.Wrap(err, "cannot calculate composition diff")
	}

	p.config.Logger.Debug("Calculated composition diff",
		"composition", newComp.GetName(),
		"hasChanges", compDiff != nil,
		"isNewComposition", originalCompUnstructured == nil)

	// Add header for composition changes (common to all cases)
	if _, err := fmt.Fprintf(stdout, "=== Composition Changes ===\n\n"); err != nil {
		return errors.Wrap(err, "cannot write composition changes header")
	}

	// Display the composition diff if there are changes
	switch compDiff.DiffType {
	case dt.DiffTypeEqual:
		// No changes detected (only possible for existing compositions)
		p.config.Logger.Info("No changes detected in composition", "composition", newComp.GetName())

		if _, err := fmt.Fprintf(stdout, "No changes detected in composition %s\n\n", newComp.GetName()); err != nil {
			return errors.Wrap(err, "cannot write no changes message")
		}
	case dt.DiffTypeAdded, dt.DiffTypeRemoved, dt.DiffTypeModified:
		// Changes detected - show the diff
		// Create a diff renderer with proper options
		rendererOptions := renderer.DefaultDiffOptions()
		rendererOptions.UseColors = p.config.Colorize
		rendererOptions.Compact = p.config.Compact
		diffRenderer := renderer.NewDiffRenderer(p.config.Logger, rendererOptions)

		// Create a map with the single composition diff
		diffs := map[string]*dt.ResourceDiff{
			fmt.Sprintf("Composition/%s", newComp.GetName()): compDiff,
		}

		if err := diffRenderer.RenderDiffs(stdout, diffs); err != nil {
			return errors.Wrap(err, "cannot render composition diff")
		}

		if _, err := fmt.Fprintf(stdout, "\n"); err != nil {
			return errors.Wrap(err, "cannot write separator")
		}
	}

	return nil
}

// filterXRsByUpdatePolicy filters XRs based on the IncludeManual configuration.
// By default (IncludeManual=false), only XRs with Automatic policy are included.
// When IncludeManual=true, all XRs are included regardless of policy.
func (p *DefaultCompDiffProcessor) filterXRsByUpdatePolicy(xrs []*un.Unstructured) []*un.Unstructured {
	if p.config.IncludeManual {
		// Include all XRs when flag is set
		return xrs
	}

	// Filter to only include Automatic policy XRs
	filtered := make([]*un.Unstructured, 0, len(xrs))

	for _, xr := range xrs {
		policy := p.getCompositionUpdatePolicy(xr)

		p.config.Logger.Debug("Checking XR update policy",
			"xr", xr.GetName(),
			"kind", xr.GetKind(),
			"policy", policy)

		// Include XRs that are not explicitly set to Manual (i.e., Automatic or empty/default)
		if policy != "Manual" {
			filtered = append(filtered, xr)
		}
	}

	return filtered
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
	return "Automatic"
}
