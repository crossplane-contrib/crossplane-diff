package crossplane

import (
	"context"
	"sync"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	ucomposite "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"
)

// CompositeResourceDefinitionKind is the kind for Composite Resource Definitions.
const CompositeResourceDefinitionKind = "CompositeResourceDefinition"

// DefinitionClient handles Crossplane definitions (XRDs).
//
//nolint:interfacebloat // The 6 methods are cohesively about XRD lookup; splitting just to satisfy the linter would create surface without value.
type DefinitionClient interface {
	core.Initializable

	// GetXRDs gets all XRDs in the cluster
	GetXRDs(ctx context.Context) ([]*un.Unstructured, error)

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
	logger         logging.Logger

	// XRDs cache
	gvks       []schema.GroupVersionKind
	xrds       []*un.Unstructured
	xrdsMutex  sync.RWMutex
	xrdsLoaded bool
}

// NewDefinitionClient creates a new DefaultDefinitionClient.
func NewDefinitionClient(resourceClient kubernetes.ResourceClient, logger logging.Logger) DefinitionClient {
	return &DefaultDefinitionClient{
		resourceClient: resourceClient,
		logger:         logger,
		xrds:           []*un.Unstructured{},
	}
}

// Initialize loads XRDs into the cache.
func (c *DefaultDefinitionClient) Initialize(ctx context.Context) error {
	c.logger.Debug("Initializing definition client")

	gvks, err := c.resourceClient.GetGVKsForGroupKind(ctx, CrossplaneAPIExtGroup, CompositeResourceDefinitionKind)
	if err != nil {
		return errors.Wrap(err, "cannot get XRD GVKs")
	}

	c.gvks = gvks

	// Load XRDs
	_, err = c.GetXRDs(ctx)
	if err != nil {
		return errors.Wrap(err, "cannot load XRDs")
	}

	c.logger.Debug("Definition client initialized", "xrdsCount", len(c.xrds))

	return nil
}

// GetXRDs gets all XRDs in the cluster.
func (c *DefaultDefinitionClient) GetXRDs(ctx context.Context) ([]*un.Unstructured, error) {
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
	c.logger.Debug("Looking for XRD that defines claim",
		"gvk", gvk.String())

	// Get all XRDs
	xrds, err := c.GetXRDs(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get XRDs")
	}

	// Loop through XRDs to find one that defines this GVK as a claim
	for _, xrd := range xrds {
		claimGroup, found, _ := un.NestedString(xrd.Object, "spec", "group")

		// Skip if group doesn't match
		if !found || claimGroup != gvk.Group {
			continue
		}

		// Check claim kind
		claimNames, found, _ := un.NestedMap(xrd.Object, "spec", "claimNames")
		if !found || claimNames == nil {
			continue
		}

		claimKind, found, _ := un.NestedString(claimNames, "kind")
		if !found || claimKind != gvk.Kind {
			continue
		}

		c.logger.Debug("Found matching XRD for claim type",
			"gvk", gvk.String(),
			"xrd", xrd.GetName())

		return xrd, nil
	}

	return nil, errors.Errorf("no XRD found that defines claim type %s", gvk.String())
}

// GetXRDForXR finds the XRD that defines the given XR type.
func (c *DefaultDefinitionClient) GetXRDForXR(ctx context.Context, gvk schema.GroupVersionKind) (*un.Unstructured, error) {
	c.logger.Debug("Looking for XRD that defines XR",
		"gvk", gvk.String())

	// Get all XRDs
	xrds, err := c.GetXRDs(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get XRDs")
	}

	// Loop through XRDs to find one that defines this GVK as an XR
	for _, xrd := range xrds {
		xrGroup, found, _ := un.NestedString(xrd.Object, "spec", "group")

		// Skip if group doesn't match
		if !found || xrGroup != gvk.Group {
			continue
		}

		// Check XR kind
		xrKind, found, _ := un.NestedString(xrd.Object, "spec", "names", "kind")
		if !found || xrKind != gvk.Kind {
			continue
		}

		// Check version matches
		versions, found, _ := un.NestedSlice(xrd.Object, "spec", "versions")
		if !found {
			continue
		}

		versionMatches := false

		for _, v := range versions {
			version, ok := v.(map[string]any)
			if !ok {
				continue
			}

			name, found, _ := un.NestedString(version, "name")
			if found && name == gvk.Version {
				versionMatches = true
				break
			}
		}

		if !versionMatches {
			continue
		}

		c.logger.Debug("Found matching XRD for XR type",
			"gvk", gvk.String(),
			"xrd", xrd.GetName())

		return xrd, nil
	}

	return nil, errors.Errorf("no XRD found that defines XR type %s", gvk.String())
}

// IsClaimResource checks if the given resource is a claim type by attempting
// to find an XRD that defines it as a claim.
func (c *DefaultDefinitionClient) IsClaimResource(ctx context.Context, resource *un.Unstructured) bool {
	gvk := resource.GroupVersionKind()

	// Try to find an XRD that defines this resource type as a claim
	_, err := c.GetXRDForClaim(ctx, gvk)
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
