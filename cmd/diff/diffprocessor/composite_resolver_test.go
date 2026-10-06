package diffprocessor

import (
	"context"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	gcmp "github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"
)

// fakeClusterResources is a ResourceManager whose FetchCurrentObject answers from a fixed set of cluster
// objects, keyed by kind and name. A name in notFound is reported as not existing; a name in errs fails;
// any other name fails too, so a test notices a fetch it did not expect.
type fakeClusterResources struct {
	objects  map[string]*un.Unstructured
	notFound map[string]bool
	errs     map[string]error
}

func fetchKey(kind, name string) string { return kind + "/" + name }

func (f *fakeClusterResources) FetchCurrentObject(_ context.Context, _ *un.Unstructured, desired *un.Unstructured) (*un.Unstructured, bool, error) {
	key := fetchKey(desired.GetKind(), desired.GetName())

	switch {
	case f.objects[key] != nil:
		return f.objects[key].DeepCopy(), false, nil
	case f.notFound[key]:
		return nil, true, nil
	case f.errs[key] != nil:
		return nil, false, f.errs[key]
	default:
		return nil, false, errors.Errorf("unexpected fetch of %s", key)
	}
}

func (f *fakeClusterResources) UpdateOwnerRefs(context.Context, *un.Unstructured, *un.Unstructured) {}

func (f *fakeClusterResources) FetchObservedResources(context.Context, *cmp.Unstructured) ([]cpd.Unstructured, error) {
	return nil, nil
}

