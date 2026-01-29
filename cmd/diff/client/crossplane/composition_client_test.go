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

var _ CompositionClient = (*tu.MockCompositionClient)(nil)

const (
	CrossplaneAPIExtGroup   = "apiextensions.crossplane.io"
	CrossplaneAPIExtGroupV1 = "apiextensions.crossplane.io/v1"
)

func TestDefaultCompositionClient_FindMatchingComposition(t *testing.T) {
	type fields struct {
		compositions map[string]*apiextensionsv1.Composition
	}

	type args struct {
		ctx context.Context
		res *un.Unstructured
	}

	type want struct {
		composition *apiextensionsv1.Composition
		err         error
	}

	// Create test compositions
	matchingComp := tu.NewComposition("matching-comp").
		WithCompositeTypeRef("example.org/v1", "XR1").
		Build()

	nonMatchingComp := tu.NewComposition("non-matching-comp").
		WithCompositeTypeRef("example.org/v1", "OtherXR").
		Build()

	referencedComp := tu.NewComposition("referenced-comp").
		WithCompositeTypeRef("example.org/v1", "XR1").
		Build()

	incompatibleComp := tu.NewComposition("incompatible-comp").
		WithCompositeTypeRef("example.org/v1", "OtherXR").
		Build()

	labeledComp := func() *apiextensionsv1.Composition {
		comp := tu.NewComposition("labeled-comp").
			WithCompositeTypeRef("example.org/v1", "XR1").
			Build()
		comp.SetLabels(map[string]string{
			"environment": "production",
			"tier":        "standard",
		})

		return comp
	}()

	aComp := func() *apiextensionsv1.Composition {
		comp := tu.NewComposition("a-comp").
			WithCompositeTypeRef("example.org/v1", "XR1").
			Build()
		comp.SetLabels(map[string]string{
			"environment": "production",
		})

		return comp
	}()

	bComp := func() *apiextensionsv1.Composition {
		comp := tu.NewComposition("b-comp").
			WithCompositeTypeRef("example.org/v1", "XR1").
			Build()
		comp.SetLabels(map[string]string{
			"environment": "production",
		})

		return comp
	}()

	versionMismatchComp := tu.NewComposition("version-mismatch-comp").
		WithCompositeTypeRef("example.org/v2", "XR1").
		Build()

	tests := map[string]struct {
		reason       string
		mockResource tu.MockResourceClient
		mockDef      tu.MockDefinitionClient
		fields       fields
		args         args
		want         want
	}{
		"NoMatchingComposition": {
			reason: "Should return error when no matching composition exists",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"non-matching-comp": nonMatchingComp,
				},
			},
			args: args{
				ctx: t.Context(),
				res: tu.NewResource("example.org/v1", "XR1", "my-xr").Build(),
			},
			want: want{
				err: errors.Errorf("no composition found for %s", "example.org/v1, Kind=XR1"),
			},
		},
		"MatchingComposition": {
			reason: "Should return the matching composition",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"matching-comp":     matchingComp,
					"non-matching-comp": nonMatchingComp,
				},
			},
			args: args{
				ctx: t.Context(),
				res: tu.NewResource("example.org/v1", "XR1", "my-xr").Build(),
			},
			want: want{
				composition: matchingComp,
			},
		},
		"DirectCompositionReference": {
			reason: "Should return the composition referenced by spec.compositionRef.name",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"default-comp":    matchingComp,
					"referenced-comp": referencedComp,
				},
			},
			args: args{
				ctx: t.Context(),
				res: func() *un.Unstructured {
					xr := tu.NewResource("example.org/v1", "XR1", "my-xr").Build()
					_ = un.SetNestedField(xr.Object, "referenced-comp", "spec", "compositionRef", "name")

					return xr
				}(),
			},
			want: want{
				composition: referencedComp,
			},
		},
		"DirectCompositionReferenceIncompatible": {
			reason: "Should return error when directly referenced composition is incompatible",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"matching-comp":     matchingComp,
					"incompatible-comp": incompatibleComp,
				},
			},
			args: args{
				ctx: t.Context(),
				res: func() *un.Unstructured {
					xr := tu.NewResource("example.org/v1", "XR1", "my-xr").Build()
					_ = un.SetNestedField(xr.Object, "incompatible-comp", "spec", "compositionRef", "name")

					return xr
				}(),
			},
			want: want{
				err: errors.Errorf("composition incompatible-comp is not compatible with example.org/v1, Kind=XR1"),
			},
		},
		"ReferencedCompositionNotFound": {
			reason: "Should return error when referenced composition doesn't exist",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"existing-comp": matchingComp,
				},
			},
			args: args{
				ctx: t.Context(),
				res: func() *un.Unstructured {
					xr := tu.NewResource("example.org/v1", "XR1", "my-xr").Build()
					_ = un.SetNestedField(xr.Object, "non-existent-comp", "spec", "compositionRef", "name")

					return xr
				}(),
			},
			want: want{
				err: errors.Errorf("composition non-existent-comp referenced in example.org/v1, Kind=XR1/my-xr not found"),
			},
		},
		"CompositionSelectorMatch": {
			reason: "Should return composition matching the selector labels",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"labeled-comp":      labeledComp,
					"non-matching-comp": nonMatchingComp,
				},
			},
			args: args{
				ctx: t.Context(),
				res: func() *un.Unstructured {
					xr := tu.NewResource("example.org/v1", "XR1", "my-xr").Build()
					_ = un.SetNestedStringMap(xr.Object, map[string]string{
						"environment": "production",
					}, "spec", "compositionSelector", "matchLabels")

					return xr
				}(),
			},
			want: want{
				composition: labeledComp,
			},
		},
		"CompositionSelectorNoMatch": {
			reason: "Should return error when no composition matches the selector",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"labeled-comp": func() *apiextensionsv1.Composition {
						comp := tu.NewComposition("labeled-comp").
							WithCompositeTypeRef("example.org/v1", "XR1").
							Build()
						comp.SetLabels(map[string]string{
							"environment": "staging",
						})

						return comp
					}(),
				},
			},
			args: args{
				ctx: t.Context(),
				res: func() *un.Unstructured {
					xr := tu.NewResource("example.org/v1", "XR1", "my-xr").Build()
					_ = un.SetNestedStringMap(xr.Object, map[string]string{
						"environment": "production",
					}, "spec", "compositionSelector", "matchLabels")

					return xr
				}(),
			},
			want: want{
				err: errors.Errorf("no compatible composition found matching labels map[environment:production] for example.org/v1, Kind=XR1/my-xr"),
			},
		},
		"MultipleCompositionMatches": {
			reason: "Should return an error when multiple compositions match the selector",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"a-comp": aComp,
					"b-comp": bComp,
				},
			},
			args: args{
				ctx: t.Context(),
				res: func() *un.Unstructured {
					xr := tu.NewResource("example.org/v1", "XR1", "my-xr").Build()
					_ = un.SetNestedStringMap(xr.Object, map[string]string{
						"environment": "production",
					}, "spec", "compositionSelector", "matchLabels")

					return xr
				}(),
			},
			want: want{
				err: errors.New("ambiguous composition selection: multiple compositions match"),
			},
		},
		"EmptyCompositionCache_DefaultLookup": {
			reason: "Should return error when composition cache is empty (default lookup)",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{},
			},
			args: args{
				ctx: t.Context(),
				res: tu.NewResource("example.org/v1", "XR1", "my-xr").Build(),
			},
			want: want{
				err: errors.Errorf("no composition found for %s", "example.org/v1, Kind=XR1"),
			},
		},
		"EmptyCompositionCache_DirectReference": {
			reason: "Should return error when composition cache is empty (direct reference)",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{},
			},
			args: args{
				ctx: t.Context(),
				res: func() *un.Unstructured {
					xr := tu.NewResource("example.org/v1", "XR1", "my-xr").Build()
					_ = un.SetNestedField(xr.Object, "referenced-comp", "spec", "compositionRef", "name")

					return xr
				}(),
			},
			want: want{
				err: errors.Errorf("composition referenced-comp referenced in example.org/v1, Kind=XR1/my-xr not found"),
			},
		},
		"EmptyCompositionCache_Selector": {
			reason: "Should return error when composition cache is empty (selector)",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{},
			},
			args: args{
				ctx: t.Context(),
				res: func() *un.Unstructured {
					xr := tu.NewResource("example.org/v1", "XR1", "my-xr").Build()
					_ = un.SetNestedStringMap(xr.Object, map[string]string{
						"environment": "production",
					}, "spec", "compositionSelector", "matchLabels")

					return xr
				}(),
			},
			want: want{
				err: errors.Errorf("no compatible composition found matching labels map[environment:production] for example.org/v1, Kind=XR1/my-xr"),
			},
		},
		"AmbiguousDefaultSelection": {
			reason: "Should return error when multiple compositions match by type but no selection criteria provided",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"comp1": matchingComp,
					"comp2": referencedComp, // Both match same XR type
				},
			},
			args: args{
				ctx: t.Context(),
				res: tu.NewResource("example.org/v1", "XR1", "my-xr").Build(),
			},
			want: want{
				err: errors.New("ambiguous composition selection: multiple compositions exist for example.org/v1, Kind=XR1"),
			},
		},
		"DifferentVersions": {
			reason: "Should not match compositions with different versions",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV1XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"version-mismatch-comp": versionMismatchComp,
				},
			},
			args: args{
				ctx: t.Context(),
				res: tu.NewResource("example.org/v1", "XR1", "my-xr").Build(),
			},
			want: want{
				err: errors.Errorf("no composition found for %s", "example.org/v1, Kind=XR1"),
			},
		},
		"ClaimResource": {
			reason: "Should find composition for a claim type by determining XR type from XRD",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithListResources(func(_ context.Context, gvk schema.GroupVersionKind, _ string) ([]*un.Unstructured, error) {
					// Set up to return XRDs when requested
					if gvk.Group == CrossplaneAPIExtGroup && gvk.Kind == CompositeResourceDefinitionKind {
						return []*un.Unstructured{
							tu.NewResource(CrossplaneAPIExtGroupV1, CompositeResourceDefinitionKind, "xexampleresources.example.org").
								WithSpecField("group", "example.org").
								WithSpecField("names", map[string]any{
									"kind": "XExampleResource",
								}).
								WithSpecField("claimNames", map[string]any{
									"kind": "ExampleResourceClaim",
								}).
								WithSpecField("versions", []any{
									map[string]any{
										"name":          "v1",
										"served":        true,
										"referenceable": false,
									},
									map[string]any{
										"name":          "v2",
										"served":        true,
										"referenceable": true, // This is the version compositions should reference
									},
									map[string]any{
										"name":          "v3alpha1",
										"served":        true,
										"referenceable": false,
									},
								}).Build(),
						}, nil
					}

					return []*un.Unstructured{}, nil
				}).
				WithResourcesFoundByLabel([]*un.Unstructured{}, LabelCompositionName, "matching-comp").
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithXRDForClaim(
					tu.NewResource(CrossplaneAPIExtGroupV1, CompositeResourceDefinitionKind, "xexampleresources.example.org").
						WithSpecField("group", "example.org").
						WithSpecField("names", map[string]any{
							"kind": "XExampleResource",
						}).
						WithSpecField("claimNames", map[string]any{
							"kind": "ExampleResourceClaim",
						}).
						WithSpecField("versions", []any{
							map[string]any{
								"name":          "v1",
								"served":        true,
								"referenceable": false,
							},
							map[string]any{
								"name":          "v2",
								"served":        true,
								"referenceable": true, // This is the version compositions should reference
							},
							map[string]any{
								"name":          "v3alpha1",
								"served":        true,
								"referenceable": false,
							},
						}).Build(),
				).
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"matching-comp": {
						ObjectMeta: metav1.ObjectMeta{
							Name: "matching-comp",
						},
						Spec: apiextensionsv1.CompositionSpec{
							CompositeTypeRef: apiextensionsv1.TypeReference{
								APIVersion: "example.org/v2", // Match the referenceable version v2
								Kind:       "XExampleResource",
							},
						},
					},
				},
			},
			args: args{
				ctx: t.Context(),
				res: tu.NewResource("example.org/v1", "ExampleResourceClaim", "test-claim").
					WithSpecField("compositionRef", map[string]any{
						"name": "matching-comp",
					}).
					Build(),
			},
			want: want{
				composition: &apiextensionsv1.Composition{
					ObjectMeta: metav1.ObjectMeta{
						Name: "matching-comp",
					},
					Spec: apiextensionsv1.CompositionSpec{
						CompositeTypeRef: apiextensionsv1.TypeReference{
							APIVersion: "example.org/v2",
							Kind:       "XExampleResource",
						},
					},
				},
				err: nil,
			},
		},
		"ClaimResourceWithNoReferenceableVersion": {
			reason: "Should return error when XRD has no referenceable version",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithListResources(func(_ context.Context, gvk schema.GroupVersionKind, _ string) ([]*un.Unstructured, error) {
					// Return XRDs when requested - but this one has NO referenceable version
					if gvk.Group == CrossplaneAPIExtGroup && gvk.Kind == CompositeResourceDefinitionKind {
						return []*un.Unstructured{
							tu.NewResource(
								CrossplaneAPIExtGroupV1, CompositeResourceDefinitionKind, "xexampleresources.example.org").
								WithSpecField("group", "example.org").
								WithSpecField("names", map[string]any{
									"kind": "XExampleResource",
								}).
								WithSpecField("claimNames", map[string]any{
									"kind": "ExampleResourceClaim",
								}).
								WithSpecField("versions", []any{
									map[string]any{
										"name":          "v1",
										"served":        true,
										"referenceable": false, // No referenceable version
									},
									map[string]any{
										"name":          "v2",
										"served":        true,
										"referenceable": false, // No referenceable version
									},
								}).Build(),
						}, nil
					}

					return []*un.Unstructured{}, nil
				}).
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithXRDForClaim(
					tu.NewResource(
						CrossplaneAPIExtGroupV1, CompositeResourceDefinitionKind, "xexampleresources.example.org").
						WithSpecField("group", "example.org").
						WithSpecField("names", map[string]any{
							"kind": "XExampleResource",
						}).
						WithSpecField("claimNames", map[string]any{
							"kind": "ExampleResourceClaim",
						}).
						WithSpecField("versions", []any{
							map[string]any{
								"name":          "v1",
								"served":        true,
								"referenceable": false, // No referenceable version
							},
							map[string]any{
								"name":          "v2",
								"served":        true,
								"referenceable": false, // No referenceable version
							},
						}).Build(),
				).Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"matching-comp": {
						ObjectMeta: metav1.ObjectMeta{
							Name: "matching-comp",
						},
						Spec: apiextensionsv1.CompositionSpec{
							CompositeTypeRef: apiextensionsv1.TypeReference{
								APIVersion: "example.org/v1",
								Kind:       "XExampleResource",
							},
						},
					},
				},
			},
			args: args{
				ctx: t.Context(),
				res: tu.NewResource("example.org/v1", "ExampleResourceClaim", "test-claim").
					WithSpecField("compositionRef", map[string]any{
						"name": "matching-comp",
					}).
					Build(),
			},
			want: want{
				composition: nil,
				err:         errors.New("no referenceable version found in XRD"), // Should fail with this error
			},
		},
		// TODO:  add more tests against v2 XRDs
		"ResourceWithV2XRD": {
			reason: "Should determine path to compositionRef by determining XR type from XRD",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithListResources(func(_ context.Context, gvk schema.GroupVersionKind, _ string) ([]*un.Unstructured, error) {
					// Set up to return XRDs when requested
					if gvk.Group == CrossplaneAPIExtGroup && gvk.Kind == CompositeResourceDefinitionKind {
						return []*un.Unstructured{
							tu.NewResource(CrossplaneAPIExtGroupV1, CompositeResourceDefinitionKind, "xexampleresources.example.org").
								WithSpecField("group", "example.org").
								WithSpecField("names", map[string]any{
									"kind": "XExampleResource",
								}).
								WithSpecField("versions", []any{
									map[string]any{
										"name":          "v1",
										"served":        true,
										"referenceable": false,
									},
									map[string]any{
										"name":          "v2",
										"served":        true,
										"referenceable": true, // This is the version compositions should reference
									},
									map[string]any{
										"name":          "v3alpha1",
										"served":        true,
										"referenceable": false,
									},
								}).Build(),
						}, nil
					}

					return []*un.Unstructured{}, nil
				}).
				WithResourcesFoundByLabel([]*un.Unstructured{}, LabelCompositionName, "matching-comp").
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithV2XRDForXR().
				Build(),
			fields: fields{
				compositions: map[string]*apiextensionsv1.Composition{
					"matching-comp": {
						ObjectMeta: metav1.ObjectMeta{
							Name: "matching-comp",
						},
						Spec: apiextensionsv1.CompositionSpec{
							CompositeTypeRef: apiextensionsv1.TypeReference{
								APIVersion: "example.org/v2", // Match the referenceable version v2
								Kind:       "XExampleResource",
							},
						},
					},
				},
			},
			args: args{
				ctx: t.Context(),
				res: tu.NewResource("example.org/v2", "XExampleResource", "my-xr").
					WithSpecField("crossplane", map[string]any{
						"compositionRef": map[string]any{
							"name": "matching-comp",
						},
					}).
					Build(),
			},
			want: want{
				composition: &apiextensionsv1.Composition{
					ObjectMeta: metav1.ObjectMeta{
						Name: "matching-comp",
					},
					Spec: apiextensionsv1.CompositionSpec{
						CompositeTypeRef: apiextensionsv1.TypeReference{
							APIVersion: "example.org/v2",
							Kind:       "XExampleResource",
						},
					},
				},
				err: nil,
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Create the CompositionClient
			c := &DefaultCompositionClient{
				resourceClient:   &tt.mockResource,
				definitionClient: &tt.mockDef,
				revisionClient:   NewCompositionRevisionClient(&tt.mockResource, tu.TestLogger(t, false)),
				logger:           tu.TestLogger(t, false),
				compositions:     tt.fields.compositions,
			}

			// Test the FindMatchingComposition method
			got, err := c.FindMatchingComposition(tt.args.ctx, tt.args.res)

			if tt.want.err != nil {
				if err == nil {
					t.Errorf("\n%s\nFindMatchingComposition(...): expected error but got none", tt.reason)
					return
				}

				if !strings.Contains(err.Error(), tt.want.err.Error()) {
					t.Errorf("\n%s\nFindMatchingComposition(...): expected error containing %q, got %q",
						tt.reason, tt.want.err.Error(), err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nFindMatchingComposition(...): unexpected error: %v", tt.reason, err)
				return
			}

			if tt.want.composition != nil {
				if diff := cmp.Diff(tt.want.composition.Name, got.Name); diff != "" {
					t.Errorf("\n%s\nFindMatchingComposition(...): -want composition name, +got composition name:\n%s", tt.reason, diff)
				}

				if diff := cmp.Diff(tt.want.composition.Spec.CompositeTypeRef, got.Spec.CompositeTypeRef); diff != "" {
					t.Errorf("\n%s\nFindMatchingComposition(...): -want composition type ref, +got composition type ref:\n%s", tt.reason, diff)
				}
			}
		})
	}
}

