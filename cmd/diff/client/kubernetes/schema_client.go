package kubernetes

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// SchemaClient handles operations related to Kubernetes schemas and CRDs.
type SchemaClient interface {
	// GetCRD gets the CustomResourceDefinition for a given GVK
	GetCRD(ctx context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error)

	// GetCRDByName gets the CustomResourceDefinition by its name
	GetCRDByName(ctx context.Context, name string) (*extv1.CustomResourceDefinition, error)

	// IsCRDRequired checks if a GVK requires a CRD
	IsCRDRequired(ctx context.Context, gvk schema.GroupVersionKind) bool

	// GetAllCRDs returns every CRD fetched so far (needed for external validation library)
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

	// CRDs fetched so far, and the NotFound error for each name found not to exist. crdsMu guards all
	// three, and is never held across a cluster request.
	crds        []*extv1.CustomResourceDefinition
	crdsMu      sync.RWMutex
	crdByName   map[string]*extv1.CustomResourceDefinition // for fast lookup by name
	missingCRDs map[string]error
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
		missingCRDs:     make(map[string]error),
	}
}

// GetCRD gets the CustomResourceDefinition for a given GVK.
func (c *DefaultSchemaClient) GetCRD(ctx context.Context, gvk schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
	// Get the pluralized resource name to construct CRD name
	resourceName, err := c.typeConverter.GetResourceNameForGVK(ctx, gvk)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot determine CRD name for %s", gvk.String())
	}

	crdName := fmt.Sprintf("%s.%s", resourceName, gvk.Group)

	crd, err := c.getCRD(ctx, crdName)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot get CRD %s for %s", crdName, gvk.String())
	}

	return crd, nil
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
	if slices.Contains(builtInGroups, gvk.Group) {
		c.cacheResourceType(gvk, false)
		return false
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

// GetCRDByName gets the named CRD, from the cache or else from the cluster, caching what it finds and a
// CRD found not to exist. Any other failure is not cached.
func (c *DefaultSchemaClient) GetCRDByName(ctx context.Context, name string) (*extv1.CustomResourceDefinition, error) {
	crd, err := c.getCRD(ctx, name)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot get CRD %s", name)
	}

	return crd, nil
}

// getCRD is GetCRDByName without wrapping the cluster's error, which each caller names in its own terms.
func (c *DefaultSchemaClient) getCRD(ctx context.Context, name string) (*extv1.CustomResourceDefinition, error) {
	c.crdsMu.RLock()
	crd, found := c.crdByName[name]
	missingErr, missing := c.missingCRDs[name]
	c.crdsMu.RUnlock()

	switch {
	case found:
		c.logger.Debug("Using cached CRD", "crdName", name)
		return crd, nil
	case missing:
		return nil, missingErr
	}

	c.logger.Debug("Looking up CRD", "crdName", name)

	// Define the CRD GVR directly to avoid recursion
	crdGVR := schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}

	crdObj, err := c.dynamicClient.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		c.logger.Debug("Failed to get CRD", "crdName", name, "error", err)

		if apierrors.IsNotFound(err) {
			c.crdsMu.Lock()
			c.missingCRDs[name] = err
			c.crdsMu.Unlock()
		}

		return nil, err
	}

	c.logger.Debug("Successfully retrieved CRD", "crdName", name)

	crdTyped := &extv1.CustomResourceDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(crdObj.Object, crdTyped); err != nil {
		c.logger.Debug("Error converting CRD", "crdName", name, "error", err)
		return nil, errors.Wrapf(err, "cannot convert CRD %s to typed", name)
	}

	return c.addCRD(crdTyped), nil
}

// GetAllCRDs returns every CRD fetched so far.
func (c *DefaultSchemaClient) GetAllCRDs() []*extv1.CustomResourceDefinition {
	c.crdsMu.RLock()
	defer c.crdsMu.RUnlock()

	// Return a copy to prevent external modification
	result := make([]*extv1.CustomResourceDefinition, len(c.crds))
	copy(result, c.crds)

	return result
}

// addCRD adds a CRD to the cache and returns the cached CRD of its name: crd, or the one a concurrent
// caller fetched first.
func (c *DefaultSchemaClient) addCRD(crd *extv1.CustomResourceDefinition) *extv1.CustomResourceDefinition {
	c.crdsMu.Lock()
	defer c.crdsMu.Unlock()

	// Check if already cached to avoid duplicates
	if cached, exists := c.crdByName[crd.Name]; exists {
		c.logger.Debug("CRD already in cache, skipping", "crdName", crd.Name)
		return cached
	}

	// Add to slice
	c.crds = append(c.crds, crd)

	// Add to name lookup map
	c.crdByName[crd.Name] = crd

	c.logger.Debug("Added CRD to cache", "crdName", crd.Name)

	return crd
}
