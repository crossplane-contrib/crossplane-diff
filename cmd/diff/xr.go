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

// Package main contains the XR diff command.
package main

import (
	"fmt"

	"github.com/alecthomas/kong"
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
	"k8s.io/client-go/rest"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	ld "github.com/crossplane/crossplane/v2/cmd/crank/common/load"
)

// XRCmd represents the XR diff command.
type XRCmd struct {
	// Embed common fields
	CommonCmdFields

	Namespace string   `default:"crossplane-system" help:"Namespace to compare resources against."             name:"namespace" short:"n"`
	Files     []string `arg:""                      help:"YAML files containing Crossplane resources to diff." optional:""`
}

// Help returns help instructions for the XR diff command.
func (c *XRCmd) Help() string {
	return `
This command returns a diff of the in-cluster resources that would be modified if the provided Crossplane resources were applied.

Similar to kubectl diff, it requires Crossplane to be operating in the live cluster found in your kubeconfig.

Examples:
  # Show the changes that would result from applying xr.yaml (via file) in the default 'crossplane-system' namespace.
  crossplane-diff xr xr.yaml

  # Show the changes that would result from applying xr.yaml (via stdin) in the default 'crossplane-system' namespace.
  cat xr.yaml | crossplane-diff xr --

  # Show the changes that would result from applying xr.yaml, xr1.yaml, and xr2.yaml in the default 'crossplane-system' namespace.
  cat xr.yaml | crossplane-diff xr xr1.yaml xr2.yaml --

  # Show the changes that would result from applying xr.yaml (via file) in the 'foobar' namespace with no color output.
  crossplane-diff xr xr.yaml -n foobar --no-color

  # Show the changes in a compact format with minimal context.
  crossplane-diff xr xr.yaml --compact
`
}

// AfterApply implements kong's AfterApply method to bind our dependencies.
func (c *XRCmd) AfterApply(ctx *kong.Context, log logging.Logger, config *rest.Config) error {
	return c.initializeDependencies(ctx, log, config)
}

func (c *XRCmd) initializeDependencies(ctx *kong.Context, log logging.Logger, config *rest.Config) error {
	appCtx, err := initializeSharedDependencies(ctx, log, config, c.CommonCmdFields)
	if err != nil {
		return err
	}

	proc := makeDefaultXRProc(c, appCtx, log)

	loader, err := makeDefaultXRLoader(c)
	if err != nil {
		return errors.Wrap(err, "cannot create resource loader")
	}

	ctx.BindTo(proc, (*dp.DiffProcessor)(nil))
	ctx.BindTo(loader, (*ld.Loader)(nil))

	return nil
}

func makeDefaultXRProc(c *XRCmd, ctx *AppContext, log logging.Logger) dp.DiffProcessor {
	opts := defaultProcessorOptions()
	opts = append(opts,
		dp.WithNamespace(c.Namespace),
		dp.WithLogger(log),
		dp.WithColorize(!c.NoColor), // Override default if NoColor is set
		dp.WithCompact(c.Compact),   // Override default if Compact is set
	)

	return dp.NewDiffProcessor(ctx.K8sClients, ctx.XpClients, opts...)
}

func makeDefaultXRLoader(c *XRCmd) (ld.Loader, error) {
	return ld.NewCompositeLoader(c.Files)
}

// DeprecatedDiffCmd represents the deprecated diff command for backward compatibility.
type DeprecatedDiffCmd struct {
	XRCmd
}

// Help returns help instructions for the deprecated diff command.
func (c *DeprecatedDiffCmd) Help() string {
	return `
This command is deprecated. Please use 'crossplane-diff xr' instead.

This command returns a diff of the in-cluster resources that would be modified if the provided Crossplane resources were applied.

Similar to kubectl diff, it requires Crossplane to be operating in the live cluster found in your kubeconfig.

Examples:
  # Deprecated usage (still works but will show warning)
  crossplane-diff diff xr.yaml

  # Preferred usage
  crossplane-diff xr xr.yaml
`
}

// Run executes the deprecated diff command with a deprecation warning.
func (c *DeprecatedDiffCmd) Run(k *kong.Context, log logging.Logger, appCtx *AppContext, proc dp.DiffProcessor, loader ld.Loader) error {
	if _, err := fmt.Fprintln(k.Stderr, "Warning: The 'diff' subcommand is deprecated. Please use 'xr' instead."); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(k.Stderr, "Example: crossplane-diff xr your-file.yaml"); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(k.Stderr); err != nil {
		return err
	}

	return c.XRCmd.Run(k, log, appCtx, proc, loader)
}

// Run executes the XR diff command.
func (c *XRCmd) Run(k *kong.Context, log logging.Logger, appCtx *AppContext, proc dp.DiffProcessor, loader ld.Loader) error {
	// the rest config here is provided by a function in main.go that's only invoked for commands that request it
	// in their arguments.  that means we won't get "can't find kubeconfig" errors for cases where the config isn't asked for.

	// TODO:  add a file output option
	// TODO:  make sure namespacing works everywhere; what to do with the -n argument?
	// TODO:  test for the case of applying a namespaced object inside a composition using fn-gotemplating inside fn-kubectl?
	// TODO:  add test for new vs updated XRs with downstream fields plumbed from Status field
	// TODO:  diff against upgraded schema that isn't applied yet
	// TODO:  diff against upgraded composition that isn't applied yet
	// TODO:  diff against upgraded composition version that is already available
	ctx, cancel, err := initializeAppContext(c.Timeout, appCtx, log)
	if err != nil {
		return err
	}
	defer cancel()

	resources, err := loader.Load()
	if err != nil {
		return errors.Wrap(err, "cannot load resources")
	}

	err = proc.Initialize(ctx)
	if err != nil {
		return errors.Wrap(err, "cannot initialize diff processor")
	}

	if err := proc.PerformDiff(ctx, k.Stdout, resources, appCtx.XpClients.Composition.FindMatchingComposition); err != nil {
		return errors.Wrap(err, "unable to process one or more resources")
	}

	return nil
}
