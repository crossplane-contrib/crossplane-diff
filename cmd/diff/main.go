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
	"time"

	"github.com/alecthomas/kong"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/version"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

var _ = kong.Must(&cli{})

type (
	verboseFlag bool
)

// CommonCmdFields contains common fields shared by both XR and Comp commands.
type CommonCmdFields struct {
	// Configuration options
	Context        string        `help:"Kubernetes context to use (defaults to current context)." name:"context"`
	NoColor        bool          `help:"Disable colorized output."                                 name:"no-color"`
	Compact        bool          `help:"Show compact diffs with minimal context."                  name:"compact"`
	MaxNestedDepth int           `default:"10"                                                     help:"Maximum depth for nested XR recursion." name:"max-nested-depth"`
	Timeout        time.Duration `default:"1m"                                                     help:"How long to run before timing out."`
}

func (v verboseFlag) BeforeApply(ctx *kong.Context) error { //nolint:unparam // BeforeApply requires this signature.
	logger := logging.NewLogrLogger(zap.New(zap.UseDevMode(true)))
	ctx.BindTo(logger, (*logging.Logger)(nil))

	return nil
}

// BeforeApply creates and binds the REST config based on the context flag.
// This makes rest.Config available as an injected dependency to commands.
func (c *CommonCmdFields) BeforeApply(ctx *kong.Context) error {
	// Get the logger (may be nop logger or verbose logger depending on --verbose flag)
	var logger logging.Logger
	ctx.Bind(&logger)

	// Create rest config for the specified context
	config, err := getRestConfig(c.Context)
	if err != nil {
		return errors.Wrap(err, "cannot get rest config")
	}

	// Set default rate limits
	config = initRestConfig(config, logger)

	// Bind the config so it's available to AfterApply and Run methods
	ctx.BindTo(config, (*rest.Config)(nil))

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
	ctx := kong.Parse(&cli{},
		kong.Name("crossplane-diff"),
		kong.Description("A command line tool for diffing  Crossplane resources."),
		// Binding a variable to kong context makes it available to all commands
		// at runtime.
		kong.BindTo(logger, (*logging.Logger)(nil)),
		kong.ConfigureHelp(kong.HelpOptions{
			FlagsLast:      true,
			Compact:        true,
			WrapUpperBound: 80,
		}),
		kong.UsageOnError())
	err := ctx.Run()
	ctx.FatalIfErrorf(err)
}

func getRestConfig(context string) (*rest.Config, error) {
	// Use the standard client-go loading rules:
	// 1. If KUBECONFIG env var is set, use that
	// 2. Otherwise, use ~/.kube/config
	// 3. Respects current context from the kubeconfig (or uses specified context)
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}

	// If a specific context is requested, override the current context
	if context != "" {
		configOverrides.CurrentContext = context
	}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	return kubeConfig.ClientConfig()
}