func TestDefaultCompositionClient_GetComposition(t *testing.T) {
	ctx := t.Context()

	// Create a test composition
	testComp := tu.NewComposition("test-comp").
		WithCompositeTypeRef("example.org/v1", "XR1").
		Build()

	// Mock resource client
	mockResource := tu.NewMockResourceClient().
		WithSuccessfulInitialize().
		WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
			if gvk.Group == CrossplaneAPIExtGroup && gvk.Kind == "Composition" && name == "test-comp" {
				u := &un.Unstructured{}
				u.SetGroupVersionKind(gvk)
				u.SetName(name)

				obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(testComp)
				if err != nil {
					return nil, err
				}

				u.SetUnstructuredContent(obj)

				return u, nil
			}

			return nil, errors.New("composition not found")
		}).
		Build()

	tests := map[string]struct {
		reason      string
		name        string
		cache       map[string]*apiextensionsv1.Composition
		expectComp  *apiextensionsv1.Composition
		expectError bool
	}{
		"CachedComposition": {
			reason: "Should return composition from cache when available",
			name:   "cached-comp",
			cache: map[string]*apiextensionsv1.Composition{
				"cached-comp": testComp,
			},
			expectComp:  testComp,
			expectError: false,
		},
		"FetchFromCluster": {
			reason:      "Should fetch composition from cluster when not in cache",
			name:        "test-comp",
			cache:       map[string]*apiextensionsv1.Composition{},
			expectComp:  testComp,
			expectError: false,
		},
		"NotFound": {
			reason:      "Should return error when composition doesn't exist",
			name:        "nonexistent-comp",
			cache:       map[string]*apiextensionsv1.Composition{},
			expectComp:  nil,
			expectError: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultCompositionClient{
				resourceClient: mockResource,
				revisionClient: NewCompositionRevisionClient(mockResource, tu.TestLogger(t, false)),
				logger:         tu.TestLogger(t, false),
				compositions:   tt.cache,
			}

			comp, err := c.GetComposition(ctx, tt.name)

			if tt.expectError {
				if err == nil {
					t.Errorf("\n%s\nGetComposition(...): expected error but got none", tt.reason)
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nGetComposition(...): unexpected error: %v", tt.reason, err)
				return
			}

			if diff := cmp.Diff(tt.expectComp.GetName(), comp.GetName()); diff != "" {
				t.Errorf("\n%s\nGetComposition(...): -want name, +got name:\n%s", tt.reason, diff)
			}

			if diff := cmp.Diff(tt.expectComp.Spec.CompositeTypeRef, comp.Spec.CompositeTypeRef); diff != "" {
				t.Errorf("\n%s\nGetComposition(...): -want type ref, +got type ref:\n%s", tt.reason, diff)
			}
		})
	}
}

