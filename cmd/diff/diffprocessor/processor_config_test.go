package diffprocessor

import (
	"testing"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
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

			if got.UseColors != tt.expected.UseColors {
				t.Errorf("GetDiffOptions().UseColors = %v, want %v", got.UseColors, tt.expected.UseColors)
			}

			if got.Compact != tt.expected.Compact {
				t.Errorf("GetDiffOptions().Compact = %v, want %v", got.Compact, tt.expected.Compact)
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
			if config.Namespace != tt.expected.Namespace {
				t.Errorf("Namespace = %v, want %v", config.Namespace, tt.expected.Namespace)
			}

			// Check colorize
			if config.Colorize != tt.expected.Colorize {
				t.Errorf("Colorize = %v, want %v", config.Colorize, tt.expected.Colorize)
			}

			// Check compact
			if config.Compact != tt.expected.Compact {
				t.Errorf("Compact = %v, want %v", config.Compact, tt.expected.Compact)
			}
		})
	}
}
