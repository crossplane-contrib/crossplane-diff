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
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/dockerrun"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	renderv1alpha1 "github.com/crossplane/cli/v2/proto/render/v1alpha1"
	gcmp "github.com/google/go-cmp/cmp"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	ucomposite "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
)

// newTestRenderFn builds an engineRenderFn wired with the supplied mock engine
// and a stub startRuntimes that returns a bare *render.FunctionAddresses. The
// stopRuntimes counter ticks once per invocation so tests can assert cleanup.
func newTestRenderFn(mock *render.MockEngine, startCalls, stopCalls *int32) *EngineRenderFn {
	return &EngineRenderFn{
		engine: mock,
		log:    logging.NewNopLogger(),
		startRuntimes: func(_ context.Context, _ logging.Logger, _ []pkgv1.Function) (*render.FunctionAddresses, error) {
			atomic.AddInt32(startCalls, 1)
			// Empty FunctionAddresses — Addresses() returns nil, which is fine for BuildCompositeRequest.
			return &render.FunctionAddresses{}, nil
		},
		stopRuntimes: func(_ context.Context, _ *render.FunctionAddresses) error {
			atomic.AddInt32(stopCalls, 1)
			return nil
		},
	}
}

// The three tests below (HappyPath, CleanupIdempotent, Serialization) each
// exercise a distinct lifecycle property of EngineRenderFn — they're not
// data-variant cases of a single operation. HappyPath asserts setup-once /
// reuse semantics across two sequential renders. CleanupIdempotent asserts
// teardown counts after 0/1/2 cleanup calls. Serialization spawns concurrent
// goroutines and asserts the internal mutex never lets two renders enter the
// engine at the same time. Forcing these into a table would require per-row
// setup hooks, per-row assertion sets, and per-row concurrency primitives —
// the rows would share almost nothing. Procedural tests read more clearly
// here.

// minimalRenderInputs returns RenderInputs with just enough populated that
// BuildCompositeRequest will not error during marshaling. Includes a single
// function so EngineRenderFn's "got a new function, call startRuntimes" path
// is exercised by tests that don't override Functions themselves.
func minimalRenderInputs() RenderInputs {
	xr := ucomposite.New()
	xr.SetAPIVersion("example.org/v1")
	xr.SetKind("XExample")
	xr.SetName("test-xr")

	return RenderInputs{
		CompositeResource: xr,
		Composition: &apiextensionsv1.Composition{
			Spec: apiextensionsv1.CompositionSpec{
				Mode: apiextensionsv1.CompositionModePipeline,
			},
		},
		Functions: []pkgv1.Function{
			{ObjectMeta: metav1.ObjectMeta{Name: "fn-default"}},
		},
	}
}

func TestEngineRenderFn_HappyPath(t *testing.T) {
	ctx := t.Context()

	var renderCalls atomic.Int32

	mock := &render.MockEngine{
		MockRender: func(_ context.Context, req *renderv1alpha1.RenderRequest) (*renderv1alpha1.RenderResponse, error) {
			renderCalls.Add(1)
			// Echo the composite resource back so ParseCompositeResponse succeeds.
			if c := req.GetComposite(); c == nil {
				t.Fatalf("expected composite input on request")
			}

			return &renderv1alpha1.RenderResponse{
				Output: &renderv1alpha1.RenderResponse_Composite{
					Composite: &renderv1alpha1.CompositeOutput{
						CompositeResource: req.GetComposite().GetCompositeResource(),
					},
				},
			}, nil
		},
	}

	var startCalls, stopCalls int32

	e := newTestRenderFn(mock, &startCalls, &stopCalls)

	// First render: Setup + StartFunctionRuntimes should each run once.
	out, err := e.Render(ctx, logging.NewNopLogger(), minimalRenderInputs())
	if err != nil {
		t.Fatalf("first Render: unexpected error: %v", err)
	}

	if out.CompositeResource == nil {
		t.Fatalf("first Render: expected CompositeResource in output")
	}

	if got := atomic.LoadInt32(&startCalls); got != 1 {
		t.Fatalf("first Render: startRuntimes calls = %d, want 1", got)
	}

	if got := renderCalls.Load(); got != 1 {
		t.Fatalf("first Render: engine.Render calls = %d, want 1", got)
	}

	// Second render: runtimes should be reused, no new start call.
	if _, err := e.Render(ctx, logging.NewNopLogger(), minimalRenderInputs()); err != nil {
		t.Fatalf("second Render: unexpected error: %v", err)
	}

	if got := atomic.LoadInt32(&startCalls); got != 1 {
		t.Fatalf("second Render: startRuntimes calls = %d, want still 1 (reused)", got)
	}

	if got := renderCalls.Load(); got != 2 {
		t.Fatalf("second Render: engine.Render calls = %d, want 2", got)
	}
}