func TestDefaultCompositionClient_ListCompositions(t *testing.T) {
	ctx := t.Context()

	// Create test compositions
	comp1 := tu.NewComposition("comp1").
		WithCompositeTypeRef("example.org/v1", "XR1").
		Build()
	comp2 := tu.NewComposition("comp2").
		WithCompositeTypeRef("example.org/v1", "XR2").
		Build()

	// Convert compositions to unstructured
	u1 := &un.Unstructured{}
	obj1, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(comp1)
	u1.SetUnstructuredContent(obj1)
	u1.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   CrossplaneAPIExtGroup,
		Version: "v1",
		Kind:    "Composition",
	})

	u2 := &un.Unstructured{}
	obj2, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(comp2)
	u2.SetUnstructuredContent(obj2)
	u2.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   CrossplaneAPIExtGroup,
		Version: "v1",
		Kind:    "Composition",
	})

	tests := map[string]struct {
		reason        string
		mockResource  *tu.MockResourceClient
		expectComps   []*apiextensionsv1.Composition
		expectError   bool
		errorContains string
	}{
		"SuccessfulList": {
			reason: "Should return compositions when list succeeds",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithListResources(func(_ context.Context, gvk schema.GroupVersionKind, _ string) ([]*un.Unstructured, error) {
					if gvk.Group == CrossplaneAPIExtGroup && gvk.Kind == "Composition" {
						return []*un.Unstructured{u1, u2}, nil
					}

					return nil, errors.New("unexpected GVK")
				}).
				Build(),
			expectComps: []*apiextensionsv1.Composition{comp1, comp2},
			expectError: false,
		},
		"ListError": {
			reason: "Should return error when list fails",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithListResourcesFailure("list error").
				Build(),
			expectComps:   nil,
			expectError:   true,
			errorContains: "cannot list compositions",
		},
		"ConversionError": {
			reason: "Should return error when conversion fails",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithListResources(func(_ context.Context, gvk schema.GroupVersionKind, _ string) ([]*un.Unstructured, error) {
					// Create an invalid unstructured that will definitely fail conversion
					invalid := &un.Unstructured{}
					invalid.SetGroupVersionKind(gvk)
					invalid.SetName("invalid")

					// Include invalid data that won't convert to a Composition
					invalid.Object["spec"] = "not-a-map-but-a-string" // This will cause conversion to fail

					return []*un.Unstructured{invalid}, nil
				}).
				Build(),
			expectComps:   nil,
			expectError:   true,
			errorContains: "cannot convert unstructured to Composition",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultCompositionClient{
				resourceClient: tt.mockResource,
				revisionClient: NewCompositionRevisionClient(tt.mockResource, tu.TestLogger(t, false)),
				logger:         tu.TestLogger(t, false),
				compositions:   make(map[string]*apiextensionsv1.Composition),
			}

			comps, err := c.ListCompositions(ctx)

			if tt.expectError {
				if err == nil {
					t.Errorf("\n%s\nListCompositions(...): expected error but got none", tt.reason)
				} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("\n%s\nListCompositions(...): expected error containing %q, got %q",
						tt.reason, tt.errorContains, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nListCompositions(...): unexpected error: %v", tt.reason, err)
				return
			}

			if len(comps) != len(tt.expectComps) {
				t.Errorf("\n%s\nListCompositions(...): expected %d compositions, got %d",
					tt.reason, len(tt.expectComps), len(comps))

				return
			}

			// Check that we got the expected compositions
			for i, expected := range tt.expectComps {
				found := false

				for _, actual := range comps {
					if actual.GetName() == expected.GetName() {
						found = true
						break
					}
				}

				if !found {
					t.Errorf("\n%s\nListCompositions(...): composition %s not found in result",
						tt.reason, tt.expectComps[i].GetName())
				}
			}
		})
	}
}

