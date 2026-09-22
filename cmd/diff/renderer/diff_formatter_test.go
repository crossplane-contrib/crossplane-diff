package renderer

import (
	"strings"
	"testing"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	"github.com/sergi/go-diff/diffmatchpatch"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestGenerateDiffWithOptions_SeededRevisionRef pins both directions of the comp-scoped suppression
// added for issue #474. comp seeds each re-pointing composite's compositionRevisionRef with the revision
// the diffed composition would produce, so that value is tool-authored bookkeeping and is hidden; xr
// never seeds, so a user's own hand-edited revision pin must still be shown. Getting this backwards
// either spams N composites with one field each (and exits 3 on every revision-creating change), or
// silently swallows a real user edit.
func TestGenerateDiffWithOptions_SeededRevisionRef(t *testing.T) {
	// Differ only in the revision ref, as a seeded composite does against its cluster copy.
	refOnly := func(name string) *un.Unstructured {
		return tu.NewResource("example.org/v1", "XResource", "test-xr").
			WithSpecField("coolField", "same").
			WithNestedField(map[string]any{"name": name}, "spec", "crossplane", "compositionRevisionRef").
			Build()
	}

	current := refOnly("xr.example.org-aaaaaaa")
	desired := refOnly("xr.example.org-bbbbbbb")

	// Same ref change, but with a real spec change alongside it — the case that matters most, where a
	// composed value derived from the revision name is what the suppression must not hide.
	desiredWithRealChange := refOnly("xr.example.org-bbbbbbb")
	_ = un.SetNestedField(desiredWithRealChange.Object, "changed", "spec", "coolField")

	tests := map[string]struct {
		desired  *un.Unstructured
		options  DiffOptions
		wantType types.DiffType
		// wantRefInClean is whether the ref survives into the rendered (Clean) view. Only meaningful
		// when wantType is not Equal, since equal diffs leave Clean nil by design.
		wantRefInClean bool
	}{
		"NotSeeded_RefChangeIsShown": {
			// The xr path. The user pinned this composite to a different revision themselves.
			desired:        desired,
			options:        DiffOptions{},
			wantType:       types.DiffTypeModified,
			wantRefInClean: true,
		},
		"Seeded_RefOnlyChangeIsSuppressed": {
			// The comp path. revisionImpact reports the revision once per composition, so repeating it
			// once per composite adds nothing — and would flip the composite to "changed".
			desired:  desired,
			options:  DiffOptions{SeededRevisionRef: true},
			wantType: types.DiffTypeEqual,
		},
		"Seeded_DerivedChangeStillShows": {
			// Not suppressed: a real difference alongside the seeded ref. This is the signal the seeding
			// exists to surface, so the diff must survive even though the ref itself is hidden.
			desired:        desiredWithRealChange,
			options:        DiffOptions{SeededRevisionRef: true},
			wantType:       types.DiffTypeModified,
			wantRefInClean: false,
		},
		"Seeded_ForVerdictKeepsTheRef": {
			// A verdict comparison never loses information to a display preference, so the ref is kept
			// even though it would be stripped for rendering.
			desired:        desired,
			options:        DiffOptions{SeededRevisionRef: true, ForVerdict: true},
			wantType:       types.DiffTypeModified,
			wantRefInClean: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := GenerateDiffWithOptions(t.Context(), current, tt.desired, tu.TestLogger(t, false), tt.options)
			if err != nil {
				t.Fatalf("GenerateDiffWithOptions() unexpected error: %v", err)
			}

			if got.DiffType != tt.wantType {
				t.Fatalf("GenerateDiffWithOptions() DiffType = %q, want %q", got.DiffType, tt.wantType)
			}

			if tt.wantType == types.DiffTypeEqual {
				return
			}

			_, found, _ := un.NestedString(got.Desired.Clean.Object, "spec", "crossplane", "compositionRevisionRef", "name")
			if found != tt.wantRefInClean {
				t.Errorf("compositionRevisionRef present in Clean = %t, want %t; clean spec: %v",
					found, tt.wantRefInClean, got.Desired.Clean.Object["spec"])
			}
		})
	}
}

