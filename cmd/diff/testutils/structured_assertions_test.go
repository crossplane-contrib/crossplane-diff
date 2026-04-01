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

func TestAssertStructuredDiff_Summary(_ *testing.T) {
	jsonOutput := `{
		"summary": {"added": 1, "modified": 2, "removed": 3},
		"changes": []
	}`

	// This should pass - matching summary
	mockT := &testing.T{}
	AssertStructuredDiff(mockT, jsonOutput, ExpectDiff().WithSummary(1, 2, 3))
}

func TestAssertStructuredDiff_AddedResource(_ *testing.T) {
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

func TestAssertStructuredDiff_ModifiedResource(_ *testing.T) {
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

func TestAssertStructuredDiff_RemovedResource(_ *testing.T) {
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

func TestAssertStructuredDiff_NamePattern(_ *testing.T) {
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

func TestGetNestedFieldWithArrayIndex(t *testing.T) {
	// Simulate a composition structure with pipeline steps
	obj := map[string]any{
		"spec": map[string]any{
			"pipeline": []any{
				map[string]any{
					"functionRef": map[string]any{
						"name": "function-patch-and-transform",
					},
					"input": map[string]any{
						"resources": []any{
							map[string]any{
								"name": "nop-resource",
								"patches": []any{
									map[string]any{
										"fromFieldPath": "spec.coolField",
										"transforms": []any{
											map[string]any{
												"string": map[string]any{
													"fmt": "updated-%s",
												},
											},
										},
									},
									map[string]any{
										"fromFieldPath": "spec.otherField",
										"transforms": []any{
											map[string]any{
												"string": map[string]any{
													"fmt": "basic-%s",
												},
											},
										},
									},
								},
							},
						},
					},
					"step": "generate-resources",
				},
				map[string]any{
					"functionRef": map[string]any{
						"name": "function-auto-ready",
					},
					"step": "detect-ready-resources",
				},
			},
		},
	}

	tests := []struct {
		name string
		path string
		want any
	}{
		{
			name: "first pipeline step function name",
			path: "spec.pipeline[0].functionRef.name",
			want: "function-patch-and-transform",
		},
		{
			name: "second pipeline step function name",
			path: "spec.pipeline[1].functionRef.name",
			want: "function-auto-ready",
		},
		{
			name: "first resource name",
			path: "spec.pipeline[0].input.resources[0].name",
			want: "nop-resource",
		},
		{
			name: "first patch transform fmt",
			path: "spec.pipeline[0].input.resources[0].patches[0].transforms[0].string.fmt",
			want: "updated-%s",
		},
		{
			name: "second patch transform fmt",
			path: "spec.pipeline[0].input.resources[0].patches[1].transforms[0].string.fmt",
			want: "basic-%s",
		},
		{
			name: "out of bounds index",
			path: "spec.pipeline[5].functionRef.name",
			want: nil,
		},
		{
			name: "negative index (last element)",
			path: "spec.pipeline[-1].functionRef.name",
			want: "function-auto-ready", // k8s jsonpath supports Python-style negative indices
		},
		{
			name: "non-array with index",
			path: "spec.pipeline[0].functionRef[0]",
			want: nil,
		},
		{
			name: "step field",
			path: "spec.pipeline[0].step",
			want: "generate-resources",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getNestedField(obj, tt.path)
			if !valuesEqual(got, tt.want) {
				t.Errorf("getNestedField(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestGetNestedFieldWithBracketNotation(t *testing.T) {
	// Test data with keys containing dots (like Kubernetes annotations)
	obj := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				"getcomposed.example.org/source-bucket":   "bucket-name-abc123",
				"crossplane.io/composition-resource-name": "test-resource",
				"simple-key": "simple-value",
			},
		},
	}

	tests := []struct {
		name string
		path string
		want any
	}{
		{
			name: "bracket notation with single quotes",
			path: "metadata.annotations['getcomposed.example.org/source-bucket']",
			want: "bucket-name-abc123",
		},
		{
			name: "bracket notation with double quotes",
			path: `metadata.annotations["crossplane.io/composition-resource-name"]`,
			want: "test-resource",
		},
		{
			name: "simple key without dots",
			path: "metadata.annotations['simple-key']",
			want: "simple-value",
		},
		{
			name: "simple dot notation still works",
			path: "metadata.annotations.simple-key",
			want: "simple-value",
		},
		{
			name: "nonexistent key with brackets",
			path: "metadata.annotations['nonexistent.key']",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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

func TestAssertStructuredCompDiff_DownstreamFieldChanges(_ *testing.T) {
	jsonOutput := `{
		"compositions": [{
			"name": "test-composition",
			"affectedResources": {
				"total": 1,
				"withChanges": 1,
				"unchanged": 0,
				"withErrors": 0
			},
			"impactAnalysis": [{
				"apiVersion": "test.example.org/v1alpha1",
				"kind": "XTestResource",
				"name": "test-xr",
				"status": "changed",
				"downstreamChanges": {
					"summary": {"added": 0, "modified": 1, "removed": 0},
					"changes": [{
						"type": "modified",
						"apiVersion": "nop.crossplane.io/v1alpha1",
						"kind": "ClusterNopResource",
						"name": "test-xr-abc123",
						"diff": {
							"old": {
								"metadata": {
									"annotations": {
										"config-data": "old-value",
										"resource-tier": "basic"
									}
								}
							},
							"new": {
								"metadata": {
									"annotations": {
										"config-data": "new-value",
										"resource-tier": "premium"
									}
								}
							}
						}
					}]
				}
			}]
		}]
	}`

	mockT := &testing.T{}
	AssertStructuredCompDiff(mockT, jsonOutput,
		ExpectCompDiff().
			WithComposition("test-composition").
			WithAffectedResources(1, 1, 0, 0).
			WithXRImpact("XTestResource", "test-xr", "", "changed").
			WithDownstreamSummary(0, 1, 0).
			WithDownstreamResource("modified", "ClusterNopResource", "", "").
			WithAnyName().
			WithFieldChange("metadata.annotations.config-data", "old-value", "new-value").
			WithFieldChange("metadata.annotations.resource-tier", "basic", "premium").
			AndXR().
			AndComp().
			And())
}

func TestFindMatchingDownstreamChange(t *testing.T) {
	changes := []ChangeDetail{
		{
			Type:      "modified",
			Kind:      "ClusterNopResource",
			Name:      "test-resource-abc123",
			Namespace: "",
		},
		{
			Type:      "added",
			Kind:      "ConfigMap",
			Name:      "test-config",
			Namespace: "default",
		},
	}

	tests := []struct {
		name   string
		expect *DownstreamResourceExpectation
		want   bool
	}{
		{
			name: "exact name match",
			expect: &DownstreamResourceExpectation{
				changeType: "modified",
				kind:       "ClusterNopResource",
				name:       "test-resource-abc123",
			},
			want: true,
		},
		{
			name: "any name match",
			expect: &DownstreamResourceExpectation{
				changeType:     "modified",
				kind:           "ClusterNopResource",
				anyNameAllowed: true,
			},
			want: true,
		},
		{
			name: "name pattern match",
			expect: func() *DownstreamResourceExpectation {
				d := &DownstreamResourceExpectation{
					changeType: "modified",
					kind:       "ClusterNopResource",
				}
				d.WithNamePattern(`test-resource-[a-z0-9]+`)

				return d
			}(),
			want: true,
		},
		{
			name: "kind mismatch",
			expect: &DownstreamResourceExpectation{
				changeType: "modified",
				kind:       "WrongKind",
				name:       "test-resource-abc123",
			},
			want: false,
		},
		{
			name: "change type mismatch",
			expect: &DownstreamResourceExpectation{
				changeType: "added",
				kind:       "ClusterNopResource",
				name:       "test-resource-abc123",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found := findMatchingDownstreamChange(changes, tt.expect)
			if (found != nil) != tt.want {
				t.Errorf("findMatchingDownstreamChange() found = %v, want found = %v", found != nil, tt.want)
			}
		})
	}
}

func TestAssertStructuredCompDiff_FieldValuePattern(_ *testing.T) {
	jsonOutput := `{
		"compositions": [{
			"name": "test-composition",
			"affectedResources": {
				"total": 1,
				"withChanges": 1,
				"unchanged": 0,
				"withErrors": 0
			},
			"impactAnalysis": [{
				"apiVersion": "test.example.org/v1alpha1",
				"kind": "XTestResource",
				"name": "test-xr",
				"status": "changed",
				"downstreamChanges": {
					"summary": {"added": 0, "modified": 1, "removed": 0},
					"changes": [{
						"type": "modified",
						"apiVersion": "nop.crossplane.io/v1alpha1",
						"kind": "ClusterNopResource",
						"name": "test-xr-abc123",
						"diff": {
							"old": {
								"metadata": {
									"annotations": {
										"existing-annotation": "value"
									}
								}
							},
							"new": {
								"metadata": {
									"annotations": {
										"existing-annotation": "value",
										"generated-ref": "test-xr-xyz789"
									}
								}
							}
						}
					}]
				}
			}]
		}]
	}`

	mockT := &testing.T{}
	AssertStructuredCompDiff(mockT, jsonOutput,
		ExpectCompDiff().
			WithComposition("test-composition").
			WithAffectedResources(1, 1, 0, 0).
			WithXRImpact("XTestResource", "test-xr", "", "changed").
			WithDownstreamSummary(0, 1, 0).
			WithDownstreamResource("modified", "ClusterNopResource", "", "").
			WithAnyName().
			WithFieldValuePattern("metadata.annotations.generated-ref", `test-xr-[a-z0-9]+`).
			AndXR().
			AndComp().
			And())
}
