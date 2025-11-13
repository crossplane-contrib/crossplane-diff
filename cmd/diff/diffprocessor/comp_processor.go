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
	"maps"
	"strings"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
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
	return len(r.Diffs) > 0
}

// HasError returns true if this XR encountered a processing error.
func (r *XRDiffResult) HasError() bool {
	return r.Error != nil
}

// CompDiffProcessor defines the interface for composition diffing.
type CompDiffProcessor interface {
	DiffComposition(ctx context.Context, stdout io.Writer, compositions []*un.Unstructured, namespace string) error
	Initialize(ctx context.Context) error
}

// DefaultCompDiffProcessor implements CompDiffProcessor.
type DefaultCompDiffProcessor struct {
	compositionClient xp.CompositionClient
	config            ProcessorConfig
	xrProc            DiffProcessor
}

// NewCompDiffProcessor creates a new DefaultCompDiffProcessor.
func NewCompDiffProcessor(xrProc DiffProcessor, compositionClient xp.CompositionClient, opts ...ProcessorOption) CompDiffProcessor {
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

	// Set default factories if not provided
	config.SetDefaultFactories()

	return &DefaultCompDiffProcessor{
		compositionClient: compositionClient,
		config:            config,
		xrProc:            xrProc,
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
		return p.handleNoXRsFound(stdout, newComp.GetName(), len(affectedXRs))
	}

	// Use filtered XRs for the rest of the processing
	affectedXRs = filteredXRs

	// Process affected XRs and collect diffs to determine which ones have changes
	p.config.Logger.Debug("Processing XRs to collect diff information", "count", len(affectedXRs))

	results := p.collectXRDiffs(ctx, stdout, affectedXRs, newComp)

	// Render the impact analysis (XR list and diffs)
	if err := p.renderXRImpactAnalysis(stdout, affectedXRs, results); err != nil {
		return err
	}

	// Collect any errors from the results and return them
	var processingErrs []error

	for _, result := range results {
		if result.HasError() {
			processingErrs = append(processingErrs, result.Error)
		}
	}

	if len(processingErrs) > 0 {
		return errors.Join(processingErrs...)
	}

	return nil
}

// handleNoXRsFound writes appropriate messages when no XRs are found or all are filtered.
func (p *DefaultCompDiffProcessor) handleNoXRsFound(stdout io.Writer, compositionName string, totalXRs int) error {
	if !p.config.IncludeManual && totalXRs > 0 {
		// XRs exist but were filtered out due to Manual policy
		p.config.Logger.Info("All XRs using composition have Manual update policy (use --include-manual to see them)",
			"composition", compositionName,
			"filteredCount", totalXRs)

		if _, err := fmt.Fprintf(stdout, "All %d XR(s) using composition %s have Manual update policy (use --include-manual to see them)\n",
			totalXRs, compositionName); err != nil {
			return errors.Wrap(err, "cannot write filtered XRs message")
		}
	} else {
		// No XRs found at all
		p.config.Logger.Info("No XRs found using composition", "composition", compositionName)

		if _, err := fmt.Fprintf(stdout, "No XRs found using composition %s\n", compositionName); err != nil {
			return errors.Wrap(err, "cannot write no XRs message")
		}
	}

	return nil
}

