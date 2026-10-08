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
