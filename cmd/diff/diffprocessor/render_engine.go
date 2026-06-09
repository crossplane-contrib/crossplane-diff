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

package diffprocessor

import (
	"context"
	"sync"

	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	corev1 "k8s.io/api/core/v1"
	kunstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/kube-openapi/pkg/spec3"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	composed "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	ucomposite "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
)

// RenderFn is the render abstraction injected into diff processors. Callers
// provide the Function CRs they already have; the implementation owns the
// engine and FunctionAddresses lifecycle.
type RenderFn func(ctx context.Context, log logging.Logger, in RenderInputs) (render.CompositionOutputs, error)

// RenderInputs carries what the diff processor already holds. It deliberately
// omits FunctionAddrs — that's engine state, not caller state.
type RenderInputs struct {
	CompositeResource   *ucomposite.Unstructured
	Composition         *apiextensionsv1.Composition
	Functions           []pkgv1.Function
	FunctionCredentials []corev1.Secret
	ObservedResources   []composed.Unstructured
	RequiredResources   []kunstructured.Unstructured
	RequiredSchemas     []spec3.OpenAPI

	// XRD is the CompositeResourceDefinition the binary should consider
	// when rendering. Set by RenderToStableState based on a defClient lookup
	// so the binary can pick the right composite.Schema (Legacy vs Modern)
	// for the input XR's GVK. The renderer then writes canonical fields at
	// the path the cluster CRD declares (spec.* for v1, spec.crossplane.*
	// for v2), making dry-run apply succeed and the rendered desired
	// comparable against cluster state. Optional; when nil the binary
	// falls back to SchemaModern.
	XRD *kunstructured.Unstructured
}

// EngineRenderFn is the default RenderFn implementation. It lazily sets up the
// render engine and starts function runtimes on first use, reuses both across
// subsequent calls, and serializes concurrent renders with an internal mutex.
type EngineRenderFn struct {
	engine         render.Engine
	fnAddrs        *render.FunctionAddresses
	networkCleanup func()
	started        bool
	mu             sync.Mutex
	log            logging.Logger

	// startRuntimes / stopRuntimes are seams for testing. They default to the
	// real render package functions.
	startRuntimes func(ctx context.Context, log logging.Logger, fns []pkgv1.Function) (*render.FunctionAddresses, error)
	stopRuntimes  func(log logging.Logger, fa *render.FunctionAddresses)
}

// NewEngineRenderFn constructs an EngineRenderFn.
//
// binaryPath threads through to render.EngineFlags.CrossplaneBinary, so when
// non-empty the upstream localRenderEngine drives rendering against a local
// `crossplane` binary; when empty the upstream dockerRenderEngine pulls
// xpkg.crossplane.io/crossplane/crossplane:stable. Both engines now capture
// stderr into the returned error and honour exit code 3 (partial-output-on-
// fatal — crossplane/crossplane#7455) per crossplane/cli#91, so we no longer
// wrap them. Our EngineRenderFn.Render still expects (rsp != nil, err != nil)
// on pipeline fatal and surfaces both — see the comment there.
func NewEngineRenderFn(log logging.Logger, binaryPath string) *EngineRenderFn {
	return &EngineRenderFn{
		engine:        render.NewEngineFromFlags(&render.EngineFlags{CrossplaneBinary: binaryPath}, log),
		log:           log,
		startRuntimes: render.StartFunctionRuntimes,
		stopRuntimes:  render.StopFunctionRuntimes,
	}
}

// Render performs one render. It is safe for concurrent use — calls are
// serialized internally. On the first invocation it runs engine.Setup and
// startRuntimes; subsequent invocations reuse both.
func (e *EngineRenderFn) Render(ctx context.Context, log logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.started {
		cleanup, err := e.engine.Setup(ctx, in.Functions)
		if err != nil {
			return render.CompositionOutputs{}, errors.Wrap(err, "cannot setup render engine")
		}

		e.networkCleanup = cleanup

		fnAddrs, err := e.startRuntimes(ctx, log, in.Functions)
		if err != nil {
			// Unwind the setup cleanup so we don't leak networks on a failed start.
			if e.networkCleanup != nil {
				e.networkCleanup()
				e.networkCleanup = nil
			}

			return render.CompositionOutputs{}, errors.Wrap(err, "cannot start function runtimes")
		}

		e.fnAddrs = fnAddrs
		e.started = true
	}

	req, err := render.BuildCompositeRequest(render.CompositionInputs{
		CompositeResource:   in.CompositeResource,
		Composition:         in.Composition,
		FunctionAddrs:       e.fnAddrs.Addresses(),
		FunctionCredentials: in.FunctionCredentials,
		ObservedResources:   in.ObservedResources,
		RequiredResources:   in.RequiredResources,
		RequiredSchemas:     in.RequiredSchemas,
		XRD:                 in.XRD,
	})
	if err != nil {
		return render.CompositionOutputs{}, errors.Wrap(err, "cannot build render request")
	}

	rsp, renderErr := e.engine.Render(ctx, req)
	// When a pipeline step returns SEVERITY_FATAL, the binary exits with a
	// distinct code, surfaces a populated rsp, and returns an error. Treat
	// this case as "we have partial output; preserve it AND the error so
	// the caller (RenderToStableState) can iterate on RequiredResources
	// even when the pipeline fataled."
	if renderErr != nil && rsp == nil {
		return render.CompositionOutputs{}, errors.Wrap(renderErr, "cannot render")
	}

	out, err := render.ParseCompositeResponse(rsp.GetComposite())
	if err != nil {
		return render.CompositionOutputs{}, errors.Wrap(err, "cannot parse render response")
	}

	if renderErr != nil {
		return out, errors.Wrap(renderErr, "render returned partial output after pipeline fatal")
	}
	return out, nil
}

// Cleanup stops any running function runtimes and releases the engine's
// network. It is idempotent and safe to call when Render was never invoked.
func (e *EngineRenderFn) Cleanup(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.fnAddrs != nil {
		e.stopRuntimes(e.log, e.fnAddrs)
		e.fnAddrs = nil
	}

	if e.networkCleanup != nil {
		e.networkCleanup()
		e.networkCleanup = nil
	}

	e.started = false

	return nil
}
