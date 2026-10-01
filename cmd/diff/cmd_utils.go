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
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	ld "github.com/crossplane/cli/v2/cmd/crossplane/common/load"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// signalSource registers and unregisters signal delivery; it exists so tests can deliver signals without
// signalling the test process.
type signalSource struct {
	notify func(c chan<- os.Signal, sig ...os.Signal)
	stop   func(c chan<- os.Signal)
}

// osSignals is the real signal source.
var osSignals = signalSource{notify: signal.Notify, stop: signal.Stop} //nolint:gochecknoglobals // stateless adapter over os/signal

// newRunContext returns the run's context: cancelled when timeout expires, when the returned cancel is
// called, or when the process receives SIGINT or SIGTERM. Without this, Go's default handling of those
// signals kills the process outright and no deferred cleanup runs, leaking function containers.
//
// An interrupt cancels the context with an *diffprocessor.InterruptedError as its cause, and then
// unregisters the handler, so a second signal falls back to Go's default and kills the process. That
// is the way out if cleanup hangs (it is bounded only by diffprocessor.CleanupTimeout).
func newRunContext(timeout time.Duration, signals signalSource) (context.Context, context.CancelFunc) {
	runCtx, cancelRun := context.WithCancelCause(context.Background())

	sigCh := make(chan os.Signal, 1)
	signals.notify(sigCh, os.Interrupt, syscall.SIGTERM)

	var stopOnce sync.Once

	stop := func() { stopOnce.Do(func() { signals.stop(sigCh) }) }

	done := make(chan struct{})

	go func() {
		select {
		case sig := <-sigCh:
			stop()
			cancelRun(&dp.InterruptedError{Signal: sig})
		case <-done:
		}
	}()

	ctx, cancelTimeout := context.WithTimeout(runCtx, timeout)

	var cancelOnce sync.Once

	return ctx, func() {
		cancelOnce.Do(func() {
			stop()
			close(done)
			cancelTimeout()
			cancelRun(context.Canceled)
		})
	}
}

// interruptedRunResult reports an interrupted run as such: when ctx was cancelled by a signal it
// returns the interruption as the run's error and sets the matching exit code, replacing whatever the
// run produced (mostly the cancelled calls the interruption caused). Otherwise err is returned as is.
func interruptedRunResult(ctx context.Context, err error, exitCode *ExitCode) error {
	ie := dp.InterruptCause(ctx)
	if ie == nil {
		return err
	}

	exitCode.Code = dp.DetermineExitCode(ie, false)

	return ie
}

// initializeAppContext initializes the application context with timeout, signal handling (see
// newRunContext) and error handling.
func initializeAppContext(timeout time.Duration, appCtx *AppContext, log logging.Logger) (context.Context, context.CancelFunc, error) {
	ctx, cancel := newRunContext(timeout, osSignals)
	if err := appCtx.Initialize(ctx, log); err != nil {
		cancel()
		return nil, nil, errors.Wrap(err, "cannot initialize client")
	}

	return ctx, cancel, nil
}

// defaultProcessorOptions returns the standard default options used by both XR and composition processors.
// This is the single source of truth for behavior defaults in the CLI layer.
func defaultProcessorOptions(fields CommonCmdFields) []dp.ProcessorOption {
	// IgnorePaths carries only what the user asked to mask. The paths that are always filtered
	// (kubectl's last-applied-configuration) are stripped unconditionally by the renderer instead —
	// see renderer.alwaysIgnoredPaths. Prepending them here would make "the user passed
	// --ignore-paths" indistinguishable from "we always ignore something", which comp-diff relies on
	// to tell an edited composition from a merely re-applied one.
	opts := []dp.ProcessorOption{
		dp.WithColorize(!fields.NoColor),
		dp.WithCompact(fields.Compact),
		dp.WithMaxNestedDepth(fields.MaxNestedDepth),
		dp.WithMaxRenderIterations(fields.MaxIterations),
		dp.WithEventualState(fields.EventualState),
		dp.WithIgnorePaths(fields.IgnorePaths),
		dp.WithDryRunOn(dp.DryRunOn(fields.DryRunOn)),
	}

	// Add output format option
	// Import renderer package to use OutputFormat type
	var outputFormat renderer.OutputFormat

	switch renderer.OutputFormat(fields.Output) {
	case renderer.OutputFormatJSON:
		outputFormat = renderer.OutputFormatJSON
	case renderer.OutputFormatYAML:
		outputFormat = renderer.OutputFormatYAML
	case renderer.OutputFormatDiff:
		outputFormat = renderer.OutputFormatDiff
	default:
		// Empty string or unrecognized values fall back to the human-readable diff.
		outputFormat = renderer.OutputFormatDiff
	}

	opts = append(opts, dp.WithOutputFormat(outputFormat))

	// Add function credentials if provided (empty path with no secrets errors in FunctionCredentials.Decode)
	if len(fields.FunctionCredentials.Secrets) > 0 {
		opts = append(opts, dp.WithFunctionCredentials(fields.FunctionCredentials.Secrets))
	}

	if fields.FunctionRegistryOverride != "" {
		opts = append(opts, dp.WithFunctionRegistryOverride(fields.FunctionRegistryOverride))
	}

	if fields.CrossplaneRenderBinary != "" {
		opts = append(opts, dp.WithCrossplaneRenderBinary(fields.CrossplaneRenderBinary))
	}

	if fields.CrossplaneVersion != "" {
		opts = append(opts, dp.WithCrossplaneVersion(fields.CrossplaneVersion))
	}

	if fields.CrossplaneImage != "" {
		opts = append(opts, dp.WithCrossplaneImage(fields.CrossplaneImage))
	}

	return opts
}

// LoadFunctionCredentials loads Secret resources from a YAML file or directory.
// The function supports both single files and directories containing YAML files.
// Only resources of kind "Secret" are returned; other resources are silently skipped.
func LoadFunctionCredentials(path string) ([]corev1.Secret, error) {
	if path == "" {
		return nil, nil
	}

	// Use the crossplane loader which handles files, directories, and multi-document YAML
	loader, err := ld.NewLoader(path)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot create loader for path %q", path)
	}

	resources, err := loader.Load()
	if err != nil {
		return nil, errors.Wrapf(err, "cannot load resources from %q", path)
	}

	secrets := make([]corev1.Secret, 0, len(resources))

	for _, res := range resources {
		// Only process Secret resources
		if res.GetKind() != "Secret" || res.GetAPIVersion() != "v1" {
			continue
		}

		// Convert unstructured to corev1.Secret
		secret := corev1.Secret{}

		err := runtime.DefaultUnstructuredConverter.FromUnstructured(res.UnstructuredContent(), &secret)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot convert Secret %q to corev1.Secret", res.GetName())
		}

		secrets = append(secrets, secret)
	}

	return secrets, nil
}
