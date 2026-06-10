package diffprocessor

import (
	"context"
	"strconv"
	"strings"
	"sync"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	v1 "github.com/crossplane/function-sdk-go/proto/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// addUniqueResource adds a resource to the map if not already present.
// Returns true if the resource was new. Used by RenderToStableState
// to deduplicate required resources.
func addUniqueResource(m map[string]un.Unstructured, res *un.Unstructured) bool {
	if res == nil {
		return false
	}

	key := dt.MakeDiffKeyFromResource(res)
	if _, exists := m[key]; !exists {
		m[key] = *res

		return true
	}

	return false
}

// RequirementsProvider consolidates requirement processing with caching.
type RequirementsProvider struct {
	client    k8.ResourceClient
	envClient xp.EnvironmentClient
	logger    logging.Logger

	// Resource cache by resource key (apiVersion+kind+namespace+name — see
	// dt.MakeDiffKey). Namespace is required so same-named resources in
	// different namespaces don't collide.
	resourceCache map[string]*un.Unstructured
	cacheMutex    sync.RWMutex
}

// NewRequirementsProvider creates a new provider with caching.
func NewRequirementsProvider(res k8.ResourceClient, env xp.EnvironmentClient, logger logging.Logger) *RequirementsProvider {
	return &RequirementsProvider{
		client:        res,
		envClient:     env,
		logger:        logger,
		resourceCache: make(map[string]*un.Unstructured),
	}
}

// Initialize pre-fetches resources like environment configs.
func (p *RequirementsProvider) Initialize(ctx context.Context) error {
	p.logger.Debug("Initializing extra resource provider")

	// Pre-fetch environment configs
	envConfigs, err := p.envClient.GetEnvironmentConfigs(ctx)
	if err != nil {
		return errors.Wrap(err, "cannot get environment configs")
	}

	// Add to cache
	p.cacheResources(envConfigs)

	p.logger.Debug("Extra resource provider initialized",
		"envConfigCount", len(envConfigs),
		"cacheSize", len(p.resourceCache))

	return nil
}

// ClearCache clears all cached resources.
func (p *RequirementsProvider) ClearCache() {
	p.cacheMutex.Lock()
	defer p.cacheMutex.Unlock()

	p.resourceCache = make(map[string]*un.Unstructured)
	p.logger.Debug("Resource cache cleared")
}

// cacheResources adds resources to the cache.
func (p *RequirementsProvider) cacheResources(resources []*un.Unstructured) {
	p.cacheMutex.Lock()
	defer p.cacheMutex.Unlock()

	for _, res := range resources {
		key := dt.MakeDiffKeyFromResource(res)
		p.resourceCache[key] = res
	}
}

// getCachedResource retrieves a resource from cache if available.
func (p *RequirementsProvider) getCachedResource(apiVersion, kind, namespace, name string) *un.Unstructured {
	p.cacheMutex.RLock()
	defer p.cacheMutex.RUnlock()

	key := dt.MakeDiffKey(apiVersion, kind, namespace, name)

	return p.resourceCache[key]
}

// ResolveSelectors resolves a flat list of ResourceSelector entries into their
// backing resources. Checks the cache first; on a miss it defers to the
// per-selector fetcher and caches the result.
//
// Matches the render.CompositionOutputs.RequiredResources shape introduced in
// upstream crossplane PR #7339.
func (p *RequirementsProvider) ResolveSelectors(ctx context.Context, selectors []*v1.ResourceSelector, xrNamespace string) ([]*un.Unstructured, error) {
	if len(selectors) == 0 {
		p.logger.Debug("No selectors provided, returning empty")
		return nil, nil
	}

	var (
		allResources          []*un.Unstructured
		newlyFetchedResources []*un.Unstructured
	)

	for i, selector := range selectors {
		res, fetched, err := p.processSelector(ctx, strconv.Itoa(i), selector, xrNamespace)
		if err != nil {
			return nil, err
		}

		allResources = append(allResources, res...)
		newlyFetchedResources = append(newlyFetchedResources, fetched...)
	}

	if len(newlyFetchedResources) > 0 {
		p.cacheResources(newlyFetchedResources)
	}

	p.logger.Debug("Resolved selectors",
		"selectorCount", len(selectors),
		"resourceCount", len(allResources),
		"newlyFetchedCount", len(newlyFetchedResources),
		"cacheSize", len(p.resourceCache))

	return allResources, nil
}

// processSelector processes a single resource selector. resourceKey is a
// short identifier (typically the selector's index in the parent slice) used
// only for debug logging.
func (p *RequirementsProvider) processSelector(ctx context.Context, resourceKey string, selector *v1.ResourceSelector, xrNamespace string) ([]*un.Unstructured, []*un.Unstructured, error) {
	if selector == nil {
		p.logger.Debug("Nil selector in requirements", "resourceKey", resourceKey)
		return nil, nil, nil
	}

	gvk := schema.GroupVersionKind{
		Group:   parseGroupFromAPIVersion(selector.GetApiVersion()),
		Version: parseVersionFromAPIVersion(selector.GetApiVersion()),
		Kind:    selector.GetKind(),
	}

	switch {
	case selector.GetMatchName() != "":
		resources, fromCache, err := p.processNameSelector(ctx, selector, gvk, xrNamespace)
		if err != nil {
			return nil, nil, err
		}

		// Return resources as newly fetched only if not from cache
		var newlyFetched []*un.Unstructured
		if !fromCache {
			newlyFetched = resources
		}

		return resources, newlyFetched, nil

	case selector.GetMatchLabels() != nil:
		resources, err := p.processLabelSelector(ctx, selector, gvk, xrNamespace)
		if err != nil {
			return nil, nil, errors.Wrap(err, "cannot get resources by label")
		}

		// Label selector results are always newly fetched (can't cache efficiently)
		return resources, resources, nil

	default:
		p.logger.Debug("Unsupported selector type", "resourceKey", resourceKey)
		return nil, nil, nil
	}
}

// parseGroupFromAPIVersion extracts the group from an apiVersion string.
func parseGroupFromAPIVersion(apiVersion string) string {
	group, _ := parseAPIVersion(apiVersion)
	return group
}

// parseVersionFromAPIVersion extracts the version from an apiVersion string.
func parseVersionFromAPIVersion(apiVersion string) string {
	_, version := parseAPIVersion(apiVersion)
	return version
}

// resolveNamespace determines the appropriate namespace for a resource based on its scope and selector.
func (p *RequirementsProvider) resolveNamespace(ctx context.Context, gvk schema.GroupVersionKind, selector *v1.ResourceSelector, xrNamespace string) (string, error) {
	isNamespaced, err := p.client.IsNamespacedResource(ctx, gvk)
	if err != nil {
		return "", errors.Wrapf(err, "cannot determine namespace scope for resource %s", gvk.String())
	}

	if !isNamespaced {
		return "", nil
	}

	if selector.GetNamespace() != "" {
		return selector.GetNamespace(), nil
	}

	return xrNamespace, nil
}

// processNameSelector handles resource selection by name.
// Returns (resources, fromCache, error) where fromCache indicates if the resource was found in cache.
func (p *RequirementsProvider) processNameSelector(ctx context.Context, selector *v1.ResourceSelector, gvk schema.GroupVersionKind, xrNamespace string) ([]*un.Unstructured, bool, error) {
	name := selector.GetMatchName()

	// Resolve namespace FIRST so we can check cache correctly.
	// This prevents returning wrong resources when same-named resources exist
	// in different namespaces (e.g., ConfigMap/my-config in both ns-a and ns-b).
	ns, err := p.resolveNamespace(ctx, gvk, selector, xrNamespace)
	if err != nil {
		return nil, false, err
	}

	// Now check cache with correct namespace
	if cached := p.getCachedResource(selector.GetApiVersion(), selector.GetKind(), ns, name); cached != nil {
		p.logger.Debug("Found resource in cache",
			"apiVersion", selector.GetApiVersion(),
			"kind", selector.GetKind(),
			"namespace", ns,
			"name", name)

		return []*un.Unstructured{cached}, true, nil
	}

	p.logger.Debug("Fetching reference by name",
		"gvk", gvk.String(),
		"name", name,
		"namespace", ns)

	resource, err := p.client.GetResource(ctx, gvk, ns, name)
	if err != nil {
		return nil, false, errors.Wrapf(err, "cannot get referenced resource %s/%s", ns, name)
	}

	return []*un.Unstructured{resource}, false, nil
}

// processLabelSelector handles resource selection by labels.
func (p *RequirementsProvider) processLabelSelector(ctx context.Context, selector *v1.ResourceSelector, gvk schema.GroupVersionKind, xrNamespace string) ([]*un.Unstructured, error) {
	labelSelector := metav1.LabelSelector{
		MatchLabels: selector.GetMatchLabels().GetLabels(),
	}

	// Resolve namespace
	ns, err := p.resolveNamespace(ctx, gvk, selector, xrNamespace)
	if err != nil {
		return nil, err
	}

	p.logger.Debug("Fetching resources by label",
		"gvk", gvk.String(),
		"labels", labelSelector.MatchLabels,
		"namespace", ns)

	return p.client.GetResourcesByLabel(ctx, gvk, ns, labelSelector)
}

// Helper to parse apiVersion into group and version.
func parseAPIVersion(apiVersion string) (string, string) {
	var group, version string
	if parts := strings.SplitN(apiVersion, "/", 2); len(parts) == 2 {
		// Normal case: group/version (e.g., "apps/v1")
		group, version = parts[0], parts[1]
	} else {
		// Core case: version only (e.g., "v1")
		version = apiVersion
	}

	return group, version
}
