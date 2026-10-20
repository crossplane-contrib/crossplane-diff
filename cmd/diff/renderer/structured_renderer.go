package renderer

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/result"
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

// OutputVersion selects the schema of structured (JSON/YAML) output. Every structured document
// names its version in its top-level outputVersion field, so a consumer can detect the shape.
type OutputVersion string

const (
	// OutputVersionV1 is the original schema: xr emits the deprecated flat changes[] beside xrs[], and
	// comp's impactAnalysis[] entries inline the XR's identity, wrap changes in downstreamChanges and
	// carry a per-XR (and per-composition) error string.
	OutputVersionV1 OutputVersion = "v1"
	// OutputVersionV2 is the converged schema: xrs[] and impactAnalysis[] hold the same entry type
	// (xrEntryWire), xr drops the flat changes[], and comp carries structured errors[] per XR and per
	// composition, with failed compositions also reported in the top-level errors[].
	OutputVersionV2 OutputVersion = "v2"
)

// OutputError is an alias for dt.OutputError for convenience.
// Use this type for error handling in structured output.
type OutputError = dt.OutputError

// StructuredDiffOutput represents the structured output format for diffs.
// Note: Only JSON tags are used because sigs.k8s.io/yaml uses JSON tags for YAML serialization.
//
// This is the v1 document (OutputVersionV1): the v2 document (structuredDiffOutputV2) plus the
// deprecated flat Changes. It is only ever derived from the v2 one, by structuredDiffOutputV2.v1.
type StructuredDiffOutput struct {
	// OutputVersion names the schema of this document.
	OutputVersion OutputVersion `json:"outputVersion"`

	// Summary counts exactly what Changes lists: each distinct change once, however many input XRs
	// reach it. Per-input counts are on each Xrs entry; where inputs overlap those can sum to more.
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
	Xrs []xrEntryWire `json:"xrs"`

	// Warnings is the list of non-fatal advisories raised during the run — things worth knowing that
	// did not invalidate the diff. Unlike Errors they do NOT affect the exit code, so a consumer
	// gating on failure should read Errors. Warnings are not grouped per input XR because they
	// originate deep in the call stack, below the point where the owning input is known. They are
	// sorted by message, then context, followed by the renderer's summary of unverified additions (see
	// dt.DryRunWarnings).
	Warnings []dt.OutputWarning `json:"warnings,omitempty"`
}

// structuredDiffOutputV2 is the v2 (OutputVersionV2) xr document, and the one the renderer always
// builds: v1 is derived from it (see v1). Its xrs[] entries are the xrEntryWire that comp's
// impactAnalysis[] also holds.
type structuredDiffOutputV2 struct {
	OutputVersion OutputVersion `json:"outputVersion"`
	// Summary counts each distinct change once, however many input XRs reach it, as in v1.
	Summary  Summary            `json:"summary"`
	Errors   []dt.OutputError   `json:"errors,omitempty"`
	Xrs      []xrEntryWire      `json:"xrs"`
	Warnings []dt.OutputWarning `json:"warnings,omitempty"`
}

// v1 projects the document to v1, which differs only by adding the deprecated flat changes[] (see
// flatChangeSet). That list is passed in because it is not recoverable from the document: it holds
// each distinct change once, however many input XRs reach it.
func (d structuredDiffOutputV2) v1(changes []ChangeDetail) StructuredDiffOutput {
	return StructuredDiffOutput{
		OutputVersion: OutputVersionV1,
		Summary:       d.Summary,
		Changes:       changes,
		Errors:        d.Errors,
		Xrs:           d.Xrs,
		Warnings:      d.Warnings,
	}
}

