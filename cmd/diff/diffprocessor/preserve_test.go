package diffprocessor

import (
	"testing"

	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCopyLabels(t *testing.T) {
	tests := []struct {
		name           string
		source         *un.Unstructured
		target         *un.Unstructured
		keys           []string
		expectedLabels map[string]string
	}{
		{
			name: "CopiesSingleLabel",
			source: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{
							"crossplane.io/composite": "root-xr",
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{},
				},
			},
			keys: []string{LabelComposite},
			expectedLabels: map[string]string{
				"crossplane.io/composite": "root-xr",
			},
		},
		{
			name: "CopiesMultipleLabels",
			source: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{
							"crossplane.io/composite":       "root-xr",
							"crossplane.io/claim-name":      "my-claim",
							"crossplane.io/claim-namespace": "default",
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{},
				},
			},
			keys: []string{LabelComposite, LabelClaimName, LabelClaimNamespace},
			expectedLabels: map[string]string{
				"crossplane.io/composite":       "root-xr",
				"crossplane.io/claim-name":      "my-claim",
				"crossplane.io/claim-namespace": "default",
			},
		},
		{
			name: "PreservesExistingTargetLabels",
			source: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{
							"crossplane.io/composite": "root-xr",
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{
							"existing-label": "existing-value",
						},
					},
				},
			},
			keys: []string{LabelComposite},
			expectedLabels: map[string]string{
				"crossplane.io/composite": "root-xr",
				"existing-label":          "existing-value",
			},
		},
		{
			name: "OverwritesTargetLabelWithSourceValue",
			source: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{
							"crossplane.io/composite": "correct-root-xr",
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{
							"crossplane.io/composite": "wrong-root-xr",
						},
					},
				},
			},
			keys: []string{LabelComposite},
			expectedLabels: map[string]string{
				"crossplane.io/composite": "correct-root-xr",
			},
		},
		{
			name: "NoOpWhenSourceHasNoLabels",
			source: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{
							"existing-label": "existing-value",
						},
					},
				},
			},
			keys: []string{LabelComposite},
			expectedLabels: map[string]string{
				"existing-label": "existing-value",
			},
		},
		{
			name: "NoOpWhenSourceLabelsNil",
			source: &un.Unstructured{
				Object: map[string]any{},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{
							"existing-label": "existing-value",
						},
					},
				},
			},
			keys: []string{LabelComposite},
			expectedLabels: map[string]string{
				"existing-label": "existing-value",
			},
		},
		{
			name: "SkipsKeysNotInSource",
			source: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{
							"crossplane.io/composite": "root-xr",
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{},
				},
			},
			keys: []string{LabelComposite, LabelClaimName, LabelClaimNamespace},
			expectedLabels: map[string]string{
				"crossplane.io/composite": "root-xr",
			},
		},
		{
			name: "CreatesLabelsMapWhenTargetHasNone",
			source: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{
							"crossplane.io/composite": "root-xr",
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{},
			},
			keys: []string{LabelComposite},
			expectedLabels: map[string]string{
				"crossplane.io/composite": "root-xr",
			},
		},
		{
			name: "NoOpWhenNoKeysSpecified",
			source: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"labels": map[string]any{
							"crossplane.io/composite": "root-xr",
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{},
				},
			},
			keys:           []string{},
			expectedLabels: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			CopyLabels(tt.source, tt.target, tt.keys...)

			actualLabels := tt.target.GetLabels()

			if tt.expectedLabels == nil {
				if len(actualLabels) > 0 {
					t.Errorf("Expected no labels, got %v", actualLabels)
				}

				return
			}

			if len(actualLabels) != len(tt.expectedLabels) {
				t.Errorf("Expected %d labels, got %d: %v", len(tt.expectedLabels), len(actualLabels), actualLabels)
				return
			}

			for key, expectedValue := range tt.expectedLabels {
				actualValue, exists := actualLabels[key]
				if !exists {
					t.Errorf("Expected label %s to exist", key)
					continue
				}

				if actualValue != expectedValue {
					t.Errorf("Expected label %s=%s, got %s", key, expectedValue, actualValue)
				}
			}
		})
	}
}

