package renderer

import (
	"cmp"
	"encoding/json"
	"fmt"
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
	// XRStatusFiltered indicates the XR matched the composition by name but was excluded from
	// evaluation because it would not adopt the composition change being diffed. The specific cause
	// is carried separately in XRImpact.FilterReason (outcome and reason are intentionally divorced
	// so the reason set can grow without expanding the status enum). The XR is surfaced in impact
	// analysis with no downstream changes so users see the skip explicitly.
	XRStatusFiltered XRStatus = "filtered"
)

// FilterReason explains why an XRImpact has XRStatusFiltered. It is only meaningful when
// XRImpact.Status == XRStatusFiltered.
type FilterReason string

const (
	// FilterReasonManualPolicy indicates the XR was excluded because it has a Manual
	// compositionUpdatePolicy and --include-manual was not set. Such XRs are pinned to a specific
	// revision and would not adopt the composition change automatically.
	FilterReasonManualPolicy FilterReason = "manual_policy"
	// FilterReasonRevisionSelectorMismatch indicates the XR was excluded because it has an Automatic
	// compositionUpdatePolicy with a compositionRevisionSelector that does not match the labels of
	// the composition change being diffed. Such XRs would not select the resulting revision.
	FilterReasonRevisionSelectorMismatch FilterReason = "revision_selector_mismatch"
)

// OutputError is an alias for dt.OutputError for convenience.
// Use this type for error handling in structured output.
type OutputError = dt.OutputError

// StructuredDiffOutput represents the structured output format for diffs.
// Note: Only JSON tags are used because sigs.k8s.io/yaml uses JSON tags for YAML serialization.
type StructuredDiffOutput struct {
	// Summary is the aggregate change count across every input XR.
	Summary Summary `json:"summary"`

	// Changes is the flat, ungrouped list of all resource changes across every
	// input XR.
	//
	// Deprecated: use Xrs for per-input-XR grouping. This field is retained for
	// backward compatibility and will be removed in a future major release.
	Changes []ChangeDetail `json:"changes"`

	// Errors is the union of resource-processing errors across all input XRs
	// (the top-level/global error list). Each errored XR also surfaces its
	// error inside its own Xrs entry; this list stays complete for consumers
	// that read errors here.
	Errors []dt.OutputError `json:"errors,omitempty"`

	// Xrs groups changes by the input XR/claim that produced them, one entry
	// per input in input order. This is the recommended view; the flat Changes
	// field above is deprecated.
	Xrs []xrDiffWire `json:"xrs"`
}

// xrIdentity identifies an input XR/claim in structured output. It is a
// deliberately small projection of the input's metadata (not the full
// corev1.ObjectReference) so the JSON schema stays stable and free of
// server-side fields like UID or resourceVersion.
type xrIdentity struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
}

// xrDiffWire is the per-input-XR entry in StructuredDiffOutput.Xrs. It carries
// the input XR's identity, its processing status, and its own summary,
// changes, and errors.
type xrDiffWire struct {
	XR      xrIdentity       `json:"xr"`
	Status  XRStatus         `json:"status"`
	Summary Summary          `json:"summary"`
	Changes []ChangeDetail   `json:"changes"`
	Errors  []dt.OutputError `json:"errors,omitempty"`
}

// Summary contains aggregated counts of changes.
type Summary struct {
	Added    int `json:"added"`
	Modified int `json:"modified"`
	Removed  int `json:"removed"`
}

// increment bumps the counter for the given diff type. Equal (and any unknown)
// types are no-ops, since equal diffs are excluded from structured output; the
// explicit case keeps the exhaustive-switch linter satisfied.
func (s *Summary) increment(t dt.DiffType) {
	switch t {
	case dt.DiffTypeAdded:
		s.Added++
	case dt.DiffTypeModified:
		s.Modified++
	case dt.DiffTypeRemoved:
		s.Removed++
	case dt.DiffTypeEqual:
		// Equal diffs are not counted.
	}
}

// ChangeDetail represents a single resource change.
type ChangeDetail struct {
	Type       string         `json:"type"`
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Name       string         `json:"name"`
	Namespace  string         `json:"namespace,omitempty"`
	Diff       map[string]any `json:"diff"`
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
	Total       int `json:"total"`
	WithChanges int `json:"withChanges"`
	Unchanged   int `json:"unchanged"`
	WithErrors  int `json:"withErrors"`
	// FilteredByPolicy counts XRs excluded because of a Manual compositionUpdatePolicy
	// (FilterReasonManualPolicy).
	FilteredByPolicy int `json:"filteredByPolicy,omitempty"`
	// FilteredBySelector counts XRs excluded because their compositionRevisionSelector does not match
	// the diffed composition's labels (FilterReasonRevisionSelectorMismatch). Kept separate from
	// FilteredByPolicy so the breakdown is visible even in default-discovery mode, where individual
	// XR impacts are not surfaced.
	FilteredBySelector int `json:"filteredBySelector,omitempty"`
}

