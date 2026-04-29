/*
Copyright 2026 The Crossplane Authors.

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

// Package kubecfg builds *rest.Config instances using kubeconfig loading
// rules, honoring an optional kubeconfig context override. It is the shared
// REST config entry point for crossplane-diff commands so that every command
// consults the same kubeconfig the user's kubectl context points at, rather
// than preferring an in-cluster ServiceAccount when run inside a pod.
package kubecfg

import (
	"fmt"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Context is a kubeconfig context name. Kong binds flag values through this
// type so providers can distinguish it from other plain strings when resolved.
type Context string

// Provider supplies the kubeconfig context the caller wants to use.
// Commands implement this by exposing a --context flag.
type Provider interface {
	GetKubeContext() Context
}

// Provide builds a *rest.Config using the provider's context.
//
// Resolution order:
//  1. Standard clientcmd loading rules ($KUBECONFIG, then $HOME/.kube/config).
//  2. If the provider supplies a non-empty context, it overrides the
//     kubeconfig's current-context.
//  3. If no kubeconfig is available at all, fall back to the in-cluster
//     ServiceAccount config and emit a warning to stderr.
//
// This differs from controller-runtime's GetConfig, which prefers in-cluster
// first — that behavior causes `crossplane-diff` running inside a pod to
// ignore the user's kubeconfig context.
func Provide(p Provider) (*rest.Config, error) {
	return provide(p, rest.InClusterConfig, func(msg string) {
		fmt.Fprintln(os.Stderr, "warning: "+msg)
	})
}

// provide is the testable core of Provide. It takes the in-cluster config
// loader and a warning sink as seams.
func provide(p Provider, inCluster func() (*rest.Config, error), warn func(msg string)) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()

	overrides := &clientcmd.ConfigOverrides{}
	if kc := p.GetKubeContext(); kc != "" {
		overrides.CurrentContext = string(kc)
	}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

	cfg, err := kubeConfig.ClientConfig()
	if err != nil {
		if clientcmd.IsEmptyConfig(err) {
			icc, iccErr := inCluster()
			if iccErr == nil {
				warn("no kubeconfig found, falling back to in-cluster config")
				applyDefaults(icc)

				return icc, nil
			}
			// Return the original empty-config error — it's more actionable
			// to a user who thinks they provided a kubeconfig than the
			// in-cluster "token not found" error.
			return nil, err
		}

		return nil, err
	}

	applyDefaults(cfg)

	return cfg, nil
}

func applyDefaults(cfg *rest.Config) {
	if cfg.QPS == 0 {
		cfg.QPS = 20
	}

	if cfg.Burst == 0 {
		cfg.Burst = 30
	}
}
