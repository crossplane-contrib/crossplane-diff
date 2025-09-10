package diffprocessor

import (
	"context"
	"fmt"
	"strings"
	"sync"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	v1 "github.com/crossplane/crossplane/v2/proto/fn/v1"
)

// RequirementsProvider consolidates requirement processing with caching.
type RequirementsProvider struct {
	client    k8.ResourceClient
	envClient xp.EnvironmentClient
	renderFn  RenderFunc
	logger    logging.Logger

	// Resource cache by resource key (apiVersion+kind+name)
	resourceCache map[string]*un.Unstructured
	cacheMutex    sync.RWMutex
}

// NewRequirementsProvider creates a new provider with caching.
func NewRequirementsProvider(res k8.ResourceClient, env xp.EnvironmentClient, renderFn RenderFunc, logger logging.Logger) *RequirementsProvider {
	return &RequirementsProvider{
		client:        res,
		envClient:     env,
		renderFn:      renderFn,
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

// cacheResources adds resources to the cache.
func (p *RequirementsProvider) cacheResources(resources []*un.Unstructured) {
	p.cacheMutex.Lock()
	defer p.cacheMutex.Unlock()

	for _, res := range resources {
		key := fmt.Sprintf("%s/%s/%s", res.GetAPIVersion(), res.GetKind(), res.GetName())
		p.resourceCache[key] = res
	}
}

// getCachedResource retrieves a resource from cache if available.
func (p *RequirementsProvider) getCachedResource(apiVersion, kind, name string) *un.Unstructured {
	p.cacheMutex.RLock()
	defer p.cacheMutex.RUnlock()

	key := fmt.Sprintf("%s/%s/%s", apiVersion, kind, name)
	return p.resourceCache[key]
}

// ProvideRequirements provides requirements, checking cache first.
func (p *RequirementsProvider) ProvideRequirements(ctx context.Context, requirements map[string]v1.Requirements, xrNamespace string) ([]*un.Unstructured, error) {
	if len(requirements) == 0 {
		p.logger.Debug("No requirements provided, returning empty")
		return nil, nil
	}

	allResources, newlyFetchedResources, err := p.processAllSteps(ctx, requirements, xrNamespace)
	if err != nil {
		return nil, err
	}

	// Cache any newly fetched resources
	if len(newlyFetchedResources) > 0 {
		p.cacheResources(newlyFetchedResources)
	}

	p.logger.Debug("Processed all requirements",
		"resourceCount", len(allResources),
		"newlyFetchedCount", len(newlyFetchedResources),
		"cacheSize", len(p.resourceCache))

	return allResources, nil
}

// processAllSteps processes requirements for all steps without copying protobuf structs.
func (p *RequirementsProvider) processAllSteps(ctx context.Context, requirements map[string]v1.Requirements, xrNamespace string) ([]*un.Unstructured, []*un.Unstructured, error) {
	var allResources []*un.Unstructured
	var newlyFetchedResources []*un.Unstructured

	// Process each step's requirements
	for stepName := range requirements {
		stepResources, stepNewlyFetched, err := p.processStepSelectors(
			ctx,
			stepName,
			requirements[stepName].Resources, //nolint:protogetter // Direct field access required for protobuf struct values
			// to avoid copying mutexes.  also, we need to keep using ExtraResources since we aren't guaranteed that all
			// functions in our pipeline have been upgraded to v2.
			requirements[stepName].ExtraResources, //nolint:staticcheck,protogetter // ExtraResources deprecated but needed for backward compatibility
			xrNamespace,
		)
		if err != nil {
			return nil, nil, err
		}

		allResources = append(allResources, stepResources...)
		newlyFetchedResources = append(newlyFetchedResources, stepNewlyFetched...)
	}

	return allResources, newlyFetchedResources, nil
}

// processStepSelectors processes selectors from Resources and ExtraResources maps.
func (p *RequirementsProvider) processStepSelectors(ctx context.Context, stepName string, resources, extraResources map[string]*v1.ResourceSelector, xrNamespace string) ([]*un.Unstructured, []*un.Unstructured, error) {
	totalSelectors := len(resources) + len(extraResources)

	p.logger.Debug("Processing step requirements",
		"step", stepName,
		"resources", len(resources),
		"extraResources", len(extraResources),
		"total", totalSelectors)

	var stepResources []*un.Unstructured
	var newlyFetched []*un.Unstructured

	// Process Resources selectors
	for resourceKey, selector := range resources {
		res, fetched, err := p.processSelector(ctx, stepName, resourceKey, selector, xrNamespace)
		if err != nil {
			return nil, nil, err
		}
		stepResources = append(stepResources, res...)
		newlyFetched = append(newlyFetched, fetched...)
	}

	// Process ExtraResources selectors (deprecated but backward compatible)
	for resourceKey, selector := range extraResources {
		res, fetched, err := p.processSelector(ctx, stepName, resourceKey, selector, xrNamespace)
		if err != nil {
			return nil, nil, err
		}
		stepResources = append(stepResources, res...)
		newlyFetched = append(newlyFetched, fetched...)
	}

	return stepResources, newlyFetched, nil
}

// processSelector processes a single resource selector.
func (p *RequirementsProvider) processSelector(ctx context.Context, stepName, resourceKey string, selector *v1.ResourceSelector, xrNamespace string) ([]*un.Unstructured, []*un.Unstructured, error) {
	if selector == nil {
		p.logger.Debug("Nil selector in requirements",
			"step", stepName,
			"resourceKey", resourceKey)
		return nil, nil, nil
	}

	gvk := schema.GroupVersionKind{
		Group:   parseGroupFromAPIVersion(selector.GetApiVersion()),
		Version: parseVersionFromAPIVersion(selector.GetApiVersion()),
		Kind:    selector.GetKind(),
	}

	switch {
	case selector.GetMatchName() != "":
		resources, err := p.processNameSelector(ctx, selector, gvk, xrNamespace, stepName)
		if err != nil {
			return nil, nil, err
		}

		// Check if any resources were fetched (not from cache)
		var newlyFetched []*un.Unstructured
		if !p.isResourceCached(selector.GetApiVersion(), selector.GetKind(), selector.GetMatchName()) {
			newlyFetched = resources
		}

		return resources, newlyFetched, nil

	case selector.GetMatchLabels() != nil:
		resources, err := p.processLabelSelector(ctx, selector, gvk, xrNamespace, stepName)
		if err != nil {
			return nil, nil, errors.Wrap(err, "cannot get resources by label")
		}

		// Label selector results are always newly fetched (can't cache efficiently)
		return resources, resources, nil

	default:
		p.logger.Debug("Unsupported selector type",
			"step", stepName,
			"resourceKey", resourceKey)
		return nil, nil, nil
	}
}

// isResourceCached checks if a resource is in the cache.
func (p *RequirementsProvider) isResourceCached(apiVersion, kind, name string) bool {
	return p.getCachedResource(apiVersion, kind, name) != nil
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

// ClearCache clears all cached resources.
func (p *RequirementsProvider) ClearCache() {
	p.cacheMutex.Lock()
	defer p.cacheMutex.Unlock()

	p.resourceCache = make(map[string]*un.Unstructured)
	p.logger.Debug("Resource cache cleared")
}

// resolveNamespace determines the appropriate namespace for a resource based on its scope and selector.
func (p *RequirementsProvider) resolveNamespace(ctx context.Context, gvk schema.GroupVersionKind, selector *v1.ResourceSelector, xrNamespace, stepName string) (string, error) {
	isNamespaced, err := p.client.IsNamespacedResource(ctx, gvk)
	if err != nil {
		return "", errors.Wrapf(err, "cannot determine namespace scope for resource %s when processing requirements for step %s", gvk.String(), stepName)
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
func (p *RequirementsProvider) processNameSelector(ctx context.Context, selector *v1.ResourceSelector, gvk schema.GroupVersionKind, xrNamespace, stepName string) ([]*un.Unstructured, error) {
	name := selector.GetMatchName()

	// Try cache first
	if cached := p.getCachedResource(selector.GetApiVersion(), selector.GetKind(), name); cached != nil {
		p.logger.Debug("Found resource in cache",
			"apiVersion", selector.GetApiVersion(),
			"kind", selector.GetKind(),
			"name", name)
		return []*un.Unstructured{cached}, nil
	}

	// Resolve namespace
	ns, err := p.resolveNamespace(ctx, gvk, selector, xrNamespace, stepName)
	if err != nil {
		return nil, err
	}

	p.logger.Debug("Fetching reference by name",
		"gvk", gvk.String(),
		"name", name,
		"namespace", ns)

	resource, err := p.client.GetResource(ctx, gvk, ns, name)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot get referenced resource %s/%s", ns, name)
	}

	return []*un.Unstructured{resource}, nil
}

// processLabelSelector handles resource selection by labels.
func (p *RequirementsProvider) processLabelSelector(ctx context.Context, selector *v1.ResourceSelector, gvk schema.GroupVersionKind, xrNamespace, stepName string) ([]*un.Unstructured, error) {
	labelSelector := metav1.LabelSelector{
		MatchLabels: selector.GetMatchLabels().GetLabels(),
	}

	// Resolve namespace
	ns, err := p.resolveNamespace(ctx, gvk, selector, xrNamespace, stepName)
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
