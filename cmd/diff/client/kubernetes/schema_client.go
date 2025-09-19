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

	xpextv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	xpextv2 "github.com/crossplane/crossplane/v2/apis/apiextensions/v2"
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

// extractGVKsFromXRD extracts all GroupVersionKinds from an XRD using strongly-typed conversion.
// This method handles both v1 and v2 XRDs and leverages Kubernetes runtime conversion.
func extractGVKsFromXRD(xrd *un.Unstructured) ([]schema.GroupVersionKind, error) {
	apiVersion := xrd.GetAPIVersion()

	switch apiVersion {
	case "apiextensions.crossplane.io/v1":
		return extractGVKsFromV1XRD(xrd)
	case "apiextensions.crossplane.io/v2":
		return extractGVKsFromV2XRD(xrd)
	default:
		return nil, errors.Errorf("unsupported XRD apiVersion %s in XRD %s", apiVersion, xrd.GetName())
	}
}

// extractGVKsFromV1XRD extracts GVKs from a v1 XRD using strongly-typed conversion.
func extractGVKsFromV1XRD(xrd *un.Unstructured) ([]schema.GroupVersionKind, error) {
	typedXRD := &xpextv1.CompositeResourceDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(xrd.Object, typedXRD); err != nil {
		return nil, errors.Wrapf(err, "cannot convert XRD %s to v1 typed object", xrd.GetName())
	}

	// Extract GVKs for each version - no validation needed since XRDs from server are guaranteed valid
	gvks := make([]schema.GroupVersionKind, 0, len(typedXRD.Spec.Versions))
	for _, version := range typedXRD.Spec.Versions {
		gvks = append(gvks, schema.GroupVersionKind{
			Group:   typedXRD.Spec.Group,
			Version: version.Name,
			Kind:    typedXRD.Spec.Names.Kind,
		})
	}

	return gvks, nil
}

// extractGVKsFromV2XRD extracts GVKs from a v2 XRD using strongly-typed conversion.
func extractGVKsFromV2XRD(xrd *un.Unstructured) ([]schema.GroupVersionKind, error) {
	typedXRD := &xpextv2.CompositeResourceDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(xrd.Object, typedXRD); err != nil {
		return nil, errors.Wrapf(err, "cannot convert XRD %s to v2 typed object", xrd.GetName())
	}

	// Extract GVKs for each version - no validation needed since XRDs from server are guaranteed valid
	gvks := make([]schema.GroupVersionKind, 0, len(typedXRD.Spec.Versions))
	for _, version := range typedXRD.Spec.Versions {
		gvks = append(gvks, schema.GroupVersionKind{
			Group:   typedXRD.Spec.Group,
			Version: version.Name,
			Kind:    typedXRD.Spec.Names.Kind,
		})
	}

	return gvks, nil
}

// LoadCRDsFromXRDs fetches corresponding CRDs from the cluster for the given XRDs and caches them.
// Instead of converting XRDs to CRDs, this method fetches the actual CRDs that should already
// exist in the cluster since the Crossplane control plane manages both XRDs and their corresponding CRDs.
func (c *DefaultSchemaClient) LoadCRDsFromXRDs(ctx context.Context, xrds []*un.Unstructured) error {
	c.logger.Debug("Loading CRDs from cluster for XRDs", "xrdCount", len(xrds))

	if len(xrds) == 0 {
		c.logger.Debug("No XRDs provided, nothing to load")
		return nil
	}

	// Extract group-version-kinds from XRDs to find corresponding CRDs
	// Fail fast on any invalid XRD - per repository guidelines: never continue in degraded state
	var crdsToFetch []schema.GroupVersionKind

	for _, xrd := range xrds {
		gvks, err := extractGVKsFromXRD(xrd)
		if err != nil {
			return err // Error already wrapped with context from extractGVKsFromXRD
		}

		crdsToFetch = append(crdsToFetch, gvks...)
	}

	c.logger.Debug("Identified GVKs to fetch CRDs for", "count", len(crdsToFetch))

	// Fetch CRDs from cluster for each GVK - fail fast if any CRD is missing
	// Per repository guidelines: never continue in a degraded state
	fetchedCRDs := make([]*extv1.CustomResourceDefinition, 0, len(crdsToFetch))

	for _, gvk := range crdsToFetch {
		crd, err := c.GetCRD(ctx, gvk)
		if err != nil {
			c.logger.Debug("Failed to fetch required CRD for GVK", "gvk", gvk.String(), "error", err)
			return errors.Wrapf(err, "cannot fetch required CRD for %s", gvk.String())
		}

		fetchedCRDs = append(fetchedCRDs, crd)
	}

	c.logger.Debug("Successfully fetched all required CRDs from cluster", "count", len(fetchedCRDs))

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
