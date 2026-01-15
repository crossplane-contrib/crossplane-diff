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
	"time"

	"github.com/alecthomas/kong"
	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	ld "github.com/crossplane/crossplane/v2/cmd/crank/common/load"
)

// CompDiffProcessor is imported from the diffprocessor package

// CompCmd represents the composition diff command.
type CompCmd struct {
	// Embed common fields
	CommonCmdFields

	Files []string `arg:"" help:"YAML files containing updated Composition(s)." optional:""`

	// Configuration options
	Namespace     string `default:""      help:"Namespace to find XRs (empty = all namespaces)."                            name:"namespace"      short:"n"`
	IncludeManual bool   `default:"false" help:"Include XRs with Manual update policy (default: only Automatic policy XRs)" name:"include-manual"`
}

// Help returns help instructions for the composition diff command.
func (c *CompCmd) Help() string {
	return `
This command shows the impact of composition changes on existing XRs in the cluster.

It finds all XRs that use the specified composition(s) and shows what would change
if they were rendered with the updated composition(s) from the file(s).

Examples:
  # Show impact of updated composition on all XRs using it
  crossplane-diff comp updated-composition.yaml

  # Show impact of multiple composition changes
  crossplane-diff comp comp1.yaml comp2.yaml comp3.yaml

  # Show impact only on XRs in a specific namespace
  crossplane-diff comp updated-composition.yaml -n production

  # Show compact diffs with minimal context
  crossplane-diff comp updated-composition.yaml --compact

  # Include XRs with Manual update policy (pinned revisions)
  crossplane-diff comp updated-composition.yaml --include-manual
`
}

// AfterApply implements kong's AfterApply method to bind our dependencies.
func (c *CompCmd) AfterApply(ctx *kong.Context, log logging.Logger) error {
	return c.initializeDependencies(ctx, log)
}

func (c *CompCmd) initializeDependencies(ctx *kong.Context, log logging.Logger) error {
	// Get the REST config using the context flag from CommonCmdFields
	config, err := c.GetRestConfig()
	if err != nil {
		return errors.Wrap(err, "cannot create kubernetes client config")
	}

	appCtx, err := initializeSharedDependencies(ctx, log, config)
	if err != nil {
		return err
	}

	proc, fnProvider := makeDefaultCompProc(c, appCtx, log)

	loader, err := ld.NewCompositeLoader(c.Files)
	if err != nil {
		return errors.Wrap(err, "cannot create composition loader")
	}

	ctx.BindTo(proc, (*dp.CompDiffProcessor)(nil))
	ctx.BindTo(loader, (*ld.Loader)(nil))
	ctx.BindTo(fnProvider, (*dp.FunctionProvider)(nil))

	return nil
}

func makeDefaultCompProc(c *CompCmd, ctx *AppContext, log logging.Logger) (dp.CompDiffProcessor, dp.FunctionProvider) {
	// Use provided namespace or default to "default"
	namespace := c.Namespace
	if namespace == "" {
		namespace = "default"
	}

	// Create the cached function provider that we'll return for cleanup
	fnProvider := dp.NewCachedFunctionProvider(ctx.XpClients.Function, log)

	// Both processors share the same options since they're part of the same command
	opts := defaultProcessorOptions(c.CommonCmdFields, namespace)
	opts = append(opts,
		dp.WithLogger(log),
		dp.WithRenderMutex(&globalRenderMutex),
		dp.WithIncludeManual(c.IncludeManual),
		// Use the function provider we created above
		dp.WithFunctionProviderFactory(func(xp.FunctionClient, logging.Logger) dp.FunctionProvider {
			return fnProvider
		}),
	)

	// Create XR processor first (peer processor)
	xrProc := dp.NewDiffProcessor(ctx.K8sClients, ctx.XpClients, opts...)

	// Inject it into composition processor
	return dp.NewCompDiffProcessor(xrProc, ctx.XpClients.Composition, opts...), fnProvider
}

// Run executes the composition diff command.
func (c *CompCmd) Run(k *kong.Context, log logging.Logger, appCtx *AppContext, proc dp.CompDiffProcessor, loader ld.Loader, fnProvider dp.FunctionProvider) error {
	ctx, cancel, err := initializeAppContext(c.Timeout, appCtx, log)
	if err != nil {
		return err
	}
	defer cancel()

	// Cleanup any resources created by the function provider
	defer func() {
		// Use background context with timeout for cleanup instead of the command context.
		// The command context may be cancelled (user Ctrl+C, timeout, etc.), which would cause
		// Docker API calls to fail immediately, leaving containers running. By using a background
		// context, we ensure cleanup completes even after cancellation, but we add a timeout to
		// prevent cleanup from blocking indefinitely if the Docker daemon is slow or hung.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()

		if err := fnProvider.Cleanup(cleanupCtx); err != nil {
			log.Debug("Failed to cleanup function provider resources", "error", err)
		}
	}()

	err = proc.Initialize(ctx)
	if err != nil {
		return errors.Wrap(err, "cannot initialize composition diff processor")
	}

	compositions, err := loader.Load()
	if err != nil {
		return errors.Wrap(err, "cannot load compositions")
	}

	if err := proc.DiffComposition(ctx, k.Stdout, compositions, c.Namespace); err != nil {
		return errors.Wrap(err, "unable to process composition diff")
	}

	return nil
}
