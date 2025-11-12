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

	"github.com/alecthomas/kong"
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
	"k8s.io/client-go/rest"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// globalRenderMutex serializes all render operations globally across all diff processors.
// This prevents concurrent Docker container operations that can overwhelm the Docker daemon
// when processing multiple XRs with the same functions. See issue #59.
//
//nolint:gochecknoglobals // Required for global serialization across processors
var globalRenderMutex sync.Mutex

// initializeSharedDependencies handles the common initialization logic for both commands.
func initializeSharedDependencies(ctx *kong.Context, log logging.Logger, config *rest.Config) (*AppContext, error) {
	appCtx, err := NewAppContext(config, log)
	if err != nil {
		return nil, errors.Wrap(err, "cannot create app context")
	}

	ctx.Bind(appCtx)

	return appCtx, nil
}

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
	return []dp.ProcessorOption{
		dp.WithNamespace(namespace),
		dp.WithColorize(!fields.NoColor),
		dp.WithCompact(fields.Compact),
		dp.WithMaxNestedDepth(fields.MaxNestedDepth),
	}
}
