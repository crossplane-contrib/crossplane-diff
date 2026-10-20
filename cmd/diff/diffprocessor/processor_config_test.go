package diffprocessor

import (
	"testing"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	gcmp "github.com/google/go-cmp/cmp"
)

func TestNewDiffProcessor(t *testing.T) {
	tests := map[string]struct {
		options     []ProcessorOption
		expectError bool
	}{
		"WithOptions": {
			options:     []ProcessorOption{WithMaxNestedDepth(3), WithIncludeManual(true)},
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

// TestProcessorsDefaultEngineLimits verifies a caller that bypasses the CLI still gets the CLI's
// engine limits, and that an explicit option overrides them.
func TestProcessorsDefaultEngineLimits(t *testing.T) {
	type limits struct{ MaxNestedDepth, MaxRenderIterations int }

	defaults := limits{DefaultMaxNestedDepth, DefaultMaxRenderIterations}

	xr := func(opts ...ProcessorOption) limits {
		c := NewDiffProcessor(k8.Clients{}, xp.Clients{}, opts...).(*DefaultDiffProcessor).config
		return limits{c.MaxNestedDepth, c.MaxRenderIterations}
	}
	comp := func(opts ...ProcessorOption) limits {
		c := NewCompDiffProcessor(nil, nil, opts...).(*DefaultCompDiffProcessor).config
		return limits{c.MaxNestedDepth, c.MaxRenderIterations}
	}

	tests := map[string]struct {
		got  func(...ProcessorOption) limits
		opts []ProcessorOption
		want limits
	}{
		"XRDefaults":     {got: xr, want: defaults},
		"CompDefaults":   {got: comp, want: defaults},
		"XROverridden":   {got: xr, opts: []ProcessorOption{WithMaxNestedDepth(3), WithMaxRenderIterations(4)}, want: limits{3, 4}},
		"CompOverridden": {got: comp, opts: []ProcessorOption{WithMaxNestedDepth(3), WithMaxRenderIterations(4)}, want: limits{3, 4}},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gcmp.Diff(tt.want, tt.got(tt.opts...)); diff != "" {
				t.Errorf("limits mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestWithCrossplaneRenderOverrides verifies that the render-backend selector
// options set the corresponding ProcessorConfig fields, so they thread through
// to render.EngineFlags when the default engine-backed RenderFn is built.
func TestWithCrossplaneRenderOverrides(t *testing.T) {
	tests := map[string]struct {
		option ProcessorOption
		want   ProcessorConfig
	}{
		"WithCrossplaneVersion": {
			option: WithCrossplaneVersion("v2.4.0"),
			want:   ProcessorConfig{CrossplaneVersion: "v2.4.0"},
		},
		"WithCrossplaneImage": {
			option: WithCrossplaneImage("example.com/mirror/crossplane:v2.3.4"),
			want:   ProcessorConfig{CrossplaneImage: "example.com/mirror/crossplane:v2.3.4"},
		},
		"WithCrossplaneRenderBinary": {
			option: WithCrossplaneRenderBinary("/usr/local/bin/crossplane"),
			want:   ProcessorConfig{CrossplaneRenderBinary: "/usr/local/bin/crossplane"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var config ProcessorConfig
			tt.option(&config)

			if diff := gcmp.Diff(tt.want.CrossplaneVersion, config.CrossplaneVersion); diff != "" {
				t.Errorf("CrossplaneVersion mismatch (-want +got):\n%s", diff)
			}

			if diff := gcmp.Diff(tt.want.CrossplaneImage, config.CrossplaneImage); diff != "" {
				t.Errorf("CrossplaneImage mismatch (-want +got):\n%s", diff)
			}

			if diff := gcmp.Diff(tt.want.CrossplaneRenderBinary, config.CrossplaneRenderBinary); diff != "" {
				t.Errorf("CrossplaneRenderBinary mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNewDiffProcessorThreadsRenderBackendToEngine closes the gap
// TestWithCrossplaneRenderOverrides leaves open: that test proves the options
// land on the ProcessorConfig, not that NewDiffProcessor then hands them to the
// engine. Blanking both selector arguments at the NewEngineRenderFn call site
// used to leave the whole package green (crossplane-diff#488).
func TestNewDiffProcessorThreadsRenderBackendToEngine(t *testing.T) {
	tests := map[string]struct {
		options []ProcessorOption
		want    string
	}{
		"NoSelectorResolvesStable": {
			want: render.DefaultCrossplaneImage + ":stable",
		},
		"CrossplaneVersionReachesEngine": {
			options: []ProcessorOption{WithCrossplaneVersion("v2.4.0")},
			want:    render.DefaultCrossplaneImage + ":v2.4.0",
		},
		"BareCrossplaneVersionReachesEngineNormalized": {
			options: []ProcessorOption{WithCrossplaneVersion("2.3.4")},
			want:    render.DefaultCrossplaneImage + ":v2.3.4",
		},
		"CrossplaneImageReachesEngine": {
			options: []ProcessorOption{WithCrossplaneImage("example.com/mirror/crossplane:v2.4.1")},
			want:    "example.com/mirror/crossplane:v2.4.1",
		},
		"CrossplaneRenderBinaryReachesEngine": {
			options: []ProcessorOption{WithCrossplaneRenderBinary("/usr/local/bin/crossplane")},
			want:    "",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			processor := NewDiffProcessor(k8.Clients{}, xp.Clients{}, tt.options...)

			p, ok := processor.(*DefaultDiffProcessor)
			if !ok {
				t.Fatalf("NewDiffProcessor() returned %T, want *DefaultDiffProcessor", processor)
			}

			if p.engineFn == nil {
				t.Fatal("NewDiffProcessor() left engineFn nil; the default engine-backed RenderFn was not built")
			}

			if diff := gcmp.Diff(tt.want, p.engineFn.RenderImage()); diff != "" {
				t.Errorf("engine render image mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestWithMaxRecvMessageSize verifies the option sets the field and that the
// zero value is preserved when the option is not applied.
func TestWithMaxRecvMessageSize(t *testing.T) {
	def := ProcessorConfig{}
	if def.MaxRecvMessageSize != 0 {
		t.Errorf("Expected default MaxRecvMessageSize to be 0, got: %d", def.MaxRecvMessageSize)
	}

	config := ProcessorConfig{}
	WithMaxRecvMessageSize(16)(&config)

	if config.MaxRecvMessageSize != 16 {
		t.Errorf("Expected config.MaxRecvMessageSize to be 16, got: %d", config.MaxRecvMessageSize)
	}
}
