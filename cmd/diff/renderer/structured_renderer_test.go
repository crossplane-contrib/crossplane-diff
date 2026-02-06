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

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

func TestStructuredDiffRenderer_RenderDiffs_JSON(t *testing.T) {
	logger := logging.NewNopLogger()
	renderer := NewStructuredDiffRenderer(logger, OutputFormatJSON)

	diffs := map[string]*dt.ResourceDiff{
		"added": {
			DiffType:     dt.DiffTypeAdded,
			ResourceName: "new-resource",
			Gvk: schema.GroupVersionKind{
				Group:   "nop.crossplane.io",
				Version: "v1alpha1",
				Kind:    "NopResource",
			},
			Desired: tu.NewResource("nop.crossplane.io/v1alpha1", "NopResource", "new-resource").
				InNamespace("default").
				WithSpec(map[string]any{
					"forProvider": map[string]any{
						"field": "value",
					},
				}).
				Build(),
		},
		"modified": {
			DiffType:     dt.DiffTypeModified,
			ResourceName: "modified-resource",
			Gvk: schema.GroupVersionKind{
				Group:   "example.org",
				Version: "v1alpha1",
				Kind:    "XExample",
			},
			Current: tu.NewResource("example.org/v1alpha1", "XExample", "modified-resource").
				InNamespace("production").
				WithSpec(map[string]any{
					"oldValue": "something",
				}).
				Build(),
			Desired: tu.NewResource("example.org/v1alpha1", "XExample", "modified-resource").
				InNamespace("production").
				WithSpec(map[string]any{
					"newValue": "something-else",
				}).
				Build(),
		},
		"removed": {
			DiffType:     dt.DiffTypeRemoved,
			ResourceName: "removed-resource",
			Gvk: schema.GroupVersionKind{
				Group:   "example.org",
				Version: "v1alpha1",
				Kind:    "XNopResource",
			},
			Current: tu.NewResource("example.org/v1alpha1", "XNopResource", "removed-resource").
				WithSpec(map[string]any{
					"coolField": "goodbye",
				}).
				Build(),
		},
		"equal": {
			DiffType:     dt.DiffTypeEqual,
			ResourceName: "unchanged-resource",
			Gvk: schema.GroupVersionKind{
				Kind: "NopResource",
			},
		},
	}

	var buf bytes.Buffer

	err := renderer.RenderDiffs(&buf, diffs)
	if err != nil {
		t.Fatalf("RenderDiffs failed: %v", err)
	}

	// Parse the JSON output
	var output StructuredDiffOutput

	err = json.Unmarshal(buf.Bytes(), &output)
	if err != nil {
		t.Fatalf("Failed to parse JSON output: %v", err)
	}

	// Verify summary
	if output.Summary.Added != 1 {
		t.Errorf("Expected 1 added resource, got %d", output.Summary.Added)
	}

	if output.Summary.Modified != 1 {
		t.Errorf("Expected 1 modified resource, got %d", output.Summary.Modified)
	}

	if output.Summary.Removed != 1 {
		t.Errorf("Expected 1 removed resource, got %d", output.Summary.Removed)
	}

	// Verify changes (equal should be excluded)
	if len(output.Changes) != 3 {
		t.Errorf("Expected 3 changes, got %d", len(output.Changes))
	}

	// Find the added resource
	var addedChange *ChangeDetail

	for i := range output.Changes {
		if output.Changes[i].Type == string(dt.DiffTypeAdded) {
			addedChange = &output.Changes[i]
			break
		}
	}

	if addedChange == nil {
		t.Fatal("Added resource not found in changes")
	}

	// Verify added resource details
	if addedChange.Kind != "NopResource" {
		t.Errorf("Expected Kind 'NopResource', got '%s'", addedChange.Kind)
	}

	if addedChange.Name != "new-resource" {
		t.Errorf("Expected Name 'new-resource', got '%s'", addedChange.Name)
	}

	if addedChange.Namespace != "default" {
		t.Errorf("Expected Namespace 'default', got '%s'", addedChange.Namespace)
	}
}

func TestStructuredDiffRenderer_RenderDiffs_YAML(t *testing.T) {
	logger := logging.NewNopLogger()
	renderer := NewStructuredDiffRenderer(logger, OutputFormatYAML)

	diffs := map[string]*dt.ResourceDiff{
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
	}

	var buf bytes.Buffer

	err := renderer.RenderDiffs(&buf, diffs)
	if err != nil {
		t.Fatalf("RenderDiffs failed: %v", err)
	}

	// Parse the YAML output
	var output StructuredDiffOutput

	err = sigsyaml.Unmarshal(buf.Bytes(), &output)
	if err != nil {
		t.Fatalf("Failed to parse YAML output: %v", err)
	}

	// Verify summary
	if output.Summary.Added != 1 {
		t.Errorf("Expected 1 added resource, got %d", output.Summary.Added)
	}

	// Verify YAML format (should contain YAML markers)
	yamlOutput := buf.String()
	if !strings.Contains(yamlOutput, "summary:") {
		t.Error("Expected YAML output to contain 'summary:' field")
	}

	if !strings.Contains(yamlOutput, "changes:") {
		t.Error("Expected YAML output to contain 'changes:' field")
	}
}

func TestStructuredDiffRenderer_EmptyDiffs(t *testing.T) {
	logger := logging.NewNopLogger()
	renderer := NewStructuredDiffRenderer(logger, OutputFormatJSON)

	diffs := map[string]*dt.ResourceDiff{}

	var buf bytes.Buffer

	err := renderer.RenderDiffs(&buf, diffs)
	if err != nil {
		t.Fatalf("RenderDiffs failed: %v", err)
	}

	// Parse the JSON output
	var output StructuredDiffOutput

	err = json.Unmarshal(buf.Bytes(), &output)
	if err != nil {
		t.Fatalf("Failed to parse JSON output: %v", err)
	}

	// Verify empty summary
	if output.Summary.Added != 0 || output.Summary.Modified != 0 || output.Summary.Removed != 0 {
		t.Error("Expected all summary counts to be 0")
	}

	// Verify empty changes
	if len(output.Changes) != 0 {
		t.Errorf("Expected 0 changes, got %d", len(output.Changes))
	}
}

func TestStructuredDiffRenderer_OnlyEqualDiffs(t *testing.T) {
	logger := logging.NewNopLogger()
	renderer := NewStructuredDiffRenderer(logger, OutputFormatJSON)

	diffs := map[string]*dt.ResourceDiff{
		"equal1": {
			DiffType:     dt.DiffTypeEqual,
			ResourceName: "resource-1",
			Gvk: schema.GroupVersionKind{
				Kind: "NopResource",
			},
		},
		"equal2": {
			DiffType:     dt.DiffTypeEqual,
			ResourceName: "resource-2",
			Gvk: schema.GroupVersionKind{
				Kind: "NopResource",
			},
		},
	}

	var buf bytes.Buffer

	err := renderer.RenderDiffs(&buf, diffs)
	if err != nil {
		t.Fatalf("RenderDiffs failed: %v", err)
	}

	// Parse the JSON output
	var output StructuredDiffOutput

	err = json.Unmarshal(buf.Bytes(), &output)
	if err != nil {
		t.Fatalf("Failed to parse JSON output: %v", err)
	}

	// Equal diffs should be excluded from changes
	if len(output.Changes) != 0 {
		t.Errorf("Expected 0 changes (equal diffs should be excluded), got %d", len(output.Changes))
	}
}
