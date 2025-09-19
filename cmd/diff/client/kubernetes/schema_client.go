package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	"github.com/crossplane/crossplane/v2/cmd/crank/common/crd"
)

// SchemaClient handles operations related to Kubernetes schemas and CRDs.
type SchemaClient interface {
	// GetCRD gets the CustomResourceDefinition for a given GVK
	GetCRD(ctx context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error)

	// IsCRDRequired checks if a GVK requires a CRD
	IsCRDRequired(ctx context.Context, gvk schema.GroupVersionKind) bool

	// LoadCRDsFromXRDs converts XRDs to CRDs and caches them
	LoadCRDsFromXRDs(ctx context.Context, xrds []*un.Unstructured) error

	// GetAllCRDs returns all cached CRDs (needed for external validation library)
	GetAllCRDs() []*extv1.CustomResourceDefinition
}

// DefaultSchemaClient implements SchemaClient.
type DefaultSchemaClient struct {
	dynamicClient dynamic.Interface
	typeConverter TypeConverter
	logger        logging.Logger

	// Resource type caching
	resourceTypeMap map[schema.GroupVersionKind]bool
	resourceMapMu   sync.RWMutex

	// CRD caching - consolidated from SchemaValidator
	crds      []*extv1.CustomResourceDefinition
	crdsMu    sync.RWMutex
	crdByName map[string]*extv1.CustomResourceDefinition // for fast lookup by name
}

// NewSchemaClient creates a new DefaultSchemaClient.
func NewSchemaClient(clients *core.Clients, typeConverter TypeConverter, logger logging.Logger) SchemaClient {
	return &DefaultSchemaClient{
		dynamicClient:   clients.Dynamic,
		typeConverter:   typeConverter,
		logger:          logger,
		resourceTypeMap: make(map[schema.GroupVersionKind]bool),
		crds:            []*extv1.CustomResourceDefinition{},
		crdByName:       make(map[string]*extv1.CustomResourceDefinition),
	}
}

// GetCRD gets the CustomResourceDefinition for a given GVK.
func (c *DefaultSchemaClient) GetCRD(ctx context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
	// Get the pluralized resource name to construct CRD name
	resourceName, err := c.typeConverter.GetResourceNameForGVK(ctx, gvk)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot determine CRD name for %s", gvk.String())
	}

	// Construct the full CRD name
	crdName := fmt.Sprintf("%s.%s", resourceName, gvk.Group)

	// Check cache first
	c.crdsMu.RLock()

	if cached, ok := c.crdByName[crdName]; ok {
		c.crdsMu.RUnlock()
		c.logger.Debug("Using cached CRD", "gvk", gvk.String(), "crdName", crdName)

		return cached, nil
	}

	c.crdsMu.RUnlock()

	c.logger.Debug("Looking up CRD", "gvk", gvk.String(), "crdName", resourceName)

	// Define the CRD GVR directly to avoid recursion
	crdGVR := schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}

	// Fetch the CRD from cluster
	crdObj, err := c.dynamicClient.Resource(crdGVR).Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		c.logger.Debug("Failed to get CRD", "gvk", gvk.String(), "crdName", crdName, "error", err)
		return nil, errors.Wrapf(err, "cannot get CRD %s for %s", crdName, gvk.String())
	}

	c.logger.Debug("Successfully retrieved CRD", "gvk", gvk.String(), "crdName", resourceName)

	// Convert to typed CRD
	crdTyped := &extv1.CustomResourceDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(crdObj.Object, crdTyped); err != nil {
		c.logger.Debug("Error converting CRD", "gvk", gvk.String(), "crdName", crdName, "error", err)
		return nil, errors.Wrapf(err, "cannot convert CRD %s to typed", crdName)
	}

	// Add to cache
	c.addCRD(crdTyped)

	return crdTyped, nil
}

