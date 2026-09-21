package crossplane

import (
	"fmt"
	"maps"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// XRRevisionLabelSelector returns the effective label selector Crossplane would use to select a
// CompositionRevision for the given XR/Claim, as a labels.Selector ready to evaluate against a
// revision's (or composition's) labels.
//
// Crossplane only consults compositionRevisionSelector under the Automatic update policy (see
// internal/controller/apiextensions/composite/api.go): under Manual policy the revision is pinned
// via compositionRevisionRef, so the selector does not restrict the match. Accordingly this returns
// labels.Everything() when the policy is non-Automatic, or when no compositionRevisionSelector is
// present. Otherwise it returns the parsed selector, honoring both matchLabels and matchExpressions.
//
// Both the v2 path (spec.crossplane.compositionRevisionSelector) and the legacy v1 path
// (spec.compositionRevisionSelector) are read, v2 preferred.
func XRRevisionLabelSelector(xr *un.Unstructured) (labels.Selector, error) {
	selector, _, err := xrRevisionSelector(xr)
	return selector, err
}

// xrRevisionSelector parses the XR's compositionRevisionSelector exactly once, returning both the
// compiled labels.Selector (for matching) and the raw *metav1.LabelSelector (for introspection in
// mismatch messages). The raw selector is nil when there is no effective restriction (non-Automatic
// policy or absent selector), in which case the compiled selector is labels.Everything().
func xrRevisionSelector(xr *un.Unstructured) (labels.Selector, *metav1.LabelSelector, error) {
	// Selector only governs revision selection under Automatic policy.
	policy, err := XRUpdatePolicy(xr.Object, xr.GetAPIVersion())
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot read compositionUpdatePolicy")
	}

	if policy != updatePolicyAutomatic {
		return labels.Everything(), nil, nil
	}

	selectorMap, found, err := nestedCrossplaneMap(xr.Object, xr.GetAPIVersion(), "compositionRevisionSelector")
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot read compositionRevisionSelector")
	}

	if !found {
		// No selector => no restriction.
		return labels.Everything(), nil, nil
	}

	sel, err := labelSelectorFromUnstructuredMap(selectorMap)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot parse compositionRevisionSelector")
	}

	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot convert compositionRevisionSelector to selector")
	}

	return selector, sel, nil
}

// XRRevisionSelectorMatch reports whether the XR's compositionRevisionSelector matches the given set
// of labels (the labels of a Composition or CompositionRevision), and — on a mismatch — a
// human-readable detail explaining it, for surfacing in diff output so users can fine-tune their
// selector. The selector is parsed exactly once here, so any parse/read failure is returned as the
// single authoritative error (no best-effort duplicate parsing elsewhere).
//
// See XRRevisionLabelSelector for the Automatic-only semantics; a non-Automatic policy or an absent
// selector always matches (and yields an empty detail).
//
// The detail displays the composition's own labels (compLabels — the tunable menu a user can edit)
// plus any additional label the selector explicitly references (e.g. a Crossplane-stamped
// crossplane.io/composition-name the user chose to select on). Labels that are neither user-set nor
// selector-referenced (the implied composition-name/-hash in the common case) are omitted so the
// hint shows only load-bearing, actionable labels.
func XRRevisionSelectorMatch(xr *un.Unstructured, targetLabels, compLabels map[string]string) (matches bool, detail string, err error) {
	selector, sel, err := xrRevisionSelector(xr)
	if err != nil {
		return false, "", err
	}

	if selector.Matches(labels.Set(targetLabels)) {
		return true, "", nil
	}

	// sel is nil when the policy is non-Automatic or no selector is present; in those cases the
	// selector is labels.Everything(), which matches everything, so we never reach here with sel==nil.
	return false, describeSelectorMismatch(sel, targetLabels, compLabels), nil
}

// describeSelectorMismatch renders the "compositionRevisionSelector X does not match composition
// labels Y" hint. The displayed label set is the union of the user's composition labels and any
// label the selector references (load-bearing labels), so stamped-but-unselected labels stay out of
// the message. Rendering uses the k8s-canonical selector/label String() forms.
func describeSelectorMismatch(sel *metav1.LabelSelector, targetLabels, compLabels map[string]string) string {
	display := labels.Set{}
	maps.Copy(display, compLabels)

	// Surface any label the selector keys on, even if it isn't one of the user's composition labels
	// (e.g. a selector that explicitly matches crossplane.io/composition-name). These are load-bearing
	// for the match, so the user needs to see their actual values to fine-tune.
	for _, req := range selectorReferencedKeys(sel) {
		if v, ok := targetLabels[req]; ok {
			display[req] = v
		}
	}

	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		// Unreachable in practice: sel already parsed cleanly in xrRevisionSelector before we got here.
		return "compositionRevisionSelector could not be rendered"
	}

	return fmt.Sprintf("compositionRevisionSelector %s does not match composition labels {%s}",
		selector.String(), display.String())
}

// selectorReferencedKeys returns the label keys a LabelSelector constrains, via both matchLabels and
// matchExpressions.
func selectorReferencedKeys(sel *metav1.LabelSelector) []string {
	keys := make([]string, 0, len(sel.MatchLabels)+len(sel.MatchExpressions))
	for k := range sel.MatchLabels {
		keys = append(keys, k)
	}

	for _, req := range sel.MatchExpressions {
		keys = append(keys, req.Key)
	}

	return keys
}

// XRUpdatePolicy reads the compositionUpdatePolicy from an XR/Claim spec object, checking the v2
// path (spec.crossplane.compositionUpdatePolicy) then the v1 path (spec.compositionUpdatePolicy),
// selected from apiVersion. Defaults to "Automatic" when unset, matching Crossplane's default
// behavior. A non-nil error means the field is present but malformed (not a string); callers that
// prioritize accuracy propagate it rather than silently defaulting.
//
// This is the single shared reader for compositionUpdatePolicy across the codebase.
func XRUpdatePolicy(obj map[string]any, apiVersion string) (string, error) {
	policy, found, err := nestedCrossplaneString(obj, apiVersion, "compositionUpdatePolicy")
	if err != nil {
		return "", err
	}

	if found && policy != "" {
		return policy, nil
	}

	return updatePolicyAutomatic, nil
}

// labelSelectorFromUnstructuredMap converts an unstructured compositionRevisionSelector map (as
// stored on an XR spec) into a typed metav1.LabelSelector via runtime conversion.
func labelSelectorFromUnstructuredMap(m map[string]any) (*metav1.LabelSelector, error) {
	sel := &metav1.LabelSelector{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, sel); err != nil {
		return nil, err
	}

	return sel, nil
}
