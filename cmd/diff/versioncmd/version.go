/*
Copyright 2023 The Crossplane Authors.

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

// Package versioncmd contains version cmd
package versioncmd

import (
	"context"
	"fmt"
	"time"

	"github.com/alecthomas/kong"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/kubecfg"
	"github.com/crossplane-contrib/crossplane-diff/internal/versioninfo"
	"github.com/pkg/errors"
	"k8s.io/client-go/rest"
)

const (
	errGetCrossplaneVersion = "unable to get crossplane version"
)

// fetchFunc fetches the Crossplane server version for a given REST config.
// Exposed as a type so tests can substitute a stub on the Cmd struct.
type fetchFunc func(ctx context.Context, cfg *rest.Config) (string, error)

// Cmd represents the version command.
type Cmd struct {
	Client  bool            `env:""                                                          help:"If true, shows client version only (no server required)."`
	Context kubecfg.Context `help:"Kubernetes context to use (defaults to current context)." name:"context"`

	fetch fetchFunc `kong:"-"` // test seam; nil means use FetchCrossplaneVersion.
}

// GetKubeContext implements kubecfg.Provider so the shared REST config provider
// (bound in main via kong.BindToProvider) can resolve a *rest.Config that
// honors the user's kubeconfig context.
func (c *Cmd) GetKubeContext() kubecfg.Context { return c.Context }

// BeforeApply binds the Cmd pointer as the kubecfg.Provider so that providers
// resolved later (in Run) see the parsed --context value.
func (c *Cmd) BeforeApply(ctx *kong.Context) error {
	ctx.BindTo(c, (*kubecfg.Provider)(nil))
	return nil
}

// Run runs the version command.
func (c *Cmd) Run(k *kong.Context) error {
	_, _ = fmt.Fprintln(k.Stdout, "Client Version: "+versioninfo.New().GetVersionString())

	if c.Client {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Resolve the REST config lazily (after flag parsing) via the shared
	// kubecfg provider — honors --context and $KUBECONFIG, falls back to
	// in-cluster only when no kubeconfig is available.
	cfg, err := kubecfg.Provide(c)
	if err != nil {
		return errors.Wrap(err, errGetCrossplaneVersion)
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = FetchCrossplaneVersion
	}

	vxp, err := fetch(ctx, cfg)
	if err != nil {
		return errors.Wrap(err, errGetCrossplaneVersion)
	}

	if vxp != "" {
		_, _ = fmt.Fprintln(k.Stdout, "Server Version: "+vxp)
	}

	return nil
}
