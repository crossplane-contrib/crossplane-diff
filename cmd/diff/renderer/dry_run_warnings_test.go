package renderer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	forbiddenSummary = "skipped apiserver verification of added resources: not authorized to create them, so their diffs omit server-side defaulting and admission"
	namespaceSummary = "skipped apiserver verification of added resources: their namespace does not exist yet, so their diffs omit server-side defaulting and admission"
	webhookSummary   = "skipped apiserver verification of added resources: the cluster could not complete admission"
	testResourceGVK  = "example.org/v1, Kind=TestResource"
	otherResourceGVK = "example.org/v1, Kind=OtherResource"
)

// unverifiedAddition builds the diff of an added resource whose dry-run create was skipped for reason,
// with the cluster's explanation detail.
func unverifiedAddition(kind, name, namespace string, reason dt.DryRunSkipReason, detail string) *dt.ResourceDiff {
	return &dt.ResourceDiff{
		Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: kind},
		Namespace:    namespace,
		ResourceName: name,
		DiffType:     dt.DiffTypeAdded,
		DryRun:       &dt.DryRunInfo{SkipReason: reason, Detail: detail},
	}
}

// diffMap keys diffs the way the diff calculator does.
func diffMap(diffs ...*dt.ResourceDiff) map[string]*dt.ResourceDiff {
	out := make(map[string]*dt.ResourceDiff, len(diffs))
	for _, d := range diffs {
		out[d.GetDiffKey()] = d
	}

	return out
}

