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

package renderer

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// CompDiffRenderer renders composition diff results.
// Both human-readable and structured (JSON/YAML) renderers implement this interface.
type CompDiffRenderer interface {
	// RenderCompDiff renders the complete composition diff output.
	// This includes composition changes, affected XR list, and impact analysis.
	RenderCompDiff(stdout io.Writer, output *CompDiffOutput) error
}

// DefaultCompDiffRenderer renders composition diffs in human-readable format.
type DefaultCompDiffRenderer struct {
	logger       logging.Logger
	diffRenderer DiffRenderer
	colorize     bool
}

// NewDefaultCompDiffRenderer creates a new human-readable composition diff renderer.
func NewDefaultCompDiffRenderer(logger logging.Logger, diffRenderer DiffRenderer, colorize bool) CompDiffRenderer {
	return &DefaultCompDiffRenderer{
		logger:       logger,
		diffRenderer: diffRenderer,
		colorize:     colorize,
	}
}

// RenderCompDiff renders the composition diff in human-readable format.
func (r *DefaultCompDiffRenderer) RenderCompDiff(stdout io.Writer, output *CompDiffOutput) error {
	for i, comp := range output.Compositions {
		if i > 0 {
			if _, err := fmt.Fprint(stdout, "\n"+strings.Repeat("=", 80)+"\n\n"); err != nil {
				return errors.Wrap(err, "cannot write composition separator")
			}
		}

		// Render composition changes section
		if err := r.renderCompositionChanges(stdout, &comp); err != nil {
			return err
		}

		// Skip remaining sections if composition had a processing error
		if comp.Error != nil {
			continue
		}

		// Render affected XRs list with status indicators
		if err := r.renderAffectedResourcesList(stdout, &comp); err != nil {
			return err
		}

		// Render impact analysis (downstream diffs)
		if err := r.renderImpactAnalysis(stdout, &comp); err != nil {
			return err
		}
	}

	return nil
}

// renderCompositionChanges renders the composition changes section.
func (r *DefaultCompDiffRenderer) renderCompositionChanges(stdout io.Writer, comp *CompositionDiff) error {
	if _, err := fmt.Fprintf(stdout, "=== Composition Changes ===\n\n"); err != nil {
		return errors.Wrap(err, "cannot write composition changes header")
	}

	// Check for composition processing error first
	if comp.Error != nil {
		if _, err := fmt.Fprintf(stdout, "Error processing composition %s: %s\n\n", comp.Name, comp.Error.Error()); err != nil {
			return errors.Wrap(err, "cannot write composition error")
		}

		return nil
	}

	if comp.CompositionDiff == nil || comp.CompositionDiff.DiffType == dt.DiffTypeEqual {
		if _, err := fmt.Fprintf(stdout, "No changes detected in composition %s\n\n", comp.Name); err != nil {
			return errors.Wrap(err, "cannot write no changes message")
		}

		return nil
	}

	diffs := map[string]*dt.ResourceDiff{
		fmt.Sprintf("Composition/%s", comp.Name): comp.CompositionDiff,
	}

	if err := r.diffRenderer.RenderDiffs(stdout, diffs, nil); err != nil {
		return errors.Wrap(err, "cannot render composition diff")
	}

	if _, err := fmt.Fprintf(stdout, "\n"); err != nil {
		return errors.Wrap(err, "cannot write separator")
	}

	return nil
}

// renderAffectedResourcesList renders the affected XRs list with status indicators.
func (r *DefaultCompDiffRenderer) renderAffectedResourcesList(stdout io.Writer, comp *CompositionDiff) error {
	if len(comp.ImpactAnalysis) == 0 {
		// Check if all resources were filtered by policy
		if comp.AffectedResources.FilteredByPolicy > 0 {
			if _, err := fmt.Fprintf(stdout, "All %d XR(s) using composition %s have Manual update policy (use --include-manual to see them)\n",
				comp.AffectedResources.FilteredByPolicy, comp.Name); err != nil {
				return errors.Wrap(err, "cannot write filtered XRs message")
			}
		} else {
			if _, err := fmt.Fprintf(stdout, "No XRs found using composition %s\n", comp.Name); err != nil {
				return errors.Wrap(err, "cannot write no XRs message")
			}
		}

		return nil
	}

	// Build the XR list with status indicators
	xrList := r.buildXRStatusList(comp.ImpactAnalysis)

	// Generate summary line
	summary := formatXRStatusSummary(
		comp.AffectedResources.WithChanges,
		comp.AffectedResources.Unchanged,
		comp.AffectedResources.WithErrors,
	)

	// Write the XR list with summary
	if _, err := fmt.Fprintf(stdout, "=== Affected Composite Resources ===\n\n%s%s\n", xrList, summary); err != nil {
		return errors.Wrap(err, "cannot write XR list")
	}

	return nil
}

// renderImpactAnalysis renders the impact analysis section with downstream diffs.
func (r *DefaultCompDiffRenderer) renderImpactAnalysis(stdout io.Writer, comp *CompositionDiff) error {
	if _, err := fmt.Fprintf(stdout, "=== Impact Analysis ===\n\n"); err != nil {
		return errors.Wrap(err, "cannot write impact analysis header")
	}

	// Collect all diffs from the impact analysis using stored ResourceDiffs.
	allDiffs := make(map[string]*dt.ResourceDiff)

	for _, impact := range comp.ImpactAnalysis {
		if impact.Status == XRStatusChanged && impact.Diffs != nil {
			for key, diff := range impact.Diffs {
				// Skip equal diffs (may be stored for removal detection purposes)
				if diff.DiffType != dt.DiffTypeEqual {
					allDiffs[key] = diff
				}
			}
		}
	}

	// Render all diffs if we found some, or show a message if empty
	if len(allDiffs) > 0 {
		if err := r.diffRenderer.RenderDiffs(stdout, allDiffs, nil); err != nil {
			r.logger.Debug("Failed to render diffs", "error", err)
			return errors.Wrap(err, "failed to render diffs")
		}
	} else {
		if _, err := fmt.Fprint(stdout, "All composite resources are up-to-date. No downstream resource changes detected.\n\n"); err != nil {
			return errors.Wrap(err, "cannot write empty impact message")
		}
	}

	return nil
}

