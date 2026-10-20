package result

import (
	corev1 "k8s.io/api/core/v1"
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
	// FilterReasonDeleting indicates the XR was excluded because it has a metadata.deletionTimestamp.
	// Crossplane's composite reconciler takes its deletion path for such an XR — it tears down
	// composed resources rather than composing them — so the XR will never adopt the resulting
	// revision, and rendering it produces no composed resources to diff against.
	FilterReasonDeleting FilterReason = "deleting"
)

// CompDiffOutput is the top-level output for composition diffs (internal representation).
// This stores rich ResourceDiff data. Conversion to JSON happens in the renderer.
type CompDiffOutput struct {
	Compositions []CompositionDiff
	Errors       []OutputError // top-level errors (e.g., XRs that failed impact analysis)
	// Warnings are non-fatal advisories raised during the run. Like the xr command's, they do not
	// affect the exit code and are not attributed to a single composition — they are raised below the
	// point where the owning composition is known. They have already been written to stderr when
	// raised; this field carries them into structured output. The summary of unverified additions is
	// not among them: the renderer derives it from the diffs (see CompDryRunWarnings).
	Warnings []OutputWarning
}

// CompositionDiff represents the diff result for a single composition (internal).
// This stores rich ResourceDiff data. Conversion to JSON happens in the renderer.
type CompositionDiff struct {
	Name string
	// Error is the per-composition processing error (nil if successful). Its ResourceID is
	// "Composition/<name>".
	Error             *OutputError
	CompositionDiff   *ResourceDiff // the actual composition diff (nil if unchanged)
	AffectedResources AffectedResourcesSummary
	ImpactAnalysis    []XRImpact
	// ImpactAnalysisSkipped records that the affected XRs were deliberately not evaluated, because
	// the composition changed by less than --analyze-on asked to analyse. Distinguishes "we did not
	// look" from "we looked and found no affected XRs", which would otherwise be indistinguishable.
	//
	// It does not mean ImpactAnalysis is empty. Composites excluded by update policy, revision
	// selector or deletion are identified locally, with no render, so they are reported whether or not
	// the analysis ran — in --resource mode as XRStatusFiltered entries here, and always via the
	// AffectedResources filter counters (issue #478). What a skip suppresses is the renders, and
	// therefore any changed/unchanged/errored verdict about the composites that were kept.
	//
	// Why the composites went unevaluated is not recoverable from this field alone — read
	// RevisionImpact.ChangeScope alongside it. A "none" scope means no CompositionRevision is created
	// and so nothing could have changed; anything else means a revision is created and simply was not
	// evaluated.
	ImpactAnalysisSkipped bool
	// MaskedChangesOnly records that the composition differs from the cluster's only in fields
	// excluded from the rendered diff — the user's --ignore-paths, or the comparison's display-only
	// suppressions. CompositionDiff is nil in that case, but the composition did change, so the
	// renderer must not report it as unchanged.
	MaskedChangesOnly bool
	// RevisionImpact is what applying this composition does to CompositionRevisions, independent of
	// whether anything renders differently. Populated whenever the comparison completed, including
	// when the composites were not evaluated; left zero for a composition that failed to process,
	// where nothing about it was established (see RevisionImpact.Determined).
	RevisionImpact RevisionImpact
}

