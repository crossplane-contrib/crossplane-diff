package diffprocessor

import (
	"bytes"
	"io"
	"os"
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

// TestGetDiffOptions_PropagatesStdoutStderrFormat verifies that when a ProcessorConfig
// has Stdout, Stderr, and OutputFormat set, those values flow through to the returned
// DiffOptions. When Stdout/Stderr are nil, the defaults (os.Stdout/os.Stderr) must be
// preserved.
func TestGetDiffOptions_PropagatesStdoutStderrFormat(t *testing.T) {
	t.Run("CustomWritersAndFormatPropagate", func(t *testing.T) {
		var (
			stdout bytes.Buffer
			stderr bytes.Buffer
		)

		config := ProcessorConfig{
			Colorize:     true,
			Stdout:       &stdout,
			Stderr:       &stderr,
			OutputFormat: renderer.OutputFormatJSON,
		}

		got := config.GetDiffOptions()

		if got.Stdout != io.Writer(&stdout) {
			t.Errorf("Expected Stdout to be the injected buffer, got: %v", got.Stdout)
		}

		if got.Stderr != io.Writer(&stderr) {
			t.Errorf("Expected Stderr to be the injected buffer, got: %v", got.Stderr)
		}

		if got.Format != renderer.OutputFormatJSON {
			t.Errorf("Expected Format to be %q, got %q", renderer.OutputFormatJSON, got.Format)
		}
	})

	t.Run("NilWritersKeepDefaults", func(t *testing.T) {
		config := ProcessorConfig{
			Colorize: true,
			// Stdout and Stderr intentionally left nil
		}

		got := config.GetDiffOptions()

		// When the config's Stdout/Stderr are nil, DefaultDiffOptions() defaults
		// (os.Stdout/os.Stderr) should be preserved.
		if got.Stdout != io.Writer(os.Stdout) {
			t.Errorf("Expected default os.Stdout when config.Stdout is nil, got: %v", got.Stdout)
		}

		if got.Stderr != io.Writer(os.Stderr) {
			t.Errorf("Expected default os.Stderr when config.Stderr is nil, got: %v", got.Stderr)
		}
	})
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

// TestWithStdoutStderr verifies that WithStdout and WithStderr set the
// corresponding writers on the ProcessorConfig.
func TestWithStdoutStderr(t *testing.T) {
	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	config := ProcessorConfig{}

	WithStdout(&stdout)(&config)
	WithStderr(&stderr)(&config)

	if config.Stdout != io.Writer(&stdout) {
		t.Errorf("Expected config.Stdout to be the injected buffer, got: %v", config.Stdout)
	}

	if config.Stderr != io.Writer(&stderr) {
		t.Errorf("Expected config.Stderr to be the injected buffer, got: %v", config.Stderr)
	}
}
