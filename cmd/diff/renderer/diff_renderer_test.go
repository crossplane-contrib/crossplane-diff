package renderer

import (
	"bytes"
	"strings"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/sergi/go-diff/diffmatchpatch"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestDefaultDiffRenderer_RenderDiffs(t *testing.T) {
	// Create test diffs
	addedDiff := &dt.ResourceDiff{
		Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "TestResource"},
		ResourceName: "added-resource",
		DiffType:     dt.DiffTypeAdded,
		LineDiffs: []diffmatchpatch.Diff{
			{Type: diffmatchpatch.DiffInsert, Text: "apiVersion: example.org/v1\nkind: TestResource\nmetadata:\n  name: added-resource\nspec:\n  field: value"},
		},
	}

	modifiedDiff := &dt.ResourceDiff{
		Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "TestResource"},
		ResourceName: "modified-resource",
		DiffType:     dt.DiffTypeModified,
		LineDiffs: []diffmatchpatch.Diff{
			{Type: diffmatchpatch.DiffEqual, Text: "apiVersion: example.org/v1\nkind: TestResource\nmetadata:\n  name: modified-resource\n"},
			{Type: diffmatchpatch.DiffDelete, Text: "spec:\n  field: old-value"},
			{Type: diffmatchpatch.DiffInsert, Text: "spec:\n  field: new-value"},
		},
	}

	removedDiff := &dt.ResourceDiff{
		Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "TestResource"},
		ResourceName: "removed-resource",
		DiffType:     dt.DiffTypeRemoved,
		LineDiffs: []diffmatchpatch.Diff{
			{Type: diffmatchpatch.DiffDelete, Text: "apiVersion: example.org/v1\nkind: TestResource\nmetadata:\n  name: removed-resource\nspec:\n  field: value"},
		},
	}

	equalDiff := &dt.ResourceDiff{
		Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "TestResource"},
		ResourceName: "equal-resource",
		DiffType:     dt.DiffTypeEqual,
		LineDiffs:    []diffmatchpatch.Diff{},
	}

	changedBucket := modifiedDiffFor("Bucket", "my-xr-bucket")

	// noColor is the base options for the grouping rows (which assert on
	// headers/summaries/footer, not on colored diff bodies). The formatting
	// rows below set their own options to exercise prefixes/context/compact.
	noColor := DefaultDiffOptions()
	noColor.UseColors = false

	tests := map[string]struct {
		groups          []dt.XRDiffGroup
		options         DiffOptions
		expectedOutputs []string
		notExpected     []string
	}{
		"RenderAllDiffTypes": {
			groups: identitylessGroups(map[string]*dt.ResourceDiff{
				addedDiff.GetDiffKey():    addedDiff,
				modifiedDiff.GetDiffKey(): modifiedDiff,
				removedDiff.GetDiffKey():  removedDiff,
				equalDiff.GetDiffKey():    equalDiff,
			}),
			options: DiffOptions{
				UseColors:      false,
				AddPrefix:      "+ ",
				DeletePrefix:   "- ",
				ContextPrefix:  "  ",
				ContextLines:   3,
				ChunkSeparator: "...",
				Compact:        false,
			},
			expectedOutputs: []string{
				"+++ TestResource/added-resource",
				"--- TestResource/removed-resource",
				"~~~ TestResource/modified-resource",
				"+ apiVersion: example.org/v1",
				"- spec:",
				"-   field: old-value",
				"+ spec:",
				"+   field: new-value",
			},
			notExpected: []string{
				"TestResource/equal-resource", // Equal resources should not be rendered
			},
		},
		"CompactMode": {
			groups: identitylessGroups(map[string]*dt.ResourceDiff{
				modifiedDiff.GetDiffKey(): modifiedDiff,
			}),
			options: DiffOptions{
				UseColors:      false,
				AddPrefix:      "+ ",
				DeletePrefix:   "- ",
				ContextPrefix:  "  ",
				ContextLines:   1, // Fewer context lines for compact mode
				ChunkSeparator: "...",
				Compact:        true,
			},
			expectedOutputs: []string{
				"~~~ TestResource/modified-resource",
				"- spec:",
				"-   field: old-value",
				"+ spec:",
				"+   field: new-value",
			},
			notExpected: []string{
				"  apiVersion: example.org/v1", // Should be omitted due to context line limit
				"  metadata:",
			},
		},
		"EmptyDiffs": {
			groups: identitylessGroups(map[string]*dt.ResourceDiff{}),
			options: DiffOptions{
				UseColors:      false,
				AddPrefix:      "+ ",
				DeletePrefix:   "- ",
				ContextPrefix:  "  ",
				ContextLines:   3,
				ChunkSeparator: "...",
				Compact:        false,
			},
			expectedOutputs: []string{},
		},
		"OnlyEqualDiffs": {
			groups: identitylessGroups(map[string]*dt.ResourceDiff{
				equalDiff.GetDiffKey(): equalDiff,
			}),
			options: DiffOptions{
				UseColors:      false,
				AddPrefix:      "+ ",
				DeletePrefix:   "- ",
				ContextPrefix:  "  ",
				ContextLines:   3,
				ChunkSeparator: "...",
				Compact:        false,
			},
			expectedOutputs: []string{},
			notExpected:     []string{"TestResource/equal-resource"},
		},
		"SummaryOutput": {
			groups: identitylessGroups(map[string]*dt.ResourceDiff{
				addedDiff.GetDiffKey():    addedDiff,
				modifiedDiff.GetDiffKey(): modifiedDiff,
				removedDiff.GetDiffKey():  removedDiff,
			}),
			options: DiffOptions{
				UseColors:      false,
				AddPrefix:      "+ ",
				DeletePrefix:   "- ",
				ContextPrefix:  "  ",
				ContextLines:   3,
				ChunkSeparator: "...",
				Compact:        false,
			},
			expectedOutputs: []string{
				"Summary:", "1 added", "1 modified", "1 removed",
			},
		},

		// --- Grouping by input XR (identity-bearing groups) ---

		// A single identity-bearing group renders flat, exactly as before
		// grouping existed: no section header and no aggregate footer, since
		// there is nothing to disambiguate.
		"SingleChangedXRRendersFlat": {
			groups: []dt.XRDiffGroup{
				xrGroup("XNopResource", "my-xr", map[string]*dt.ResourceDiff{
					changedBucket.GetDiffKey(): changedBucket,
				}),
			},
			options: noColor,
			expectedOutputs: []string{
				"~~~ Bucket/my-xr-bucket",
				"Summary: 1 modified",
			},
			notExpected: []string{"===", "Total:"},
		},
		// Multiple identity-bearing groups: each gets a header in input order, an
		// unchanged group says "No changes.", and an aggregate footer tallies all.
		"MultipleXRsWithUnchanged": {
			groups: []dt.XRDiffGroup{
				xrGroup("XNopResource", "first-xr", map[string]*dt.ResourceDiff{
					changedBucket.GetDiffKey(): changedBucket,
				}),
				xrGroup("XNopResource", "second-xr", map[string]*dt.ResourceDiff{}),
			},
			options: noColor,
			expectedOutputs: []string{
				"=== XNopResource/first-xr ===",
				"=== XNopResource/second-xr ===",
				"No changes.",
				"Total: 1 modified across 2 XRs (1 unchanged)",
			},
		},
		// Multiple XRs where one errored: the errored XR gets an inline Error
		// section on stdout and is tallied in the footer.
		"MultipleXRsWithError": {
			groups: []dt.XRDiffGroup{
				xrGroup("XNopResource", "ok-xr", map[string]*dt.ResourceDiff{
					changedBucket.GetDiffKey(): changedBucket,
				}),
				{
					XR:  corev1.ObjectReference{APIVersion: "example.org/v1", Kind: "XNopResource", Name: "broken-xr"},
					Err: &dt.OutputError{ResourceID: "XNopResource/broken-xr", Message: "cannot get composition"},
				},
			},
			options: noColor,
			expectedOutputs: []string{
				"=== XNopResource/ok-xr ===",
				"=== XNopResource/broken-xr ===",
				"Error: cannot get composition",
				"Total: 1 modified across 2 XRs (1 error)",
			},
		},
		// An identity-less group (the composition renderer's reuse) renders flat:
		// no section header, no aggregate footer.
		"IdentityLessRendersFlat": {
			groups: identitylessGroups(map[string]*dt.ResourceDiff{
				changedBucket.GetDiffKey(): changedBucket,
			}),
			options: noColor,
			expectedOutputs: []string{
				"~~~ Bucket/my-xr-bucket",
				"Summary: 1 modified",
			},
			notExpected: []string{"===", "Total:"},
		},
		// Changed + unchanged + errored in one batch: the footer reports all
		// three qualifiers together (the design doc's illustrative example).
		"AllThreeFooterQualifiers": {
			groups: []dt.XRDiffGroup{
				xrGroup("XNopResource", "changed-xr", map[string]*dt.ResourceDiff{
					changedBucket.GetDiffKey(): changedBucket,
				}),
				xrGroup("XNopResource", "unchanged-xr", map[string]*dt.ResourceDiff{}),
				{
					XR:  corev1.ObjectReference{APIVersion: "example.org/v1", Kind: "XNopResource", Name: "broken-xr"},
					Err: &dt.OutputError{ResourceID: "XNopResource/broken-xr", Message: "boom"},
				},
			},
			options: noColor,
			expectedOutputs: []string{
				"Total: 1 modified across 3 XRs (1 unchanged, 1 error)",
			},
		},
		// A group whose only diff is equal renders "No changes." and counts as
		// unchanged in the footer.
		"GroupedEqualOnlyIsUnchanged": {
			groups: []dt.XRDiffGroup{
				xrGroup("XNopResource", "equal-xr", map[string]*dt.ResourceDiff{
					"equal": {
						Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Bucket"},
						ResourceName: "bucket-eq",
						DiffType:     dt.DiffTypeEqual,
					},
				}),
				xrGroup("XNopResource", "changed-xr", map[string]*dt.ResourceDiff{
					changedBucket.GetDiffKey(): changedBucket,
				}),
			},
			options: noColor,
			expectedOutputs: []string{
				"=== XNopResource/equal-xr ===",
				"No changes.",
				"Total: 1 modified across 2 XRs (1 unchanged)",
			},
		},
		// A mixed batch (identity-less group alongside identity-bearing ones):
		// the identity-less group's diffs fold into the aggregate with no header.
		// Two identity-bearing groups ensure the footer fires so the fold-in is
		// observable in the total.
		"MixedIdentityLessFoldsInWithoutHeader": {
			groups: []dt.XRDiffGroup{
				xrGroup("XNopResource", "real-xr", map[string]*dt.ResourceDiff{
					changedBucket.GetDiffKey(): changedBucket,
				}),
				xrGroup("XNopResource", "real-xr-2", map[string]*dt.ResourceDiff{
					"q": modifiedDiffFor("Queue", "real-queue"),
				}),
				{Diffs: map[string]*dt.ResourceDiff{
					"extra": modifiedDiffFor("Topic", "loose-topic"),
				}},
			},
			options: noColor,
			expectedOutputs: []string{
				"=== XNopResource/real-xr ===",
				"=== XNopResource/real-xr-2 ===",
				"~~~ Topic/loose-topic",          // identity-less diff still rendered
				"Total: 3 modified across 3 XRs", // all three counted
			},
			notExpected: []string{
				"=== /", "=== (unknown)", // no header for the identity-less group
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger := tu.TestLogger(t, false)

			// Create a buffer to capture output
			var buffer bytes.Buffer

			// Set the buffer as stdout in options
			opts := tt.options
			opts.Stdout = &buffer
			opts.Stderr = &bytes.Buffer{} // discard stderr for these tests

			// Create a renderer with options pointing to buffer
			renderer := NewDiffRenderer(logger, opts)

			// Call the method under test
			err := renderer.RenderDiffs(tt.groups, nil)
			if err != nil {
				t.Fatalf("RenderDiffs() failed with error: %v", err)
			}

			// Get the output as a string
			output := buffer.String()

			// Check for expected output
			for _, expected := range tt.expectedOutputs {
				if !strings.Contains(output, expected) {
					t.Errorf("Expected output to contain %q but it didn't\nOutput: %s", expected, output)
				}
			}

			// Check for things that should not be in the output
			for _, notExpected := range tt.notExpected {
				if strings.Contains(output, notExpected) {
					t.Errorf("Output should not contain %q but it did\nOutput: %s", notExpected, output)
				}
			}
		})
	}
}

