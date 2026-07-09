package renderer

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	sigsyaml "sigs.k8s.io/yaml"
)

// testDiffFixture defines a reusable test case with input diffs and expected output.
type testDiffFixture struct {
	name            string
	diffs           map[string]*dt.ResourceDiff
	errs            []dt.OutputError
	expectedSummary Summary
	expectedChanges int
	// Optional: additional validation beyond summary/changes count
	validate func(t *testing.T, output *StructuredDiffOutput)
}

// sharedDiffFixtures returns test fixtures that should be run through both JSON and YAML renderers.
func sharedDiffFixtures() []testDiffFixture {
	// Create reusable test resources
	addedResource := tu.NewResource("nop.crossplane.io/v1alpha1", "NopResource", "new-resource").
		InNamespace("default").
		WithSpec(map[string]any{
			"forProvider": map[string]any{
				"field": "value",
			},
		}).
		Build()

	modifiedCurrentResource := tu.NewResource("example.org/v1alpha1", "XExample", "modified-resource").
		InNamespace("production").
		WithSpec(map[string]any{
			"oldValue": "something",
		}).
		Build()

	modifiedDesiredResource := tu.NewResource("example.org/v1alpha1", "XExample", "modified-resource").
		InNamespace("production").
		WithSpec(map[string]any{
			"newValue": "something-else",
		}).
		Build()

	removedResource := tu.NewResource("example.org/v1alpha1", "XNopResource", "removed-resource").
		WithSpec(map[string]any{
			"coolField": "goodbye",
		}).
		Build()

	return []testDiffFixture{
		{
			name: "AllDiffTypes",
			diffs: map[string]*dt.ResourceDiff{
				"added": {
					DiffType:     dt.DiffTypeAdded,
					ResourceName: "new-resource",
					Namespace:    "default",
					Gvk: schema.GroupVersionKind{
						Group:   "nop.crossplane.io",
						Version: "v1alpha1",
						Kind:    "NopResource",
					},
					Desired: dt.ResourceViews{Raw: addedResource, Clean: addedResource},
				},
				"modified": {
					DiffType:     dt.DiffTypeModified,
					ResourceName: "modified-resource",
					Namespace:    "production",
					Gvk: schema.GroupVersionKind{
						Group:   "example.org",
						Version: "v1alpha1",
						Kind:    "XExample",
					},
					Current: dt.ResourceViews{Raw: modifiedCurrentResource, Clean: modifiedCurrentResource},
					Desired: dt.ResourceViews{Raw: modifiedDesiredResource, Clean: modifiedDesiredResource},
				},
				"removed": {
					DiffType:     dt.DiffTypeRemoved,
					ResourceName: "removed-resource",
					Gvk: schema.GroupVersionKind{
						Group:   "example.org",
						Version: "v1alpha1",
						Kind:    "XNopResource",
					},
					Current: dt.ResourceViews{Raw: removedResource, Clean: removedResource},
				},
				"equal": {
					DiffType:     dt.DiffTypeEqual,
					ResourceName: "unchanged-resource",
					Gvk: schema.GroupVersionKind{
						Kind: "NopResource",
					},
				},
			},
			expectedSummary: Summary{Added: 1, Modified: 1, Removed: 1},
			expectedChanges: 3, // equal should be excluded
			validate: func(t *testing.T, output *StructuredDiffOutput) {
				t.Helper()

				// Find and verify added resource
				var addedChange *ChangeDetail

				for i := range output.Changes {
					if output.Changes[i].Type == dt.DiffTypeWordAdded {
						addedChange = &output.Changes[i]
						break
					}
				}

				if addedChange == nil {
					t.Fatal("Added resource not found in changes")
				}

				if addedChange.Kind != "NopResource" {
					t.Errorf("Expected Kind 'NopResource', got '%s'", addedChange.Kind)
				}

				if addedChange.Name != "new-resource" {
					t.Errorf("Expected Name 'new-resource', got '%s'", addedChange.Name)
				}

				if addedChange.Namespace != "default" {
					t.Errorf("Expected Namespace 'default', got '%s'", addedChange.Namespace)
				}
			},
		},
		{
			name:            "EmptyDiffs",
			diffs:           map[string]*dt.ResourceDiff{},
			expectedSummary: Summary{Added: 0, Modified: 0, Removed: 0},
			expectedChanges: 0,
		},
		{
			name: "OnlyEqualDiffs",
			diffs: map[string]*dt.ResourceDiff{
				"equal1": {
					DiffType:     dt.DiffTypeEqual,
					ResourceName: "resource-1",
					Gvk:          schema.GroupVersionKind{Kind: "NopResource"},
				},
				"equal2": {
					DiffType:     dt.DiffTypeEqual,
					ResourceName: "resource-2",
					Gvk:          schema.GroupVersionKind{Kind: "NopResource"},
				},
			},
			expectedSummary: Summary{Added: 0, Modified: 0, Removed: 0},
			expectedChanges: 0, // equal diffs should be excluded
		},
		{
			name:  "WithErrors",
			diffs: map[string]*dt.ResourceDiff{},
			errs: []dt.OutputError{
				{ResourceID: "example.org/v1/XResource/my-xr", Message: "failed to render XR: missing composition"},
				{Message: "cluster connection timeout"},
			},
			expectedSummary: Summary{Added: 0, Modified: 0, Removed: 0},
			expectedChanges: 0,
			validate: func(t *testing.T, output *StructuredDiffOutput) {
				t.Helper()

				if len(output.Errors) != 2 {
					t.Fatalf("Expected 2 errors, got %d", len(output.Errors))
				}

				if output.Errors[0].ResourceID != "example.org/v1/XResource/my-xr" {
					t.Errorf("Expected first error ResourceID 'example.org/v1/XResource/my-xr', got '%s'", output.Errors[0].ResourceID)
				}

				if output.Errors[1].Message != "cluster connection timeout" {
					t.Errorf("Expected second error message 'cluster connection timeout', got '%s'", output.Errors[1].Message)
				}
			},
		},
	}
}

