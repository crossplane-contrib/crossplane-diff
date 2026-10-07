package diffprocessor

import (
	"context"
	"fmt"
	"maps"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	clixrgen "github.com/crossplane/cli/v2/cmd/crossplane/xr"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"
	"github.com/crossplane/crossplane-runtime/v2/pkg/xcrd"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
)

// clusterWrittenFields are the Crossplane machinery fields of a composite that Crossplane, not the
// composite's author, writes: the composite reconciler selects the composition and revision, and the
// apiserver defaults the update policy. An apply that omits one leaves the cluster's value in place,
// so a composite whose input says nothing about one is reconciled with the cluster's. Each lives at
// spec.crossplane.<field> on a modern XR and at spec.<field> on a legacy one.
//
//nolint:gochecknoglobals // A constant list; Go has no const slices.
var clusterWrittenFields = []string{"compositionRef", "compositionRevisionRef", "compositionUpdatePolicy"}

// EffectiveComposite is one composite seen the ways a diff needs it.
type EffectiveComposite struct {
	// Authored is the composite as supplied (see DefaultDiffProcessor.SanitizeXR). It is never
	// modified, because it is what gets validated and dry-run applied: under server-side apply every
	// field in a payload claims ownership of that field (#503). For a claim it is the claim, which is
	// defaulted only with its own CRD (by the apiserver, or by the lenient Defaulter for a predicted
	// addition), never its XR's: the two differ (crossplane-runtime pkg/xcrd schemas.go:75 and :209),
	// since only the XR's defaults compositionUpdatePolicy and only the claim's compositeDeletePolicy.
	Authored *cmp.Unstructured

	// Cluster is the cluster's copy of Authored, or nil if the cluster has none. For a claim it is the
	// claim.
	Cluster *un.Unstructured

	// Backing is the cluster's copy of a claim's backing XR. It is nil for an XR, and for a claim whose
	// backing XR does not exist yet.
	Backing *un.Unstructured

	// Effective is the XR Crossplane would compose, before CRD defaulting. For an XR, that is Authored
	// plus the fields Crossplane writes that Authored leaves out, taken from Cluster. For a claim it is
	// the backing XR (Backing, or one synthesized from the claim) with the claim's spec synced into it
	// the way Crossplane's claim syncer does. It is a fresh object the caller may modify.
	Effective *un.Unstructured

	// IsClaim reports whether Authored is a claim.
	IsClaim bool
}

// Composed returns the cluster object whose composed resources Effective observes: Backing for a claim,
// Cluster for an XR. It is nil when that object does not exist yet.
func (c EffectiveComposite) Composed() *un.Unstructured {
	if c.IsClaim {
		return c.Backing
	}

	return c.Cluster
}

// CompositeResolver computes the effective composite: what Crossplane would actually reconcile for a
// composite as supplied, given what the cluster already holds. It is computed once per composite,
// before the composition is resolved, so that composition resolution and render agree on which
// revision, composition and update policy apply.
type CompositeResolver struct {
	resources ResourceManager
	defs      xp.DefinitionClient
	logger    logging.Logger
}

// NewCompositeResolver creates a CompositeResolver.
func NewCompositeResolver(resources ResourceManager, defs xp.DefinitionClient, logger logging.Logger) *CompositeResolver {
	return &CompositeResolver{resources: resources, defs: defs, logger: logger}
}