// TestDryRunWarnings covers how the advisory for unverified additions is derived from the DryRunInfo
// on the diffs: one warning per GVK + namespace + distinct cause, counting the resources behind it, and
// no cause ever dropped. The text renderer shows no DryRunInfo, so this warning is the only place a
// human learns that an addition was not verified, or why.
func TestDryRunWarnings(t *testing.T) {
	tests := map[string]struct {
		reason   string
		diffSets []map[string]*dt.ResourceDiff
		want     []dt.OutputWarning
	}{
		"ForbiddenCollapsesPerGVKAndNamespace": {
			reason: "Four denied additions across three GVK+namespace groups raise three warnings, each counting its resources.",
			diffSets: []map[string]*dt.ResourceDiff{diffMap(
				unverifiedAddition("TestResource", "a", "ns-a", dt.DryRunSkipForbidden, "no create"),
				unverifiedAddition("TestResource", "b", "ns-a", dt.DryRunSkipForbidden, "no create"),
				unverifiedAddition("TestResource", "c", "ns-b", dt.DryRunSkipForbidden, "no create"),
				unverifiedAddition("OtherResource", "d", "ns-a", dt.DryRunSkipForbidden, "no create"),
			)},
			want: []dt.OutputWarning{
				{Message: forbiddenSummary, Context: map[string]string{"gvk": otherResourceGVK, "namespace": "ns-a", "reason": "no create", "count": "1"}},
				{Message: forbiddenSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "reason": "no create", "count": "2"}},
				{Message: forbiddenSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-b", "reason": "no create", "count": "1"}},
			},
		},
		"DistinctCausesAreAllKept": {
			reason: "Two different apiserver causes in one GVK+namespace group are two warnings. Grouping on reason alone would keep one and silently drop the other.",
			diffSets: []map[string]*dt.ResourceDiff{diffMap(
				unverifiedAddition("TestResource", "a", "ns-a", dt.DryRunSkipWebhookUnavailable, `failed calling webhook "policy.example.org"`),
				unverifiedAddition("TestResource", "b", "ns-a", dt.DryRunSkipWebhookUnavailable, `failed calling webhook "quota.example.org"`),
				unverifiedAddition("TestResource", "c", "ns-a", dt.DryRunSkipWebhookUnavailable, `failed calling webhook "policy.example.org"`),
			)},
			want: []dt.OutputWarning{
				{Message: webhookSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "cause": `failed calling webhook "policy.example.org"`, "count": "2"}},
				{Message: webhookSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "cause": `failed calling webhook "quota.example.org"`, "count": "1"}},
			},
		},
		"MissingNamespaceAcrossInputsCountsEachResourceOnce": {
			reason: "Additions from several inputs share one warning. A resource two overlapping inputs both render is one resource, so it is counted once.",
			diffSets: []map[string]*dt.ResourceDiff{
				diffMap(
					unverifiedAddition("TestResource", "a", "ns-a", dt.DryRunSkipNamespaceNotFound, `namespaces "ns-a" not found`),
					unverifiedAddition("TestResource", "shared", "ns-a", dt.DryRunSkipNamespaceNotFound, `namespaces "ns-a" not found`),
				),
				diffMap(
					unverifiedAddition("TestResource", "b", "ns-a", dt.DryRunSkipNamespaceNotFound, `namespaces "ns-a" not found`),
					unverifiedAddition("TestResource", "shared", "ns-a", dt.DryRunSkipNamespaceNotFound, `namespaces "ns-a" not found`),
				),
			},
			want: []dt.OutputWarning{
				{Message: namespaceSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "cause": `namespaces "ns-a" not found`, "count": "3"}},
			},
		},
		"ClusterScopedResourceKeepsAnEmptyNamespace": {
			reason: "A cluster-scoped addition still carries the namespace key, empty, so every summary has the same context keys.",
			diffSets: []map[string]*dt.ResourceDiff{diffMap(
				unverifiedAddition("TestResource", "a", "", dt.DryRunSkipForbidden, "no create"),
			)},
			want: []dt.OutputWarning{
				{Message: forbiddenSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "", "reason": "no create", "count": "1"}},
			},
		},
		"ReasonsAreOrderedDeterministically": {
			reason: "Diffs arrive in map order, so the summary is sorted: by GVK, then namespace, then reason, then cause.",
			diffSets: []map[string]*dt.ResourceDiff{diffMap(
				unverifiedAddition("TestResource", "a", "ns-a", dt.DryRunSkipWebhookUnavailable, "timeout"),
				unverifiedAddition("TestResource", "b", "ns-a", dt.DryRunSkipForbidden, "no create"),
				unverifiedAddition("TestResource", "c", "ns-a", dt.DryRunSkipNamespaceNotFound, "gone"),
			)},
			want: []dt.OutputWarning{
				{Message: forbiddenSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "reason": "no create", "count": "1"}},
				{Message: namespaceSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "cause": "gone", "count": "1"}},
				{Message: webhookSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "cause": "timeout", "count": "1"}},
			},
		},
		"DisabledIsNotWarnedAbout": {
			reason: "--dry-run-on=existing is the user's own choice, so the additions it leaves unverified are not news to them.",
			diffSets: []map[string]*dt.ResourceDiff{diffMap(
				unverifiedAddition("TestResource", "a", "ns-a", dt.DryRunSkipDisabled, ""),
			)},
			want: nil,
		},
		"VerifiedDiffsRaiseNothing": {
			reason: "A diff without DryRunInfo was verified, and there is nothing to say about it.",
			diffSets: []map[string]*dt.ResourceDiff{diffMap(&dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "TestResource"},
				Namespace:    "ns-a",
				ResourceName: "a",
				DiffType:     dt.DiffTypeAdded,
			})},
			want: nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if d := cmp.Diff(tc.want, dryRunWarnings(tc.diffSets...)); d != "" {
				t.Errorf("\n%s\ndryRunWarnings() mismatch (-want +got):\n%s", tc.reason, d)
			}
		})
	}
}