// collectXRDiffs processes XRs and collects their diffs, returning results for each XR.
func (p *DefaultCompDiffProcessor) collectXRDiffs(ctx context.Context, stdout io.Writer, xrs []*un.Unstructured, newComp *un.Unstructured) map[string]*XRDiffResult {
	// Composition provider function for getting the updated composition
	compositionProvider := func(context.Context, *un.Unstructured) (*apiextensionsv1.Composition, error) {
		comp := &apiextensionsv1.Composition{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(newComp.Object, comp); err != nil {
			return nil, errors.Wrap(err, "cannot convert unstructured to Composition")
		}

		return comp, nil
	}

	results := make(map[string]*XRDiffResult)

	for _, xr := range xrs {
		resourceID := fmt.Sprintf("%s/%s", xr.GetKind(), xr.GetName())

		diffs, err := p.xrProc.DiffSingleResource(ctx, xr, compositionProvider)
		if err != nil {
			p.config.Logger.Debug("Failed to process resource", "resource", resourceID, "error", err)

			// Write error message to stdout so user can see it
			errMsg := fmt.Sprintf("ERROR: Failed to process %s: %v\n\n", resourceID, err)
			if _, writeErr := fmt.Fprint(stdout, errMsg); writeErr != nil {
				p.config.Logger.Debug("Failed to write error message", "error", writeErr)
			}

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

// renderXRImpactAnalysis renders the XR list with status indicators and all collected diffs.
func (p *DefaultCompDiffProcessor) renderXRImpactAnalysis(stdout io.Writer, xrs []*un.Unstructured, results map[string]*XRDiffResult) error {
	// Build the XR list with status indicators and counts
	xrList, changedCount, unchangedCount, errorCount := buildXRStatusList(xrs, results, p.config.Colorize)

	// Generate summary line
	summary := formatXRStatusSummary(changedCount, unchangedCount, errorCount)

	// Write the XR list with summary
	if _, err := fmt.Fprintf(stdout, "=== Affected Composite Resources ===\n\n%s%s\n=== Impact Analysis ===\n\n",
		xrList, summary); err != nil {
		return errors.Wrap(err, "cannot write XR list and headers")
	}

	// Collect all diffs from the results
	allDiffs := make(map[string]*dt.ResourceDiff)

	for _, result := range results {
		if !result.HasError() && result.HasChanges() {
			maps.Copy(allDiffs, result.Diffs)
		}
	}

	// Render all diffs if we found some, or show a message if empty
	if len(allDiffs) > 0 {
		diffRenderer := p.config.Factories.DiffRenderer(
			p.config.Logger,
			renderer.DiffOptions{
				UseColors:      p.config.Colorize,
				AddPrefix:      "+ ",
				DeletePrefix:   "- ",
				ContextPrefix:  "  ",
				ContextLines:   3,
				ChunkSeparator: "...",
				Compact:        p.config.Compact,
			},
		)

		if err := diffRenderer.RenderDiffs(stdout, allDiffs); err != nil {
			p.config.Logger.Debug("Failed to render diffs", "error", err)
			return errors.Wrap(err, "failed to render diffs")
		}
	} else {
		// No diffs found - write explanatory message
		if _, err := fmt.Fprint(stdout, "All composite resources are up-to-date. No downstream resource changes detected.\n\n"); err != nil {
			return errors.Wrap(err, "cannot write empty impact message")
		}
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
	diffOptions.IgnorePaths = p.config.IgnorePaths

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

// pluralize returns "s" if count is not 1, otherwise returns empty string.
func pluralize(count int) string {
	if count == 1 {
		return ""
	}

	return "s"
}

// formatXRStatusSummary generates the summary line with correct pluralization.
func formatXRStatusSummary(changedCount, unchangedCount, errorCount int) string {
	parts := []string{}

	if changedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d resource%s with changes", changedCount, pluralize(changedCount)))
	}

	if unchangedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d resource%s unchanged", unchangedCount, pluralize(unchangedCount)))
	}

	if errorCount > 0 {
		parts = append(parts, fmt.Sprintf("%d resource%s with errors", errorCount, pluralize(errorCount)))
	}

	return fmt.Sprintf("\nSummary: %s\n", strings.Join(parts, ", "))
}

// buildXRStatusList builds the XR list with status indicators and returns the formatted string with counts.
func buildXRStatusList(xrs []*un.Unstructured, results map[string]*XRDiffResult, colorize bool) (xrList string, changedCount, unchangedCount, errorCount int) {
	var sb strings.Builder

	// Color codes and indicators (colors remain empty strings when colorization is disabled)
	checkMark := "✓"
	warningMark := "⚠"
	errorMark := "✗"
	colorGreen := ""
	colorYellow := ""
	colorRed := ""
	colorReset := ""

	if colorize {
		colorGreen = dt.ColorGreen
		colorYellow = dt.ColorYellow
		colorRed = dt.ColorRed
		colorReset = dt.ColorReset
	}

	for _, xr := range xrs {
		resourceID := fmt.Sprintf("%s/%s", xr.GetKind(), xr.GetName())

		// Format namespace/scope information
		scope := fmt.Sprintf("namespace: %s", xr.GetNamespace())
		if xr.GetNamespace() == "" {
			scope = "cluster-scoped"
		}

		// Determine status indicator and color based on result
		var indicator, color string

		result := results[resourceID]
		switch {
		case result != nil && result.HasError():
			// Processing error - show red X
			indicator = errorMark
			color = colorRed
			errorCount++
		case result != nil && result.HasChanges():
			// Has changes - show yellow warning
			indicator = warningMark
			color = colorYellow
			changedCount++
		default:
			// No changes - show green check
			indicator = checkMark
			color = colorGreen
			unchangedCount++
		}

		sb.WriteString(fmt.Sprintf("%s  %s %s/%s (%s)%s\n",
			color,
			indicator,
			xr.GetKind(), xr.GetName(), scope,
			colorReset))
	}

	return sb.String(), changedCount, unchangedCount, errorCount
}
