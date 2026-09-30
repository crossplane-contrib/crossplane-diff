package diffprocessor

import (
	"bytes"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	gcmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// errMessage is err's message, or "" for none, so verdicts compare as plain values.
func errMessage(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

func TestCheckInputs(t *testing.T) {
	xr := func(name string) *tu.ResourceBuilder { return tu.NewResource("example.org/v1", "XR", name) }

	type verdict struct {
		Duplicate bool
		Err       string
	}

	conflict := func(position, other int, name string) string {
		return errMessage(conflictingInputError(position, other, xr(name).Build()))
	}

	tests := map[string]struct {
		reason       string
		inputs       []*un.Unstructured
		want         []verdict
		wantWarnings []string
	}{
		"DistinctInputsPass": {
			reason: "Inputs naming different objects are each rendered.",
			inputs: []*un.Unstructured{xr("a").Build(), xr("b").Build()},
			want:   []verdict{{}, {}},
		},
		"IdenticalDuplicateIsDropped": {
			reason:       "An input identical to an earlier one has one clear intent, so it is dropped with a warning.",
			inputs:       []*un.Unstructured{xr("a").WithSpecField("f", "v").Build(), xr("a").WithSpecField("f", "v").Build()},
			want:         []verdict{{}, {Duplicate: true}},
			wantWarnings: []string{"Ignoring a duplicate input: it is identical to an earlier one"},
		},
		"SameObjectWithDifferentContentFailsBoth": {
			reason: "Neither of two differing definitions can be preferred, so each fails, naming the other.",
			inputs: []*un.Unstructured{xr("a").WithSpecField("f", "1").Build(), xr("a").WithSpecField("f", "2").Build()},
			want:   []verdict{{Err: conflict(1, 2, "a")}, {Err: conflict(2, 1, "a")}},
		},
		"SameObjectAtAnotherAPIVersionIsStillTheSameObject": {
			reason: "Identity is version-independent, so one object at two versions is a conflict, not two objects.",
			inputs: []*un.Unstructured{xr("a").Build(), tu.NewResource("example.org/v2", "XR", "a").Build()},
			want:   []verdict{{Err: conflict(1, 2, "a")}, {Err: conflict(2, 1, "a")}},
		},
		"SameNameInAnotherNamespaceIsAnotherObject": {
			reason: "Namespace is part of identity.",
			inputs: []*un.Unstructured{xr("a").InNamespace("one").Build(), xr("a").InNamespace("two").Build()},
			want:   []verdict{{}, {}},
		},
		"GenerateNameOnlyInputsAreNotJudged": {
			reason: "An input with no name yet has no identity: two identical ones are two XRs, not a duplicate.",
			inputs: []*un.Unstructured{xr("").WithGenerateName("g-").Build(), xr("").WithGenerateName("g-").Build()},
			want:   []verdict{{}, {}},
		},
		"LaterInputIsJudgedAgainstTheFirst": {
			reason: "A third input identical to the first is a duplicate of it, even after a conflicting second.",
			inputs: []*un.Unstructured{
				xr("a").WithSpecField("f", "1").Build(),
				xr("a").WithSpecField("f", "2").Build(),
				xr("a").WithSpecField("f", "1").Build(),
			},
			want:         []verdict{{Err: conflict(1, 2, "a")}, {Err: conflict(2, 1, "a")}, {Duplicate: true}},
			wantWarnings: []string{"Ignoring a duplicate input: it is identical to an earlier one"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger := NewWarningLogger(tu.TestLogger(t, false), &bytes.Buffer{})

			checks := checkInputs(logger, tt.inputs)

			got := make([]verdict, 0, len(checks))
			for _, c := range checks {
				got = append(got, verdict{Duplicate: c.duplicate, Err: errMessage(c.err)})
			}

			if diff := gcmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s\ncheckInputs(...): -want, +got:\n%s", tt.reason, diff)
			}

			warnings := logger.Warnings()

			gotWarnings := make([]string, 0, len(warnings))
			for _, w := range warnings {
				gotWarnings = append(gotWarnings, w.Message)
			}

			if diff := gcmp.Diff(tt.wantWarnings, gotWarnings, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s\ncheckInputs(...) warnings: -want, +got:\n%s", tt.reason, diff)
			}
		})
	}
}

func TestRejectManagedInputs(t *testing.T) {
	xr := func(name string) *tu.ResourceBuilder { return tu.NewResource("example.org/v1", "XR", name) }
	claim := func(name string) *tu.ResourceBuilder {
		return tu.NewResource("example.org/v1", "Claim", name).InNamespace("default")
	}
	resourceRef := map[string]any{"apiVersion": "example.org/v1", "kind": "XR", "name": "backing"}
	claimRef := map[string]any{"apiVersion": "example.org/v1", "kind": "Claim", "name": "my-claim", "namespace": "default"}
	claimLabels := func(namespace string) map[string]string {
		return map[string]string{LabelClaimName: "my-claim", LabelClaimNamespace: namespace}
	}

	const (
		composedErr = "XR/child is composed by input XR/parent, whose composition writes it; supplying it as an " +
			"input too gives it a second writer — pass XR/parent only"
		claimErr = "XR/backing is the XR bound to claim Claim/my-claim, which Crossplane's claim controller writes; " +
			"supplying it as an input too gives it a second writer — pass the claim, not its XR"
	)

	// input is one group as the processor would hand it over.
	type input struct {
		res *un.Unstructured
		// cluster is the cluster copy carried by the input's own diff, if any.
		cluster *un.Unstructured
		// rendered is every resource key the input's render produced.
		rendered []string
		// failedRender marks the input's own render as failed; rejected marks it failed before rendering.
		failedRender, rejected bool
	}

	tests := map[string]struct {
		reason string
		inputs []input
		want   map[int]string
	}{
		"UnrelatedInputsPass": {
			reason: "Inputs that neither compose nor bind one another are all valid.",
			inputs: []input{{res: xr("a").Build()}, {res: xr("b").Build()}},
		},
		"ChildComposedByAParentInputIsRejected": {
			reason: "A child in the parent's rendered keys has a second writer if it is also an input.",
			inputs: []input{
				{res: xr("parent").Build(), rendered: []string{dt.MakeDiffKey("example.org/v1", "XR", "", "child")}},
				{res: xr("child").Build()},
			},
			want: map[int]string{1: composedErr},
		},
		"ChildRenderedAtAnotherAPIVersionIsStillRejected": {
			reason: "The parent may render the child at another version; it is the same object.",
			inputs: []input{
				{res: xr("parent").Build(), rendered: []string{dt.MakeDiffKey("example.org/v2", "XR", "", "child")}},
				{res: xr("child").Build()},
			},
			want: map[int]string{1: composedErr},
		},
		"ManagedInputIsJudgedDespiteItsOwnRenderFailure": {
			reason: "The verdict supersedes the managed input's own render failure, which is moot.",
			inputs: []input{
				{res: xr("parent").Build(), rendered: []string{dt.MakeDiffKey("example.org/v1", "XR", "", "child")}},
				{res: xr("child").Build(), failedRender: true},
			},
			want: map[int]string{1: composedErr},
		},
		"AFailedManagerManagesNothing": {
			reason: "What a manager manages is read from its render, so one that failed to render cannot reject.",
			inputs: []input{
				{res: xr("parent").Build(), rendered: []string{dt.MakeDiffKey("example.org/v1", "XR", "", "child")}, failedRender: true},
				{res: xr("child").Build()},
			},
		},
		"InputRejectedBeforeRenderingIsNotJudged": {
			reason: "An input already rejected by checkInputs keeps that error.",
			inputs: []input{
				{res: xr("parent").Build(), rendered: []string{dt.MakeDiffKey("example.org/v1", "XR", "", "child")}},
				{res: xr("child").Build(), rejected: true},
			},
		},
		"GenerateNameOnlyInputIsNotJudged": {
			reason: "An input with no name yet has no identity to be managed by.",
			inputs: []input{
				{res: xr("parent").Build(), rendered: []string{dt.MakeDiffKey("example.org/v1", "XR", "", "")}},
				{res: xr("").WithGenerateName("child-").Build()},
			},
		},
		"AnInputWithNoNameIsNobodysClaim": {
			reason: "A generateName-only input must not match an XR's absent claimRef, which is also empty.",
			inputs: []input{{res: xr("named").Build()}, {res: xr("").WithGenerateName("gen-").Build()}},
		},
		"BackingXRNamedByTheClaimsClusterResourceRef": {
			reason: "A bound claim's cluster copy names its XR in spec.resourceRef.",
			inputs: []input{
				{res: claim("my-claim").Build(), cluster: claim("my-claim").WithSpecField("resourceRef", resourceRef).Build()},
				{res: xr("backing").Build()},
			},
			want: map[int]string{1: claimErr},
		},
		"BackingXRNamedByTheClaimsInputResourceRef": {
			reason: "A claim exported from the cluster carries spec.resourceRef in the input itself.",
			inputs: []input{
				{res: claim("my-claim").WithSpecField("resourceRef", resourceRef).Build()},
				{res: xr("backing").Build()},
			},
			want: map[int]string{1: claimErr},
		},
		"BackingXRWhoseClaimRefNamesTheClaim": {
			reason: "The XR's side of the binding is enough on its own.",
			inputs: []input{
				{res: claim("my-claim").Build()},
				{res: xr("backing").WithSpecField("claimRef", claimRef).Build()},
			},
			want: map[int]string{1: claimErr},
		},
		"BackingXRWhoseClaimLabelsNameTheClaim": {
			reason: "Crossplane stamps the claim's name and namespace on the XR as labels.",
			inputs: []input{
				{res: claim("my-claim").Build()},
				{res: xr("backing").Build(), cluster: xr("backing").WithLabels(claimLabels("default")).Build()},
			},
			want: map[int]string{1: claimErr},
		},
		"ClaimLabelsForAnotherNamespaceDoNotMatch": {
			reason: "A claim of the same name in another namespace is a different claim.",
			inputs: []input{
				{res: claim("my-claim").Build()},
				{res: xr("backing").WithLabels(claimLabels("elsewhere")).Build()},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var (
				groups   = make([]dt.XRDiffGroup, len(tt.inputs))
				checks   = make([]inputCheck, len(tt.inputs))
				rendered = make([]map[string]bool, len(tt.inputs))
				failed   = make([]error, len(tt.inputs))
			)

			for i, in := range tt.inputs {
				checks[i] = inputCheck{res: in.res}
				if in.rejected {
					checks[i].err = errors.New("rejected before rendering")
				}

				if in.failedRender {
					failed[i] = errors.New("render failed")
				}

				if in.cluster != nil {
					groups[i].Diffs = map[string]*dt.ResourceDiff{
						dt.MakeDiffKeyFromResource(in.res): {Current: dt.ResourceViews{Raw: in.cluster}},
					}
				}

				rendered[i] = map[string]bool{}
				for _, key := range in.rendered {
					rendered[i][key] = true
				}
			}

			got := map[int]string{}
			for i, err := range rejectManagedInputs(groups, checks, rendered, failed) {
				got[i] = errMessage(err)
			}

			if diff := gcmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s\nrejectManagedInputs(...): -want, +got:\n%s", tt.reason, diff)
			}
		})
	}
}