// buildXRStatusList builds the XR list with status indicators.
func (r *DefaultCompDiffRenderer) buildXRStatusList(impacts []XRImpact) string {
	var sb strings.Builder

	// Color codes and indicators
	checkMark := "\u2713"
	warningMark := "\u26a0"
	errorMark := "\u2717"
	colorGreen := ""
	colorYellow := ""
	colorRed := ""
	colorReset := ""

	if r.colorize {
		colorGreen = dt.ColorGreen
		colorYellow = dt.ColorYellow
		colorRed = dt.ColorRed
		colorReset = dt.ColorReset
	}

	for _, impact := range impacts {
		// Format namespace/scope information
		scope := fmt.Sprintf("namespace: %s", impact.Namespace)
		if impact.Namespace == "" {
			scope = "cluster-scoped"
		}

		// Determine status indicator and color based on status
		var indicator, color string

		switch impact.Status {
		case XRStatusError:
			indicator = errorMark
			color = colorRed
		case XRStatusChanged:
			indicator = warningMark
			color = colorYellow
		case XRStatusUnchanged:
			indicator = checkMark
			color = colorGreen
		}

		fmt.Fprintf(&sb, "%s  %s %s/%s (%s)%s\n",
			color,
			indicator,
			impact.Kind, impact.Name, scope,
			colorReset)

		// Include error details for XRStatusError impacts so users can diagnose issues.
		if impact.Status == XRStatusError && impact.Error != nil {
			fmt.Fprintf(&sb, "%s    Error: %s%s\n", color, impact.Error.Error(), colorReset)
		}
	}

	return sb.String()
}

// StructuredCompDiffRenderer renders composition diffs in JSON/YAML format.
type StructuredCompDiffRenderer struct {
	logger logging.Logger
	format OutputFormat
}

// NewStructuredCompDiffRenderer creates a new structured composition diff renderer.
func NewStructuredCompDiffRenderer(logger logging.Logger, format OutputFormat) CompDiffRenderer {
	return &StructuredCompDiffRenderer{
		logger: logger,
		format: format,
	}
}

// RenderCompDiff renders the composition diff in structured format (JSON/YAML).
func (r *StructuredCompDiffRenderer) RenderCompDiff(stdout io.Writer, output *CompDiffOutput) error {
	// Convert internal representation to JSON output structure
	jsonOutput := r.buildStructuredCompOutput(output)

	var (
		data []byte
		err  error
	)

	switch r.format {
	case OutputFormatJSON:
		data, err = json.MarshalIndent(jsonOutput, "", "  ")
	case OutputFormatYAML:
		data, err = sigsyaml.Marshal(jsonOutput)
	case OutputFormatDiff:
		fallthrough
	default:
		return errors.Errorf("unsupported format for structured comp diff renderer: %s", r.format)
	}

	if err != nil {
		return errors.Wrap(err, "failed to marshal comp diff output")
	}

	_, err = stdout.Write(append(data, '\n'))

	return errors.Wrap(err, "failed to write output")
}

// buildStructuredCompOutput converts internal CompDiffOutput to JSON-serializable structure.
func (r *StructuredCompDiffRenderer) buildStructuredCompOutput(output *CompDiffOutput) *compDiffJSONOutput {
	result := &compDiffJSONOutput{
		Compositions: make([]compositionDiffJSON, 0, len(output.Compositions)),
		Errors:       output.Errors,
	}

	for _, comp := range output.Compositions {
		jsonComp := compositionDiffJSON{
			Name:              comp.Name,
			AffectedResources: comp.AffectedResources,
			ImpactAnalysis:    make([]xrImpactJSON, 0, len(comp.ImpactAnalysis)),
		}

		// Include per-composition error if present
		if comp.Error != nil {
			jsonComp.Error = comp.Error.Error()
		}

		// Convert composition diff if present and not equal
		if comp.CompositionDiff != nil && comp.CompositionDiff.DiffType != dt.DiffTypeEqual {
			jsonComp.CompositionChanges = resourceDiffToChangeDetail(comp.CompositionDiff)
		}

		// Convert each XR impact
		for _, impact := range comp.ImpactAnalysis {
			jsonImpact := xrImpactJSON{
				ObjectReference: impact.ObjectReference,
				Status:          impact.Status,
			}
			if impact.Error != nil {
				jsonImpact.Error = impact.Error.Error()
			}

			if impact.Status == XRStatusChanged && len(impact.Diffs) > 0 {
				jsonImpact.DownstreamChanges = buildDownstreamChanges(impact.Diffs)
			}

			jsonComp.ImpactAnalysis = append(jsonComp.ImpactAnalysis, jsonImpact)
		}

		result.Compositions = append(result.Compositions, jsonComp)
	}

	return result
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

	if len(parts) == 0 {
		return ""
	}

	return fmt.Sprintf("\nSummary: %s\n", strings.Join(parts, ", "))
}

// pluralize returns "s" if count is not 1, otherwise returns empty string.
func pluralize(count int) string {
	if count == 1 {
		return ""
	}

	return "s"
}