func TestDefaultDiffRenderer_RenderDiffs_WithErrors(t *testing.T) {
	tests := map[string]struct {
		errs     []dt.OutputError
		expected []string
	}{
		"ErrorsWithResourceID": {
			errs: []dt.OutputError{
				{ResourceID: "Resource/my-resource", Message: "failed to render"},
				{ResourceID: "OtherResource/other", Message: "connection refused"},
			},
			expected: []string{
				"ERROR: Resource/my-resource: failed to render",
				"ERROR: OtherResource/other: connection refused",
			},
		},
		"ErrorsWithEmptyResourceID": {
			errs: []dt.OutputError{
				{ResourceID: "", Message: "cluster connection timeout"},
				{ResourceID: "Resource/has-id", Message: "specific error"},
			},
			expected: []string{
				"ERROR: <global>: cluster connection timeout",
				"ERROR: Resource/has-id: specific error",
			},
		},
		"NoErrors": {
			errs:     []dt.OutputError{},
			expected: []string{},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger := tu.TestLogger(t, false)

			// Errors go to stderr now, so capture stderr
			var stderr bytes.Buffer

			opts := DefaultDiffOptions()
			opts.Stdout = &bytes.Buffer{} // discard stdout
			opts.Stderr = &stderr

			renderer := NewDiffRenderer(logger, opts)

			err := renderer.RenderDiffs(identitylessGroups(map[string]*dt.ResourceDiff{}), tt.errs)
			if err != nil {
				t.Fatalf("RenderDiffs() failed with error: %v", err)
			}

			output := stderr.String()

			for _, expectedMsg := range tt.expected {
				if !strings.Contains(output, expectedMsg) {
					t.Errorf("Expected stderr to contain %q but it didn't\nStderr: %s", expectedMsg, output)
				}
			}
		})
	}
}

// modifiedDiffFor builds a minimal modified diff for a composed resource of the
// given kind/name, used to populate identity-bearing groups in grouped tests.
func modifiedDiffFor(kind, name string) *dt.ResourceDiff {
	return &dt.ResourceDiff{
		Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: kind},
		ResourceName: name,
		DiffType:     dt.DiffTypeModified,
		LineDiffs: []diffmatchpatch.Diff{
			{Type: diffmatchpatch.DiffDelete, Text: "spec:\n  field: old"},
			{Type: diffmatchpatch.DiffInsert, Text: "spec:\n  field: new"},
		},
	}
}

// xrGroup builds an identity-bearing XRDiffGroup for the given input XR kind/name.
func xrGroup(kind, name string, diffs map[string]*dt.ResourceDiff) dt.XRDiffGroup {
	return dt.XRDiffGroup{
		XR:    corev1.ObjectReference{APIVersion: "example.org/v1", Kind: kind, Name: name},
		Diffs: diffs,
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
