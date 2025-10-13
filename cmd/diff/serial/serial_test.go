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

package serial

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"

	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
	"github.com/crossplane/crossplane/v2/cmd/crank/render"
)

func TestRenderFunc_Passthrough(t *testing.T) {
	type ctxKey string

	key := ctxKey("test")
	ctx := context.WithValue(t.Context(), key, "test-value")
	inputs := render.Inputs{Functions: []pkgv1.Function{{}, {}}}

	var mu sync.Mutex

	mockFunc := func(ctx context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
		// Verify context is passed through
		if ctx.Value(key) != "test-value" {
			t.Error("context not passed through")
		}
		// Verify inputs are passed through
		if len(in.Functions) != 2 {
			t.Errorf("expected 2 functions, got %d", len(in.Functions))
		}

		return render.Outputs{ComposedResources: []composed.Unstructured{*composed.New(), *composed.New()}}, nil
	}

	serialized := RenderFunc(mockFunc, &mu)

	outputs, err := serialized(ctx, logging.NewNopLogger(), inputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Verify outputs are returned
	if len(outputs.ComposedResources) != 2 {
		t.Errorf("expected 2 composed resources, got %d", len(outputs.ComposedResources))
	}
}

func TestRenderFunc_Error(t *testing.T) {
	var mu sync.Mutex

	expectedErr := errors.New("render failed")

	mockFunc := func(_ context.Context, _ logging.Logger, _ render.Inputs) (render.Outputs, error) {
		return render.Outputs{}, expectedErr
	}

	serialized := RenderFunc(mockFunc, &mu)
	_, err := serialized(t.Context(), logging.NewNopLogger(), render.Inputs{})

	if !errors.Is(err, expectedErr) {
		t.Errorf("expected error %v, got %v", expectedErr, err)
	}
}

func TestRenderFunc_Serialization(t *testing.T) {
	var (
		mu              sync.Mutex
		concurrentCount atomic.Int32
		maxConcurrent   atomic.Int32
	)

	mockFunc := func(_ context.Context, _ logging.Logger, _ render.Inputs) (render.Outputs, error) {
		current := concurrentCount.Add(1)

		// Update maxConcurrent if needed
		for {
			maxVal := maxConcurrent.Load()
			if current <= maxVal || maxConcurrent.CompareAndSwap(maxVal, current) {
				break
			}
		}

		time.Sleep(10 * time.Millisecond)
		concurrentCount.Add(-1)

		return render.Outputs{}, nil
	}

	serialized := RenderFunc(mockFunc, &mu)

	// Run multiple renders concurrently
	const numCalls = 10

	var wg sync.WaitGroup
	wg.Add(numCalls)

	for range numCalls {
		go func() {
			defer wg.Done()

			if _, err := serialized(t.Context(), logging.NewNopLogger(), render.Inputs{}); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}

	wg.Wait()

	// Verify that only one render ran at a time
	if maxVal := maxConcurrent.Load(); maxVal != 1 {
		t.Errorf("expected max concurrent executions to be 1, got %d", maxVal)
	}
}
