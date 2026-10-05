package diffprocessor

import (
	"context"
	"reflect"

	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	clixr "github.com/crossplane/cli/v2/pkg/xr"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// SchemaDefaulter predicts the defaults the apiserver would apply to a resource from its CRD schema.
//
// It is a stand-in for the apiserver, used only where crossplane-diff has no apiserver result to show:
// an added resource that was not dry-run created (see DefaultDiffCalculator.dryRunCreateAddition).
// Nothing it returns may be sent to the apiserver. Under server-side apply, sending a field claims
// it, so a locally defaulted field would take ownership away from whichever manager set it in the
// cluster and report a change the real apply would not make (#503).
//
// The prediction covers CRD `default:` values only. Mutating admission and admission plugins are not
// modelled at all, and neither is structural-schema pruning of unknown fields (#527) or conversion of
// a multi-version CRD through its storage version (#528).
type SchemaDefaulter interface {
	// Default returns a copy of obj with its CRD's defaults applied. obj itself is never modified.
	Default(ctx context.Context, obj *un.Unstructured) (*un.Unstructured, error)
}

// DefaultSchemaDefaulter implements SchemaDefaulter using CRDs from a SchemaClient.
type DefaultSchemaDefaulter struct {
	schemaClient k8.SchemaClient
}

// NewSchemaDefaulter creates a new DefaultSchemaDefaulter.
func NewSchemaDefaulter(sc k8.SchemaClient) SchemaDefaulter {
	return &DefaultSchemaDefaulter{schemaClient: sc}
}

// Default returns a copy of obj with its CRD's defaults applied.
//
// Built-in types, which have no CRD, and resources whose CRD cannot be found come back as an unchanged
// copy. A failed CRD lookup is not an error here because the schema validator is the gate on missing
// CRDs, and every resource reaching the diff calculator has already passed it.
func (d *DefaultSchemaDefaulter) Default(ctx context.Context, obj *un.Unstructured) (*un.Unstructured, error) {
	out := obj.DeepCopy()

	gvk := out.GroupVersionKind()
	if !d.schemaClient.IsCRDRequired(ctx, gvk) {
		return out, nil
	}

	crd, err := d.schemaClient.GetCRD(ctx, gvk)
	if err != nil {
		return out, nil //nolint:nilerr // An unknown CRD has no defaults to apply; see above.
	}

	if err := clixr.ApplyCRDDefaults(out.Object, out.GetAPIVersion(), *crd); err != nil {
		return nil, errors.Wrapf(err, "cannot apply CRD defaults for %s/%s", gvk.String(), out.GetName())
	}

	return out, nil
}

// withoutLocalDefaults returns a copy of obj without the fields that local defaulting added to it.
//
// original is an object before local defaulting and defaulted is the same object after it; obj is
// something built from defaulted since, such as a rendered XR merged with its input. A field counts as
// a local default when it is in defaulted but not in original, and obj still holds the defaulted
// value there. Everything else in obj is kept: what the user wrote, even where it equals a default,
// and whatever was added after defaulting.
//
// This is for building a server-side apply payload from an object that has to be defaulted locally
// for some other reason, as an XR is so that its composition renders against the same spec Crossplane
// would see. Sending the defaults would claim them (see SchemaDefaulter).
func withoutLocalDefaults(obj, defaulted, original *un.Unstructured) *un.Unstructured {
	out := obj.DeepCopy()
	removeLocalDefaults(out.Object, defaulted.Object, original.Object)

	return out
}

// removeLocalDefaults removes local defaults from obj in place. See withoutLocalDefaults.
func removeLocalDefaults(obj, defaulted, original map[string]any) {
	for k, dv := range defaulted {
		v, inObj := obj[k]
		if !inObj {
			continue
		}

		ov, inOriginal := original[k]
		if !inOriginal {
			if reflect.DeepEqual(v, dv) {
				delete(obj, k)
			}

			continue
		}

		removeLocalDefaultsFromValue(v, dv, ov)
	}
}

// removeLocalDefaultsFromValue descends into a value present in all three objects. Structural
// defaulting reaches into list items, so lists are followed item by item, but only while all three
// still have the same length: otherwise items can no longer be paired up.
func removeLocalDefaultsFromValue(v, dv, ov any) {
	switch d := dv.(type) {
	case map[string]any:
		m, mok := v.(map[string]any)
		o, ook := ov.(map[string]any)

		if mok && ook {
			removeLocalDefaults(m, d, o)
		}
	case []any:
		l, lok := v.([]any)
		o, ook := ov.([]any)

		if !lok || !ook || len(l) != len(d) || len(o) != len(d) {
			return
		}

		for i := range d {
			removeLocalDefaultsFromValue(l[i], d[i], o[i])
		}
	}
}