// Resolve computes the effective composite for authored. existing is authored's cluster copy when the
// caller already has it (a nested XR, found among its parent's observed resources); otherwise it is nil
// and Resolve fetches it.
//
// Fetch errors are fatal: a composite resolved without the cluster state it depends on would be
// rendered against the wrong revision, composition or observed resources, and the diff would be
// silently wrong.
func (r *CompositeResolver) Resolve(ctx context.Context, authored *cmp.Unstructured, existing *un.Unstructured) (EffectiveComposite, error) {
	c := EffectiveComposite{Authored: authored, Cluster: existing}

	if c.Cluster == nil {
		cluster, _, err := r.resources.FetchCurrentObject(ctx, nil, authored.GetUnstructured())
		if err != nil {
			return EffectiveComposite{}, errors.Wrapf(err, "cannot fetch %s %q from the cluster", authored.GetKind(), authored.GetName())
		}

		c.Cluster = cluster
	}

	if !r.defs.IsClaimResource(ctx, authored.GetUnstructured()) {
		c.Effective = inheritClusterFields(authored.GetUnstructured(), c.Cluster)
		return c, nil
	}

	c.IsClaim = true

	backing, base, err := r.backingXR(ctx, authored, c.Cluster)
	if err != nil {
		return EffectiveComposite{}, err
	}

	c.Backing = backing

	var backingSpec map[string]any
	if backing != nil {
		backingSpec, _, _ = un.NestedMap(backing.Object, "spec")
	}

	manual, err := isManualPolicy(base)
	if err != nil {
		return EffectiveComposite{}, errors.Wrapf(err, "cannot read compositionUpdatePolicy of the backing XR of claim %q", authored.GetName())
	}

	spec, err := syncClaimSpec(authored.GetUnstructured(), backingSpec, manual)
	if err != nil {
		return EffectiveComposite{}, errors.Wrapf(err, "cannot sync claim %q into its backing XR", authored.GetName())
	}

	if err := un.SetNestedField(base.Object, spec, "spec"); err != nil {
		return EffectiveComposite{}, errors.Wrapf(err, "cannot set the spec of the backing XR of claim %q", authored.GetName())
	}

	c.Effective = base

	r.logger.Debug("Claim resolves to its backing XR",
		"claim", authored.GetName(),
		"backingXR", base.GetName(),
		"backingXRExists", backing != nil)

	return c, nil
}

// backingXR returns the cluster's copy of claim's backing XR (nil if it has none), and a fresh copy of
// the XR to sync the claim into: that same XR, or one synthesized from the claim when there is none.
//
// The backing XR is the one cluster's spec.resourceRef names. A claim with no cluster copy, or whose
// cluster copy names no XR yet, is bound to a new one, synthesized under the claim's own name. A
// resourceRef naming an XR that does not exist is treated the same way, but under the name the ref
// gives, because that is the XR the claim syncer would create. It applies the XR under the ref's name
// (claim/syncer_ssa.go:78-80), and it binds the claim before creating the XR (syncer_ssa.go:166-172,
// 215-225), so the ref can name an XR that does not exist yet; it would also re-create one deleted out
// of band. Any other fetch error is fatal, never a reason to diff without the backing XR (#533).
func (r *CompositeResolver) backingXR(ctx context.Context, claim *cmp.Unstructured, cluster *un.Unstructured) (*un.Unstructured, *un.Unstructured, error) {
	ref, hasRef := resourceRef(cluster)
	if !hasRef {
		synthetic, err := r.synthesizeBackingXR(ctx, claim, claim.GetName())
		return nil, synthetic, err
	}

	want := &un.Unstructured{}
	want.SetAPIVersion(ref.apiVersion)
	want.SetKind(ref.kind)
	want.SetName(ref.name)

	backing, _, err := r.resources.FetchCurrentObject(ctx, nil, want)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "cannot fetch backing XR %s %q of claim %q", ref.kind, ref.name, claim.GetName())
	}

	if backing == nil {
		// Info is the advisory level: the user should know the composed resources are diffed as though
		// they were all new.
		r.logger.Info("The claim's backing XR does not exist in the cluster; diffing as though Crossplane will create it",
			"claim", fmt.Sprintf("%s/%s", claim.GetKind(), claim.GetName()),
			"namespace", claim.GetNamespace(),
			"backingXR", fmt.Sprintf("%s/%s", ref.kind, ref.name))

		synthetic, err := r.synthesizeBackingXR(ctx, claim, ref.name)

		return nil, synthetic, err
	}

	return backing, backing.DeepCopy(), nil
}

