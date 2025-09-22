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

// Package diffprocessor contains the composition diff processor.
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
	k8sClients k8.Clients
	xpClients  xp.Clients
	config     ProcessorConfig
	xrProc     DiffProcessor
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
		k8sClients: k8cs,
		xpClients:  xpcs,
		config:     config,
	}
}

// Initialize loads required resources.
func (p *DefaultCompDiffProcessor) Initialize(ctx context.Context) error {
	p.config.Logger.Debug("Initializing composition diff processor")

	// Create an XR diff processor that we'll reuse
	xrOptions := []ProcessorOption{
		WithNamespace(p.config.Namespace),
		WithLogger(p.config.Logger),
		WithRenderFunc(render.Render),
		WithColorize(p.config.Colorize),
		WithCompact(p.config.Compact),
	}

	p.xrProc = NewDiffProcessor(p.k8sClients, p.xpClients, xrOptions...)

	// Initialize the XR processor
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
		compositionID := fmt.Sprintf("composition %d", i+1)

		// Skip non-Composition objects (e.g., GoTemplate objects extracted from pipeline steps)
		if comp.GetKind() != "Composition" {
			p.config.Logger.Debug("Skipping non-Composition object", "kind", comp.GetKind(), "apiVersion", comp.GetAPIVersion())
			continue
		}

		// Convert unstructured to typed composition
		newComp, err := p.unstructuredToComposition(comp)
		if err != nil {
			p.config.Logger.Debug("Failed to convert composition", "composition", compositionID, "error", err)
			errs = append(errs, errors.Wrapf(err, "cannot convert %s from unstructured", compositionID))

			continue
		}

		compositionID = newComp.GetName() // Use actual name once we have it
		p.config.Logger.Debug("Processing composition", "name", compositionID)

		// Process this single composition
		if err := p.processSingleComposition(ctx, stdout, newComp, namespace); err != nil {
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
func (p *DefaultCompDiffProcessor) processSingleComposition(ctx context.Context, stdout io.Writer, newComp *apiextensionsv1.Composition, namespace string) error {
	// First, show the composition diff itself
	if err := p.displayCompositionDiff(ctx, stdout, newComp); err != nil {
		return errors.Wrap(err, "cannot display composition diff")
	}

	// Find all XRs that use this composition
	affectedXRs, err := p.findXRsUsingComposition(ctx, newComp.GetName(), namespace)
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

	if len(affectedXRs) == 0 {
		p.config.Logger.Info("No XRs found using composition", "composition", newComp.GetName())

		if _, err := fmt.Fprintf(stdout, "No XRs found using composition %s\n", newComp.GetName()); err != nil {
			return errors.Wrap(err, "cannot write no XRs message")
		}

		return nil
	}

	// Process affected XRs using the existing XR processor with composition override
	// List the affected XRs so users can understand the scope of impact
	if _, err := fmt.Fprintf(stdout, "=== Affected Composite Resources ===\n\n"); err != nil {
		return errors.Wrap(err, "cannot write affected XRs header")
	}

	for _, xr := range affectedXRs {
		if _, err := fmt.Fprintf(stdout, "- %s/%s (namespace: %s)\n", xr.GetKind(), xr.GetName(), xr.GetNamespace()); err != nil {
			return errors.Wrap(err, "cannot write affected XR info")
		}
	}

	if _, err := fmt.Fprintf(stdout, "\n=== Impact Analysis ===\n\n"); err != nil {
		return errors.Wrap(err, "cannot write impact analysis header")
	}

	if err := p.xrProc.PerformDiff(ctx, stdout, affectedXRs, func(context.Context, *un.Unstructured) (*apiextensionsv1.Composition, error) {
		return newComp, nil
	}); err != nil {
		return errors.Wrap(err, "cannot process XRs with composition override")
	}

	return nil
}

// findXRsUsingComposition finds all XRs that use the specified composition.
func (p *DefaultCompDiffProcessor) findXRsUsingComposition(ctx context.Context, compositionName string, namespace string) ([]*un.Unstructured, error) {
	// Use the composition client to find XRs that reference this composition
	return p.xpClients.Composition.FindXRsUsingComposition(ctx, compositionName, namespace)
}

// displayCompositionDiff shows the diff between the cluster composition and the file composition.
func (p *DefaultCompDiffProcessor) displayCompositionDiff(ctx context.Context, stdout io.Writer, newComp *apiextensionsv1.Composition) error {
	p.config.Logger.Debug("Displaying composition diff", "composition", newComp.GetName())

	// Get the original composition from the cluster
	originalComp, err := p.xpClients.Composition.GetComposition(ctx, newComp.GetName())
	if err != nil {
		p.config.Logger.Debug("Original composition not found in cluster, treating as new composition",
			"composition", newComp.GetName(), "error", err)

		// Handle case where composition doesn't exist in cluster (net-new composition)
		return p.displayNewComposition(ctx, stdout, newComp)
	}

	p.config.Logger.Debug("Retrieved original composition from cluster", "name", originalComp.GetName(), "composition", originalComp)

	// Convert both compositions to unstructured for comparison
	originalCompUnstructured, err := p.compositionToUnstructured(originalComp)
	if err != nil {
		return errors.Wrap(err, "cannot convert original composition to unstructured")
	}

	newCompUnstructured, err := p.compositionToUnstructured(newComp)
	if err != nil {
		return errors.Wrap(err, "cannot convert new composition to unstructured")
	}

	// Clean up managed fields and other cluster metadata before diff calculation
	originalCompUnstructured.SetManagedFields(nil)
	originalCompUnstructured.SetResourceVersion("")
	originalCompUnstructured.SetUID("")
	originalCompUnstructured.SetGeneration(0)
	originalCompUnstructured.SetCreationTimestamp(metav1.Time{})

	newCompUnstructured.SetManagedFields(nil)
	newCompUnstructured.SetResourceVersion("")
	newCompUnstructured.SetUID("")
	newCompUnstructured.SetGeneration(0)
	newCompUnstructured.SetCreationTimestamp(metav1.Time{})

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
		"hasChanges", compDiff != nil)

	// Display the composition diff if there are changes
	if compDiff != nil && compDiff.DiffType != dt.DiffTypeEqual {
		// Create a diff renderer with proper options
		rendererOptions := renderer.DefaultDiffOptions()
		rendererOptions.UseColors = p.config.Colorize
		rendererOptions.Compact = p.config.Compact
		diffRenderer := renderer.NewDiffRenderer(p.config.Logger, rendererOptions)

		// Create a map with the single composition diff
		diffs := map[string]*dt.ResourceDiff{
			fmt.Sprintf("Composition/%s", newComp.GetName()): compDiff,
		}

		// Add a header to distinguish composition diff from XR diffs
		if _, err := fmt.Fprintf(stdout, "=== Composition Changes ===\n\n"); err != nil {
			return errors.Wrap(err, "cannot write composition changes header")
		}

		if err := diffRenderer.RenderDiffs(stdout, diffs); err != nil {
			return errors.Wrap(err, "cannot render composition diff")
		}

		if _, err := fmt.Fprintf(stdout, "\n"); err != nil {
			return errors.Wrap(err, "cannot write separator")
		}
	} else {
		p.config.Logger.Info("No changes detected in composition", "composition", newComp.GetName())

		if _, err := fmt.Fprintf(stdout, "=== Composition Changes ===\n\n"); err != nil {
			return errors.Wrap(err, "cannot write composition changes header")
		}

		if _, err := fmt.Fprintf(stdout, "No changes detected in composition %s\n\n", newComp.GetName()); err != nil {
			return errors.Wrap(err, "cannot write no changes message")
		}
	}

	return nil
}

// compositionToUnstructured converts a typed Composition to unstructured for diff calculation.
func (p *DefaultCompDiffProcessor) compositionToUnstructured(comp *apiextensionsv1.Composition) (*un.Unstructured, error) {
	// Convert composition to unstructured using runtime conversion
	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(comp)
	if err != nil {
		return nil, errors.Wrap(err, "cannot convert composition to unstructured map")
	}

	return &un.Unstructured{Object: unstructuredObj}, nil
}

// unstructuredToComposition converts an unstructured object to a typed Composition.
func (p *DefaultCompDiffProcessor) unstructuredToComposition(u *un.Unstructured) (*apiextensionsv1.Composition, error) {
	comp := &apiextensionsv1.Composition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, comp); err != nil {
		return nil, errors.Wrap(err, "cannot convert unstructured to Composition")
	}

	return comp, nil
}

