package testutils

import (
	"testing"
)

func TestParseStructuredOutput(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantErr   bool
		wantAdded int
	}{
		{
			name: "valid JSON with summary",
			input: `{
				"summary": {"added": 1, "modified": 2, "removed": 0},
				"changes": []
			}`,
			wantErr:   false,
			wantAdded: 1,
		},
		{
			name:    "invalid JSON",
			input:   `{invalid}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := ParseStructuredOutput(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseStructuredOutput() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && output.Summary.Added != tt.wantAdded {
				t.Errorf("Summary.Added = %d, want %d", output.Summary.Added, tt.wantAdded)
			}
		})
	}
}

func TestAssertStructuredDiff_Summary(t *testing.T) {
	jsonOutput := `{
		"summary": {"added": 1, "modified": 2, "removed": 3},
		"changes": []
	}`

	// This should pass - matching summary
	mockT := &testing.T{}
	AssertStructuredDiff(mockT, jsonOutput, ExpectDiff().WithSummary(1, 2, 3))
}

func TestAssertStructuredDiff_AddedResource(t *testing.T) {
	jsonOutput := `{
		"summary": {"added": 1, "modified": 0, "removed": 0},
		"changes": [{
			"type": "added",
			"apiVersion": "v1alpha1",
			"kind": "XNopResource",
			"name": "test-resource",
			"namespace": "default",
			"diff": {
				"spec": {
					"spec": {
						"forProvider": {
							"configData": "new-value"
						}
					}
				}
			}
		}]
	}`

	mockT := &testing.T{}
	AssertStructuredDiff(mockT, jsonOutput,
		ExpectDiff().
			WithSummary(1, 0, 0).
			WithAddedResource("XNopResource", "test-resource", "default").
			WithField("spec.forProvider.configData", "new-value").
			And())
}

func TestAssertStructuredDiff_ModifiedResource(t *testing.T) {
	jsonOutput := `{
		"summary": {"added": 0, "modified": 1, "removed": 0},
		"changes": [{
			"type": "modified",
			"apiVersion": "v1alpha1",
			"kind": "XNopResource",
			"name": "test-resource",
			"namespace": "default",
			"diff": {
				"old": {"spec": {"forProvider": {"configData": "old-value"}}},
				"new": {"spec": {"forProvider": {"configData": "new-value"}}}
			}
		}]
	}`

	mockT := &testing.T{}
	AssertStructuredDiff(mockT, jsonOutput,
		ExpectDiff().
			WithSummary(0, 1, 0).
			WithModifiedResource("XNopResource", "test-resource", "default").
			WithFieldChange("spec.forProvider.configData", "old-value", "new-value").
			And())
}

func TestAssertStructuredDiff_RemovedResource(t *testing.T) {
	jsonOutput := `{
		"summary": {"added": 0, "modified": 0, "removed": 1},
		"changes": [{
			"type": "removed",
			"apiVersion": "v1alpha1",
			"kind": "XNopResource",
			"name": "removed-resource",
			"namespace": "default",
			"diff": {
				"spec": {
					"spec": {
						"forProvider": {
							"configData": "deleted-value"
						}
					}
				}
			}
		}]
	}`

	mockT := &testing.T{}
	AssertStructuredDiff(mockT, jsonOutput,
		ExpectDiff().
			WithSummary(0, 0, 1).
			WithRemovedResource("XNopResource", "removed-resource", "default").
			WithField("spec.forProvider.configData", "deleted-value").
			And())
}

func TestAssertStructuredDiff_NamePattern(t *testing.T) {
	jsonOutput := `{
		"summary": {"added": 1, "modified": 0, "removed": 0},
		"changes": [{
			"type": "added",
			"apiVersion": "v1alpha1",
			"kind": "XNopResource",
			"name": "test-resource-abc123",
			"namespace": "default",
			"diff": {"spec": {}}
		}]
	}`

	mockT := &testing.T{}
	AssertStructuredDiff(mockT, jsonOutput,
		ExpectDiff().
			WithSummary(1, 0, 0).
			WithAddedResource("XNopResource", "", "default").
			WithNamePattern(`test-resource-[a-z0-9]+`).
			And())
}

func TestGetNestedField(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"forProvider": map[string]any{
				"configData": "test-value",
			},
		},
	}

	tests := []struct {
		path string
		want any
	}{
		{"spec.forProvider.configData", "test-value"},
		{"spec.forProvider", map[string]any{"configData": "test-value"}},
		{"spec.nonexistent", nil},
		{"", obj},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := getNestedField(obj, tt.path)
			if !valuesEqual(got, tt.want) {
				t.Errorf("getNestedField(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestValuesEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b any
		want bool
	}{
		{"nil == nil", nil, nil, true},
		{"string match", "foo", "foo", true},
		{"string mismatch", "foo", "bar", false},
		{"int == float64", 42, float64(42), true},
		{"float64 == int", float64(42), 42, true},
		{"bool match", true, true, true},
		{"bool mismatch", true, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := valuesEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("valuesEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