// backingRef identifies a claim's backing XR.
type backingRef struct {
	apiVersion, kind, name string
}

// resourceRef reads a cluster claim's spec.resourceRef. It reports false when there is no cluster claim,
// or no complete ref on it: Crossplane has not bound the claim to an XR yet.
func resourceRef(cluster *un.Unstructured) (backingRef, bool) {
	if cluster == nil {
		return backingRef{}, false
	}

	ref, found, err := un.NestedStringMap(cluster.Object, "spec", "resourceRef")
	if err != nil || !found || ref["apiVersion"] == "" || ref["kind"] == "" || ref["name"] == "" {
		return backingRef{}, false
	}

	return backingRef{apiVersion: ref["apiVersion"], kind: ref["kind"], name: ref["name"]}, true
}

// synthesizeBackingXR creates the backing XR a claim would be bound to that has none yet, named name.
// It carries a spec.claimRef, the claim's labels and annotations, and a generated UID, as Crossplane's
// would, so that compositions reading any of them render.
//
// The conversion is upstream's cli helper. The XR kind comes from the XRD, because XRDs need not follow
// the "X"+claimKind convention, and the name is pinned rather than given upstream's random suffix,
// for cleaner diff output.
func (r *CompositeResolver) synthesizeBackingXR(ctx context.Context, claim *cmp.Unstructured, name string) (*un.Unstructured, error) {
	xrd, err := r.defs.GetXRDForClaim(ctx, claim.GroupVersionKind())
	if err != nil {
		return nil, errors.Wrap(err, "cannot get XRD for claim")
	}

	xrKind, _, _ := un.NestedString(xrd.Object, "spec", "names", "kind")

	xr, err := clixrgen.ConvertClaimToXR(claim.GetUnstructured(), clixrgen.Options{
		Name:        name,
		Kind:        xrKind,
		Direct:      false, // sets spec.claimRef, which compositions may reference
		GenerateUID: true,
	})
	if err != nil {
		return nil, errors.Wrap(err, "cannot convert claim to a backing XR")
	}

	r.logger.Debug("Synthesized a backing XR for a claim with none in the cluster",
		"claim", claim.GetName(),
		"namespace", claim.GetNamespace(),
		"xrName", xr.GetName(),
		"kind", xr.GetKind())

	return xr.GetUnstructured(), nil
}

// isManualPolicy reports whether xr's compositionUpdatePolicy is Manual.
func isManualPolicy(xr *un.Unstructured) (bool, error) {
	policy, err := xp.XRUpdatePolicy(xr.Object, xr.GetAPIVersion())
	return policy == compositionUpdatePolicyManual, err
}

// inheritClusterFields returns a copy of authored carrying cluster's UID and each of
// clusterWrittenFields that authored leaves out, copied to the path cluster holds it at. A field
// authored sets at either path is authored's opinion about it, so it always wins: that is how a
// Manual composite is moved to another revision.
//
// The UID matters because render keeps an input UID (crossplane internal/render/composite.Render), so
// it is what composition functions observe and what render checks observed resources' controller
// references against, rather than a fake one derived from the XR's name.
func inheritClusterFields(authored, cluster *un.Unstructured) *un.Unstructured {
	effective := authored.DeepCopy()
	if cluster == nil {
		return effective
	}

	effective.SetUID(cluster.GetUID())

	for _, field := range clusterWrittenFields {
		inheritField(cluster, effective, field)
	}

	return effective
}

