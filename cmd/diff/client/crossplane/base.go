// Package crossplane contains interfaces and implementations for clients that talk to Kubernetes about Crossplane
// primitives, often by consuming clients from the kubernetes package.
package crossplane

import (
	"context"
	"fmt"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
)

// Initialize initializes all the clients in this bundle.
func (c *Clients) Initialize(ctx context.Context, logger logging.Logger) error {
	return core.InitializeClients(ctx, logger,
		// definition client before composition client since it's a dependency
		c.Definition,
		c.Composition,
		c.Environment,
		c.Function,
		c.ResourceTree,
	)
}

// Clients is an aggregation of all of our Crossplane clients, used to pass them as a bundle,
// typically for initialization where the consumer can select which ones they need.
type Clients struct {
	Composition  CompositionClient
	Definition   DefinitionClient
	Environment  EnvironmentClient
	Function     FunctionClient
	ResourceTree ResourceTreeClient
}

func cacheKey(namespace, name string) string {
	// Create a unique cache key based on namespace and name
	return fmt.Sprintf("%s/%s", namespace, name)
}

// getFirstMatchingResource retrieves the first resource matching the given GVKs and name.
func getFirstMatchingResource(ctx context.Context, client kubernetes.ResourceClient, gvks []schema.GroupVersionKind, name, namespace string, cache map[string]*un.Unstructured) (*un.Unstructured, error) {
	key := cacheKey(namespace, name)

	// Check cache first
	if config, ok := cache[key]; ok {
		return config, nil
	}

	var errs []error
	for _, gvk := range gvks {
		// try for this version
		item, err := client.GetResource(ctx, gvk, namespace, name)
		if err != nil {
			errs = append(errs, errors.Wrapf(err, "cannot get %s %s", gvk.String(), name))
			// not found in this version, try next
			continue
		}

		// Found item; update cache
		cache[key] = item

		return item, nil
	}

	return nil, errors.Join(errs...) // return all errors if none found
}

func listMatchingResources(ctx context.Context, client kubernetes.ResourceClient, gvks []schema.GroupVersionKind, namespace string) ([]*un.Unstructured, error) {
	// List all matching resources for the given GVKs
	var envConfigs []*un.Unstructured
	for _, gvk := range gvks {
		ecs, err := client.ListResources(ctx, gvk, namespace)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot list items for GVK %s", gvk.String())
		}
		envConfigs = append(envConfigs, ecs...)
	}

	return envConfigs, nil
}
