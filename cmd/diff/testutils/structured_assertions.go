// Package testutils provides test utilities for crossplane-diff.
package testutils

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
)

// StructuredDiffOutput mirrors renderer.StructuredDiffOutput to avoid import cycles.
// These types are used only for test assertions.
type StructuredDiffOutput struct {
	Summary Summary        `json:"summary"`
	Changes []ChangeDetail `json:"changes"`
}

// Summary mirrors renderer.Summary.
type Summary struct {
	Added    int `json:"added"`
	Modified int `json:"modified"`
	Removed  int `json:"removed"`
}

// ChangeDetail mirrors renderer.ChangeDetail.
type ChangeDetail struct {
	Type       string         `json:"type"`
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Name       string         `json:"name"`
	Namespace  string         `json:"namespace,omitempty"`
	Diff       map[string]any `json:"diff"`
}

// ExpectedDiff is a fluent builder for test expectations on structured diff output.
type ExpectedDiff struct {
	summary   *expectedSummary
	resources []*ResourceExpectation
}

type expectedSummary struct {
	added    int
	modified int
	removed  int
}

// ResourceExpectation defines expectations for a single resource change.
type ResourceExpectation struct {
	parent         *ExpectedDiff
	changeType     string // "added", "modified", "removed"
	kind           string
	name           string
	namePattern    *regexp.Regexp
	namespace      string
	fieldValues    map[string]any           // For added/removed: field path -> expected value
	fieldChanges   map[string][2]any        // For modified: field path -> [old, new]
	specMatch      map[string]any           // For strict spec matching
	anyNameAllowed bool                     // If true, any name is accepted
}

// ExpectDiff creates a new ExpectedDiff builder.
func ExpectDiff() *ExpectedDiff {
	return &ExpectedDiff{
		resources: make([]*ResourceExpectation, 0),
	}
}

// WithSummary sets the expected summary counts.
func (e *ExpectedDiff) WithSummary(added, modified, removed int) *ExpectedDiff {
	e.summary = &expectedSummary{
		added:    added,
		modified: modified,
		removed:  removed,
	}
	return e
}

// WithAddedResource adds an expectation for an added resource.
func (e *ExpectedDiff) WithAddedResource(kind, name, namespace string) *ResourceExpectation {
	r := &ResourceExpectation{
		parent:       e,
		changeType:   dt.DiffTypeWordAdded,
		kind:         kind,
		name:         name,
		namespace:    namespace,
		fieldValues:  make(map[string]any),
		fieldChanges: make(map[string][2]any),
	}
	e.resources = append(e.resources, r)
	return r
}

// WithModifiedResource adds an expectation for a modified resource.
func (e *ExpectedDiff) WithModifiedResource(kind, name, namespace string) *ResourceExpectation {
	r := &ResourceExpectation{
		parent:       e,
		changeType:   dt.DiffTypeWordModified,
		kind:         kind,
		name:         name,
		namespace:    namespace,
		fieldValues:  make(map[string]any),
		fieldChanges: make(map[string][2]any),
	}
	e.resources = append(e.resources, r)
	return r
}

// WithRemovedResource adds an expectation for a removed resource.
func (e *ExpectedDiff) WithRemovedResource(kind, name, namespace string) *ResourceExpectation {
	r := &ResourceExpectation{
		parent:       e,
		changeType:   dt.DiffTypeWordRemoved,
		kind:         kind,
		name:         name,
		namespace:    namespace,
		fieldValues:  make(map[string]any),
		fieldChanges: make(map[string][2]any),
	}
	e.resources = append(e.resources, r)
	return r
}

// WithField asserts a specific field exists with exact value (for added/removed resources).
func (r *ResourceExpectation) WithField(path string, value any) *ResourceExpectation {
	r.fieldValues[path] = value
	return r
}

