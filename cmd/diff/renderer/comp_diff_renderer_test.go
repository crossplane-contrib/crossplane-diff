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

// testCompDiffFixture defines a reusable test case with input and expected output.
type testCompDiffFixture struct {
	name     string
	output   *CompDiffOutput
	validate func(t *testing.T, format OutputFormat, result string)
}

// sharedCompDiffFixtures returns test fixtures that should be run through both JSON and YAML renderers.
func sharedCompDiffFixtures() []testCompDiffFixture {
	return []testCompDiffFixture{
		{
			name:   "EmptyCompositions",
			output: &CompDiffOutput{Compositions: []CompositionDiff{}},
			validate: func(t *testing.T, format OutputFormat, result string) {
				t.Helper()

				if format == OutputFormatJSON {
					var parsed compDiffJSONOutput
					if err := json.Unmarshal([]byte(result), &parsed); err != nil {
						t.Fatalf("Failed to parse JSON: %v", err)
					}

					if len(parsed.Compositions) != 0 {
						t.Errorf("Expected 0 compositions, got %d", len(parsed.Compositions))
					}
				} else if !strings.Contains(result, "compositions:") {
					t.Error("Expected YAML to contain 'compositions:'")
				}
			},
		},
		{
			name: "WithChanges",
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
			validate: func(t *testing.T, format OutputFormat, result string) {
				t.Helper()

				if format == OutputFormatJSON {
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

					comps, ok := rawParsed["compositions"].([]any)
					if !ok {
						t.Fatalf("Expected 'compositions' to be array, got %T", rawParsed["compositions"])
					}

					comp, ok := comps[0].(map[string]any)
					if !ok {
						t.Fatalf("Expected compositions[0] to be object, got %T", comps[0])
					}

					impacts, ok := comp["impactAnalysis"].([]any)
					if !ok {
						t.Fatalf("Expected 'impactAnalysis' to be array, got %T", comp["impactAnalysis"])
					}

					impact, ok := impacts[0].(map[string]any)
					if !ok {
						t.Fatalf("Expected impacts[0] to be object, got %T", impacts[0])
					}

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
				} else {
					if !strings.Contains(result, "compositions:") {
						t.Error("Expected YAML to contain 'compositions:'")
					}

					if !strings.Contains(result, "name: test-comp") {
						t.Error("Expected YAML to contain 'name: test-comp'")
					}
				}
			},
		},
	}
}

func TestStructuredCompDiffRenderer_RenderCompDiff(t *testing.T) {
	formats := []OutputFormat{OutputFormatJSON, OutputFormatYAML}
	fixtures := sharedCompDiffFixtures()

	for _, format := range formats {
		for _, fixture := range fixtures {
			testName := string(format) + "/" + fixture.name
			t.Run(testName, func(t *testing.T) {
				logger := tu.TestLogger(t, false)

				var buf bytes.Buffer

				opts := DefaultDiffOptions()
				opts.Format = format
				opts.Stdout = &buf
				opts.Stderr = &bytes.Buffer{} // discard stderr

				renderer := NewStructuredCompDiffRenderer(logger, opts)

				err := renderer.RenderCompDiff(fixture.output)
				if err != nil {
					t.Fatalf("RenderCompDiff() failed: %v", err)
				}

				fixture.validate(t, format, buf.String())
			})
		}
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
		"CompositionWithError": {
			output: &CompDiffOutput{
				Compositions: []CompositionDiff{{
					Name:  "error-comp",
					Error: errors.New("failed to fetch composition from cluster"),
					AffectedResources: AffectedResourcesSummary{
						Total:       0,
						WithChanges: 0,
						Unchanged:   0,
						WithErrors:  0,
					},
					ImpactAnalysis: []XRImpact{},
				}},
			},
			colorize: false,
			validate: func(t *testing.T, result string) {
				t.Helper()

				// Should show the error message
				if !strings.Contains(result, "Error processing composition error-comp") {
					t.Error("Expected error processing message")
				}

				if !strings.Contains(result, "failed to fetch composition from cluster") {
					t.Error("Expected error details")
				}

				// Should NOT show affected resources or impact analysis sections
				if strings.Contains(result, "=== Affected Composite Resources ===") {
					t.Error("Should not show affected resources header when composition has error")
				}

				if strings.Contains(result, "=== Impact Analysis ===") {
					t.Error("Should not show impact analysis header when composition has error")
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger := tu.TestLogger(t, false)

			var buf bytes.Buffer

			opts := DefaultDiffOptions()
			opts.UseColors = tt.colorize
			opts.Stdout = &buf
			opts.Stderr = &bytes.Buffer{} // discard stderr

			diffRenderer := NewDiffRenderer(logger, opts)
			renderer := NewDefaultCompDiffRenderer(logger, diffRenderer, opts)

			err := renderer.RenderCompDiff(tt.output)
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

// TestCompDiffOutput_JSONSchema validates that the internal CompDiffOutput type
// serializes to JSON with the correct schema structure. This includes verifying:
// - compositionChanges field appears with correct type symbol
// - downstreamChanges is present for changed XRs with proper summary counts
// - error field serializes correctly on error impacts
// - nested structures (ImpactAnalysis, AffectedResources) serialize properly
//
// YAML serialization uses the same JSON tags (via sigs.k8s.io/yaml), so if JSON
// serializes correctly, YAML will too. YAML format coverage is provided by
// TestStructuredCompDiffRenderer_RenderCompDiff.
func TestCompDiffOutput_JSONSchema(t *testing.T) {
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

	var jsonBuf bytes.Buffer

	opts := DefaultDiffOptions()
	opts.Format = OutputFormatJSON
	opts.Stdout = &jsonBuf
	opts.Stderr = &bytes.Buffer{} // discard stderr

	jsonRenderer := NewStructuredCompDiffRenderer(logger, opts)

	if err := jsonRenderer.RenderCompDiff(output); err != nil {
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

	if comp.CompositionChanges.Type != "modified" {
		t.Errorf("Expected compositionChanges.type 'modified', got '%s'", comp.CompositionChanges.Type)
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
}
