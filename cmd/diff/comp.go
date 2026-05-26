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
	"strings"
	"time"

	"github.com/alecthomas/kong"
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	ld "github.com/crossplane/cli/v2/cmd/crossplane/common/load"
)

// CompDiffProcessor is imported from the diffprocessor package

// CompCmd represents the composition diff command.
type CompCmd struct {
	// Embed common fields
	CommonCmdFields

	Files []string `arg:"" help:"YAML files containing updated Composition(s)." optional:""`

	// Configuration options
	Namespace     string   `default:""                                                                                                                                          help:"Namespace to find XRs (empty = all namespaces)."                            name:"namespace"      short:"n"`
	IncludeManual bool     `default:"false"                                                                                                                                     help:"Include XRs with Manual update policy (default: only Automatic policy XRs)" name:"include-manual"`
	Resources     []string `help:"Limit impact analysis to specific composites in [namespace/]name format. Repeatable or comma-separated. Mutually exclusive with --namespace." name:"resource"`
}

// validateFlags returns an error if mutually exclusive flags are set together.
func (c *CompCmd) validateFlags() error {
	if c.Namespace != "" && len(c.Resources) > 0 {
		return errors.New("--namespace and --resource are mutually exclusive; use --resource=[namespace/]name to scope by name")
	}

	return nil
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

  # Show eventual state with function-sequencer (all stages, not just first).
  crossplane-diff comp updated-composition.yaml --eventual-state

  # Limit impact analysis to specific composites (by [namespace/]name)
  crossplane-diff comp updated-composition.yaml --resource=default/my-claim
  crossplane-diff comp updated-composition.yaml --resource=default/xr-1,default/xr-2

Notes:
  --resource cannot be combined with --namespace.
  Composites with Manual update policy are surfaced as "filtered_by_policy"
  unless --include-manual is also passed.
`
}

// AfterApply implements kong's AfterApply method to bind command-specific dependencies.
// AppContext is received via dependency injection - Kong resolves it through the provider chain:
// ContextProvider (bound in CommonCmdFields.BeforeApply) -> provideRestConfig -> provideAppContext.
func (c *CompCmd) AfterApply(ctx *kong.Context, log logging.Logger, appCtx *AppContext) error {
	if err := c.validateFlags(); err != nil {
		return err
	}

	proc := makeDefaultCompProc(c, ctx, appCtx, log)

	loader, err := ld.NewCompositeLoader(c.Files)
	if err != nil {
		return errors.Wrap(err, "cannot create composition loader")
	}

	ctx.BindTo(proc, (*dp.CompDiffProcessor)(nil))
	ctx.BindTo(loader, (*ld.Loader)(nil))

	return nil
}

func makeDefaultCompProc(c *CompCmd, kongCtx *kong.Context, appCtx *AppContext, log logging.Logger) dp.CompDiffProcessor {
	// Use provided namespace or default to "default"
	namespace := c.Namespace
	if namespace == "" {
		namespace = "default"
	}

	// Both processors share the same options since they're part of the same command
	opts := defaultProcessorOptions(c.CommonCmdFields, namespace)
	opts = append(opts,
		dp.WithLogger(log),
		dp.WithIncludeManual(c.IncludeManual),
		dp.WithStdout(kongCtx.Stdout),
		dp.WithStderr(kongCtx.Stderr),
	)

	// Create XR processor first (peer processor)
	xrProc := dp.NewDiffProcessor(appCtx.K8sClients, appCtx.XpClients, opts...)

	// Inject it into composition processor
	return dp.NewCompDiffProcessor(xrProc, appCtx.XpClients.Composition, opts...)
}

// parseResourceRef parses a "[namespace/]name" string into a NamespacedName.
// Bare "name" (no slash) means cluster-scoped (v1 XRs, v2 cluster-scoped XRs).
// "ns/name" means namespaced (Claims, v2 namespaced XRs).
// "/name" (empty namespace before slash) is rejected because the user's intent is clearly namespaced.
func parseResourceRef(value string) (k8stypes.NamespacedName, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return k8stypes.NamespacedName{}, errors.Errorf("invalid --resource value %q: cannot be empty", value)
	}

	parts := strings.Split(trimmed, "/")
	switch len(parts) {
	case 1:
		return k8stypes.NamespacedName{Name: parts[0]}, nil
	case 2:
		ns, name := parts[0], parts[1]
		if ns == "" {
			return k8stypes.NamespacedName{}, errors.Errorf("invalid --resource value %q: namespace must not be empty (use bare name for cluster-scoped composites)", value)
		}

		if name == "" {
			return k8stypes.NamespacedName{}, errors.Errorf("invalid --resource value %q: name must not be empty", value)
		}

		return k8stypes.NamespacedName{Namespace: ns, Name: name}, nil
	default:
		return k8stypes.NamespacedName{}, errors.Errorf("invalid --resource value %q: expected [namespace/]name format, got %d slash-separated parts", value, len(parts)-1)
	}
}

// Run executes the composition diff command.
func (c *CompCmd) Run(_ *kong.Context, log logging.Logger, appCtx *AppContext, proc dp.CompDiffProcessor, loader ld.Loader, exitCode *ExitCode) error {
	ctx, cancel, err := initializeAppContext(c.Timeout, appCtx, log)
	if err != nil {
		exitCode.Code = dp.ExitCodeToolError
		return err
	}
	defer cancel()

	// Cleanup any resources held by the processor (e.g., Docker containers)
	defer func() {
		// Use background context with timeout for cleanup instead of the command context.
		// The command context may be cancelled (user Ctrl+C, timeout, etc.), which would cause
		// Docker API calls to fail immediately, leaving containers running. By using a background
		// context, we ensure cleanup completes even after cancellation, but we add a timeout to
		// prevent cleanup from blocking indefinitely if the Docker daemon is slow or hung.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()

		if err := proc.Cleanup(cleanupCtx); err != nil {
			log.Debug("Failed to cleanup processor resources", "error", err)
		}
	}()

	err = proc.Initialize(ctx)
	if err != nil {
		exitCode.Code = dp.ExitCodeToolError
		return errors.Wrap(err, "cannot initialize composition diff processor")
	}

	compositions, err := loader.Load()
	if err != nil {
		exitCode.Code = dp.ExitCodeToolError
		return errors.Wrap(err, "cannot load compositions")
	}

	parsedRefs := make([]k8stypes.NamespacedName, 0, len(c.Resources))

	for _, raw := range c.Resources {
		ref, err := parseResourceRef(raw)
		if err != nil {
			exitCode.Code = dp.ExitCodeToolError
			return err
		}

		parsedRefs = append(parsedRefs, ref)
	}

	hasDiffs, err := proc.DiffComposition(ctx, compositions, c.Namespace, parsedRefs)

	// Determine exit code based on result
	exitCode.Code = dp.DetermineExitCode(err, hasDiffs)
	if err != nil {
		return errors.Wrap(err, "unable to process composition diff")
	}

	return nil
}