func TestStructuredDiffRenderer_RenderDiffs(t *testing.T) {
	formats := []OutputFormat{OutputFormatJSON, OutputFormatYAML}
	fixtures := sharedDiffFixtures()

	for _, format := range formats {
		for _, fixture := range fixtures {
			testName := string(format) + "/" + fixture.name
			t.Run(testName, func(t *testing.T) {
				logger := tu.TestLogger(t, false)

				var buf bytes.Buffer

				opts := DefaultDiffOptions()
				opts.Format = format
				opts.Stdout = &buf
				opts.Stderr = &bytes.Buffer{} // discard stderr for these tests

				renderer := NewStructuredDiffRenderer(logger, opts)

				err := renderer.RenderDiffs(fixture.diffs, fixture.errs)
				if err != nil {
					t.Fatalf("RenderDiffs() failed: %v", err)
				}

				// Parse the output based on format
				var output StructuredDiffOutput

				switch format {
				case OutputFormatJSON:
					if err := json.Unmarshal(buf.Bytes(), &output); err != nil {
						t.Fatalf("Failed to parse JSON output: %v\nOutput: %s", err, buf.String())
					}
				case OutputFormatYAML:
					if err := sigsyaml.Unmarshal(buf.Bytes(), &output); err != nil {
						t.Fatalf("Failed to parse YAML output: %v\nOutput: %s", err, buf.String())
					}
				case OutputFormatDiff:
					t.Fatal("OutputFormatDiff should not be used with StructuredDiffRenderer")
				}

				// Verify summary
				if output.Summary.Added != fixture.expectedSummary.Added {
					t.Errorf("Summary.Added = %d, want %d", output.Summary.Added, fixture.expectedSummary.Added)
				}

				if output.Summary.Modified != fixture.expectedSummary.Modified {
					t.Errorf("Summary.Modified = %d, want %d", output.Summary.Modified, fixture.expectedSummary.Modified)
				}

				if output.Summary.Removed != fixture.expectedSummary.Removed {
					t.Errorf("Summary.Removed = %d, want %d", output.Summary.Removed, fixture.expectedSummary.Removed)
				}

				// Verify change count
				if len(output.Changes) != fixture.expectedChanges {
					t.Errorf("len(Changes) = %d, want %d", len(output.Changes), fixture.expectedChanges)
				}

				// Verify errors round-trip if present
				if len(fixture.errs) > 0 {
					if diff := cmp.Diff(fixture.errs, output.Errors); diff != "" {
						t.Errorf("Errors mismatch (-want +got):\n%s", diff)
					}
				}

				// Run additional validation if provided
				if fixture.validate != nil {
					fixture.validate(t, &output)
				}
			})
		}
	}
}

