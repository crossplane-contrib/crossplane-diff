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
	"github.com/alecthomas/kong"
	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
	"k8s.io/client-go/rest"

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
func (c *CompCmd) AfterApply(ctx *kong.Context, log logging.Logger, config *rest.Config) error {
	return c.initializeDependencies(ctx, log, config)
}

func (c *CompCmd) initializeDependencies(ctx *kong.Context, log logging.Logger, config *rest.Config) error {
	appCtx, err := initializeSharedDependencies(ctx, log, config, c.CommonCmdFields)
	if err != nil {
		return err
	}

	proc := makeDefaultCompProc(c, appCtx, log)

	loader, err := ld.NewCompositeLoader(c.Files)
	if err != nil {
		return errors.Wrap(err, "cannot create composition loader")
	}

	ctx.BindTo(proc, (*dp.CompDiffProcessor)(nil))
	ctx.BindTo(loader, (*ld.Loader)(nil))

	return nil
}

func makeDefaultCompProc(c *CompCmd, ctx *AppContext, log logging.Logger) dp.CompDiffProcessor {
	// Use provided namespace or default to "default"
	namespace := c.Namespace
	if namespace == "" {
		namespace = "default"
	}

	// Composition processor options
	opts := defaultProcessorOptions(c.CommonCmdFields, namespace)
	opts = append(opts,
		dp.WithLogger(log),
		dp.WithRenderMutex(&globalRenderMutex),
		dp.WithIncludeManual(c.IncludeManual),
		dp.WithDiffProcessorFactory(func(k8Clients k8.Clients, xpClients xp.Clients, processorOpts []dp.ProcessorOption) dp.DiffProcessor {
			return dp.NewDiffProcessor(k8Clients, xpClients, processorOpts...)
		}),
		// Use cached function provider for composition diffs to enable Docker container reuse
		// The provider lazy-loads and caches functions by composition name
		dp.WithFunctionProviderFactory(dp.NewCachedFunctionProvider),
	)

	// Create composition processor with clients
	return dp.NewCompDiffProcessor(ctx.K8sClients, ctx.XpClients, opts...)
}

// Run executes the composition diff command.
func (c *CompCmd) Run(k *kong.Context, log logging.Logger, appCtx *AppContext, proc dp.CompDiffProcessor, loader ld.Loader) error {
	ctx, cancel, err := initializeAppContext(c.Timeout, appCtx, log)
	if err != nil {
		return err
	}
	defer cancel()

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
