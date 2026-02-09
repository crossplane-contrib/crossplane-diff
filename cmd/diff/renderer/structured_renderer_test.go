package renderer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"k8s.io/apimachinery/pkg/runtime/schema"
	sigsyaml "sigs.k8s.io/yaml"
)

func TestStructuredDiffRenderer_RenderDiffs(t *testing.T) {
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

	tests := map[string]struct {
		format          OutputFormat
		diffs           map[string]*dt.ResourceDiff
		expectedSummary Summary
		expectedChanges int
		checkOutput     func(t *testing.T, output string)
	}{
		"JSONWithAllDiffTypes": {
			format: OutputFormatJSON,
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
			checkOutput: func(t *testing.T, output string) {
				t.Helper()

				// Parse JSON and verify added resource details
				var parsed StructuredDiffOutput
				if err := json.Unmarshal([]byte(output), &parsed); err != nil {
					t.Fatalf("Failed to parse JSON: %v", err)
				}

				// Find and verify added resource
				var addedChange *ChangeDetail

				for i := range parsed.Changes {
					if parsed.Changes[i].Type == string(dt.DiffTypeAdded) {
						addedChange = &parsed.Changes[i]
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
		"YAMLFormat": {
			format: OutputFormatYAML,
			diffs: map[string]*dt.ResourceDiff{
				"added": {
					DiffType:     dt.DiffTypeAdded,
					ResourceName: "new-resource",
					Gvk: schema.GroupVersionKind{
						Group:   "nop.crossplane.io",
						Version: "v1alpha1",
						Kind:    "NopResource",
					},
					Desired: tu.NewResource("nop.crossplane.io/v1alpha1", "NopResource", "new-resource").
						WithSpec(map[string]any{
							"field": "value",
						}).
						Build(),
				},
			},
			expectedSummary: Summary{Added: 1, Modified: 0, Removed: 0},
			expectedChanges: 1,
			checkOutput: func(t *testing.T, output string) {
				t.Helper()

				if !strings.Contains(output, "summary:") {
					t.Error("Expected YAML output to contain 'summary:' field")
				}

				if !strings.Contains(output, "changes:") {
					t.Error("Expected YAML output to contain 'changes:' field")
				}
			},
		},
		"EmptyDiffs": {
			format:          OutputFormatJSON,
			diffs:           map[string]*dt.ResourceDiff{},
			expectedSummary: Summary{Added: 0, Modified: 0, Removed: 0},
			expectedChanges: 0,
		},
		"OnlyEqualDiffs": {
			format: OutputFormatJSON,
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
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger := tu.TestLogger(t, false)
			renderer := NewStructuredDiffRenderer(logger, tt.format)

			var buf bytes.Buffer

			err := renderer.RenderDiffs(&buf, tt.diffs)
			if err != nil {
				t.Fatalf("RenderDiffs() failed: %v", err)
			}

			// Parse the output based on format
			var output StructuredDiffOutput

			switch tt.format {
			case OutputFormatJSON:
				if err := json.Unmarshal(buf.Bytes(), &output); err != nil {
					t.Fatalf("Failed to parse JSON output: %v", err)
				}
			case OutputFormatYAML:
				if err := sigsyaml.Unmarshal(buf.Bytes(), &output); err != nil {
					t.Fatalf("Failed to parse YAML output: %v", err)
				}
			case OutputFormatDiff:
				t.Fatal("OutputFormatDiff should not be used with StructuredDiffRenderer")
			}

			// Verify summary
			if output.Summary.Added != tt.expectedSummary.Added {
				t.Errorf("Summary.Added = %d, want %d", output.Summary.Added, tt.expectedSummary.Added)
			}

			if output.Summary.Modified != tt.expectedSummary.Modified {
				t.Errorf("Summary.Modified = %d, want %d", output.Summary.Modified, tt.expectedSummary.Modified)
			}

			if output.Summary.Removed != tt.expectedSummary.Removed {
				t.Errorf("Summary.Removed = %d, want %d", output.Summary.Removed, tt.expectedSummary.Removed)
			}

			// Verify change count
			if len(output.Changes) != tt.expectedChanges {
				t.Errorf("len(Changes) = %d, want %d", len(output.Changes), tt.expectedChanges)
			}

			// Run additional output checks if provided
			if tt.checkOutput != nil {
				tt.checkOutput(t, buf.String())
			}
		})
	}
}