// TestStructuredDiffRenderer_RenderDiffs_ErrorsToStderr verifies that errors are
// written to stderr for human visibility in addition to being included in the
// structured output for machine parsing.
func TestStructuredDiffRenderer_RenderDiffs_ErrorsToStderr(t *testing.T) {
	errs := []dt.OutputError{
		{ResourceID: "example.org/v1/XResource/my-xr", Message: "failed to render XR: missing composition"},
		{Message: "cluster connection timeout"},
	}

	for _, format := range []OutputFormat{OutputFormatJSON, OutputFormatYAML} {
		t.Run(string(format), func(t *testing.T) {
			logger := tu.TestLogger(t, false)

			var (
				stdout bytes.Buffer
				stderr bytes.Buffer
			)

			opts := DefaultDiffOptions()
			opts.Format = format
			opts.Stdout = &stdout
			opts.Stderr = &stderr

			renderer := NewStructuredDiffRenderer(logger, opts)

			err := renderer.RenderDiffs(map[string]*dt.ResourceDiff{}, errs)
			if err != nil {
				t.Fatalf("RenderDiffs() failed: %v", err)
			}

			// Verify errors are in stderr
			stderrOut := stderr.String()
			for _, e := range errs {
				if !strings.Contains(stderrOut, e.FormatError()) {
					t.Errorf("Expected stderr to contain %q, got: %q", e.FormatError(), stderrOut)
				}
			}

			// Verify errors are ALSO in structured output (stdout)
			var output StructuredDiffOutput

			switch format {
			case OutputFormatJSON:
				if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
					t.Fatalf("Failed to parse JSON output: %v\nOutput: %s", err, stdout.String())
				}
			case OutputFormatYAML:
				if err := sigsyaml.Unmarshal(stdout.Bytes(), &output); err != nil {
					t.Fatalf("Failed to parse YAML output: %v\nOutput: %s", err, stdout.String())
				}
			case OutputFormatDiff:
				t.Fatal("OutputFormatDiff should not be used with StructuredDiffRenderer")
			}

			if diff := cmp.Diff(errs, output.Errors); diff != "" {
				t.Errorf("Structured output errors mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestStructuredDiffRenderer_RespectsIgnorePaths verifies that --ignore-paths
// and the unconditional cleanup performed for the human diff are also honored
// when rendering structured (JSON/YAML) output.
//
// Each case runs GenerateDiffWithOptions to classify the diff (matching real
// CLI flow) and then renders through the structured renderer, then asserts on
// the parsed structured output.
//
// See: .requirements/20260709T200044Z_resourceviews_dedup/REQUIREMENTS.md.
func TestStructuredDiffRenderer_RespectsIgnorePaths(t *testing.T) {
	const (
		ignoredAnnotation = "argocd.argoproj.io/tracking-id"
		ignoredLabel      = "argocd.argoproj.io/instance"
	)

	ignorePaths := []string{
		"metadata.annotations[" + ignoredAnnotation + "]",
		"metadata.labels[" + ignoredLabel + "]",
	}

	// xExample returns a builder for a namespaced XExample resource named "r1".
	// Callers chain expressive builder methods (WithSpecField, WithAnnotations,
	// WithServerMetadata, WithStatus, WithOwnerReference, …) to add exactly the
	// fields the case needs, then call Build().
	xExample := func() *tu.ResourceBuilder {
		return tu.NewResource("example.org/v1alpha1", "XExample", "r1").
			InNamespace("default")
	}

	// cleanSpec is the object body we expect the renderer to emit for a resource
	// whose only surviving content is its identity plus spec.configData — i.e.
	// after cleanup has stripped ignored annotations/labels and all server-side
	// fields. Built with the same builder used for inputs, minus the noise, so
	// the expectation reads as "this is the shape that should come out".
	cleanSpec := func(configData string) map[string]any {
		return xExample().WithSpecField("configData", configData).Build().Object
	}

	cases := []struct {
		name        string
		current     *un.Unstructured // pass nil for Added
		desired     *un.Unstructured // pass nil for Removed
		ignorePaths []string
		wantSummary Summary
		wantChanges int
		// wantDiffDetail is the expected Changes[0].Diff payload, as decoded from
		// the rendered JSON/YAML (so it holds nested map[string]any structures).
		// Every leaf value in these fixtures is a string, so the comparison isn't
		// affected by JSON's number decoding (all numbers would decode to
		// float64). Left nil when wantChanges is 0.
		wantDiffDetail map[string]any
	}{
		{
			// AC5.2: only user-supplied ignored path differs -> classified Equal.
			name: "OnlyIgnoredAnnotation_UnchangedInSummary",
			current: xExample().WithSpecField("configData", "same").
				WithAnnotations(map[string]string{ignoredAnnotation: "id-old"}).Build(),
			desired: xExample().WithSpecField("configData", "same").
				WithAnnotations(map[string]string{ignoredAnnotation: "id-new"}).Build(),
			ignorePaths: ignorePaths,
			wantSummary: Summary{},
			wantChanges: 0,
		},
		{
			// AC5.1 / AC2.2: only ownerReferences differ -> classified Equal
			// via unconditional cleanup, without any user --ignore-paths.
			name: "OnlyOwnerReferences_UnchangedInSummary",
			current: xExample().WithSpecField("configData", "same").
				WithOwnerReference("Owner", "old", "v1", "u1").Build(),
			desired: xExample().WithSpecField("configData", "same").
				WithOwnerReference("Owner", "new", "v1", "u2").Build(),
			wantSummary: Summary{},
			wantChanges: 0,
		},
		{
			// AC1.1 / AC5.3: mixed ignored + non-ignored change. Modified count
			// increments once; diff.old/new carry only the cleaned bodies — the
			// ignored annotation and label are absent, the spec change survives.
			name: "IgnoredPlusNonIgnored_CountOneAndIgnoredStripped",
			current: xExample().WithSpecField("configData", "old").
				WithAnnotations(map[string]string{ignoredAnnotation: "id-old"}).
				WithLabels(map[string]string{ignoredLabel: "app-old"}).Build(),
			desired: xExample().WithSpecField("configData", "new").
				WithAnnotations(map[string]string{ignoredAnnotation: "id-new"}).
				WithLabels(map[string]string{ignoredLabel: "app-new"}).Build(),
			ignorePaths: ignorePaths,
			wantSummary: Summary{Modified: 1},
			wantChanges: 1,
			wantDiffDetail: map[string]any{
				dt.DiffKeyOld: cleanSpec("old"),
				dt.DiffKeyNew: cleanSpec("new"),
			},
		},
		{
			// AC2.1: unconditional-cleanup fields (server metadata, managedFields,
			// ownerReferences, status) must not leak into the JSON diff even
			// without user-supplied --ignore-paths.
			name: "ServerSideFieldsStripped",
			current: xExample().WithSpecField("configData", "old").
				WithServerMetadata().
				WithFieldManagers("kubectl").
				WithOwnerReference("Owner", "o", "v1", "u").
				WithStatus(map[string]any{"phase": "Ready"}).Build(),
			desired: xExample().WithSpecField("configData", "new").
				WithServerMetadata().
				WithFieldManagers("kubectl").
				WithOwnerReference("Owner", "o", "v1", "u").
				WithStatus(map[string]any{"phase": "Ready"}).Build(),
			wantSummary: Summary{Modified: 1},
			wantChanges: 1,
			wantDiffDetail: map[string]any{
				dt.DiffKeyOld: cleanSpec("old"),
				dt.DiffKeyNew: cleanSpec("new"),
			},
		},
		{
			// AC3.1: Added resource — diff.spec carries only the cleaned body,
			// no ignored annotation and no server-side fields.
			name:    "AddedResource_IgnoresPathsInSpec",
			current: nil,
			desired: xExample().WithSpecField("configData", "new").
				WithAnnotations(map[string]string{ignoredAnnotation: "id-new"}).
				WithServerMetadata().
				WithFieldManagers("kubectl").Build(),
			ignorePaths: ignorePaths,
			wantSummary: Summary{Added: 1},
			wantChanges: 1,
			wantDiffDetail: map[string]any{
				dt.DiffKeySpec: cleanSpec("new"),
			},
		},
		{
			// AC4.1: Removed resource — diff.spec carries only the cleaned body,
			// no ignored annotation and no ownerReferences.
			name: "RemovedResource_IgnoresPathsInSpec",
			current: xExample().WithSpecField("configData", "old").
				WithAnnotations(map[string]string{ignoredAnnotation: "id-old"}).
				WithServerMetadata().
				WithOwnerReference("Owner", "o", "v1", "u").Build(),
			desired:     nil,
			ignorePaths: ignorePaths,
			wantSummary: Summary{Removed: 1},
			wantChanges: 1,
			wantDiffDetail: map[string]any{
				dt.DiffKeySpec: cleanSpec("old"),
			},
		},
	}

	// R6: run every case through both JSON and YAML.
	formats := []OutputFormat{OutputFormatJSON, OutputFormatYAML}

	for _, format := range formats {
		for _, tc := range cases {
			t.Run(string(format)+"/"+tc.name, func(t *testing.T) {
				logger := tu.TestLogger(t, false)

				// Run the same classify-then-render pipeline the CLI uses, so
				// we exercise classification + rendering together rather than
				// only the renderer. Cleanup (ignore-paths + server-side fields)
				// happens here, during generation, and is stored on the diff's
				// clean views; the renderer just emits them.
				//
				// Note: this uses tc.ignorePaths verbatim and does NOT prepend
				// the CLI's built-in default ignore path
				// (metadata.annotations[kubectl.kubernetes.io/last-applied-configuration],
				// added in defaultProcessorOptions, cmd_utils.go). That default
				// wiring is covered end-to-end by the IgnorePathsArgoCD
				// integration test in diff_integration_test.go.
				diffOpts := DefaultDiffOptions()
				diffOpts.IgnorePaths = tc.ignorePaths

				rd, err := GenerateDiffWithOptions(context.Background(), tc.current, tc.desired, logger, diffOpts)
				if err != nil {
					t.Fatalf("GenerateDiffWithOptions: %v", err)
				}

				var buf bytes.Buffer

				// The renderer no longer performs cleanup, so it needs no
				// IgnorePaths — the diff already carries cleaned views.
				renderOpts := DefaultDiffOptions()
				renderOpts.Format = format
				renderOpts.Stdout = &buf
				renderOpts.Stderr = &bytes.Buffer{}

				r := NewStructuredDiffRenderer(logger, renderOpts)
				if err := r.RenderDiffs(map[string]*dt.ResourceDiff{"r1": rd}, nil); err != nil {
					t.Fatalf("RenderDiffs() failed: %v", err)
				}

				var output StructuredDiffOutput

				switch format {
				case OutputFormatJSON:
					if err := json.Unmarshal(buf.Bytes(), &output); err != nil {
						t.Fatalf("json.Unmarshal: %v\noutput: %s", err, buf.String())
					}
				case OutputFormatYAML:
					if err := sigsyaml.Unmarshal(buf.Bytes(), &output); err != nil {
						t.Fatalf("yaml.Unmarshal: %v\noutput: %s", err, buf.String())
					}
				case OutputFormatDiff:
					t.Fatal("diff format not supported by structured renderer")
				}

				if diff := cmp.Diff(tc.wantSummary, output.Summary); diff != "" {
					t.Errorf("summary mismatch (-want +got):\n%s", diff)
				}

				if len(output.Changes) != tc.wantChanges {
					t.Errorf("len(Changes) = %d, want %d\noutput: %s", len(output.Changes), tc.wantChanges, buf.String())
				}

				// When a change is expected, the emitted diff detail must match
				// the cleaned bodies exactly — this catches both leaked ignored/
				// server-side fields and any missing non-ignored content.
				if tc.wantDiffDetail != nil {
					if len(output.Changes) == 0 {
						t.Fatalf("expected one change with diff detail, got none\noutput: %s", buf.String())
					}

					if diff := cmp.Diff(tc.wantDiffDetail, output.Changes[0].Diff); diff != "" {
						t.Errorf("diff detail mismatch (-want +got):\n%s", diff)
					}
				}
			})
		}
	}
}
