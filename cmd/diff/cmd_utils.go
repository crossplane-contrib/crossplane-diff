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

package main

import (
	"context"
	"sync"
	"time"

	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	ld "github.com/crossplane/crossplane/v2/cmd/crank/common/load"
)

// globalRenderMutex serializes all render operations globally across all diff processors.
// This prevents concurrent Docker container operations that can overwhelm the Docker daemon
// when processing multiple XRs with the same functions. See issue #59.
//
//nolint:gochecknoglobals // Required for global serialization across processors
var globalRenderMutex sync.Mutex

// initializeAppContext initializes the application context with timeout and error handling.
func initializeAppContext(timeout time.Duration, appCtx *AppContext, log logging.Logger) (context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	if err := appCtx.Initialize(ctx, log); err != nil {
		cancel()
		return nil, nil, errors.Wrap(err, "cannot initialize client")
	}

	return ctx, cancel, nil
}

// defaultProcessorOptions returns the standard default options used by both XR and composition processors.
// This is the single source of truth for behavior defaults in the CLI layer.
func defaultProcessorOptions(fields CommonCmdFields, namespace string) []dp.ProcessorOption {
	// Default ignored paths - always filtered from diffs
	// Preallocate with capacity for default + user-specified paths
	allIgnorePaths := make([]string, 0, 1+len(fields.IgnorePaths))
	allIgnorePaths = append(allIgnorePaths, "metadata.annotations[kubectl.kubernetes.io/last-applied-configuration]")

	// Combine default paths with user-specified ones
	allIgnorePaths = append(allIgnorePaths, fields.IgnorePaths...)

	opts := []dp.ProcessorOption{
		dp.WithNamespace(namespace),
		dp.WithColorize(!fields.NoColor),
		dp.WithCompact(fields.Compact),
		dp.WithMaxNestedDepth(fields.MaxNestedDepth),
		dp.WithIgnorePaths(allIgnorePaths),
	}

	// Add function credentials if provided (empty path with no secrets errors in FunctionCredentials.Decode)
	if len(fields.FunctionCredentials.Secrets) > 0 {
		opts = append(opts, dp.WithFunctionCredentials(fields.FunctionCredentials.Secrets))
	}

	return opts
}

// LoadFunctionCredentials loads Secret resources from a YAML file or directory.
// The function supports both single files and directories containing YAML files.
// Only resources of kind "Secret" are returned; other resources are silently skipped.
func LoadFunctionCredentials(path string) ([]corev1.Secret, error) {
	if path == "" {
		return nil, nil
	}

	// Use the crossplane loader which handles files, directories, and multi-document YAML
	loader, err := ld.NewLoader(path)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot create loader for path %q", path)
	}

	resources, err := loader.Load()
	if err != nil {
		return nil, errors.Wrapf(err, "cannot load resources from %q", path)
	}

	secrets := make([]corev1.Secret, 0, len(resources))

	for _, res := range resources {
		// Only process Secret resources
		if res.GetKind() != "Secret" || res.GetAPIVersion() != "v1" {
			continue
		}

		// Convert unstructured to corev1.Secret
		secret := corev1.Secret{}

		err := runtime.DefaultUnstructuredConverter.FromUnstructured(res.UnstructuredContent(), &secret)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot convert Secret %q to corev1.Secret", res.GetName())
		}

		secrets = append(secrets, secret)
	}

	return secrets, nil
}
