package diffprocessor

import (
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
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
			source: tu.NewResource("v1", "Resource", "source").
				WithLabels(map[string]string{
					LabelComposite: "root-xr",
				}).
				Build(),
			target: tu.NewResource("v1", "Resource", "target").Build(),
			keys:   []string{LabelComposite},
			expectedLabels: map[string]string{
				LabelComposite: "root-xr",
			},
		},
		{
			name: "CopiesMultipleLabels",
			source: tu.NewResource("v1", "Resource", "source").
				WithLabels(map[string]string{
					LabelComposite:      "root-xr",
					LabelClaimName:      "my-claim",
					LabelClaimNamespace: "default",
				}).
				Build(),
			target: tu.NewResource("v1", "Resource", "target").Build(),
			keys:   []string{LabelComposite, LabelClaimName, LabelClaimNamespace},
			expectedLabels: map[string]string{
				LabelComposite:      "root-xr",
				LabelClaimName:      "my-claim",
				LabelClaimNamespace: "default",
			},
		},
		{
			name: "PreservesExistingTargetLabels",
			source: tu.NewResource("v1", "Resource", "source").
				WithLabels(map[string]string{
					LabelComposite: "root-xr",
				}).
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithLabels(map[string]string{
					"existing-label": "existing-value",
				}).
				Build(),
			keys: []string{LabelComposite},
			expectedLabels: map[string]string{
				LabelComposite:   "root-xr",
				"existing-label": "existing-value",
			},
		},
		{
			name: "OverwritesTargetLabelWithSourceValue",
			source: tu.NewResource("v1", "Resource", "source").
				WithLabels(map[string]string{
					LabelComposite: "correct-root-xr",
				}).
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithLabels(map[string]string{
					LabelComposite: "wrong-root-xr",
				}).
				Build(),
			keys: []string{LabelComposite},
			expectedLabels: map[string]string{
				LabelComposite: "correct-root-xr",
			},
		},
		{
			name:   "NoOpWhenSourceHasNoLabels",
			source: tu.NewResource("v1", "Resource", "source").Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithLabels(map[string]string{
					"existing-label": "existing-value",
				}).
				Build(),
			keys: []string{LabelComposite},
			expectedLabels: map[string]string{
				"existing-label": "existing-value",
			},
		},
		{
			name: "NoOpWhenSourceLabelsNil",
			// Use raw construction to test truly nil metadata
			source: &un.Unstructured{
				Object: map[string]any{},
			},
			target: tu.NewResource("v1", "Resource", "target").
				WithLabels(map[string]string{
					"existing-label": "existing-value",
				}).
				Build(),
			keys: []string{LabelComposite},
			expectedLabels: map[string]string{
				"existing-label": "existing-value",
			},
		},
		{
			name: "SkipsKeysNotInSource",
			source: tu.NewResource("v1", "Resource", "source").
				WithLabels(map[string]string{
					LabelComposite: "root-xr",
				}).
				Build(),
			target: tu.NewResource("v1", "Resource", "target").Build(),
			keys:   []string{LabelComposite, LabelClaimName, LabelClaimNamespace},
			expectedLabels: map[string]string{
				LabelComposite: "root-xr",
			},
		},
		{
			name: "CreatesLabelsMapWhenTargetHasNone",
			source: tu.NewResource("v1", "Resource", "source").
				WithLabels(map[string]string{
					LabelComposite: "root-xr",
				}).
				Build(),
			// Use raw construction to test target with no labels map
			target: &un.Unstructured{
				Object: map[string]any{},
			},
			keys: []string{LabelComposite},
			expectedLabels: map[string]string{
				LabelComposite: "root-xr",
			},
		},
		{
			name: "NoOpWhenNoKeysSpecified",
			source: tu.NewResource("v1", "Resource", "source").
				WithLabels(map[string]string{
					LabelComposite: "root-xr",
				}).
				Build(),
			target:         tu.NewResource("v1", "Resource", "target").Build(),
			keys:           []string{},
			expectedLabels: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			CopyLabels(tt.source, tt.target, tt.keys...)

			actualLabels := tt.target.GetLabels()

			if diff := cmp.Diff(tt.expectedLabels, actualLabels); diff != "" {
				t.Errorf("Labels mismatch (-want +got):\n%s", diff)
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
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField(map[string]any{"name": "my-composition"}, "spec", "compositionRef").
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithSpec(map[string]any{}).
				Build(),
			expectedRef: map[string]any{
				"name": "my-composition",
			},
			path: []string{"spec", "compositionRef"},
		},
		{
			name: "CopiesV2CompositionRef",
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField(map[string]any{"name": "my-composition"}, "spec", "crossplane", "compositionRef").
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithSpec(map[string]any{}).
				Build(),
			expectedRef: map[string]any{
				"name": "my-composition",
			},
			path: []string{"spec", "crossplane", "compositionRef"},
		},
		{
			name: "V1TakesPrecedenceOverV2",
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField(map[string]any{"name": "v1-composition"}, "spec", "compositionRef").
				WithNestedField(map[string]any{"name": "v2-composition"}, "spec", "crossplane", "compositionRef").
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithSpec(map[string]any{}).
				Build(),
			expectedRef: map[string]any{
				"name": "v1-composition",
			},
			path: []string{"spec", "compositionRef"},
		},
		{
			name: "NoOpWhenSourceHasNoCompositionRef",
			source: tu.NewResource("v1", "Resource", "source").
				WithSpecField("someField", "someValue").
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithSpec(map[string]any{}).
				Build(),
			expectedRef: nil,
			path:        []string{"spec", "compositionRef"},
		},
		{
			name: "NoOpWhenSourceHasEmptySpec",
			// Use raw construction to test truly empty object
			source: &un.Unstructured{
				Object: map[string]any{},
			},
			target: tu.NewResource("v1", "Resource", "target").
				WithSpec(map[string]any{}).
				Build(),
			expectedRef: nil,
			path:        []string{"spec", "compositionRef"},
		},
		{
			name: "PreservesExistingTargetSpecFields",
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField(map[string]any{"name": "my-composition"}, "spec", "compositionRef").
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithSpecField("coolField", "cool-value").
				Build(),
			expectedRef: map[string]any{
				"name": "my-composition",
			},
			path: []string{"spec", "compositionRef"},
		},
		{
			name: "V2CreatesSpecCrossplaneIfNotExists",
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField(map[string]any{"name": "my-composition"}, "spec", "crossplane", "compositionRef").
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithSpecField("coolField", "cool-value").
				Build(),
			expectedRef: map[string]any{
				"name": "my-composition",
			},
			path: []string{"spec", "crossplane", "compositionRef"},
		},
		{
			name: "V2PreservesExistingCrossplaneFields",
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField(map[string]any{"name": "my-composition"}, "spec", "crossplane", "compositionRef").
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				Build(),
			expectedRef: map[string]any{
				"name": "my-composition",
			},
			path: []string{"spec", "crossplane", "compositionRef"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			CopyCompositionRef(tt.source, tt.target)

			actualRef, _, _ := un.NestedMap(tt.target.Object, tt.path...)

			if diff := cmp.Diff(tt.expectedRef, actualRef); diff != "" {
				t.Errorf("CompositionRef mismatch at %v (-want +got):\n%s", tt.path, diff)
			}
		})
	}
}

