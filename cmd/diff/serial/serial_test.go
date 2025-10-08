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

func TestRenderFunc_Serialization(t *testing.T) {
	var mu sync.Mutex
	
	// Track concurrent executions
	var concurrentCount atomic.Int32
	var maxConcurrent atomic.Int32
	
	mockRenderFunc := func(ctx context.Context, log logging.Logger, in render.Inputs) (render.Outputs, error) {
		// Increment concurrent counter
		current := concurrentCount.Add(1)
		
		// Update max if needed
		for {
			max := maxConcurrent.Load()
			if current <= max {
				break
			}
			if maxConcurrent.CompareAndSwap(max, current) {
				break
			}
		}
		
		// Simulate some work
		time.Sleep(10 * time.Millisecond)
		
		// Decrement counter
		concurrentCount.Add(-1)
		
		return render.Outputs{
			ComposedResources: []composed.Unstructured{},
		}, nil
	}
	
	serializedFunc := RenderFunc(mockRenderFunc, &mu)
	
	// Run multiple renders concurrently
	const numCalls = 10
	var wg sync.WaitGroup
	wg.Add(numCalls)
	
	for i := 0; i < numCalls; i++ {
		go func() {
			defer wg.Done()
			_, err := serializedFunc(context.Background(), logging.NewNopLogger(), render.Inputs{})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	
	wg.Wait()
	
	// Verify that only one render ran at a time
	if max := maxConcurrent.Load(); max != 1 {
		t.Errorf("expected max concurrent executions to be 1, got %d", max)
	}
}

func TestRenderFunc_ReturnsResults(t *testing.T) {
	var mu sync.Mutex
	
	expectedOutputs := render.Outputs{
		ComposedResources: []composed.Unstructured{*composed.New(), *composed.New()},
	}
	
	mockRenderFunc := func(ctx context.Context, log logging.Logger, in render.Inputs) (render.Outputs, error) {
		return expectedOutputs, nil
	}
	
	serializedFunc := RenderFunc(mockRenderFunc, &mu)
	
	outputs, err := serializedFunc(context.Background(), logging.NewNopLogger(), render.Inputs{})
	
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	
	if len(outputs.ComposedResources) != len(expectedOutputs.ComposedResources) {
		t.Errorf("expected %d composed resources, got %d", 
			len(expectedOutputs.ComposedResources), 
			len(outputs.ComposedResources))
	}
}

func TestRenderFunc_ReturnsErrors(t *testing.T) {
	var mu sync.Mutex
	
	expectedErr := errors.New("render failed")
	
	mockRenderFunc := func(ctx context.Context, log logging.Logger, in render.Inputs) (render.Outputs, error) {
		return render.Outputs{}, expectedErr
	}
	
	serializedFunc := RenderFunc(mockRenderFunc, &mu)
	
	_, err := serializedFunc(context.Background(), logging.NewNopLogger(), render.Inputs{})
	
	if err != expectedErr {
		t.Errorf("expected error %v, got %v", expectedErr, err)
	}
}

func TestRenderFunc_IncrementsCalls(t *testing.T) {
	var mu sync.Mutex
	
	callCount := 0
	mockRenderFunc := func(ctx context.Context, log logging.Logger, in render.Inputs) (render.Outputs, error) {
		callCount++
		return render.Outputs{}, nil
	}
	
	serializedFunc := RenderFunc(mockRenderFunc, &mu)
	
	// Call multiple times
	const numCalls = 5
	for i := 0; i < numCalls; i++ {
		_, err := serializedFunc(context.Background(), logging.NewNopLogger(), render.Inputs{})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
	
	if callCount != numCalls {
		t.Errorf("expected %d calls, got %d", numCalls, callCount)
	}
}

func TestRenderFunc_PassesContext(t *testing.T) {
	var mu sync.Mutex
	
	type ctxKey string
	key := ctxKey("test")
	expectedValue := "test-value"
	ctx := context.WithValue(context.Background(), key, expectedValue)
	
	mockRenderFunc := func(ctx context.Context, log logging.Logger, in render.Inputs) (render.Outputs, error) {
		value := ctx.Value(key)
		if value != expectedValue {
			t.Errorf("expected context value %v, got %v", expectedValue, value)
		}
		return render.Outputs{}, nil
	}
	
	serializedFunc := RenderFunc(mockRenderFunc, &mu)
	
	_, err := serializedFunc(ctx, logging.NewNopLogger(), render.Inputs{})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRenderFunc_PassesInputs(t *testing.T) {
	var mu sync.Mutex
	
	expectedInputs := render.Inputs{
		Functions: []pkgv1.Function{
			{},
			{},
		},
	}
	
	mockRenderFunc := func(ctx context.Context, log logging.Logger, in render.Inputs) (render.Outputs, error) {
		if len(in.Functions) != len(expectedInputs.Functions) {
			t.Errorf("expected %d functions, got %d", 
				len(expectedInputs.Functions), 
				len(in.Functions))
		}
		return render.Outputs{}, nil
	}
	
	serializedFunc := RenderFunc(mockRenderFunc, &mu)
	
	_, err := serializedFunc(context.Background(), logging.NewNopLogger(), expectedInputs)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
