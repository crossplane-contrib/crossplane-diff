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

// Package main contains shared command utilities.
package main

import (
	"context"
	"time"

	"github.com/alecthomas/kong"
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
	"k8s.io/client-go/rest"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	"github.com/crossplane/crossplane/v2/cmd/crank/render"
)

// CommonCmdFields contains common fields shared by both XR and Comp commands.
type CommonCmdFields struct {
	// Configuration options
	NoColor bool          `help:"Disable colorized output."                name:"no-color"`
	Compact bool          `help:"Show compact diffs with minimal context." name:"compact"`
	Timeout time.Duration `default:"1m"                                    help:"How long to run before timing out."`
	QPS     float32       `default:"0"                                     help:"Maximum QPS to the API server."`
	Burst   int           `default:"0"                                     help:"Maximum burst for throttle."`
}

// initializeSharedDependencies handles the common initialization logic for both commands.
func initializeSharedDependencies(ctx *kong.Context, log logging.Logger, config *rest.Config, fields CommonCmdFields) (*AppContext, error) {
	config = initRestConfig(config, log, fields)

	appCtx, err := NewAppContext(config, log)
	if err != nil {
		return nil, errors.Wrap(err, "cannot create app context")
	}

	ctx.Bind(appCtx)

	return appCtx, nil
}

// initRestConfig configures REST client rate limits for both commands.
func initRestConfig(config *rest.Config, logger logging.Logger, fields CommonCmdFields) *rest.Config {
	// Set default QPS and Burst if they are not set in the config
	// or override with values from options if provided
	originalQPS := config.QPS
	originalBurst := config.Burst

	if fields.QPS > 0 {
		config.QPS = fields.QPS
	} else if config.QPS == 0 {
		config.QPS = 20
	}

	if fields.Burst > 0 {
		config.Burst = fields.Burst
	} else if config.Burst == 0 {
		config.Burst = 30
	}

	logger.Debug("Configured REST client rate limits",
		"original_qps", originalQPS,
		"original_burst", originalBurst,
		"options_qps", fields.QPS,
		"options_burst", fields.Burst,
		"final_qps", config.QPS,
		"final_burst", config.Burst)

	return config
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
// Call sites can append additional options or override these defaults as needed.
func defaultProcessorOptions() []dp.ProcessorOption {
	return []dp.ProcessorOption{
		dp.WithColorize(true),
		dp.WithCompact(false),
		dp.WithRenderFunc(render.Render),
	}
}
