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

// RenderDiffs formats and prints the diffs.
// Diff output goes to r.diffOpts.Stdout, errors go to r.diffOpts.Stderr.
//
// TODO(#405, S3): render identity-bearing groups as per-XR sections. For now
// this flattens all groups and preserves the pre-change flat behavior.
func (r *DefaultDiffRenderer) RenderDiffs(groups []dt.XRDiffGroup, errs []dt.OutputError) error {
	diffs := flattenGroups(groups)

	r.logger.Debug("Rendering diffs to output",
		"diffCount", len(diffs),
		"errorCount", len(errs),
		"useColors", r.diffOpts.UseColors,
		"compact", r.diffOpts.Compact)

	stdout := r.diffOpts.Stdout
	stderr := r.diffOpts.Stderr

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

	// Add a summary to the output if there were diffs.
	if counts.output > 0 {
		if line := summaryLine(counts); line != "" {
			if _, err := fmt.Fprintf(stdout, "\nSummary: %s\n", line); err != nil {
				return errors.Wrap(err, "failed to write summary to output")
			}
		}
	}

	// Write errors to stderr following Unix conventions
	for _, e := range errs {
		if _, err := fmt.Fprintln(stderr, e.FormatError()); err != nil {
			return errors.Wrap(err, "failed to write error to stderr")
		}
	}

	return nil
}