func TestEngineRenderFn_CleanupIdempotent(t *testing.T) {
	ctx := t.Context()

	var setupCalls, setupCleanupCalls int32

	mock := &render.MockEngine{
		MockSetup: func(_ context.Context, _ []pkgv1.Function) (func(), error) {
			atomic.AddInt32(&setupCalls, 1)
			return func() { atomic.AddInt32(&setupCleanupCalls, 1) }, nil
		},
	}

	var startCalls, stopCalls int32

	e := newTestRenderFn(mock, &startCalls, &stopCalls)

	// Cleanup before any render: no-op.
	if err := e.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup before Render: unexpected error: %v", err)
	}

	if got := atomic.LoadInt32(&stopCalls); got != 0 {
		t.Fatalf("Cleanup before Render: stopRuntimes = %d, want 0", got)
	}

	if got := atomic.LoadInt32(&setupCleanupCalls); got != 0 {
		t.Fatalf("Cleanup before Render: setupCleanup = %d, want 0", got)
	}

	// Render once to establish state.
	if _, err := e.Render(ctx, logging.NewNopLogger(), minimalRenderInputs()); err != nil {
		t.Fatalf("Render: unexpected error: %v", err)
	}

	if got := atomic.LoadInt32(&setupCalls); got != 1 {
		t.Fatalf("Render: MockSetup = %d, want 1", got)
	}

	// First real cleanup: runs Stop + setup-cleanup exactly once.
	if err := e.Cleanup(ctx); err != nil {
		t.Fatalf("first Cleanup: unexpected error: %v", err)
	}

	if got := atomic.LoadInt32(&stopCalls); got != 1 {
		t.Fatalf("first Cleanup: stopRuntimes = %d, want 1", got)
	}

	if got := atomic.LoadInt32(&setupCleanupCalls); got != 1 {
		t.Fatalf("first Cleanup: setupCleanup = %d, want 1", got)
	}

	// Second cleanup: idempotent (no extra Stop or setup-cleanup).
	if err := e.Cleanup(ctx); err != nil {
		t.Fatalf("second Cleanup: unexpected error: %v", err)
	}

	if got := atomic.LoadInt32(&stopCalls); got != 1 {
		t.Fatalf("second Cleanup: stopRuntimes = %d, want still 1", got)
	}

	if got := atomic.LoadInt32(&setupCleanupCalls); got != 1 {
		t.Fatalf("second Cleanup: setupCleanup = %d, want still 1", got)
	}
}

func TestEngineRenderFn_Serialization(t *testing.T) {
	ctx := t.Context()

	// inFlight tracks concurrent entries to the engine's Render; must never exceed 1.
	var (
		inFlight      atomic.Int32
		maxInFlight   atomic.Int32
		renderEntered = make(chan struct{})
		allowReturn   = make(chan struct{})
	)

	mock := &render.MockEngine{
		MockRender: func(_ context.Context, req *renderv1alpha1.RenderRequest) (*renderv1alpha1.RenderResponse, error) {
			n := inFlight.Add(1)

			for {
				m := maxInFlight.Load()
				if n <= m || maxInFlight.CompareAndSwap(m, n) {
					break
				}
			}
			// Signal arrival on the first goroutine only so we can release it deterministically.
			select {
			case renderEntered <- struct{}{}:
			default:
			}

			<-allowReturn
			inFlight.Add(-1)

			return &renderv1alpha1.RenderResponse{
				Output: &renderv1alpha1.RenderResponse_Composite{
					Composite: &renderv1alpha1.CompositeOutput{
						CompositeResource: req.GetComposite().GetCompositeResource(),
					},
				},
			}, nil
		},
	}

	var startCalls, stopCalls int32

	e := newTestRenderFn(mock, &startCalls, &stopCalls)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()

		if _, err := e.Render(ctx, logging.NewNopLogger(), minimalRenderInputs()); err != nil {
			t.Errorf("goroutine A Render: %v", err)
		}
	}()
	go func() {
		defer wg.Done()

		if _, err := e.Render(ctx, logging.NewNopLogger(), minimalRenderInputs()); err != nil {
			t.Errorf("goroutine B Render: %v", err)
		}
	}()

	// Wait for one goroutine to have entered engine.Render, then release both in order.
	<-renderEntered
	// At this moment, at most one render is inside engine.Render.
	if got := inFlight.Load(); got != 1 {
		t.Fatalf("inFlight on first entry = %d, want 1", got)
	}

	close(allowReturn)
	wg.Wait()

	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("maxInFlight across two concurrent Render calls = %d, want 1", got)
	}
}

