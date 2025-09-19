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
)

// CompositeResourceDefinitionKind is the kind for Composite Resource Definitions.
const CompositeResourceDefinitionKind = "CompositeResourceDefinition"

// DefinitionClient handles Crossplane definitions (XRDs).
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

	gvks, err := c.resourceClient.GetGVKsForGroupKind(ctx, "apiextensions.crossplane.io", CompositeResourceDefinitionKind)
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
			version, ok := v.(map[string]interface{})
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