// XRImpact represents the impact analysis for a single XR (internal).
// This stores rich ResourceDiff data. Conversion to JSON happens in the renderer.
// Embeds corev1.ObjectReference for the common resource identity fields.
type XRImpact struct {
	corev1.ObjectReference

	Status XRStatus
	// FilterReason explains a Status == XRStatusFiltered outcome; empty otherwise.
	FilterReason FilterReason
	// FilterDetail is an optional human-readable explanation for a filtered outcome (e.g. which
	// selector failed to match which labels), surfaced to help users self-diagnose the exclusion.
	FilterDetail string
	Error        error                       // store actual error, not string
	Diffs        map[string]*dt.ResourceDiff // downstream diffs (nil if unchanged/error)
}

// --- JSON Output Types (used by StructuredCompDiffRenderer) ---
// Note: Only JSON tags are used because sigs.k8s.io/yaml uses JSON tags for YAML serialization.

// compDiffWire is the serialized (JSON/YAML) shape for composition diffs.
type compDiffWire struct {
	Compositions []compositionDiffWire `json:"compositions"`
	Errors       []dt.OutputError      `json:"errors,omitempty"`
}

type compositionDiffWire struct {
	Name               string                   `json:"name"`
	Error              string                   `json:"error,omitempty"`
	CompositionChanges *ChangeDetail            `json:"compositionChanges,omitempty"`
	AffectedResources  AffectedResourcesSummary `json:"affectedResources"`
	ImpactAnalysis     []xrImpactWire           `json:"impactAnalysis"`
}

type xrImpactWire struct {
	corev1.ObjectReference `json:",inline"`

	Status            XRStatus           `json:"status"`
	FilterReason      FilterReason       `json:"filterReason,omitempty"`
	FilterDetail      string             `json:"filterDetail,omitempty"`
	Error             string             `json:"error,omitempty"`
	DownstreamChanges *DownstreamChanges `json:"downstreamChanges,omitempty"`
}

// DownstreamChanges contains the downstream resource changes for an XR.
type DownstreamChanges struct {
	Summary Summary        `json:"summary"`
	Changes []ChangeDetail `json:"changes"`
}

// StructuredDiffRenderer renders diffs in structured formats (JSON/YAML).
type StructuredDiffRenderer struct {
	logger logging.Logger
	opts   DiffOptions
}

// NewStructuredDiffRenderer creates a new structured renderer with the specified format.
func NewStructuredDiffRenderer(logger logging.Logger, opts DiffOptions) DiffRenderer {
	return &StructuredDiffRenderer{
		logger: logger,
		opts:   opts,
	}
}

// RenderDiffs renders the diffs in the configured structured format.
//
// It emits two views built from the same groups: the deprecated flat
// changes[]/summary (merged across all groups) for backward compatibility, and
// the per-input-XR xrs[] grouping. The top-level errors[] is the union passed
// by the caller.
func (r *StructuredDiffRenderer) RenderDiffs(groups []dt.XRDiffGroup, errs []dt.OutputError) error {
	r.logger.Debug("Rendering diffs in structured format",
		"format", r.opts.Format,
		"groupCount", len(groups),
		"errorCount", len(errs))

	// Flat, deprecated view: merge all groups' diffs.
	summary, changes := buildChangeSet(flattenGroups(groups))
	output := StructuredDiffOutput{Summary: summary, Changes: changes}
	output.Errors = errs

	// Grouped view: one entry per input XR, in input order.
	output.Xrs = buildXRGroups(groups)

	var (
		data []byte
		err  error
	)

	switch r.opts.Format {
	case OutputFormatJSON:
		data, err = json.MarshalIndent(output, "", "  ")
	case OutputFormatYAML:
		data, err = sigsyaml.Marshal(output)
	case OutputFormatDiff:
		return errors.Errorf("unsupported output format for structured renderer: %s", r.opts.Format)
	}

	if err != nil {
		return errors.Wrap(err, "failed to marshal diff output")
	}

	_, err = r.opts.Stdout.Write(data)
	if err != nil {
		return errors.Wrap(err, "failed to write structured output")
	}

	// Add newline for cleaner terminal output
	_, err = r.opts.Stdout.Write([]byte("\n"))
	if err != nil {
		return errors.Wrap(err, "failed to write newline")
	}

	// Write errors to stderr for human visibility (they're also included in the structured output)
	for _, e := range errs {
		if _, err := fmt.Fprintln(r.opts.Stderr, e.FormatError()); err != nil {
			return errors.Wrap(err, "failed to write error to stderr")
		}
	}

	return nil
}