// TestEngineRenderFn_MultiCompositionFunctionSet asserts that EngineRenderFn
// correctly handles renders whose RenderInputs.Functions slice differs across
// calls — the case where one `xr` invocation processes XRs that resolve to
// different compositions with overlapping but non-identical function pipelines.
//
// Required behaviour:
//   - Setup runs once with the first batch of functions (creates network N).
//   - The network name is captured from the annotation upstream's Setup
//     stamps onto the first batch.
//   - On each subsequent render, any functions NOT already started get the
//     same network annotation applied directly, then are passed to
//     startRuntimes — joining N as expected.
//   - Already-running functions are skipped (their addresses cached).
//   - All started FunctionAddresses are stopped on Cleanup.
func TestEngineRenderFn_MultiCompositionFunctionSet(t *testing.T) {
	ctx := t.Context()

	const networkName = "test-network-name"

	mock := &render.MockEngine{
		MockSetup: func(_ context.Context, fns []pkgv1.Function) (func(), error) {
			// Mimic dockerRenderEngine.Setup's network-annotation behaviour
			// so EngineRenderFn can capture the network name back off the
			// supplied functions.
			for i := range fns {
				if fns[i].Annotations == nil {
					fns[i].Annotations = map[string]string{}
				}

				fns[i].Annotations[render.AnnotationKeyRuntimeDockerNetwork] = networkName
			}

			return func() {}, nil
		},
		MockRender: func(_ context.Context, req *renderv1alpha1.RenderRequest) (*renderv1alpha1.RenderResponse, error) {
			return &renderv1alpha1.RenderResponse{
				Output: &renderv1alpha1.RenderResponse_Composite{
					Composite: &renderv1alpha1.CompositeOutput{
						CompositeResource: req.GetComposite().GetCompositeResource(),
					},
				},
			}, nil
		},
	}

	// startedNames records which function names startRuntimes was called with,
	// in the order it was called. Each StartFunctionRuntimes call is a single
	// element listing names from that call. Used to assert dedup + new-fn-only
	// semantics across renders.
	type startCall struct {
		names    []string
		networks []string
	}

	var (
		startCallsLog []startCall
		startCallsMu  sync.Mutex
	)

	e := &EngineRenderFn{
		engine: mock,
		log:    logging.NewNopLogger(),
		startRuntimes: func(_ context.Context, _ logging.Logger, fns []pkgv1.Function) (*render.FunctionAddresses, error) {
			startCallsMu.Lock()
			defer startCallsMu.Unlock()

			call := startCall{}
			for _, fn := range fns {
				call.names = append(call.names, fn.GetName())
				call.networks = append(call.networks, fn.GetAnnotations()[render.AnnotationKeyRuntimeDockerNetwork])
			}

			startCallsLog = append(startCallsLog, call)

			return &render.FunctionAddresses{}, nil
		},
		stopRuntimes: func(_ context.Context, _ *render.FunctionAddresses) error { return nil },
	}

	mkFn := func(name string) pkgv1.Function {
		return pkgv1.Function{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		}
	}

	// First render — composition A's functions [F1, F2].
	in1 := minimalRenderInputs()

	in1.Functions = []pkgv1.Function{mkFn("F1"), mkFn("F2")}
	if _, err := e.Render(ctx, logging.NewNopLogger(), in1); err != nil {
		t.Fatalf("first Render: %v", err)
	}

	// Second render — composition B's functions [F1, F3]. F3 is new, F1 is
	// shared with composition A and must NOT be re-started.
	in2 := minimalRenderInputs()

	in2.Functions = []pkgv1.Function{mkFn("F1"), mkFn("F3")}
	if _, err := e.Render(ctx, logging.NewNopLogger(), in2); err != nil {
		t.Fatalf("second Render: %v", err)
	}

	// Third render — composition A again. All functions already running.
	in3 := minimalRenderInputs()

	in3.Functions = []pkgv1.Function{mkFn("F1"), mkFn("F2")}
	if _, err := e.Render(ctx, logging.NewNopLogger(), in3); err != nil {
		t.Fatalf("third Render: %v", err)
	}

	// Expectations:
	//   - First render starts F1 + F2.
	//   - Second render starts only F3 (F1 already running).
	//   - Third render starts nothing (F1 + F2 already running).
	//   - Every started function has the captured network annotation.
	want := []startCall{
		{names: []string{"F1", "F2"}, networks: []string{networkName, networkName}},
		{names: []string{"F3"}, networks: []string{networkName}},
	}

	startCallsMu.Lock()
	defer startCallsMu.Unlock()

	if got := len(startCallsLog); got != len(want) {
		t.Fatalf("startRuntimes calls = %d, want %d (calls=%v)", got, len(want), startCallsLog)
	}

	for i, w := range want {
		if got := startCallsLog[i]; !equalStartCall(got, w) {
			t.Errorf("startRuntimes call #%d = %v, want %v", i+1, got, w)
		}
	}
}