// IsCRDRequired checks if a GVK requires a CRD.
func (c *DefaultSchemaClient) IsCRDRequired(ctx context.Context, gvk schema.GroupVersionKind) bool {
	// Check cache first
	c.resourceMapMu.RLock()

	if val, ok := c.resourceTypeMap[gvk]; ok {
		c.resourceMapMu.RUnlock()
		return val
	}

	c.resourceMapMu.RUnlock()

	// Core API resources never need CRDs
	if gvk.Group == "" {
		c.cacheResourceType(gvk, false)
		return false
	}

	// Standard Kubernetes API groups
	builtInGroups := []string{
		"apps", "batch", "extensions", "policy", "autoscaling",
	}
	for _, group := range builtInGroups {
		if gvk.Group == group {
			c.cacheResourceType(gvk, false)
			return false
		}
	}

	// k8s.io domain suffix groups are typically built-in
	// (except apiextensions.k8s.io which defines CRDs themselves)
	if strings.HasSuffix(gvk.Group, ".k8s.io") && gvk.Group != "apiextensions.k8s.io" {
		c.cacheResourceType(gvk, false)
		return false
	}

	// Try to query the discovery API to see if this resource exists
	_, err := c.typeConverter.GetResourceNameForGVK(ctx, gvk)
	if err != nil {
		// If we can't find it through discovery, assume it requires a CRD
		c.logger.Debug("Resource not found in discovery, assuming CRD is required",
			"gvk", gvk.String(),
			"error", err)
		c.cacheResourceType(gvk, true)

		return true
	}

	// Default to requiring a CRD
	c.cacheResourceType(gvk, true)

	return true
}

// Helper to cache resource type requirements.
func (c *DefaultSchemaClient) cacheResourceType(gvk schema.GroupVersionKind, requiresCRD bool) {
	c.resourceMapMu.Lock()
	defer c.resourceMapMu.Unlock()

	c.resourceTypeMap[gvk] = requiresCRD
}

// LoadCRDsFromXRDs converts XRDs to CRDs and caches them.
func (c *DefaultSchemaClient) LoadCRDsFromXRDs(_ context.Context, xrds []*un.Unstructured) error {
	c.logger.Debug("Loading CRDs from XRDs", "xrdCount", len(xrds))

	// Convert XRDs to CRDs
	crds, err := crd.ConvertToCRDs(xrds)
	if err != nil {
		c.logger.Debug("Failed to convert XRDs to CRDs", "error", err)
		return errors.Wrap(err, "cannot convert XRDs to CRDs")
	}

	// Set the CRDs in our cache
	c.setCRDs(crds)
	c.logger.Debug("Loaded CRDs from XRDs", "count", len(crds))

	return nil
}

// GetAllCRDs returns all cached CRDs.
func (c *DefaultSchemaClient) GetAllCRDs() []*extv1.CustomResourceDefinition {
	c.crdsMu.RLock()
	defer c.crdsMu.RUnlock()

	// Return a copy to prevent external modification
	result := make([]*extv1.CustomResourceDefinition, len(c.crds))
	copy(result, c.crds)

	return result
}

// addCRD adds a CRD to the cache.
func (c *DefaultSchemaClient) addCRD(crd *extv1.CustomResourceDefinition) {
	c.crdsMu.Lock()
	defer c.crdsMu.Unlock()

	// Add to slice
	c.crds = append(c.crds, crd)

	// Add to name lookup map
	c.crdByName[crd.Name] = crd

	c.logger.Debug("Added CRD to cache", "crdName", crd.Name)
}

// setCRDs sets the CRDs directly, used internally for bulk loading.
func (c *DefaultSchemaClient) setCRDs(crds []*extv1.CustomResourceDefinition) {
	c.crdsMu.Lock()
	defer c.crdsMu.Unlock()

	c.crds = crds

	// Rebuild name lookup map
	c.crdByName = make(map[string]*extv1.CustomResourceDefinition)
	for _, crd := range crds {
		c.crdByName[crd.Name] = crd
	}

	c.logger.Debug("Set CRDs directly", "count", len(crds))
}