func TestGenerateDiffWithOptions(t *testing.T) {
	// Create test resources for diffing
	current := tu.NewResource("example.org/v1", "TestResource", "test-resource").
		WithSpecField("field1", "old-value").
		WithSpecField("field2", int64(123)).
		Build()

	desired := tu.NewResource("example.org/v1", "TestResource", "test-resource").
		WithSpecField("field1", "new-value").
		WithSpecField("field2", int64(456)).
		WithSpecField("field4", "added-field").
		Build()

	// Identical to current, for no-change test
	noChanges := current.DeepCopy()

	// Resources carrying content that cleanupForDiff strips: an ignored
	// annotation (via options.IgnorePaths below) and a server-side field
	// (resourceVersion). Used to assert the Raw/Clean split.
	const ignoredAnnotation = "argocd.argoproj.io/tracking-id"

	noisyCurrent := tu.NewResource("example.org/v1", "TestResource", "test-resource").
		WithSpecField("field1", "old-value").
		WithAnnotations(map[string]string{ignoredAnnotation: "id-old"}).
		Build()
	noisyCurrent.SetResourceVersion("100")

	noisyDesired := tu.NewResource("example.org/v1", "TestResource", "test-resource").
		WithSpecField("field1", "new-value").
		WithAnnotations(map[string]string{ignoredAnnotation: "id-new"}).
		Build()
	noisyDesired.SetResourceVersion("200")

	// The expected Clean views for the noisy pair: the ignored annotation (and
	// the now-empty annotations map) and the server-side resourceVersion are
	// stripped, while the non-ignored spec change survives.
	noisyCurrentClean := tu.NewResource("example.org/v1", "TestResource", "test-resource").
		WithSpecField("field1", "old-value").
		Build()

	noisyDesiredClean := tu.NewResource("example.org/v1", "TestResource", "test-resource").
		WithSpecField("field1", "new-value").
		Build()

	ignoreOpts := DefaultDiffOptions()
	ignoreOpts.IgnorePaths = []string{"metadata.annotations[" + ignoredAnnotation + "]"}

	// A modified pair where the current (cluster) object is namespaced but the
	// desired (rendered) object omits metadata.namespace — as manifests often
	// do. Generation must prefer the current namespace so downstream (structured
	// output, human diff) doesn't emit an empty namespace. Neither object has
	// ignorable content, so Clean == Raw content.
	nsCurrent := tu.NewResource("example.org/v1", "TestResource", "test-resource").
		InNamespace("production").
		WithSpecField("field1", "old-value").
		Build()

	nsDesired := tu.NewResource("example.org/v1", "TestResource", "test-resource").
		WithSpecField("field1", "new-value").
		Build()

	tests := map[string]struct {
		current  *un.Unstructured
		desired  *un.Unstructured
		kind     string
		resName  string
		options  DiffOptions
		wantDiff *types.ResourceDiff
		wantErr  bool
	}{
		"ModifiedResource": {
			current: current,
			desired: desired,
			kind:    "TestResource",
			resName: "test-resource",
			options: DefaultDiffOptions(),
			wantDiff: &types.ResourceDiff{
				Gvk:          current.GroupVersionKind(),
				ResourceName: "test-resource",
				DiffType:     types.DiffTypeModified,
				// Nothing to strip, so Clean is content-equal to Raw.
				Current: types.ResourceViews{Raw: current, Clean: current},
				Desired: types.ResourceViews{Raw: desired, Clean: desired},
				// LineDiffs will be checked separately
			},
		},
		"NoChanges": {
			current: current,
			desired: noChanges,
			kind:    "TestResource",
			resName: "test-resource",
			options: DefaultDiffOptions(),
			wantDiff: &types.ResourceDiff{
				Gvk:          current.GroupVersionKind(),
				ResourceName: "test-resource",
				DiffType:     types.DiffTypeEqual,
				// Equal diffs render nothing, so Clean is left nil by generation.
				Current: types.ResourceViews{Raw: current},
				Desired: types.ResourceViews{Raw: noChanges},
			},
		},
		"NewResource": {
			current: nil,
			desired: desired,
			kind:    "TestResource",
			resName: "test-resource",
			options: DefaultDiffOptions(),
			wantDiff: &types.ResourceDiff{
				Gvk:          desired.GroupVersionKind(),
				ResourceName: "test-resource",
				DiffType:     types.DiffTypeAdded,
				Current:      types.ResourceViews{},
				Desired:      types.ResourceViews{Raw: desired, Clean: desired},
				// LineDiffs will be checked separately
			},
		},
		"RemovedResource": {
			current: current,
			desired: nil,
			kind:    "TestResource",
			resName: "test-resource",
			options: DefaultDiffOptions(),
			wantDiff: &types.ResourceDiff{
				Gvk:          current.GroupVersionKind(),
				ResourceName: "test-resource",
				DiffType:     types.DiffTypeRemoved,
				Current:      types.ResourceViews{Raw: current, Clean: current},
				Desired:      types.ResourceViews{},
				// LineDiffs will be checked separately
			},
		},
		"ModifiedResource_RawAndCleanViews": {
			// Locks the Raw/Clean split: Raw retains the ignored annotation and
			// server-side resourceVersion, while Clean has both stripped and
			// keeps only the non-ignored spec change.
			current: noisyCurrent,
			desired: noisyDesired,
			kind:    "TestResource",
			resName: "test-resource",
			options: ignoreOpts,
			wantDiff: &types.ResourceDiff{
				Gvk:          noisyCurrent.GroupVersionKind(),
				ResourceName: "test-resource",
				DiffType:     types.DiffTypeModified,
				Current:      types.ResourceViews{Raw: noisyCurrent, Clean: noisyCurrentClean},
				Desired:      types.ResourceViews{Raw: noisyDesired, Clean: noisyDesiredClean},
			},
		},
		"ModifiedResource_PrefersCurrentNamespace": {
			// Regression: desired omits metadata.namespace but current has one.
			// diff.Namespace must be the current namespace, not empty.
			current: nsCurrent,
			desired: nsDesired,
			kind:    "TestResource",
			resName: "test-resource",
			options: DefaultDiffOptions(),
			wantDiff: &types.ResourceDiff{
				Gvk:          nsCurrent.GroupVersionKind(),
				ResourceName: "test-resource",
				Namespace:    "production",
				DiffType:     types.DiffTypeModified,
				Current:      types.ResourceViews{Raw: nsCurrent, Clean: nsCurrent},
				Desired:      types.ResourceViews{Raw: nsDesired, Clean: nsDesired},
			},
		},
		"BothNil": {
			current: nil,
			desired: nil,
			kind:    "TestResource",
			resName: "test-resource",
			options: DefaultDiffOptions(),
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			diff, err := GenerateDiffWithOptions(t.Context(), tt.current, tt.desired, tu.TestLogger(t, false), tt.options)

			if tt.wantErr {
				if err == nil {
					t.Errorf("GenerateDiffWithOptions() expected error but got none")
				}

				return
			}

			if err != nil {
				t.Fatalf("GenerateDiffWithOptions() returned error: %v", err)
			}

			if diff == nil {
				t.Fatalf("GenerateDiffWithOptions() returned nil, want non-nil")
			}

			// Check the basic properties
			if diffStr := cmp.Diff(tt.wantDiff.Gvk, diff.Gvk); diffStr != "" {
				t.Errorf("Gvk mismatch (-want +got):\n%s", diffStr)
			}

			if diffStr := cmp.Diff(tt.wantDiff.ResourceName, diff.ResourceName); diffStr != "" {
				t.Errorf("ResourceName mismatch (-want +got):\n%s", diffStr)
			}

			if diffStr := cmp.Diff(tt.wantDiff.DiffType, diff.DiffType); diffStr != "" {
				t.Errorf("DiffType mismatch (-want +got):\n%s", diffStr)
			}

			// Check for line diffs - should be non-empty for changed resources
			if diff.DiffType != types.DiffTypeEqual && len(diff.LineDiffs) == 0 {
				t.Errorf("LineDiffs is empty for %s", name)
			}

			if diffStr := cmp.Diff(tt.wantDiff.Namespace, diff.Namespace); diffStr != "" {
				t.Errorf("Namespace mismatch (-want +got):\n%s", diffStr)
			}

			// Check the Current and Desired views in full: Raw is the original
			// object, Clean is the post-cleanup object (nil for equal diffs, and
			// content-equal to Raw when there is nothing to strip).
			if diffStr := cmp.Diff(tt.wantDiff.Current, diff.Current); diffStr != "" {
				t.Errorf("Current views mismatch (-want +got):\n%s", diffStr)
			}

			if diffStr := cmp.Diff(tt.wantDiff.Desired, diff.Desired); diffStr != "" {
				t.Errorf("Desired views mismatch (-want +got):\n%s", diffStr)
			}
		})
	}
}

