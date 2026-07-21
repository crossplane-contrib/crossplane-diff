package crossplane

import (
	"strings"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestXRRevisionSelectorMatch_Matches verifies the boolean match decision made by
// XRRevisionSelectorMatch for an XR's compositionRevisionSelector against a target label set.
//
// Crossplane only consults compositionRevisionSelector under the Automatic update policy
// (internal/controller/apiextensions/composite/api.go). Under Manual policy the revision is
// pinned via compositionRevisionRef, so the selector must NOT restrict the match here.
func TestXRRevisionSelectorMatch_Matches(t *testing.T) {
	tests := map[string]struct {
		xr           *un.Unstructured
		targetLabels map[string]string
		want         bool
		wantErr      bool
	}{
		// AC1.1: no selector => matches (no restriction), regardless of policy.
		"NoSelector_Automatic_Matches": {
			xr: tu.NewResource("example.org/v1", "XResource", "no-selector").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				Build(),
			targetLabels: map[string]string{"version": "0.0.2"},
			want:         true,
		},
		"NoSelector_NoPolicy_Matches": {
			xr: tu.NewResource("example.org/v1", "XResource", "no-selector-no-policy").
				Build(),
			targetLabels: map[string]string{"version": "0.0.2"},
			want:         true,
		},
		// AC1.2: Automatic + matchLabels subset of composition labels => matches.
		"Automatic_MatchLabelsSubset_Matches": {
			xr: tu.NewResource("example.org/v1", "XResource", "match").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				WithCompositionRevisionSelector(
					CrossplaneAPIExtGroupV2,
					map[string]string{"version": "0.0.2"},
					nil,
				).
				Build(),
			targetLabels: map[string]string{"version": "0.0.2", "extra": "ignored"},
			want:         true,
		},
		// AC1.3: Automatic + matchLabels not satisfied => does not match.
		"Automatic_MatchLabelsMismatch_DoesNotMatch": {
			xr: tu.NewResource("example.org/v1", "XResource", "mismatch").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				WithCompositionRevisionSelector(
					CrossplaneAPIExtGroupV2,
					map[string]string{"version": "0.0.1"},
					nil,
				).
				Build(),
			targetLabels: map[string]string{"version": "0.0.2"},
			want:         false,
		},
		// AC1.4: matchExpressions honored (In / NotIn / Exists / DoesNotExist).
		"Automatic_MatchExpressionsIn_Matches": {
			xr: tu.NewResource("example.org/v1", "XResource", "expr-in").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				WithCompositionRevisionSelector(
					CrossplaneAPIExtGroupV2,
					nil,
					[]map[string]any{
						{"key": "channel", "operator": "In", "values": []any{"active", "beta"}},
					},
				).
				Build(),
			targetLabels: map[string]string{"channel": "active"},
			want:         true,
		},
		"Automatic_MatchExpressionsNotIn_DoesNotMatch": {
			xr: tu.NewResource("example.org/v1", "XResource", "expr-notin").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				WithCompositionRevisionSelector(
					CrossplaneAPIExtGroupV2,
					nil,
					[]map[string]any{
						{"key": "channel", "operator": "NotIn", "values": []any{"preview"}},
					},
				).
				Build(),
			targetLabels: map[string]string{"channel": "preview"},
			want:         false,
		},
		// AC1.5: v1 path is read as well.
		"Automatic_V1Path_MatchLabels_Matches": {
			xr: tu.NewResource("example.org/v1", "XResource", "v1-path").
				WithNestedField("Automatic", "spec", "compositionUpdatePolicy").
				WithCompositionRevisionSelector(
					CrossplaneAPIExtGroupV1,
					map[string]string{"version": "0.0.2"},
					nil,
				).
				Build(),
			targetLabels: map[string]string{"version": "0.0.2"},
			want:         true,
		},
		// AC1.6: Manual policy => selector not applied by this predicate (returns matches=true
		// so Manual XRs are governed only by the policy filter / --include-manual).
		"Manual_MismatchingSelector_StillMatches": {
			xr: tu.NewResource("example.org/v1", "XResource", "manual-mismatch").
				WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
				WithCompositionRevisionSelector(
					CrossplaneAPIExtGroupV2,
					map[string]string{"version": "0.0.1"},
					nil,
				).
				Build(),
			targetLabels: map[string]string{"version": "0.0.2"},
			want:         true,
		},
		// AC1.7: empty selector (no matchLabels, no matchExpressions) => matches everything.
		"Automatic_EmptySelector_Matches": {
			xr: tu.NewResource("example.org/v1", "XResource", "empty-selector").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				WithCompositionRevisionSelector(
					CrossplaneAPIExtGroupV2,
					map[string]string{},
					nil,
				).
				Build(),
			targetLabels: map[string]string{"version": "0.0.2"},
			want:         true,
		},
		// Nil target labels with a matchLabels selector => cannot match.
		"Automatic_MatchLabels_NilTargetLabels_DoesNotMatch": {
			xr: tu.NewResource("example.org/v1", "XResource", "nil-target").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				WithCompositionRevisionSelector(
					CrossplaneAPIExtGroupV2,
					map[string]string{"version": "0.0.2"},
					nil,
				).
				Build(),
			targetLabels: nil,
			want:         false,
		},
		// A malformed selector (a scalar where an object is expected) must be a hard error, not
		// silently treated as "no selector" (which would wrongly match everything). Accuracy first.
		"Automatic_MalformedSelector_NotAnObject_Errors": {
			xr: tu.NewResource("example.org/v1", "XResource", "malformed-selector").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				WithNestedField("not-an-object", "spec", "crossplane", "compositionRevisionSelector").
				Build(),
			targetLabels: map[string]string{"version": "0.0.2"},
			wantErr:      true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// compLabels only affects the mismatch detail string, not the boolean; pass nil here.
			got, _, err := XRRevisionSelectorMatch(tt.xr, tt.targetLabels, nil)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("XRRevisionSelectorMatch() expected error, got nil")
				}

				return
			}

			if err != nil {
				t.Fatalf("XRRevisionSelectorMatch() unexpected error: %v", err)
			}

			if got != tt.want {
				t.Errorf("XRRevisionSelectorMatch() matches = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestXRRevisionSelectorMatch_Detail verifies the human-readable mismatch detail returned by
// XRRevisionSelectorMatch. The displayed "composition labels" are the union of the user's own
// composition labels (compLabels — the tunable menu) and any label the selector references, so a
// Crossplane-stamped label like crossplane.io/composition-name is shown only when it's load-bearing
// for the match. Matching itself uses the full targetLabels set.
func TestXRRevisionSelectorMatch_Detail(t *testing.T) {
	// targetLabels mirror predictedRevisionLabels: the user's composition labels plus the stamped
	// composition-name label. compLabels are the user's own composition labels.
	targetLabels := map[string]string{
		"version":            "0.0.2",
		LabelCompositionName: "xnopresources.example.org",
	}
	compLabels := map[string]string{"version": "0.0.2"}

	tests := map[string]struct {
		xr           *un.Unstructured
		wantMatch    bool
		wantContains []string
		wantOmits    []string
	}{
		// A selector not referencing composition-name: the hint shows the user's composition labels
		// (version=0.0.2) and the selector (version=0.0.1), but omits the stamped composition-name.
		"MatchLabelsSelector_HidesStampedNameLabel": {
			xr: tu.NewResource("example.org/v1", "XResource", "sel").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				WithCompositionRevisionSelector(CrossplaneAPIExtGroupV2, map[string]string{"version": "0.0.1"}, nil).
				Build(),
			wantMatch:    false,
			wantContains: []string{"version=0.0.1", "version=0.0.2"},
			wantOmits:    []string{LabelCompositionName},
		},
		// A selector that references composition-name is load-bearing, so it's surfaced with its value.
		"SelectorReferencesNameLabel_ShowsIt": {
			xr: tu.NewResource("example.org/v1", "XResource", "sel-name").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				WithCompositionRevisionSelector(CrossplaneAPIExtGroupV2, map[string]string{LabelCompositionName: "other"}, nil).
				Build(),
			wantMatch:    false,
			wantContains: []string{LabelCompositionName, "xnopresources.example.org"},
		},
		// No selector => matches => empty detail.
		"NoSelector_Matches_EmptyDetail": {
			xr: tu.NewResource("example.org/v1", "XResource", "no-sel").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				Build(),
			wantMatch: true,
			wantOmits: []string{"compositionRevisionSelector"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			matches, detail, err := XRRevisionSelectorMatch(tt.xr, targetLabels, compLabels)
			if err != nil {
				t.Fatalf("XRRevisionSelectorMatch() unexpected error: %v", err)
			}

			if matches != tt.wantMatch {
				t.Errorf("XRRevisionSelectorMatch() matches = %v, want %v", matches, tt.wantMatch)
			}

			for _, want := range tt.wantContains {
				if !strings.Contains(detail, want) {
					t.Errorf("expected detail to contain %q, got %q", want, detail)
				}
			}

			for _, omit := range tt.wantOmits {
				if strings.Contains(detail, omit) {
					t.Errorf("expected detail to omit %q, got %q", omit, detail)
				}
			}
		})
	}
}
