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

// Package serial provides utilities for serializing render operations.
package serial

import (
	"context"
	"sync"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	"github.com/crossplane/crossplane/v2/cmd/crank/render"
)

// RenderFunc wraps a render function to serialize all render calls using the provided mutex.
// This prevents concurrent Docker container operations that can overwhelm the Docker daemon
// when processing many XRs with the same functions. The serialization ensures:
//
//  1. Only one render operation runs at a time globally
//  2. Named Docker containers (via annotations) can be reused safely between renders
//  3. Container startup races are eliminated
//
// For e2e tests, combine this with versioned named container annotations for optimal performance.
// For production, this works without requiring users to annotate their Function resources.
func RenderFunc(
	renderFunc func(context.Context, logging.Logger, render.Inputs) (render.Outputs, error),
	mu *sync.Mutex,
) func(context.Context, logging.Logger, render.Inputs) (render.Outputs, error) {
	renderCount := 0

	return func(ctx context.Context, log logging.Logger, in render.Inputs) (render.Outputs, error) {
		mu.Lock()
		defer mu.Unlock()

		renderCount++
		log.Debug("Starting serialized render",
			"renderNumber", renderCount,
			"functionCount", len(in.Functions))

		start := time.Now()
		result, err := renderFunc(ctx, log, in)
		duration := time.Since(start)

		if err != nil {
			log.Debug("Render completed with error",
				"renderNumber", renderCount,
				"error", err,
				"duration", duration)
		} else {
			log.Debug("Render completed successfully",
				"renderNumber", renderCount,
				"duration", duration,
				"composedResourceCount", len(result.ComposedResources))
		}

		return result, err
	}
}
