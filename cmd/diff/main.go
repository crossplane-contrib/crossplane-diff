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

package main

import (
	"os"

	"github.com/alecthomas/kong"
	"github.com/crossplane/crossplane/v2/cmd/crank/version"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

var _ = kong.Must(&cli{})

type (
	verboseFlag bool
)

func (v verboseFlag) BeforeApply(ctx *kong.Context) error { //nolint:unparam // BeforeApply requires this signature.
	logger := logging.NewLogrLogger(zap.New(zap.UseDevMode(true)))
	ctx.BindTo(logger, (*logging.Logger)(nil))
	return nil
}

// The top-level crossplane CLI.
type cli struct {
	// Subcommands and flags will appear in the CLI help output in the same
	// order they're specified here. Keep them in alphabetical order.

	// Subcommands.
	Diff Cmd `cmd:"" help:"See what changes will be made against a live cluster when a given Crossplane resource would be applied."`

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
		kong.BindToProvider(getRestConfig),
		kong.ConfigureHelp(kong.HelpOptions{
			FlagsLast:      true,
			Compact:        true,
			WrapUpperBound: 80,
		}),
		kong.UsageOnError())
	err := ctx.Run()
	ctx.FatalIfErrorf(err)
}

func getRestConfig() (*rest.Config, error) {
	return clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
}
