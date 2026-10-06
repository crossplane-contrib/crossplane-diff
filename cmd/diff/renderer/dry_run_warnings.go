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
// and the context key its DryRunInfo.Detail is reported under.
type dryRunSkipSummary struct {
	message   string
	detailKey string
}

// summaryFor returns how additions skipped for reason are summarised, and false for a reason that is
// not worth telling the user about.
//
// dt.DryRunSkipDisabled is deliberately not summarised: --dry-run-on=existing is the user's own choice,
// so the additions it leaves unverified are not news to them.
func summaryFor(reason dt.DryRunSkipReason) (dryRunSkipSummary, bool) {
	switch reason {
	case dt.DryRunSkipForbidden:
		return dryRunSkipSummary{
			message:   "skipped apiserver verification of added resources: not authorized to create them, so their diffs omit server-side defaulting and admission",
			detailKey: "reason",
		}, true
	case dt.DryRunSkipNamespaceNotFound:
		return dryRunSkipSummary{
			message:   "skipped apiserver verification of added resources: their namespace does not exist yet, so their diffs omit server-side defaulting and admission",
			detailKey: "cause",
		}, true
	case dt.DryRunSkipWebhookUnavailable:
		return dryRunSkipSummary{
			message:   "skipped apiserver verification of added resources: the cluster could not complete admission",
			detailKey: "cause",
		}, true
	case dt.DryRunSkipDisabled:
		return dryRunSkipSummary{}, false
	default:
		return dryRunSkipSummary{}, false
	}
}

// dryRunGroup is what one summary warning stands for: additions of one GVK in one namespace that were
// not verified for one reason and one cause.
type dryRunGroup struct {
	gvk       string
	namespace string
	reason    dt.DryRunSkipReason
	detail    string
}

// dryRunWarnings derives, from the DryRunInfo carried on the supplied diffs, the warnings that tell a
// human which added resources could not be verified against the apiserver, and why.
//
// It is the only place a human sees this: the text renderer does not show DryRunInfo. So there is one
// warning per GVK + namespace + distinct cause, rather than per reason, because grouping any coarser
// would keep one cause and silently drop the rest. Each warning counts the resources behind it under
// the `count` context key. A resource that appears in more than one set (two overlapping inputs that
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

			if _, summarised := summaryFor(d.DryRun.SkipReason); !summarised {
				continue
			}

			g := dryRunGroup{gvk: d.Gvk.String(), namespace: d.Namespace, reason: d.DryRun.SkipReason, detail: d.DryRun.Detail}
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
		)
	})

	warnings := make([]dt.OutputWarning, 0, len(groups))

	for _, g := range groups {
		s, _ := summaryFor(g.reason)
		warnings = append(warnings, dt.OutputWarning{
			Message: s.message,
			Context: map[string]string{
				"gvk":       g.gvk,
				"namespace": g.namespace,
				s.detailKey: g.detail,
				"count":     strconv.Itoa(len(resources[g])),
			},
		})
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