// WithFieldChange asserts a field changed from old to new value (for modified resources).
func (r *ResourceExpectation) WithFieldChange(path string, oldValue, newValue any) *ResourceExpectation {
	r.fieldChanges[path] = [2]any{oldValue, newValue}
	return r
}

// WithSpec asserts the entire spec matches (for strict equality).
func (r *ResourceExpectation) WithSpec(spec map[string]any) *ResourceExpectation {
	r.specMatch = spec
	return r
}

// WithNamePattern matches resource name against a regex pattern instead of exact name.
func (r *ResourceExpectation) WithNamePattern(pattern string) *ResourceExpectation {
	r.namePattern = regexp.MustCompile(pattern)
	r.anyNameAllowed = false
	return r
}

// WithAnyName allows any resource name (useful for generated names).
func (r *ResourceExpectation) WithAnyName() *ResourceExpectation {
	r.anyNameAllowed = true
	return r
}

// And returns the parent ExpectedDiff to chain more resource expectations.
func (r *ResourceExpectation) And() *ExpectedDiff {
	return r.parent
}

// ParseStructuredOutput parses JSON output into StructuredDiffOutput.
func ParseStructuredOutput(jsonOutput string) (StructuredDiffOutput, error) {
	var output StructuredDiffOutput
	if err := json.Unmarshal([]byte(jsonOutput), &output); err != nil {
		return output, fmt.Errorf("failed to parse JSON output: %w", err)
	}
	return output, nil
}

// AssertStructuredDiff compares actual JSON output against expected.
func AssertStructuredDiff(t *testing.T, jsonOutput string, expected *ExpectedDiff) {
	t.Helper()

	output, err := ParseStructuredOutput(jsonOutput)
	if err != nil {
		t.Fatalf("Failed to parse structured output: %v\nOutput was:\n%s", err, jsonOutput)
	}

	// Check summary if specified
	if expected.summary != nil {
		if output.Summary.Added != expected.summary.added {
			t.Errorf("Summary.Added: expected %d, got %d", expected.summary.added, output.Summary.Added)
		}
		if output.Summary.Modified != expected.summary.modified {
			t.Errorf("Summary.Modified: expected %d, got %d", expected.summary.modified, output.Summary.Modified)
		}
		if output.Summary.Removed != expected.summary.removed {
			t.Errorf("Summary.Removed: expected %d, got %d", expected.summary.removed, output.Summary.Removed)
		}
	}

	// Check each resource expectation
	for _, expectRes := range expected.resources {
		found := findMatchingChange(output.Changes, expectRes)
		if found == nil {
			// Build detailed message showing what we expected vs what we got
			actualResources := make([]string, 0, len(output.Changes))
			for _, c := range output.Changes {
				actualResources = append(actualResources, fmt.Sprintf("%s %s/%s (ns=%s)", c.Type, c.Kind, c.Name, c.Namespace))
			}
			t.Errorf("Expected %s resource %s/%s (ns=%s) not found in output. Actual resources: %v",
				expectRes.changeType, expectRes.kind, expectRes.name, expectRes.namespace, actualResources)
			continue
		}

		// Validate field values for added/removed resources
		for path, expectedValue := range expectRes.fieldValues {
			actualValue := getFieldFromDiff(found.Diff, expectRes.changeType, path)
			if !valuesEqual(actualValue, expectedValue) {
				t.Errorf("%s %s/%s: field %s: expected %v, got %v",
					expectRes.changeType, expectRes.kind, expectRes.name, path, expectedValue, actualValue)
			}
		}

		// Validate field changes for modified resources
		for path, change := range expectRes.fieldChanges {
			oldVal := getFieldFromDiff(found.Diff, "old", path)
			newVal := getFieldFromDiff(found.Diff, "new", path)

			if !valuesEqual(oldVal, change[0]) {
				t.Errorf("%s %s/%s: field %s old value: expected %v, got %v",
					expectRes.changeType, expectRes.kind, expectRes.name, path, change[0], oldVal)
			}
			if !valuesEqual(newVal, change[1]) {
				t.Errorf("%s %s/%s: field %s new value: expected %v, got %v",
					expectRes.changeType, expectRes.kind, expectRes.name, path, change[1], newVal)
			}
		}

		// Validate spec match if specified
		if expectRes.specMatch != nil {
			spec := getFieldFromDiff(found.Diff, expectRes.changeType, "spec")
			if specMap, ok := spec.(map[string]any); ok {
				if !mapsMatch(specMap, expectRes.specMatch) {
					t.Errorf("%s %s/%s: spec mismatch: expected %v, got %v",
						expectRes.changeType, expectRes.kind, expectRes.name, expectRes.specMatch, specMap)
				}
			} else {
				t.Errorf("%s %s/%s: spec is not a map: %v",
					expectRes.changeType, expectRes.kind, expectRes.name, spec)
			}
		}
	}

	// Check for unexpected resources
	if len(expected.resources) > 0 && len(output.Changes) != len(expected.resources) {
		t.Errorf("Expected %d changes, got %d", len(expected.resources), len(output.Changes))
	}
}

