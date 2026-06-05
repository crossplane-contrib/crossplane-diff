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
	"bytes"
	"context"
	"os"
	"os/exec"
	"sync"

	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	renderv1alpha1 "github.com/crossplane/cli/v2/proto/render/v1alpha1"
	"google.golang.org/protobuf/proto"
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

// NewEngineRenderFn constructs an engineRenderFn backed by the default Docker
// render engine (via render.NewEngineFromFlags with zero-value EngineFlags).
func NewEngineRenderFn(log logging.Logger) *EngineRenderFn {
	// If CROSSPLANE_RENDER_BINARY points at a local `crossplane` binary, use
	// our stderr-capturing engine instead of the upstream localRenderEngine.
	// The upstream implementation forwards stderr directly to os.Stderr, which
	// means fatal-result messages are visible on the terminal but are NOT
	// included in the returned Go error — making programmatic inspection (e.g.
	// in integration tests) impossible. Our engine captures stderr and includes
	// it in the error string so callers can surface it to users and tests can
	// assert on it.
	var engine render.Engine
	if bin := os.Getenv("CROSSPLANE_RENDER_BINARY"); bin != "" {
		engine = &stderrCapturingLocalEngine{binaryPath: bin}
	} else {
		// Empty EngineFlags → upstream cli falls back to
		// xpkg.crossplane.io/crossplane/crossplane:stable, which gets advanced on
		// each crossplane release. (We previously hardcoded "main" here to dodge
		// an empty-tag issue in the older crank API; that path is no longer
		// needed and ":main" on xpkg has been stale since upstream's nix
		// migration anyway.) User-facing override flags are tracked separately.
		engine = render.NewEngineFromFlags(&render.EngineFlags{}, log)
	}

	return &EngineRenderFn{
		engine:        engine,
		log:           log,
		startRuntimes: render.StartFunctionRuntimes,
		stopRuntimes:  render.StopFunctionRuntimes,
	}
}

// stderrCapturingLocalEngine is a render.Engine that runs a local crossplane
// binary for rendering, identical to the upstream localRenderEngine, except
// that it captures stderr into a buffer and includes it in the returned error.
// The upstream implementation forwards stderr directly to os.Stderr, so any
// fatal-result messages from function containers are visible on the terminal
// but NOT included in the Go error — making it impossible for callers (and
// tests) to inspect them programmatically.
type stderrCapturingLocalEngine struct {
	binaryPath string
}

func (e *stderrCapturingLocalEngine) CheckContextSupport() error { return nil }

// Setup is a no-op: function containers publish ports to localhost, so there
// is nothing extra to configure for the local engine.
func (e *stderrCapturingLocalEngine) Setup(_ context.Context, _ []pkgv1.Function) (func(), error) {
	return func() {}, nil
}

// Render marshals req, runs it through the local binary, and returns the
// parsed response. If the binary exits non-zero, the captured stderr output
// is included verbatim in the returned error so callers can surface it.
func (e *stderrCapturingLocalEngine) Render(ctx context.Context, req *renderv1alpha1.RenderRequest) (*renderv1alpha1.RenderResponse, error) {
	data, err := proto.Marshal(req)
	if err != nil {
		return nil, errors.Wrap(err, "cannot marshal render request")
	}

	var stderrBuf bytes.Buffer

	cmd := exec.CommandContext(ctx, e.binaryPath, "internal", "render") //nolint:gosec // The binary path is user-supplied via env var.
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stderr = &stderrBuf

	out, err := cmd.Output()

	// As of crossplane v2.4 (PR #7446), `crossplane internal render` exits
	// with code 3 and a populated stdout when a pipeline step returns a
	// SEVERITY_FATAL result, so callers can recover the partial
	// CompositeOutput (especially RequiredResources) and iterate. Other
	// non-zero exits indicate hard failures with no usable stdout.
	const exitCodePipelineFatal = 3

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		// Success path; fall through to unmarshal.
	case errors.As(err, &exitErr) && exitErr.ExitCode() == exitCodePipelineFatal && len(out) > 0:
		// Partial output on pipeline-fatal. Unmarshal and surface both.
		rsp := &renderv1alpha1.RenderResponse{}
		if uerr := proto.Unmarshal(out, rsp); uerr != nil {
			return nil, errors.Errorf("cannot unmarshal partial render response after pipeline fatal: %s: %s", uerr.Error(), stderrBuf.String())
		}
		return rsp, errors.Errorf("crossplane internal render: pipeline returned fatal: %s", stderrBuf.String())
	default:
		return nil, errors.Errorf("cannot run crossplane internal render: %s: %s", err.Error(), stderrBuf.String())
	}

	rsp := &renderv1alpha1.RenderResponse{}
	if err := proto.Unmarshal(out, rsp); err != nil {
		return nil, errors.Wrap(err, "cannot unmarshal render response")
	}

	return rsp, nil
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
