/*
Copyright 2020 The Crossplane Authors.

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

// Package main implements the crossplane-diff CLI tool for diffing Crossplane resources.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/alecthomas/kong"
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/version"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

var _ = kong.Must(&cli{})

type (
	verboseFlag bool
	// KubeContext represents the Kubernetes context name from the kubeconfig.
	KubeContext string
)

// ContextProvider is an interface for accessing the Kubernetes context configuration.
// Commands embed CommonCmdFields which implements this interface. By binding a pointer
// to the command struct via this interface in BeforeApply, providers can access the
// context value after flag parsing completes (when providers are actually resolved).
type ContextProvider interface {
	GetKubeContext() KubeContext
}

// ExitCode tracks the exit code to return after command execution.
// Commands set this based on their results (diffs found, validation errors, etc.).
type ExitCode struct {
	Code int
}

// FunctionCredentials holds Secret credentials loaded from a file path.
// It implements kong.MapperValue to load secrets at CLI parse time.
type FunctionCredentials struct {
	Path    string          // Original path for logging/debugging
	Secrets []corev1.Secret // Loaded secrets
}

// Decode implements kong.MapperValue to load secrets from the provided path.
func (f *FunctionCredentials) Decode(ctx *kong.DecodeContext) error {
	var path string
	if err := ctx.Scan.PopValueInto("path", &path); err != nil {
		return err
	}

	if path == "" {
		return nil
	}

	f.Path = path

	secrets, err := LoadFunctionCredentials(path)
	if err != nil {
		return err
	}

	if len(secrets) == 0 {
		return fmt.Errorf("no Secret resources found in %q - file must contain v1/Secret resources", path)
	}

	f.Secrets = secrets

	return nil
}

// CommonCmdFields contains common fields shared by both XR and Comp commands.
// It implements ContextProvider to allow providers to access the context value
// after flag parsing completes.
type CommonCmdFields struct {
	// Configuration options
	Context             KubeContext         `help:"Kubernetes context to use (defaults to current context)."                                   name:"context"`
	NoColor             bool                `help:"Disable colorized output."                                                                  name:"no-color"`
	Compact             bool                `help:"Show compact diffs with minimal context."                                                   name:"compact"`
	MaxNestedDepth      int                 `default:"10"                                                                                      help:"Maximum depth for nested XR recursion." name:"max-nested-depth"`
	Timeout             time.Duration       `default:"1m"                                                                                      help:"How long to run before timing out."`
	IgnorePaths         []string            `help:"Paths to ignore in diffs (e.g., 'metadata.annotations[argocd.argoproj.io/tracking-id]')."   name:"ignore-paths"`
	FunctionCredentials FunctionCredentials `help:"A YAML file or directory of YAML files specifying Secret credentials to pass to Functions." name:"function-credentials"                   placeholder:"PATH"`
}

// GetKubeContext implements ContextProvider.
func (c *CommonCmdFields) GetKubeContext() KubeContext {
	return c.Context
}

func (v verboseFlag) BeforeApply(ctx *kong.Context) error { //nolint:unparam // BeforeApply requires this signature.
	logger := logging.NewLogrLogger(zap.New(zap.UseDevMode(true)))
	ctx.BindTo(logger, (*logging.Logger)(nil))

	return nil
}

// BeforeApply binds the CommonCmdFields pointer via the ContextProvider interface.
// This allows providers to access the context value after flag parsing completes.
// The key insight is that we bind a POINTER here - when providers are resolved later
// (in AfterApply or Run), they dereference the pointer and get the current field values.
func (c *CommonCmdFields) BeforeApply(ctx *kong.Context) error { //nolint:unparam // BeforeApply requires this signature.
	ctx.BindTo(c, (*ContextProvider)(nil))
	return nil
}

// The top-level crossplane CLI.
type cli struct {
	// Subcommands and flags will appear in the CLI help output in the same
	// order they're specified here. Keep them in alphabetical order.

	// Subcommands.
	Comp CompCmd `cmd:""         help:"Show impact of composition changes on existing XRs."`
	XR   XRCmd   `aliases:"diff" cmd:""                                                     help:"See what changes will be made against a live cluster when a given Crossplane resource would be applied."`

	Version version.Cmd `cmd:"" help:"Print the client and server version information for the current context."`

	// Flags.
	Verbose verboseFlag `help:"Print verbose logging statements." name:"verbose"`
}

func main() {
	logger := logging.NewNopLogger()
	exitCode := &ExitCode{Code: dp.ExitCodeSuccess} // Default to success

	ctx := kong.Parse(&cli{},
		kong.Name("crossplane-diff"),
		kong.Description("A command line tool for diffing  Crossplane resources."),
		// Binding a variable to kong context makes it available to all commands
		// at runtime.
		kong.BindTo(logger, (*logging.Logger)(nil)),
		kong.Bind(exitCode), // Bind exit code state
		// Providers are resolved lazily when dependencies are needed.
		// provideRestConfig depends on ContextProvider (bound in CommonCmdFields.BeforeApply)
		// provideAppContext depends on *rest.Config and logging.Logger
		kong.BindToProvider(provideRestConfig),
		kong.BindToProvider(provideAppContext),
		kong.ConfigureHelp(kong.HelpOptions{
			FlagsLast:      true,
			Compact:        true,
			WrapUpperBound: 80,
		}),
		kong.UsageOnError())
	err := ctx.Run()
	// Handle error output - commands set exitCode.Code based on their results
	if err != nil {
		// Schema validation errors are already rendered by the processors as part of the
		// diff output, so don't duplicate them here. Only print other (tool) errors.
		if !dp.IsSchemaValidationError(err) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		// If the command returned an error but didn't set an exit code, default to tool error
		if exitCode.Code == dp.ExitCodeSuccess {
			exitCode.Code = dp.ExitCodeToolError
		}
	}

	os.Exit(exitCode.Code)
}

// provideRestConfig creates a Kubernetes REST config using the context from ContextProvider.
// This provider is resolved lazily when dependencies need it (in AfterApply or Run),
// at which point flag parsing is complete and GetKubeContext() returns the correct value.
func provideRestConfig(cp ContextProvider) (*rest.Config, error) {
	kubeContext := cp.GetKubeContext()

	// Use the standard client-go loading rules:
	// 1. If KUBECONFIG env var is set, use that
	// 2. Otherwise, use ~/.kube/config
	// 3. Respects current context from the kubeconfig (or uses specified context)
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}

	// If a specific context is requested, override the current context
	if kubeContext != "" {
		configOverrides.CurrentContext = string(kubeContext)
	}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, err
	}

	// Set default QPS and Burst if not already set
	if config.QPS == 0 {
		config.QPS = 20
	}

	if config.Burst == 0 {
		config.Burst = 30
	}

	return config, nil
}

// cachedAppContext stores the singleton AppContext instance.
// Kong providers are called each time a dependency is requested, but we need
// the same AppContext instance throughout the command lifecycle so that
// initialization in Run() affects the same clients used by processors created in AfterApply.
//
//nolint:gochecknoglobals // Required for singleton pattern with Kong providers
var cachedAppContext *AppContext

// provideAppContext creates the application context with all initialized clients.
// This provider depends on *rest.Config and logging.Logger, which Kong resolves first.
// The result is cached to ensure the same instance is used throughout the command lifecycle.
func provideAppContext(config *rest.Config, log logging.Logger) (*AppContext, error) {
	if cachedAppContext != nil {
		return cachedAppContext, nil
	}

	appCtx, err := NewAppContext(config, log)
	if err != nil {
		return nil, err
	}

	cachedAppContext = appCtx

	return appCtx, nil
}
