package kubernetes

import (
	"context"
	"fmt"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// ResourceClient handles basic CRUD operations for Kubernetes resources.
type ResourceClient interface {
	// GetResource retrieves a resource by its GVK, namespace, and name
	GetResource(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*un.Unstructured, error)

	// ListResources lists resources matching the given GVK and namespace
	ListResources(ctx context.Context, gvk schema.GroupVersionKind, namespace string) ([]*un.Unstructured, error)

	// GetResourcesByLabel returns resources matching labels in the given namespace
	GetResourcesByLabel(ctx context.Context, gvk schema.GroupVersionKind, namespace string, sel metav1.LabelSelector) ([]*un.Unstructured, error)

	// GetGVKsForGroupKind retrieves all GroupVersionKinds for a given group and kind
	GetGVKsForGroupKind(ctx context.Context, group, kind string) ([]schema.GroupVersionKind, error)

	// IsNamespacedResource determines if a given GVK represents a namespaced resource
	IsNamespacedResource(ctx context.Context, gvk schema.GroupVersionKind) (bool, error)
}

// DefaultResourceClient implements the ResourceClient interface.
type DefaultResourceClient struct {
	dynamicClient   dynamic.Interface
	discoveryClient discovery.DiscoveryInterface
	converter       TypeConverter
	logger          logging.Logger
}

// NewResourceClient creates a new DefaultResourceClient instance.
func NewResourceClient(clients *core.Clients, converter TypeConverter, logger logging.Logger) ResourceClient {
	return &DefaultResourceClient{
		dynamicClient:   clients.Dynamic,
		discoveryClient: clients.Discovery,
		converter:       converter,
		logger:          logger,
	}
}

// GetResource retrieves a resource from the cluster based on its GVK, namespace, and name.
func (c *DefaultResourceClient) GetResource(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*un.Unstructured, error) {
	resourceID := fmt.Sprintf("%s/%s/%s", gvk.String(), namespace, name)
	c.logger.Debug("Getting resource from cluster", "resource", resourceID)

	// Convert GVK to GVR
	gvr, err := c.converter.GVKToGVR(ctx, gvk)
	if err != nil {
		c.logger.Debug("Failed to convert GVK to GVR", "gvk", gvk.String(), "error", err)
		return nil, errors.Wrapf(err, "cannot get resource %s/%s of kind %s", namespace, name, gvk.Kind)
	}

	// Get the resource
	res, err := c.dynamicClient.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		c.logger.Debug("Failed to get resource", "resource", resourceID, "error", err)
		return nil, errors.Wrapf(err, "cannot get resource %s/%s of kind %s", namespace, name, gvk.Kind)
	}

	c.logger.Debug("Retrieved resource",
		"resource", resourceID,
		"uid", res.GetUID(),
		"resourceVersion", res.GetResourceVersion())

	return res, nil
}

// GetResourcesByLabel returns resources matching labels in the given namespace.
func (c *DefaultResourceClient) GetResourcesByLabel(ctx context.Context, gvk schema.GroupVersionKind, namespace string, sel metav1.LabelSelector) ([]*un.Unstructured, error) {
	c.logger.Debug("Getting resources by label",
		"namespace", namespace,
		"gvk", gvk.String(),
		"selector", sel.MatchLabels)

	// Convert GVK to GVR
	gvr, err := c.converter.GVKToGVR(ctx, gvk)
	if err != nil {
		c.logger.Debug("Failed to convert GVK to GVR", "gvk", gvk.String(), "error", err)
		return nil, errors.Wrapf(err, "cannot list resources for '%s' matching labels", gvk.String())
	}

	// Create list options with label selector
	opts := metav1.ListOptions{}
	if len(sel.MatchLabels) > 0 {
		opts.LabelSelector = metav1.FormatLabelSelector(&sel)
	}

	// Perform the list operation
	list, err := c.dynamicClient.Resource(gvr).Namespace(namespace).List(ctx, opts)
	if err != nil {
		c.logger.Debug("Failed to list resources", "gvk", gvk.String(), "namespace", namespace, "labelSelector", opts.LabelSelector, "error", err)
		return nil, errors.Wrapf(err, "cannot list resources for '%s/%s' matching '%s'", namespace, gvk.String(), opts.LabelSelector)
	}

	// Convert the list items to a slice of pointers
	resources := make([]*un.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		resources = append(resources, &list.Items[i])
	}

	c.logger.Debug("Resources found by label", "count", len(resources), "gvk", gvk.String())

	return resources, nil
}