func TestCompositeResolver_Resolve(t *testing.T) {
	const (
		claimAPIVersion = "example.org/v1"
		generatedUID    = "generated-uid"
	)

	xrd := &un.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.crossplane.io/v1",
		"kind":       "CompositeResourceDefinition",
		"metadata":   map[string]any{"name": "xrs.example.org"},
		"spec": map[string]any{
			"group":      "example.org",
			"names":      map[string]any{"kind": "XR", "plural": "xrs"},
			"claimNames": map[string]any{"kind": "Claim", "plural": "claims"},
		},
	}}

	claimRef := map[string]any{"apiVersion": claimAPIVersion, "kind": "Claim", "name": "my-claim", "namespace": "ns"}
	resourceRef := map[string]any{"apiVersion": claimAPIVersion, "kind": "XR", "name": "my-claim-abc12"}

	authoredXR := tu.NewResource("example.org/v1", "XR", "my-xr").WithSpecField("field", "authored").BuildUComposite()

	clusterXR := tu.NewResource("example.org/v1", "XR", "my-xr").WithUID("cluster-uid").
		WithSpecField("field", "in-cluster").
		WithNestedField(map[string]any{"name": "comp"}, "spec", "crossplane", "compositionRef").
		WithNestedField(map[string]any{"name": "comp-abc1234"}, "spec", "crossplane", "compositionRevisionRef").
		WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
		Build()

	// The claim as supplied: it has no ref of its own, as a manifest kept in git would not.
	authoredClaim := tu.NewResource(claimAPIVersion, "Claim", "my-claim").InNamespace("ns").
		WithSpecField("field", "authored").BuildUComposite()

	clusterClaim := tu.NewResource(claimAPIVersion, "Claim", "my-claim").InNamespace("ns").WithUID("claim-uid").
		WithSpecField("field", "in-cluster").
		WithSpecField("resourceRef", resourceRef).
		Build()

	backingXR := tu.NewResource(claimAPIVersion, "XR", "my-claim-abc12").WithUID("backing-uid").
		WithLabels(map[string]string{LabelClaimName: "my-claim", LabelClaimNamespace: "ns"}).
		WithSpecField("field", "in-cluster").
		WithSpecField("claimRef", claimRef).
		WithSpecField("compositionRef", map[string]any{"name": "comp"}).
		WithSpecField("compositionRevisionRef", map[string]any{"name": "comp-abc1234"}).
		WithSpecField("compositionUpdatePolicy", "Automatic").
		WithSpecField("resourceRefs", []any{map[string]any{"apiVersion": "nop/v1", "kind": "Nop", "name": "nop"}}).
		Build()

	// synthesized is the backing XR ConvertClaimToXR gives authoredClaim under name.
	synthesized := func(name string) *un.Unstructured {
		return tu.NewResource(claimAPIVersion, "XR", name).WithUID(generatedUID).
			WithLabels(map[string]string{LabelClaimName: "my-claim", LabelClaimNamespace: "ns"}).
			WithSpecField("field", "authored").
			WithSpecField("claimRef", claimRef).
			Build()
	}

	tests := map[string]struct {
		reason    string
		authored  *cmp.Unstructured
		existing  *un.Unstructured
		resources *fakeClusterResources
		want      EffectiveComposite
		wantErr   bool
	}{
		"NewXRIsComposedAsAuthored": {
			reason:    "An XR with no cluster copy is composed as written.",
			authored:  authoredXR,
			resources: &fakeClusterResources{notFound: map[string]bool{"XR/my-xr": true}},
			want: EffectiveComposite{
				Authored:  authoredXR,
				Effective: authoredXR.GetUnstructured(),
			},
		},
		"ExistingXRInheritsWhatCrossplaneWrote": {
			reason:    "An existing XR's input says nothing about its composition, revision or policy, so it is reconciled with the cluster's, and with the cluster's UID.",
			authored:  authoredXR,
			resources: &fakeClusterResources{objects: map[string]*un.Unstructured{"XR/my-xr": clusterXR}},
			want: EffectiveComposite{
				Authored: authoredXR,
				Cluster:  clusterXR,
				Effective: tu.NewResource("example.org/v1", "XR", "my-xr").WithUID("cluster-uid").
					WithSpecField("field", "authored").
					WithNestedField(map[string]any{"name": "comp"}, "spec", "crossplane", "compositionRef").
					WithNestedField(map[string]any{"name": "comp-abc1234"}, "spec", "crossplane", "compositionRevisionRef").
					WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
			},
		},
		"SuppliedClusterCopyIsNotFetchedAgain": {
			reason:    "A nested XR's cluster copy is already known from its parent's observed resources.",
			authored:  authoredXR,
			existing:  clusterXR,
			resources: &fakeClusterResources{},
			want: EffectiveComposite{
				Authored: authoredXR,
				Cluster:  clusterXR,
				Effective: tu.NewResource("example.org/v1", "XR", "my-xr").WithUID("cluster-uid").
					WithSpecField("field", "authored").
					WithNestedField(map[string]any{"name": "comp"}, "spec", "crossplane", "compositionRef").
					WithNestedField(map[string]any{"name": "comp-abc1234"}, "spec", "crossplane", "compositionRevisionRef").
					WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
					Build(),
			},
		},
		"XRFetchErrorIsFatal": {
			reason:    "Resolving without the cluster copy could render the wrong revision.",
			authored:  authoredXR,
			resources: &fakeClusterResources{errs: map[string]error{"XR/my-xr": errors.New("boom")}},
			wantErr:   true,
		},
		"ClaimIsComposedAsItsBackingXR": {
			reason: "Crossplane composes a claim's backing XR, with the claim's spec synced in. The claim has " +
				"no ref and the backing XR is Automatic, so the backing XR keeps its own (#498).",
			authored: authoredClaim,
			resources: &fakeClusterResources{objects: map[string]*un.Unstructured{
				"Claim/my-claim":    clusterClaim,
				"XR/my-claim-abc12": backingXR,
			}},
			want: EffectiveComposite{
				Authored: authoredClaim,
				Cluster:  clusterClaim,
				Backing:  backingXR,
				Effective: tu.NewResource(claimAPIVersion, "XR", "my-claim-abc12").WithUID("backing-uid").
					WithLabels(map[string]string{LabelClaimName: "my-claim", LabelClaimNamespace: "ns"}).
					WithSpecField("field", "authored").
					WithSpecField("claimRef", claimRef).
					WithSpecField("compositionRef", map[string]any{"name": "comp"}).
					WithSpecField("compositionRevisionRef", map[string]any{"name": "comp-abc1234"}).
					WithSpecField("compositionUpdatePolicy", "Automatic").
					WithSpecField("resourceRefs", []any{map[string]any{"apiVersion": "nop/v1", "kind": "Nop", "name": "nop"}}).
					Build(),
				IsClaim: true,
			},
		},
		"BackingXRFetchErrorIsFatal": {
			reason:   "Rendering a claim without its backing XR would diff its composed resources against nothing (#533).",
			authored: authoredClaim,
			resources: &fakeClusterResources{
				objects: map[string]*un.Unstructured{"Claim/my-claim": clusterClaim},
				errs:    map[string]error{"XR/my-claim-abc12": apierrors.NewForbidden(schema.GroupResource{Resource: "xrs"}, "my-claim-abc12", errors.New("denied"))},
			},
			wantErr: true,
		},
		"MissingBackingXRIsSynthesizedUnderItsRefName": {
			reason:   "A resourceRef naming no XR is the XR the claim syncer would create, under that name.",
			authored: authoredClaim,
			resources: &fakeClusterResources{
				objects:  map[string]*un.Unstructured{"Claim/my-claim": clusterClaim},
				notFound: map[string]bool{"XR/my-claim-abc12": true},
			},
			want: EffectiveComposite{
				Authored:  authoredClaim,
				Cluster:   clusterClaim,
				Effective: synthesized("my-claim-abc12"),
				IsClaim:   true,
			},
		},
		"NewClaimIsSynthesizedUnderItsOwnName": {
			reason:    "A claim with no cluster copy is bound to a new XR, carrying a claimRef.",
			authored:  authoredClaim,
			resources: &fakeClusterResources{notFound: map[string]bool{"Claim/my-claim": true}},
			want: EffectiveComposite{
				Authored:  authoredClaim,
				Effective: synthesized("my-claim"),
				IsClaim:   true,
			},
		},
		"UnboundClaimIsSynthesizedUnderItsOwnName": {
			reason:   "A cluster claim Crossplane has not bound to an XR yet is treated as new.",
			authored: authoredClaim,
			resources: &fakeClusterResources{objects: map[string]*un.Unstructured{
				"Claim/my-claim": tu.NewResource(claimAPIVersion, "Claim", "my-claim").InNamespace("ns").Build(),
			}},
			want: EffectiveComposite{
				Authored:  authoredClaim,
				Cluster:   tu.NewResource(claimAPIVersion, "Claim", "my-claim").InNamespace("ns").Build(),
				Effective: synthesized("my-claim"),
				IsClaim:   true,
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defs := tu.NewMockDefinitionClient().
				WithIsClaimResource(func(_ context.Context, res *un.Unstructured) bool { return res.GetKind() == "Claim" }).
				WithXRDForClaim(xrd).
				Build()

			authoredBefore := tt.authored.DeepCopy()

			got, err := NewCompositeResolver(tt.resources, defs, tu.TestLogger(t, false)).Resolve(t.Context(), tt.authored, tt.existing)

			if tt.wantErr {
				if err == nil {
					t.Errorf("%s\nResolve(): want error, got none", tt.reason)
				}

				return
			}

			if err != nil {
				t.Fatalf("%s\nResolve(): %v", tt.reason, err)
			}

			// A synthesized backing XR gets a random UID; pin it so the rest can be compared exactly.
			if got.Backing == nil && got.IsClaim && got.Effective.GetUID() != "" {
				got.Effective.SetUID(generatedUID)
			}

			if diff := gcmp.Diff(tt.want, got); diff != "" {
				t.Errorf("%s\nResolve() (-want +got):\n%s", tt.reason, diff)
			}

			if diff := gcmp.Diff(authoredBefore, tt.authored); diff != "" {
				t.Errorf("%s\nResolve() modified the authored composite (-before +after):\n%s", tt.reason, diff)
			}
		})
	}
}

