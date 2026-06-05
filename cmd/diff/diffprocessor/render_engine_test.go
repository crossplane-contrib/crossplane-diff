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
	"sync/atomic"
	"testing"

	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	renderv1alpha1 "github.com/crossplane/cli/v2/proto/render/v1alpha1"
	"google.golang.org/protobuf/types/known/structpb"

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
		stopRuntimes: func(_ logging.Logger, _ *render.FunctionAddresses) {
			atomic.AddInt32(stopCalls, 1)
		},
	}
}

// minimalRenderInputs returns RenderInputs with just enough populated that
// BuildCompositeRequest will not error during marshaling.
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
	}
}

func TestEngineRenderFn_HappyPath(t *testing.T) {
	ctx := context.Background()

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
	ctx := context.Background()

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
	ctx := context.Background()

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

// ensure structpb is referenced so the import survives even if future tests
// drop their uses. Kept deliberately trivial.
var _ = structpb.NewNullValue