// TestBundleInputValidatorVerdicts drives the default validator through its stages as PerformDiff does,
// pinning what it owns beyond its rules: dropping duplicates from ToRender, and the precedence of a
// managed-input verdict over that input's own render failure.
func TestBundleInputValidatorVerdicts(t *testing.T) {
	xr := func(name string) *un.Unstructured { return tu.NewResource("example.org/v1", "XR", name).Build() }
	childKey := dt.MakeDiffKey("example.org/v1", "XR", "", "child")

	const composedErr = "XR/child is composed by input XR/parent, whose composition writes it; supplying it as an " +
		"input too gives it a second writer — pass XR/parent only"

	conflict34 := conflictingInputError(3, 4, xr("b")).Error()
	conflict43 := conflictingInputError(4, 3, xr("b")).Error()

	// render is one input's recorded render: its rendered keys and its own failure.
	type render struct {
		keys []string
		err  string
	}

	type want struct {
		ToRender []string // name, or "name: err" for an input rejected before rendering
		Verdicts []string
	}

	tests := map[string]struct {
		reason  string
		inputs  []*un.Unstructured
		renders map[int]render // by ToRender index; an input rejected before rendering is not rendered
		want    want
	}{
		"ManagedVerdictReplacesTheManagedInputsRenderFailure": {
			reason: "A managed input whose own render failed reports why it should not have been passed, not the render failure.",
			inputs: []*un.Unstructured{xr("parent"), xr("child")},
			renders: map[int]render{
				0: {keys: []string{childKey}},
				1: {err: "render failed"},
			},
			want: want{ToRender: []string{"parent", "child"}, Verdicts: []string{"", composedErr}},
		},
		"AnUnmanagedRenderFailureIsKept": {
			reason:  "Without a managed-input verdict, a group's final error is its own render failure.",
			inputs:  []*un.Unstructured{xr("a"), xr("b")},
			renders: map[int]render{0: {}, 1: {err: "render failed"}},
			want:    want{ToRender: []string{"a", "b"}, Verdicts: []string{"", "render failed"}},
		},
		"DuplicateIsDroppedAndAConflictIsRejectedBeforeRendering": {
			reason: "An identical duplicate is not handed back to render; the same object twice differently is, " +
				"rejected, and that rejection is its verdict.",
			inputs: []*un.Unstructured{
				xr("a"), xr("a"),
				tu.NewResource("example.org/v1", "XR", "b").WithSpecField("v", "1").Build(),
				tu.NewResource("example.org/v1", "XR", "b").WithSpecField("v", "2").Build(),
			},
			renders: map[int]render{0: {}},
			want: want{
				ToRender: []string{"a", "b: " + conflict34, "b: " + conflict43},
				Verdicts: []string{"", conflict34, conflict43},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			v := NewBundleInputValidator(tu.TestLogger(t, false), tt.inputs)

			got := want{}

			toRender := v.ToRender()
			for i, in := range toRender {
				entry := in.Resource.GetName()
				if in.Err != nil {
					entry += ": " + in.Err.Error()
				}

				got.ToRender = append(got.ToRender, entry)

				r, ok := tt.renders[i]
				if !ok {
					continue
				}

				keys := map[string]bool{}
				for _, k := range r.keys {
					keys[k] = true
				}

				var err error
				if r.err != "" {
					err = errors.New(r.err)
				}

				v.RecordRender(i, keys, err)
			}

			for _, err := range v.Verdicts(make([]dt.XRDiffGroup, len(toRender))) {
				got.Verdicts = append(got.Verdicts, errMessage(err))
			}

			if diff := gcmp.Diff(tt.want, got); diff != "" {
				t.Errorf("%s\nbundleInputValidator: -want, +got:\n%s", tt.reason, diff)
			}
		})
	}
}