// findMatchingChange finds a change that matches the resource expectation.
func findMatchingChange(changes []ChangeDetail, expect *ResourceExpectation) *ChangeDetail {
	for i := range changes {
		change := &changes[i]
		if change.Type != expect.changeType {
			continue
		}
		if change.Kind != expect.kind {
			continue
		}
		if expect.namespace != "" && change.Namespace != expect.namespace {
			continue
		}

		// Check name matching
		if expect.anyNameAllowed {
			return change
		}
		if expect.namePattern != nil {
			if expect.namePattern.MatchString(change.Name) {
				return change
			}
			continue
		}
		if change.Name == expect.name {
			return change
		}
	}
	return nil
}

// getFieldFromDiff extracts a field value from the diff structure.
// For added/removed, key is "spec"; for modified, key is "old" or "new".
func getFieldFromDiff(diff map[string]any, key, path string) any {
	// Handle special keys for modified resources
	var root any
	if key == "old" || key == "new" {
		root = diff[key]
	} else if key == "added" || key == "removed" {
		root = diff["spec"]
	} else {
		root = diff[key]
	}

	if root == nil {
		return nil
	}

	return getNestedField(root, path)
}

// getNestedField extracts a nested field using dot notation (e.g., "spec.forProvider.configData").
func getNestedField(obj any, path string) any {
	if path == "" {
		return obj
	}

	parts := strings.Split(path, ".")
	current := obj

	for _, part := range parts {
		switch v := current.(type) {
		case map[string]any:
			current = v[part]
		default:
			return nil
		}
		if current == nil {
			return nil
		}
	}

	return current
}

// valuesEqual compares two values for equality.
func valuesEqual(a, b any) bool {
	// Handle nil cases
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	// For numeric comparisons, handle type coercion
	// JSON numbers are float64, but we might compare with int
	switch av := a.(type) {
	case float64:
		switch bv := b.(type) {
		case float64:
			return av == bv
		case int:
			return av == float64(bv)
		case int64:
			return av == float64(bv)
		}
	case int:
		switch bv := b.(type) {
		case float64:
			return float64(av) == bv
		case int:
			return av == bv
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return av == bv
		}
	case string:
		if bv, ok := b.(string); ok {
			return av == bv
		}
	}

	// Fall back to direct comparison
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// mapsMatch checks if two maps have the same keys and values (recursively).
func mapsMatch(actual, expected map[string]any) bool {
	if len(actual) != len(expected) {
		return false
	}
	for k, ev := range expected {
		av, exists := actual[k]
		if !exists {
			return false
		}
		// Recursive map comparison
		if em, ok := ev.(map[string]any); ok {
			am, ok := av.(map[string]any)
			if !ok || !mapsMatch(am, em) {
				return false
			}
			continue
		}
		if !valuesEqual(av, ev) {
			return false
		}
	}
	return true
}