// TestInheritClusterFields covers how an XR inherits the fields Crossplane writes from its cluster copy.
// The load-bearing behaviour is the refusal: a field the input sets at either path is the input's
// opinion about it, so it always wins. That is how a Manual composite is moved to another revision.
func TestInheritClusterFields(t *testing.T) {
	tests := map[string]struct {
		reason   string
		authored *un.Unstructured
		cluster  *un.Unstructured
		want     *un.Unstructured
	}{
		"NoClusterCopyIsACopyOfTheInput": {
			reason:   "A new XR has nothing to inherit.",
			authored: tu.NewResource("v1", "XR", "xr").WithSpecField("coolField", "v").Build(),
			want:     tu.NewResource("v1", "XR", "xr").WithSpecField("coolField", "v").Build(),
		},
		"V2FieldsAreCopiedToTheV2Path": {
			reason: "A modern XR keeps its machinery under spec.crossplane, alongside any the input sets.",
			authored: tu.NewResource("v1", "XR", "xr").
				WithNestedField(map[string]any{"matchLabels": map[string]any{"a": "b"}}, "spec", "crossplane", "compositionSelector").
				Build(),
			cluster: tu.NewResource("v1", "XR", "xr").WithUID("uid").
				WithNestedField(map[string]any{"name": "comp"}, "spec", "crossplane", "compositionRef").
				WithNestedField(map[string]any{"name": "comp-rev"}, "spec", "crossplane", "compositionRevisionRef").
				WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
				Build(),
			want: tu.NewResource("v1", "XR", "xr").WithUID("uid").
				WithNestedField(map[string]any{"matchLabels": map[string]any{"a": "b"}}, "spec", "crossplane", "compositionSelector").
				WithNestedField(map[string]any{"name": "comp"}, "spec", "crossplane", "compositionRef").
				WithNestedField(map[string]any{"name": "comp-rev"}, "spec", "crossplane", "compositionRevisionRef").
				WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
				Build(),
		},
		"V1FieldsAreCopiedToTheV1Path": {
			reason:   "A legacy XR keeps its machinery directly under spec.",
			authored: tu.NewResource("v1", "XR", "xr").WithSpecField("coolField", "v").Build(),
			cluster: tu.NewResource("v1", "XR", "xr").WithUID("uid").
				WithSpecField("compositionRef", map[string]any{"name": "comp"}).
				WithSpecField("compositionRevisionRef", map[string]any{"name": "comp-rev"}).
				WithSpecField("compositionUpdatePolicy", "Automatic").
				Build(),
			want: tu.NewResource("v1", "XR", "xr").WithUID("uid").
				WithSpecField("coolField", "v").
				WithSpecField("compositionRef", map[string]any{"name": "comp"}).
				WithSpecField("compositionRevisionRef", map[string]any{"name": "comp-rev"}).
				WithSpecField("compositionUpdatePolicy", "Automatic").
				Build(),
		},
		"BothPathsAreCopiedWhenTheClusterHasBoth": {
			reason:   "Pathological, but render must see what the cluster holds at whichever path is read.",
			authored: tu.NewResource("v1", "XR", "xr").Build(),
			cluster: tu.NewResource("v1", "XR", "xr").
				WithSpecField("compositionRevisionRef", map[string]any{"name": "v1-rev"}).
				WithNestedField(map[string]any{"name": "v2-rev"}, "spec", "crossplane", "compositionRevisionRef").
				Build(),
			want: tu.NewResource("v1", "XR", "xr").
				WithSpecField("compositionRevisionRef", map[string]any{"name": "v1-rev"}).
				WithNestedField(map[string]any{"name": "v2-rev"}, "spec", "crossplane", "compositionRevisionRef").
				Build(),
		},
		"InputAtTheSamePathWins": {
			reason: "The input's own pin and policy: pointing a Manual composite at another revision must keep working.",
			authored: tu.NewResource("v1", "XR", "xr").
				WithNestedField(map[string]any{"name": "user-rev"}, "spec", "crossplane", "compositionRevisionRef").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				Build(),
			cluster: tu.NewResource("v1", "XR", "xr").
				WithNestedField(map[string]any{"name": "cluster-rev"}, "spec", "crossplane", "compositionRevisionRef").
				WithNestedField("Manual", "spec", "crossplane", "compositionUpdatePolicy").
				Build(),
			want: tu.NewResource("v1", "XR", "xr").
				WithNestedField(map[string]any{"name": "user-rev"}, "spec", "crossplane", "compositionRevisionRef").
				WithNestedField("Automatic", "spec", "crossplane", "compositionUpdatePolicy").
				Build(),
		},
		"InputAtTheOtherPathWins": {
			reason: "A field set at either path is an opinion about it, so the cluster's is not layered on top.",
			authored: tu.NewResource("v1", "XR", "xr").
				WithSpecField("compositionRef", map[string]any{"name": "user-comp"}).
				Build(),
			cluster: tu.NewResource("v1", "XR", "xr").
				WithNestedField(map[string]any{"name": "cluster-comp"}, "spec", "crossplane", "compositionRef").
				Build(),
			want: tu.NewResource("v1", "XR", "xr").
				WithSpecField("compositionRef", map[string]any{"name": "user-comp"}).
				Build(),
		},
		"OnlyCrossplaneWrittenFieldsAreInherited": {
			reason:   "A field the input drops is gone, unless Crossplane wrote it: an apply removes what it no longer sets.",
			authored: tu.NewResource("v1", "XR", "xr").WithSpecField("coolField", "v").Build(),
			cluster: tu.NewResource("v1", "XR", "xr").WithUID("uid").
				WithSpecField("coolField", "old").
				WithSpecField("removedField", "old").
				WithNestedField(map[string]any{"matchLabels": map[string]any{"a": "b"}}, "spec", "crossplane", "compositionSelector").
				Build(),
			want: tu.NewResource("v1", "XR", "xr").WithUID("uid").WithSpecField("coolField", "v").Build(),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			authoredBefore := tt.authored.DeepCopy()

			var clusterBefore *un.Unstructured
			if tt.cluster != nil {
				clusterBefore = tt.cluster.DeepCopy()
			}

			got := inheritClusterFields(tt.authored, tt.cluster)

			if diff := gcmp.Diff(tt.want, got); diff != "" {
				t.Errorf("%s\ninheritClusterFields() (-want +got):\n%s", tt.reason, diff)
			}

			// The copy must be deep: later writes to the effective XR (comp seeding a predicted name, say)
			// must reach neither the input nor the cluster's object.
			SetCompositionRevisionRefName(got, "mutated")

			if diff := gcmp.Diff(authoredBefore, tt.authored); diff != "" {
				t.Errorf("%s\ninheritClusterFields() aliased the input (-before +after):\n%s", tt.reason, diff)
			}

			if diff := gcmp.Diff(clusterBefore, tt.cluster); diff != "" {
				t.Errorf("%s\ninheritClusterFields() aliased the cluster copy (-before +after):\n%s", tt.reason, diff)
			}
		})
	}
}

