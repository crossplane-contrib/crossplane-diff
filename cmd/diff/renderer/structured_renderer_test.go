package renderer

import (
	"bytes"
	"encoding/json"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/runtime/schema"
	sigsyaml "sigs.k8s.io/yaml"
)

// testDiffFixture defines a reusable test case with input diffs and expected output.
type testDiffFixture struct {
	name            string
	diffs           map[string]*dt.ResourceDiff
	errs            []dt.OutputError
	expectedSummary Summary
	expectedChanges int
	// Optional: additional validation beyond summary/changes count
	validate func(t *testing.T, output *StructuredDiffOutput)
}

// sharedDiffFixtures returns test fixtures that should be run through both JSON and YAML renderers.
func sharedDiffFixtures() []testDiffFixture {
	// Create reusable test resources
	addedResource := tu.NewResource("nop.crossplane.io/v1alpha1", "NopResource", "new-resource").
		InNamespace("default").
		WithSpec(map[string]any{
			"forProvider": map[string]any{
				"field": "value",
			},
		}).
		Build()

	modifiedCurrentResource := tu.NewResource("example.org/v1alpha1", "XExample", "modified-resource").
		InNamespace("production").
		WithSpec(map[string]any{
			"oldValue": "something",
		}).
		Build()

	modifiedDesiredResource := tu.NewResource("example.org/v1alpha1", "XExample", "modified-resource").
		InNamespace("production").
		WithSpec(map[string]any{
			"newValue": "something-else",
		}).
		Build()

	removedResource := tu.NewResource("example.org/v1alpha1", "XNopResource", "removed-resource").
		WithSpec(map[string]any{
			"coolField": "goodbye",
		}).
		Build()

	return []testDiffFixture{
		{
			name: "AllDiffTypes",
			diffs: map[string]*dt.ResourceDiff{
				"added": {
					DiffType:     dt.DiffTypeAdded,
					ResourceName: "new-resource",
					Gvk: schema.GroupVersionKind{
						Group:   "nop.crossplane.io",
						Version: "v1alpha1",
						Kind:    "NopResource",
					},
					Desired: addedResource,
				},
				"modified": {
					DiffType:     dt.DiffTypeModified,
					ResourceName: "modified-resource",
					Gvk: schema.GroupVersionKind{
						Group:   "example.org",
						Version: "v1alpha1",
						Kind:    "XExample",
					},
					Current: modifiedCurrentResource,
					Desired: modifiedDesiredResource,
				},
				"removed": {
					DiffType:     dt.DiffTypeRemoved,
					ResourceName: "removed-resource",
					Gvk: schema.GroupVersionKind{
						Group:   "example.org",
						Version: "v1alpha1",
						Kind:    "XNopResource",
					},
					Current: removedResource,
				},
				"equal": {
					DiffType:     dt.DiffTypeEqual,
					ResourceName: "unchanged-resource",
					Gvk: schema.GroupVersionKind{
						Kind: "NopResource",
					},
				},
			},
			expectedSummary: Summary{Added: 1, Modified: 1, Removed: 1},
			expectedChanges: 3, // equal should be excluded
			validate: func(t *testing.T, output *StructuredDiffOutput) {
				t.Helper()

				// Find and verify added resource
				var addedChange *ChangeDetail

				for i := range output.Changes {
					if output.Changes[i].Type == dt.DiffTypeWordAdded {
						addedChange = &output.Changes[i]
						break
					}
				}

				if addedChange == nil {
					t.Fatal("Added resource not found in changes")
				}

				if addedChange.Kind != "NopResource" {
					t.Errorf("Expected Kind 'NopResource', got '%s'", addedChange.Kind)
				}

				if addedChange.Name != "new-resource" {
					t.Errorf("Expected Name 'new-resource', got '%s'", addedChange.Name)
				}

				if addedChange.Namespace != "default" {
					t.Errorf("Expected Namespace 'default', got '%s'", addedChange.Namespace)
				}
			},
		},
		{
			name:            "EmptyDiffs",
			diffs:           map[string]*dt.ResourceDiff{},
			expectedSummary: Summary{Added: 0, Modified: 0, Removed: 0},
			expectedChanges: 0,
		},
		{
			name: "OnlyEqualDiffs",
			diffs: map[string]*dt.ResourceDiff{
				"equal1": {
					DiffType:     dt.DiffTypeEqual,
					ResourceName: "resource-1",
					Gvk:          schema.GroupVersionKind{Kind: "NopResource"},
				},
				"equal2": {
					DiffType:     dt.DiffTypeEqual,
					ResourceName: "resource-2",
					Gvk:          schema.GroupVersionKind{Kind: "NopResource"},
				},
			},
			expectedSummary: Summary{Added: 0, Modified: 0, Removed: 0},
			expectedChanges: 0, // equal diffs should be excluded
		},
		{
			name:  "WithErrors",
			diffs: map[string]*dt.ResourceDiff{},
			errs: []dt.OutputError{
				{ResourceID: "example.org/v1/XResource/my-xr", Message: "failed to render XR: missing composition"},
				{Message: "cluster connection timeout"},
			},
			expectedSummary: Summary{Added: 0, Modified: 0, Removed: 0},
			expectedChanges: 0,
			validate: func(t *testing.T, output *StructuredDiffOutput) {
				t.Helper()

				if len(output.Errors) != 2 {
					t.Fatalf("Expected 2 errors, got %d", len(output.Errors))
				}

				if output.Errors[0].ResourceID != "example.org/v1/XResource/my-xr" {
					t.Errorf("Expected first error ResourceID 'example.org/v1/XResource/my-xr', got '%s'", output.Errors[0].ResourceID)
				}

				if output.Errors[1].Message != "cluster connection timeout" {
					t.Errorf("Expected second error message 'cluster connection timeout', got '%s'", output.Errors[1].Message)
				}
			},
		},
	}
}

