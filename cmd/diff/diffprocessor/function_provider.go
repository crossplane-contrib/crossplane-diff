/*
Copyright 2025 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package diffprocessor

import (
	"context"
	"strings"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
)

// FunctionProvider provides functions for rendering compositions.
// Different implementations can fetch functions on-demand or return cached functions.
type FunctionProvider interface {
	// GetFunctionsForComposition returns the functions needed to render a composition.
	GetFunctionsForComposition(comp *apiextensionsv1.Composition) ([]pkgv1.Function, error)

	// Cleaner stops and removes any resources created during function execution.
	// For providers that don't create resources (like DefaultFunctionProvider), this is a no-op.
	Cleaner
}

// EnvDockerNetwork is the environment variable that specifies which Docker
// network the crossplane-render container and function containers should
// join. NewEngineRenderFn reads this once and routes the value through
// render.EngineFlags.CrossplaneDockerNetwork; the upstream docker engine
// then both runs the render container on that network and annotates fns
// to join it at Setup time. This is needed when crossplane-diff runs
// inside a Docker container (e.g. a GitHub Actions container job).
const EnvDockerNetwork = "CROSSPLANE_DIFF_DOCKER_NETWORK"

// DefaultFunctionProvider fetches functions from the cluster on each call.
// This is appropriate for the xr command where each XR is processed independently.
type DefaultFunctionProvider struct {
	fnClient xp.FunctionClient
	logger   logging.Logger
}

// NewDefaultFunctionProvider creates a new DefaultFunctionProvider.
func NewDefaultFunctionProvider(fnClient xp.FunctionClient, logger logging.Logger) FunctionProvider {
	return &DefaultFunctionProvider{
		fnClient: fnClient,
		logger:   logger,
	}
}

// GetFunctionsForComposition fetches functions from the cluster.
func (p *DefaultFunctionProvider) GetFunctionsForComposition(comp *apiextensionsv1.Composition) ([]pkgv1.Function, error) {
	p.logger.Debug("Fetching functions from pipeline", "composition", comp.GetName())

	fns, err := p.fnClient.GetFunctionsFromPipeline(comp)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get functions from pipeline")
	}

	p.logger.Debug("Fetched functions from pipeline", "composition", comp.GetName(), "count", len(fns))

	return fns, nil
}

// Cleanup is a no-op for DefaultFunctionProvider as it doesn't create any resources.
func (p *DefaultFunctionProvider) Cleanup(_ context.Context) error {
	return nil
}

// CachedFunctionProvider lazy-loads functions and caches them by composition name.
// This is appropriate for the comp command, where many XRs use the same composition:
// the cluster is asked for a composition's functions once, however many XRs render it.
// The function containers themselves are started, reused across renders and removed
// by the render engine, which names them for the run that owns them (see EngineRenderFn).
type CachedFunctionProvider struct {
	fnClient xp.FunctionClient
	cache    map[string][]pkgv1.Function
	logger   logging.Logger
}

// NewCachedFunctionProvider creates a new CachedFunctionProvider.
func NewCachedFunctionProvider(fnClient xp.FunctionClient, logger logging.Logger) FunctionProvider {
	return &CachedFunctionProvider{
		fnClient: fnClient,
		cache:    make(map[string][]pkgv1.Function),
		logger:   logger,
	}
}

// GetFunctionsForComposition fetches and caches functions on first call per composition.
func (p *CachedFunctionProvider) GetFunctionsForComposition(comp *apiextensionsv1.Composition) ([]pkgv1.Function, error) {
	compName := comp.GetName()

	if cached, ok := p.cache[compName]; ok {
		p.logger.Debug("Using cached functions", "composition", compName, "count", len(cached))
		return cached, nil
	}

	// Cache miss - fetch and cache functions
	p.logger.Debug("Fetching functions for caching", "composition", compName)

	fns, err := p.fnClient.GetFunctionsFromPipeline(comp)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get functions from pipeline")
	}

	p.logger.Debug("Fetched functions for caching", "composition", compName, "count", len(fns))

	p.cache[compName] = fns

	return fns, nil
}

// Cleanup is a no-op for CachedFunctionProvider: the render engine owns the containers.
func (p *CachedFunctionProvider) Cleanup(_ context.Context) error {
	return nil
}

// RegistryOverrideFunctionProvider wraps another FunctionProvider and replaces
// the registry in each function's package reference before returning them.
type RegistryOverrideFunctionProvider struct {
	inner    FunctionProvider
	registry string
	logger   logging.Logger
}

// NewRegistryOverrideFunctionProvider wraps inner, replacing the registry
// portion of every function package ref with the given registry.
func NewRegistryOverrideFunctionProvider(inner FunctionProvider, registry string, logger logging.Logger) FunctionProvider {
	return &RegistryOverrideFunctionProvider{
		inner:    inner,
		registry: registry,
		logger:   logger,
	}
}

// GetFunctionsForComposition delegates to the wrapped provider and rewrites
// the registry portion of each returned function's package ref.
func (p *RegistryOverrideFunctionProvider) GetFunctionsForComposition(comp *apiextensionsv1.Composition) ([]pkgv1.Function, error) {
	fns, err := p.inner.GetFunctionsForComposition(comp)
	if err != nil {
		return nil, err
	}

	for i := range fns {
		orig := fns[i].Spec.Package

		replaced := replaceRegistry(orig, p.registry)
		if replaced != orig {
			p.logger.Debug("Overriding function registry",
				"function", fns[i].GetName(),
				"from", orig,
				"to", replaced)
			fns[i].Spec.Package = replaced
		}
	}

	return fns, nil
}

// Cleanup delegates to the wrapped provider.
func (p *RegistryOverrideFunctionProvider) Cleanup(ctx context.Context) error {
	return p.inner.Cleanup(ctx)
}

// replaceRegistry replaces the registry portion of an OCI package reference,
// preserving the repository path, tag, and/or digest. A trailing slash on
// newRegistry is trimmed.
//
// The first path component is treated as a registry host only when it follows
// the standard OCI rule: it contains a '.' or ':', or it is exactly
// "localhost". Otherwise the ref has no explicit registry (e.g.
// "crossplane-contrib/function-auto-ready:v1.0.0") and newRegistry is prepended
// to the full ref instead of replacing the first path segment.
func replaceRegistry(pkg, newRegistry string) string {
	newRegistry = strings.TrimRight(newRegistry, "/")

	idx := strings.Index(pkg, "/")
	if idx < 0 {
		return newRegistry + "/" + pkg
	}

	first := pkg[:idx]
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return newRegistry + "/" + pkg
	}

	return newRegistry + pkg[idx:]
}
