package crossplane

import (
	"context"
	"strings"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
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

			rev, err := c.GetLatestRevisionForComposition(ctx, tt.compositionName)

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