func TestDefaultCompositionClient_Initialize(t *testing.T) {
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
				WithFoundGVKs([]schema.GroupVersionKind{{Group: CrossplaneAPIExtGroup, Kind: "Composition"}}).
				WithEmptyListResources().
				Build(),
			expectError: false,
		},
		"ResourceClientInitFailed": {
			reason: "Should return error when resource client initialization fails",
			mockResource: tu.NewMockResourceClient().
				WithInitialize(func(_ context.Context) error {
					return errors.New("init error")
				}).
				Build(),
			expectError: true,
		},
		"ListCompositionsFailed": {
			reason: "Should return error when listing compositions fails",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithListResourcesFailure("list error").
				Build(),
			expectError: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultCompositionClient{
				resourceClient: tt.mockResource,
				revisionClient: NewCompositionRevisionClient(tt.mockResource, tu.TestLogger(t, false)),
				logger:         tu.TestLogger(t, false),
				compositions:   make(map[string]*apiextensionsv1.Composition),
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

func TestDefaultCompositionClient_ResolveCompositionFromRevisions(t *testing.T) {
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

	// Create test XRD
	v1XRD := tu.NewResource(CrossplaneAPIExtGroupV1, CompositeResourceDefinitionKind, "xr1s.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
			"kind": "XR1",
		}).
		WithSpecField("versions", []any{
			map[string]any{
				"name":          "v1",
				"served":        true,
				"referenceable": true,
			},
		}).Build()

	v2XRD := tu.NewResource(CrossplaneAPIExtGroupV1, CompositeResourceDefinitionKind, "xr1s.example.org").
		WithSpecField("group", "example.org").
		WithSpecField("names", map[string]any{
			"kind": "XR1",
		}).
		WithSpecField("versions", []any{
			map[string]any{
				"name":          "v2",
				"served":        true,
				"referenceable": true,
			},
		}).Build()

	tests := map[string]struct {
		reason          string
		xrd             *un.Unstructured
		res             *un.Unstructured
		compositionName string
		mockResource    *tu.MockResourceClient
		expectComp      *apiextensionsv1.Composition
		expectNil       bool
		expectError     bool
		errorPattern    string
	}{
		"AutomaticPolicyUsesLatestRevision": {
			reason: "Should use latest revision when update policy is Automatic",
			xrd:    v1XRD,
			res: tu.NewResource("example.org/v1", "XR1", "my-xr").
				WithSpecField("compositionRef", map[string]any{
					"name": "test-comp",
				}).
				WithSpecField("compositionUpdatePolicy", "Automatic").
				Build(),
			compositionName: "test-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithResourcesFoundByLabel([]*un.Unstructured{
					toUnstructured(rev1), toUnstructured(rev2),
				}, LabelCompositionName, "test-comp").
				Build(),
			expectComp: &apiextensionsv1.Composition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-comp",
				},
				Spec: apiextensionsv1.CompositionSpec{
					CompositeTypeRef: apiextensionsv1.TypeReference{
						APIVersion: "example.org/v1",
						Kind:       "XR1",
					},
				},
			},
			expectError: false,
		},
		"ManualPolicyWithRevisionRefUsesSpecifiedRevision": {
			reason: "Should use specified revision when update policy is Manual with revision ref",
			xrd:    v1XRD,
			res: tu.NewResource("example.org/v1", "XR1", "my-xr").
				WithSpecField("compositionRef", map[string]any{
					"name": "test-comp",
				}).
				WithSpecField("compositionRevisionRef", map[string]any{
					"name": "test-comp-rev1",
				}).
				WithSpecField("compositionUpdatePolicy", "Manual").
				Build(),
			compositionName: "test-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithGetResource(func(_ context.Context, _ schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
					if name == "test-comp-rev1" {
						return toUnstructured(rev1), nil
					}

					return nil, errors.New("not found")
				}).
				Build(),
			expectComp: &apiextensionsv1.Composition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-comp",
				},
				Spec: apiextensionsv1.CompositionSpec{
					CompositeTypeRef: apiextensionsv1.TypeReference{
						APIVersion: "example.org/v1",
						Kind:       "XR1",
					},
				},
			},
			expectError: false,
		},
		"ManualPolicyWithoutRevisionRefUsesLatestRevision": {
			reason: "Should use latest revision when update policy is Manual without revision ref (net new XR case)",
			xrd:    v1XRD,
			res: tu.NewResource("example.org/v1", "XR1", "my-xr").
				WithSpecField("compositionRef", map[string]any{
					"name": "test-comp",
				}).
				WithSpecField("compositionUpdatePolicy", "Manual").
				Build(),
			compositionName: "test-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithResourcesFoundByLabel([]*un.Unstructured{
					toUnstructured(rev1), toUnstructured(rev2),
				}, LabelCompositionName, "test-comp").
				Build(),
			expectComp: &apiextensionsv1.Composition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-comp",
				},
				Spec: apiextensionsv1.CompositionSpec{
					CompositeTypeRef: apiextensionsv1.TypeReference{
						APIVersion: "example.org/v1",
						Kind:       "XR1",
					},
				},
			},
			expectError: false,
		},
		"V2XRWithAutomaticPolicy": {
			reason: "Should use latest revision for v2 XR with Automatic policy",
			xrd:    v2XRD,
			res: tu.NewResource("example.org/v2", "XR1", "my-xr").
				WithSpecField("crossplane", map[string]any{
					"compositionRef": map[string]any{
						"name": "test-comp",
					},
					"compositionUpdatePolicy": "Automatic",
				}).
				Build(),
			compositionName: "test-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithResourcesFoundByLabel([]*un.Unstructured{
					toUnstructured(rev1), toUnstructured(rev2),
				}, LabelCompositionName, "test-comp").
				Build(),
			expectComp: &apiextensionsv1.Composition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-comp",
				},
				Spec: apiextensionsv1.CompositionSpec{
					CompositeTypeRef: apiextensionsv1.TypeReference{
						APIVersion: "example.org/v1",
						Kind:       "XR1",
					},
				},
			},
			expectError: false,
		},
		"V2XRWithManualPolicyWithoutRevisionRef": {
			reason: "Should use latest revision for v2 XR with Manual policy but no revision ref",
			xrd:    v2XRD,
			res: tu.NewResource("example.org/v2", "XR1", "my-xr").
				WithSpecField("crossplane", map[string]any{
					"compositionRef": map[string]any{
						"name": "test-comp",
					},
					"compositionUpdatePolicy": "Manual",
				}).
				Build(),
			compositionName: "test-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithResourcesFoundByLabel([]*un.Unstructured{
					toUnstructured(rev1), toUnstructured(rev2),
				}, LabelCompositionName, "test-comp").
				Build(),
			expectComp: &apiextensionsv1.Composition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-comp",
				},
				Spec: apiextensionsv1.CompositionSpec{
					CompositeTypeRef: apiextensionsv1.TypeReference{
						APIVersion: "example.org/v1",
						Kind:       "XR1",
					},
				},
			},
			expectError: false,
		},
		"NoRevisionsFoundFallsBackToNil": {
			reason: "Should return nil when no revisions exist (unpublished composition)",
			xrd:    v1XRD,
			res: tu.NewResource("example.org/v1", "XR1", "my-xr").
				WithSpecField("compositionRef", map[string]any{
					"name": "test-comp",
				}).
				WithSpecField("compositionUpdatePolicy", "Automatic").
				Build(),
			compositionName: "test-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithResourcesFoundByLabel([]*un.Unstructured{}, LabelCompositionName, "test-comp").
				Build(),
			expectNil:   true,
			expectError: false,
		},
		"ManualPolicyWithNonexistentRevisionRef": {
			reason: "Should return error when specified revision doesn't exist",
			xrd:    v1XRD,
			res: tu.NewResource("example.org/v1", "XR1", "my-xr").
				WithSpecField("compositionRef", map[string]any{
					"name": "test-comp",
				}).
				WithSpecField("compositionRevisionRef", map[string]any{
					"name": "nonexistent-rev",
				}).
				WithSpecField("compositionUpdatePolicy", "Manual").
				Build(),
			compositionName: "test-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithGetResource(func(_ context.Context, _ schema.GroupVersionKind, _, _ string) (*un.Unstructured, error) {
					return nil, errors.New("not found")
				}).
				Build(),
			expectError:  true,
			errorPattern: "cannot get pinned composition revision",
		},
		"ManualPolicyWithRevisionFromDifferentComposition": {
			reason: "Should return error when revision belongs to different composition",
			xrd:    v1XRD,
			res: tu.NewResource("example.org/v1", "XR1", "my-xr").
				WithSpecField("compositionRef", map[string]any{
					"name": "test-comp",
				}).
				WithSpecField("compositionRevisionRef", map[string]any{
					"name": "other-comp-rev1",
				}).
				WithSpecField("compositionUpdatePolicy", "Manual").
				Build(),
			compositionName: "test-comp",
			mockResource: tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithGetResource(func(_ context.Context, _ schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
					if name == "other-comp-rev1" {
						wrongRev := &apiextensionsv1.CompositionRevision{
							ObjectMeta: metav1.ObjectMeta{
								Name: "other-comp-rev1",
								Labels: map[string]string{
									LabelCompositionName: "other-comp",
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

						return toUnstructured(wrongRev), nil
					}

					return nil, errors.New("not found")
				}).
				Build(),
			expectError:  true,
			errorPattern: "belongs to composition other-comp, not test-comp",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultCompositionClient{
				resourceClient: tt.mockResource,
				revisionClient: NewCompositionRevisionClient(tt.mockResource, tu.TestLogger(t, false)),
				logger:         tu.TestLogger(t, false),
				compositions:   make(map[string]*apiextensionsv1.Composition),
			}

			comp, err := c.resolveCompositionFromRevisions(ctx, tt.xrd, tt.res, tt.compositionName, "test-resource-id")

			if tt.expectError {
				if err == nil {
					t.Errorf("\n%s\nresolveCompositionFromRevisions(...): expected error but got none", tt.reason)
					return
				}

				if tt.errorPattern != "" && !strings.Contains(err.Error(), tt.errorPattern) {
					t.Errorf("\n%s\nresolveCompositionFromRevisions(...): expected error containing %q, got %q",
						tt.reason, tt.errorPattern, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nresolveCompositionFromRevisions(...): unexpected error: %v", tt.reason, err)
				return
			}

			if tt.expectNil {
				if comp != nil {
					t.Errorf("\n%s\nresolveCompositionFromRevisions(...): expected nil composition, got %v", tt.reason, comp)
				}

				return
			}

			if comp == nil {
				t.Errorf("\n%s\nresolveCompositionFromRevisions(...): unexpected nil composition", tt.reason)
				return
			}

			if diff := cmp.Diff(tt.expectComp.GetName(), comp.GetName()); diff != "" {
				t.Errorf("\n%s\nresolveCompositionFromRevisions(...): -want name, +got name:\n%s", tt.reason, diff)
			}

			if diff := cmp.Diff(tt.expectComp.Spec.CompositeTypeRef, comp.Spec.CompositeTypeRef); diff != "" {
				t.Errorf("\n%s\nresolveCompositionFromRevisions(...): -want type ref, +got type ref:\n%s", tt.reason, diff)
			}
		})
	}
}

func TestGetCrossplaneRefPaths(t *testing.T) {
	tests := map[string]struct {
		reason     string
		apiVersion string
		path       []string
		want       [][]string
	}{
		"V1XRDReturnsOnlyV1Path": {
			reason:     "Should return only v1 path for v1 XRD",
			apiVersion: "apiextensions.crossplane.io/v1",
			path:       []string{"compositionRef", "name"},
			want: [][]string{
				{"spec", "compositionRef", "name"},
			},
		},
		"V2XRDReturnsBothPaths": {
			reason:     "Should return both v2 and v1 paths for v2 XRD (v2 first)",
			apiVersion: "apiextensions.crossplane.io/v2",
			path:       []string{"compositionRef", "name"},
			want: [][]string{
				{"spec", "crossplane", "compositionRef", "name"},
				{"spec", "compositionRef", "name"},
			},
		},
		"NonCrossplaneAPIVersionReturnsBothPaths": {
			reason:     "Should return both paths for non-Crossplane API version (treated as v2)",
			apiVersion: "example.org/v1",
			path:       []string{"compositionRef", "name"},
			want: [][]string{
				{"spec", "crossplane", "compositionRef", "name"},
				{"spec", "compositionRef", "name"},
			},
		},
		"CompositionSelector": {
			reason:     "Should handle compositionSelector path correctly",
			apiVersion: "apiextensions.crossplane.io/v2",
			path:       []string{"compositionSelector", "matchLabels"},
			want: [][]string{
				{"spec", "crossplane", "compositionSelector", "matchLabels"},
				{"spec", "compositionSelector", "matchLabels"},
			},
		},
		"CompositionUpdatePolicy": {
			reason:     "Should handle compositionUpdatePolicy path correctly",
			apiVersion: "apiextensions.crossplane.io/v2",
			path:       []string{"compositionUpdatePolicy"},
			want: [][]string{
				{"spec", "crossplane", "compositionUpdatePolicy"},
				{"spec", "compositionUpdatePolicy"},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := getCrossplaneRefPaths(tt.apiVersion, tt.path...)

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("\n%s\ngetCrossplaneRefPaths(%q, %v): -want, +got:\n%s",
					tt.reason, tt.apiVersion, tt.path, diff)
			}
		})
	}
}

func TestDefaultCompositionClient_V2XRDWithV1StylePaths(t *testing.T) {
	// This test verifies the fix for issue #206 - v2 XRDs using v1-style composition paths.
	// When an XRD is v2, but the XR specifies composition fields at v1-style paths
	// (e.g., spec.compositionRef instead of spec.crossplane.compositionRef),
	// the client should still find them.
	matchingComp := tu.NewComposition("matching-comp").
		WithCompositeTypeRef("example.org/v1", "XExampleResource").
		Build()

	tests := map[string]struct {
		reason       string
		mockResource tu.MockResourceClient
		mockDef      tu.MockDefinitionClient
		compositions map[string]*apiextensionsv1.Composition
		res          *un.Unstructured
		wantComp     *apiextensionsv1.Composition
		wantErr      string
	}{
		"V2XRDWithV1StyleCompositionRef": {
			reason: "Should find composition when v2 XRD uses v1-style spec.compositionRef path",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV2XRDForXR().
				Build(),
			compositions: map[string]*apiextensionsv1.Composition{
				"matching-comp": matchingComp,
			},
			res: tu.NewResource("example.org/v1", "XExampleResource", "my-xr").
				// v1-style path: spec.compositionRef (not spec.crossplane.compositionRef)
				WithSpecField("compositionRef", map[string]any{
					"name": "matching-comp",
				}).
				Build(),
			wantComp: matchingComp,
		},
		"V2XRDWithV1StyleCompositionSelector": {
			reason: "Should find composition when v2 XRD uses v1-style spec.compositionSelector path",
			mockResource: *tu.NewMockResourceClient().
				WithSuccessfulInitialize().
				WithEmptyListResources().
				Build(),
			mockDef: *tu.NewMockDefinitionClient().
				WithSuccessfulInitialize().
				WithEmptyXRDsFetch().
				WithV2XRDForXR().
				Build(),
			compositions: map[string]*apiextensionsv1.Composition{
				"matching-comp": func() *apiextensionsv1.Composition {
					comp := tu.NewComposition("matching-comp").
						WithCompositeTypeRef("example.org/v1", "XExampleResource").
						Build()
					comp.SetLabels(map[string]string{
						"environment": "production",
					})

					return comp
				}(),
			},
			res: tu.NewResource("example.org/v1", "XExampleResource", "my-xr").
				// v1-style path: spec.compositionSelector (not spec.crossplane.compositionSelector)
				WithSpecField("compositionSelector", map[string]any{
					"matchLabels": map[string]any{
						"environment": "production",
					},
				}).
				Build(),
			wantComp: func() *apiextensionsv1.Composition {
				comp := tu.NewComposition("matching-comp").
					WithCompositeTypeRef("example.org/v1", "XExampleResource").
					Build()
				comp.SetLabels(map[string]string{
					"environment": "production",
				})

				return comp
			}(),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultCompositionClient{
				resourceClient:   &tt.mockResource,
				definitionClient: &tt.mockDef,
				revisionClient:   NewCompositionRevisionClient(&tt.mockResource, tu.TestLogger(t, false)),
				logger:           tu.TestLogger(t, false),
				compositions:     tt.compositions,
			}

			got, err := c.FindMatchingComposition(t.Context(), tt.res)

			if tt.wantErr != "" {
				if err == nil {
					t.Errorf("\n%s\nFindMatchingComposition(...): expected error containing %q but got none",
						tt.reason, tt.wantErr)

					return
				}

				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("\n%s\nFindMatchingComposition(...): expected error containing %q, got %q",
						tt.reason, tt.wantErr, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\nFindMatchingComposition(...): unexpected error: %v", tt.reason, err)
				return
			}

			if diff := cmp.Diff(tt.wantComp.Name, got.Name); diff != "" {
				t.Errorf("\n%s\nFindMatchingComposition(...): -want composition name, +got:\n%s",
					tt.reason, diff)
			}
		})
	}
}

func TestDefaultCompositionClient_getClaimTypeFromXRD(t *testing.T) {
	type args struct {
		xrd *un.Unstructured
	}

	type want struct {
		gvk    schema.GroupVersionKind
		errMsg string
	}

	tests := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"XRDWithClaimNames": {
			reason: "Should extract claim GVK from XRD with claimNames",
			args: args{
				xrd: tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "test-xrd").
					WithSpecField("group", "example.org").
					WithSpecField("names", map[string]any{
						"kind":     "XTestResource",
						"plural":   "xtestresources",
						"singular": "xtestresource",
					}).
					WithSpecField("claimNames", map[string]any{
						"kind":     "TestClaim",
						"plural":   "testclaims",
						"singular": "testclaim",
					}).
					WithSpecField("versions", []any{
						map[string]any{
							"name":          "v1alpha1",
							"referenceable": true,
							"served":        true,
						},
					}).
					Build(),
			},
			want: want{
				gvk: schema.GroupVersionKind{
					Group:   "example.org",
					Version: "v1alpha1",
					Kind:    "TestClaim",
				},
			},
		},
		"XRDWithoutClaimNames": {
			reason: "Should return empty GVK when XRD doesn't define claims",
			args: args{
				xrd: tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "test-xrd").
					WithSpecField("group", "example.org").
					WithSpecField("names", map[string]any{
						"kind":     "XTestResource",
						"plural":   "xtestresources",
						"singular": "xtestresource",
					}).
					WithSpecField("versions", []any{
						map[string]any{
							"name":          "v1alpha1",
							"referenceable": true,
							"served":        true,
						},
					}).
					Build(),
			},
			want: want{
				gvk: schema.GroupVersionKind{}, // empty GVK
			},
		},
		"XRDWithClaimNamesButMissingKind": {
			reason: "Should return error when claimNames exists but kind is missing",
			args: args{
				xrd: tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "test-xrd").
					WithSpecField("group", "example.org").
					WithSpecField("claimNames", map[string]any{
						"plural":   "testclaims",
						"singular": "testclaim",
					}).
					WithSpecField("versions", []any{
						map[string]any{
							"name":          "v1alpha1",
							"referenceable": true,
							"served":        true,
						},
					}).
					Build(),
			},
			want: want{
				errMsg: "missing kind",
			},
		},
		"XRDWithNoReferenceableVersion": {
			reason: "Should return error when no referenceable version found",
			args: args{
				xrd: tu.NewResource("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "test-xrd").
					WithSpecField("group", "example.org").
					WithSpecField("claimNames", map[string]any{
						"kind":     "TestClaim",
						"plural":   "testclaims",
						"singular": "testclaim",
					}).
					WithSpecField("versions", []any{
						map[string]any{
							"name":          "v1alpha1",
							"referenceable": false,
							"served":        true,
						},
					}).
					Build(),
			},
			want: want{
				errMsg: "no referenceable version",
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			client := &DefaultCompositionClient{
				logger: tu.TestLogger(t, false),
			}

			got, err := client.getClaimTypeFromXRD(tt.args.xrd)

			if tt.want.errMsg != "" {
				if err == nil {
					t.Errorf("\n%s\ngetClaimTypeFromXRD(): expected error containing %q but got none", tt.reason, tt.want.errMsg)
					return
				}

				if !strings.Contains(err.Error(), tt.want.errMsg) {
					t.Errorf("\n%s\ngetClaimTypeFromXRD(): expected error containing %q, got %q", tt.reason, tt.want.errMsg, err.Error())
				}

				return
			}

			if err != nil {
				t.Errorf("\n%s\ngetClaimTypeFromXRD(): unexpected error: %v", tt.reason, err)
				return
			}

			if diff := cmp.Diff(tt.want.gvk, got); diff != "" {
				t.Errorf("\n%s\ngetClaimTypeFromXRD(): -want GVK, +got GVK:\n%s", tt.reason, diff)
			}
		})
	}
}
