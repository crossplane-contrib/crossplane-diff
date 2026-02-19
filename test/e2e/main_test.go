/*
Copyright 2022 The Crossplane Authors.

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

// Package e2e implements end-to-end tests for Crossplane.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/e2e-framework/klient/conf"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/support/kind"
	"sigs.k8s.io/e2e-framework/third_party/helm"

	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
	"github.com/crossplane/crossplane/v2/test/e2e/config"
	"github.com/crossplane/crossplane/v2/test/e2e/funcs"
)

// TODO(phisco): make it configurable.
const namespace = "crossplane-system"

const (
	// TODO(phisco): make it configurable.
	helmChartDir = "cluster/%s/charts/crossplane"
	// TODO(phisco): make it configurable.
	helmReleaseName = "crossplane"
)

var environment = config.NewEnvironmentFromFlags()

func TestMain(m *testing.M) {
	// TODO(negz): Global loggers are dumb and klog is dumb. Remove this when
	// e2e-framework is running controller-runtime v0.15.x per
	// https://github.com/kubernetes-sigs/e2e-framework/issues/270
	log.SetLogger(klog.NewKlogr())

	// Parse flags to ensure we have the environment configured
	cfg, err := envconf.NewFromFlags()
	if err != nil {
		panic(err)
	}

	imageRepo := strings.Split(environment.GetCrossplaneImage(), ":")[0]
	imageTag := strings.Split(environment.GetCrossplaneImage(), ":")[1]

	versionedHelmChartDir := fmt.Sprintf(helmChartDir, imageTag)

	// Set the default suite, to be used as base for all the other suites.
	environment.AddDefaultTestSuite(
		config.WithoutBaseDefaultTestSuite(),
		config.WithHelmInstallOpts(
			helm.WithName(helmReleaseName),
			helm.WithNamespace(namespace),
			helm.WithChart(versionedHelmChartDir),
			// wait for the deployment to be ready for up to 5 minutes before returning
			helm.WithWait(),
			helm.WithTimeout("5m"),
			helm.WithArgs(
				// Run with debug logging to ensure all log statements are run.
				"--set args={--debug}",
				"--set image.repository="+imageRepo,
				"--set image.tag="+imageTag,
				"--set metrics.enabled=true",
			),
		),
		config.WithLabelsToSelect(features.Labels{
			config.LabelTestSuite: []string{config.TestSuiteDefault},
		}),
	)

	var (
		setup  []env.Func
		finish []env.Func
	)

	if environment.IsKindCluster() {
		setup = append(setup, envfuncs.CreateClusterWithConfig(
			kind.NewProvider(),
			environment.GetKindClusterName(),
			"./test/e2e/manifests/kind/kind-config.yaml",
		))
	} else {
		cfg.WithKubeconfigFile(conf.ResolveKubeConfigFile())
	}

	// Enrich the selected labels with the ones from the suite.
	// Not replacing the user provided ones if any.
	cfg.WithLabels(environment.EnrichLabels(cfg.Labels()))

	environment.SetEnvironment(env.NewWithConfig(cfg))

	if environment.ShouldLoadImages() {
		clusterName := environment.GetKindClusterName()
		setup = append(setup,
			envfuncs.LoadDockerImageToCluster(clusterName, environment.GetCrossplaneImage()),
		)
	}

	// Add the setup functions defined by the suite being used
	setup = append(setup,
		environment.GetSelectedSuiteAdditionalEnvSetup()...,
	)

	if environment.ShouldInstallCrossplane() {
		setup = append(setup,
			envfuncs.CreateNamespace(namespace),
			environment.HelmInstallBaseCrossplane(),
		)
	}

	// We always want to add our types to the scheme.
	setup = append(setup, funcs.AddCrossplaneTypesToScheme(), funcs.AddCRDsToScheme())

	// Install shared functions and providers for all tests in this variant
	sharedSetupPath := filepath.Join("test/e2e/manifests/beta/diff", imageTag, "_setup")

	setup = append(setup, func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		client, err := cfg.NewClient()
		if err != nil {
			return ctx, fmt.Errorf("failed to create k8s client: %w", err)
		}

		// Read and apply all YAML files in the setup directory
		files, err := filepath.Glob(filepath.Join(sharedSetupPath, "*.yaml"))
		if err != nil {
			return ctx, fmt.Errorf("failed to glob setup files: %w", err)
		}

		for _, file := range files {
			f, err := os.Open(file)
			if err != nil {
				return ctx, fmt.Errorf("failed to open %s: %w", file, err)
			}

			decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)

			for {
				obj := &unstructured.Unstructured{}
				if err := decoder.Decode(obj); err != nil {
					if errors.Is(err, io.EOF) {
						break
					}

					f.Close()

					return ctx, fmt.Errorf("failed to decode %s: %w", file, err)
				}

				if err := client.Resources().Create(ctx, obj); err != nil {
					f.Close()
					return ctx, fmt.Errorf("failed to create resource from %s: %w", file, err)
				}
			}

			f.Close()
		}

		// Wait for functions to be ready
		functionList := &pkgv1.FunctionList{}
		if err := wait.For(conditions.New(client.Resources()).ResourcesFound(functionList), wait.WithTimeout(30*time.Second)); err != nil {
			return ctx, fmt.Errorf("functions not found: %w", err)
		}

		if err := client.Resources().List(ctx, functionList); err != nil {
			return ctx, fmt.Errorf("failed to list functions: %w", err)
		}

		for _, fn := range functionList.Items {
			obj := fn.DeepCopy()
			if err := wait.For(conditions.New(client.Resources()).ResourceMatch(obj, func(object k8s.Object) bool {
				fn := object.(*pkgv1.Function)

				return fn.Status.GetCondition(pkgv1.TypeHealthy).Status == "True" &&
					fn.Status.GetCondition(pkgv1.TypeInstalled).Status == "True"
			}), wait.WithTimeout(3*time.Minute)); err != nil {
				return ctx, fmt.Errorf("function %s not ready: %w", fn.Name, err)
			}
		}

		// Wait for provider to be ready
		providerList := &pkgv1.ProviderList{}
		if err := wait.For(conditions.New(client.Resources()).ResourcesFound(providerList), wait.WithTimeout(30*time.Second)); err != nil {
			return ctx, fmt.Errorf("providers not found: %w", err)
		}

		if err := client.Resources().List(ctx, providerList); err != nil {
			return ctx, fmt.Errorf("failed to list providers: %w", err)
		}

		for _, prov := range providerList.Items {
			obj := prov.DeepCopy()
			if err := wait.For(conditions.New(client.Resources()).ResourceMatch(obj, func(object k8s.Object) bool {
				prov := object.(*pkgv1.Provider)

				return prov.Status.GetCondition(pkgv1.TypeHealthy).Status == "True" &&
					prov.Status.GetCondition(pkgv1.TypeInstalled).Status == "True"
			}), wait.WithTimeout(2*time.Minute)); err != nil {
				return ctx, fmt.Errorf("provider %s not ready: %w", prov.Name, err)
			}
		}

		return ctx, nil
	})

	if environment.ShouldCollectKindLogsOnFailure() {
		finish = append(finish, envfuncs.ExportClusterLogs(environment.GetKindClusterName(), environment.GetKindClusterLogsLocation()))
	}

	// We want to destroy the cluster if we created it, but only if we created it,
	// otherwise the random name will be meaningless.
	if environment.ShouldDestroyKindCluster() {
		finish = append(finish, envfuncs.DestroyCluster(environment.GetKindClusterName()))
	}

	// Check that all features are specifying a suite they belong to via LabelTestSuite.
	//nolint:thelper // We can't make testing.T the second argument because we want to satisfy types.FeatureEnvFunc.
	environment.BeforeEachFeature(func(ctx context.Context, _ *envconf.Config, t *testing.T, feature features.Feature) (context.Context, error) {
		t.Helper()

		if _, exists := feature.Labels()[config.LabelTestSuite]; !exists {
			t.Fatalf("Feature %q does not have the required %q label set", feature.Name(), config.LabelTestSuite)
		}

		return ctx, nil
	})

	environment.Setup(setup...)
	environment.Finish(finish...)
	os.Exit(environment.Run(m))
}
