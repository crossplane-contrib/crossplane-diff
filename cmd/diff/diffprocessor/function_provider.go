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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"

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

	// Cleanup stops and removes any resources created during function execution.
	// For providers that don't create resources (like DefaultFunctionProvider), this is a no-op.
	Cleanup(ctx context.Context) error
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

// Cleanup is a no-op for DefaultFunctionProvider as it doesn't create any resources.
func (p *DefaultFunctionProvider) Cleanup(_ context.Context) error {
	return nil
}

// CachedFunctionProvider lazy-loads and caches functions with reuse annotations.
// This is appropriate for the comp command where many XRs use the same composition,
// allowing Docker containers to be reused across renders.
type CachedFunctionProvider struct {
	fnClient       xp.FunctionClient
	cache          map[string][]pkgv1.Function
	containerNames []string // Track container names for cleanup
	instanceID     string   // Unique identifier for this provider instance
	logger         logging.Logger
}

// NewCachedFunctionProvider creates a new CachedFunctionProvider.
func NewCachedFunctionProvider(fnClient xp.FunctionClient, logger logging.Logger) FunctionProvider {
	return &CachedFunctionProvider{
		fnClient:       fnClient,
		cache:          make(map[string][]pkgv1.Function),
		containerNames: make([]string, 0),
		instanceID:     generateInstanceID(),
		logger:         logger,
	}
}

// generateInstanceID creates a short random identifier for this provider instance.
// This ensures container names are unique across different provider instances and test runs.
func generateInstanceID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// Fallback to a timestamp-based approach if crypto/rand fails
		// This is extremely unlikely but we handle it for completeness
		return fmt.Sprintf("%x", time.Now().UnixNano()&0xFFFFFFFF)
	}

	return hex.EncodeToString(b)
}

// GetFunctionsForComposition fetches and caches functions on first call per composition.
func (p *CachedFunctionProvider) GetFunctionsForComposition(comp *apiextensionsv1.Composition) ([]pkgv1.Function, error) {
	compName := comp.GetName()

	// Check cache first
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

	// Add reuse annotations to each function
	for i := range fns {
		fn := &fns[i]

		// Generate a stable container name from the function package and instance ID
		// The instance ID ensures containers are unique across provider instances and test runs
		containerName := generateContainerName(fn.Spec.Package, p.instanceID)

		p.logger.Debug("Adding reuse annotations to function",
			"function", fn.GetName(),
			"package", fn.Spec.Package,
			"containerName", containerName,
			"instanceID", p.instanceID)

		// Initialize annotations map if it doesn't exist
		if fn.Annotations == nil {
			fn.Annotations = make(map[string]string)
		}

		// Add Docker reuse annotations
		// Containers will be cleaned up via Cleanup() method called by comp command
		fn.Annotations["render.crossplane.io/runtime-docker-name"] = containerName
		fn.Annotations["render.crossplane.io/runtime-docker-cleanup"] = "Orphan"

		// Track container name for cleanup
		p.containerNames = append(p.containerNames, containerName)
	}

	// Cache for future calls
	p.cache[compName] = fns

	return fns, nil
}

// Cleanup stops and removes Docker containers created during function execution.
func (p *CachedFunctionProvider) Cleanup(ctx context.Context) error {
	if len(p.containerNames) == 0 {
		p.logger.Debug("No containers to clean up")
		return nil
	}

	p.logger.Info("Cleaning up function containers", "count", len(p.containerNames))

	// Create Docker client
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		p.logger.Debug("Failed to create Docker client", "error", err)
		return nil // Graceful degradation - don't fail cleanup
	}

	defer func() {
		if err := cli.Close(); err != nil {
			p.logger.Debug("Error closing Docker client", "error", err)
		}
	}()

	var errs []error

	for _, containerName := range p.containerNames {
		// List containers matching this name
		filterArgs := filters.NewArgs()
		filterArgs.Add("name", fmt.Sprintf("^%s$", containerName))

		containers, err := cli.ContainerList(ctx, container.ListOptions{
			All:     true,
			Filters: filterArgs,
		})
		if err != nil {
			p.logger.Debug("Error listing containers", "container", containerName, "error", err)
			continue
		}

		// Skip if container doesn't exist
		if len(containers) == 0 {
			p.logger.Debug("Container does not exist, skipping", "container", containerName)
			continue
		}

		// Remove the container (force=true stops and removes)
		p.logger.Debug("Stopping and removing container", "container", containerName)

		removeOpts := container.RemoveOptions{
			Force: true, // Stop and remove
		}
		if err := cli.ContainerRemove(ctx, containers[0].ID, removeOpts); err != nil {
			p.logger.Debug("Error removing container", "container", containerName, "error", err)
			errs = append(errs, errors.Wrapf(err, "failed to remove container %s", containerName))
		} else {
			p.logger.Debug("Successfully removed container", "container", containerName)
		}
	}

	if len(errs) > 0 {
		// Don't fail the entire cleanup if some containers couldn't be removed
		// Log the error but return nil to allow graceful degradation
		p.logger.Info("Some containers could not be cleaned up", "errors", len(errs))

		for _, err := range errs {
			p.logger.Debug("Cleanup error", "error", err)
		}
	}

	return nil
}

// generateContainerName creates a stable Docker container name from a function package reference and instance ID.
// The instance ID ensures containers are unique across provider instances and test runs to avoid race conditions.
// Example: xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.11.0 with instanceID "a1b2c3d4"
// Returns: function-go-templating-v0.11.0-comp-a1b2c3d4.
func generateContainerName(pkg, instanceID string) string {
	// Handle empty package string
	if pkg == "" {
		return fmt.Sprintf("unknown-comp-%s", instanceID)
	}

	// Split package into path and version
	// Format: registry/org/name:version
	parts := strings.Split(pkg, "/")

	// Get the last part (name:version)
	nameAndVersion := parts[len(parts)-1]

	// Replace colon with hyphen to make it container-name friendly
	// function-go-templating:v0.11.0 -> function-go-templating-v0.11.0
	containerName := strings.ReplaceAll(nameAndVersion, ":", "-")

	// Add suffix and instance ID to make it unique per provider instance
	containerName += fmt.Sprintf("-comp-%s", instanceID)

	return containerName
}
