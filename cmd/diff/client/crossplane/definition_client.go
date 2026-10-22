package crossplane

import (
	"context"
	"sync"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	ucomposite "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"
)

// CompositeResourceDefinitionKind is the kind for Composite Resource Definitions.
const CompositeResourceDefinitionKind = "CompositeResourceDefinition"

// DefinitionClient handles Crossplane definitions (XRDs).
type DefinitionClient interface {
	core.Initializable

	// GetXRDForClaim finds the XRD that defines the given claim type
	GetXRDForClaim(ctx context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error)

	// GetXRDForXR finds the XRD that defines the given XR type
	GetXRDForXR(ctx context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error)

	// IsClaimResource checks if the given resource is a claim type
	IsClaimResource(ctx context.Context, resource *un.Unstructured) bool

	// GetCompositeSchema returns the composite.Schema (Legacy or Modern) that
	// applies to the given XR or claim GVK. The scope is read from the XRD's
	// spec.scope field (NOT the XRD's own apiVersion, which the apiserver may
	// rewrite during v1↔v2 conversion). spec.scope == "LegacyCluster" maps to
	// SchemaLegacy (canonical fields under spec.*); "Cluster"/"Namespaced" map
	// to SchemaModern (canonical fields under spec.crossplane.*). Mirrors the
	// rule the render binary uses (selectSchema in crossplane's
	// internal/render/composite/render.go).
	GetCompositeSchema(ctx context.Context, gvk schema.GroupVersionKind) (ucomposite.Schema, error)
}

// DefaultDefinitionClient implements DefinitionClient.
type DefaultDefinitionClient struct {
	resourceClient kubernetes.ResourceClient
	typeConverter  kubernetes.TypeConverter
	logger         logging.Logger

	// XRDs cache: every XRD in the cluster, listed by listXRDs at most once.
	gvks       []schema.GroupVersionKind
	xrds       []*un.Unstructured
	xrdsMutex  sync.RWMutex
	xrdsLoaded bool

	// The XRD that defines each XR and claim GVK looked up so far, or nil where none does.
	// byGVKMu guards both, and is never held across a cluster request.
	xrdsForXR    map[schema.GroupVersionKind]*un.Unstructured
	xrdsForClaim map[schema.GroupVersionKind]*un.Unstructured
	byGVKMu      sync.Mutex
}

// crdGVK is the GVK of the CustomResourceDefinitions Crossplane generates for each XRD's XR and claim.
func crdGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
}

// NewDefinitionClient creates a new DefaultDefinitionClient. The type converter supplies the plural
// resource names XRDs and their CRDs are named by.
func NewDefinitionClient(resourceClient kubernetes.ResourceClient, typeConverter kubernetes.TypeConverter, logger logging.Logger) DefinitionClient {
	return &DefaultDefinitionClient{
		resourceClient: resourceClient,
		typeConverter:  typeConverter,
		logger:         logger,
		xrds:           []*un.Unstructured{},
		xrdsForXR:      make(map[schema.GroupVersionKind]*un.Unstructured),
		xrdsForClaim:   make(map[schema.GroupVersionKind]*un.Unstructured),
	}
}

// Initialize discovers the served XRD versions. XRDs are fetched on demand.
func (c *DefaultDefinitionClient) Initialize(ctx context.Context) error {
	c.logger.Debug("Initializing definition client")

	gvks, err := c.resourceClient.GetGVKsForGroupKind(ctx, CrossplaneAPIExtGroup, CompositeResourceDefinitionKind)
	if err != nil {
		return errors.Wrap(err, "cannot get XRD GVKs")
	}

	c.gvks = gvks

	c.logger.Debug("Definition client initialized")

	return nil
}

// listXRDs lists every XRD in the cluster, at most once per client.
func (c *DefaultDefinitionClient) listXRDs(ctx context.Context) ([]*un.Unstructured, error) {
	// Check if XRDs are already loaded
	c.xrdsMutex.RLock()

	if c.xrdsLoaded {
		xrds := c.xrds
		c.xrdsMutex.RUnlock()
		c.logger.Debug("Using cached XRDs", "count", len(xrds))

		return xrds, nil
	}

	c.xrdsMutex.RUnlock()

	// Need to load XRDs
	c.xrdsMutex.Lock()
	defer c.xrdsMutex.Unlock()

	// Double-check now that we have the write lock
	if c.xrdsLoaded {
		c.logger.Debug("Using cached XRDs (after recheck)", "count", len(c.xrds))
		return c.xrds, nil
	}

	c.logger.Debug("Fetching XRDs from cluster")

	xrds, err := listMatchingResources(ctx, c.resourceClient, c.gvks, "" /* XRDs are cluster scoped */)
	// List all XRDs
	if err != nil {
		c.logger.Debug("Failed to list XRDs", "error", err)
		return nil, errors.Wrap(err, "cannot list XRDs")
	}

	// Cache the result
	c.xrds = xrds
	c.xrdsLoaded = true

	c.logger.Debug("Successfully retrieved and cached XRDs", "count", len(xrds))

	return xrds, nil
}

