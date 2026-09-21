package crossplane

import (
	"context"
	"maps"
	"strings"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
)

func TestDefaultCompositionRevisionClient_Initialize(t *testing.T) {
	ctx := t.Context()

	tests := map[string]struct {
		reason       string
		mockResource *tu.MockResourceClient
		expectError  bool
	}{
		"SuccessfulInitialization": {
			reason: "Should successfully initialize the client",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithFoundGVKs([]schema.GroupVersionKind{{Group: CrossplaneAPIExtGroup, Kind: "CompositionRevision"}}).
				Build(),
			expectError: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultCompositionRevisionClient{
				resourceClient:         tt.mockResource,
				logger:                 tu.TestLogger(t, false),
				revisions:              make(map[string]*apiextensionsv1.CompositionRevision),
				revisionsByComposition: make(map[string][]*apiextensionsv1.CompositionRevision),
			}

			err := c.Initialize(ctx)

			if tt.expectError && err == nil {
				t.Errorf("\n%s\nInitialize(): expected error but got none", tt.reason)
			} else if !tt.expectError && err != nil {
				t.Errorf("\n%s\nInitialize(): unexpected error: %v", tt.reason, err)
			}
		})
	}
}

func TestDefaultCompositionRevisionClient_GetCompositionRevision(t *testing.T) {
	ctx := t.Context()

	// Create a test composition revision
	testRev := &apiextensionsv1.CompositionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-comp-abc123",
			Labels: map[string]string{
				LabelCompositionName: "test-comp",
			},
		},
		Spec: apiextensionsv1.CompositionRevisionSpec{
			Revision: 1,
			CompositeTypeRef: apiextensionsv1.TypeReference{
				APIVersion: "example.org/v1",
				Kind:       "XR1",
			},
		},
	}

	// Mock resource client
	mockResource := tu.NewMockResourceClient().
		WithSuccessfulInitialize().
		WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
			if gvk.Group == CrossplaneAPIExtGroup && gvk.Kind == "CompositionRevision" && name == "test-comp-abc123" {
				u := &un.Unstructured{}
				u.SetGroupVersionKind(gvk)
				u.SetName(name)

				obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(testRev)
				if err != nil {
					return nil, err
				}

				u.SetUnstructuredContent(obj)

				return u, nil
			}

			return nil, errors.New("composition revision not found")
		}).
		Build()

	tests := map[string]struct {
		reason       string
		name         string
		cache        map[string]*apiextensionsv1.CompositionRevision
		expectRev    *apiextensionsv1.CompositionRevision
		expectError  bool
		errorPattern string
	}{
		"CachedRevision": {
			reason: "Should return revision from cache when available",
			name:   "cached-rev",
			cache: map[string]*apiextensionsv1.CompositionRevision{
				"cached-rev": testRev,
			},
			expectRev:   testRev,
			expectError: false,
		},
		"FetchFromCluster": {
			reason:      "Should fetch revision from cluster when not in cache",
			name:        "test-comp-abc123",
			cache:       map[string]*apiextensionsv1.CompositionRevision{},
			expectRev:   testRev,
			expectError: false,
		},
		"NotFound": {
			reason:       "Should return error when revision doesn't exist",
			name:         "nonexistent-rev",
			cache:        map[string]*apiextensionsv1.CompositionRevision{},
			expectRev:    nil,
			expectError:  true,
			errorPattern: "cannot get composition revision",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultCompositionRevisionClient{
				resourceClient:         mockResource,
				logger:                 tu.TestLogger(t, false),
				revisions:              tt.cache,
				revisionsByComposition: make(map[string][]*apiextensionsv1.CompositionRevision),
			}

			rev, err := c.GetCompositionRevision(ctx, tt.name)

			if tt.expectError {
				if err == nil {
					t.Errorf("\n%s\nGetCompositionRevision(...): expected error but got none", tt.reason)
					return
				}

				if tt.errorPattern != "" && !strings.Contains(err.Error(), tt.errorPattern) {
					t.Errorf("\n%s\nGetCompositionRevision(...): expected error containing %q, got %q",
						tt.reason, tt.errorPattern, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nGetCompositionRevision(...): unexpected error: %v", tt.reason, err)
				return
			}

			if diff := cmp.Diff(tt.expectRev.GetName(), rev.GetName()); diff != "" {
				t.Errorf("\n%s\nGetCompositionRevision(...): -want name, +got name:\n%s", tt.reason, diff)
			}
		})
	}
}