// TestEngineRenderFn_PreservesExistingNetworkAnnotation asserts that an
// existing annotation is preserved: when a function arrives with a non-empty
// runtime-docker-network annotation already set (e.g. via
// CROSSPLANE_DIFF_DOCKER_NETWORK / a future containerized-job env var path),
// EngineRenderFn must NOT overwrite that value with the captured engine
// network on the subsequent-render annotate path.
func TestEngineRenderFn_PreservesExistingNetworkAnnotation(t *testing.T) {
	ctx := t.Context()

	const (
		engineNetwork = "engine-network"
		userNetwork   = "user-supplied-network"
	)

	mock := &render.MockEngine{
		MockSetup: func(_ context.Context, fns []pkgv1.Function) (func(), error) {
			for i := range fns {
				if fns[i].Annotations == nil {
					fns[i].Annotations = map[string]string{}
				}

				if fns[i].Annotations[render.AnnotationKeyRuntimeDockerNetwork] == "" {
					fns[i].Annotations[render.AnnotationKeyRuntimeDockerNetwork] = engineNetwork
				}
			}

			return func() {}, nil
		},
		MockRender: func(_ context.Context, req *renderv1alpha1.RenderRequest) (*renderv1alpha1.RenderResponse, error) {
			return &renderv1alpha1.RenderResponse{
				Output: &renderv1alpha1.RenderResponse_Composite{
					Composite: &renderv1alpha1.CompositeOutput{
						CompositeResource: req.GetComposite().GetCompositeResource(),
					},
				},
			}, nil
		},
	}

	var (
		seen   []string
		seenMu sync.Mutex
	)

	e := &EngineRenderFn{
		engine: mock,
		log:    logging.NewNopLogger(),
		startRuntimes: func(_ context.Context, _ logging.Logger, fns []pkgv1.Function) (*render.FunctionAddresses, error) {
			seenMu.Lock()
			defer seenMu.Unlock()

			for i := range fns {
				seen = append(seen, fns[i].GetName()+"="+fns[i].GetAnnotations()[render.AnnotationKeyRuntimeDockerNetwork])
			}

			return &render.FunctionAddresses{}, nil
		},
		stopRuntimes: func(_ context.Context, _ *render.FunctionAddresses) error { return nil },
	}

	// First render — composition A's [F1]. Setup stamps engineNetwork on F1.
	in1 := minimalRenderInputs()

	in1.Functions = []pkgv1.Function{
		{ObjectMeta: metav1.ObjectMeta{Name: "F1"}},
	}
	if _, err := e.Render(ctx, logging.NewNopLogger(), in1); err != nil {
		t.Fatalf("first Render: %v", err)
	}

	// Second render — composition B introduces F2 with a pre-set user network.
	// EngineRenderFn's annotate-on-subsequent-render path must NOT overwrite it.
	in2 := minimalRenderInputs()

	in2.Functions = []pkgv1.Function{
		{ObjectMeta: metav1.ObjectMeta{Name: "F2", Annotations: map[string]string{
			render.AnnotationKeyRuntimeDockerNetwork: userNetwork,
		}}},
	}
	if _, err := e.Render(ctx, logging.NewNopLogger(), in2); err != nil {
		t.Fatalf("second Render: %v", err)
	}

	want := []string{
		"F1=" + engineNetwork,
		"F2=" + userNetwork,
	}

	seenMu.Lock()
	defer seenMu.Unlock()

	if len(seen) != len(want) {
		t.Fatalf("startRuntimes saw %d invocations of fn=net pairs, want %d (%v)", len(seen), len(want), seen)
	}

	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("startRuntimes seen[%d] = %q, want %q", i, seen[i], want[i])
		}
	}
}