// buildChangeSet walks a diff map into the (summary, changes) pair shared by
// every structured view: the flat top-level changes[], each xrs[] entry, and
// each comp downstreamChanges block. Equal diffs are skipped (they contribute
// to neither the summary nor the change list). Changes are sorted by the
// canonical order (see diffSortFunc) for deterministic output.
func buildChangeSet(diffs map[string]*dt.ResourceDiff) (Summary, []ChangeDetail) {
	sorted := slices.AppendSeq(make([]*dt.ResourceDiff, 0, len(diffs)), maps.Values(diffs))
	slices.SortFunc(sorted, diffSortFunc)

	summary := Summary{}
	changes := make([]ChangeDetail, 0, len(sorted))

	for _, diff := range sorted {
		if diff.DiffType == dt.DiffTypeEqual {
			continue
		}

		summary.increment(diff.DiffType)
		changes = append(changes, *resourceDiffToChangeDetail(diff))
	}

	return summary, changes
}

// diffSortFunc is the canonical ordering for structured change lists: primarily
// by the human-meaningful Kind/Name (matching the human renderer's getKindName
// sort), with the full apiVersion/kind/namespace/name key as a stable
// tiebreaker so same-kind/same-name resources in different namespaces still
// sort deterministically.
func diffSortFunc(a, b *dt.ResourceDiff) int {
	return cmp.Or(
		cmp.Compare(a.Gvk.Kind, b.Gvk.Kind),
		cmp.Compare(a.ResourceName, b.ResourceName),
		cmp.Compare(a.GetDiffKey(), b.GetDiffKey()),
	)
}

// resourceDiffToChangeDetail converts a ResourceDiff to a ChangeDetail for
// structured (JSON/YAML) output.
//
// It reads the pre-cleaned views populated during diff generation, so
// --ignore-paths and the unconditional-cleanup fields are already stripped;
// the renderer performs no cleanup of its own.
func resourceDiffToChangeDetail(diff *dt.ResourceDiff) *ChangeDetail {
	change := &ChangeDetail{
		Type:       diff.DiffType.ToWord(),
		APIVersion: diff.Gvk.GroupVersion().String(),
		Kind:       diff.Gvk.Kind,
		Name:       diff.ResourceName,
		Namespace:  diff.Namespace,
		Diff:       make(map[string]any),
	}

	switch diff.DiffType {
	case dt.DiffTypeAdded:
		if diff.Desired.Clean != nil {
			change.Diff[dt.DiffKeySpec] = diff.Desired.Clean.Object
		}
	case dt.DiffTypeRemoved:
		if diff.Current.Clean != nil {
			change.Diff[dt.DiffKeySpec] = diff.Current.Clean.Object
		}
	case dt.DiffTypeModified:
		if diff.Current.Clean != nil && diff.Desired.Clean != nil {
			change.Diff[dt.DiffKeyOld] = diff.Current.Clean.Object
			change.Diff[dt.DiffKeyNew] = diff.Desired.Clean.Object
		}
	case dt.DiffTypeEqual:
		// Equal diffs have no detail to show
	}

	return change
}

// buildXRGroups converts the processor's per-input-XR groups into the xrs[]
// structured view, preserving input order. Each entry gets its identity, a
// derived status, and (for changed XRs) its own summary and changes; errored
// XRs carry their error.
func buildXRGroups(groups []dt.XRDiffGroup) []xrDiffWire {
	out := make([]xrDiffWire, 0, len(groups))

	for _, g := range groups {
		entry := xrDiffWire{
			XR: xrIdentity{
				APIVersion: g.XR.APIVersion,
				Kind:       g.XR.Kind,
				Name:       g.XR.Name,
				Namespace:  g.XR.Namespace,
			},
			Changes: []ChangeDetail{},
		}

		switch {
		case g.Err != nil:
			entry.Status = XRStatusError
			entry.Errors = []dt.OutputError{*g.Err}
		default:
			// buildDownstreamChanges returns nil when there are no non-equal
			// diffs, which is exactly the "unchanged" case.
			if changes := buildDownstreamChanges(g.Diffs); changes != nil {
				entry.Status = XRStatusChanged
				entry.Summary = changes.Summary
				entry.Changes = changes.Changes
			} else {
				entry.Status = XRStatusUnchanged
			}
		}

		out = append(out, entry)
	}

	return out
}

// buildDownstreamChanges builds DownstreamChanges from a map of ResourceDiffs,
// returning nil when there are no non-equal changes (the "unchanged" case its
// callers rely on to derive status).
func buildDownstreamChanges(diffs map[string]*dt.ResourceDiff) *DownstreamChanges {
	summary, changes := buildChangeSet(diffs)
	if len(changes) == 0 {
		return nil
	}

	return &DownstreamChanges{Summary: summary, Changes: changes}
}
