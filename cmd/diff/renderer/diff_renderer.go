package renderer

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// flattenGroups merges every group's diffs into a single map. It is the basis
// for the deprecated flat changes[] view in structured output and for the
// composition renderer's flat reuse of the human renderer. A nil group.Diffs
// is a safe no-op under maps.Copy.
func flattenGroups(groups []dt.XRDiffGroup) map[string]*dt.ResourceDiff {
	out := make(map[string]*dt.ResourceDiff)
	for _, g := range groups {
		maps.Copy(out, g.Diffs)
	}

	return out
}

// DiffRenderer handles rendering diffs to output.
type DiffRenderer interface {
	// RenderDiffs formats and outputs diffs, grouped by input XR.
	// Diff output goes to DiffOptions.Stdout, errors go to DiffOptions.Stderr.
	// The errs parameter contains the union of resource processing errors to
	// include in output (the top-level/global error list).
	RenderDiffs(groups []dt.XRDiffGroup, errs []dt.OutputError) error
}

// DefaultDiffRenderer implements the DiffRenderer interface.
type DefaultDiffRenderer struct {
	logger   logging.Logger
	diffOpts DiffOptions
}

// NewDiffRenderer creates a new DefaultDiffRenderer with the given options.
func NewDiffRenderer(logger logging.Logger, diffOpts DiffOptions) DiffRenderer {
	return &DefaultDiffRenderer{
		logger:   logger,
		diffOpts: diffOpts,
	}
}

// SetDiffOptions updates the diff options used by the renderer.
func (r *DefaultDiffRenderer) SetDiffOptions(options DiffOptions) {
	r.diffOpts = options
}

func getKindName(d *dt.ResourceDiff) string {
	return fmt.Sprintf("%s/%s", d.Gvk.Kind, d.ResourceName)
}

// diffCounts holds the per-diff-type tally produced while rendering a list of
// diffs. outputCount is the number of diffs that actually emitted content
// (non-equal with a non-empty rendered body).
type diffCounts struct {
	added    int
	modified int
	removed  int
	equal    int
	output   int
}

// renderDiffList renders a flat set of diffs (sorted by kind/name) to stdout in
// the human-readable +++/~~~/--- form and returns the per-type counts. It does
// not print a summary; callers decide whether to emit a per-section or
// aggregate summary from the returned counts. Equal diffs are skipped.
func (r *DefaultDiffRenderer) renderDiffList(diffs map[string]*dt.ResourceDiff) (diffCounts, error) {
	stdout := r.diffOpts.Stdout

	// Sort by GetKindName, which is how it's displayed to the user.
	d := slices.AppendSeq(make([]*dt.ResourceDiff, 0, len(diffs)), maps.Values(diffs))
	slices.SortFunc(d, func(a, b *dt.ResourceDiff) int {
		return cmp.Compare(getKindName(a), getKindName(b))
	})

	var counts diffCounts

	for _, diff := range d {
		resourceID := getKindName(diff)

		var header string

		// The added/modified/removed counters increment here, before the
		// content-empty check below. This relies on the invariant that a
		// non-equal ResourceDiff always renders non-empty content (its
		// LineDiffs are non-trivial by construction). If that ever ceased to
		// hold, counts.output could lag the type counters, desyncing the
		// summary line from the number of rendered blocks.
		switch diff.DiffType {
		case dt.DiffTypeAdded:
			counts.added++
			header = fmt.Sprintf("+++ %s", resourceID)
		case dt.DiffTypeRemoved:
			counts.removed++
			header = fmt.Sprintf("--- %s", resourceID)
		case dt.DiffTypeModified:
			counts.modified++
			header = fmt.Sprintf("~~~ %s", resourceID)
		case dt.DiffTypeEqual:
			counts.equal++
			// Skip rendering equal resources
			continue
		}

		content := FormatDiff(diff.LineDiffs, r.diffOpts)

		if content != "" {
			if _, err := fmt.Fprintf(stdout, "%s\n%s\n---\n", header, content); err != nil {
				r.logger.Debug("Error writing diff to output", "resource", resourceID, "error", err)
				return counts, errors.Wrap(err, "failed to write diff to output")
			}

			counts.output++
		} else {
			r.logger.Debug("Empty diff content, skipping output", "resource", resourceID)
		}
	}

	return counts, nil
}

// summaryLine formats a "N added, N modified, N removed" fragment from counts,
// omitting zero categories. Returns "" when there is nothing to report.
func summaryLine(counts diffCounts) string {
	parts := make([]string, 0, 3)

	if counts.added > 0 {
		parts = append(parts, fmt.Sprintf("%d added", counts.added))
	}

	if counts.modified > 0 {
		parts = append(parts, fmt.Sprintf("%d modified", counts.modified))
	}

	if counts.removed > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", counts.removed))
	}

	return strings.Join(parts, ", ")
}

// hasIdentity reports whether a group carries input-XR identity. The
// composition renderer reuses this renderer with identity-less groups (no
// single owning XR); those render as a flat, header-less block, preserving
// comp's output. The xr command always sets identity, producing per-XR
// sections.
func hasIdentity(g dt.XRDiffGroup) bool {
	return g.XR.Kind != "" || g.XR.Name != ""
}

