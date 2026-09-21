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
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
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

// EngineRenderFn is the default RenderFn implementation. It lazily starts
// function runtimes on first use, reuses them across subsequent calls, and
// serializes concurrent renders with an internal mutex.
//
// A single `xr` invocation can render against XRs from multiple compositions
// whose function pipelines overlap but aren't identical. Engine.Setup
// (crossplane/cli#159) accepts being called once per batch: the call that
// creates a new environment — if any — returns a real cleanup; calls that
// integrate fns into an environment that already exists (because a prior
// Setup call established it, or because the engine was pre-configured to use
// an externally-managed environment via --crossplane-docker-network) return a
// no-op cleanup, as do calls on engines with nothing to clean up (the local
// engine). We accumulate every returned cleanup in a slice and call them in
// LIFO order on Cleanup without coordinating which one is real, per the
// caller contract in the upstream Engine.Setup docstring.
type EngineRenderFn struct {
	engine    render.Engine
	cleanups  []func()
	addrsList []*render.FunctionAddresses
	// startedNames is the set of function names we've already passed to
	// startRuntimes; used for dedup so we don't restart a function that's
	// already running.
	startedNames map[string]struct{}
	// addrs is the merged Addresses() map from every startRuntimes call.
	// Filtered to in.Functions when building each render request.
	addrs map[string]string
	mu    sync.Mutex
	log   logging.Logger

	// image is the fully-resolved render image reference the engine will pull,
	// or "" when a local binary backend was selected. Written once at
	// construction and never mutated, so it is read without holding mu.
	image string

	// startRuntimes / stopRuntimes are seams for testing. They default to the
	// real render package functions.
	startRuntimes func(ctx context.Context, log logging.Logger, fns []pkgv1.Function) (*render.FunctionAddresses, error)
	stopRuntimes  func(log logging.Logger, fa *render.FunctionAddresses)
}

// NewEngineRenderFn constructs an EngineRenderFn.
//
// binaryPath, version, and image select the render backend via
// render.EngineFlags (they are mutually exclusive upstream, grouped under the
// "crossplane-selector" xor):
//   - binaryPath (CrossplaneBinary): drives the upstream localRenderEngine
//     against a local `crossplane` binary.
//   - version (CrossplaneVersion): the docker engine pulls
//     xpkg.crossplane.io/crossplane/crossplane:<version>.
//   - image (CrossplaneImage): the docker engine pulls that full image ref.
//
// When all three are empty the docker engine pulls
// xpkg.crossplane.io/crossplane/crossplane:stable. Both engines now capture
// stderr into the returned error and honour exit code 3 (partial-output-on-
// fatal — crossplane/crossplane#7455) per crossplane/cli#91, so we no longer
// wrap them. Our EngineRenderFn.Render still expects (rsp != nil, err != nil)
// on pipeline fatal and surfaces both — see the comment there.
//
// CROSSPLANE_DIFF_DOCKER_NETWORK is read once here and threaded through
// render.EngineFlags.CrossplaneDockerNetwork (crossplane/cli#65). The docker
// engine then runs the crossplane-render container on that network AND
// annotates each fn at Setup time so its container joins it too — closing
// both halves of the "crossplane-diff inside a container" case
// (crossplane/cli#75). For the local engine the flag is a no-op.
func NewEngineRenderFn(log logging.Logger, binaryPath, version, image string) *EngineRenderFn {
	// Normalize before the value reaches EngineFlags: upstream formats it into
	// the tag verbatim and only v-prefixed tags are published, so the bare form
	// ValidateMinRenderVersion accepts would otherwise pass validation and then
	// fail at pull time. This is the single EngineFlags construction site, so
	// normalizing here also covers callers that set the version programmatically
	// via WithCrossplaneVersion rather than through the CLI flag.
	version = NormalizeRenderVersion(version)

	// A full image reference is floor-checked at the CLI boundary only as far as
	// its tag permits; when it carries no comparable version there is nothing to
	// check, and the user should hear that rather than assume the minimum was
	// enforced. Advisory only — an unverifiable reference is not an invalid one.
	if w := UncomparableRenderImageWarning(image); w != "" {
		log.Info(w)
	}

	return &EngineRenderFn{
		engine: render.NewEngineFromFlags(&render.EngineFlags{
			CrossplaneBinary:        binaryPath,
			CrossplaneVersion:       version,
			CrossplaneImage:         image,
			CrossplaneDockerNetwork: os.Getenv(EnvDockerNetwork),
		}, log),
		image:         resolveRenderImage(binaryPath, version, image),
		log:           log,
		startRuntimes: render.StartFunctionRuntimes,
		stopRuntimes:  render.StopFunctionRuntimes,
	}
}