// GetXRDForClaim finds the XRD that defines the given claim type.
func (c *DefaultDefinitionClient) GetXRDForClaim(ctx context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
	xrd, err := c.xrdForClaim(ctx, gvk)
	if err != nil {
		return nil, err
	}

	// A copy, so a caller that modifies what it gets does not modify this cache, which concurrent callers share.
	return xrd.DeepCopy(), nil
}

// GetXRDForXR finds the XRD that defines the given XR type.
func (c *DefaultDefinitionClient) GetXRDForXR(ctx context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
	c.logger.Debug("Looking for XRD that defines XR", "gvk", gvk.String())

	xrd, err := c.cachedLookup(ctx, c.xrdsForXR, gvk, c.fetchXRDForXR)
	if err != nil {
		return nil, err
	}

	if xrd == nil {
		return nil, errors.Errorf("no XRD found that defines XR type %s", gvk.String())
	}

	c.logger.Debug("Found matching XRD for XR type", "gvk", gvk.String(), "xrd", xrd.GetName())

	// A copy, so a caller that modifies what it gets does not modify this cache, which concurrent callers share.
	return xrd.DeepCopy(), nil
}

// xrdForClaim is GetXRDForClaim without the copy, for callers that only read the XRD.
func (c *DefaultDefinitionClient) xrdForClaim(ctx context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
	c.logger.Debug("Looking for XRD that defines claim", "gvk", gvk.String())

	xrd, err := c.cachedLookup(ctx, c.xrdsForClaim, gvk, c.fetchXRDForClaim)
	if err != nil {
		return nil, err
	}

	if xrd == nil {
		return nil, errors.Errorf("no XRD found that defines claim type %s", gvk.String())
	}

	c.logger.Debug("Found matching XRD for claim type", "gvk", gvk.String(), "xrd", xrd.GetName())

	return xrd, nil
}

// cachedLookup returns cache's entry for gvk, nil if gvk is cached as defined by no XRD, and otherwise
// fetches it and caches the result, a miss included. A failed fetch is not cached.
func (c *DefaultDefinitionClient) cachedLookup(ctx context.Context, cache map[schema.GroupVersionKind]*un.Unstructured, gvk schema.GroupVersionKind,
	fetch func(context.Context, schema.GroupVersionKind) (*un.Unstructured, error),
) (*un.Unstructured, error) {
	c.byGVKMu.Lock()
	xrd, ok := cache[gvk]
	c.byGVKMu.Unlock()

	if ok {
		return xrd, nil
	}

	if gvk.Group == "" {
		// A core type, such as a ConfigMap. No XRD defines one: an XRD's group is required.
		return nil, nil
	}

	xrd, err := fetch(ctx, gvk)
	if err != nil {
		return nil, err
	}

	c.byGVKMu.Lock()
	cache[gvk] = xrd
	c.byGVKMu.Unlock()

	return xrd, nil
}

// fetchXRDForXR gets the XRD that defines gvk as an XR, or nil if none does. An XRD is named
// <plural>.<group> of the XR type it defines, and discovery knows the XR type's plural.
func (c *DefaultDefinitionClient) fetchXRDForXR(ctx context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
	plural, err := c.typeConverter.GetResourceNameForGVK(ctx, gvk)
	if err != nil {
		// Not served, so no XRD defines it at this version. Not cached: discovery already caches it.
		return nil, errors.Wrapf(err, "no XRD found that defines XR type %s", gvk.String())
	}

	xrd, err := c.getXRD(ctx, plural+"."+gvk.Group)
	if err != nil || xrd == nil || !definesXR(xrd, gvk) {
		return nil, err
	}

	return xrd, nil
}

// fetchXRDForClaim gets the XRD that defines gvk as a claim, or nil if none does. Crossplane names a
// claim's CRD <plural>.<group> and makes the XRD its controller. A CRD without a controller reference
// (applied by hand rather than generated by Crossplane) is matched against every XRD, listed once.
func (c *DefaultDefinitionClient) fetchXRDForClaim(ctx context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
	plural, err := c.typeConverter.GetResourceNameForGVK(ctx, gvk)
	if err != nil {
		// Not served, so no XRD defines it at this version. Not cached: discovery already caches it.
		return nil, errors.Wrapf(err, "no XRD found that defines claim type %s", gvk.String())
	}

	crdName := plural + "." + gvk.Group

	crd, err := c.resourceClient.GetResource(ctx, crdGVK(), "", crdName)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, errors.Wrapf(err, "cannot get CRD %s", crdName)
	}

	owner := metav1.GetControllerOf(crd)
	if owner == nil {
		c.logger.Debug("CRD has no controller; looking for its claim type among all XRDs", "crd", crdName)

		xrds, err := c.listXRDs(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "cannot get XRDs")
		}

		for _, xrd := range xrds {
			if definesClaim(xrd, gvk) {
				return xrd, nil
			}
		}

		return nil, nil
	}

	// A CRD controlled by something other than an XRD, such as a provider's, names no XRD that
	// defines the claim, and definesClaim rejects any XRD that shares its owner's name.
	xrd, err := c.getXRD(ctx, owner.Name)
	if err != nil || xrd == nil || !definesClaim(xrd, gvk) {
		return nil, err
	}

	return xrd, nil
}

