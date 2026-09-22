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

const (
	headerCompositionChanges = "=== Composition Changes ==="
	headerAffectedResources  = "=== Affected Composite Resources ==="
	headerImpactAnalysis     = "=== Impact Analysis ==="
)

// CompDiffRenderer renders composition diff results.
// Both human-readable and structured (JSON/YAML) renderers implement this interface.
type CompDiffRenderer interface {
	// RenderCompDiff renders the complete composition diff output.
	// Top-level and tool errors go to DiffOptions.Stderr and are included in
	// structured output payloads. Per-composition messages (diffs, "no changes",
	// per-composition errors) go to DiffOptions.Stdout as part of the diff narrative.
	RenderCompDiff(output *CompDiffOutput) error
}

// DefaultCompDiffRenderer renders composition diffs in human-readable format.
type DefaultCompDiffRenderer struct {
	logger       logging.Logger
	diffRenderer DiffRenderer
	opts         DiffOptions
}

// NewDefaultCompDiffRenderer creates a new human-readable composition diff renderer.
func NewDefaultCompDiffRenderer(logger logging.Logger, diffRenderer DiffRenderer, opts DiffOptions) CompDiffRenderer {
	return &DefaultCompDiffRenderer{
		logger:       logger,
		diffRenderer: diffRenderer,
		opts:         opts,
	}
}

// RenderCompDiff renders the composition diff in human-readable format.
// Top-level errors go to r.opts.Stderr. Per-composition output (diffs, status
// messages, per-composition errors) goes to r.opts.Stdout.
func (r *DefaultCompDiffRenderer) RenderCompDiff(output *CompDiffOutput) error {
	stdout := r.opts.Stdout

	for i, comp := range output.Compositions {
		if i > 0 {
			if _, err := fmt.Fprint(stdout, "\n"+strings.Repeat("=", 80)+"\n\n"); err != nil {
				return errors.Wrap(err, "cannot write composition separator")
			}
		}

		switch {
		case r.opts.MinimizeComposition:
			if err := r.renderMinimizedCompositionChanges(&comp); err != nil {
				return err
			}
		default:
			if err := r.renderCompositionChanges(&comp); err != nil {
				return err
			}
		}

		// Skip remaining sections if composition had a processing error
		if comp.Error != nil {
			continue
		}

		// The affected-XR and impact-analysis sections would both be empty and misleading for a
		// composition whose XRs were deliberately not evaluated; say so once instead.
		if comp.ImpactAnalysisSkipped {
			if _, err := fmt.Fprint(stdout, skippedMessage(comp.RevisionImpact)); err != nil {
				return errors.Wrap(err, "cannot write impact analysis skipped message")
			}

			continue
		}

		// Name the revision before the per-composite sections. A composed resource whose template reads
		// the revision name shows a diff below with no other visible cause; this line is that cause.
		if _, err := fmt.Fprint(stdout, revisionImpactMessage(comp.RevisionImpact)); err != nil {
			return errors.Wrap(err, "cannot write revision impact message")
		}

		// Render affected XRs list with status indicators
		if err := r.renderAffectedResourcesList(&comp); err != nil {
			return err
		}

		// Render impact analysis (downstream diffs)
		if err := r.renderImpactAnalysis(&comp); err != nil {
			return err
		}
	}

	// Write top-level errors to stderr
	for _, e := range output.Errors {
		if _, err := fmt.Fprintln(r.opts.Stderr, e.FormatError()); err != nil {
			return errors.Wrap(err, "failed to write error to stderr")
		}
	}

	return nil
}

// renderCompositionChanges renders the composition changes section.
func (r *DefaultCompDiffRenderer) renderCompositionChanges(comp *CompositionDiff) error {
	stdout := r.opts.Stdout

	if _, err := fmt.Fprintf(stdout, headerCompositionChanges+"\n\n"); err != nil {
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
		if err := writeNoDisplayableChanges(stdout, comp); err != nil {
			return err
		}

		return nil
	}

	diffs := map[string]*dt.ResourceDiff{
		fmt.Sprintf("Composition/%s", comp.Name): comp.CompositionDiff,
	}

	// Identity-less group: the human renderer renders it as a flat, header-less
	// block, preserving comp's output. Comp provides its own XR grouping via
	// impact analysis one level up.
	if err := r.diffRenderer.RenderDiffs(identitylessGroups(diffs), nil, nil); err != nil {
		return errors.Wrap(err, "cannot render composition diff")
	}

	if _, err := fmt.Fprintf(stdout, "\n"); err != nil {
		return errors.Wrap(err, "cannot write separator")
	}

	return nil
}