// RenderImage returns the fully-resolved render image reference the engine will
// pull, or "" when a local crossplane binary was selected instead. It exists so
// callers — and tests — can observe which backend a processor actually ended up
// with; the upstream Engine interface exposes no accessor for it, so before this
// the selector arguments vanished at the boundary and dropping them entirely was
// undetectable (crossplane-diff#488).
func (e *EngineRenderFn) RenderImage() string {
	return e.image
}

// resolveRenderImage mirrors the upstream render package's unexported
// crossplaneImageFromFlags. Keep the two in sync: the same inputs must produce
// the same reference, or RenderImage() misreports what actually ran.
func resolveRenderImage(binaryPath, version, image string) string {
	switch {
	case binaryPath != "":
		// The local engine runs a binary; it pulls nothing.
		return ""
	case image != "":
		return image
	case version != "":
		return render.DefaultCrossplaneImage + ":" + version
	default:
		return render.DefaultCrossplaneImage + ":stable"
	}
}

// Render performs one render. It is safe for concurrent use — calls are
// serialized internally. Setup runs only on invocations that introduce
// previously-unseen functions; renders whose fns are all already running
// skip straight to building the request. See the EngineRenderFn docstring
// for the per-batch Setup contract this relies on.
func (e *EngineRenderFn) Render(ctx context.Context, log logging.Logger, in RenderInputs) (render.CompositionOutputs, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.addrs == nil {
		e.addrs = make(map[string]string)
	}

	if e.startedNames == nil {
		e.startedNames = make(map[string]struct{})
	}

	// Identify functions we haven't started yet. Dedup is by function name —
	// independent of whether the address map has an entry, so test stubs that
	// return an empty *FunctionAddresses don't mistakenly re-trigger Start.
	newFns := make([]pkgv1.Function, 0, len(in.Functions))
	for i := range in.Functions {
		if _, ok := e.startedNames[in.Functions[i].GetName()]; ok {
			continue
		}

		newFns = append(newFns, in.Functions[i])
	}

	// Only call Setup when there's actually a new batch to integrate.
	// Renders where all fns are already running (newFns is empty) have no
	// work for the engine and would just accumulate no-op cleanups in the
	// slice for the lifetime of the engine.
	if len(newFns) > 0 {
		// Setup integrates newFns into the engine's environment. Whether
		// this call creates the environment or only adds to one that
		// already exists is the engine's concern; we just hold onto
		// whatever cleanup it gives back and let Cleanup walk them LIFO.
		cleanup, err := e.engine.Setup(ctx, newFns)
		if err != nil {
			return render.CompositionOutputs{}, errors.Wrap(err, "cannot setup render engine")
		}

		fa, startErr := e.startRuntimes(ctx, log, newFns)
		if startErr != nil {
			// Roll back this Setup. If this call created the environment,
			// cleanup releases it; otherwise cleanup is a no-op and no
			// harm is done.
			cleanup()
			return render.CompositionOutputs{}, errors.Wrap(startErr, "cannot start function runtimes")
		}

		e.addrsList = append(e.addrsList, fa)
		for i := range newFns {
			e.startedNames[newFns[i].GetName()] = struct{}{}
		}

		maps.Copy(e.addrs, fa.Addresses())
		e.cleanups = append(e.cleanups, cleanup)
	}

	// Build request with addresses for in.Functions only — the binary needs
	// addresses for this render's pipeline, not for every function we've
	// ever started.
	fnAddrs := make(map[string]string, len(in.Functions))
	for i := range in.Functions {
		name := in.Functions[i].GetName()
		if a, ok := e.addrs[name]; ok {
			fnAddrs[name] = a
		}
	}

	req, err := render.BuildCompositeRequest(render.CompositionInputs{
		CompositeResource:   in.CompositeResource,
		Composition:         in.Composition,
		FunctionAddrs:       fnAddrs,
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
	//
	// Anything else with rsp == nil (including a hypothetical (nil, nil) from
	// a misbehaving Engine implementation) is a hard failure — we can't parse
	// what we don't have.
	if rsp == nil {
		if renderErr == nil {
			return render.CompositionOutputs{}, errors.New("render engine returned nil response with no error")
		}

		outErr := errors.Wrap(renderErr, "cannot render")

		// Augment, never replace: the hint rides along with the original
		// failure (and everything it wraps) rather than standing in for it.
		if hint := e.staleRenderBackendHint(renderErr); hint != "" {
			outErr = errors.Errorf("%w; %s", outErr, hint)
		}

		return render.CompositionOutputs{}, outErr
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

// errStaleRenderBackend is the kong usage error a crossplane CLI predating
// v2.3.x emits when asked to run `crossplane internal render`: the `internal`
// command did not exist, so it is rejected as a stray positional argument.
//
// Matching this phrase is deliberately narrow. It can only originate from a CLI
// that does not know the `internal` verb, so it cannot be produced by a
// composition, pipeline, or connectivity failure against a current backend — the
// failures a misattributed hint would send a user chasing the wrong thing for.
const errStaleRenderBackend = "unexpected argument internal"

// staleRenderBackendHint returns remediation text when err carries the signature
// of a render backend older than `crossplane internal render`, or "" when it does
// not.
//
// This shape is otherwise a dead end for the user: a bare kong usage error naming
// an argument they never passed, with nothing connecting it to a render image
// that has quietly gone stale. Upstream pulls the render image only when it is
// absent, so a locally cached :stable tag is frozen at whatever was first pulled
// — for however long that machine lives.
func (e *EngineRenderFn) staleRenderBackendHint(err error) string {
	if err == nil || !strings.Contains(err.Error(), errStaleRenderBackend) {
		return ""
	}

	if e.image == "" {
		return fmt.Sprintf("the crossplane render binary does not support `crossplane internal render`, so it predates %s, the minimum this tool supports; point --crossplane-render-binary at a newer binary",
			MinCrossplaneRenderVersion)
	}

	return fmt.Sprintf("the cached render image %q does not support `crossplane internal render`, so it predates %s, the minimum this tool supports; re-pull it with `docker pull %s`",
		e.image, MinCrossplaneRenderVersion, e.image)
}

// Cleanup stops every function runtime started across the engine's lifetime
// and runs the cleanups accumulated from each engine.Setup call in LIFO
// order. The effect of those cleanups is engine-specific — for the docker
// engine the one real cleanup releases the docker network it created;
// other engines (local, or docker pre-configured with an externally-managed
// network) accumulate only no-op cleanups. Idempotent and safe to call when
// Render was never invoked.
func (e *EngineRenderFn) Cleanup(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, fa := range e.addrsList {
		e.stopRuntimes(e.log, fa)
	}

	e.addrsList = nil
	e.addrs = nil
	e.startedNames = nil

	// Run cleanups in LIFO order. Per the upstream Engine.Setup contract,
	// at most one cleanup is real (the one that created the environment,
	// if any); the rest are no-ops. LIFO matches the natural deferred-
	// cleanup shape and is correct even if a future engine wants ordering.
	for _, v := range slices.Backward(e.cleanups) {
		v()
	}

	e.cleanups = nil

	return nil
}