// TestEngineRenderFn_CleanupStopsAllFunctionAddresses asserts that every
// *FunctionAddresses ever returned by startRuntimes is passed to stopRuntimes
// during Cleanup, not just the most recent one.
func TestEngineRenderFn_CleanupStopsAllFunctionAddresses(t *testing.T) {
	ctx := t.Context()

	mock := &render.MockEngine{
		MockSetup: func(_ context.Context, fns []pkgv1.Function) (func(), error) {
			for i := range fns {
				if fns[i].Annotations == nil {
					fns[i].Annotations = map[string]string{}
				}

				fns[i].Annotations[render.AnnotationKeyRuntimeDockerNetwork] = "test-network"
			}

			return func() {}, nil
		},
		MockRender: func(_ context.Context, req *renderv1alpha1.RenderRequest) (*renderv1alpha1.RenderResponse, error) {
			return &renderv1alpha1.RenderResponse{
				Output: &renderv1alpha1.RenderResponse_Composite{
					Composite: &renderv1alpha1.CompositeOutput{
						CompositeResource: req.GetComposite().GetCompositeResource(),
					},
				},
			}, nil
		},
	}

	var stopCalls atomic.Int32

	e := &EngineRenderFn{
		engine: mock,
		log:    logging.NewNopLogger(),
		startRuntimes: func(_ context.Context, _ logging.Logger, _ []pkgv1.Function) (*render.FunctionAddresses, error) {
			return &render.FunctionAddresses{}, nil
		},
		stopRuntimes: func(_ context.Context, _ *render.FunctionAddresses) error {
			stopCalls.Add(1)
			return nil
		},
	}

	// Two renders that each introduce a brand-new function → two
	// FunctionAddresses entries in fnAddrsList.
	in1 := minimalRenderInputs()

	in1.Functions = []pkgv1.Function{{ObjectMeta: metav1.ObjectMeta{Name: "F1"}}}
	if _, err := e.Render(ctx, logging.NewNopLogger(), in1); err != nil {
		t.Fatalf("first Render: %v", err)
	}

	in2 := minimalRenderInputs()

	in2.Functions = []pkgv1.Function{{ObjectMeta: metav1.ObjectMeta{Name: "F2"}}}
	if _, err := e.Render(ctx, logging.NewNopLogger(), in2); err != nil {
		t.Fatalf("second Render: %v", err)
	}

	if err := e.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if got := stopCalls.Load(); got != 2 {
		t.Errorf("stopRuntimes called %d times, want 2 (one per FunctionAddresses)", got)
	}
}

// equalStartCall ignores ordering — startRuntimes can receive functions in any
// order as long as the set + network annotations match.
func equalStartCall(a, b struct {
	names    []string
	networks []string
},
) bool {
	if len(a.names) != len(b.names) {
		return false
	}

	bn := map[string]string{}
	for i, n := range b.names {
		bn[n] = b.networks[i]
	}

	for i, n := range a.names {
		want, ok := bn[n]
		if !ok || want != a.networks[i] {
			return false
		}
	}

	return true
}

