package renderer

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// OutputFormat represents the desired output format for diffs.
type OutputFormat string

const (
	// OutputFormatDiff is the default human-readable diff format.
	OutputFormatDiff OutputFormat = "diff"
	// OutputFormatJSON outputs structured JSON.
	OutputFormatJSON OutputFormat = "json"
	// OutputFormatYAML outputs structured YAML.
	OutputFormatYAML OutputFormat = "yaml"
)

// StructuredDiffOutput represents the structured output format for diffs.
type StructuredDiffOutput struct {
	Summary Summary        `json:"summary" yaml:"summary"`
	Changes []ChangeDetail `json:"changes" yaml:"changes"`
}

// Summary contains aggregated counts of changes.
type Summary struct {
	Added    int `json:"added"    yaml:"added"`
	Modified int `json:"modified" yaml:"modified"`
	Removed  int `json:"removed"  yaml:"removed"`
}

// ChangeDetail represents a single resource change.
type ChangeDetail struct {
	Type       string         `json:"type"                yaml:"type"`
	APIVersion string         `json:"apiVersion"          yaml:"apiVersion"`
	Kind       string         `json:"kind"                yaml:"kind"`
	Name       string         `json:"name"                yaml:"name"`
	Namespace  string         `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Diff       map[string]any `json:"diff"                yaml:"diff"`
}

// StructuredDiffRenderer renders diffs in structured formats (JSON/YAML).
type StructuredDiffRenderer struct {
	logger logging.Logger
	format OutputFormat
}

// NewStructuredDiffRenderer creates a new structured renderer with the specified format.
func NewStructuredDiffRenderer(logger logging.Logger, format OutputFormat) DiffRenderer {
	return &StructuredDiffRenderer{
		logger: logger,
		format: format,
	}
}

// RenderDiffs renders the diffs in the configured structured format.
func (r *StructuredDiffRenderer) RenderDiffs(stdout io.Writer, diffs map[string]*dt.ResourceDiff) error {
	r.logger.Debug("Rendering diffs in structured format",
		"format", r.format,
		"diffCount", len(diffs))

	output := r.buildStructuredOutput(diffs)

	var (
		data []byte
		err  error
	)

	switch r.format {
	case OutputFormatJSON:
		data, err = json.MarshalIndent(output, "", "  ")
	case OutputFormatYAML:
		data, err = sigsyaml.Marshal(output)
	case OutputFormatDiff:
		return errors.Errorf("unsupported output format for structured renderer: %s", r.format)
	}

	if err != nil {
		return errors.Wrap(err, "failed to marshal diff output")
	}

	_, err = stdout.Write(data)
	if err != nil {
		return errors.Wrap(err, "failed to write structured output")
	}

	// Add newline for cleaner terminal output
	_, err = stdout.Write([]byte("\n"))
	if err != nil {
		return errors.Wrap(err, "failed to write newline")
	}

	return nil
}

// buildStructuredOutput converts ResourceDiff map into structured output format.
func (r *StructuredDiffRenderer) buildStructuredOutput(diffs map[string]*dt.ResourceDiff) StructuredDiffOutput {
	output := StructuredDiffOutput{
		Summary: Summary{},
		Changes: []ChangeDetail{},
	}

	// Sort diffs for consistent output
	sortedDiffs := slices.AppendSeq(make([]*dt.ResourceDiff, 0, len(diffs)), maps.Values(diffs))
	slices.SortFunc(sortedDiffs, func(a, b *dt.ResourceDiff) int {
		aKey := fmt.Sprintf("%s/%s", a.Gvk.Kind, a.ResourceName)

		bKey := fmt.Sprintf("%s/%s", b.Gvk.Kind, b.ResourceName)
		if aKey != bKey {
			return compareStrings(aKey, bKey)
		}

		return 0
	})

	for _, diff := range sortedDiffs {
		// Skip equal resources
		if diff.DiffType == dt.DiffTypeEqual {
			continue
		}

		// Update summary counts
		switch diff.DiffType {
		case dt.DiffTypeAdded:
			output.Summary.Added++
		case dt.DiffTypeModified:
			output.Summary.Modified++
		case dt.DiffTypeRemoved:
			output.Summary.Removed++
		case dt.DiffTypeEqual:
			// Equal diffs are filtered above, this case satisfies exhaustive lint check
		}

		// Extract namespace from resource if available
		var namespace string
		if diff.Desired != nil {
			namespace = diff.Desired.GetNamespace()
		} else if diff.Current != nil {
			namespace = diff.Current.GetNamespace()
		}

		// Build change detail
		change := ChangeDetail{
			Type:       string(diff.DiffType),
			APIVersion: diff.Gvk.GroupVersion().String(),
			Kind:       diff.Gvk.Kind,
			Name:       diff.ResourceName,
			Namespace:  namespace,
			Diff:       r.buildDiffDetail(diff),
		}

		output.Changes = append(output.Changes, change)
	}

	return output
}

// buildDiffDetail creates the diff detail structure for a resource change.
func (r *StructuredDiffRenderer) buildDiffDetail(diff *dt.ResourceDiff) map[string]any {
	detail := make(map[string]any)

	switch diff.DiffType {
	case dt.DiffTypeAdded:
		// For added resources, include the full spec
		if diff.Desired != nil {
			detail["spec"] = diff.Desired.Object
		}

	case dt.DiffTypeRemoved:
		// For removed resources, include the old spec
		if diff.Current != nil {
			detail["spec"] = diff.Current.Object
		}

	case dt.DiffTypeEqual:
		// Equal diffs have no detail to show

	case dt.DiffTypeModified:
		// For modified resources, show both old and new
		if diff.Current != nil && diff.Desired != nil {
			detail["old"] = diff.Current.Object
			detail["new"] = diff.Desired.Object
		}
	}

	return detail
}

// compareStrings provides a simple string comparison for sorting.
func compareStrings(a, b string) int {
	if a < b {
		return -1
	}

	if a > b {
		return 1
	}

	return 0
}