func TestDefaultCompositionRevisionClient_GetLatestRevisionForComposition(t *testing.T) {
	ctx := t.Context()

	// Create test revisions
	rev1 := &apiextensionsv1.CompositionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-comp-rev1",
			Labels: map[string]string{
				LabelCompositionName: "test-comp",
			},
		},
		Spec: apiextensionsv1.CompositionRevisionSpec{
			Revision: 1,
			CompositeTypeRef: apiextensionsv1.TypeReference{
				APIVersion: "example.org/v1",
				Kind:       "XR1",
			},
		},
	}

	rev2 := &apiextensionsv1.CompositionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-comp-rev2",
			Labels: map[string]string{
				LabelCompositionName: "test-comp",
			},
		},
		Spec: apiextensionsv1.CompositionRevisionSpec{
			Revision: 2,
			CompositeTypeRef: apiextensionsv1.TypeReference{
				APIVersion: "example.org/v1",
				Kind:       "XR1",
			},
		},
	}

	rev3 := &apiextensionsv1.CompositionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-comp-rev3",
			Labels: map[string]string{
				LabelCompositionName: "test-comp",
			},
		},
		Spec: apiextensionsv1.CompositionRevisionSpec{
			Revision: 3,
			CompositeTypeRef: apiextensionsv1.TypeReference{
				APIVersion: "example.org/v1",
				Kind:       "XR1",
			},
		},
	}

	// Create duplicate revision numbers (error case)
	duplicateRev1 := &apiextensionsv1.CompositionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dup-comp-rev1",
			Labels: map[string]string{
				LabelCompositionName: "dup-comp",
			},
		},
		Spec: apiextensionsv1.CompositionRevisionSpec{
			Revision: 5,
			CompositeTypeRef: apiextensionsv1.TypeReference{
				APIVersion: "example.org/v1",
				Kind:       "XR1",
			},
		},
	}

	duplicateRev2 := &apiextensionsv1.CompositionRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dup-comp-rev2",
			Labels: map[string]string{
				LabelCompositionName: "dup-comp",
			},
		},
		Spec: apiextensionsv1.CompositionRevisionSpec{
			Revision: 5, // Same revision number as duplicateRev1
			CompositeTypeRef: apiextensionsv1.TypeReference{
				APIVersion: "example.org/v1",
				Kind:       "XR1",
			},
		},
	}

	// Convert revisions to unstructured
	toUnstructured := func(rev *apiextensionsv1.CompositionRevision) *un.Unstructured {
		u := &un.Unstructured{}
		obj, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(rev)
		u.SetUnstructuredContent(obj)
		u.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   CrossplaneAPIExtGroup,
			Version: "v1",
			Kind:    "CompositionRevision",
		})

		return u
	}

	tests := map[string]struct {
		reason          string
		compositionName string
		mockResource    *tu.MockResourceClient
		cachedByComp    map[string][]*apiextensionsv1.CompositionRevision
		expectRev       *apiextensionsv1.CompositionRevision
		expectError     bool
		errorPattern    string
	}{
		"ReturnsLatestRevision": {
			reason:          "Should return the revision with the highest revision number",
			compositionName: "test-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithResourcesFoundByLabel([]*un.Unstructured{
					toUnstructured(rev1), toUnstructured(rev2), toUnstructured(rev3),
				}, LabelCompositionName, "test-comp").
				Build(),
			expectRev:   rev3,
			expectError: false,
		},
		"SingleRevision": {
			reason:          "Should return the only revision when there's just one",
			compositionName: "test-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithResourcesFoundByLabel([]*un.Unstructured{
					toUnstructured(rev1),
				}, LabelCompositionName, "test-comp").
				Build(),
			expectRev:   rev1,
			expectError: false,
		},
		"NoRevisionsFound": {
			reason:          "Should return error when no revisions exist for the composition",
			compositionName: "nonexistent-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithResourcesFoundByLabel([]*un.Unstructured{
					// Set up one composition but query for a different one
					toUnstructured(rev1),
				}, LabelCompositionName, "test-comp").
				Build(),
			expectRev:    nil,
			expectError:  true,
			errorPattern: "no composition revisions found",
		},
		"DuplicateRevisionNumbers": {
			reason:          "Should return error when multiple revisions have the same number",
			compositionName: "dup-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithResourcesFoundByLabel([]*un.Unstructured{
					toUnstructured(duplicateRev1), toUnstructured(duplicateRev2),
				}, LabelCompositionName, "dup-comp").
				Build(),
			expectRev:    nil,
			expectError:  true,
			errorPattern: "multiple composition revisions found with the same revision number",
		},
		"UsesCache": {
			reason:          "Should use cached revisions when available",
			compositionName: "test-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithGetResourcesByLabel(func(_ context.Context, _ schema.GroupVersionKind, _ string, _ metav1.LabelSelector) ([]*un.Unstructured, error) {
					// This should not be called because cache is populated
					return nil, errors.New("should not call GetResourcesByLabel when cache is populated")
				}).
				Build(),
			cachedByComp: map[string][]*apiextensionsv1.CompositionRevision{
				"test-comp": {rev1, rev2, rev3},
			},
			expectRev:   rev3,
			expectError: false,
		},
		"ListRevisionsError": {
			reason:          "Should return error when listing revisions fails",
			compositionName: "test-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithGetResourcesByLabel(func(_ context.Context, _ schema.GroupVersionKind, _ string, _ metav1.LabelSelector) ([]*un.Unstructured, error) {
					return nil, errors.New("list error")
				}).
				Build(),
			expectRev:    nil,
			expectError:  true,
			errorPattern: "cannot list composition revisions",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultCompositionRevisionClient{
				resourceClient:         tt.mockResource,
				logger:                 tu.TestLogger(t, false),
				revisions:              make(map[string]*apiextensionsv1.CompositionRevision),
				revisionsByComposition: tt.cachedByComp,
			}

			if tt.cachedByComp == nil {
				c.revisionsByComposition = make(map[string][]*apiextensionsv1.CompositionRevision)
			}

			rev, err := c.GetLatestRevisionForComposition(ctx, tt.compositionName, nil)

			if tt.expectError {
				if err == nil {
					t.Errorf("\n%s\nGetLatestRevisionForComposition(...): expected error but got none", tt.reason)
					return
				}

				if tt.errorPattern != "" && !strings.Contains(err.Error(), tt.errorPattern) {
					t.Errorf("\n%s\nGetLatestRevisionForComposition(...): expected error containing %q, got %q",
						tt.reason, tt.errorPattern, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nGetLatestRevisionForComposition(...): unexpected error: %v", tt.reason, err)
				return
			}

			if diff := cmp.Diff(tt.expectRev.GetName(), rev.GetName()); diff != "" {
				t.Errorf("\n%s\nGetLatestRevisionForComposition(...): -want name, +got name:\n%s", tt.reason, diff)
			}

			if diff := cmp.Diff(tt.expectRev.Spec.Revision, rev.Spec.Revision); diff != "" {
				t.Errorf("\n%s\nGetLatestRevisionForComposition(...): -want revision number, +got revision number:\n%s",
					tt.reason, diff)
			}
		})
	}
}

