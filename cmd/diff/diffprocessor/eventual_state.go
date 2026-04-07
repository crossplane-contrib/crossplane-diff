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
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
	"github.com/crossplane/crossplane/v2/cmd/crank/render"
)

const (
	// maxSimulationIterations is the maximum number of render iterations to prevent infinite loops.
	// This should be sufficient for most multi-stage function-sequencer pipelines.
	maxSimulationIterations = 20

	// ConditionTypeReady is the type for Ready conditions.
	ConditionTypeReady = "Ready"
)

// EventualStateSimulator iteratively renders a composition pipeline until a stable state is reached.
// This is useful with function-sequencer which hides resources for later stages until earlier
// stages become Ready. The simulator synthesizes Ready status on rendered resources to simulate
// what Crossplane would do after multiple reconciliation cycles.
type EventualStateSimulator struct {
	renderFunc           RenderFunc
	logger               logging.Logger
	functionCredentials  []corev1.Secret
	requirementsResolver RequirementsResolver
}

// NewEventualStateSimulator creates a new EventualStateSimulator.
func NewEventualStateSimulator(
	renderFunc RenderFunc,
	logger logging.Logger,
	functionCredentials []corev1.Secret,
	requirementsResolver RequirementsResolver,
) *EventualStateSimulator {
	return &EventualStateSimulator{
		renderFunc:           renderFunc,
		logger:               logger,
		functionCredentials:  functionCredentials,
		requirementsResolver: requirementsResolver,
	}
}

// SimulateToStableState iteratively renders until no new resources appear.
// It returns augmented observed resources that represent the eventual state after all
// reconciliation cycles complete.
//
// The simulation loop:
// 1. Render with current observed resources and resolved requirements
// 2. Process any requirements from the render output (environment configs, etc.)
// 3. Check if any new resources appeared (compared to previous iteration)
// 4. If no new resources, we've reached stable state - return current observed
// 5. Synthesize Ready=True status on all rendered resources
// 6. Merge with existing observed resources for next iteration
// 7. Repeat until stable or max iterations exceeded.
func (s *EventualStateSimulator) SimulateToStableState(
	ctx context.Context,
	xr *cmp.Unstructured,
	comp *apiextensionsv1.Composition,
	fns []pkgv1.Function,
	initialObserved []cpd.Unstructured,
	xrNamespace string,
) ([]cpd.Unstructured, error) {
	observed := initialObserved

	// Track required resources across iterations (e.g., environment configs)
	requiredResources := make(map[string]un.Unstructured)

	s.logger.Debug("Starting eventual state simulation",
		"xr", xr.GetName(),
		"composition", comp.GetName(),
		"initialObservedCount", len(initialObserved))

	for i := range maxSimulationIterations {
		s.logger.Debug("Simulation iteration",
			"iteration", i+1,
			"observedCount", len(observed),
			"requiredResourcesCount", len(requiredResources))

		// Render with current observed state and any resolved requirements
		output, err := s.renderFunc(ctx, s.logger, render.Inputs{
			CompositeResource:   xr,
			Composition:         comp,
			Functions:           fns,
			FunctionCredentials: s.functionCredentials,
			ObservedResources:   observed,
			RequiredResources:   slices.Collect(maps.Values(requiredResources)),
		})

		// Handle requirements even if render returned an error
		// (functions may return requirements along with errors)
		newRequirementsResolved := false

		if len(output.Requirements) > 0 && s.requirementsResolver != nil {
			s.logger.Debug("Processing requirements from simulation render",
				"iteration", i+1,
				"requirementCount", len(output.Requirements))

			additionalResources, resolveErr := s.requirementsResolver.ProvideRequirements(ctx, output.Requirements, xrNamespace)
			if resolveErr != nil {
				s.logger.Debug("Failed to resolve requirements in simulation",
					"iteration", i+1,
					"error", resolveErr)
				// Continue without the additional resources - let the original error surface if any
			} else {
				// Add resources with deduplication
				newCount := 0
				for _, res := range additionalResources {
					if addUniqueResource(requiredResources, res) {
						newCount++
					}
				}
				newRequirementsResolved = newCount > 0

				s.logger.Debug("Resolved requirements for simulation",
					"iteration", i+1,
					"fetchedCount", len(additionalResources),
					"newUniqueCount", newCount,
					"totalRequiredResourcesCount", len(requiredResources),
					"newRequirementsResolved", newRequirementsResolved)
			}
		}

		// Check for render errors - but continue if we just resolved new requirements
		// (the next iteration may succeed with the resolved requirements)
		if err != nil {
			if newRequirementsResolved {
				s.logger.Debug("Render failed but resolved new requirements, continuing simulation",
					"iteration", i+1,
					"error", err)

				continue
			}

			return nil, errors.Wrapf(err, "simulation render failed at iteration %d", i+1)
		}

		// Check for stability (no new resources compared to observed)
		newResources := findNewResources(output.ComposedResources, observed)
		if len(newResources) == 0 {
			s.logger.Debug("Eventual state simulation reached stability",
				"iterations", i+1,
				"finalObservedCount", len(observed))

			return observed, nil
		}

		s.logger.Debug("Found new resources in simulation",
			"iteration", i+1,
			"newResourceCount", len(newResources))

		// Synthesize Ready status on all composed resources from this render
		readyResources, err := synthesizeReadyStatus(output.ComposedResources)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to synthesize Ready status at iteration %d", i+1)
		}

		// Merge with existing observed resources
		observed = mergeObservedResources(observed, readyResources)
	}

	return nil, errors.Errorf("eventual state simulation did not stabilize after %d iterations", maxSimulationIterations)
}

