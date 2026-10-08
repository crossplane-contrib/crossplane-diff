package diffprocessor

import (
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Crossplane label keys used for resource identity and ownership.
const (
	// LabelComposite identifies the root composite resource that owns a composed resource.
	LabelComposite = "crossplane.io/composite"
	// LabelClaimName identifies the claim that triggered creation of the composite.
	LabelClaimName = "crossplane.io/claim-name"
	// LabelClaimNamespace identifies the namespace of the claim.
	LabelClaimNamespace = "crossplane.io/claim-namespace"
)

// CopyLabels copies specified labels from source to target if they exist in source.
// This is a no-op if source has no labels or if none of the specified keys exist.
func CopyLabels(source, target *un.Unstructured, keys ...string) {
	sourceLabels := source.GetLabels()
	if sourceLabels == nil {
		return
	}

	targetLabels := target.GetLabels()
	if targetLabels == nil {
		targetLabels = make(map[string]string)
	}

	copied := false

	for _, key := range keys {
		if value, exists := sourceLabels[key]; exists {
			targetLabels[key] = value
			copied = true
		}
	}

	if copied {
		target.SetLabels(targetLabels)
	}
}

// SetCompositionRevisionRefName points target's existing compositionRevisionRef at name, returning
// whether it did. Handles both V1 (spec.compositionRevisionRef) and V2
// (spec.crossplane.compositionRevisionRef) paths, preferring V2 — the same order
// nestedCrossplaneString reads them in, so for a (pathological) object carrying both we overwrite the
// value that would actually be consumed.
//
// It deliberately does NOT create a ref that isn't already there. An existing ref is the only case
// with a stale value to correct, and it proves the path is one the composite's schema accepts;
// inventing a path would risk a validation failure for a composite that renders fine today. A
// composite not yet tracking a revision therefore keeps rendering no ref at all — unchanged from
// before, and so never a spurious diff.
//
// Used to apply types.XRDiffOptions.RevisionName, with which comp seeds the CompositionRevision a
// diffed composition would produce, so a template reading the revision name renders the value it would
// really get. It is applied to the effective composite (for a claim, its backing XR), after the
// cluster's ref has been inherited. Only ever applied to composites that would genuinely re-point; see
// issue #474.
func SetCompositionRevisionRefName(target *un.Unstructured, name string) bool {
	for _, path := range [][]string{
		{"spec", "crossplane", "compositionRevisionRef"},
		{"spec", "compositionRevisionRef"},
	} {
		if _, found, err := un.NestedMap(target.Object, path...); err != nil || !found {
			continue
		}

		if err := un.SetNestedField(target.Object, name, append(path, "name")...); err != nil {
			// Unreachable: NestedMap just proved the parent is a map[string]any, so setting a string
			// leaf under it cannot fail on type grounds.
			return false
		}

		return true
	}

	return false
}
