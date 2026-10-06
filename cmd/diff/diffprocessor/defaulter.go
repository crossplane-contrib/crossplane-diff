package diffprocessor

import (
	"context"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	clixr "github.com/crossplane/cli/v2/pkg/xr"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// Defaulter predicts the defaults the apiserver would apply to a resource from its CRD's schema.
//
// Nothing it returns may be sent to the apiserver. Under server-side apply every field in a request
// claims ownership of that field, so a locally defaulted field would take a value away from whichever
// manager set it in the cluster and report a change that applying the manifest would not make (#503).
// It has exactly two uses: the XR that composition and render consume, which must carry the spec
// Crossplane would see (StrictDefaulting); and the predicted result of an addition that has no
// apiserver result (LenientDefaulting).
//
// The prediction covers CRD `default:` values only. Mutating admission and admission plugins are not
// modelled at all, and neither is structural-schema pruning of unknown fields (#527) or conversion of
// a multi-version CRD through its storage version (#528).
type Defaulter interface {
	// Default returns a copy of obj with its CRD's defaults applied. obj itself is never modified.
	Default(ctx context.Context, obj *un.Unstructured) (*un.Unstructured, error)
}

// DefaultingPolicy says what a Defaulter does with a resource it finds no CRD for.
type DefaultingPolicy int

const (
	// StrictDefaulting fails for a resource it finds no CRD for. It is the policy for the XR that render
	// consumes: rendering without the defaults Crossplane would see could produce a wrong diff, so a
	// missing XRD or CRD must not be passed over.
	StrictDefaulting DefaultingPolicy = iota

	// LenientDefaulting returns a resource it finds no CRD for as an unchanged copy: a built-in type,
	// which has no CRD, or a resource whose CRD cannot be found. It is the policy for predicting an
	// addition the apiserver did not see, which is best effort by nature; the schema validator, not
	// the Defaulter, is the gate on missing CRDs.
	LenientDefaulting
)

// CRDDefaulter implements Defaulter from the CRDs a SchemaClient knows.
type CRDDefaulter struct {
	schemaClient k8.SchemaClient
	defClient    xp.DefinitionClient
	policy       DefaultingPolicy
}

// NewDefaulter creates a CRDDefaulter with the given policy.
func NewDefaulter(sc k8.SchemaClient, dc xp.DefinitionClient, policy DefaultingPolicy) Defaulter {
	return &CRDDefaulter{schemaClient: sc, defClient: dc, policy: policy}
}

// Default returns a copy of obj with its CRD's defaults applied. What happens when no CRD can be found
// depends on the policy. A CRD that is found but does not define obj's apiVersion is an error under
// either policy: there is nothing to predict from, and returning obj as though defaulted would hide it.
func (d *CRDDefaulter) Default(ctx context.Context, obj *un.Unstructured) (*un.Unstructured, error) {
	out := obj.DeepCopy()
	gvk := out.GroupVersionKind()

	crd, err := d.crdFor(ctx, gvk)
	if err != nil {
		if d.policy == LenientDefaulting {
			return out, nil
		}

		return nil, errors.Wrapf(err, "cannot default %s %q", gvk.String(), out.GetName())
	}

	if err := clixr.ApplyCRDDefaults(out.Object, out.GetAPIVersion(), *crd); err != nil {
		return nil, errors.Wrapf(err, "cannot apply CRD defaults to %s %q", gvk.String(), out.GetName())
	}

	return out, nil
}

// crdFor finds the CRD for gvk. An XR's comes from its XRD, by name, from the CRDs loaded when the
// processor initialized; any other resource's is looked up by GVK. A built-in type has none.
func (d *CRDDefaulter) crdFor(ctx context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
	if xrd, err := d.defClient.GetXRDForXR(ctx, gvk); err == nil && xrd != nil {
		crd, err := d.schemaClient.GetCRDByName(xrd.GetName())
		if err != nil {
			return nil, errors.Wrapf(err, "cannot find CRD for XRD %s", xrd.GetName())
		}

		return crd, nil
	}

	if !d.schemaClient.IsCRDRequired(ctx, gvk) {
		return nil, errors.Errorf("%s is a built-in type, which has no CRD", gvk.String())
	}

	crd, err := d.schemaClient.GetCRD(ctx, gvk)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot find CRD for %s", gvk.String())
	}

	return crd, nil
}
