package renderer

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// dryRunSkipSummary is how one dt.DryRunSkipReason is summarised for a human: the warning's message,
// and the context key its DryRunInfo.Detail is reported under (none for a reason without a cause).
type dryRunSkipSummary struct {
	message   string
	detailKey string
}

// storageVersionNote is appended to the summary of additions whose CRD stores them at a version other
// than the one requested (dt.DryRunInfo.StorageVersion). Their local prediction defaulted against the
// requested version only, so the storage version's defaults, and any conversion through it, are not
// in their diffs. The versions are reported under the `versions` context key.
const storageVersionNote = "; they are stored at a version other than the one requested, whose defaults and conversion the cluster may apply but their diffs do not"

// summaryFor returns how the additions of group g are summarised, and false for a group that is not
// worth telling the user about. Additions stored at a version other than the one requested get
// storageVersionNote appended to their reason's message.
//
// dt.DryRunSkipDisabled is summarised only for additions stored at another version:
// --dry-run-on=existing is the user's own choice, so the additions it leaves unverified are not news to
// them, but that the cluster stores some at a version the prediction did not default against is.
func summaryFor(g dryRunGroup) (dryRunSkipSummary, bool) {
	var s dryRunSkipSummary

	switch g.reason {
	case dt.DryRunSkipForbidden:
		s = dryRunSkipSummary{
			message:   "skipped apiserver verification of added resources: not authorized to create them, so their diffs omit server-side defaulting and admission",
			detailKey: "reason",
		}
	case dt.DryRunSkipNamespaceNotFound:
		s = dryRunSkipSummary{
			message:   "skipped apiserver verification of added resources: their namespace does not exist yet, so their diffs omit server-side defaulting and admission",
			detailKey: "cause",
		}
	case dt.DryRunSkipWebhookUnavailable:
		s = dryRunSkipSummary{
			message:   "skipped apiserver verification of added resources: the cluster could not complete admission",
			detailKey: "cause",
		}
	case dt.DryRunSkipDisabled:
		if g.storageVersion == "" {
			return dryRunSkipSummary{}, false
		}

		// The user's own choice has no cause, so there is no detail key.
		s = dryRunSkipSummary{message: "did not verify added resources against the apiserver (--dry-run-on=existing)"}
	default:
		return dryRunSkipSummary{}, false
	}

	if g.storageVersion != "" {
		s.message += storageVersionNote
	}

	return s, true
}

// dryRunGroup is what one summary warning stands for: additions of one GVK in one namespace that were
// not verified for one reason and one cause, and are stored at one version.
//
// storageVersion never splits a group in practice: one GVK has one CRD, and so one storage version. It
// is part of the key so that the note a warning carries is always true of every resource it counts.
type dryRunGroup struct {
	gvk            string
	version        string
	namespace      string
	reason         dt.DryRunSkipReason
	detail         string
	storageVersion string
}

// dryRunWarnings derives, from the DryRunInfo carried on the supplied diffs, the warnings that tell a
// human which added resources could not be verified against the apiserver, and why.
//
// It is the only place a human sees this: the text renderer does not show DryRunInfo. So there is one
// warning per GVK + namespace + distinct cause, rather than per reason, because grouping any coarser
// would keep one cause and silently drop the rest. Each warning counts the resources behind it under
// the `count` context key, and, for additions stored at a version other than the one requested, names
// both under the `versions` context key. A resource that appears in more than one set (two overlapping inputs that
// render the same object) is one resource and is counted once.
//
// The result is sorted by GVK, namespace, reason and cause, since diffs arrive in map order. It is nil
// when there is nothing to report.
//
// The per-resource dryRun field in structured output stays the machine-readable record: a warning has
// no resource anchor, so it cannot tell a pipeline WHICH additions were degraded.
func dryRunWarnings(diffSets ...map[string]*dt.ResourceDiff) []dt.OutputWarning {
	resources := make(map[dryRunGroup]map[string]struct{})

	for _, diffs := range diffSets {
		for _, d := range diffs {
			if d.DryRun == nil {
				continue
			}

			g := dryRunGroup{
				gvk:            d.Gvk.String(),
				version:        d.Gvk.Version,
				namespace:      d.Namespace,
				reason:         d.DryRun.SkipReason,
				detail:         d.DryRun.Detail,
				storageVersion: d.DryRun.StorageVersion,
			}

			if _, summarised := summaryFor(g); !summarised {
				continue
			}

			if resources[g] == nil {
				resources[g] = make(map[string]struct{})
			}

			resources[g][d.GetDiffKey()] = struct{}{}
		}
	}

	if len(resources) == 0 {
		return nil
	}

	groups := slices.SortedFunc(maps.Keys(resources), func(a, b dryRunGroup) int {
		return cmp.Or(
			cmp.Compare(a.gvk, b.gvk),
			cmp.Compare(a.namespace, b.namespace),
			cmp.Compare(a.reason, b.reason),
			cmp.Compare(a.detail, b.detail),
			cmp.Compare(a.storageVersion, b.storageVersion),
		)
	})

	warnings := make([]dt.OutputWarning, 0, len(groups))

	for _, g := range groups {
		s, _ := summaryFor(g)

		context := map[string]string{
			"gvk":       g.gvk,
			"namespace": g.namespace,
			"count":     strconv.Itoa(len(resources[g])),
		}
		if s.detailKey != "" {
			context[s.detailKey] = g.detail
		}

		if g.storageVersion != "" {
			context["versions"] = fmt.Sprintf("requested %s, stored %s", g.version, g.storageVersion)
		}

		warnings = append(warnings, dt.OutputWarning{Message: s.message, Context: context})
	}

	return warnings
}

// xrDryRunWarnings is dryRunWarnings over the diffs an xr run emits, across every input.
//
// A failed input's diffs are withheld, so nothing is summarised for it: the summary says how far to
// trust the diffs that are shown, and a failed input shows none. Its error is reported instead.
func xrDryRunWarnings(groups []dt.XRDiffGroup) []dt.OutputWarning {
	sets := make([]map[string]*dt.ResourceDiff, 0, len(groups))
	for _, g := range groups {
		sets = append(sets, g.Diffs)
	}

	return dryRunWarnings(sets...)
}

// compDryRunWarnings is dryRunWarnings over the downstream diffs a comp run emits, across every
// composition: one run-wide summary rather than one per composition, matching where warnings[] lives.
// As for xr, a failed XR's diffs are withheld and so are not summarised.
func compDryRunWarnings(output *CompDiffOutput) []dt.OutputWarning {
	var sets []map[string]*dt.ResourceDiff

	for _, comp := range output.Compositions {
		for _, impact := range comp.ImpactAnalysis {
			sets = append(sets, impact.Diffs)
		}
	}

	return dryRunWarnings(sets...)
}

// writeWarnings writes each warning to w as its human-readable stderr line.
//
// Only warnings derived at render time belong here. Those raised during the run were written when they
// were raised (see diffprocessor.WarningLogger), and writing them again would print them twice.
func writeWarnings(w io.Writer, warnings []dt.OutputWarning) error {
	for _, warning := range warnings {
		if _, err := fmt.Fprintln(w, warning.FormatWarning()); err != nil {
			return errors.Wrap(err, "failed to write warning to stderr")
		}
	}

	return nil
}
