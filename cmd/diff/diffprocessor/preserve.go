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