func TestFormatDiff(t *testing.T) {
	// Create test diffs
	simpleDiffs := []diffmatchpatch.Diff{
		{Type: diffmatchpatch.DiffEqual, Text: "unchanged line\n"},
		{Type: diffmatchpatch.DiffDelete, Text: "deleted line\n"},
		{Type: diffmatchpatch.DiffInsert, Text: "inserted line\n"},
		{Type: diffmatchpatch.DiffEqual, Text: "another unchanged line\n"},
	}

	tests := map[string]struct {
		diffs    []diffmatchpatch.Diff
		options  DiffOptions
		contains []string
		excludes []string
	}{
		"EmptyDiffs": {
			diffs:    []diffmatchpatch.Diff{},
			options:  DefaultDiffOptions(),
			contains: []string{},
			excludes: []string{"unchanged", "deleted", "inserted"},
		},
		"StandardFormatting": {
			diffs:   simpleDiffs,
			options: DefaultDiffOptions(),
			contains: []string{
				"unchanged line",
				"deleted line",
				"inserted line",
				"another unchanged line",
			},
		},
		"WithoutColors": {
			diffs: simpleDiffs,
			options: func() DiffOptions {
				opts := DefaultDiffOptions()
				opts.UseColors = false

				return opts
			}(),
			contains: []string{
				"  unchanged line",
				"- deleted line",
				"+ inserted line",
				"  another unchanged line",
			},
			excludes: []string{
				"\x1b[31m", // Red color code
				"\x1b[32m", // Green color code
			},
		},
		"WithColors": {
			diffs: simpleDiffs,
			options: func() DiffOptions {
				opts := DefaultDiffOptions()
				opts.UseColors = true

				return opts
			}(),
			contains: []string{
				"unchanged line",
				"deleted line",
				"inserted line",
			},
		},
		"CompactFormat": {
			diffs: []diffmatchpatch.Diff{
				{Type: diffmatchpatch.DiffEqual, Text: "context line 1\ncontext line 2\ncontext line 3\n"},
				{Type: diffmatchpatch.DiffDelete, Text: "deleted line 1\ndeleted line 2\n"},
				{Type: diffmatchpatch.DiffInsert, Text: "inserted line 1\ninserted line 2\n"},
				{Type: diffmatchpatch.DiffEqual, Text: "context line 4\ncontext line 5\ncontext line 6\n"},
			},
			options: func() DiffOptions {
				opts := DefaultDiffOptions()
				opts.Compact = true
				opts.ContextLines = 1

				return opts
			}(),
			contains: []string{
				"context line 3",
				"deleted line 1",
				"deleted line 2",
				"inserted line 1",
				"inserted line 2",
				"context line 4",
			},
			excludes: []string{
				"context line 1",
				"context line 2",
				"context line 5",
				"context line 6",
			},
		},
		"CustomPrefixes": {
			diffs: simpleDiffs,
			options: func() DiffOptions {
				opts := DefaultDiffOptions()
				opts.UseColors = false
				opts.AddPrefix = "ADD "
				opts.DeletePrefix = "DEL "
				opts.ContextPrefix = "CTX "

				return opts
			}(),
			contains: []string{
				"CTX unchanged line",
				"DEL deleted line",
				"ADD inserted line",
				"CTX another unchanged line",
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Format the diff
			result := FormatDiff(tt.diffs, tt.options)

			// Check that the result contains expected substrings
			for _, expected := range tt.contains {
				if expected == "" {
					continue
				}

				if !strings.Contains(result, expected) {
					t.Errorf("FormatDiff() result missing expected content: %q", expected)
				}
			}

			// Check that the result excludes certain substrings
			for _, excluded := range tt.excludes {
				if excluded == "" {
					continue
				}

				if strings.Contains(result, excluded) {
					t.Errorf("FormatDiff() result contains unexpected content: %q", excluded)
				}
			}
		})
	}
}