// TestSyncClaimSpec covers the spec Crossplane's claim syncer gives a claim's backing XR. The claim's
// spec is the source of truth for what the claim authors; the XR controller's own fields survive the
// sync; and compositionRevisionRef travels from the claim only under a Manual policy.
func TestSyncClaimSpec(t *testing.T) {
	claimRef := map[string]any{"apiVersion": "example.org/v1", "kind": "Claim", "name": "my-claim", "namespace": "ns"}

	claim := func(spec map[string]any) *un.Unstructured {
		return tu.NewResource("example.org/v1", "Claim", "my-claim").InNamespace("ns").WithSpec(spec).Build()
	}

	tests := map[string]struct {
		reason      string
		claim       *un.Unstructured
		backingSpec map[string]any
		manual      bool
		want        map[string]any
	}{
		"FieldsRemovedFromTheClaimAreRemoved": {
			reason:      "The claim controller owns what it propagated, so a field the claim drops leaves the XR.",
			claim:       claim(map[string]any{"newField": "new"}),
			backingSpec: map[string]any{"newField": "new", "deprecatedField": "old", "claimRef": claimRef},
			want:        map[string]any{"newField": "new", "claimRef": claimRef},
		},
		"XRControllerFieldsSurvive": {
			reason: "compositionRef, compositionRevisionRef, compositionUpdatePolicy, resourceRefs and " +
				"writeConnectionSecretToRef are written by the XR controller (or defaulted), so the claim's apply leaves them.",
			claim: claim(map[string]any{"field": "v"}),
			backingSpec: map[string]any{
				"field":                      "old",
				"compositionRef":             map[string]any{"name": "comp"},
				"compositionRevisionRef":     map[string]any{"name": "comp-rev"},
				"compositionUpdatePolicy":    "Automatic",
				"resourceRefs":               []any{map[string]any{"kind": "Nop", "name": "nop"}},
				"writeConnectionSecretToRef": map[string]any{"name": "uid", "namespace": "crossplane-system"},
			},
			want: map[string]any{
				"field":                      "v",
				"claimRef":                   claimRef,
				"compositionRef":             map[string]any{"name": "comp"},
				"compositionRevisionRef":     map[string]any{"name": "comp-rev"},
				"compositionUpdatePolicy":    "Automatic",
				"resourceRefs":               []any{map[string]any{"kind": "Nop", "name": "nop"}},
				"writeConnectionSecretToRef": map[string]any{"name": "uid", "namespace": "crossplane-system"},
			},
		},
		"PropagatedFieldsComeFromTheClaim": {
			reason: "compositionRef, compositionSelector, compositionUpdatePolicy and compositionRevisionSelector propagate claim to XR.",
			claim: claim(map[string]any{
				"compositionRef":              map[string]any{"name": "new-comp"},
				"compositionSelector":         map[string]any{"matchLabels": map[string]any{"a": "b"}},
				"compositionUpdatePolicy":     "Manual",
				"compositionRevisionSelector": map[string]any{"matchLabels": map[string]any{"c": "d"}},
			}),
			backingSpec: map[string]any{"compositionRef": map[string]any{"name": "old-comp"}, "compositionUpdatePolicy": "Automatic"},
			want: map[string]any{
				"claimRef":                    claimRef,
				"compositionRef":              map[string]any{"name": "new-comp"},
				"compositionSelector":         map[string]any{"matchLabels": map[string]any{"a": "b"}},
				"compositionUpdatePolicy":     "Manual",
				"compositionRevisionSelector": map[string]any{"matchLabels": map[string]any{"c": "d"}},
			},
		},
		"CompositionSelectorTheClaimDropsIsRemoved": {
			reason:      "The selector is the claim's to propagate, not the XR controller's to write.",
			claim:       claim(map[string]any{"field": "v"}),
			backingSpec: map[string]any{"compositionSelector": map[string]any{"matchLabels": map[string]any{"a": "b"}}},
			want:        map[string]any{"field": "v", "claimRef": claimRef},
		},
		"ClaimOnlyAndXRMachineryFieldsAreStripped": {
			reason: "A claim's own fields mean nothing on its XR, and the XR machinery a claim could smuggle in is the XR controller's.",
			claim: claim(map[string]any{
				"field":                      "v",
				"resourceRef":                map[string]any{"name": "x"},
				"compositeDeletePolicy":      "Background",
				"writeConnectionSecretToRef": map[string]any{"name": "claim-secret"},
				"resourceRefs":               []any{map[string]any{"name": "smuggled"}},
				"claimRef":                   map[string]any{"name": "smuggled"},
			}),
			backingSpec: map[string]any{"writeConnectionSecretToRef": map[string]any{"name": "uid", "namespace": "crossplane-system"}},
			want: map[string]any{
				"field":                      "v",
				"claimRef":                   claimRef,
				"writeConnectionSecretToRef": map[string]any{"name": "uid", "namespace": "crossplane-system"},
			},
		},
		"AutomaticClaimRevisionRefIsStripped": {
			reason: "Under Automatic the XR controller owns the ref, so the backing XR keeps its own, whatever the claim carries (#498).",
			claim:  claim(map[string]any{"compositionRevisionRef": map[string]any{"name": "claim-rev"}}),
			backingSpec: map[string]any{
				"compositionRevisionRef": map[string]any{"name": "xr-rev"},
			},
			want: map[string]any{"claimRef": claimRef, "compositionRevisionRef": map[string]any{"name": "xr-rev"}},
		},
		"ManualClaimRevisionRefIsPropagated": {
			reason:      "Under Manual the claim is authoritative for the ref.",
			claim:       claim(map[string]any{"compositionRevisionRef": map[string]any{"name": "claim-rev"}}),
			backingSpec: map[string]any{"compositionRevisionRef": map[string]any{"name": "xr-rev"}},
			manual:      true,
			want:        map[string]any{"claimRef": claimRef, "compositionRevisionRef": map[string]any{"name": "claim-rev"}},
		},
		"ManualClaimWithoutARefKeepsTheBackingXRsPin": {
			reason:      "The claim syncer copies the ref back to the claim only under Automatic, so a Manual claim often carries none.",
			claim:       claim(map[string]any{"field": "v"}),
			backingSpec: map[string]any{"compositionRevisionRef": map[string]any{"name": "xr-rev"}},
			manual:      true,
			want:        map[string]any{"field": "v", "claimRef": claimRef, "compositionRevisionRef": map[string]any{"name": "xr-rev"}},
		},
		"NewBackingXRHasOnlyWhatTheClaimPropagates": {
			reason: "An XR that does not exist yet has no fields of its own to keep.",
			claim:  claim(map[string]any{"field": "v", "compositionRevisionRef": map[string]any{"name": "claim-rev"}}),
			want:   map[string]any{"field": "v", "claimRef": claimRef},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := syncClaimSpec(tt.claim, tt.backingSpec, tt.manual)
			if err != nil {
				t.Fatalf("%s\nsyncClaimSpec(): %v", tt.reason, err)
			}

			if diff := gcmp.Diff(tt.want, got); diff != "" {
				t.Errorf("%s\nsyncClaimSpec() (-want +got):\n%s", tt.reason, diff)
			}
		})
	}
}