// skippedMessage explains why the composites were not evaluated, which depends on why the analysis
// was skipped. The two reasons are not interchangeable: an identical composition creates no
// CompositionRevision and so genuinely cannot affect anything, whereas a metadata-only change does
// create one — the user simply asked not to pay for evaluating it. Reporting the first message for
// the second case would claim a guarantee the tool did not establish.
func skippedMessage(impact RevisionImpact) string {
	if impact.CreatesRevision {
		return fmt.Sprintf("Impact analysis skipped: --analyze-on=spec-change and this composition's spec is unchanged. Applying it still creates a new CompositionRevision that %d composite%s would adopt; whether that changes any rendered output was not evaluated. Pass --analyze-on=any-change to check.\n\n",
			impact.RepointedComposites, pluralize(impact.RepointedComposites))
	}

	return "Impact analysis skipped: this composition is identical to the cluster's, so applying it creates no new CompositionRevision and no composite resource could change as a result. Pass --analyze-on=always to evaluate them anyway.\n\n"
}

// revisionImpactMessage reports the revision churn applying this composition causes, for the run where
// the composites *were* evaluated. It is what keeps a downstream diff interpretable: with the
// composites' own compositionRevisionRef suppressed from display, a composed resource whose template
// reads the revision name would otherwise change with no visible cause. See issue #474.
//
// Deliberately does NOT name the revision, even though PredictedRevisionName is available here. The
// name's suffix is a hash of the composition's content, so putting it in human output would make
// otherwise-stable output churn on any composition edit — and it buys the reader nothing, because the
// diff body already shows the value wherever it actually propagates. Consumers that want the name read
// revisionImpact.predictedRevisionName from structured output, where a hash is not a readability
// problem. For the same reason the unpredictable case is not distinguished here; the warning on stderr
// says so, which is the right channel for "this could not be checked".
//
// Empty unless a revision is created AND some composite would adopt it: with nothing adopting it there
// is no downstream diff to explain, and the sections around this one already report the composition as
// new or changed.
//
// The skipped path does not use this: skippedMessage carries the same fact in its own sentence, so
// emitting both would state it twice.
func revisionImpactMessage(impact RevisionImpact) string {
	if !impact.CreatesRevision || impact.RepointedComposites == 0 {
		return ""
	}

	return fmt.Sprintf("Applying this composition creates a new CompositionRevision, which %d composite%s would adopt.\n\n",
		impact.RepointedComposites, pluralize(impact.RepointedComposites))
}

// writeNoDisplayableChanges reports a composition with no diff body to show. That covers two
// different situations, and conflating them would misreport the second: the composition really is
// identical, or it differs only in fields excluded from the diff. The latter is still a change —
// applying it produces a new CompositionRevision that affected composites adopt — so it must not be
// announced as "no changes".
func writeNoDisplayableChanges(stdout io.Writer, comp *CompositionDiff) error {
	if comp.MaskedChangesOnly {
		if _, err := fmt.Fprintf(stdout, "Composition %s differs only in fields excluded from this diff (--ignore-paths, or fields suppressed for readability). That is still a change: applying it creates a new CompositionRevision.\n\n", comp.Name); err != nil {
			return errors.Wrap(err, "cannot write masked-changes message")
		}

		return nil
	}

	if _, err := fmt.Fprintf(stdout, "No changes detected in composition %s\n\n", comp.Name); err != nil {
		return errors.Wrap(err, "cannot write no changes message")
	}

	return nil
}

// renderMinimizedCompositionChanges renders a single marker line per composition
// instead of the full YAML diff body. Errors and no-change compositions are
// shown as usual (i.e., not minimized); only changed compositions are collapsed.
func (r *DefaultCompDiffRenderer) renderMinimizedCompositionChanges(comp *CompositionDiff) error {
	stdout := r.opts.Stdout

	if _, err := fmt.Fprintf(stdout, headerCompositionChanges+"\n\n"); err != nil {
		return errors.Wrap(err, "cannot write composition changes header")
	}

	if comp.Error != nil {
		if _, err := fmt.Fprintf(stdout, "Error processing composition %s: %s\n\n", comp.Name, comp.Error.Error()); err != nil {
			return errors.Wrap(err, "cannot write composition error")
		}

		return nil
	}

	if comp.CompositionDiff == nil || comp.CompositionDiff.DiffType == dt.DiffTypeEqual {
		if err := writeNoDisplayableChanges(stdout, comp); err != nil {
			return err
		}

		return nil
	}

	marker := strings.Repeat(string(comp.CompositionDiff.DiffType), 3)

	color, resetColor := "", ""

	if r.opts.UseColors {
		switch comp.CompositionDiff.DiffType {
		case dt.DiffTypeModified:
			color = dt.ColorYellow
		case dt.DiffTypeAdded:
			color = dt.ColorGreen
		case dt.DiffTypeRemoved:
			color = dt.ColorRed
		case dt.DiffTypeEqual:
			// no color for equal
		}

		resetColor = dt.ColorReset
	}

	if _, err := fmt.Fprintf(stdout, "%s%s Composition/%s (minimized)%s\n\n", color, marker, comp.Name, resetColor); err != nil {
		return errors.Wrap(err, "cannot write minimized composition line")
	}

	return nil
}

