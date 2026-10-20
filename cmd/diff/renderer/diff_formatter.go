// Package renderer contains the logic for formatting and displaying diffs.
package renderer

import (
	"fmt"
	"io"
	"os"
	"strings"

	t "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// DiffOptions holds configuration options for the diff output.
type DiffOptions struct {
	// Stdout is the writer for diff output (defaults to os.Stdout)
	Stdout io.Writer

	// Stderr is the writer for error output (defaults to os.Stderr)
	// Errors are written here following Unix conventions (errors to stderr, output to stdout)
	Stderr io.Writer

	// Format specifies the output format (diff, json, yaml)
	Format OutputFormat

	// UseColors determines whether to colorize the output
	UseColors bool

	// AddPrefix is the prefix for added lines (default "+")
	AddPrefix string

	// DeletePrefix is the prefix for deleted lines (default "-")
	DeletePrefix string

	// ContextPrefix is the prefix for unchanged lines (default " ")
	ContextPrefix string

	// ContextLines is the number of unchanged lines to show before/after changes in compact mode
	ContextLines int

	// ChunkSeparator is the string used to separate chunks in compact mode
	ChunkSeparator string

	// Compact determines whether to show a compact diff
	Compact bool

	// MinimizeComposition collapses composition changes to a single marker line
	// per composition, omitting the full YAML diff body. Only consumed by the
	// human-readable composition diff renderer; structured output is unaffected.
	MinimizeComposition bool

	// OutputVersion selects the schema of structured (JSON/YAML) output. Empty means
	// OutputVersionV1. The human-readable diff format ignores it.
	OutputVersion OutputVersion
}

// DefaultDiffOptions returns the default options with colors enabled.
func DefaultDiffOptions() DiffOptions {
	return DiffOptions{
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
		Format:         OutputFormatDiff,
		OutputVersion:  OutputVersionV1,
		UseColors:      true,
		AddPrefix:      "+ ",
		DeletePrefix:   "- ",
		ContextPrefix:  "  ",
		ContextLines:   3,
		ChunkSeparator: "...",
		Compact:        false,
	}
}

// DiffFormatter is the interface that defines the contract for diff formatters.
type DiffFormatter interface {
	Format(diffs []diffmatchpatch.Diff, options DiffOptions) string
}

// FullDiffFormatter formats diffs with all context lines.
type FullDiffFormatter struct{}

// CompactDiffFormatter formats diffs with limited context lines.
type CompactDiffFormatter struct{}

// NewFormatter returns a DiffFormatter based on whether compact mode is desired.
func NewFormatter(compact bool) DiffFormatter {
	if compact {
		return &CompactDiffFormatter{}
	}

	return &FullDiffFormatter{}
}

// FormatDiff formats a slice of diffs according to the provided options.
func FormatDiff(diffs []diffmatchpatch.Diff, options DiffOptions) string {
	// Use the appropriate formatter
	formatter := NewFormatter(options.Compact)
	return formatter.Format(diffs, options)
}

// Format implements the DiffFormatter interface for FullDiffFormatter.
func (f *FullDiffFormatter) Format(diffs []diffmatchpatch.Diff, options DiffOptions) string {
	var builder strings.Builder

	for _, diff := range diffs {
		formattedLines, _ := processLines(diff, options)
		for _, line := range formattedLines {
			builder.WriteString(line)
			builder.WriteString("\n")
		}
	}

	return builder.String()
}

// Find change blocks (sequences of inserts/deletes).
type changeBlock struct {
	StartIdx int
	EndIdx   int
}

type lineItem struct {
	Type      diffmatchpatch.Operation
	Content   string
	Formatted string
}

// Format implements the DiffFormatter interface for CompactDiffFormatter.
func (f *CompactDiffFormatter) Format(diffs []diffmatchpatch.Diff, options DiffOptions) string {
	// Create a flat array of all formatted lines with their diff types
	// Preallocate with reasonable initial capacity based on number of diffs
	allLines := make([]lineItem, 0, len(diffs)*4)

	for _, diff := range diffs {
		formattedLines, hasTrailingNewline := processLines(diff, options)

		for i, formatted := range formattedLines {
			// For non-trailing empty lines or regular lines
			content := ""
			if isEmptyTrailer := hasTrailingNewline && len(formattedLines) == 1 && i == 0; !isEmptyTrailer {
				content = strings.Split(diff.Text, "\n")[i]
			}

			allLines = append(allLines, lineItem{
				Type:      diff.Type,
				Content:   content,
				Formatted: formatted,
			})
		}
	}

	var (
		changeBlocks []changeBlock
		currentBlock *changeBlock
	)

	// Identify all the change blocks

	for i, line := range allLines {
		if line.Type != diffmatchpatch.DiffEqual {
			// Start a new block if we don't have one
			if currentBlock == nil {
				currentBlock = &changeBlock{StartIdx: i, EndIdx: i}
			} else {
				// Extend current block
				currentBlock.EndIdx = i
			}
		} else if currentBlock != nil {
			// If we were in a block and hit an equal line, finish the block
			changeBlocks = append(changeBlocks, *currentBlock)
			currentBlock = nil
		}
	}

	// Add the last block if it's still active
	if currentBlock != nil {
		changeBlocks = append(changeBlocks, *currentBlock)
	}

	// If we have no change blocks, return an empty string
	if len(changeBlocks) == 0 {
		return ""
	}

	return f.stringBlocksWithContext(changeBlocks, allLines, options)
}

func (f *CompactDiffFormatter) stringBlocksWithContext(changes []changeBlock, lines []lineItem, opts DiffOptions) string {
	// Now build compact output with context
	var builder strings.Builder

	contextLines := opts.ContextLines

	// Keep track of the last line we printed
	lastPrintedIdx := -1

	// Now process each block with its context
	for blockIdx, block := range changes {
		// Calculate visible range for context before the block
		contextStart := max(0, block.StartIdx-contextLines)

		// If this isn't the first block, check if we need a separator
		if blockIdx > 0 {
			prevBlock := changes[blockIdx-1]
			prevContextEnd := min(len(lines), prevBlock.EndIdx+contextLines+1)

			// If there's a gap between the end of the previous context and the start of this context,
			// add a separator
			if contextStart > prevContextEnd {
				// Add separator
				fmt.Fprintf(&builder, "%s\n", opts.ChunkSeparator)

				lastPrintedIdx = -1 // Reset to force printing of context lines
			} else {
				// Contexts overlap or are adjacent - adjust the start to avoid duplicate lines
				contextStart = max(lastPrintedIdx+1, contextStart)
			}
		}

		// Print context before the change if we haven't already printed it
		for i := contextStart; i < block.StartIdx; i++ {
			if i > lastPrintedIdx {
				builder.WriteString(lines[i].Formatted)
				builder.WriteString("\n")

				lastPrintedIdx = i
			}
		}

		// Print the changes
		for i := block.StartIdx; i <= block.EndIdx; i++ {
			builder.WriteString(lines[i].Formatted)
			builder.WriteString("\n")

			lastPrintedIdx = i
		}

		// Print context after the change
		contextEnd := min(len(lines), block.EndIdx+contextLines+1)
		for i := block.EndIdx + 1; i < contextEnd; i++ {
			builder.WriteString(lines[i].Formatted)
			builder.WriteString("\n")

			lastPrintedIdx = i
		}
	}

	return builder.String()
}

// processLines extracts lines from a diff and processes them into a standardized format
// Returns the processed lines and whether there was a trailing newline.
func processLines(diff diffmatchpatch.Diff, options DiffOptions) ([]string, bool) {
	lines := strings.Split(diff.Text, "\n")
	hasTrailingNewline := strings.HasSuffix(diff.Text, "\n")

	// If there's a trailing newline, the split produces an empty string at the end
	if hasTrailingNewline && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}

	result := make([]string, 0, len(lines))

	// Format each line with appropriate prefix and color
	for _, line := range lines {
		result = append(result, formatLine(line, diff.Type, options))
	}

	// Add formatted empty line if there was just a newline
	if hasTrailingNewline && len(lines) == 0 {
		result = append(result, formatLine("", diff.Type, options))
	}

	return result, hasTrailingNewline
}

// formatLine applies the appropriate prefix and color to a single line.
func formatLine(line string, diffType diffmatchpatch.Operation, options DiffOptions) string {
	var (
		prefix               string
		colorStart, colorEnd string
	)

	switch diffType {
	case diffmatchpatch.DiffInsert:
		prefix = options.AddPrefix
		if options.UseColors {
			colorStart = t.ColorGreen
			colorEnd = t.ColorReset
		}
	case diffmatchpatch.DiffDelete:
		prefix = options.DeletePrefix
		if options.UseColors {
			colorStart = t.ColorRed
			colorEnd = t.ColorReset
		}
	case diffmatchpatch.DiffEqual:
		prefix = options.ContextPrefix
	}

	if options.UseColors && colorStart != "" {
		return fmt.Sprintf("%s%s%s%s", colorStart, prefix, line, colorEnd)
	}

	return fmt.Sprintf("%s%s", prefix, line)
}