func TestRenderOverlapErrors(t *testing.T) {
	const key = "example.org/v1/Bucket/default/shared"

	// bucket is the shared resource as one render produces it: diffed as diffType, with spec.value set
	// to value, controlled by the XR named controller ("" for none) under uid. Clean mirrors
	// cleanupForDiff, which strips ownerReferences before anything is compared.
	bucket := func(diffType dt.DiffType, controller, uid, value string) *dt.ResourceDiff {
		b := tu.NewResource("example.org/v1", "Bucket", "shared").InNamespace("default").WithSpecField("value", value)
		if controller != "" {
			b = b.WithControllerReference("XR", controller, "example.org/v1", uid)
		}

		raw := b.Build()
		clean := raw.DeepCopy()
		clean.SetOwnerReferences(nil)

		return &dt.ResourceDiff{
			Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Bucket"},
			Namespace:    "default",
			ResourceName: "shared",
			DiffType:     diffType,
			Desired:      dt.ResourceViews{Raw: raw, Clean: clean},
		}
	}

	group := func(name string, diffs map[string]*dt.ResourceDiff) dt.XRDiffGroup {
		return dt.XRDiffGroup{XR: corev1.ObjectReference{APIVersion: "example.org/v1", Kind: "XR", Name: name}, Diffs: diffs}
	}

	sharing := func(names ...string) func(func(i int) *dt.ResourceDiff) []dt.XRDiffGroup {
		return func(diff func(i int) *dt.ResourceDiff) []dt.XRDiffGroup {
			groups := make([]dt.XRDiffGroup, len(names))
			for i, n := range names {
				groups[i] = group(n, map[string]*dt.ResourceDiff{key: diff(i)})
			}

			return groups
		}
	}

	generated := func(groups []dt.XRDiffGroup) []dt.XRDiffGroup {
		for i := range groups {
			groups[i].NameGenerated = true
		}

		return groups
	}

	two := sharing("a", "b")

	const (
		contention = `cannot combine diffs: resource "example.org/v1/Bucket/default/shared" would be controlled by more ` +
			`than one XR (XR/a, XR/b); Crossplane gives it to whichever of them creates it first, and the other fails to ` +
			`reconcile it, so no diff predicts applying these inputs together — diff them separately`
		disagreement = `cannot combine diffs: inputs XR/a, XR/b both produce resource "example.org/v1/Bucket/default/shared", ` +
			`but differently, so no single diff is correct for both — diff them separately`
		placeholder = `cannot combine diffs: inputs XR/g-(generated), XR/g-(generated) both produce resource ` +
			`"example.org/v1/Bucket/default/shared" only because crossplane-diff gives XRs that share a generateName the ` +
			`same placeholder name; the API server would name them differently, so they cannot be told apart here — diff ` +
			`them separately`
	)

	tests := map[string]struct {
		reason string
		groups []dt.XRDiffGroup
		want   []string
	}{
		"KeysProducedOnceAreFine": {
			reason: "Only a key produced by more than one render can overlap.",
			groups: []dt.XRDiffGroup{
				group("a", map[string]*dt.ResourceDiff{key: bucket(dt.DiffTypeModified, "a", "1", "v")}),
				group("b", map[string]*dt.ResourceDiff{"example.org/v1/Bucket/default/other": bucket(dt.DiffTypeModified, "b", "2", "v")}),
			},
		},
		"DifferentControllersContendEvenWhenIdentical": {
			reason: "Crossplane gives the object to whichever XR creates it first; the renderings cannot change that.",
			groups: two(func(i int) *dt.ResourceDiff { return bucket(dt.DiffTypeModified, []string{"a", "b"}[i], "u", "v") }),
			want:   []string{contention},
		},
		"AllEqualIsTolerated": {
			reason: "Equal diffs appear in no rendered view, so merging them loses nothing.",
			groups: two(func(i int) *dt.ResourceDiff { return bucket(dt.DiffTypeEqual, []string{"a", "b"}[i], "u", "v") }),
		},
		"SameControllerIdenticalIsMerged": {
			reason: "One change reached twice through one controller; the controller is compared by name, not UID.",
			groups: two(func(i int) *dt.ResourceDiff { return bucket(dt.DiffTypeModified, "parent", []string{"1", "2"}[i], "v") }),
		},
		"SameControllerDifferentRenderingsDisagree": {
			reason: "No single diff is right for both.",
			groups: two(func(i int) *dt.ResourceDiff { return bucket(dt.DiffTypeModified, "parent", "u", []string{"1", "2"}[i]) }),
			want:   []string{disagreement},
		},
		"UncontrolledDifferentRenderingsDisagree": {
			reason: "The same holds for an object no XR controls.",
			groups: two(func(i int) *dt.ResourceDiff { return bucket(dt.DiffTypeModified, "", "", []string{"1", "2"}[i]) }),
			want:   []string{disagreement},
		},
		"RemovalIsJudgedByItsCurrentController": {
			reason: "A removal has no rendered form, so its controller is read from the current object.",
			groups: two(func(i int) *dt.ResourceDiff {
				d := bucket(dt.DiffTypeRemoved, []string{"a", "b"}[i], "u", "v")
				d.Current, d.Desired = d.Desired, dt.ResourceViews{}

				return d
			}),
			want: []string{contention},
		},
		"SharedGenerateNameCannotBeToldApart": {
			reason: "Both render under one placeholder, so even identical renderings must not merge.",
			groups: generated(sharing("g-(generated)", "g-(generated)")(func(int) *dt.ResourceDiff {
				return bucket(dt.DiffTypeModified, "parent", "u", "v")
			})),
			want: []string{placeholder},
		},
		"DifferentGenerateNamesAreJudgedOnTheirMerits": {
			reason: "Different generateNames get different placeholders, so the overlap is an ordinary one.",
			groups: generated(sharing("g-(generated)", "h-(generated)")(func(int) *dt.ResourceDiff {
				return bucket(dt.DiffTypeModified, "parent", "u", "v")
			})),
		},
		"OverlapsAreReportedInKeyOrder": {
			reason: "Map iteration is unordered; the reported errors must not be.",
			groups: []dt.XRDiffGroup{
				group("a", map[string]*dt.ResourceDiff{
					"example.org/v1/Bucket/default/z": bucket(dt.DiffTypeModified, "", "", "1"),
					key:                               bucket(dt.DiffTypeModified, "", "", "1"),
				}),
				group("b", map[string]*dt.ResourceDiff{
					"example.org/v1/Bucket/default/z": bucket(dt.DiffTypeModified, "", "", "2"),
					key:                               bucket(dt.DiffTypeModified, "", "", "2"),
				}),
			},
			want: []string{
				disagreement,
				`cannot combine diffs: inputs XR/a, XR/b both produce resource "example.org/v1/Bucket/default/z", ` +
					`but differently, so no single diff is correct for both — diff them separately`,
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			errs := renderOverlapErrors(tt.groups)

			got := make([]string, 0, len(errs))
			for _, err := range errs {
				got = append(got, errMessage(err))
			}

			if diff := gcmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s\nrenderOverlapErrors(...): -want, +got:\n%s", tt.reason, diff)
			}
		})
	}
}

func TestDiffKeyIdentity(t *testing.T) {
	tests := map[string]struct {
		key  string
		want string
	}{
		"GroupedAPIVersion":           {key: "example.org/v1/Bucket/default/b", want: "example.org/Bucket/default/b"},
		"CoreAPIVersion":              {key: "v1/ConfigMap/default/cm", want: "/ConfigMap/default/cm"},
		"ClusterScoped":               {key: "example.org/v1/Bucket//b", want: "example.org/Bucket//b"},
		"VersionDoesNotAffectIt":      {key: "example.org/v2/Bucket/default/b", want: "example.org/Bucket/default/b"},
		"TooFewSegmentsHasNoIdentity": {key: "Bucket/b", want: ""},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := diffKeyIdentity(tt.key); got != tt.want {
				t.Errorf("diffKeyIdentity(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}
