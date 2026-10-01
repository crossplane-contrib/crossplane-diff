package diffprocessor

import (
	"fmt"
	"sort"
	"strings"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/types"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// This file validates the xr command's input set. PerformDiff drives an InputValidator through three
// stages; the default, bundleInputValidator, treats the inputs as one change set, so any overlap among
// them is settled here:
//
//  1. checkInputs, before rendering (ToRender): a duplicate input, and the same object given twice differently.
//  2. rejectManagedInputs, after rendering (Verdicts): an input that another input manages.
//  3. renderOverlapErrors (RenderOverlaps): two inputs' renders reaching one resource.
//
// Identity throughout is group, kind, namespace and name, independent of API version, since one object
// served at two versions is still one object. An input with no name yet (generateName only) has none.

// InputValidator validates one PerformDiff run's input set. It is created per run by an
// InputValidatorFactory, because it carries that run's state between stages.
type InputValidator interface {
	// ToRender returns the inputs to render, in input order, with duplicates already dropped.
	// An input with a non-nil Err was rejected before rendering and must not be rendered.
	ToRender() []types.ValidatedInput
	// RecordRender records the render of ToRender()[i]: every resource key it produced
	// (including unchanged ones) and its error, if any.
	RecordRender(i int, rendered map[string]bool, err error)
	// Verdicts returns the final error for each group (indexed like ToRender()), or nil.
	// It applies stage 2 and the precedence rule: a managed-input verdict replaces that
	// group's own render failure.
	Verdicts(groups []dt.XRDiffGroup) []error
	// RenderOverlaps returns the stage-3 errors.
	RenderOverlaps(groups []dt.XRDiffGroup) []error
}

// InputValidatorFactory creates the InputValidator for one run.
type InputValidatorFactory func(logger logging.Logger, inputs []*un.Unstructured) InputValidator

// bundleInputValidator is the default InputValidator. It is a "bundle" validator because it treats the
// inputs as one change set, to be applied together: any overlap among them — a duplicate, the same object
// twice differently, an input another input manages, two renders reaching one resource — is settled
// against the set as a whole, not per input.
type bundleInputValidator struct {
	// checks are checkInputs' verdicts on the inputs to render (duplicates dropped), indexed like toRender.
	checks   []inputCheck
	toRender []types.ValidatedInput
	// rendered and failed are, by index, each render's resource keys and each input's own failure
	// (its pre-render rejection, or else its render error).
	rendered []map[string]bool
	failed   []error
}

// NewBundleInputValidator returns the default InputValidator for one run over inputs. It has the
// InputValidatorFactory signature. Stage 1 (checkInputs) runs here, so duplicates are logged once.
func NewBundleInputValidator(logger logging.Logger, inputs []*un.Unstructured) InputValidator {
	v := &bundleInputValidator{}

	for _, in := range checkInputs(logger, inputs) {
		if in.duplicate {
			continue
		}

		v.checks = append(v.checks, in)
		v.toRender = append(v.toRender, types.ValidatedInput{Resource: in.res, Err: in.err})
		v.failed = append(v.failed, in.err)
	}

	v.rendered = make([]map[string]bool, len(v.checks))

	return v
}

// ToRender returns the non-duplicate inputs in input order; see InputValidator.
func (v *bundleInputValidator) ToRender() []types.ValidatedInput {
	return v.toRender
}

// RecordRender records the render of ToRender()[i]; see InputValidator.
func (v *bundleInputValidator) RecordRender(i int, rendered map[string]bool, err error) {
	v.rendered[i] = rendered
	v.failed[i] = err
}

// Verdicts returns each group's final error. A managed-input verdict replaces the group's own failure:
// that failure is moot, and often a symptom of the same mistake. See rejectManagedInputs.
func (v *bundleInputValidator) Verdicts(groups []dt.XRDiffGroup) []error {
	verdicts := make([]error, len(v.failed))
	copy(verdicts, v.failed)

	for i, err := range rejectManagedInputs(groups, v.checks, v.rendered, v.failed) {
		verdicts[i] = err
	}

	return verdicts
}

// RenderOverlaps returns the stage-3 errors; see renderOverlapErrors.
func (v *bundleInputValidator) RenderOverlaps(groups []dt.XRDiffGroup) []error {
	return renderOverlapErrors(groups)
}

// inputCheck is checkInputs' verdict on one input.
type inputCheck struct {
	res *un.Unstructured
	// duplicate is true for an input identical to an earlier one; it is not rendered.
	duplicate bool
	// err rejects the input before it is rendered.
	err error
}

// checkInputs judges the input set before anything is rendered. An input identical to an earlier one
// has one clear intent — a fat-fingered command line, or CI enumerating the same file twice — so it is
// dropped with a warning. The same object twice with different content fails both inputs: applying both
// would leave whichever is applied last, and the order inputs arrive in (often a glob's) is not intent.
func checkInputs(logger logging.Logger, resources []*un.Unstructured) []inputCheck {
	checks := make([]inputCheck, len(resources))
	first := make(map[string]int, len(resources))

	for i, res := range resources {
		checks[i] = inputCheck{res: res}

		id := identityOf(res)
		if id == "" {
			continue
		}

		j, seen := first[id]
		if !seen {
			first[id] = i
			continue
		}

		if equality.Semantic.DeepEqual(resources[j].Object, res.Object) {
			checks[i].duplicate = true
			logger.Info("Ignoring a duplicate input: it is identical to an earlier one",
				"resource", fmt.Sprintf("%s/%s", res.GetKind(), res.GetName()), "input", i+1, "duplicateOf", j+1)

			continue
		}

		checks[i].err = conflictingInputError(i+1, j+1, res)
		if checks[j].err == nil {
			checks[j].err = conflictingInputError(j+1, i+1, res)
		}
	}

	return checks
}

func conflictingInputError(position, other int, res *un.Unstructured) error {
	return errors.Errorf("input %d defines %s/%s differently from input %d; applying both would leave whichever "+
		"is applied last, and the order inputs are given in is not a statement of intent — pass only one of them",
		position, res.GetKind(), res.GetName(), other)
}

// rejectManagedInputs returns, by group index, an error for each input that another input manages: an
// XR a parent input composes, and an XR a claim input is bound to. Crossplane writes such an object on
// the manager's behalf — the parent's composition, or the claim controller, applies it with forced
// ownership — so supplying it as an input too gives it a second writer. The managed input is the
// mistake, so it is the one rejected.
//
// inputs, rendered and failed are indexed like groups: each group's input, every resource key its
// render produced (including unchanged ones, which its diffs omit), and its own failure. A managed input
// is judged even if its own render failed, because the verdict supersedes that failure: it is moot, and
// often a symptom of the same mistake (an XR's cluster copy is rarely a valid standalone input). A
// manager must have rendered, since what it manages is read from its render.
func rejectManagedInputs(groups []dt.XRDiffGroup, inputs []inputCheck, rendered []map[string]bool, failed []error) map[int]error {
	renderedIDs := make([]map[string]bool, len(groups))

	for i := range groups {
		renderedIDs[i] = make(map[string]bool, len(rendered[i]))
		for key := range rendered[i] {
			renderedIDs[i][diffKeyIdentity(key)] = true
		}
	}

	rejected := make(map[int]error)

	for b := range groups {
		managed := inputs[b].res
		if inputs[b].err != nil || identityOf(managed) == "" {
			continue
		}

		for a := range groups {
			if a == b || failed[a] != nil {
				continue
			}

			manager := inputs[a].res

			switch {
			case renderedIDs[a][identityOf(managed)]:
				rejected[b] = errors.Errorf("%s is composed by input %s, whose composition writes it; supplying it "+
					"as an input too gives it a second writer — pass %s only", kindName(managed), kindName(manager), kindName(manager))
			case boundToClaim(groups[b], managed, groups[a], manager):
				rejected[b] = errors.Errorf("%s is the XR bound to claim %s, which Crossplane's claim controller writes; "+
					"supplying it as an input too gives it a second writer — pass the claim, not its XR", kindName(managed), kindName(manager))
			}

			if rejected[b] != nil {
				break
			}
		}
	}

	return rejected
}

// boundToClaim reports whether xr is the XR bound to claim. Either side of the binding is enough, read
// from the raw input and from the cluster copy its own diff carries: the claim's spec.resourceRef (which
// only claims have; XRs have resourceRefs) naming the XR, or the XR's spec.claimRef, or the
// claim-name/claim-namespace labels Crossplane stamps on it, naming the claim.
func boundToClaim(xrGroup dt.XRDiffGroup, xr *un.Unstructured, claimGroup dt.XRDiffGroup, claim *un.Unstructured) bool {
	// A claim with no name yet binds nothing. Without this, an XR with no claimRef — whose reference
	// identity is also "" — would match it.
	if identityOf(claim) == "" {
		return false
	}

	for _, obj := range knownForms(claimGroup, claim) {
		if refIdentity(obj, "spec", "resourceRef") == identityOf(xr) {
			return true
		}
	}

	for _, obj := range knownForms(xrGroup, xr) {
		if refIdentity(obj, "spec", "claimRef") == identityOf(claim) {
			return true
		}

		labels := obj.GetLabels()
		if name, ok := labels[LabelClaimName]; ok && name == claim.GetName() && labels[LabelClaimNamespace] == claim.GetNamespace() {
			return true
		}
	}

	return false
}

// knownForms returns what is known of an input's object: the input as given and, from its own diff —
// which the calculator stores even when equal — its cluster copy and rendered form.
func knownForms(g dt.XRDiffGroup, res *un.Unstructured) []*un.Unstructured {
	forms := []*un.Unstructured{res}

	if own := g.Diffs[dt.MakeDiffKeyFromResource(res)]; own != nil {
		for _, obj := range []*un.Unstructured{own.Current.Raw, own.Desired.Raw} {
			if obj != nil {
				forms = append(forms, obj)
			}
		}
	}

	return forms
}

// renderOverlapErrors reports the resources produced by more than one input's render that no single
// diff can truthfully represent, most-significant cause first per resource and ordered by identity.
//
// Resources are matched on their version-independent identity (group, kind, namespace, name), not on the
// diff key: served versions are views of one stored object, so one object rendered at two API versions
// produces two keys yet is still contested. Renderings at different versions are never identical, so such
// an overlap lands on contention or disagreement.
//
// A diff key carries no owning-input component (see dt.MakeDiffKey), so any view that merges the groups
// into one map — the deprecated flat changes[] — keeps only one entry per key. Whether that loses
// anything is decided by who would control the resource, read from the rendered (or, for a removal,
// current) object's controller reference and compared by group, kind and name — never UID, which a
// render synthesizes afresh for an XR that does not exist yet:
//
//   - Two inputs sharing a generateName, checked first: both render under one synthesized placeholder,
//     so their resources collide here though the API server would name them apart, and merging even
//     identical renderings would report one XR's changes for two.
//   - Different controllers: contention. Crossplane's server-side apply refuses a second controller
//     reference, so the first XR to create the object keeps it and every other fails to reconcile it.
//     No diff predicts that, however alike the renderings.
//   - One controller or none, identical renderings (diff type and Clean views, which cleanupForDiff has
//     stripped of ownerReferences and uid): one change reached twice. Merged, not reported.
//   - One controller or none, differing renderings: no single diff is right for both. Defensive — the
//     ordinary causes are input errors, caught by the earlier stages.
//
// A key whose every entry is DiffTypeEqual is not reported: equal diffs appear in no rendered view.
func renderOverlapErrors(groups []dt.XRDiffGroup) []error {
	byID := make(map[string][]overlapProducer) // identity -> its renderings, in input order

	for i, g := range groups {
		keys := make([]string, 0, len(g.Diffs))
		for key := range g.Diffs {
			keys = append(keys, key)
		}

		sort.Strings(keys) // one input may render an identity under two keys; order them stably

		for _, key := range keys {
			id := diffKeyIdentity(key)
			if id == "" {
				id = key // not a parseable key; fall back to comparing it exactly
			}

			byID[id] = append(byID[id], overlapProducer{group: i, key: key})
		}
	}

	ids := make([]string, 0, len(byID))
	for id, ps := range byID {
		if len(ps) > 1 {
			ids = append(ids, id)
		}
	}

	sort.Strings(ids)

	var errs []error

	for _, id := range ids {
		if err := renderOverlapError(groups, byID[id]); err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}

// overlapProducer is one rendering of an overlapping resource: the input group that produced it and the
// diff key it produced it under. One object reached at two API versions has two keys.
type overlapProducer struct {
	group int
	key   string
}

func renderOverlapError(groups []dt.XRDiffGroup, producers []overlapProducer) error {
	inputs := make([]string, 0, len(producers))

	var (
		controllers       []string
		allEqual          = true
		identical         = true
		sharedPlaceholder = false
		placeholders      = make(map[string]bool)
		seenController    = make(map[string]bool)
		seenGroup         = make(map[int]bool)
		keys              []string
		seenKey           = make(map[string]bool)
		first             = groups[producers[0].group].Diffs[producers[0].key]
	)

	for _, p := range producers {
		g, d := groups[p.group], groups[p.group].Diffs[p.key]

		if !seenKey[p.key] {
			seenKey[p.key] = true
			keys = append(keys, p.key)
		}

		// The same input may reach the object again at another version. Its rendering still counts, but it
		// is neither a second input nor a second placeholder.
		if !seenGroup[p.group] {
			seenGroup[p.group] = true
			inputs = append(inputs, fmt.Sprintf("%s/%s", g.XR.Kind, g.XR.Name))

			if g.NameGenerated {
				sharedPlaceholder = sharedPlaceholder || placeholders[g.XR.Name]
				placeholders[g.XR.Name] = true
			}
		}

		// A nil entry is not expected; treat it as observable and as matching nothing, so a malformed
		// group errors loudly rather than being merged away.
		if d == nil || d.DiffType != dt.DiffTypeEqual {
			allEqual = false
		}

		if !sameRendering(first, d) {
			identical = false
		}

		if id, display := controllerOf(d); id != "" && !seenController[id] {
			seenController[id] = true
			controllers = append(controllers, display)
		}
	}

	sort.Strings(controllers)

	key := fmt.Sprintf("%q", keys[0])
	if len(keys) > 1 {
		others := make([]string, 0, len(keys)-1)
		for _, k := range keys[1:] {
			others = append(others, fmt.Sprintf("%q", k))
		}

		key += " (also rendered as " + strings.Join(others, ", ") + ")"
	}

	switch {
	case allEqual:
		return nil
	case sharedPlaceholder:
		return errors.Errorf("cannot combine diffs: inputs %s both produce resource %s only because crossplane-diff "+
			"gives XRs that share a generateName the same placeholder name; the API server would name them "+
			"differently, so they cannot be told apart here — diff them separately", strings.Join(inputs, ", "), key)
	case len(controllers) > 1:
		return errors.Errorf("cannot combine diffs: resource %s would be controlled by more than one XR (%s); "+
			"Crossplane gives it to whichever of them creates it first, and the other fails to reconcile it, "+
			"so no diff predicts applying these inputs together — diff them separately", key, strings.Join(controllers, ", "))
	case identical:
		return nil
	default:
		return errors.Errorf("cannot combine diffs: inputs %s both produce resource %s, but differently, so no "+
			"single diff is correct for both — diff them separately", strings.Join(inputs, ", "), key)
	}
}

// controllerOf returns the identity of the XR that would control the diffed resource, as a comparison
// key (group/kind/name) and a display form (Kind/Name), or empty strings if nothing controls it. It
// reads the rendered object, falling back to the current one for a removal. The UID is deliberately
// ignored; see renderOverlapErrors.
func controllerOf(d *dt.ResourceDiff) (string, string) {
	if d == nil {
		return "", ""
	}

	obj := d.Desired.Raw
	if obj == nil {
		obj = d.Current.Raw
	}

	if obj == nil {
		return "", ""
	}

	ref := metav1.GetControllerOf(obj)
	if ref == nil {
		return "", ""
	}

	return identity(ref.APIVersion, ref.Kind, "", ref.Name), fmt.Sprintf("%s/%s", ref.Kind, ref.Name)
}

// sameRendering reports whether two diffs describe the same change: the same diff type, and the same
// cleaned desired and current objects.
func sameRendering(a, b *dt.ResourceDiff) bool {
	if a == nil || b == nil {
		return false
	}

	return a.DiffType == b.DiffType &&
		sameObject(a.Desired.Clean, b.Desired.Clean) &&
		sameObject(a.Current.Clean, b.Current.Clean)
}

func sameObject(a, b *un.Unstructured) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	return equality.Semantic.DeepEqual(a.Object, b.Object)
}

// identity is an object's version-independent identity; "" for an object with no name yet.
func identity(apiVersion, kind, namespace, name string) string {
	if name == "" || kind == "" {
		return ""
	}

	group := ""
	if gv, err := schema.ParseGroupVersion(apiVersion); err == nil {
		group = gv.Group
	}

	return group + "/" + kind + "/" + namespace + "/" + name
}

func identityOf(res *un.Unstructured) string {
	return identity(res.GetAPIVersion(), res.GetKind(), res.GetNamespace(), res.GetName())
}

// diffKeyIdentity is identity for a key built by dt.MakeDiffKey ("<apiVersion>/<kind>/<namespace>/<name>",
// where apiVersion may itself contain a slash).
func diffKeyIdentity(key string) string {
	parts := strings.Split(key, "/")
	n := len(parts)

	if n < 4 {
		return ""
	}

	return identity(strings.Join(parts[:n-3], "/"), parts[n-3], parts[n-2], parts[n-1])
}

// refIdentity is the identity of the object reference (apiVersion, kind, name, and optionally namespace)
// at path, or "" if there is none.
func refIdentity(obj *un.Unstructured, path ...string) string {
	ref, found, err := un.NestedStringMap(obj.Object, path...)
	if err != nil || !found {
		return ""
	}

	return identity(ref["apiVersion"], ref["kind"], ref["namespace"], ref["name"])
}

func kindName(res *un.Unstructured) string {
	return fmt.Sprintf("%s/%s", res.GetKind(), res.GetName())
}
