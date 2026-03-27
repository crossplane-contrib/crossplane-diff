package renderer

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	corev1 "k8s.io/api/core/v1"
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

// XRStatus represents the processing status of an XR in composition diffs.
type XRStatus string

const (
	// XRStatusChanged indicates the XR has downstream resource changes.
	XRStatusChanged XRStatus = "changed"
	// XRStatusUnchanged indicates the XR has no downstream resource changes.
	XRStatusUnchanged XRStatus = "unchanged"
	// XRStatusError indicates an error occurred while processing the XR.
	XRStatusError XRStatus = "error"
)

// OutputError is an alias for dt.OutputError for convenience.
// Use this type for error handling in structured output.
type OutputError = dt.OutputError

// StructuredDiffOutput represents the structured output format for diffs.
type StructuredDiffOutput struct {
	Summary Summary          `json:"summary"          yaml:"summary"`
	Changes []ChangeDetail   `json:"changes"          yaml:"changes"`
	Errors  []dt.OutputError `json:"errors,omitempty" yaml:"errors,omitempty"`
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

// CompDiffOutput is the top-level output for composition diffs (internal representation).
// This stores rich ResourceDiff data. Conversion to JSON happens in the renderer.
type CompDiffOutput struct {
	Compositions []CompositionDiff
	Errors       []dt.OutputError // top-level errors (e.g., XRs that failed impact analysis)
}

// CompositionDiff represents the diff result for a single composition (internal).
// This stores rich ResourceDiff data. Conversion to JSON happens in the renderer.
type CompositionDiff struct {
	Name              string
	Error             error            // per-composition error (nil if successful)
	CompositionDiff   *dt.ResourceDiff // the actual composition diff (nil if unchanged)
	AffectedResources AffectedResourcesSummary
	ImpactAnalysis    []XRImpact
}

// HasChanges returns true if this composition diff has any changes.
func (c *CompositionDiff) HasChanges() bool {
	if c.CompositionDiff != nil && c.CompositionDiff.DiffType != dt.DiffTypeEqual {
		return true
	}

	for _, impact := range c.ImpactAnalysis {
		if impact.Status == XRStatusChanged {
			return true
		}
	}

	return false
}

// AffectedResourcesSummary contains counts of affected resources by status.
type AffectedResourcesSummary struct {
	Total            int `json:"total"                      yaml:"total"`
	WithChanges      int `json:"withChanges"                yaml:"withChanges"`
	Unchanged        int `json:"unchanged"                  yaml:"unchanged"`
	WithErrors       int `json:"withErrors"                 yaml:"withErrors"`
	FilteredByPolicy int `json:"filteredByPolicy,omitempty" yaml:"filteredByPolicy,omitempty"`
}

// XRImpact represents the impact analysis for a single XR (internal).
// This stores rich ResourceDiff data. Conversion to JSON happens in the renderer.
// Embeds corev1.ObjectReference for the common resource identity fields.
type XRImpact struct {
	corev1.ObjectReference

	Status XRStatus
	Error  error                       // store actual error, not string
	Diffs  map[string]*dt.ResourceDiff // downstream diffs (nil if unchanged/error)
}

// --- JSON Output Types (used by StructuredCompDiffRenderer) ---

// compDiffJSONOutput is the JSON schema for composition diffs.
type compDiffJSONOutput struct {
	Compositions []compositionDiffJSON `json:"compositions"     yaml:"compositions"`
	Errors       []dt.OutputError      `json:"errors,omitempty" yaml:"errors,omitempty"`
}

type compositionDiffJSON struct {
	Name               string                   `json:"name"                         yaml:"name"`
	Error              string                   `json:"error,omitempty"              yaml:"error,omitempty"`
	CompositionChanges *ChangeDetail            `json:"compositionChanges,omitempty" yaml:"compositionChanges,omitempty"`
	AffectedResources  AffectedResourcesSummary `json:"affectedResources"            yaml:"affectedResources"`
	ImpactAnalysis     []xrImpactJSON           `json:"impactAnalysis"               yaml:"impactAnalysis"`
}

type xrImpactJSON struct {
	corev1.ObjectReference `json:",inline"`

	Status            XRStatus           `json:"status"                      yaml:"status"`
	Error             string             `json:"error,omitempty"             yaml:"error,omitempty"`
	DownstreamChanges *DownstreamChanges `json:"downstreamChanges,omitempty" yaml:"downstreamChanges,omitempty"`
}

// DownstreamChanges contains the downstream resource changes for an XR.
type DownstreamChanges struct {
	Summary Summary        `json:"summary" yaml:"summary"`
	Changes []ChangeDetail `json:"changes" yaml:"changes"`
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
func (r *StructuredDiffRenderer) RenderDiffs(stdout io.Writer, diffs map[string]*dt.ResourceDiff, errs []dt.OutputError) error {
	r.logger.Debug("Rendering diffs in structured format",
		"format", r.format,
		"diffCount", len(diffs),
		"errorCount", len(errs))

	output := r.buildStructuredOutput(diffs)
	output.Errors = errs

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

// resourceDiffToChangeDetail converts a ResourceDiff to a ChangeDetail for JSON output.
func resourceDiffToChangeDetail(diff *dt.ResourceDiff) *ChangeDetail {
	change := &ChangeDetail{
		Type:       string(diff.DiffType),
		APIVersion: diff.Gvk.GroupVersion().String(),
		Kind:       diff.Gvk.Kind,
		Name:       diff.ResourceName,
		Namespace:  diff.Namespace,
		Diff:       make(map[string]any),
	}

	// Build the diff detail structure
	switch diff.DiffType {
	case dt.DiffTypeAdded:
		if diff.Desired != nil {
			change.Diff["spec"] = diff.Desired.Object
		}
	case dt.DiffTypeRemoved:
		if diff.Current != nil {
			change.Diff["spec"] = diff.Current.Object
		}
	case dt.DiffTypeModified:
		if diff.Current != nil && diff.Desired != nil {
			change.Diff["old"] = diff.Current.Object
			change.Diff["new"] = diff.Desired.Object
		}
	case dt.DiffTypeEqual:
		// Equal diffs have no detail to show
	}

	return change
}

// buildDownstreamChanges builds DownstreamChanges from a map of ResourceDiffs.
func buildDownstreamChanges(diffs map[string]*dt.ResourceDiff) *DownstreamChanges {
	if len(diffs) == 0 {
		return nil
	}

	changes := &DownstreamChanges{
		Summary: Summary{},
		Changes: make([]ChangeDetail, 0),
	}

	for _, diff := range diffs {
		// Skip equal diffs
		if diff.DiffType == dt.DiffTypeEqual {
			continue
		}

		// Update summary counts
		switch diff.DiffType {
		case dt.DiffTypeAdded:
			changes.Summary.Added++
		case dt.DiffTypeModified:
			changes.Summary.Modified++
		case dt.DiffTypeRemoved:
			changes.Summary.Removed++
		case dt.DiffTypeEqual:
			// Equal diffs already filtered above, this case satisfies exhaustive lint check
		}

		changes.Changes = append(changes.Changes, *resourceDiffToChangeDetail(diff))
	}

	// Return nil if no non-equal changes
	if len(changes.Changes) == 0 {
		return nil
	}

	return changes
}
