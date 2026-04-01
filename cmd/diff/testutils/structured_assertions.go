// Package testutils provides test utilities for crossplane-diff.
package testutils

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	"k8s.io/client-go/util/jsonpath"
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
	parent              *ExpectedDiff
	changeType          string // "added", "modified", "removed"
	kind                string
	name                string
	namePattern         *regexp.Regexp
	namespace           string
	fieldValues         map[string]any            // For added/removed: field path -> expected value
	fieldChanges        map[string][2]any         // For modified: field path -> [old, new]
	fieldValuePatterns  map[string]*regexp.Regexp // For pattern matching field values
	specMatch           map[string]any            // For strict spec matching
	anyNameAllowed      bool                      // If true, any name is accepted
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
		parent:             e,
		changeType:         dt.DiffTypeWordAdded,
		kind:               kind,
		name:               name,
		namespace:          namespace,
		fieldValues:        make(map[string]any),
		fieldChanges:       make(map[string][2]any),
		fieldValuePatterns: make(map[string]*regexp.Regexp),
	}
	e.resources = append(e.resources, r)
	return r
}

// WithModifiedResource adds an expectation for a modified resource.
func (e *ExpectedDiff) WithModifiedResource(kind, name, namespace string) *ResourceExpectation {
	r := &ResourceExpectation{
		parent:             e,
		changeType:         dt.DiffTypeWordModified,
		kind:               kind,
		name:               name,
		namespace:          namespace,
		fieldValues:        make(map[string]any),
		fieldChanges:       make(map[string][2]any),
		fieldValuePatterns: make(map[string]*regexp.Regexp),
	}
	e.resources = append(e.resources, r)
	return r
}

