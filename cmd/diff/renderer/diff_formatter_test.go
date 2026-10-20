package renderer

import (
	"strings"
	"testing"

	"github.com/sergi/go-diff/diffmatchpatch"
)

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
