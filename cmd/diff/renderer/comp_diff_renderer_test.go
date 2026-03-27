package renderer

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	corev1 "k8s.io/api/core/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestStructuredCompDiffRenderer_RenderCompDiff(t *testing.T) {
	tests := map[string]struct {
		format   OutputFormat
		output   *CompDiffOutput
		validate func(t *testing.T, result string)
	}{
		"JSONEmptyCompositions": {
			format: OutputFormatJSON,
			output: &CompDiffOutput{Compositions: []CompositionDiff{}},
			validate: func(t *testing.T, result string) {
				t.Helper()

				var parsed compDiffJSONOutput
				if err := json.Unmarshal([]byte(result), &parsed); err != nil {
					t.Fatalf("Failed to parse JSON: %v", err)
				}

				if len(parsed.Compositions) != 0 {
					t.Errorf("Expected 0 compositions, got %d", len(parsed.Compositions))
				}
			},
		},
		"JSONWithChanges": {
			format: OutputFormatJSON,
			output: &CompDiffOutput{
				Compositions: []CompositionDiff{{
					Name: "test-comp",
					CompositionDiff: &dt.ResourceDiff{
						DiffType:     dt.DiffTypeAdded,
						ResourceName: "test-comp",
						Gvk:          schema.GroupVersionKind{Group: "apiextensions.crossplane.io", Version: "v1", Kind: "Composition"},
						Desired:      &un.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.crossplane.io/v1", "kind": "Composition"}},
					},
					AffectedResources: AffectedResourcesSummary{Total: 2, WithChanges: 1, Unchanged: 1},
					ImpactAnalysis: []XRImpact{
						{ObjectReference: corev1.ObjectReference{APIVersion: "example.org/v1", Kind: "XResource", Name: "xr-1"}, Status: XRStatusChanged},
						{ObjectReference: corev1.ObjectReference{APIVersion: "example.org/v1", Kind: "XResource", Name: "xr-2"}, Status: XRStatusUnchanged},
					},
				}},
			},
			validate: func(t *testing.T, result string) {
				t.Helper()

				var parsed compDiffJSONOutput
				if err := json.Unmarshal([]byte(result), &parsed); err != nil {
					t.Fatalf("Failed to parse JSON: %v", err)
				}

				if len(parsed.Compositions) != 1 {
					t.Fatalf("Expected 1 composition, got %d", len(parsed.Compositions))
				}

				if parsed.Compositions[0].Name != "test-comp" {
					t.Errorf("Expected name 'test-comp', got '%s'", parsed.Compositions[0].Name)
				}

				if parsed.Compositions[0].AffectedResources.Total != 2 {
					t.Errorf("Expected total 2, got %d", parsed.Compositions[0].AffectedResources.Total)
				}
				// Verify compositionChanges is present in JSON
				if parsed.Compositions[0].CompositionChanges == nil {
					t.Error("Expected compositionChanges to be present")
				}

				// Verify embedded ObjectReference fields are at top level (not nested)
				// This tests that json:",inline" works correctly
				var rawParsed map[string]any
				if err := json.Unmarshal([]byte(result), &rawParsed); err != nil {
					t.Fatalf("Failed to parse raw JSON: %v", err)
				}

				comps := rawParsed["compositions"].([]any)
				comp := comps[0].(map[string]any)
				impacts := comp["impactAnalysis"].([]any)
				impact := impacts[0].(map[string]any)

				// Verify apiVersion, kind, name are top-level keys (not nested under "objectReference")
				if _, ok := impact["apiVersion"]; !ok {
					t.Error("Expected 'apiVersion' to be a top-level field in xrImpactJSON (embedded from ObjectReference)")
				}
				if _, ok := impact["kind"]; !ok {
					t.Error("Expected 'kind' to be a top-level field in xrImpactJSON (embedded from ObjectReference)")
				}
				if _, ok := impact["name"]; !ok {
					t.Error("Expected 'name' to be a top-level field in xrImpactJSON (embedded from ObjectReference)")
				}
				// ObjectReference should NOT be nested
				if _, ok := impact["objectReference"]; ok {
					t.Error("ObjectReference should be inlined, not a nested field")
				}
			},
		},
		"YAMLFormat": {
			format: OutputFormatYAML,
			output: &CompDiffOutput{
				Compositions: []CompositionDiff{{
					Name:              "test-comp",
					AffectedResources: AffectedResourcesSummary{Total: 1, Unchanged: 1},
					ImpactAnalysis:    []XRImpact{{ObjectReference: corev1.ObjectReference{APIVersion: "example.org/v1", Kind: "XResource", Name: "xr-1"}, Status: XRStatusUnchanged}},
				}},
			},
			validate: func(t *testing.T, result string) {
				t.Helper()

				if !strings.Contains(result, "compositions:") {
					t.Error("Expected YAML to contain 'compositions:'")
				}

				if !strings.Contains(result, "name: test-comp") {
					t.Error("Expected YAML to contain 'name: test-comp'")
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger := tu.TestLogger(t, false)
			renderer := NewStructuredCompDiffRenderer(logger, tt.format)

			var buf bytes.Buffer

			err := renderer.RenderCompDiff(&buf, tt.output)
			if err != nil {
				t.Fatalf("RenderCompDiff() failed: %v", err)
			}

			tt.validate(t, buf.String())
		})
	}
}