// xrEntryWire is the per-XR entry, shared by xr's xrs[] (in both versions) and comp's v2
// impactAnalysis[], so one parser handles both. comp's v1 entry is projected from it (see
// xrEntryWire.v1). Build it only with toXREntry.
//
// XR is projected to apiVersion/kind/name/namespace (see toXREntry). Summary and Changes are always
// present, zero and empty unless Status is changed. FilterReason and FilterDetail are set only by comp,
// for a filtered XR. Errors is set only when Status is error, and holds the same OutputError the
// command lists in its top-level errors[].
type xrEntryWire struct {
	XR           corev1.ObjectReference `json:"xr"`
	Status       dt.XRStatus            `json:"status"`
	FilterReason dt.FilterReason        `json:"filterReason,omitempty"`
	FilterDetail string                 `json:"filterDetail,omitempty"`
	Summary      Summary                `json:"summary"`
	Changes      []ChangeDetail         `json:"changes"`
	Errors       []dt.OutputError       `json:"errors,omitempty"`
}

// toXREntry builds the per-XR entry both commands emit. An errored XR carries err; a changed XR its
// non-equal diffs. Other statuses get the zero summary and an empty change list.
func toXREntry(ref corev1.ObjectReference, status dt.XRStatus, diffs map[string]*dt.ResourceDiff, err *dt.OutputError) xrEntryWire {
	// Project to exactly apiVersion/kind/name/namespace. We use ObjectReference as the type (so
	// consumers can unmarshal "xr" into the platform-standard struct), but deliberately omit its other
	// fields: fieldPath is meaningless for an XR; uid is unstable across delete/recreate and would
	// mislead consumers tracking an XR across runs (stable identity is kind/namespace/name); and
	// resourceVersion, if ever wanted, is a property of the diff run, not per-XR identity.
	entry := xrEntryWire{
		XR: corev1.ObjectReference{
			APIVersion: ref.APIVersion,
			Kind:       ref.Kind,
			Name:       ref.Name,
			Namespace:  ref.Namespace,
		},
		Status:  status,
		Changes: []ChangeDetail{},
	}

	switch status {
	case dt.XRStatusChanged:
		entry.Summary, entry.Changes = buildChangeSet(diffs)
	case dt.XRStatusError:
		if err != nil {
			entry.Errors = []dt.OutputError{*err}
		}
	case dt.XRStatusUnchanged, dt.XRStatusFiltered:
		// No changes or errors to report.
	}

	return entry
}

// xrGroupStatus derives an xr input's status: error if it failed, changed if it has any non-equal
// diff, unchanged otherwise.
func xrGroupStatus(g dt.XRDiffGroup) dt.XRStatus {
	switch {
	case g.Err != nil:
		return dt.XRStatusError
	case slices.ContainsFunc(slices.Collect(maps.Values(g.Diffs)), func(d *dt.ResourceDiff) bool { return d.DiffType != dt.DiffTypeEqual }):
		return dt.XRStatusChanged
	default:
		return dt.XRStatusUnchanged
	}
}