func TestCopyCompositionRef(t *testing.T) {
	tests := []struct {
		name        string
		source      *un.Unstructured
		target      *un.Unstructured
		expectedRef map[string]any
		path        []string // path to check for compositionRef
	}{
		{
			name: "CopiesV1CompositionRef",
			source: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{
						"compositionRef": map[string]any{
							"name": "my-composition",
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{},
				},
			},
			expectedRef: map[string]any{
				"name": "my-composition",
			},
			path: []string{"spec", "compositionRef"},
		},
		{
			name: "CopiesV2CompositionRef",
			source: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{
						"crossplane": map[string]any{
							"compositionRef": map[string]any{
								"name": "my-composition",
							},
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{},
				},
			},
			expectedRef: map[string]any{
				"name": "my-composition",
			},
			path: []string{"spec", "crossplane", "compositionRef"},
		},
		{
			name: "V1TakesPrecedenceOverV2",
			source: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{
						"compositionRef": map[string]any{
							"name": "v1-composition",
						},
						"crossplane": map[string]any{
							"compositionRef": map[string]any{
								"name": "v2-composition",
							},
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{},
				},
			},
			expectedRef: map[string]any{
				"name": "v1-composition",
			},
			path: []string{"spec", "compositionRef"},
		},
		{
			name: "NoOpWhenSourceHasNoCompositionRef",
			source: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{
						"someField": "someValue",
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{},
				},
			},
			expectedRef: nil,
			path:        []string{"spec", "compositionRef"},
		},
		{
			name: "NoOpWhenSourceHasEmptySpec",
			source: &un.Unstructured{
				Object: map[string]any{},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{},
				},
			},
			expectedRef: nil,
			path:        []string{"spec", "compositionRef"},
		},
		{
			name: "PreservesExistingTargetSpecFields",
			source: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{
						"compositionRef": map[string]any{
							"name": "my-composition",
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{
						"coolField": "cool-value",
					},
				},
			},
			expectedRef: map[string]any{
				"name": "my-composition",
			},
			path: []string{"spec", "compositionRef"},
		},
		{
			name: "V2CreatesSpecCrossplaneIfNotExists",
			source: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{
						"crossplane": map[string]any{
							"compositionRef": map[string]any{
								"name": "my-composition",
							},
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{
						"coolField": "cool-value",
					},
				},
			},
			expectedRef: map[string]any{
				"name": "my-composition",
			},
			path: []string{"spec", "crossplane", "compositionRef"},
		},
		{
			name: "V2PreservesExistingCrossplaneFields",
			source: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{
						"crossplane": map[string]any{
							"compositionRef": map[string]any{
								"name": "my-composition",
							},
						},
					},
				},
			},
			target: &un.Unstructured{
				Object: map[string]any{
					"spec": map[string]any{
						"crossplane": map[string]any{
							"compositionUpdatePolicy": "Automatic",
						},
					},
				},
			},
			expectedRef: map[string]any{
				"name": "my-composition",
			},
			path: []string{"spec", "crossplane", "compositionRef"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			CopyCompositionRef(tt.source, tt.target)

			actualRef, found, _ := un.NestedMap(tt.target.Object, tt.path...)

			if tt.expectedRef == nil {
				if found && actualRef != nil {
					t.Errorf("Expected no compositionRef at %v, got %v", tt.path, actualRef)
				}

				return
			}

			if !found || actualRef == nil {
				t.Errorf("Expected compositionRef at %v, got nothing", tt.path)
				return
			}

			expectedName, _ := tt.expectedRef["name"].(string)
			actualName, _ := actualRef["name"].(string)

			if actualName != expectedName {
				t.Errorf("Expected compositionRef.name=%s, got %s", expectedName, actualName)
			}
		})
	}
}

func TestCopyCompositionRef_V2PreservesOtherCrossplaneFields(t *testing.T) {
	source := &un.Unstructured{
		Object: map[string]any{
			"spec": map[string]any{
				"crossplane": map[string]any{
					"compositionRef": map[string]any{
						"name": "my-composition",
					},
				},
			},
		},
	}

	target := &un.Unstructured{
		Object: map[string]any{
			"spec": map[string]any{
				"crossplane": map[string]any{
					"compositionUpdatePolicy": "Manual",
					"compositionRevisionRef": map[string]any{
						"name": "my-revision",
					},
				},
			},
		},
	}

	CopyCompositionRef(source, target)

	// Verify compositionRef was copied
	compRef, found, _ := un.NestedMap(target.Object, "spec", "crossplane", "compositionRef")
	if !found {
		t.Fatal("Expected compositionRef to be copied")
	}

	if compRef["name"] != "my-composition" {
		t.Errorf("Expected compositionRef.name=my-composition, got %v", compRef["name"])
	}

	// Verify other crossplane fields are preserved
	policy, found, _ := un.NestedString(target.Object, "spec", "crossplane", "compositionUpdatePolicy")
	if !found || policy != "Manual" {
		t.Errorf("Expected compositionUpdatePolicy=Manual to be preserved, got %v", policy)
	}

	revRef, found, _ := un.NestedMap(target.Object, "spec", "crossplane", "compositionRevisionRef")
	if !found {
		t.Error("Expected compositionRevisionRef to be preserved")
	}

	if revRef["name"] != "my-revision" {
		t.Errorf("Expected compositionRevisionRef.name=my-revision, got %v", revRef["name"])
	}
}