func TestDefaultCompDiffRenderer_RenderCompDiff(t *testing.T) {
	tests := map[string]struct {
		output   *CompDiffOutput
		colorize bool
		validate func(t *testing.T, result string)
	}{
		"EmptyCompositions": {
			output:   &CompDiffOutput{Compositions: []CompositionDiff{}},
			colorize: false,
			validate: func(t *testing.T, result string) {
				t.Helper()

				if result != "" {
					t.Errorf("Expected empty output, got: %q", result)
				}
			},
		},
		"NoChangesComposition": {
			output: &CompDiffOutput{
				Compositions: []CompositionDiff{{
					Name:              "test-comp",
					AffectedResources: AffectedResourcesSummary{Total: 1, Unchanged: 1},
					ImpactAnalysis:    []XRImpact{{ObjectReference: corev1.ObjectReference{APIVersion: "example.org/v1", Kind: "XResource", Name: "xr-1"}, Status: XRStatusUnchanged}},
				}},
			},
			colorize: false,
			validate: func(t *testing.T, result string) {
				t.Helper()

				if !strings.Contains(result, "=== Composition Changes ===") {
					t.Error("Expected composition changes header")
				}

				if !strings.Contains(result, "No changes detected in composition test-comp") {
					t.Error("Expected no changes message")
				}

				if !strings.Contains(result, "=== Affected Composite Resources ===") {
					t.Error("Expected affected resources header")
				}
			},
		},
		"FilteredByPolicy": {
			output: &CompDiffOutput{
				Compositions: []CompositionDiff{{
					Name:              "test-comp",
					AffectedResources: AffectedResourcesSummary{FilteredByPolicy: 3},
					ImpactAnalysis:    []XRImpact{},
				}},
			},
			colorize: false,
			validate: func(t *testing.T, result string) {
				t.Helper()

				if !strings.Contains(result, "Manual update policy") {
					t.Error("Expected Manual update policy message")
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger := tu.TestLogger(t, false)
			diffRenderer := NewDiffRenderer(logger, DefaultDiffOptions())
			renderer := NewDefaultCompDiffRenderer(logger, diffRenderer, tt.colorize)

			var buf bytes.Buffer

			err := renderer.RenderCompDiff(&buf, tt.output)
			if err != nil {
				t.Fatalf("RenderCompDiff() failed: %v", err)
			}

			tt.validate(t, buf.String())
		})
	}
}

func Test_formatXRStatusSummary(t *testing.T) {
	tests := map[string]struct {
		changed, unchanged, errors int
		want                       string
	}{
		"NoResources":     {0, 0, 0, ""},
		"OneChanged":      {1, 0, 0, "\nSummary: 1 resource with changes\n"},
		"MultipleChanged": {5, 0, 0, "\nSummary: 5 resources with changes\n"},
		"AllTypes":        {2, 3, 1, "\nSummary: 2 resources with changes, 3 resources unchanged, 1 resource with errors\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := formatXRStatusSummary(tt.changed, tt.unchanged, tt.errors)
			if got != tt.want {
				t.Errorf("formatXRStatusSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func Test_pluralize(t *testing.T) {
	if pluralize(1) != "" {
		t.Error("pluralize(1) should return empty string")
	}

	if pluralize(0) != "s" {
		t.Error("pluralize(0) should return 's'")
	}

	if pluralize(2) != "s" {
		t.Error("pluralize(2) should return 's'")
	}
}

func TestCompDiffOutput_JSONSchema(t *testing.T) {
	// Create internal representation with ResourceDiff
	output := &CompDiffOutput{
		Compositions: []CompositionDiff{{
			Name: "xbuckets.example.org",
			CompositionDiff: &dt.ResourceDiff{
				DiffType:     dt.DiffTypeModified,
				ResourceName: "xbuckets.example.org",
				Gvk:          schema.GroupVersionKind{Group: "apiextensions.crossplane.io", Version: "v1", Kind: "Composition"},
				Current:      &un.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.crossplane.io/v1", "kind": "Composition"}},
				Desired:      &un.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.crossplane.io/v1", "kind": "Composition"}},
			},
			AffectedResources: AffectedResourcesSummary{Total: 5, WithChanges: 2, Unchanged: 2, WithErrors: 1},
			ImpactAnalysis: []XRImpact{
				{
					ObjectReference: corev1.ObjectReference{APIVersion: "example.org/v1", Kind: "XBucket", Name: "bucket-1"},
					Status:          XRStatusChanged,
					Diffs: map[string]*dt.ResourceDiff{
						"s3.aws.upbound.io/v1beta1/Bucket//new-bucket": {
							DiffType:     dt.DiffTypeAdded,
							ResourceName: "new-bucket",
							Gvk:          schema.GroupVersionKind{Group: "s3.aws.upbound.io", Version: "v1beta1", Kind: "Bucket"},
							Desired:      &un.Unstructured{Object: map[string]any{"apiVersion": "s3.aws.upbound.io/v1beta1", "kind": "Bucket"}},
						},
						"s3.aws.upbound.io/v1beta1/Bucket//existing-bucket": {
							DiffType:     dt.DiffTypeModified,
							ResourceName: "existing-bucket",
							Gvk:          schema.GroupVersionKind{Group: "s3.aws.upbound.io", Version: "v1beta1", Kind: "Bucket"},
							Current:      &un.Unstructured{Object: map[string]any{"apiVersion": "s3.aws.upbound.io/v1beta1", "kind": "Bucket"}},
							Desired:      &un.Unstructured{Object: map[string]any{"apiVersion": "s3.aws.upbound.io/v1beta1", "kind": "Bucket"}},
						},
					},
				},
				{ObjectReference: corev1.ObjectReference{APIVersion: "example.org/v1", Kind: "XBucket", Name: "bucket-2"}, Status: XRStatusUnchanged},
				{ObjectReference: corev1.ObjectReference{APIVersion: "example.org/v1", Kind: "XBucket", Name: "bucket-3"}, Status: XRStatusError, Error: errors.New("render failed")},
			},
		}},
	}

	// Test via the structured renderer (JSON)
	logger := tu.TestLogger(t, false)
	jsonRenderer := NewStructuredCompDiffRenderer(logger, OutputFormatJSON)

	var jsonBuf bytes.Buffer
	if err := jsonRenderer.RenderCompDiff(&jsonBuf, output); err != nil {
		t.Fatalf("Failed to render JSON: %v", err)
	}

	var parsed compDiffJSONOutput
	if err := json.Unmarshal(jsonBuf.Bytes(), &parsed); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	if len(parsed.Compositions) != 1 {
		t.Fatalf("Expected 1 composition, got %d", len(parsed.Compositions))
	}

	comp := parsed.Compositions[0]
	if comp.AffectedResources.Total != 5 {
		t.Errorf("Expected total 5, got %d", comp.AffectedResources.Total)
	}

	if len(comp.ImpactAnalysis) != 3 {
		t.Errorf("Expected 3 impacts, got %d", len(comp.ImpactAnalysis))
	}

	// Verify compositionChanges in JSON
	if comp.CompositionChanges == nil {
		t.Error("Expected compositionChanges to be present")
	}

	if comp.CompositionChanges.Type != "~" {
		t.Errorf("Expected compositionChanges.type '~', got '%s'", comp.CompositionChanges.Type)
	}

	// Verify error impact in JSON
	if comp.ImpactAnalysis[2].Error != "render failed" {
		t.Errorf("Expected error 'render failed', got '%s'", comp.ImpactAnalysis[2].Error)
	}

	// Verify changed impact has downstreamChanges in JSON
	if comp.ImpactAnalysis[0].DownstreamChanges == nil {
		t.Error("Expected downstreamChanges for changed XR")
	}

	if comp.ImpactAnalysis[0].DownstreamChanges.Summary.Added != 1 {
		t.Errorf("Expected 1 added, got %d", comp.ImpactAnalysis[0].DownstreamChanges.Summary.Added)
	}

	if comp.ImpactAnalysis[0].DownstreamChanges.Summary.Modified != 1 {
		t.Errorf("Expected 1 modified, got %d", comp.ImpactAnalysis[0].DownstreamChanges.Summary.Modified)
	}

	// Test via the structured renderer (YAML)
	yamlRenderer := NewStructuredCompDiffRenderer(logger, OutputFormatYAML)

	var yamlBuf bytes.Buffer
	if err := yamlRenderer.RenderCompDiff(&yamlBuf, output); err != nil {
		t.Fatalf("Failed to render YAML: %v", err)
	}

	if !strings.Contains(yamlBuf.String(), "compositions:") {
		t.Error("YAML should contain 'compositions:'")
	}
}