func TestRemoveNestedPath(t *testing.T) {
	tests := map[string]struct {
		obj     map[string]any
		path    string
		want    bool
		wantObj map[string]any
		descr   string
	}{
		"SimplePath": {
			obj: map[string]any{
				"metadata": map[string]any{
					"name":      "test",
					"namespace": "default",
				},
			},
			path: "metadata.namespace",
			want: true,
			wantObj: map[string]any{
				"metadata": map[string]any{
					"name": "test",
				},
			},
			descr: "removes a simple nested field",
		},
		"MapKeyPath": {
			obj: map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						"kubectl.kubernetes.io/last-applied-configuration": "large-json",
						"argocd.argoproj.io/tracking-id":                   "some-id",
						"keep-this":                                        "value",
					},
				},
			},
			path: "metadata.annotations[kubectl.kubernetes.io/last-applied-configuration]",
			want: true,
			wantObj: map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						"argocd.argoproj.io/tracking-id": "some-id",
						"keep-this":                      "value",
					},
				},
			},
			descr: "removes a specific key from a map",
		},
		"MapKeyPathWithSlash": {
			obj: map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{
						"argocd.argoproj.io/instance": "some-instance",
						"provider":                    "aws",
					},
				},
			},
			path: "metadata.labels[argocd.argoproj.io/instance]",
			want: true,
			wantObj: map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{
						"provider": "aws",
					},
				},
			},
			descr: "removes a label with slash in key",
		},
		"NonExistentPath": {
			obj: map[string]any{
				"metadata": map[string]any{
					"name": "test",
				},
			},
			path: "metadata.nonexistent",
			want: false,
			wantObj: map[string]any{
				"metadata": map[string]any{
					"name": "test",
				},
			},
			descr: "returns false for non-existent path",
		},
		"NonExistentMapKey": {
			obj: map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						"keep-this": "value",
					},
				},
			},
			path: "metadata.annotations[nonexistent-key]",
			want: false,
			wantObj: map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						"keep-this": "value",
					},
				},
			},
			descr: "returns false for non-existent map key",
		},
		"EmptyPath": {
			obj: map[string]any{
				"metadata": map[string]any{
					"name": "test",
				},
			},
			path: "",
			want: false,
			wantObj: map[string]any{
				"metadata": map[string]any{
					"name": "test",
				},
			},
			descr: "returns false for empty path",
		},
		"RemoveEntireSection": {
			obj: map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						"key": "value",
					},
				},
				"spec": map[string]any{
					"field": "value",
				},
			},
			path: "metadata.annotations",
			want: true,
			wantObj: map[string]any{
				"metadata": map[string]any{},
				"spec": map[string]any{
					"field": "value",
				},
			},
			descr: "removes entire nested map",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := removeNestedPath(tc.obj, tc.path)

			if got != tc.want {
				t.Errorf("removeNestedPath() = %v, want %v for %s", got, tc.want, tc.descr)
			}

			if diff := cmp.Diff(tc.wantObj, tc.obj); diff != "" {
				t.Errorf("removeNestedPath() object mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