// displayNewComposition shows a new composition that doesn't exist in the cluster.
func (p *DefaultCompDiffProcessor) displayNewComposition(ctx context.Context, stdout io.Writer, newComp *apiextensionsv1.Composition) error {
	p.config.Logger.Debug("Displaying new composition", "composition", newComp.GetName())

	// Convert new composition to unstructured for rendering
	newCompUnstructured, err := p.compositionToUnstructured(newComp)
	if err != nil {
		return errors.Wrap(err, "cannot convert new composition to unstructured")
	}

	// Clean up managed fields and other cluster metadata
	newCompUnstructured.SetManagedFields(nil)
	newCompUnstructured.SetResourceVersion("")
	newCompUnstructured.SetUID("")
	newCompUnstructured.SetGeneration(0)
	newCompUnstructured.SetCreationTimestamp(metav1.Time{})

	// Generate a diff showing the entire composition as new (all additions)
	diffOptions := renderer.DefaultDiffOptions()
	diffOptions.UseColors = p.config.Colorize
	diffOptions.Compact = p.config.Compact

	// Create a diff with empty original (nil) to show everything as additions
	compDiff, err := renderer.GenerateDiffWithOptions(ctx, nil, newCompUnstructured, p.config.Logger, diffOptions)
	if err != nil {
		return errors.Wrap(err, "cannot calculate new composition diff")
	}

	p.config.Logger.Debug("Calculated new composition diff",
		"composition", newComp.GetName(),
		"hasChanges", compDiff != nil)

	// Display the new composition diff
	if compDiff != nil {
		// Create a diff renderer with proper options
		rendererOptions := renderer.DefaultDiffOptions()
		rendererOptions.UseColors = p.config.Colorize
		rendererOptions.Compact = p.config.Compact
		diffRenderer := renderer.NewDiffRenderer(p.config.Logger, rendererOptions)

		// Create a map with the single composition diff
		diffs := map[string]*dt.ResourceDiff{
			fmt.Sprintf("Composition/%s", newComp.GetName()): compDiff,
		}

		// Add a header to distinguish composition diff from XR diffs
		if _, err := fmt.Fprintf(stdout, "=== Composition Changes ===\n\n"); err != nil {
			return errors.Wrap(err, "cannot write composition changes header")
		}

		if err := diffRenderer.RenderDiffs(stdout, diffs); err != nil {
			return errors.Wrap(err, "cannot render new composition diff")
		}

		if _, err := fmt.Fprintf(stdout, "\n"); err != nil {
			return errors.Wrap(err, "cannot write separator")
		}
	} else {
		// This shouldn't happen for a new composition, but handle it gracefully
		p.config.Logger.Debug("No diff generated for new composition", "composition", newComp.GetName())

		if _, err := fmt.Fprintf(stdout, "=== Composition Changes ===\n\n"); err != nil {
			return errors.Wrap(err, "cannot write composition changes header")
		}

		if _, err := fmt.Fprintf(stdout, "New composition %s\n\n", newComp.GetName()); err != nil {
			return errors.Wrap(err, "cannot write new composition message")
		}
	}

	return nil
}