// TestDefaultCompositionRevisionClient_GetLatestRevisionForComposition_Selector covers the
// selector-aware revision selection used by the xr command (issue #388, Bug B): under Automatic
// policy with a compositionRevisionSelector, Crossplane picks the newest revision whose (inherited)
// labels match the selector — not the newest revision overall.
func TestDefaultCompositionRevisionClient_GetLatestRevisionForComposition_Selector(t *testing.T) {
	ctx := t.Context()

	rev := func(name string, revision int64, extraLabels map[string]string) *apiextensionsv1.CompositionRevision {
		lbls := map[string]string{LabelCompositionName: "chan-comp"}
		maps.Copy(lbls, extraLabels)

		return &apiextensionsv1.CompositionRevision{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls},
			Spec: apiextensionsv1.CompositionRevisionSpec{
				Revision:         revision,
				CompositeTypeRef: apiextensionsv1.TypeReference{APIVersion: "example.org/v1", Kind: "XR1"},
			},
		}
	}

	// active is revision 1 (channel=active); preview is revision 2 (channel=preview, newest overall).
	active := rev("chan-comp-active", 1, map[string]string{"channel": "active"})
	preview := rev("chan-comp-preview", 2, map[string]string{"channel": "preview"})
	// Two revisions sharing a walking tag major=v1: v1old (rev 1), v1new (rev 3, newest matching).
	v1old := rev("chan-comp-v1old", 1, map[string]string{"major": "v1"})
	v1new := rev("chan-comp-v1new", 3, map[string]string{"major": "v1"})

	toUnstructured := func(r *apiextensionsv1.CompositionRevision) *un.Unstructured {
		u := &un.Unstructured{}
		obj, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(r)
		u.SetUnstructuredContent(obj)
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: CrossplaneAPIExtGroup, Version: "v1", Kind: "CompositionRevision"})

		return u
	}

	mustSelector := func(m map[string]string) labels.Selector {
		s, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: m})
		if err != nil {
			t.Fatalf("building selector: %v", err)
		}

		return s
	}

	tests := map[string]struct {
		reason       string
		revisions    []*apiextensionsv1.CompositionRevision
		selector     labels.Selector
		expectName   string
		expectError  bool
		errorPattern string
	}{
		"SelectorPinsOlderActiveRevision": { // AC3.1
			reason:     "selector channel=active resolves to the active revision, not the newest overall",
			revisions:  []*apiextensionsv1.CompositionRevision{active, preview},
			selector:   mustSelector(map[string]string{"channel": "active"}),
			expectName: "chan-comp-active",
		},
		"SelectorSelectsPreviewRevision": { // AC3.2
			reason:     "selector channel=preview resolves to the preview revision",
			revisions:  []*apiextensionsv1.CompositionRevision{active, preview},
			selector:   mustSelector(map[string]string{"channel": "preview"}),
			expectName: "chan-comp-preview",
		},
		"WalkingTagSelectsNewestMatching": { // AC3.3
			reason:     "selector major=v1 matches multiple revisions; newest matching wins",
			revisions:  []*apiextensionsv1.CompositionRevision{v1old, v1new},
			selector:   mustSelector(map[string]string{"major": "v1"}),
			expectName: "chan-comp-v1new",
		},
		"EverythingSelectorSelectsNewestOverall": { // AC3.4
			reason:     "labels.Everything() selects the newest revision overall",
			revisions:  []*apiextensionsv1.CompositionRevision{active, preview},
			selector:   labels.Everything(),
			expectName: "chan-comp-preview",
		},
		"NilSelectorSelectsNewestOverall": { // AC3.4 (nil is the ergonomic "no restriction")
			reason:     "a nil selector means no restriction and selects the newest revision overall",
			revisions:  []*apiextensionsv1.CompositionRevision{active, preview},
			selector:   nil,
			expectName: "chan-comp-preview",
		},
		"NoRevisionMatchesSelector": { // AC3.5
			reason:       "a selector matching no revision is a hard error",
			revisions:    []*apiextensionsv1.CompositionRevision{active, preview},
			selector:     mustSelector(map[string]string{"channel": "nonexistent"}),
			expectError:  true,
			errorPattern: "match selector",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			unRevs := make([]*un.Unstructured, len(tt.revisions))
			for i, r := range tt.revisions {
				unRevs[i] = toUnstructured(r)
			}

			mockResource := tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithResourcesFoundByLabel(unRevs, LabelCompositionName, "chan-comp").
				Build()

			c := &DefaultCompositionRevisionClient{
				resourceClient:         mockResource,
				logger:                 tu.TestLogger(t, false),
				revisions:              make(map[string]*apiextensionsv1.CompositionRevision),
				revisionsByComposition: make(map[string][]*apiextensionsv1.CompositionRevision),
			}

			got, err := c.GetLatestRevisionForComposition(ctx, "chan-comp", tt.selector)

			if tt.expectError {
				if err == nil {
					t.Fatalf("\n%s\nexpected error but got none", tt.reason)
				}

				if tt.errorPattern != "" && !strings.Contains(err.Error(), tt.errorPattern) {
					t.Errorf("\n%s\nexpected error containing %q, got %q", tt.reason, tt.errorPattern, err.Error())
				}

				return
			}

			if err != nil {
				t.Fatalf("\n%s\nunexpected error: %v", tt.reason, err)
			}

			if got.GetName() != tt.expectName {
				t.Errorf("\n%s\ngot revision %q, want %q", tt.reason, got.GetName(), tt.expectName)
			}
		})
	}
}