// buildXREntries converts the processor's per-input-XR groups into v2 xrs[] entries, in input order.
func buildXREntries(groups []dt.XRDiffGroup) []xrEntryWire {
	out := make([]xrEntryWire, 0, len(groups))

	for _, g := range groups {
		ref := corev1.ObjectReference{APIVersion: g.XR.APIVersion, Kind: g.XR.Kind, Name: g.XR.Name, Namespace: g.XR.Namespace}
		out = append(out, toXREntry(ref, xrGroupStatus(g), g.Diffs, g.Err))
	}

	return out
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
//
// This is the shared per-resource wire shape for both commands and both versions: it is reached
// via xrEntryWire.Changes, and in comp's v1 via DownstreamChanges.Changes. A field added here
// therefore surfaces in every output without per-command plumbing.
type ChangeDetail struct {
	Type       string         `json:"type"`
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Name       string         `json:"name"`
	Namespace  string         `json:"namespace,omitempty"`
	Diff       map[string]any `json:"diff"`

	// DryRun is present only when this resource's desired state was not
	// verified against the apiserver. See dt.DryRunInfo for why absence is the
	// success case and what it does not promise for removals.
	DryRun *dt.DryRunInfo `json:"dryRun,omitempty"`
}

// --- JSON Output Types (used by StructuredCompDiffRenderer) ---
// Note: Only JSON tags are used because sigs.k8s.io/yaml uses JSON tags for YAML serialization.

// compDiffWire is the serialized (JSON/YAML) v1 (OutputVersionV1) shape for composition diffs;
// compDiffWireV2 is the v2 one.
type compDiffWire struct {
	OutputVersion OutputVersion         `json:"outputVersion"`
	Compositions  []compositionDiffWire `json:"compositions"`
	Errors        []dt.OutputError      `json:"errors,omitempty"`
	Warnings      []dt.OutputWarning    `json:"warnings,omitempty"`
}

type compositionDiffWire struct {
	Name               string                      `json:"name"`
	Error              string                      `json:"error,omitempty"`
	CompositionChanges *ChangeDetail               `json:"compositionChanges,omitempty"`
	AffectedResources  dt.AffectedResourcesSummary `json:"affectedResources"`
	ImpactAnalysis     []xrImpactWire              `json:"impactAnalysis"`
	// ImpactAnalysisSkipped tells consumers that no changed/unchanged/errored verdict was reached for
	// the composites that would adopt the resulting revision, rather than that none were found. It
	// does not imply impactAnalysis is empty: composites excluded by update policy, revision selector
	// or deletion are identified without rendering, so they are still reported here and in
	// affectedResources (issue #478). Read revisionImpact.changeScope alongside it to tell "nothing
	// could have changed" from "a revision is created but was not evaluated".
	ImpactAnalysisSkipped bool `json:"impactAnalysisSkipped,omitempty"`
	// MaskedChangesOnly tells consumers that an absent compositionChanges does not mean the
	// composition is unchanged: it differs only in fields excluded from the diff.
	MaskedChangesOnly bool `json:"maskedChangesOnly,omitempty"`
	// RevisionImpact is what applying this composition does to CompositionRevisions, independent of
	// whether anything renders differently. Present whenever the comparison completed — including when
	// impactAnalysis was skipped, which is the point of it — and absent for a composition that failed
	// to process, where the `error` field says why nothing about the revision was determined.
	RevisionImpact *dt.RevisionImpact `json:"revisionImpact,omitempty"`
}

// compDiffWireV2 is the v2 (OutputVersionV2) composition diff document. Its Errors also lists each
// failed composition's error, which v1 reports only on the composition.
type compDiffWireV2 struct {
	OutputVersion OutputVersion           `json:"outputVersion"`
	Compositions  []compositionDiffWireV2 `json:"compositions"`
	Errors        []dt.OutputError        `json:"errors,omitempty"`
	Warnings      []dt.OutputWarning      `json:"warnings,omitempty"`
}

// compositionDiffWireV2 is compositionDiffWire with the per-composition error as structured errors[]
// and impactAnalysis[] entries of the shared xrEntryWire type. See compositionDiffWire for the
// meaning of the other fields.
type compositionDiffWireV2 struct {
	Name                  string                      `json:"name"`
	Errors                []dt.OutputError            `json:"errors,omitempty"`
	CompositionChanges    *ChangeDetail               `json:"compositionChanges,omitempty"`
	AffectedResources     dt.AffectedResourcesSummary `json:"affectedResources"`
	ImpactAnalysis        []xrEntryWire               `json:"impactAnalysis"`
	ImpactAnalysisSkipped bool                        `json:"impactAnalysisSkipped,omitempty"`
	MaskedChangesOnly     bool                        `json:"maskedChangesOnly,omitempty"`
	RevisionImpact        *dt.RevisionImpact          `json:"revisionImpact,omitempty"`
}

// xrImpactWire is the v1 (OutputVersionV1) impactAnalysis[] entry; v2 uses xrEntryWire instead.
type xrImpactWire struct {
	corev1.ObjectReference `json:",inline"`

	Status            dt.XRStatus        `json:"status"`
	FilterReason      dt.FilterReason    `json:"filterReason,omitempty"`
	FilterDetail      string             `json:"filterDetail,omitempty"`
	Error             string             `json:"error,omitempty"`
	DownstreamChanges *DownstreamChanges `json:"downstreamChanges,omitempty"`
}

// DownstreamChanges contains the downstream resource changes for an XR. It is part of the v1
// (OutputVersionV1) impactAnalysis[] entry only; v2's xrEntryWire carries summary and changes directly.
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
func (r *StructuredDiffRenderer) RenderDiffs(groups []dt.XRDiffGroup, errs []dt.OutputError, warnings []dt.OutputWarning) error {
	r.logger.Debug("Rendering diffs in structured format",
		"format", r.opts.Format,
		"groupCount", len(groups),
		"errorCount", len(errs),
		"warningCount", len(warnings))

	// Flat, deprecated view: every input's changes, one copy of each distinct change (see
	// flatChangeSet). The summary counts exactly what changes[] lists.
	summary, changes := flatChangeSet(groups)

	// The summary of unverified additions is derived here, from the diffs, and follows the warnings
	// raised during the run rather than being interleaved with them. See the DiffRenderer interface.
	dryRun := dt.XRDryRunWarnings(groups)
	allWarnings := append(slices.Clone(warnings), dryRun...)

	// Grouped view: one entry per input XR, in input order, each complete for its input. Where inputs
	// overlap, a shared resource appears under each, so the xrs[] summaries can sum to more than the
	// top-level summary, which counts resources rather than inputs' views of them.
	v2 := structuredDiffOutputV2{
		OutputVersion: OutputVersionV2,
		Summary:       summary,
		Errors:        errs,
		Xrs:           buildXREntries(groups),
		Warnings:      allWarnings,
	}

	// Anything but v2, including the empty value, is v1.
	var output any = v2
	if r.opts.OutputVersion != OutputVersionV2 {
		output = v2.v1(changes)
	}

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

	// Write errors to stderr for human visibility (they're also included in the structured output).
	// Of the warnings, only the ones derived here are written: the rest already went to stderr when
	// they were raised.
	if err := writeWarnings(r.opts.Stderr, dryRun); err != nil {
		return err
	}

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

// flatChangeSet builds the deprecated flat view: every group's non-equal changes, in canonical order
// (see diffSortFunc), keeping one copy of a change identical to one already listed for the same
// resource. It is lossless on its own terms: two inputs that render one resource differently are both
// listed, rather than one silently replacing the other in a map keyed by resource (issue #476). That
// keeps the flat view correct whatever the input validator allows, including a validator that treats
// inputs individually, where they may legitimately disagree about a resource. Identical renderings, the
// same change reached twice, still count once.
func flatChangeSet(groups []dt.XRDiffGroup) (Summary, []ChangeDetail) {
	var all []*dt.ResourceDiff

	for _, g := range groups {
		for _, d := range g.Diffs {
			if d.DiffType != dt.DiffTypeEqual {
				all = append(all, d)
			}
		}
	}

	// Stable, so that differing changes to one resource stay in input order.
	slices.SortStableFunc(all, diffSortFunc)

	summary := Summary{}
	changes := make([]ChangeDetail, 0, len(all))
	listed := make(map[string][]ChangeDetail, len(all))

	for _, d := range all {
		change := *resourceDiffToChangeDetail(d)
		key := d.GetDiffKey()

		if slices.ContainsFunc(listed[key], func(c ChangeDetail) bool { return reflect.DeepEqual(c, change) }) {
			continue
		}

		listed[key] = append(listed[key], change)
		summary.increment(d.DiffType)
		changes = append(changes, change)
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
		// Nil for every resource whose desired state reached the apiserver, which
		// omitempty turns into an absent key — see dt.DryRunInfo.
		DryRun: diff.DryRun,
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