// TestSetCompositionRevisionRefName covers the writer comp uses to seed a composite with the revision
// a diffed composition would produce. The load-bearing behaviour is the refusal: it must never create a
// ref that isn't already there, because an existing ref is the only evidence the composite's schema
// accepts that path, and the only case where there is a stale value to correct.
func TestSetCompositionRevisionRefName(t *testing.T) {
	tests := []struct {
		name string
		// target is the composite to seed.
		target *un.Unstructured
		// wantSet is whether a ref was written.
		wantSet bool
		// wantSpec is the whole spec afterwards, so an unintended write anywhere else is caught too.
		wantSpec map[string]any
	}{
		{
			name: "V2PathIsOverwrittenInPlace",
			target: tu.NewResource("v1", "Resource", "target").
				WithNestedField(map[string]any{"name": "old-revision"}, "spec", "crossplane", "compositionRevisionRef").
				Build(),
			wantSet: true,
			wantSpec: map[string]any{
				"crossplane": map[string]any{"compositionRevisionRef": map[string]any{"name": "new-revision"}},
			},
		},
		{
			name: "V1PathIsOverwrittenInPlace",
			target: tu.NewResource("v1", "Resource", "target").
				WithNestedField(map[string]any{"name": "old-revision"}, "spec", "compositionRevisionRef").
				Build(),
			wantSet:  true,
			wantSpec: map[string]any{"compositionRevisionRef": map[string]any{"name": "new-revision"}},
		},
		{
			name: "V2WinsWhenBothPathsArePresent",
			// Pathological, but if it happens the value we overwrite must be the one the composition
			// client would read, and nestedCrossplaneString reads v2 first.
			target: tu.NewResource("v1", "Resource", "target").
				WithNestedField(map[string]any{"name": "old-v1"}, "spec", "compositionRevisionRef").
				WithNestedField(map[string]any{"name": "old-v2"}, "spec", "crossplane", "compositionRevisionRef").
				Build(),
			wantSet: true,
			wantSpec: map[string]any{
				"compositionRevisionRef": map[string]any{"name": "old-v1"},
				"crossplane":             map[string]any{"compositionRevisionRef": map[string]any{"name": "new-revision"}},
			},
		},
		{
			name: "NoRefIsNotCreated",
			// A composite not yet tracking a revision. Writing either path would be a guess at the
			// schema, so it renders unseeded instead.
			target: tu.NewResource("v1", "Resource", "target").
				WithNestedField(map[string]any{"name": "my-composition"}, "spec", "crossplane", "compositionRef").
				Build(),
			wantSet: false,
			wantSpec: map[string]any{
				"crossplane": map[string]any{"compositionRef": map[string]any{"name": "my-composition"}},
			},
		},
		{
			name:     "NoSpecAtAllIsNotCreated",
			target:   tu.NewResource("v1", "Resource", "target").Build(),
			wantSet:  false,
			wantSpec: nil,
		},
		{
			name: "SiblingCrossplaneFieldsAreUntouched",
			target: tu.NewResource("v1", "Resource", "target").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				WithNestedField(map[string]any{"name": "my-composition"}, "spec", "crossplane", "compositionRef").
				WithNestedField(map[string]any{"name": "old-revision"}, "spec", "crossplane", "compositionRevisionRef").
				Build(),
			wantSet: true,
			wantSpec: map[string]any{
				"crossplane": map[string]any{
					"compositionUpdatePolicy": "Automatic",
					"compositionRef":          map[string]any{"name": "my-composition"},
					"compositionRevisionRef":  map[string]any{"name": "new-revision"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SetCompositionRevisionRefName(tt.target, "new-revision"); got != tt.wantSet {
				t.Errorf("SetCompositionRevisionRefName() = %t, want %t", got, tt.wantSet)
			}

			spec, _, _ := un.NestedMap(tt.target.Object, "spec")
			if diff := cmp.Diff(tt.wantSpec, spec); diff != "" {
				t.Errorf("spec mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCopyCompositionRevisionRef covers how a rendered composite inherits the cluster's
// compositionRevisionRef. Crossplane's composite reconciler writes that field and an apply that omits
// it leaves it in place, so a composite whose input says nothing about it must render with the
// cluster's value. The load-bearing behaviour is the refusal: a ref already on the target — the user's
// own pin, or a value the caller seeded — always wins.
func TestCopyCompositionRevisionRef(t *testing.T) {
	tests := []struct {
		name   string
		source *un.Unstructured
		target *un.Unstructured
		// wantCopied is whether a ref was copied.
		wantCopied bool
		// wantSpec is the target's whole spec afterwards, so an unintended write anywhere else is caught.
		wantSpec map[string]any
	}{
		{
			name: "V2RefIsCopiedToV2Path",
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField(map[string]any{"name": "cluster-revision"}, "spec", "crossplane", "compositionRevisionRef").
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
				Build(),
			wantCopied: true,
			wantSpec: map[string]any{
				"crossplane": map[string]any{
					"compositionUpdatePolicy": "Manual",
					"compositionRevisionRef":  map[string]any{"name": "cluster-revision"},
				},
			},
		},
		{
			name: "V1RefIsCopiedToV1Path",
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField(map[string]any{"name": "cluster-revision"}, "spec", "compositionRevisionRef").
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithSpecField("coolField", "cool-value").
				Build(),
			wantCopied: true,
			wantSpec: map[string]any{
				"coolField":              "cool-value",
				"compositionRevisionRef": map[string]any{"name": "cluster-revision"},
			},
		},
		{
			name: "BothPathsAreCopiedWhenTheSourceCarriesBoth",
			// Pathological, but the render must then see what the cluster object holds at whichever path
			// the composition client reads.
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField(map[string]any{"name": "cluster-v1"}, "spec", "compositionRevisionRef").
				WithNestedField(map[string]any{"name": "cluster-v2"}, "spec", "crossplane", "compositionRevisionRef").
				Build(),
			target:     tu.NewResource("v1", "Resource", "target").Build(),
			wantCopied: true,
			wantSpec: map[string]any{
				"compositionRevisionRef": map[string]any{"name": "cluster-v1"},
				"crossplane":             map[string]any{"compositionRevisionRef": map[string]any{"name": "cluster-v2"}},
			},
		},
		{
			name: "TargetRefAtTheSamePathWins",
			// The user's own pin: pointing a Manual composite at a different revision must keep working.
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField(map[string]any{"name": "cluster-revision"}, "spec", "crossplane", "compositionRevisionRef").
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithNestedField(map[string]any{"name": "user-revision"}, "spec", "crossplane", "compositionRevisionRef").
				Build(),
			wantCopied: false,
			wantSpec: map[string]any{
				"crossplane": map[string]any{"compositionRevisionRef": map[string]any{"name": "user-revision"}},
			},
		},
		{
			name: "TargetRefAtTheOtherPathWins",
			// A ref at either path is an opinion about the field, so the cluster's is not layered on top.
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField(map[string]any{"name": "cluster-revision"}, "spec", "crossplane", "compositionRevisionRef").
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithNestedField(map[string]any{"name": "user-revision"}, "spec", "compositionRevisionRef").
				Build(),
			wantCopied: false,
			wantSpec:   map[string]any{"compositionRevisionRef": map[string]any{"name": "user-revision"}},
		},
		{
			name: "NoOpWhenSourceHasNoRef",
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField(map[string]any{"name": "my-composition"}, "spec", "crossplane", "compositionRef").
				Build(),
			target: tu.NewResource("v1", "Resource", "target").
				WithSpecField("coolField", "cool-value").
				Build(),
			wantCopied: false,
			wantSpec:   map[string]any{"coolField": "cool-value"},
		},
		{
			name: "MalformedSourceRefIsNotCopied",
			// Not an object, so not something Crossplane wrote; the composition client rejects it on read.
			source: tu.NewResource("v1", "Resource", "source").
				WithNestedField("not-an-object", "spec", "crossplane", "compositionRevisionRef").
				Build(),
			target:     tu.NewResource("v1", "Resource", "target").Build(),
			wantCopied: false,
			wantSpec:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sourceBefore := tt.source.DeepCopy()

			if got := CopyCompositionRevisionRef(tt.source, tt.target); got != tt.wantCopied {
				t.Errorf("CopyCompositionRevisionRef() = %t, want %t", got, tt.wantCopied)
			}

			spec, _, _ := un.NestedMap(tt.target.Object, "spec")
			if diff := cmp.Diff(tt.wantSpec, spec); diff != "" {
				t.Errorf("target spec mismatch (-want +got):\n%s", diff)
			}

			// The copy must be deep: the source is the cluster's object, and later writes to the render
			// input (comp seeding a predicted name, say) must not reach it.
			if spec != nil {
				_ = SetCompositionRevisionRefName(tt.target, "mutated")
			}

			if diff := cmp.Diff(sourceBefore.Object, tt.source.Object); diff != "" {
				t.Errorf("source was mutated (-before +after):\n%s", diff)
			}
		})
	}
}

func TestCopyCompositionRef_V2PreservesOtherCrossplaneFields(t *testing.T) {
	source := tu.NewResource("v1", "Resource", "source").
		WithNestedField(map[string]any{"name": "my-composition"}, "spec", "crossplane", "compositionRef").
		Build()

	target := tu.NewResource("v1", "Resource", "target").
		WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
		WithNestedField(map[string]any{"name": "my-revision"}, "spec", "crossplane", "compositionRevisionRef").
		Build()

	CopyCompositionRef(source, target)

	// Verify the entire crossplane section
	crossplane, found, _ := un.NestedMap(target.Object, "spec", "crossplane")
	if !found {
		t.Fatal("Expected spec.crossplane to exist")
	}

	expected := map[string]any{
		"compositionRef":          map[string]any{"name": "my-composition"},
		"compositionUpdatePolicy": "Manual",
		"compositionRevisionRef":  map[string]any{"name": "my-revision"},
	}

	if diff := cmp.Diff(expected, crossplane); diff != "" {
		t.Errorf("spec.crossplane mismatch (-want +got):\n%s", diff)
	}
}
