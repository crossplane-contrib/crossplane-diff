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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
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
	sigsyaml "sigs.k8s.io/yaml"

	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
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

	imageRepo, _, _ := strings.Cut(environment.GetCrossplaneImage(), ":")
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
				dumpPackageDiagnostics(ctx, cfg)
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
				dumpPackageDiagnostics(ctx, cfg)
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

// diagnosticsLogTailLines bounds how much of each container's log
// dumpPackageDiagnostics prints. Crossplane runs with --debug here, so its full
// log would bury everything else.
const diagnosticsLogTailLines int64 = 200

// dumpPackageDiagnostics writes the state of the shared packages and of the
// Crossplane namespace to stderr. It runs when setup gives up waiting for a
// package to become ready, so the CI log says why the package wasn't ready
// rather than only that it wasn't. The setup failure is already fatal, so this
// is best effort: anything it can't collect is reported inline and skipped.
func dumpPackageDiagnostics(ctx context.Context, cfg *envconf.Config) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	w := os.Stderr
	res := cfg.Client().Resources()

	fmt.Fprintln(w, "===== package diagnostics: begin =====")
	defer fmt.Fprintln(w, "===== package diagnostics: end =====")

	for _, l := range []struct {
		kind string
		list k8s.ObjectList
	}{
		{kind: "functions", list: &pkgv1.FunctionList{}},
		{kind: "functionrevisions", list: &pkgv1.FunctionRevisionList{}},
		{kind: "providers", list: &pkgv1.ProviderList{}},
		{kind: "providerrevisions", list: &pkgv1.ProviderRevisionList{}},
	} {
		fmt.Fprintf(w, "--- %s ---\n", l.kind)

		if err := res.List(ctx, l.list); err != nil {
			fmt.Fprintf(w, "cannot list %s: %v\n", l.kind, err)
			continue
		}

		// managedFields is noise when diagnosing readiness.
		_ = meta.EachListItem(l.list, func(o runtime.Object) error {
			if m, err := meta.Accessor(o); err == nil {
				m.SetManagedFields(nil)
			}

			return nil
		})

		out, err := sigsyaml.Marshal(l.list)
		if err != nil {
			fmt.Fprintf(w, "cannot marshal %s: %v\n", l.kind, err)
			continue
		}

		fmt.Fprintln(w, string(out))
	}

	fmt.Fprintf(w, "--- events in %s ---\n", namespace)

	events := &corev1.EventList{}
	if err := res.WithNamespace(namespace).List(ctx, events); err != nil {
		fmt.Fprintf(w, "cannot list events: %v\n", err)
	}

	for _, e := range events.Items {
		fmt.Fprintf(w, "%s %s %s %s/%s (x%d): %s\n",
			e.LastTimestamp.UTC().Format(time.RFC3339), e.Type, e.Reason,
			e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Count, e.Message)
	}

	fmt.Fprintf(w, "--- pods in %s ---\n", namespace)

	pods := &corev1.PodList{}
	if err := res.WithNamespace(namespace).List(ctx, pods); err != nil {
		fmt.Fprintf(w, "cannot list pods: %v\n", err)
		return
	}

	cs, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
	if err != nil {
		fmt.Fprintf(w, "cannot create clientset for pod logs: %v\n", err)
	}

	for _, p := range pods.Items {
		fmt.Fprintf(w, "pod %s phase=%s\n", p.Name, p.Status.Phase)

		for _, s := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
			fmt.Fprintf(w, "  container %s ready=%t restarts=%d image=%s state=%+v\n",
				s.Name, s.Ready, s.RestartCount, s.Image, s.State)
		}

		if cs == nil {
			continue
		}

		for _, c := range append(p.Spec.InitContainers, p.Spec.Containers...) {
			tail := diagnosticsLogTailLines

			logs, err := cs.CoreV1().Pods(namespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: c.Name, TailLines: &tail}).DoRaw(ctx)
			if err != nil {
				fmt.Fprintf(w, "  cannot get logs of %s/%s: %v\n", p.Name, c.Name, err)
				continue
			}

			fmt.Fprintf(w, "  --- logs %s/%s (last %d lines) ---\n%s\n", p.Name, c.Name, tail, logs)
		}
	}
}
