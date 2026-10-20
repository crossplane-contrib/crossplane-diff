package renderer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/result"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	namespaceSummary = "skipped apiserver verification of added resources: their namespace does not exist yet, so their diffs omit server-side defaulting and admission"
	testResourceGVK  = "example.org/v1, Kind=TestResource"
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

	compOutput := func() *dt.CompDiffOutput {
		return &dt.CompDiffOutput{
			Compositions: []dt.CompositionDiff{{
				Name: "xrs.example.org",
				ImpactAnalysis: []dt.XRImpact{
					{ObjectReference: corev1.ObjectReference{Kind: "XR", Name: "one"}, Status: dt.XRStatusChanged, Diffs: diffMap(first)},
					{ObjectReference: corev1.ObjectReference{Kind: "XR", Name: "two"}, Status: dt.XRStatusChanged, Diffs: diffMap(second)},
					{ObjectReference: corev1.ObjectReference{Kind: "XR", Name: "broken"}, Status: dt.XRStatusError},
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