// findNewResources identifies resources in rendered that don't exist in observed.
// Resources are matched by their composition-resource-name annotation and GVK.
func findNewResources(rendered, observed []cpd.Unstructured) []cpd.Unstructured {
	observedKeys := make(map[string]bool)

	for _, obs := range observed {
		key := makeResourceKey(&obs)
		observedKeys[key] = true
	}

	var newResources []cpd.Unstructured

	for _, res := range rendered {
		key := makeResourceKey(&res)
		if !observedKeys[key] {
			newResources = append(newResources, res)
		}
	}

	return newResources
}

// makeResourceKey creates a unique key for a resource based on its composition-resource-name
// annotation and GVK. This is how Crossplane matches composed resources across renders.
// Note: composition-resource-name must be unique within a composition, so namespace is not needed.
func makeResourceKey(res *cpd.Unstructured) string {
	compResName := res.GetAnnotations()["crossplane.io/composition-resource-name"]
	gvk := res.GroupVersionKind()

	return compResName + "/" + gvk.Group + "/" + gvk.Version + "/" + gvk.Kind
}

// synthesizeReadyStatus adds Ready=True condition to all resources.
// This simulates what Crossplane does when resources become healthy.
func synthesizeReadyStatus(resources []cpd.Unstructured) ([]cpd.Unstructured, error) {
	result := make([]cpd.Unstructured, len(resources))
	now := metav1.Now()

	for i, res := range resources {
		// Deep copy to avoid modifying the original
		copied := res.DeepCopy()
		result[i] = *copied

		if err := setReadyCondition(&result[i], now); err != nil {
			return nil, errors.Wrapf(err, "cannot set Ready condition on resource %s", res.GetName())
		}
	}

	return result, nil
}

// setReadyCondition sets a Ready=True condition on the resource's status.conditions field.
func setReadyCondition(res *cpd.Unstructured, now metav1.Time) error {
	// Get or initialize conditions
	conditionsRaw, _, _ := un.NestedSlice(res.Object, "status", "conditions")
	conditions := make([]any, 0, len(conditionsRaw)+1)

	// Copy existing conditions, excluding any existing Ready condition
	for _, c := range conditionsRaw {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}

		condType, _, _ := un.NestedString(cond, "type")
		if condType != ConditionTypeReady {
			conditions = append(conditions, cond)
		}
	}

	// Add the Ready condition
	readyCondition := map[string]any{
		"type":               ConditionTypeReady,
		"status":             "True",
		"reason":             "Available",
		"lastTransitionTime": now.Format(time.RFC3339),
	}
	conditions = append(conditions, readyCondition)

	// Set the conditions back - this can fail if there's a type mismatch in the object structure
	if err := un.SetNestedSlice(res.Object, conditions, "status", "conditions"); err != nil {
		return errors.Wrap(err, "cannot set status.conditions")
	}

	return nil
}

// mergeObservedResources merges newResources with existing observed resources.
// If a resource with the same key exists, the new version replaces the old one.
func mergeObservedResources(existing, newResources []cpd.Unstructured) []cpd.Unstructured {
	result := make(map[string]cpd.Unstructured)

	// Add existing resources
	for _, res := range existing {
		key := makeResourceKey(&res)
		result[key] = res
	}

	// Add/update with new resources
	for _, res := range newResources {
		key := makeResourceKey(&res)
		result[key] = res
	}

	// Convert back to slice
	merged := make([]cpd.Unstructured, 0, len(result))
	for _, res := range result {
		merged = append(merged, res)
	}

	return merged
}