func TestStructuredDiffRenderer_RenderDiffs(t *testing.T) {
	formats := []OutputFormat{OutputFormatJSON, OutputFormatYAML}
	fixtures := sharedDiffFixtures()

	for _, format := range formats {
		for _, fixture := range fixtures {
			testName := string(format) + "/" + fixture.name
			t.Run(testName, func(t *testing.T) {
				logger := tu.TestLogger(t, false)
				renderer := NewStructuredDiffRenderer(logger, format)

				var buf bytes.Buffer

				err := renderer.RenderDiffs(&buf, fixture.diffs, fixture.errs)
				if err != nil {
					t.Fatalf("RenderDiffs() failed: %v", err)
				}

				// Parse the output based on format
				var output StructuredDiffOutput

				switch format {
				case OutputFormatJSON:
					if err := json.Unmarshal(buf.Bytes(), &output); err != nil {
						t.Fatalf("Failed to parse JSON output: %v\nOutput: %s", err, buf.String())
					}
				case OutputFormatYAML:
					if err := sigsyaml.Unmarshal(buf.Bytes(), &output); err != nil {
						t.Fatalf("Failed to parse YAML output: %v\nOutput: %s", err, buf.String())
					}
				case OutputFormatDiff:
					t.Fatal("OutputFormatDiff should not be used with StructuredDiffRenderer")
				}

				// Verify summary
				if output.Summary.Added != fixture.expectedSummary.Added {
					t.Errorf("Summary.Added = %d, want %d", output.Summary.Added, fixture.expectedSummary.Added)
				}

				if output.Summary.Modified != fixture.expectedSummary.Modified {
					t.Errorf("Summary.Modified = %d, want %d", output.Summary.Modified, fixture.expectedSummary.Modified)
				}

				if output.Summary.Removed != fixture.expectedSummary.Removed {
					t.Errorf("Summary.Removed = %d, want %d", output.Summary.Removed, fixture.expectedSummary.Removed)
				}

				// Verify change count
				if len(output.Changes) != fixture.expectedChanges {
					t.Errorf("len(Changes) = %d, want %d", len(output.Changes), fixture.expectedChanges)
				}

				// Verify errors round-trip if present
				if len(fixture.errs) > 0 {
					if diff := cmp.Diff(fixture.errs, output.Errors); diff != "" {
						t.Errorf("Errors mismatch (-want +got):\n%s", diff)
					}
				}

				// Run additional validation if provided
				if fixture.validate != nil {
					fixture.validate(t, &output)
				}
			})
		}
	}
}