// ListResources lists resources matching the given GVK and namespace.
func (c *DefaultResourceClient) ListResources(ctx context.Context, gvk schema.GroupVersionKind, namespace string) ([]*un.Unstructured, error) {
	c.logger.Debug("Listing resources", "gvk", gvk.String(), "namespace", namespace)

	// Convert GVK to GVR
	gvr, err := c.converter.GVKToGVR(ctx, gvk)
	if err != nil {
		c.logger.Debug("Failed to convert GVK to GVR", "gvk", gvk.String(), "error", err)
		return nil, errors.Wrapf(err, "cannot list resources for '%s'", gvk.String())
	}

	// Perform the list operation
	list, err := c.dynamicClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Debug("Failed to list resources", "gvk", gvk.String(), "namespace", namespace, "error", err)
		return nil, errors.Wrapf(err, "cannot list resources for '%s'", gvk.String())
	}

	// Convert from items to slice of pointers
	resources := make([]*un.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		resources = append(resources, &list.Items[i])
	}

	c.logger.Debug("Listed resources", "gvk", gvk.String(), "namespace", namespace, "count", len(resources))

	return resources, nil
}

// GetGVKsForGroupKind returns all available GroupVersionKinds for a given group and kind.
// It queries only the specified API group rather than all server APIs, avoiding issues
// with stale discovery data in unrelated API groups.
func (c *DefaultResourceClient) GetGVKsForGroupKind(_ context.Context, group, kind string) ([]schema.GroupVersionKind, error) {
	// Get API groups (lightweight - doesn't fetch all resources)
	apiGroups, err := c.discoveryClient.ServerGroups()
	if err != nil {
		return nil, errors.Wrap(err, "cannot get server groups")
	}

	// Find the target group and its versions
	var targetGroup *metav1.APIGroup

	for i := range apiGroups.Groups {
		if apiGroups.Groups[i].Name == group {
			targetGroup = &apiGroups.Groups[i]
			break
		}
	}

	if targetGroup == nil {
		return nil, errors.Errorf("API group %q not found on server", group)
	}

	var gvks []schema.GroupVersionKind

	// Query only the versions of the target group
	for _, version := range targetGroup.Versions {
		resourceList, err := c.discoveryClient.ServerResourcesForGroupVersion(version.GroupVersion)
		if err != nil {
			c.logger.Debug("Failed to get resources for group version", "groupVersion", version.GroupVersion, "error", err)
			continue
		}

		for _, resource := range resourceList.APIResources {
			if resource.Kind == kind {
				gv, err := schema.ParseGroupVersion(version.GroupVersion)
				if err != nil {
					continue
				}

				gvk := schema.GroupVersionKind{
					Group:   gv.Group,
					Version: gv.Version,
					Kind:    kind,
				}
				gvks = append(gvks, gvk)

				break // Found the kind in this version, move to next version
			}
		}
	}

	return gvks, nil
}

// IsNamespacedResource determines if a given GVK represents a namespaced resource
// by querying the cluster's discovery API.
func (c *DefaultResourceClient) IsNamespacedResource(_ context.Context, gvk schema.GroupVersionKind) (bool, error) {
	// Get the server resources for this group/version
	groupVersion := gvk.GroupVersion().String()

	resourceList, err := c.discoveryClient.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		return false, errors.Wrapf(err, "cannot get server resources for group version %s", groupVersion)
	}

	// Find the resource matching our kind
	for _, resource := range resourceList.APIResources {
		if resource.Kind == gvk.Kind {
			// The Namespaced field indicates whether the resource is namespaced
			c.logger.Debug("Determined resource scope from discovery",
				"gvk", gvk.String(),
				"namespaced", resource.Namespaced)

			return resource.Namespaced, nil
		}
	}

	// If we can't find the resource, this is an error condition that should fail the diff
	availableKinds := make([]string, len(resourceList.APIResources))
	for i, resource := range resourceList.APIResources {
		availableKinds[i] = resource.Kind
	}

	return false, errors.Errorf("resource kind %s not found in discovery API for group version %s (available kinds: %v)", gvk.Kind, groupVersion, availableKinds)
}