// inheritField copies source's value of a Crossplane machinery field onto target, at each of the
// modern (spec.crossplane.<field>) and legacy (spec.<field>) paths source has it at, unless target
// already has it at either path.
func inheritField(source, target *un.Unstructured, field string) {
	paths := [][]string{{"spec", "crossplane", field}, {"spec", field}}

	for _, path := range paths {
		if _, found, _ := un.NestedFieldNoCopy(target.Object, path...); found {
			return
		}
	}

	for _, path := range paths {
		value, found, err := un.NestedFieldCopy(source.Object, path...)
		if err != nil || !found {
			continue
		}

		// Fails only if target holds a non-object on the way to path (spec.crossplane: "x", say), which
		// its schema rejects anyway; validation reports that.
		_ = un.SetNestedField(target.Object, value, path...)
	}
}

// syncClaimSpec returns the spec Crossplane's claim syncer gives a claim's backing XR. backingSpec is
// the backing XR's spec in the cluster, or nil when it does not exist yet. manual reports whether the
// backing XR's update policy is Manual.
//
// The rules are the syncers' (crossplane v2.4.2, internal/controller/apiextensions/claim/syncer_ssa.go,
// the default, and syncer_csa.go, which agrees on rules 1 to 3):
//
//  1. The claim's spec is propagated, minus the claim's own fields (resourceRef, compositeDeletePolicy,
//     writeConnectionSecretToRef) and the XR machinery only the XR controller may set (resourceRefs,
//     claimRef, compositionRevisionRef); compositionRef, compositionSelector, compositionUpdatePolicy
//     and compositionRevisionSelector are propagated (syncer_ssa.go:136-145, syncer_csa.go:115-123).
//  2. compositionRevisionRef is propagated too, but only when the backing XR's update policy is
//     Manual; under Automatic the XR controller owns it (syncer_ssa.go:147-152, syncer_csa.go:125-131).
//     The policy is read off the backing XR before the sync, as upstream reads it. A synthesized backing
//     XR carries the claim's own policy, which is the one it would be created with.
//  3. spec.claimRef is set to the claim (syncer_ssa.go:162-164, syncer_csa.go:141-143).
//
// The SSA syncer applies the result with the claim controller's own field manager (syncer_ssa.go:228),
// so a field the claim no longer has is removed from the XR, while the fields other managers wrote
// survive. Those are clusterWrittenFields, plus the legacy-only resourceRefs and
// writeConnectionSecretToRef, which the XR controller also writes. So each of those the propagated spec
// lacks is kept from backingSpec, and nothing else is. A synthesized backing XR (backingSpec nil) has
// none yet.
//
// Not modelled: an XRD's enforcedCompositionRef, under which the claim's compositionRef is not
// propagated either.
func syncClaimSpec(claim *un.Unstructured, backingSpec map[string]any, manual bool) (map[string]any, error) {
	claimSpec, _, err := un.NestedMap(claim.Object, "spec")
	if err != nil {
		return nil, errors.Wrap(err, "claim spec is not an object")
	}

	stripped := xcrd.CompositeResourceClaimSpecProps(nil)
	maps.Copy(stripped, xcrd.CompositeResourceSpecProps(apiextensionsv1.CompositeResourceScopeLegacyCluster, nil))

	for _, field := range xcrd.PropagateSpecProps {
		delete(stripped, field)
	}

	if manual {
		delete(stripped, xcrd.CompositionRevisionRef)
	}

	spec := make(map[string]any, len(claimSpec))

	for field, value := range claimSpec {
		if _, strip := stripped[field]; !strip {
			spec[field] = value
		}
	}

	spec[fieldClaimRef] = map[string]any{
		"apiVersion": claim.GetAPIVersion(),
		"kind":       claim.GetKind(),
		"name":       claim.GetName(),
		"namespace":  claim.GetNamespace(),
	}

	for _, field := range append([]string{fieldResourceRefs, fieldWriteConnectionSecretToRef}, clusterWrittenFields...) {
		if _, set := spec[field]; set {
			continue
		}

		if value, kept := backingSpec[field]; kept {
			spec[field] = runtime.DeepCopyJSONValue(value)
		}
	}

	return spec, nil
}
