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

package diffprocessor

import (
	"context"
	"strconv"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/v2/cmd/crank/render"
)

func TestNewEventualStateSimulator(t *testing.T) {
	logger := tu.TestLogger(t, false)
	renderFunc := func(_ context.Context, _ logging.Logger, _ render.Inputs) (render.Outputs, error) {
		return render.Outputs{}, nil
	}

	simulator := NewEventualStateSimulator(renderFunc, logger, nil)

	if simulator == nil {
		t.Fatal("NewEventualStateSimulator() returned nil")
	}

	if simulator.renderFunc == nil {
		t.Error("NewEventualStateSimulator() did not set renderFunc")
	}
}

func TestSimulateToStableState_AlreadyStable(t *testing.T) {
	// Test case: No new resources appear on first render - already stable
	logger := tu.TestLogger(t, false)

	renderCount := 0
	renderFunc := func(_ context.Context, _ logging.Logger, _ render.Inputs) (render.Outputs, error) {
		renderCount++
		// Return empty composed resources - nothing new to discover
		return render.Outputs{
			CompositeResource: cmp.New(),
			ComposedResources: []cpd.Unstructured{},
		}, nil
	}

	simulator := NewEventualStateSimulator(renderFunc, logger, nil)

	xr := cmp.New()
	xr.SetName("test-xr")
	xr.SetAPIVersion("example.org/v1")
	xr.SetKind("XR")

	comp := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "test-composition"},
	}

	observed, err := simulator.SimulateToStableState(
		context.Background(), xr, comp, nil, nil, nil)
	if err != nil {
		t.Fatalf("SimulateToStableState() error = %v", err)
	}

	if len(observed) != 0 {
		t.Errorf("SimulateToStableState() returned %d observed resources, want 0", len(observed))
	}

	if renderCount != 1 {
		t.Errorf("SimulateToStableState() rendered %d times, want 1", renderCount)
	}
}

func TestSimulateToStableState_MultiStageProgression(t *testing.T) {
	// Test case: Simulates function-sequencer with 3 stages
	// Stage 1 returns resource-a, Stage 2 returns resource-b, Stage 3 returns nothing new
	logger := tu.TestLogger(t, false)

	renderCount := 0
	renderFunc := func(_ context.Context, _ logging.Logger, in render.Inputs) (render.Outputs, error) {
		renderCount++

		// Check how many resources are in observed to determine which stage we're in
		observedCount := len(in.ObservedResources)

		switch observedCount {
		case 0:
			// First render: return stage 1 resource
			return render.Outputs{
				CompositeResource: cmp.New(),
				ComposedResources: []cpd.Unstructured{
					makeTestComposedResource("resource-a", "stage-1"),
				},
			}, nil
		case 1:
			// Second render: stage 1 is "ready", return stage 1 + stage 2
			return render.Outputs{
				CompositeResource: cmp.New(),
				ComposedResources: []cpd.Unstructured{
					makeTestComposedResource("resource-a", "stage-1"),
					makeTestComposedResource("resource-b", "stage-2"),
				},
			}, nil
		default:
			// Third render: stage 1 and 2 are "ready", return same (stable)
			return render.Outputs{
				CompositeResource: cmp.New(),
				ComposedResources: []cpd.Unstructured{
					makeTestComposedResource("resource-a", "stage-1"),
					makeTestComposedResource("resource-b", "stage-2"),
				},
			}, nil
		}
	}

	simulator := NewEventualStateSimulator(renderFunc, logger, nil)

	xr := cmp.New()
	xr.SetName("test-xr")
	xr.SetAPIVersion("example.org/v1")
	xr.SetKind("XR")

	comp := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "test-composition"},
	}

	observed, err := simulator.SimulateToStableState(
		context.Background(), xr, comp, nil, nil, nil)
	if err != nil {
		t.Fatalf("SimulateToStableState() error = %v", err)
	}

	// Should have 2 resources after reaching stability
	if len(observed) != 2 {
		t.Errorf("SimulateToStableState() returned %d observed resources, want 2", len(observed))
	}

	// Should have rendered 3 times (stage 1, stage 2, stability check)
	if renderCount != 3 {
		t.Errorf("SimulateToStableState() rendered %d times, want 3", renderCount)
	}
}