// RevisionImpact describes what applying a composition does to CompositionRevisions and the
// composites tracking them, independent of whether any composed resource renders differently.
//
// This is deliberately a typed field rather than a warning. Revision churn is a per-composition
// fact a CI consumer may want to gate on, and warnings are documented as neither attributed to a
// composition nor intended for gating — so a flat warning could not say which of several diffed
// compositions creates a revision.
type RevisionImpact struct {
	// ChangeScope is how much of the composition differs, in the terms Crossplane uses to decide
	// whether a revision is needed: "none", "metadata" or "spec". Crossplane's Composition.Hash()
	// covers labels and annotations as well as spec, so "metadata" still means a revision is created.
	ChangeScope string `json:"changeScope"`
	// CreatesRevision records whether applying this composition produces a new CompositionRevision.
	CreatesRevision bool `json:"createsRevision"`
	// RepointedComposites counts the composites that would adopt that revision and reconcile as a
	// result: those with an Automatic (or defaulted) compositionUpdatePolicy whose
	// compositionRevisionSelector, if any, selects the resulting revision, and which are not being
	// deleted. A Manual-policy composite is never counted — it is pinned by compositionRevisionRef, so
	// --include-manual keeping it for analysis does not make it re-point (issue #479).
	//
	// Note this counts composites that re-point, which is not the same as composites whose rendered
	// output changes; re-pointing alone usually renders identically.
	RepointedComposites int `json:"repointedComposites"`
	// PredictedRevisionName is what that revision would be called: "<composition>-<hash[:7]>", derived
	// from Crossplane's own Composition.Hash(). When CreatesRevision is false it names the existing
	// revision the composites already track, which is the same name by construction — an unchanged
	// composition hashes to the same value.
	//
	// It is what makes a downstream change interpretable: a composed resource whose template reads the
	// revision name shows a diff with no other visible cause, and this is the cause. The composites are
	// rendered with this value seeded onto their compositionRevisionRef, so such a change is detected
	// rather than assumed away.
	//
	// Absent with CreatesRevision true means the name could not be predicted, which happens when the
	// composition differs from the cluster's only by kubectl's last-applied-configuration annotation:
	// its post-apply value is a function of how the user applies rather than of the file, so the hash —
	// and therefore the name — is unknowable. A warning says so, and nothing is seeded in that case, so
	// a revision-observing change would go undetected. Note also that for a composition applied
	// client-side *with* other edits the suffix is predicted from the file as supplied and so may differ
	// from the eventual one; the change is still detected, its predicted value is just imprecise.
	PredictedRevisionName string `json:"predictedRevisionName,omitempty"`
}

// Determined reports whether the comparison that populates a RevisionImpact actually ran. ChangeScope
// is set unconditionally alongside the other fields and every real scope is non-empty, so an empty
// scope means nothing here was established. That is the case for a composition that failed to process:
// serializing the zero value would put changeScope outside its documented enum and would assert
// createsRevision: false, a positive claim about an apply the tool never evaluated (issue #479).
func (i RevisionImpact) Determined() bool {
	return i.ChangeScope != ""
}

// HasChanges returns true if this composition diff has any changes, which is what drives
// ExitCodeDiffDetected.
//
// Deliberately excluded: RevisionImpact. A composition whose only difference is in masked fields
// creates a CompositionRevision but has nothing to show and nothing rendering differently, and a
// GitOps loop re-applying the same manifests would otherwise fail its diff gate on every run. A
// consumer that does want to gate on revision churn reads revisionImpact.createsRevision from the
// structured output.
func (c *CompositionDiff) HasChanges() bool {
	if c.CompositionDiff != nil && c.CompositionDiff.DiffType != DiffTypeEqual {
		return true
	}

	for _, impact := range c.ImpactAnalysis {
		if impact.Status == XRStatusChanged {
			return true
		}
	}

	return false
}

// HasChanges returns true if any composition in the output has changes (see CompositionDiff.HasChanges).
func (o *CompDiffOutput) HasChanges() bool {
	for i := range o.Compositions {
		if o.Compositions[i].HasChanges() {
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
	// FilteredByDeletion counts XRs excluded because they are being deleted
	// (FilterReasonDeleting). Kept separate from the other filter counters for the same reason:
	// the breakdown stays visible in default-discovery mode.
	FilteredByDeletion int `json:"filteredByDeletion,omitempty"`
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
	// Error is the structured processing error for an XRStatusError impact (nil otherwise), the same
	// OutputError the command lists in its top-level errors.
	Error *OutputError
	Diffs map[string]*ResourceDiff // downstream diffs (nil if unchanged/error)
}