// getXRD gets the named XRD at the first served XRD version (the version listing them would have
// returned it at), or nil if there is none.
func (c *DefaultDefinitionClient) getXRD(ctx context.Context, name string) (*un.Unstructured, error) {
	if len(c.gvks) == 0 {
		return nil, nil
	}

	xrd, err := c.resourceClient.GetResource(ctx, c.gvks[0], "", name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, errors.Wrapf(err, "cannot get XRD %s", name)
	}

	return xrd, nil
}

// definesXR reports whether xrd defines gvk as an XR: its group and kind, at one of its versions.
func definesXR(xrd *un.Unstructured, gvk schema.GroupVersionKind) bool {
	xrGroup, _, _ := un.NestedString(xrd.Object, "spec", "group")
	xrKind, _, _ := un.NestedString(xrd.Object, "spec", "names", "kind")

	if xrGroup != gvk.Group || xrKind != gvk.Kind {
		return false
	}

	versions, _, _ := un.NestedSlice(xrd.Object, "spec", "versions")
	for _, v := range versions {
		version, ok := v.(map[string]any)
		if !ok {
			continue
		}

		if name, found, _ := un.NestedString(version, "name"); found && name == gvk.Version {
			return true
		}
	}

	return false
}

// definesClaim reports whether xrd defines gvk as a claim: its group and claim kind, at any version.
func definesClaim(xrd *un.Unstructured, gvk schema.GroupVersionKind) bool {
	group, _, _ := un.NestedString(xrd.Object, "spec", "group")
	claimKind, found, _ := un.NestedString(xrd.Object, "spec", "claimNames", "kind")

	return found && group == gvk.Group && claimKind == gvk.Kind
}

// IsClaimResource checks if the given resource is a claim type by attempting
// to find an XRD that defines it as a claim.
func (c *DefaultDefinitionClient) IsClaimResource(ctx context.Context, resource *un.Unstructured) bool {
	gvk := resource.GroupVersionKind()

	// Try to find an XRD that defines this resource type as a claim
	_, err := c.xrdForClaim(ctx, gvk)
	if err != nil {
		c.logger.Debug("Resource is not a claim type", "gvk", gvk.String(), "error", err)
		return false
	}

	c.logger.Debug("Resource is a claim type", "gvk", gvk.String())

	return true
}

// GetCompositeSchema returns the composite.Schema (Legacy or Modern) for the
// given XR or claim GVK by looking up the XRD and reading its spec.scope.
// LegacyCluster → SchemaLegacy (canonical fields under spec.*); Cluster or
// Namespaced → SchemaModern (canonical fields under spec.crossplane.*). Tries
// the XR path first; falls back to the claim path so the helper works for both.
// See the interface doc for why scope, not apiVersion, drives the decision.
func (c *DefaultDefinitionClient) GetCompositeSchema(ctx context.Context, gvk schema.GroupVersionKind) (ucomposite.Schema, error) {
	xrd, err := c.GetXRDForXR(ctx, gvk)
	if err != nil {
		// Not an XR GVK; try the claim path.
		var claimErr error

		xrd, claimErr = c.GetXRDForClaim(ctx, gvk)
		if claimErr != nil {
			return ucomposite.SchemaModern, errors.Wrapf(err, "no XRD found for %s (also tried claim: %v)", gvk.String(), claimErr)
		}
	}

	return SchemaFromXRD(xrd), nil
}

// SchemaFromXRD picks the composite.Schema (Legacy or Modern) for the given
// XRD by reading its spec.scope. Use this when you've already fetched the XRD
// for another reason (e.g. forwarding it to the render binary) and want to
// avoid the redundant cache lookup GetCompositeSchema would do.
//
// The rule mirrors the render binary's own selectSchema (see
// crossplane/crossplane internal/render/composite/render.go): use spec.scope
// rather than the XRD's apiVersion. The apiserver round-trips XRDs through
// v1↔v2 conversion (a v1 XRD POSTed by the user can come back from a v2 list
// as kind v2), so apiVersion is unreliable. spec.scope is preserved verbatim:
// v1 XRDs default to "LegacyCluster" and stay there; v2 XRDs declare
// "Cluster" or "Namespaced" explicitly.
func SchemaFromXRD(xrd *un.Unstructured) ucomposite.Schema {
	scope, _, _ := un.NestedString(xrd.Object, "spec", "scope")
	if scope == "" || scope == "LegacyCluster" {
		return ucomposite.SchemaLegacy
	}

	return ucomposite.SchemaModern
}