func TestDefaultCompositionRevisionClient_GetCompositionFromRevision(t *testing.T) {
	tests := map[string]struct {
		reason     string
		revision   *apiextensionsv1.CompositionRevision
		expectComp *apiextensionsv1.Composition
		expectNil  bool
	}{
		"ValidRevision": {
			reason: "Should extract composition from revision",
			revision: &apiextensionsv1.CompositionRevision{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-comp-abc123",
					Labels: map[string]string{
						LabelCompositionName: "test-comp",
					},
				},
				Spec: apiextensionsv1.CompositionRevisionSpec{
					Revision: 1,
					CompositeTypeRef: apiextensionsv1.TypeReference{
						APIVersion: "example.org/v1",
						Kind:       "XR1",
					},
					Pipeline: []apiextensionsv1.PipelineStep{
						{
							Step: "test-step",
							FunctionRef: apiextensionsv1.FunctionReference{
								Name: "test-function",
							},
						},
					},
				},
			},
			expectComp: &apiextensionsv1.Composition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-comp",
				},
				Spec: apiextensionsv1.CompositionSpec{
					CompositeTypeRef: apiextensionsv1.TypeReference{
						APIVersion: "example.org/v1",
						Kind:       "XR1",
					},
					Pipeline: []apiextensionsv1.PipelineStep{
						{
							Step: "test-step",
							FunctionRef: apiextensionsv1.FunctionReference{
								Name: "test-function",
							},
						},
					},
				},
			},
			expectNil: false,
		},
		"NilRevision": {
			reason:     "Should return nil when revision is nil",
			revision:   nil,
			expectComp: nil,
			expectNil:  true,
		},
		"NoLabelUsesRevisionName": {
			reason: "Should use revision name when label is missing",
			revision: &apiextensionsv1.CompositionRevision{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-comp-abc123",
				},
				Spec: apiextensionsv1.CompositionRevisionSpec{
					Revision: 1,
					CompositeTypeRef: apiextensionsv1.TypeReference{
						APIVersion: "example.org/v1",
						Kind:       "XR1",
					},
				},
			},
			expectComp: &apiextensionsv1.Composition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-comp-abc123", // Should use revision name
				},
				Spec: apiextensionsv1.CompositionSpec{
					CompositeTypeRef: apiextensionsv1.TypeReference{
						APIVersion: "example.org/v1",
						Kind:       "XR1",
					},
				},
			},
			expectNil: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultCompositionRevisionClient{
				logger:                 tu.TestLogger(t, false),
				revisions:              make(map[string]*apiextensionsv1.CompositionRevision),
				revisionsByComposition: make(map[string][]*apiextensionsv1.CompositionRevision),
			}

			comp := c.GetCompositionFromRevision(tt.revision)

			if tt.expectNil {
				if comp != nil {
					t.Errorf("\n%s\nGetCompositionFromRevision(...): expected nil, got composition", tt.reason)
				}

				return
			}

			if comp == nil {
				t.Errorf("\n%s\nGetCompositionFromRevision(...): unexpected nil composition", tt.reason)
				return
			}

			if diff := cmp.Diff(tt.expectComp.GetName(), comp.GetName()); diff != "" {
				t.Errorf("\n%s\nGetCompositionFromRevision(...): -want name, +got name:\n%s", tt.reason, diff)
			}

			if diff := cmp.Diff(tt.expectComp.Spec.CompositeTypeRef, comp.Spec.CompositeTypeRef); diff != "" {
				t.Errorf("\n%s\nGetCompositionFromRevision(...): -want type ref, +got type ref:\n%s", tt.reason, diff)
			}
		})
	}
}
