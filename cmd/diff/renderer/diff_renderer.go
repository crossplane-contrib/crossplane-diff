package renderer

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// DiffRenderer handles rendering diffs to output.
type DiffRenderer interface {
	// RenderDiffs formats and outputs diffs to the provided writer.
	// The errs parameter contains any resource processing errors to include in output.
	RenderDiffs(stdout io.Writer, diffs map[string]*dt.ResourceDiff, errs []dt.OutputError) error
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
	// Check if the name indicates a generated name (ends with "(generated)")
	if strings.HasSuffix(d.ResourceName, "(generated)") {
		return fmt.Sprintf("%s/%s", d.Gvk.Kind, d.ResourceName)
	}

	// Regular case with a specific name
	return fmt.Sprintf("%s/%s", d.Gvk.Kind, d.ResourceName)
}

// RenderDiffs formats and prints the diffs to the provided writer.
// For human-readable output, errors are written at the end after the summary.
func (r *DefaultDiffRenderer) RenderDiffs(stdout io.Writer, diffs map[string]*dt.ResourceDiff, errs []dt.OutputError) error {
	r.logger.Debug("Rendering diffs to output",
		"diffCount", len(diffs),
		"errorCount", len(errs),
		"useColors", r.diffOpts.UseColors,
		"compact", r.diffOpts.Compact)

	// Sort the keys to ensure a consistent output order
	d := slices.AppendSeq(make([]*dt.ResourceDiff, 0, len(diffs)), maps.Values(diffs))

	// Sort by GetKindName which is how it's displayed to the user
	slices.SortFunc(d, func(a, b *dt.ResourceDiff) int {
		return cmp.Compare(getKindName(a), getKindName(b))
	})

	// Track stats for summary logging
	addedCount := 0
	modifiedCount := 0
	removedCount := 0
	equalCount := 0
	outputCount := 0

	for _, diff := range d {
		resourceID := getKindName(diff)

		// Count by diff type for summary
		switch diff.DiffType {
		case dt.DiffTypeAdded:
			addedCount++
		case dt.DiffTypeRemoved:
			removedCount++
		case dt.DiffTypeModified:
			modifiedCount++
		case dt.DiffTypeEqual:
			equalCount++
			// Skip rendering equal resources
			continue
		}

		// Format the diff header based on the diff type
		var header string

		switch diff.DiffType {
		case dt.DiffTypeAdded:
			header = fmt.Sprintf("+++ %s", resourceID)
		case dt.DiffTypeRemoved:
			header = fmt.Sprintf("--- %s", resourceID)
		case dt.DiffTypeModified:
			header = fmt.Sprintf("~~~ %s", resourceID)
		case dt.DiffTypeEqual:
			// should never get here
			header = ""
		}

		// Format the diff content
		content := FormatDiff(diff.LineDiffs, r.diffOpts)

		if content != "" {
			_, err := fmt.Fprintf(stdout, "%s\n%s\n---\n", header, content)
			if err != nil {
				r.logger.Debug("Error writing diff to output", "resource", resourceID, "error", err)
				return errors.Wrap(err, "failed to write diff to output")
			}

			outputCount++
		} else {
			r.logger.Debug("Empty diff content, skipping output", "resource", resourceID)
		}
	}

	r.logger.Debug("Diff rendering complete",
		"added", addedCount,
		"removed", removedCount,
		"modified", modifiedCount,
		"equal", equalCount,
		"output", outputCount)

	// Add a summary to the output if there were diffs
	if outputCount > 0 {
		summary := strings.Builder{}
		summary.WriteString("\nSummary: ")

		if addedCount > 0 {
			fmt.Fprintf(&summary, "%d added, ", addedCount)
		}

		if modifiedCount > 0 {
			fmt.Fprintf(&summary, "%d modified, ", modifiedCount)
		}

		if removedCount > 0 {
			fmt.Fprintf(&summary, "%d removed, ", removedCount)
		}

		// Remove trailing comma and space
		summaryStr := strings.TrimSuffix(summary.String(), ", ")

		if summaryStr != "\nSummary: " {
			_, err := fmt.Fprintln(stdout, summaryStr)
			if err != nil {
				return errors.Wrap(err, "failed to write summary to output")
			}
		}
	}

	// Write errors at the end (for human-readable output)
	for _, e := range errs {
		if _, err := fmt.Fprintln(stdout, e.FormatError()); err != nil {
			return errors.Wrap(err, "failed to write error to output")
		}
	}

	return nil
}