// TestRenderers_EmitDryRunWarnings covers every entry point a command renders through. Each derives the
// summary from what it is handed, writes it to stderr in every output mode, and adds it to warnings[] in
// structured modes after the warnings raised during the run. The summary spans inputs, so two XRs'
// additions are counted together; a failed XR shows no diffs and contributes nothing.
func TestRenderers_EmitDryRunWarnings(t *testing.T) {
	raised := dt.OutputWarning{Message: "raised during the run"}
	summary := dt.OutputWarning{
		Message: namespaceSummary,
		Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "cause": "gone", "count": "2"},
	}

	first := unverifiedAddition("TestResource", "a", "ns-a", dt.DryRunSkipNamespaceNotFound, "gone")
	second := unverifiedAddition("TestResource", "b", "ns-a", dt.DryRunSkipNamespaceNotFound, "gone")

	xrGroups := []dt.XRDiffGroup{
		{XR: corev1.ObjectReference{Kind: "XR", Name: "one"}, Diffs: diffMap(first)},
		{XR: corev1.ObjectReference{Kind: "XR", Name: "two"}, Diffs: diffMap(second)},
		{XR: corev1.ObjectReference{Kind: "XR", Name: "broken"}, Err: &dt.OutputError{ResourceID: "XR/broken", Message: "boom"}},
	}

	compOutput := func() *CompDiffOutput {
		return &CompDiffOutput{
			Compositions: []CompositionDiff{{
				Name: "xrs.example.org",
				ImpactAnalysis: []XRImpact{
					{ObjectReference: corev1.ObjectReference{Kind: "XR", Name: "one"}, Status: XRStatusChanged, Diffs: diffMap(first)},
					{ObjectReference: corev1.ObjectReference{Kind: "XR", Name: "two"}, Status: XRStatusChanged, Diffs: diffMap(second)},
					{ObjectReference: corev1.ObjectReference{Kind: "XR", Name: "broken"}, Status: XRStatusError},
				},
			}},
			Warnings: []dt.OutputWarning{raised},
		}
	}

	type renderFn func(opts DiffOptions) error

	tests := map[string]struct {
		format OutputFormat
		render renderFn
		// structured is false for the human renderers, which have no warnings[] to check.
		structured bool
	}{
		"XRText": {
			format: OutputFormatDiff,
			render: func(opts DiffOptions) error {
				return NewDiffRenderer(tu.TestLogger(t, false), opts).RenderDiffs(xrGroups, nil, []dt.OutputWarning{raised})
			},
		},
		"XRJSON": {
			format:     OutputFormatJSON,
			structured: true,
			render: func(opts DiffOptions) error {
				return NewStructuredDiffRenderer(tu.TestLogger(t, false), opts).RenderDiffs(xrGroups, nil, []dt.OutputWarning{raised})
			},
		},
		"CompText": {
			format: OutputFormatDiff,
			render: func(opts DiffOptions) error {
				logger := tu.TestLogger(t, false)
				return NewDefaultCompDiffRenderer(logger, NewDiffRenderer(logger, opts), opts).RenderCompDiff(compOutput())
			},
		},
		"CompJSON": {
			format:     OutputFormatJSON,
			structured: true,
			render: func(opts DiffOptions) error {
				return NewStructuredCompDiffRenderer(tu.TestLogger(t, false), opts).RenderCompDiff(compOutput())
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			opts := DefaultDiffOptions()
			opts.UseColors = false
			opts.Format = tc.format
			opts.Stdout = &stdout
			opts.Stderr = &stderr

			if err := tc.render(opts); err != nil {
				t.Fatalf("render failed: %v", err)
			}

			// Exactly once on stderr: the comp text renderer reuses the XR text renderer per
			// composition, and a summary emitted at each level would repeat.
			if got := strings.Count(stderr.String(), summary.FormatWarning()); got != 1 {
				t.Errorf("stderr carries the summary %d times, want 1:\n%s", got, stderr.String())
			}

			// Warnings raised during the run reached stderr when they were raised; a renderer that
			// re-emitted them would print them twice.
			if strings.Contains(stderr.String(), raised.FormatWarning()) {
				t.Errorf("stderr repeats a warning raised during the run:\n%s", stderr.String())
			}

			if !tc.structured {
				return
			}

			var out struct {
				Warnings []dt.OutputWarning `json:"warnings"`
			}

			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatalf("cannot parse structured output: %v\n%s", err, stdout.String())
			}

			if d := cmp.Diff([]dt.OutputWarning{raised, summary}, out.Warnings); d != "" {
				t.Errorf("warnings[] mismatch (-want +got):\n%s", d)
			}
		})
	}
}
