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
	"maps"
	"sync"

	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	kunstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
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
// render engine and starts function runtimes on first use, reuses them across
// subsequent calls, and serializes concurrent renders with an internal mutex.
//
// Multi-composition note. A single `xr` invocation can render against XRs from
// multiple compositions whose function pipelines overlap but aren't identical.
// To handle this correctly without leaking docker networks (upstream's
// dockerRenderEngine.Setup creates a fresh network on every call as of cli
// v2.3.2), we:
//   - call engine.Setup exactly once on the first render — that creates the
//     docker network N and stamps each first-batch function with its name via
//     the runtime-docker-network annotation;
//   - capture N from any first-batch function's annotation into networkName;
//   - on later renders, manually apply the same annotation to any function we
//     haven't already started (preserving any value the caller pre-set);
//   - track started functions by name in addrs, and accumulate every
//     *FunctionAddresses returned by startRuntimes in fnAddrsList so Cleanup
//     can stop them all.
//
// This is a self-contained workaround. The cleaner shape — Engine.Setup either
// idempotent or paired with an Engine.AnnotateFunctions method — needs an
// upstream API change in crossplane/cli (tracked in crossplane/cli#96).
// Unwind once a cli release ships that fix; tracked downstream in
// crossplane-contrib/crossplane-diff#338.
type EngineRenderFn struct {
	engine         render.Engine
	networkCleanup func()
	networkName    string
	fnAddrsList    []*render.FunctionAddresses
	// startedNames is the set of function names we've already passed to
	// startRuntimes; used for dedup so we don't restart a function that's
	// already running.
	startedNames map[string]struct{}
	// addrs is the merged Addresses() map from every startRuntimes call.
	// Filtered to in.Functions when building each render request.
	addrs   map[string]string
	started bool
	mu      sync.Mutex
	log     logging.Logger

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
// serialized internally. Setup runs on the first invocation (creating the
// docker network); subsequent invocations annotate any newly-encountered
// functions with the captured network name and start their runtimes.
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

	if !e.started {
		// First call: let upstream Setup create the docker network and
		// stamp the first batch with the network annotation. We then read
		// the network name off the annotated functions for use on later
		// calls.
		cleanup, err := e.engine.Setup(ctx, newFns)
		if err != nil {
			return render.CompositionOutputs{}, errors.Wrap(err, "cannot setup render engine")
		}

		e.networkCleanup = cleanup
		e.networkName = firstNetworkAnnotation(newFns)
		e.started = true
	} else if e.networkName != "" {
		// Subsequent call: upstream's Setup is single-shot in cli v2.3.2
		// (calling it again would create a new network and leak the first
		// one), so we apply the same annotation to new functions ourselves.
		// Preserve any value the caller pre-set.
		applyNetworkAnnotation(newFns, e.networkName)
	}

	if len(newFns) > 0 {
		fa, err := e.startRuntimes(ctx, log, newFns)
		if err != nil {
			// Unwind the setup cleanup on the first-call failure path so
			// we don't leak a network when no functions are running.
			if e.networkCleanup != nil && len(e.fnAddrsList) == 0 {
				e.networkCleanup()
				e.networkCleanup = nil
				e.started = false
			}

			return render.CompositionOutputs{}, errors.Wrap(err, "cannot start function runtimes")
		}

		e.fnAddrsList = append(e.fnAddrsList, fa)
		for i := range newFns {
			e.startedNames[newFns[i].GetName()] = struct{}{}
		}

		maps.Copy(e.addrs, fa.Addresses())
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
		ObservedResources:   alignObservedOwnerRefs(in.CompositeResource, in.ObservedResources),
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

// firstNetworkAnnotation returns the value of the runtime-docker-network
// annotation of the first function in fns that has it, or "" if none do.
// Upstream's dockerRenderEngine.Setup stamps every function in its input slice
// with the same value, so picking the first non-empty one is sufficient. The
// local engine path leaves no annotation, so this returns "" for that case.
func firstNetworkAnnotation(fns []pkgv1.Function) string {
	for i := range fns {
		if v := fns[i].GetAnnotations()[render.AnnotationKeyRuntimeDockerNetwork]; v != "" {
			return v
		}
	}

	return ""
}

// applyNetworkAnnotation sets the runtime-docker-network annotation on each
// function that doesn't already have a non-empty value for it. Mirrors the
// upstream injectNetworkAnnotation helper (which is unexported) so we can do
// the same job for functions discovered after the first Setup call.
func applyNetworkAnnotation(fns []pkgv1.Function, networkName string) {
	for i := range fns {
		if fns[i].Annotations == nil {
			fns[i].Annotations = make(map[string]string)
		}

		if fns[i].Annotations[render.AnnotationKeyRuntimeDockerNetwork] == "" {
			fns[i].Annotations[render.AnnotationKeyRuntimeDockerNetwork] = networkName
		}
	}
}

// fakeXRUID mirrors the deterministic UID the binary assigns to the XR after
// deserializing it. The binary at internal/render/composite/render.go (line 94
// in v2.3.2) overwrites xr.UID with
//
//	uuid.NewSHA1(uuid.Nil, gvk + "\x00" + namespace + "\x00" + name)
//
// regardless of what UID we serialize on the wire. The composite reconciler's
// ExistingComposedResourceObserver (composition_functions.go:824 in v2.3.2)
// then drops any observed composed resource whose controller owner ref UID
// doesn't match xr.UID. So observed resources fetched from the cluster — which
// carry the real cluster XR UID on their owner refs — are silently filtered
// out, and templates that look them up by composition-resource-name resolve
// to <no value>, breaking dry-run apply.
//
// The formula is deterministic and a stable part of the binary's contract, so
// replicating it here is fine: if upstream ever changes how the UID is
// derived, TestDiffCompositionWithGetComposedResource (which exercised the
// regression) will fail and we'll update the formula in lockstep.
func fakeXRUID(xr *ucomposite.Unstructured) types.UID {
	gvk := xr.GroupVersionKind()
	return types.UID(uuid.NewSHA1(uuid.Nil, []byte(gvk.String()+"\x00"+xr.GetNamespace()+"\x00"+xr.GetName())).String())
}

// alignObservedOwnerRefs returns a slice of observed composed resources in
// which any owner ref pointing to xr (matched by APIVersion+Kind+Name) has
// its UID replaced with the binary's deterministic fake UID — see fakeXRUID
// for why this is necessary. Inputs are deep-copied; callers' originals are
// not mutated.
func alignObservedOwnerRefs(xr *ucomposite.Unstructured, observed []composed.Unstructured) []composed.Unstructured {
	if len(observed) == 0 {
		return observed
	}

	fakeUID := fakeXRUID(xr)
	apiVersion := xr.GetAPIVersion()
	kind := xr.GetKind()
	name := xr.GetName()

	out := make([]composed.Unstructured, len(observed))

	for i := range observed {
		out[i] = *observed[i].DeepCopy()

		refs := out[i].GetOwnerReferences()
		changed := false

		for j := range refs {
			if refs[j].APIVersion != apiVersion || refs[j].Kind != kind || refs[j].Name != name {
				continue
			}

			if refs[j].UID != fakeUID {
				refs[j].UID = fakeUID
				changed = true
			}
		}

		if changed {
			out[i].SetOwnerReferences(refs)
		}
	}

	return out
}

// Cleanup stops every function runtime started across the engine's lifetime
// and releases the docker network. Idempotent and safe to call when Render
// was never invoked.
func (e *EngineRenderFn) Cleanup(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, fa := range e.fnAddrsList {
		e.stopRuntimes(e.log, fa)
	}

	e.fnAddrsList = nil
	e.addrs = nil
	e.startedNames = nil
	e.networkName = ""

	if e.networkCleanup != nil {
		e.networkCleanup()
		e.networkCleanup = nil
	}

	e.started = false

	return nil
}
