package diffprocessor

import (
	"testing"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	gcmp "github.com/google/go-cmp/cmp"
)

func TestNewDiffProcessor(t *testing.T) {
	tests := map[string]struct {
		options     []ProcessorOption
		expectError bool
	}{
		"WithOptions": {
			options:     []ProcessorOption{WithNamespace("test"), WithColorize(false), WithCompact(true)},
			expectError: false,
		},
		"BasicOptions": {
			options:     []ProcessorOption{},
			expectError: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			processor := NewDiffProcessor(k8.Clients{}, xp.Clients{}, tt.options...)

			if processor == nil {
				t.Errorf("NewDiffProcessor() returned nil processor")
			}
		})
	}
}

func TestDiffOptions(t *testing.T) {
	tests := []struct {
		name     string
		config   ProcessorConfig
		expected renderer.DiffOptions
	}{
		{
			name: "DefaultOptions",
			config: ProcessorConfig{
				Colorize: true,
				Compact:  false,
			},
			expected: func() renderer.DiffOptions {
				opts := renderer.DefaultDiffOptions()
				opts.UseColors = true
				opts.Compact = false

				return opts
			}(),
		},
		{
			name: "NoColors",
			config: ProcessorConfig{
				Colorize: false,
				Compact:  false,
			},
			expected: func() renderer.DiffOptions {
				opts := renderer.DefaultDiffOptions()
				opts.UseColors = false
				opts.Compact = false

				return opts
			}(),
		},
		{
			name: "CompactDiff",
			config: ProcessorConfig{
				Colorize: true,
				Compact:  true,
			},
			expected: func() renderer.DiffOptions {
				opts := renderer.DefaultDiffOptions()
				opts.UseColors = true
				opts.Compact = true

				return opts
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.GetDiffOptions()

			if diff := gcmp.Diff(tt.expected.UseColors, got.UseColors); diff != "" {
				t.Errorf("GetDiffOptions().UseColors mismatch (-want +got):\n%s", diff)
			}

			if diff := gcmp.Diff(tt.expected.Compact, got.Compact); diff != "" {
				t.Errorf("GetDiffOptions().Compact mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWithOptions(t *testing.T) {
	tests := []struct {
		name     string
		options  []ProcessorOption
		expected ProcessorConfig
	}{
		{
			name: "WithNamespace",
			options: []ProcessorOption{
				WithNamespace("test-namespace"),
			},
			expected: ProcessorConfig{
				Namespace: "test-namespace",
				Colorize:  true,  // Default
				Compact:   false, // Default
			},
		},
		{
			name: "WithMultipleOptions",
			options: []ProcessorOption{
				WithNamespace("test-namespace"),
				WithColorize(false),
				WithCompact(true),
			},
			expected: ProcessorConfig{
				Namespace: "test-namespace",
				Colorize:  false,
				Compact:   true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a default config
			config := ProcessorConfig{
				Namespace: "default",
				Colorize:  true,
				Compact:   false,
			}

			// Apply the options
			for _, option := range tt.options {
				option(&config)
			}

			// Check namespace
			if diff := gcmp.Diff(tt.expected.Namespace, config.Namespace); diff != "" {
				t.Errorf("Namespace mismatch (-want +got):\n%s", diff)
			}

			// Check colorize
			if diff := gcmp.Diff(tt.expected.Colorize, config.Colorize); diff != "" {
				t.Errorf("Colorize mismatch (-want +got):\n%s", diff)
			}

			// Check compact
			if diff := gcmp.Diff(tt.expected.Compact, config.Compact); diff != "" {
				t.Errorf("Compact mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