// renderAffectedResourcesList renders the affected XRs list with status indicators.
func (r *DefaultCompDiffRenderer) renderAffectedResourcesList(comp *CompositionDiff) error {
	stdout := r.opts.Stdout

	if len(comp.ImpactAnalysis) == 0 {
		// No XRs surfaced. Either none were found, or all matched-by-name XRs were filtered out
		// (by Manual policy, revision-selector mismatch, and/or being deleted); report the
		// breakdown if so.
		switch {
		case totalFiltered(comp.AffectedResources) > 0:
			if _, err := fmt.Fprintf(stdout, "%s\n", allFilteredMessage(comp.Name, comp.AffectedResources)); err != nil {
				return errors.Wrap(err, "cannot write filtered XRs message")
			}
		default:
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
	if _, err := fmt.Fprintf(stdout, headerAffectedResources+"\n\n%s%s\n", xrList, summary); err != nil {
		return errors.Wrap(err, "cannot write XR list")
	}

	return nil
}

// renderImpactAnalysis renders the impact analysis section with downstream diffs.
func (r *DefaultCompDiffRenderer) renderImpactAnalysis(comp *CompositionDiff) error {
	stdout := r.opts.Stdout

	if _, err := fmt.Fprintf(stdout, headerImpactAnalysis+"\n\n"); err != nil {
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

	// Render all diffs if we found some, or show a message if empty.
	// Identity-less group: rendered flat (no per-XR header); comp groups via
	// impact analysis one level up.
	if len(allDiffs) > 0 {
		if err := r.diffRenderer.RenderDiffs(identitylessGroups(allDiffs), nil, nil); err != nil {
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

// totalFiltered sums the per-reason filter counters. Kept in one place so adding a reason cannot
// leave a caller silently ignoring it.
func totalFiltered(summary AffectedResourcesSummary) int {
	return summary.FilteredByPolicy + summary.FilteredBySelector + summary.FilteredByDeletion
}

// allFilteredMessage builds the default-discovery summary line for the case where every
// matched-by-name XR was filtered out, breaking the total down by reason so users understand why
// nothing is shown and how to see more. Single-reason cases get bespoke prose that names the
// remedy; mixed reasons fall through to an enumerated breakdown. Only called when at least one
// XR was filtered.
func allFilteredMessage(compName string, summary AffectedResourcesSummary) string {
	byPolicy, bySelector, byDeletion := summary.FilteredByPolicy, summary.FilteredBySelector, summary.FilteredByDeletion
	total := totalFiltered(summary)

	switch {
	case bySelector == 0 && byDeletion == 0:
		return fmt.Sprintf("All %d XR(s) using composition %s have Manual update policy (use --include-manual to see them)",
			total, compName)
	case byPolicy == 0 && byDeletion == 0:
		return fmt.Sprintf("All %d XR(s) using composition %s have a compositionRevisionSelector that does not match the composition's labels, so they would not adopt this revision",
			total, compName)
	case byPolicy == 0 && bySelector == 0:
		return fmt.Sprintf("All %d XR(s) using composition %s are being deleted, so they would not adopt this revision",
			total, compName)
	}

	clauses := make([]string, 0, 3)

	for _, c := range []struct {
		count int
		text  string
	}{
		{byPolicy, "with Manual update policy (use --include-manual to see them)"},
		{bySelector, "with a compositionRevisionSelector that does not match the composition's labels"},
		{byDeletion, "being deleted"},
	} {
		if c.count > 0 {
			clauses = append(clauses, fmt.Sprintf("%d %s", c.count, c.text))
		}
	}

	return fmt.Sprintf("All %d XR(s) using composition %s were filtered: %s",
		total, compName, strings.Join(clauses, ", "))
}

// filteredSuffix returns the human-readable explanation appended to a filtered XR line, chosen by
// the XR's FilterReason. Selector-mismatch entries additionally surface the concrete FilterDetail
// hint (which selector failed to match which labels) so users can self-diagnose the exclusion.
func filteredSuffix(impact XRImpact) string {
	switch impact.FilterReason {
	case FilterReasonManualPolicy:
		return " — filtered: Manual update policy (use --include-manual to evaluate)"
	case FilterReasonRevisionSelectorMismatch:
		if impact.FilterDetail != "" {
			return fmt.Sprintf(" — filtered: revision selector mismatch (%s)", impact.FilterDetail)
		}

		return " — filtered: revision selector mismatch"
	case FilterReasonDeleting:
		if impact.FilterDetail != "" {
			return fmt.Sprintf(" — filtered: being deleted (%s)", impact.FilterDetail)
		}

		return " — filtered: being deleted"
	default:
		return " — filtered"
	}
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

	if r.opts.UseColors {
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
		var (
			indicator, color string
			suffix           string
		)

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
		case XRStatusFiltered:
			indicator = "⊘" // ⊘
			color = colorYellow
			suffix = filteredSuffix(impact)
		}

		fmt.Fprintf(&sb, "%s  %s %s/%s (%s)%s%s\n",
			color,
			indicator,
			impact.Kind, impact.Name, scope,
			suffix,
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
	opts   DiffOptions
}

// NewStructuredCompDiffRenderer creates a new structured composition diff renderer.
func NewStructuredCompDiffRenderer(logger logging.Logger, opts DiffOptions) CompDiffRenderer {
	return &StructuredCompDiffRenderer{
		logger: logger,
		opts:   opts,
	}
}

// RenderCompDiff renders the composition diff in structured format (JSON/YAML).
// Top-level errors go to both r.opts.Stderr (for human visibility) and the
// structured output payload. Per-composition data goes to r.opts.Stdout.
func (r *StructuredCompDiffRenderer) RenderCompDiff(output *CompDiffOutput) error {
	// Convert internal representation to JSON output structure
	jsonOutput := r.buildStructuredCompOutput(output)

	var (
		data []byte
		err  error
	)

	switch r.opts.Format {
	case OutputFormatJSON:
		data, err = json.MarshalIndent(jsonOutput, "", "  ")
	case OutputFormatYAML:
		data, err = sigsyaml.Marshal(jsonOutput)
	case OutputFormatDiff:
		fallthrough
	default:
		return errors.Errorf("unsupported format for structured comp diff renderer: %s", r.opts.Format)
	}

	if err != nil {
		return errors.Wrap(err, "failed to marshal comp diff output")
	}

	_, err = r.opts.Stdout.Write(append(data, '\n'))
	if err != nil {
		return errors.Wrap(err, "failed to write output")
	}

	// Write errors to stderr for human visibility (they're also included in the structured output)
	for _, e := range output.Errors {
		if _, err := fmt.Fprintln(r.opts.Stderr, e.FormatError()); err != nil {
			return errors.Wrap(err, "failed to write error to stderr")
		}
	}

	return nil
}

// buildStructuredCompOutput converts internal CompDiffOutput to JSON-serializable structure.
func (r *StructuredCompDiffRenderer) buildStructuredCompOutput(output *CompDiffOutput) *compDiffWire {
	result := &compDiffWire{
		Compositions: make([]compositionDiffWire, 0, len(output.Compositions)),
		Errors:       output.Errors,
		Warnings:     output.Warnings,
	}

	for _, comp := range output.Compositions {
		jsonComp := compositionDiffWire{
			Name:                  comp.Name,
			AffectedResources:     comp.AffectedResources,
			ImpactAnalysis:        make([]xrImpactWire, 0, len(comp.ImpactAnalysis)),
			ImpactAnalysisSkipped: comp.ImpactAnalysisSkipped,
			MaskedChangesOnly:     comp.MaskedChangesOnly,
			RevisionImpact:        comp.RevisionImpact,
		}

		// Include per-composition error if present
		if comp.Error != nil {
			jsonComp.Error = comp.Error.Error()
		}

		// Convert composition diff if present and not equal.
		if comp.CompositionDiff != nil && comp.CompositionDiff.DiffType != dt.DiffTypeEqual {
			jsonComp.CompositionChanges = resourceDiffToChangeDetail(comp.CompositionDiff)
		}

		// Convert each XR impact
		for _, impact := range comp.ImpactAnalysis {
			jsonImpact := xrImpactWire{
				ObjectReference: impact.ObjectReference,
				Status:          impact.Status,
				FilterReason:    impact.FilterReason,
				FilterDetail:    impact.FilterDetail,
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