// RenderDiffs formats and prints the diffs.
// Diff output goes to r.diffOpts.Stdout, errors go to r.diffOpts.Stderr.
//
// Identity-bearing groups (the xr command) render as per-input-XR sections,
// each with a header and per-section summary, followed by an aggregate footer
// when there is more than one such group. Identity-less groups (the
// composition renderer's reuse) render as a single flat block, preserving the
// pre-grouping behavior.
func (r *DefaultDiffRenderer) RenderDiffs(groups []dt.XRDiffGroup, errs []dt.OutputError) error {
	r.logger.Debug("Rendering diffs to output",
		"groupCount", len(groups),
		"errorCount", len(errs),
		"useColors", r.diffOpts.UseColors,
		"compact", r.diffOpts.Compact)

	// Per-XR sections (and the aggregate footer) are only meaningful when more
	// than one input XR is present — that's what there is to disambiguate. A
	// single XR (or identity-less comp reuse) renders as a flat block, exactly
	// as before grouping was introduced.
	identityGroups := 0

	for _, g := range groups {
		if hasIdentity(g) {
			identityGroups++
		}
	}

	var err error
	if identityGroups > 1 {
		err = r.renderGrouped(groups)
	} else {
		err = r.renderFlat(flattenGroups(groups))
	}

	if err != nil {
		return err
	}

	// Write errors to stderr following Unix conventions
	for _, e := range errs {
		if _, err := fmt.Fprintln(r.diffOpts.Stderr, e.FormatError()); err != nil {
			return errors.Wrap(err, "failed to write error to stderr")
		}
	}

	return nil
}

// renderFlat renders a single flat block of diffs plus a "Summary:" line. This
// is the pre-grouping behavior, used for identity-less groups (comp reuse).
func (r *DefaultDiffRenderer) renderFlat(diffs map[string]*dt.ResourceDiff) error {
	counts, err := r.renderDiffList(diffs)
	if err != nil {
		return err
	}

	r.logger.Debug("Diff rendering complete",
		"added", counts.added,
		"removed", counts.removed,
		"modified", counts.modified,
		"equal", counts.equal,
		"output", counts.output)

	if counts.output > 0 {
		if line := summaryLine(counts); line != "" {
			if _, err := fmt.Fprintf(r.diffOpts.Stdout, "\nSummary: %s\n", line); err != nil {
				return errors.Wrap(err, "failed to write summary to output")
			}
		}
	}

	return nil
}

// renderGrouped renders each group as a per-input-XR section (header + diffs +
// per-section summary, or an inline error / "No changes."), then an aggregate
// footer. Only called when there is more than one input XR (a single XR renders
// flat), so the footer always applies. Sections are emitted in input order.
func (r *DefaultDiffRenderer) renderGrouped(groups []dt.XRDiffGroup) error {
	stdout := r.diffOpts.Stdout

	var (
		total       diffCounts
		unchangedXR int
		errorXR     int
	)

	for _, g := range groups {
		// An identity-less group (should not normally be mixed in here) has no
		// owning XR to head a section, so fold its diffs into the aggregate
		// without a header or per-section summary.
		if !hasIdentity(g) {
			counts, err := r.renderDiffList(g.Diffs)
			if err != nil {
				return err
			}

			total.added += counts.added
			total.modified += counts.modified
			total.removed += counts.removed

			continue
		}

		if _, err := fmt.Fprintf(stdout, "=== %s/%s ===\n\n", g.XR.Kind, g.XR.Name); err != nil {
			return errors.Wrap(err, "failed to write XR section header")
		}

		if g.Err != nil {
			errorXR++

			if _, err := fmt.Fprintf(stdout, "Error: %s\n\n", g.Err.Message); err != nil {
				return errors.Wrap(err, "failed to write XR error")
			}

			continue
		}

		counts, err := r.renderDiffList(g.Diffs)
		if err != nil {
			return err
		}

		total.added += counts.added
		total.modified += counts.modified
		total.removed += counts.removed

		if counts.output == 0 {
			unchangedXR++

			if _, err := fmt.Fprintf(stdout, "No changes.\n\n"); err != nil {
				return errors.Wrap(err, "failed to write no-changes message")
			}

			continue
		}

		if line := summaryLine(counts); line != "" {
			if _, err := fmt.Fprintf(stdout, "\nSummary: %s\n\n", line); err != nil {
				return errors.Wrap(err, "failed to write section summary")
			}
		}
	}

	return r.renderAggregateFooter(len(groups), total, unchangedXR, errorXR)
}

// renderAggregateFooter writes the cross-XR totals line shown when more than one
// input XR was diffed.
func (r *DefaultDiffRenderer) renderAggregateFooter(xrCount int, total diffCounts, unchangedXR, errorXR int) error {
	line := summaryLine(total)
	if line == "" {
		line = "no changes"
	}

	var qualifiers []string
	if unchangedXR > 0 {
		qualifiers = append(qualifiers, fmt.Sprintf("%d unchanged", unchangedXR))
	}

	if errorXR > 0 {
		qualifiers = append(qualifiers, fmt.Sprintf("%d error", errorXR))
	}

	suffix := ""
	if len(qualifiers) > 0 {
		suffix = fmt.Sprintf(" (%s)", strings.Join(qualifiers, ", "))
	}

	if _, err := fmt.Fprintf(r.diffOpts.Stdout, "%s\nTotal: %s across %d XRs%s\n",
		strings.Repeat("=", 80), line, xrCount, suffix); err != nil {
		return errors.Wrap(err, "failed to write aggregate footer")
	}

	return nil
}
