package compare

import (
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/result"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	"github.com/sergi/go-diff/diffmatchpatch"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGenerateDiff(t *testing.T) {
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

	ignoreOpts := Options{}
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
		options  Options
		wantDiff *dt.ResourceDiff
		wantErr  bool
	}{
		"ModifiedResource": {
			current: current,
			desired: desired,
			kind:    "TestResource",
			resName: "test-resource",
			options: Options{},
			wantDiff: &dt.ResourceDiff{
				Gvk:          current.GroupVersionKind(),
				ResourceName: "test-resource",
				DiffType:     dt.DiffTypeModified,
				// Nothing to strip, so Clean is content-equal to Raw.
				Current: dt.ResourceViews{Raw: current, Clean: current},
				Desired: dt.ResourceViews{Raw: desired, Clean: desired},
				// LineDiffs will be checked separately
			},
		},
		"NoChanges": {
			current: current,
			desired: noChanges,
			kind:    "TestResource",
			resName: "test-resource",
			options: Options{},
			wantDiff: &dt.ResourceDiff{
				Gvk:          current.GroupVersionKind(),
				ResourceName: "test-resource",
				DiffType:     dt.DiffTypeEqual,
				// Equal diffs render nothing, so Clean is left nil by generation.
				Current: dt.ResourceViews{Raw: current},
				Desired: dt.ResourceViews{Raw: noChanges},
			},
		},
		"NewResource": {
			current: nil,
			desired: desired,
			kind:    "TestResource",
			resName: "test-resource",
			options: Options{},
			wantDiff: &dt.ResourceDiff{
				Gvk:          desired.GroupVersionKind(),
				ResourceName: "test-resource",
				DiffType:     dt.DiffTypeAdded,
				Current:      dt.ResourceViews{},
				Desired:      dt.ResourceViews{Raw: desired, Clean: desired},
				// LineDiffs will be checked separately
			},
		},
		"RemovedResource": {
			current: current,
			desired: nil,
			kind:    "TestResource",
			resName: "test-resource",
			options: Options{},
			wantDiff: &dt.ResourceDiff{
				Gvk:          current.GroupVersionKind(),
				ResourceName: "test-resource",
				DiffType:     dt.DiffTypeRemoved,
				Current:      dt.ResourceViews{Raw: current, Clean: current},
				Desired:      dt.ResourceViews{},
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
			wantDiff: &dt.ResourceDiff{
				Gvk:          noisyCurrent.GroupVersionKind(),
				ResourceName: "test-resource",
				DiffType:     dt.DiffTypeModified,
				Current:      dt.ResourceViews{Raw: noisyCurrent, Clean: noisyCurrentClean},
				Desired:      dt.ResourceViews{Raw: noisyDesired, Clean: noisyDesiredClean},
			},
		},
		"ModifiedResource_PrefersCurrentNamespace": {
			// Regression: desired omits metadata.namespace but current has one.
			// diff.Namespace must be the current namespace, not empty.
			current: nsCurrent,
			desired: nsDesired,
			kind:    "TestResource",
			resName: "test-resource",
			options: Options{},
			wantDiff: &dt.ResourceDiff{
				Gvk:          nsCurrent.GroupVersionKind(),
				ResourceName: "test-resource",
				Namespace:    "production",
				DiffType:     dt.DiffTypeModified,
				Current:      dt.ResourceViews{Raw: nsCurrent, Clean: nsCurrent},
				Desired:      dt.ResourceViews{Raw: nsDesired, Clean: nsDesired},
			},
		},
		"BothNil": {
			current: nil,
			desired: nil,
			kind:    "TestResource",
			resName: "test-resource",
			options: Options{},
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			diff, err := GenerateDiff(t.Context(), tt.current, tt.desired, tu.TestLogger(t, false), tt.options)

			if tt.wantErr {
				if err == nil {
					t.Errorf("GenerateDiff() expected error but got none")
				}

				return
			}

			if err != nil {
				t.Fatalf("GenerateDiff() returned error: %v", err)
			}

			if diff == nil {
				t.Fatalf("GenerateDiff() returned nil, want non-nil")
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
			if diff.DiffType != dt.DiffTypeEqual && len(diff.LineDiffs) == 0 {
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

func TestGetLineDiff(t *testing.T) {
	tests := map[string]struct {
		oldText  string
		newText  string
		expected []diffmatchpatch.Operation
	}{
		"NoChanges": {
			oldText: "line1\nline2\nline3\n",
			newText: "line1\nline2\nline3\n",
			expected: []diffmatchpatch.Operation{
				diffmatchpatch.DiffEqual,
			},
		},
		"LineAdded": {
			oldText: "line1\nline2\n",
			newText: "line1\nline2\nline3\n",
			expected: []diffmatchpatch.Operation{
				diffmatchpatch.DiffEqual,
				diffmatchpatch.DiffInsert,
			},
		},
		"LineRemoved": {
			oldText: "line1\nline2\nline3\n",
			newText: "line1\nline3\n",
			expected: []diffmatchpatch.Operation{
				diffmatchpatch.DiffEqual,
				diffmatchpatch.DiffDelete,
				diffmatchpatch.DiffEqual,
			},
		},
		"LineModified": {
			oldText: "line1\nline2\nline3\n",
			newText: "line1\nmodified2\nline3\n",
			expected: []diffmatchpatch.Operation{
				diffmatchpatch.DiffEqual,
				diffmatchpatch.DiffDelete,
				diffmatchpatch.DiffInsert,
				diffmatchpatch.DiffEqual,
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			result := GetLineDiff(tt.oldText, tt.newText)

			// Check that we have the expected diff types
			if len(result) != len(tt.expected) {
				t.Errorf("GetLineDiff() returned %d diffs, want %d", len(result), len(tt.expected))

				for i, diff := range result {
					t.Logf("Diff %d: Type=%s, Text=%q", i, diff.Type, diff.Text)
				}

				return
			}

			// Verify the types match in sequence
			for i, expectedType := range tt.expected {
				if result[i].Type != expectedType {
					t.Errorf("GetLineDiff() diff[%d] has type %s, want %s", i, result[i].Type, expectedType)
				}
			}
		})
	}
}
