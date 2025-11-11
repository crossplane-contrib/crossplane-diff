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
	"strings"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
)

// FunctionProvider provides functions for rendering compositions.
// Different implementations can fetch functions on-demand or return cached functions.
type FunctionProvider interface {
	// GetFunctionsForComposition returns the functions needed to render a composition.
	GetFunctionsForComposition(comp *apiextensionsv1.Composition) ([]pkgv1.Function, error)
}

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

// CachedFunctionProvider returns pre-fetched functions with reuse annotations.
// This is appropriate for the comp command where many XRs use the same composition,
// allowing Docker containers to be reused across renders.
type CachedFunctionProvider struct {
	cached []pkgv1.Function
	logger logging.Logger
}

// NewCachedFunctionProvider creates a new CachedFunctionProvider.
// It fetches functions once and adds Docker reuse annotations to enable container reuse.
func NewCachedFunctionProvider(fnClient xp.FunctionClient, comp *apiextensionsv1.Composition, logger logging.Logger) (FunctionProvider, error) {
	logger.Debug("Fetching functions for caching", "composition", comp.GetName())

	// Fetch functions from the composition
	fns, err := fnClient.GetFunctionsFromPipeline(comp)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get functions from pipeline")
	}

	logger.Debug("Fetched functions for caching", "composition", comp.GetName(), "count", len(fns))

	// Add reuse annotations to each function
	for i := range fns {
		fn := &fns[i]

		// Generate a stable container name from the function package
		containerName := generateContainerName(fn.Spec.Package)

		logger.Debug("Adding reuse annotations to function",
			"function", fn.GetName(),
			"package", fn.Spec.Package,
			"containerName", containerName)

		// Initialize annotations map if it doesn't exist
		if fn.Annotations == nil {
			fn.Annotations = make(map[string]string)
		}

		// Add Docker reuse annotations
		// TODO: Add cleanup mechanism to remove orphaned containers at the end of comp command execution.
		// These containers are currently left running after the diff completes. We should track the container
		// names and stop/remove them when the command finishes to avoid accumulation of orphaned containers.
		fn.Annotations["render.crossplane.io/runtime-docker-name"] = containerName
		fn.Annotations["render.crossplane.io/runtime-docker-cleanup"] = "Orphan"
	}

	return &CachedFunctionProvider{
		cached: fns,
		logger: logger,
	}, nil
}

// GetFunctionsForComposition returns the cached functions.
func (p *CachedFunctionProvider) GetFunctionsForComposition(comp *apiextensionsv1.Composition) ([]pkgv1.Function, error) {
	p.logger.Debug("Using cached functions", "composition", comp.GetName(), "count", len(p.cached))
	return p.cached, nil
}

// generateContainerName creates a stable Docker container name from a function package reference.
// Example: xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.11.0
// Returns: function-go-templating-v0.11.0-comp.
func generateContainerName(pkg string) string {
	// Split package into path and version
	// Format: registry/org/name:version
	parts := strings.Split(pkg, "/")
	if len(parts) == 0 {
		return "unknown-comp"
	}

	// Get the last part (name:version)
	nameAndVersion := parts[len(parts)-1]

	// Replace colon with hyphen to make it container-name friendly
	// function-go-templating:v0.11.0 -> function-go-templating-v0.11.0
	containerName := strings.ReplaceAll(nameAndVersion, ":", "-")

	// Add suffix to distinguish from test containers
	containerName += "-comp"

	return containerName
}