// WithRemovedResource adds an expectation for a removed resource.
func (e *ExpectedDiff) WithRemovedResource(kind, name, namespace string) *ResourceExpectation {
	r := &ResourceExpectation{
		parent:             e,
		changeType:         dt.DiffTypeWordRemoved,
		kind:               kind,
		name:               name,
		namespace:          namespace,
		fieldValues:        make(map[string]any),
		fieldChanges:       make(map[string][2]any),
		fieldValuePatterns: make(map[string]*regexp.Regexp),
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

// WithFieldAdded asserts a field was added (for modified resources).
// Equivalent to WithFieldChange(path, nil, value).
func (r *ResourceExpectation) WithFieldAdded(path string, value any) *ResourceExpectation {
	return r.WithFieldChange(path, nil, value)
}

// WithFieldRemoved asserts a field was removed (for modified resources).
// Equivalent to WithFieldChange(path, value, nil).
func (r *ResourceExpectation) WithFieldRemoved(path string, value any) *ResourceExpectation {
	return r.WithFieldChange(path, value, nil)
}

// WithFieldValuePattern asserts a field value matches a regex pattern.
// Use this for fields with generated/dynamic values like resource names with random suffixes.
func (r *ResourceExpectation) WithFieldValuePattern(path, pattern string) *ResourceExpectation {
	r.fieldValuePatterns[path] = regexp.MustCompile(pattern)
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

		// Validate field value patterns
		for path, pattern := range expectRes.fieldValuePatterns {
			actualValue := getNewFieldValue(found.Diff, expectRes.changeType, path)
			actualStr := fmt.Sprintf("%v", actualValue)
			if !pattern.MatchString(actualStr) {
				t.Errorf("%s %s/%s: field %s value %q does not match pattern %q",
					expectRes.changeType, expectRes.kind, expectRes.name, path, actualStr, pattern.String())
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
	} else if key == dt.DiffTypeWordAdded || key == dt.DiffTypeWordRemoved {
		root = diff["spec"]
	} else {
		root = diff[key]
	}

	if root == nil {
		return nil
	}

	return getNestedField(root, path)
}

// getNewFieldValue returns the "new" field value for assertions.
// For modified resources, it returns the value from the "new" state.
// For added/removed resources, it returns the value from the "spec".
func getNewFieldValue(diff map[string]any, changeType, path string) any {
	if changeType == dt.DiffTypeWordModified {
		return getFieldFromDiff(diff, "new", path)
	}
	return getFieldFromDiff(diff, changeType, path)
}

// getNestedField extracts a nested field using k8s jsonpath.
// Examples:
//   - "spec.forProvider.configData" - simple path
//   - "spec.pipeline[0].functionRef.name" - array index
//   - "spec.pipeline[1].input.resources[0].patches[0].transforms" - multiple array indices
func getNestedField(obj any, path string) any {
	if path == "" {
		return obj
	}

	// Convert bracket notation to k8s jsonpath escaped format
	// "metadata.annotations['key.with.dots']" -> "metadata.annotations.key\.with\.dots"
	path = convertBracketNotation(path)

	// Wrap path in k8s jsonpath template syntax: "spec.foo[0]" -> "{.spec.foo[0]}"
	jp := jsonpath.New("field")
	jp.AllowMissingKeys(true)

	if err := jp.Parse("{." + path + "}"); err != nil {
		return nil
	}

	results, err := jp.FindResults(obj)
	if err != nil || len(results) == 0 || len(results[0]) == 0 {
		return nil
	}

	return results[0][0].Interface()
}

// convertBracketNotation converts bracket notation for map keys to k8s jsonpath escaped format.
// Examples:
//   - "metadata.annotations['key.with.dots']" -> "metadata.annotations.key\.with\.dots"
//   - "spec.data['my-key']" -> "spec.data.my-key"
//   - "spec.pipeline[0].name" -> "spec.pipeline[0].name" (array indices unchanged)
func convertBracketNotation(path string) string {
	// Pattern matches: ['key'] or ["key"] where key can contain dots
	bracketPattern := regexp.MustCompile(`\['([^']+)'\]|\["([^"]+)"\]`)

	return bracketPattern.ReplaceAllStringFunc(path, func(match string) string {
		// Extract the key from ['key'] or ["key"]
		key := match[2 : len(match)-2] // Remove [' and '] or [" and "]

		// Escape dots in the key for k8s jsonpath
		escapedKey := strings.ReplaceAll(key, ".", `\.`)

		// Return as dot notation with escaped dots
		return "." + escapedKey
	})
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

// --- Composition Diff Types and Assertions ---

// StructuredCompDiffOutput mirrors the JSON schema for composition diffs.
type StructuredCompDiffOutput struct {
	Compositions []CompositionDiffJSON `json:"compositions"`
	Errors       []OutputError         `json:"errors,omitempty"`
}

// OutputError mirrors dt.OutputError.
type OutputError struct {
	ResourceID string `json:"resourceId,omitempty"`
	Message    string `json:"message"`
}

// CompositionDiffJSON mirrors compositionDiffJSON from the renderer.
type CompositionDiffJSON struct {
	Name               string                   `json:"name"`
	Error              string                   `json:"error,omitempty"`
	CompositionChanges *ChangeDetail            `json:"compositionChanges,omitempty"`
	AffectedResources  AffectedResourcesSummary `json:"affectedResources"`
	ImpactAnalysis     []XRImpactJSON           `json:"impactAnalysis"`
}

// AffectedResourcesSummary mirrors renderer.AffectedResourcesSummary.
type AffectedResourcesSummary struct {
	Total            int `json:"total"`
	WithChanges      int `json:"withChanges"`
	Unchanged        int `json:"unchanged"`
	WithErrors       int `json:"withErrors"`
	FilteredByPolicy int `json:"filteredByPolicy,omitempty"`
}

// XRImpactJSON mirrors xrImpactJSON from the renderer.
type XRImpactJSON struct {
	APIVersion        string             `json:"apiVersion,omitempty"`
	Kind              string             `json:"kind,omitempty"`
	Name              string             `json:"name,omitempty"`
	Namespace         string             `json:"namespace,omitempty"`
	UID               string             `json:"uid,omitempty"`
	Status            string             `json:"status"`
	Error             string             `json:"error,omitempty"`
	DownstreamChanges *DownstreamChanges `json:"downstreamChanges,omitempty"`
}

// DownstreamChanges mirrors renderer.DownstreamChanges.
type DownstreamChanges struct {
	Summary Summary        `json:"summary"`
	Changes []ChangeDetail `json:"changes"`
}

// ParseStructuredCompOutput parses JSON output into StructuredCompDiffOutput.
func ParseStructuredCompOutput(jsonOutput string) (StructuredCompDiffOutput, error) {
	var output StructuredCompDiffOutput
	if err := json.Unmarshal([]byte(jsonOutput), &output); err != nil {
		return output, fmt.Errorf("failed to parse comp diff JSON output: %w", err)
	}
	return output, nil
}

// ExpectedCompDiff is a fluent builder for test expectations on composition diff output.
type ExpectedCompDiff struct {
	compositions []*CompositionExpectation
}

// CompositionExpectation defines expectations for a single composition in the diff.
type CompositionExpectation struct {
	parent                   *ExpectedCompDiff
	name                     string
	affectedTotal            *int
	affectedWithChanges      *int
	affectedUnchanged        *int
	affectedWithErrors       *int
	affectedFilteredByPolicy *int
	xrImpacts                []*XRImpactExpectation
	// Composition changes expectations
	compositionChangeType   string            // "modified" - composition changes are always modifications
	compositionFieldChanges map[string][2]any // For modified: field path -> [old, new]
}

// XRImpactExpectation defines expectations for a single XR impact.
type XRImpactExpectation struct {
	parent              *CompositionExpectation
	kind                string
	name                string
	namespace           string
	anyNameAllowed      bool
	status              string // "changed", "unchanged", "error"
	downstreamSummary   *expectedSummary
	downstreamResources []*DownstreamResourceExpectation
}

// ExpectCompDiff creates a new ExpectedCompDiff builder.
func ExpectCompDiff() *ExpectedCompDiff {
	return &ExpectedCompDiff{
		compositions: make([]*CompositionExpectation, 0),
	}
}

// WithComposition adds an expectation for a composition.
func (e *ExpectedCompDiff) WithComposition(name string) *CompositionExpectation {
	c := &CompositionExpectation{
		parent:    e,
		name:      name,
		xrImpacts: make([]*XRImpactExpectation, 0),
	}
	e.compositions = append(e.compositions, c)
	return c
}

// WithAffectedResources sets the expected affected resources counts.
func (c *CompositionExpectation) WithAffectedResources(total, withChanges, unchanged, withErrors int) *CompositionExpectation {
	c.affectedTotal = &total
	c.affectedWithChanges = &withChanges
	c.affectedUnchanged = &unchanged
	c.affectedWithErrors = &withErrors
	return c
}

// WithCompositionModified asserts that the composition itself is modified.
func (c *CompositionExpectation) WithCompositionModified() *CompositionExpectation {
	c.compositionChangeType = "modified"
	if c.compositionFieldChanges == nil {
		c.compositionFieldChanges = make(map[string][2]any)
	}
	return c
}

// WithCompositionFieldChange asserts a specific field in the composition changed from old to new value.
func (c *CompositionExpectation) WithCompositionFieldChange(path string, oldValue, newValue any) *CompositionExpectation {
	if c.compositionFieldChanges == nil {
		c.compositionFieldChanges = make(map[string][2]any)
	}
	c.compositionFieldChanges[path] = [2]any{oldValue, newValue}
	return c
}

// WithXRImpact adds an expectation for an XR impact.
func (c *CompositionExpectation) WithXRImpact(kind, name, namespace, status string) *XRImpactExpectation {
	x := &XRImpactExpectation{
		parent:    c,
		kind:      kind,
		name:      name,
		namespace: namespace,
		status:    status,
	}
	c.xrImpacts = append(c.xrImpacts, x)
	return x
}

// WithAnyName allows any XR name (useful for generated names).
func (x *XRImpactExpectation) WithAnyName() *XRImpactExpectation {
	x.anyNameAllowed = true
	return x
}

// WithDownstreamSummary sets the expected downstream changes summary.
func (x *XRImpactExpectation) WithDownstreamSummary(added, modified, removed int) *XRImpactExpectation {
	x.downstreamSummary = &expectedSummary{
		added:    added,
		modified: modified,
		removed:  removed,
	}
	return x
}

// WithDownstreamResource adds an expectation for a downstream resource change.
// Use this to assert on specific field-level changes in downstream resources.
func (x *XRImpactExpectation) WithDownstreamResource(changeType, kind, name, namespace string) *DownstreamResourceExpectation {
	r := &DownstreamResourceExpectation{
		parent:             x,
		changeType:         changeType,
		kind:               kind,
		name:               name,
		namespace:          namespace,
		fieldChanges:       make(map[string][2]any),
		fieldValues:        make(map[string]any),
		fieldValuePatterns: make(map[string]*regexp.Regexp),
	}
	x.downstreamResources = append(x.downstreamResources, r)
	return r
}

// DownstreamResourceExpectation defines expectations for a downstream resource change.
type DownstreamResourceExpectation struct {
	parent             *XRImpactExpectation
	changeType         string // "added", "modified", "removed"
	kind               string
	name               string
	namePattern        *regexp.Regexp
	namespace          string
	anyNameAllowed     bool
	fieldChanges       map[string][2]any         // For modified: field path -> [old, new]
	fieldValues        map[string]any            // For added/removed: field path -> expected value
	fieldValuePatterns map[string]*regexp.Regexp // For pattern matching field values
}

// WithFieldChange asserts a field changed from old to new value (for modified resources).
func (d *DownstreamResourceExpectation) WithFieldChange(path string, oldValue, newValue any) *DownstreamResourceExpectation {
	d.fieldChanges[path] = [2]any{oldValue, newValue}
	return d
}

// WithFieldAdded asserts a field was added (for modified resources).
// Equivalent to WithFieldChange(path, nil, value).
func (d *DownstreamResourceExpectation) WithFieldAdded(path string, value any) *DownstreamResourceExpectation {
	return d.WithFieldChange(path, nil, value)
}

// WithFieldRemoved asserts a field was removed (for modified resources).
// Equivalent to WithFieldChange(path, value, nil).
func (d *DownstreamResourceExpectation) WithFieldRemoved(path string, value any) *DownstreamResourceExpectation {
	return d.WithFieldChange(path, value, nil)
}

// WithField asserts a specific field exists with exact value (for added/removed resources).
func (d *DownstreamResourceExpectation) WithField(path string, value any) *DownstreamResourceExpectation {
	d.fieldValues[path] = value
	return d
}

// WithFieldValuePattern asserts a field value matches a regex pattern.
// Use this for fields with generated/dynamic values like resource names with random suffixes.
func (d *DownstreamResourceExpectation) WithFieldValuePattern(path, pattern string) *DownstreamResourceExpectation {
	d.fieldValuePatterns[path] = regexp.MustCompile(pattern)
	return d
}

// WithAnyName allows any resource name (useful for generated names).
func (d *DownstreamResourceExpectation) WithAnyName() *DownstreamResourceExpectation {
	d.anyNameAllowed = true
	return d
}

// WithNamePattern matches resource name against a regex pattern.
func (d *DownstreamResourceExpectation) WithNamePattern(pattern string) *DownstreamResourceExpectation {
	d.namePattern = regexp.MustCompile(pattern)
	return d
}

// AndXR returns the parent XRImpactExpectation to chain more downstream resources.
func (d *DownstreamResourceExpectation) AndXR() *XRImpactExpectation {
	return d.parent
}

// AndComp returns the parent CompositionExpectation to chain more XR impacts.
func (x *XRImpactExpectation) AndComp() *CompositionExpectation {
	return x.parent
}

// And returns the parent ExpectedCompDiff to chain more compositions.
func (c *CompositionExpectation) And() *ExpectedCompDiff {
	return c.parent
}

// AssertStructuredCompDiff compares actual JSON output against expected.
func AssertStructuredCompDiff(t *testing.T, jsonOutput string, expected *ExpectedCompDiff) {
	t.Helper()

	output, err := ParseStructuredCompOutput(jsonOutput)
	if err != nil {
		t.Fatalf("Failed to parse structured comp output: %v\nOutput was:\n%s", err, jsonOutput)
	}

	// Check each composition expectation
	for _, expectComp := range expected.compositions {
		found := findMatchingComposition(output.Compositions, expectComp.name)
		if found == nil {
			actualComps := make([]string, 0, len(output.Compositions))
			for _, c := range output.Compositions {
				actualComps = append(actualComps, c.Name)
			}
			t.Errorf("Expected composition %s not found in output. Actual compositions: %v",
				expectComp.name, actualComps)
			continue
		}

		// Check affected resources counts
		if expectComp.affectedTotal != nil && found.AffectedResources.Total != *expectComp.affectedTotal {
			t.Errorf("Composition %s: AffectedResources.Total: expected %d, got %d",
				expectComp.name, *expectComp.affectedTotal, found.AffectedResources.Total)
		}
		if expectComp.affectedWithChanges != nil && found.AffectedResources.WithChanges != *expectComp.affectedWithChanges {
			t.Errorf("Composition %s: AffectedResources.WithChanges: expected %d, got %d",
				expectComp.name, *expectComp.affectedWithChanges, found.AffectedResources.WithChanges)
		}
		if expectComp.affectedUnchanged != nil && found.AffectedResources.Unchanged != *expectComp.affectedUnchanged {
			t.Errorf("Composition %s: AffectedResources.Unchanged: expected %d, got %d",
				expectComp.name, *expectComp.affectedUnchanged, found.AffectedResources.Unchanged)
		}
		if expectComp.affectedWithErrors != nil && found.AffectedResources.WithErrors != *expectComp.affectedWithErrors {
			t.Errorf("Composition %s: AffectedResources.WithErrors: expected %d, got %d",
				expectComp.name, *expectComp.affectedWithErrors, found.AffectedResources.WithErrors)
		}

		// Check composition changes
		if expectComp.compositionChangeType != "" {
			if found.CompositionChanges == nil {
				t.Errorf("Composition %s: expected composition changes but none found", expectComp.name)
			} else {
				if found.CompositionChanges.Type != expectComp.compositionChangeType {
					t.Errorf("Composition %s: expected change type %s, got %s",
						expectComp.name, expectComp.compositionChangeType, found.CompositionChanges.Type)
				}

				// Check composition field changes
				for path, expected := range expectComp.compositionFieldChanges {
					oldVal := getFieldFromDiff(found.CompositionChanges.Diff, "old", path)
					newVal := getFieldFromDiff(found.CompositionChanges.Diff, "new", path)

					if !valuesEqual(oldVal, expected[0]) {
						t.Errorf("Composition %s: field %s old value: expected %v, got %v",
							expectComp.name, path, expected[0], oldVal)
					}
					if !valuesEqual(newVal, expected[1]) {
						t.Errorf("Composition %s: field %s new value: expected %v, got %v",
							expectComp.name, path, expected[1], newVal)
					}
				}
			}
		}

		// Check XR impacts
		for _, expectXR := range expectComp.xrImpacts {
			foundXR := findMatchingXRImpact(found.ImpactAnalysis, expectXR)
			if foundXR == nil {
				actualXRs := make([]string, 0, len(found.ImpactAnalysis))
				for _, x := range found.ImpactAnalysis {
					actualXRs = append(actualXRs, fmt.Sprintf("%s/%s (ns=%s, status=%s)", x.Kind, x.Name, x.Namespace, x.Status))
				}
				t.Errorf("Composition %s: Expected XR impact %s/%s (ns=%s) not found. Actual XRs: %v",
					expectComp.name, expectXR.kind, expectXR.name, expectXR.namespace, actualXRs)
				continue
			}

			// Check status
			if foundXR.Status != expectXR.status {
				t.Errorf("Composition %s: XR %s/%s: expected status %s, got %s",
					expectComp.name, expectXR.kind, expectXR.name, expectXR.status, foundXR.Status)
			}

			// Check downstream summary if specified
			if expectXR.downstreamSummary != nil && foundXR.DownstreamChanges != nil {
				if foundXR.DownstreamChanges.Summary.Added != expectXR.downstreamSummary.added {
					t.Errorf("Composition %s: XR %s/%s: DownstreamChanges.Summary.Added: expected %d, got %d",
						expectComp.name, expectXR.kind, expectXR.name, expectXR.downstreamSummary.added, foundXR.DownstreamChanges.Summary.Added)
				}
				if foundXR.DownstreamChanges.Summary.Modified != expectXR.downstreamSummary.modified {
					t.Errorf("Composition %s: XR %s/%s: DownstreamChanges.Summary.Modified: expected %d, got %d",
						expectComp.name, expectXR.kind, expectXR.name, expectXR.downstreamSummary.modified, foundXR.DownstreamChanges.Summary.Modified)
				}
				if foundXR.DownstreamChanges.Summary.Removed != expectXR.downstreamSummary.removed {
					t.Errorf("Composition %s: XR %s/%s: DownstreamChanges.Summary.Removed: expected %d, got %d",
						expectComp.name, expectXR.kind, expectXR.name, expectXR.downstreamSummary.removed, foundXR.DownstreamChanges.Summary.Removed)
				}
			}

			// Check downstream resources with field-level assertions
			for _, expectRes := range expectXR.downstreamResources {
				if foundXR.DownstreamChanges == nil {
					t.Errorf("Composition %s: XR %s/%s: expected downstream resource %s/%s but no downstream changes present",
						expectComp.name, expectXR.kind, expectXR.name, expectRes.kind, expectRes.name)
					continue
				}

				foundRes := findMatchingDownstreamChange(foundXR.DownstreamChanges.Changes, expectRes)
				if foundRes == nil {
					actualRes := make([]string, 0, len(foundXR.DownstreamChanges.Changes))
					for _, c := range foundXR.DownstreamChanges.Changes {
						actualRes = append(actualRes, fmt.Sprintf("%s/%s (type=%s)", c.Kind, c.Name, c.Type))
					}
					t.Errorf("Composition %s: XR %s/%s: expected downstream resource %s/%s not found. Actual: %v",
						expectComp.name, expectXR.kind, expectXR.name, expectRes.kind, expectRes.name, actualRes)
					continue
				}

				// Check change type
				if foundRes.Type != expectRes.changeType {
					t.Errorf("Composition %s: XR %s/%s: downstream %s/%s: expected type %s, got %s",
						expectComp.name, expectXR.kind, expectXR.name, expectRes.kind, expectRes.name, expectRes.changeType, foundRes.Type)
				}

				// Check field changes (for modified resources)
				for path, expected := range expectRes.fieldChanges {
					oldVal, newVal := expected[0], expected[1]
					assertDownstreamFieldChange(t, expectComp.name, expectXR, expectRes, foundRes, path, oldVal, newVal)
				}

				// Check field values (for added/removed resources)
				for path, expectedVal := range expectRes.fieldValues {
					assertDownstreamFieldValue(t, expectComp.name, expectXR, expectRes, foundRes, path, expectedVal)
				}

				// Check field value patterns
				for path, pattern := range expectRes.fieldValuePatterns {
					assertDownstreamFieldValuePattern(t, expectComp.name, expectXR, expectRes, foundRes, path, pattern)
				}
			}
		}
	}
}

// findMatchingComposition finds a composition by name.
func findMatchingComposition(comps []CompositionDiffJSON, name string) *CompositionDiffJSON {
	for i := range comps {
		if comps[i].Name == name {
			return &comps[i]
		}
	}
	return nil
}

// findMatchingXRImpact finds an XR impact that matches the expectation.
func findMatchingXRImpact(impacts []XRImpactJSON, expect *XRImpactExpectation) *XRImpactJSON {
	for i := range impacts {
		impact := &impacts[i]
		if impact.Kind != expect.kind {
			continue
		}
		if expect.namespace != "" && impact.Namespace != expect.namespace {
			continue
		}

		// Check name matching
		if expect.anyNameAllowed {
			return impact
		}
		if impact.Name == expect.name {
			return impact
		}
	}
	return nil
}

// findMatchingDownstreamChange finds a downstream change that matches the expectation.
func findMatchingDownstreamChange(changes []ChangeDetail, expect *DownstreamResourceExpectation) *ChangeDetail {
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

// assertDownstreamFieldChange asserts that a field changed from old to new value in a modified downstream resource.
func assertDownstreamFieldChange(t *testing.T, compName string, expectXR *XRImpactExpectation, expectRes *DownstreamResourceExpectation, foundRes *ChangeDetail, path string, expectedOld, expectedNew any) {
	t.Helper()

	// For modified resources, the diff structure has "old" and "new" keys
	actualOld := getFieldFromDiff(foundRes.Diff, "old", path)
	actualNew := getFieldFromDiff(foundRes.Diff, "new", path)

	if !valuesEqual(actualOld, expectedOld) {
		t.Errorf("Composition %s: XR %s/%s: downstream %s/%s: field %s old value: expected %v, got %v",
			compName, expectXR.kind, expectXR.name, expectRes.kind, foundRes.Name, path, expectedOld, actualOld)
	}
	if !valuesEqual(actualNew, expectedNew) {
		t.Errorf("Composition %s: XR %s/%s: downstream %s/%s: field %s new value: expected %v, got %v",
			compName, expectXR.kind, expectXR.name, expectRes.kind, foundRes.Name, path, expectedNew, actualNew)
	}
}

// assertDownstreamFieldValue asserts that a field has the expected value in an added/removed downstream resource.
func assertDownstreamFieldValue(t *testing.T, compName string, expectXR *XRImpactExpectation, expectRes *DownstreamResourceExpectation, foundRes *ChangeDetail, path string, expectedValue any) {
	t.Helper()

	// For added/removed resources, the value is in "spec" key
	actualValue := getFieldFromDiff(foundRes.Diff, expectRes.changeType, path)

	if !valuesEqual(actualValue, expectedValue) {
		t.Errorf("Composition %s: XR %s/%s: downstream %s/%s: field %s: expected %v, got %v",
			compName, expectXR.kind, expectXR.name, expectRes.kind, foundRes.Name, path, expectedValue, actualValue)
	}
}

// assertDownstreamFieldValuePattern asserts that a field value matches a regex pattern.
func assertDownstreamFieldValuePattern(t *testing.T, compName string, expectXR *XRImpactExpectation, expectRes *DownstreamResourceExpectation, foundRes *ChangeDetail, path string, pattern *regexp.Regexp) {
	t.Helper()

	actualValue := getNewFieldValue(foundRes.Diff, expectRes.changeType, path)
	actualStr := fmt.Sprintf("%v", actualValue)
	if !pattern.MatchString(actualStr) {
		t.Errorf("Composition %s: XR %s/%s: downstream %s/%s: field %s value %q does not match pattern %q",
			compName, expectXR.kind, expectXR.name, expectRes.kind, foundRes.Name, path, actualStr, pattern.String())
	}
}