// TestNewEngineRenderFn_ResolvedRenderImage asserts that the render-backend
// selector arguments reach the engine, and reach it in a form that is actually
// pullable. Before RenderImage() existed there was nothing to assert against:
// the selected image disappeared into upstream's unexported
// crossplaneImageFromFlags, so a caller that dropped the pin entirely looked
// identical to one that honoured it (crossplane-diff#488).
func TestNewEngineRenderFn_ResolvedRenderImage(t *testing.T) {
	stable := render.DefaultCrossplaneImage + ":stable"

	tests := map[string]struct {
		binaryPath string
		version    string
		image      string
		want       string
	}{
		"NoSelectorResolvesStable": {
			want: stable,
		},
		"VersionPinResolvesVersionedTag": {
			version: "v2.4.0",
			want:    render.DefaultCrossplaneImage + ":v2.4.0",
		},
		// Upstream formats the version into the tag verbatim and publishes only
		// v-prefixed tags, so the bare form ValidateMinRenderVersion accepts has
		// to be normalized before it gets here or it becomes an unpullable
		// reference.
		"BareVersionPinIsNormalized": {
			version: "2.3.4",
			want:    render.DefaultCrossplaneImage + ":v2.3.4",
		},
		"NonSemverVersionPinPassesThrough": {
			version: "stable",
			want:    stable,
		},
		"ImageOverrideResolvesVerbatim": {
			image: "example.com/mirror/crossplane:v2.4.0",
			want:  "example.com/mirror/crossplane:v2.4.0",
		},
		// The local engine pulls nothing.
		"BinaryPathResolvesNoImage": {
			binaryPath: "/usr/local/bin/crossplane",
			want:       "",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e := NewEngineRenderFn(logging.NewNopLogger(), tt.binaryPath, tt.version, tt.image)

			if diff := gcmp.Diff(tt.want, e.RenderImage()); diff != "" {
				t.Errorf("RenderImage() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNewEngineRenderFn_UncomparableImageWarns asserts that an image reference
// whose version cannot be compared against MinCrossplaneRenderVersion draws an
// advisory rather than silence. Refusing such a reference would break the
// private-mirror and digest-pinning cases --crossplane-image exists to serve, so
// a warning is the only thing left that keeps the floor visible.
func TestNewEngineRenderFn_UncomparableImageWarns(t *testing.T) {
	tests := map[string]struct {
		version     string
		image       string
		wantWarning bool
	}{
		"UncomparableImageWarns": {
			image:       "internal-mirror/crossplane",
			wantWarning: true,
		},
		"ComparableImageStaysQuiet": {
			image:       "internal-mirror/crossplane:v2.4.1",
			wantWarning: false,
		},
		// The engine's own :stable default is not the user handing us an
		// uncomparable reference, and warning on it would fire on every default
		// invocation.
		"NoSelectorStaysQuiet": {
			wantWarning: false,
		},
		"VersionPinStaysQuiet": {
			version:     "v2.4.0",
			wantWarning: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			warnings := NewWarningLogger(logging.NewNopLogger(), io.Discard)

			NewEngineRenderFn(warnings, "", tt.version, tt.image)

			got := warnings.Warnings()

			if !tt.wantWarning {
				if len(got) != 0 {
					t.Fatalf("warnings = %v, want none", got)
				}

				return
			}

			if len(got) != 1 {
				t.Fatalf("warnings = %v, want exactly 1", got)
			}

			if !strings.Contains(got[0].Message, tt.image) {
				t.Errorf("warning = %q, want it to name the image %q", got[0].Message, tt.image)
			}
		})
	}
}

// TestEngineRenderFn_StaleRenderImageHint asserts that the one render failure
// shape that is purely an environment problem — a render backend so old it has
// no `crossplane internal render` subcommand, which is what a never-refreshed
// local `:stable` image degrades into — carries remediation instead of leaving
// the user with a bare kong usage error. The hint must augment the original
// error, never replace or soften it.
func TestEngineRenderFn_StaleRenderImageHint(t *testing.T) {
	// The verbatim shape upstream surfaces: the container's stderr, wrapped.
	staleErr := errors.New("crossplane internal render in Docker returned error with output: crossplane: error: unexpected argument internal")

	tests := map[string]struct {
		image string
		// renderErr is what the engine returns alongside a nil response.
		renderErr error
		// wantHintFor, when non-empty, must appear in the final error, as must
		// MinCrossplaneRenderVersion.
		wantHintFor string
	}{
		"StaleDockerImageGetsHint": {
			image:       "xpkg.crossplane.io/crossplane/crossplane:stable",
			renderErr:   staleErr,
			wantHintFor: "xpkg.crossplane.io/crossplane/crossplane:stable",
		},
		"StaleLocalBinaryGetsHint": {
			image:       "",
			renderErr:   errors.New("crossplane: error: unexpected argument internal"),
			wantHintFor: MinCrossplaneRenderVersion,
		},
		// Narrowness matters more than coverage here: misattributing an ordinary
		// pipeline or connectivity failure to a stale image would send users to
		// fix the wrong thing.
		"UnrelatedRenderFailureGetsNoHint": {
			image:     "xpkg.crossplane.io/crossplane/crossplane:stable",
			renderErr: errors.New("cannot connect to the Docker daemon"),
		},
		"UnrelatedUnexpectedArgumentGetsNoHint": {
			image:     "xpkg.crossplane.io/crossplane/crossplane:stable",
			renderErr: errors.New("crossplane: error: unexpected argument render"),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()

			mock := &render.MockEngine{
				MockRender: func(_ context.Context, _ *renderv1alpha1.RenderRequest) (*renderv1alpha1.RenderResponse, error) {
					return nil, tt.renderErr
				},
			}

			var startCalls, stopCalls int32

			e := newTestRenderFn(mock, &startCalls, &stopCalls)
			e.image = tt.image

			_, err := e.Render(ctx, logging.NewNopLogger(), minimalRenderInputs())
			if err == nil {
				t.Fatalf("Render() = nil error, want the render failure surfaced")
			}

			// Whatever else happens, the original failure must survive intact.
			if !errors.Is(err, tt.renderErr) {
				t.Errorf("Render() error = %v, want it to wrap the engine error", err)
			}

			if !strings.Contains(err.Error(), tt.renderErr.Error()) {
				t.Errorf("Render() error = %q, want it to contain the engine message %q", err.Error(), tt.renderErr.Error())
			}

			if tt.wantHintFor == "" {
				if strings.Contains(err.Error(), MinCrossplaneRenderVersion) {
					t.Errorf("Render() error = %q, want no stale-image hint", err.Error())
				}

				return
			}

			for _, want := range []string{tt.wantHintFor, MinCrossplaneRenderVersion} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Render() error = %q, want it to contain %q", err.Error(), want)
				}
			}
		})
	}
}

// TestEngineRenderFn_RunOwnsFunctionContainers pins how the engine ties what it starts to its run
// (#525), by what a fake Docker host holds afterwards. Upstream labels each function container with the
// session its Function's annotation names, and the engine's Setup creates a network for the run that its
// cleanup removes, which the daemon refuses while a container is still attached. Every function is started
// with the run's session, overriding one it carries, with no container name another run's container could
// already have, and left for the run to remove. The run's lease is held before the engine creates anything,
// so a run killed mid-render is reclaimable. Cleanup leaves nothing of the run behind, and removes its
// containers before the engine's cleanup, which therefore succeeds. The caller's Function objects are not
// modified. A function that fails to start still has its network removed by Cleanup.
func TestEngineRenderFn_RunOwnsFunctionContainers(t *testing.T) {
	const (
		templating = "xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.11.0"
		autoReady  = "xpkg.crossplane.io/crossplane-contrib/function-auto-ready:v0.4.2"
		engineNet  = "crossplane-render-net"
	)

	orphan := string(render.AnnotationValueRuntimeDockerCleanupOrphan)

	type want struct {
		// Started is the annotations of each function the engine handed upstream to start.
		Started map[string]map[string]string
		// Containers and Networks are what the host holds after Cleanup.
		Containers []string
		Networks   []string

		LeaseHeldAtSetup    bool
		EngineCleanupFailed bool
		RenderErr           bool
	}

	tests := map[string]struct {
		reason   string
		startErr error // fails starting every function after the first
		want     want
	}{
		"Rendered": {
			reason: "A rendered run's containers and network are all gone after Cleanup, and the engine's own " +
				"cleanup could remove its network.",
			want: want{
				Containers:       []string{"unrelated"},
				Networks:         []string{"user-net"},
				LeaseHeldAtSetup: true,
			},
		},
		"StartFailed": {
			reason: "A function that fails to start leaves the engine's network for Cleanup, which removes it " +
				"once the containers that did start are gone.",
			startErr: errors.New("cannot pull image"),
			want: want{
				Containers:       []string{"unrelated"},
				Networks:         []string{"user-net"},
				LeaseHeldAtSetup: true,
				RenderErr:        true,
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			leases := t.TempDir()

			docker := &tu.FakeDocker{
				Containers: []container.Summary{tu.DockerContainer("unrelated", container.StateRunning, nil)},
				Networks:   []network.Summary{tu.DockerNetwork("user-net", nil)},
			}
			run := dockerrun.New(leases, func() (dockerrun.Docker, error) { return docker, nil }, logging.NewNopLogger())

			got := want{Started: map[string]map[string]string{}}

			mock := &render.MockEngine{
				MockSetup: func(_ context.Context, _ []pkgv1.Function) (func(), error) {
					_, err := os.Stat(filepath.Join(leases, run.ID()+".lock"))
					got.LeaseHeldAtSetup = err == nil

					docker.Networks = append(docker.Networks, tu.DockerNetwork(engineNet, tu.DockerSession(run.ID())))

					return func() {
						if _, err := docker.NetworkRemove(ctx, engineNet, client.NetworkRemoveOptions{}); err != nil {
							got.EngineCleanupFailed = true
						}
					}, nil
				},
				MockRender: func(_ context.Context, req *renderv1alpha1.RenderRequest) (*renderv1alpha1.RenderResponse, error) {
					return &renderv1alpha1.RenderResponse{
						Output: &renderv1alpha1.RenderResponse_Composite{
							Composite: &renderv1alpha1.CompositeOutput{CompositeResource: req.GetComposite().GetCompositeResource()},
						},
					}, nil
				},
			}

			e := &EngineRenderFn{
				engine: mock,
				log:    logging.NewNopLogger(),
				run:    run,
				// Starts a running container per function, labelled with the session its annotation names and
				// attached to the engine's network, as the docker runtime does.
				startRuntimes: func(_ context.Context, _ logging.Logger, fns []pkgv1.Function) (*render.FunctionAddresses, error) {
					for i, fn := range fns {
						if i > 0 && tt.startErr != nil {
							return nil, tt.startErr
						}

						got.Started[fn.GetName()] = fn.GetAnnotations()
						session := fn.GetAnnotations()[render.AnnotationKeyRuntimeDockerSession]
						docker.Containers = append(docker.Containers,
							tu.DockerContainer(fn.GetName(), container.StateRunning, tu.DockerSession(session), engineNet))
					}

					return &render.FunctionAddresses{}, nil
				},
				// The orphan cleanup policy leaves containers to the run, so stopping them removes nothing.
				stopRuntimes: func(context.Context, *render.FunctionAddresses) error { return nil },
			}

			in := minimalRenderInputs()
			in.Functions = []pkgv1.Function{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "fn-templating"},
					Spec:       pkgv1.FunctionSpec{PackageSpec: pkgv1.PackageSpec{Package: templating}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "fn-auto-ready", Annotations: map[string]string{
						"example.org/keep":                        "me",
						render.AnnotationKeyRuntimeDockerCleanup:  string(render.AnnotationValueRuntimeDockerCleanupRemove),
						render.AnnotationKeyRuntimeDockerSession:  "someone-elses",
						render.AnnotationKeyRuntimeNamedContainer: "shared-auto-ready",
					}},
					Spec: pkgv1.FunctionSpec{PackageSpec: pkgv1.PackageSpec{Package: autoReady}},
				},
			}
			callerAnnotations := maps.Clone(in.Functions[1].GetAnnotations())

			_, err := e.Render(ctx, logging.NewNopLogger(), in)
			got.RenderErr = err != nil

			if cerr := e.Cleanup(ctx); cerr != nil {
				t.Fatalf("\n%s\nCleanup(...): unexpected error: %v", tt.reason, cerr)
			}

			got.Containers = docker.ContainerIDs()
			got.Networks = docker.NetworkNames()

			tt.want.Started = map[string]map[string]string{
				"fn-templating": {
					render.AnnotationKeyRuntimeDockerSession: run.ID(),
					render.AnnotationKeyRuntimeDockerCleanup: orphan,
				},
			}
			if tt.startErr == nil {
				tt.want.Started["fn-auto-ready"] = map[string]string{
					"example.org/keep":                       "me",
					render.AnnotationKeyRuntimeDockerSession: run.ID(),
					render.AnnotationKeyRuntimeDockerCleanup: orphan,
				}
			}

			if diff := gcmp.Diff(tt.want, got); diff != "" {
				t.Errorf("\n%s\nRender(...)/Cleanup(...): -want, +got:\n%s", tt.reason, diff)
			}

			if diff := gcmp.Diff(callerAnnotations, in.Functions[1].GetAnnotations()); diff != "" {
				t.Errorf("\n%s\nRender(...) modified the caller's Function annotations, -want, +got:\n%s", tt.reason, diff)
			}
		})
	}
}

func TestClassifyRenderError(t *testing.T) {
	stale := errors.Wrap(errors.New("crossplane: error: unexpected argument internal"), "cannot render")

	if got := classifyRenderError(stale); !errors.Is(got, ErrStaleRenderBackend) || !errors.Is(got, stale) || got.Error() == "" {
		t.Errorf("classifyRenderError(stale) = %v, want ErrStaleRenderBackend preserving the original", got)
	}

	if got := classifyRenderError(errors.New("boom")); errors.Is(got, ErrStaleRenderBackend) {
		t.Errorf("classifyRenderError(unrelated) wrongly classified: %v", got)
	}

	if got := classifyRenderError(nil); got != nil {
		t.Errorf("classifyRenderError(nil) = %v, want nil", got)
	}
}