func TestSimulateToStableState_MaxIterationsExceeded(t *testing.T) {
	// Test case: Simulation never stabilizes - should fail after max iterations
	logger := tu.TestLogger(t, false)

	resourceCounter := 0
	renderFunc := func(_ context.Context, _ logging.Logger, _ render.Inputs) (render.Outputs, error) {
		// Always return a new resource - never stabilizes
		resourceCounter++

		return render.Outputs{
			CompositeResource: cmp.New(),
			ComposedResources: []cpd.Unstructured{
				makeTestComposedResource("resource", "stage-"+strconv.Itoa(resourceCounter)),
			},
		}, nil
	}

	simulator := NewEventualStateSimulator(renderFunc, logger, nil)

	xr := cmp.New()
	xr.SetName("test-xr")
	xr.SetAPIVersion("example.org/v1")
	xr.SetKind("XR")

	comp := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "test-composition"},
	}

	_, err := simulator.SimulateToStableState(
		context.Background(), xr, comp, nil, nil, nil)
	if err == nil {
		t.Fatal("SimulateToStableState() expected error for max iterations exceeded")
	}

	if resourceCounter != maxSimulationIterations {
		t.Errorf("SimulateToStableState() rendered %d times, want %d", resourceCounter, maxSimulationIterations)
	}
}

func TestSimulateToStableState_RenderError(t *testing.T) {
	// Test case: Render fails - should propagate error
	logger := tu.TestLogger(t, false)

	renderFunc := func(_ context.Context, _ logging.Logger, _ render.Inputs) (render.Outputs, error) {
		return render.Outputs{}, &testError{msg: "render failed"}
	}

	simulator := NewEventualStateSimulator(renderFunc, logger, nil)

	xr := cmp.New()
	xr.SetName("test-xr")
	xr.SetAPIVersion("example.org/v1")
	xr.SetKind("XR")

	comp := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "test-composition"},
	}

	_, err := simulator.SimulateToStableState(
		context.Background(), xr, comp, nil, nil, nil)
	if err == nil {
		t.Fatal("SimulateToStableState() expected error for render failure")
	}
}

func TestSynthesizeReadyStatus(t *testing.T) {
	resources := []cpd.Unstructured{
		makeTestComposedResource("resource-a", "stage-1"),
		makeTestComposedResource("resource-b", "stage-2"),
	}

	result := synthesizeReadyStatus(resources)

	if len(result) != 2 {
		t.Fatalf("synthesizeReadyStatus() returned %d resources, want 2", len(result))
	}

	// Check that Ready condition was added to each resource
	for i, res := range result {
		conditions, found, err := un.NestedSlice(res.Object, "status", "conditions")
		if err != nil || !found {
			t.Errorf("resource[%d] missing status.conditions", i)
			continue
		}

		hasReady := false

		for _, c := range conditions {
			cond, ok := c.(map[string]any)
			if !ok {
				continue
			}

			condType, _, _ := un.NestedString(cond, "type")
			condStatus, _, _ := un.NestedString(cond, "status")

			if condType == "Ready" && condStatus == "True" {
				hasReady = true
				break
			}
		}

		if !hasReady {
			t.Errorf("resource[%d] does not have Ready=True condition", i)
		}
	}
}

func TestFindNewResources(t *testing.T) {
	tests := map[string]struct {
		rendered []cpd.Unstructured
		observed []cpd.Unstructured
		wantNew  int
	}{
		"AllNew": {
			rendered: []cpd.Unstructured{
				makeTestComposedResource("resource-a", "stage-1"),
				makeTestComposedResource("resource-b", "stage-2"),
			},
			observed: []cpd.Unstructured{},
			wantNew:  2,
		},
		"NoneNew": {
			rendered: []cpd.Unstructured{
				makeTestComposedResource("resource-a", "stage-1"),
			},
			observed: []cpd.Unstructured{
				makeTestComposedResource("resource-a", "stage-1"),
			},
			wantNew: 0,
		},
		"SomeNew": {
			rendered: []cpd.Unstructured{
				makeTestComposedResource("resource-a", "stage-1"),
				makeTestComposedResource("resource-b", "stage-2"),
			},
			observed: []cpd.Unstructured{
				makeTestComposedResource("resource-a", "stage-1"),
			},
			wantNew: 1,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := findNewResources(tt.rendered, tt.observed)
			if len(got) != tt.wantNew {
				t.Errorf("findNewResources() returned %d new resources, want %d", len(got), tt.wantNew)
			}
		})
	}
}

func TestMergeObservedResources(t *testing.T) {
	existing := []cpd.Unstructured{
		makeTestComposedResource("resource-a", "stage-1"),
	}

	newResources := []cpd.Unstructured{
		makeTestComposedResource("resource-a-updated", "stage-1"), // Same composition-resource-name, should replace
		makeTestComposedResource("resource-b", "stage-2"),         // New resource
	}

	merged := mergeObservedResources(existing, newResources)

	if len(merged) != 2 {
		t.Errorf("mergeObservedResources() returned %d resources, want 2", len(merged))
	}
}

// Helper functions

func makeTestComposedResource(name, compResName string) cpd.Unstructured {
	res := cpd.New()
	res.SetName(name)
	res.SetAPIVersion("nop.crossplane.io/v1alpha1")
	res.SetKind("NopResource")
	res.SetAnnotations(map[string]string{
		"crossplane.io/composition-resource-name": compResName,
	})

	return *res
}

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}
