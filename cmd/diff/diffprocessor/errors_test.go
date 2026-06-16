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
	"errors"
	"fmt"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	pkgvalidate "github.com/crossplane/cli/v2/pkg/validate"
	gcmp "github.com/google/go-cmp/cmp"

	xperrors "github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

func TestSchemaValidationError_Error(t *testing.T) {
	tests := map[string]struct {
		err  *SchemaValidationError
		want string
	}{
		"WithResourceID": {
			err: &SchemaValidationError{
				ResourceID: "MyKind/my-resource",
				Message:    "field validation failed",
			},
			want: "schema validation error for MyKind/my-resource: field validation failed",
		},
		"WithoutResourceID": {
			err: &SchemaValidationError{
				Message: "general validation failed",
			},
			want: "general validation failed",
		},
		"WithWrappedError": {
			err: &SchemaValidationError{
				ResourceID: "MyKind/my-resource",
				Message:    "schema validation failed",
				Err:        errors.New("underlying error"),
			},
			want: "schema validation error for MyKind/my-resource: schema validation failed",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := tc.err.Error()
			if diff := gcmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Error() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSchemaValidationError_Unwrap(t *testing.T) {
	wrappedErr := errors.New("underlying error")
	err := &SchemaValidationError{
		Message: "wrapper",
		Err:     wrappedErr,
	}

	got := err.Unwrap()
	if !errors.Is(got, wrappedErr) {
		t.Errorf("Unwrap() = %v, want %v", got, wrappedErr)
	}
}

func TestIsSchemaValidationError(t *testing.T) {
	tests := map[string]struct {
		err  error
		want bool
	}{
		"DirectSchemaValidationError": {
			err:  &SchemaValidationError{Message: "test"},
			want: true,
		},
		"WrappedSchemaValidationError": {
			err:  errors.Join(errors.New("outer"), &SchemaValidationError{Message: "inner"}),
			want: true,
		},
		"NonSchemaValidationError": {
			err:  errors.New("regular error"),
			want: false,
		},
		"NilError": {
			err:  nil,
			want: false,
		},
		"DeeplyWrappedSchemaValidationError": {
			err: errors.Join(
				errors.New("level1"),
				errors.Join(
					errors.New("level2"),
					&SchemaValidationError{Message: "deep"},
				),
			),
			want: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := IsSchemaValidationError(tc.err)
			if got != tc.want {
				t.Errorf("IsSchemaValidationError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewSchemaValidationError(t *testing.T) {
	wrappedErr := errors.New("wrapped")
	err := NewSchemaValidationError("Kind/name", "message", wrappedErr)

	if err.ResourceID != "Kind/name" {
		t.Errorf("ResourceID = %v, want Kind/name", err.ResourceID)
	}

	if err.Message != "message" {
		t.Errorf("Message = %v, want message", err.Message)
	}

	if !errors.Is(err.Err, wrappedErr) {
		t.Errorf("Err = %v, want %v", err.Err, wrappedErr)
	}

	if err.Result != nil {
		t.Errorf("Result = %v, want nil for the basic constructor", err.Result)
	}
}

func TestSchemaValidationError_WithResult(t *testing.T) {
	result := &pkgvalidate.ValidationResult{
		Resources: []pkgvalidate.ResourceValidationResult{{
			APIVersion: "example.org/v1",
			Kind:       "XR",
			Name:       "x",
			Status:     pkgvalidate.ValidationStatusInvalid,
		}},
	}

	err := NewSchemaValidationError("", "msg", errors.New("inner")).WithResult(result)
	if err.Result != result {
		t.Errorf("Result = %v, want %v", err.Result, result)
	}
}

func TestNewOutputError(t *testing.T) {
	tests := map[string]struct {
		reason     string
		resourceID string
		err        error
		want       dt.OutputError
	}{
		"NonValidationError_NoFailuresAttached": {
			reason:     "A plain error has no structured slot to populate; ValidationFailures stays nil and only Message is set.",
			resourceID: "XR/my-xr",
			err:        errors.New("kube unreachable"),
			want: dt.OutputError{
				ResourceID: "XR/my-xr",
				Message:    "kube unreachable",
			},
		},
		"SchemaValidationError_WithoutResult_NoFailuresAttached": {
			reason:     "A SchemaValidationError without a Result (e.g. scope-validation path) contributes only Message; the typed slot stays nil so consumers can distinguish 'no failures' from 'no structured detail available'.",
			resourceID: "XR/my-xr",
			err:        NewSchemaValidationError("", "scope failure", errors.New("inner")),
			want: dt.OutputError{
				ResourceID: "XR/my-xr",
				Message:    "scope failure",
			},
		},
		"SchemaValidationError_WithResult_ExposesTypedFailures": {
			reason:     "A SchemaValidationError carrying a Result populates ValidationFailures with the converted per-resource breakdown.",
			resourceID: "XR/my-xr",
			err: NewSchemaValidationError("", "msg", errors.New("inner")).WithResult(
				&pkgvalidate.ValidationResult{
					Resources: []pkgvalidate.ResourceValidationResult{{
						APIVersion: "example.org/v1",
						Kind:       "XR",
						Name:       "my-xr",
						Status:     pkgvalidate.ValidationStatusInvalid,
						Errors: []pkgvalidate.FieldValidationError{{
							Type:    pkgvalidate.FieldErrorTypeSchema,
							Field:   "spec.region",
							Message: "spec.region: Required value",
						}},
					}},
				}),
			want: dt.OutputError{
				ResourceID: "XR/my-xr",
				Message:    "msg",
				ValidationFailures: []dt.ResourceValidationFailure{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     "invalid",
					Errors: []dt.FieldValidationError{{
						Type:    "schema",
						Field:   "spec.region",
						Message: "spec.region: Required value",
					}},
				}},
			},
		},
		"WrappedSchemaValidationError_StillSurfacesFailures": {
			reason:     "errors.As walks the chain, so a SchemaValidationError behind errors.Wrap still has its Result extracted into ValidationFailures.",
			resourceID: "XR/my-xr",
			err: xperrors.Wrap(
				NewSchemaValidationError("", "msg", errors.New("inner")).WithResult(
					&pkgvalidate.ValidationResult{
						Resources: []pkgvalidate.ResourceValidationResult{{
							APIVersion: "other.org/v1",
							Kind:       "Thing",
							Name:       "thing",
							Status:     pkgvalidate.ValidationStatusMissingSchema,
						}},
					}),
				"cannot validate resources"),
			want: dt.OutputError{
				ResourceID: "XR/my-xr",
				Message:    "cannot validate resources: msg",
				ValidationFailures: []dt.ResourceValidationFailure{{
					APIVersion: "other.org/v1",
					Kind:       "Thing",
					Name:       "thing",
					Status:     "missingSchema",
				}},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := NewOutputError(tc.resourceID, tc.err)
			if diff := gcmp.Diff(tc.want, got); diff != "" {
				t.Errorf("\n%s\nNewOutputError() mismatch (-want +got):\n%s", tc.reason, diff)
			}
		})
	}
}

func TestValidationFailuresFromResult(t *testing.T) {
	tests := map[string]struct {
		reason string
		result *pkgvalidate.ValidationResult
		want   []dt.ResourceValidationFailure
	}{
		"NilResultReturnsNil": {
			reason: "A nil ValidationResult yields nil; the converter never invents structure.",
			result: nil,
			want:   nil,
		},
		"OnlyValidResourcesReturnsNil": {
			reason: "ValidationFailures lists 'what failed', not the full audit log; valid resources are filtered out.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "ok",
					Status:     pkgvalidate.ValidationStatusValid,
				}},
			},
			want: nil,
		},
		"DefaultingFailedFiltered": {
			reason: "pkgvalidate.ResultError treats DefaultingFailed as success, so a SchemaValidationError reaching us shouldn't advertise these resources as failures via ValidationFailures.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "x",
					Status:     pkgvalidate.ValidationStatusDefaultingFailed,
					Errors: []pkgvalidate.FieldValidationError{{
						Type:    pkgvalidate.FieldErrorTypeDefaulting,
						Message: "cannot apply defaults",
					}},
				}},
			},
			want: nil,
		},
		"InvalidWithMixedErrorsSuppressesDefaulting": {
			reason: "Mirrors formatValidationErrors: when actionable errors are present, defaulting entries are dropped so the typed and rendered views agree on the failure detail.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Namespace:  "production",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{
						{Type: pkgvalidate.FieldErrorTypeDefaulting, Message: "cannot apply defaults"},
						{Type: pkgvalidate.FieldErrorTypeSchema, Field: "spec.region", Message: "spec.region: Required value"},
					},
				}},
			},
			want: []dt.ResourceValidationFailure{{
				APIVersion: "example.org/v1",
				Kind:       "XR",
				Name:       "my-xr",
				Namespace:  "production",
				Status:     "invalid",
				Errors: []dt.FieldValidationError{{
					Type:    "schema",
					Field:   "spec.region",
					Message: "spec.region: Required value",
				}},
			}},
		},
		"InvalidWithOnlyDefaultingErrorsKeepsThem": {
			reason: "Defensive: upstream's statusFromErrors won't produce Invalid+only-defaulting today, but if it ever did we surface the defaulting entries rather than emit an Invalid row with no errors.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{{
						Type:    pkgvalidate.FieldErrorTypeDefaulting,
						Message: "cannot apply defaults",
					}},
				}},
			},
			want: []dt.ResourceValidationFailure{{
				APIVersion: "example.org/v1",
				Kind:       "XR",
				Name:       "my-xr",
				Status:     "invalid",
				Errors: []dt.FieldValidationError{{
					Type:    "defaulting",
					Message: "cannot apply defaults",
				}},
			}},
		},
		"MissingSchemaSurfacedWithoutErrors": {
			reason: "Missing-schema resources surface with their GVK / name / namespace and Status, but no errors[] (the validator never ran).",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "other.org/v1",
					Kind:       "SomeResource",
					Name:       "thing",
					Status:     pkgvalidate.ValidationStatusMissingSchema,
				}},
			},
			want: []dt.ResourceValidationFailure{{
				APIVersion: "other.org/v1",
				Kind:       "SomeResource",
				Name:       "thing",
				Status:     "missingSchema",
			}},
		},
		"BadValuePropagated": {
			reason: "FieldValidationError.Value travels through unchanged, preserving its Go type, so JSON output renders it without forcing callers to parse it back.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "my-xr",
					Status:     pkgvalidate.ValidationStatusInvalid,
					Errors: []pkgvalidate.FieldValidationError{{
						Type:    pkgvalidate.FieldErrorTypeSchema,
						Field:   "spec.replicas",
						Message: `spec.replicas: Invalid value: "five"`,
						Value:   "five",
					}},
				}},
			},
			want: []dt.ResourceValidationFailure{{
				APIVersion: "example.org/v1",
				Kind:       "XR",
				Name:       "my-xr",
				Status:     "invalid",
				Errors: []dt.FieldValidationError{{
					Type:    "schema",
					Field:   "spec.replicas",
					Message: `spec.replicas: Invalid value: "five"`,
					Value:   "five",
				}},
			}},
		},
		"MultipleResourcesPreserveOrder": {
			reason: "The output preserves input order across resources and skips valid ones, so consumers can rely on the position correspondence between input resources and emitted failures.",
			result: &pkgvalidate.ValidationResult{
				Resources: []pkgvalidate.ResourceValidationResult{
					{
						APIVersion: "example.org/v1",
						Kind:       "XR",
						Name:       "first",
						Status:     pkgvalidate.ValidationStatusInvalid,
						Errors: []pkgvalidate.FieldValidationError{{
							Type:    pkgvalidate.FieldErrorTypeSchema,
							Message: "first error",
						}},
					},
					{
						APIVersion: "example.org/v1",
						Kind:       "XR",
						Name:       "ok-skipped",
						Status:     pkgvalidate.ValidationStatusValid,
					},
					{
						APIVersion: "other.org/v1",
						Kind:       "Thing",
						Name:       "third",
						Status:     pkgvalidate.ValidationStatusMissingSchema,
					},
				},
			},
			want: []dt.ResourceValidationFailure{
				{
					APIVersion: "example.org/v1",
					Kind:       "XR",
					Name:       "first",
					Status:     "invalid",
					Errors: []dt.FieldValidationError{{
						Type:    "schema",
						Message: "first error",
					}},
				},
				{
					APIVersion: "other.org/v1",
					Kind:       "Thing",
					Name:       "third",
					Status:     "missingSchema",
				},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := validationFailuresFromResult(tc.result)
			if diff := gcmp.Diff(tc.want, got); diff != "" {
				t.Errorf("\n%s\nvalidationFailuresFromResult() mismatch (-want +got):\n%s", tc.reason, diff)
			}
		})
	}
}

func TestIsOnlySchemaValidationErrors(t *testing.T) {
	tests := map[string]struct {
		err  error
		want bool
	}{
		"NilError": {
			err:  nil,
			want: true,
		},
		"SingleSchemaValidationError": {
			err:  &SchemaValidationError{Message: "test"},
			want: true,
		},
		"SingleToolError": {
			err:  errors.New("tool error"),
			want: false,
		},
		"MultipleSchemaValidationErrors": {
			err: errors.Join(
				&SchemaValidationError{ResourceID: "Kind1/res1", Message: "error1"},
				&SchemaValidationError{ResourceID: "Kind2/res2", Message: "error2"},
			),
			want: true,
		},
		"MixedErrors_ToolFirst": {
			err: errors.Join(
				errors.New("tool error"),
				&SchemaValidationError{ResourceID: "Kind/res", Message: "validation"},
			),
			want: false,
		},
		"MixedErrors_SchemaFirst": {
			err: errors.Join(
				&SchemaValidationError{ResourceID: "Kind/res", Message: "validation"},
				errors.New("tool error"),
			),
			want: false,
		},
		"NestedSchemaValidationError": {
			err: &SchemaValidationError{
				ResourceID: "Kind/res",
				Message:    "outer",
				Err:        &SchemaValidationError{Message: "inner"},
			},
			want: true,
		},
		"SchemaValidationWrappingUnderlyingError": {
			// A SchemaValidationError wrapping an underlying error (from the validation library)
			// is still a schema validation error as a whole. The wrapped error is just the cause/detail.
			err: &SchemaValidationError{
				ResourceID: "Kind/res",
				Message:    "wrapper",
				Err:        errors.New("underlying validation error"),
			},
			want: true,
		},
		"DeeplyNestedAllSchemaValidation": {
			err: errors.Join(
				&SchemaValidationError{Message: "level1"},
				errors.Join(
					&SchemaValidationError{Message: "level2a"},
					&SchemaValidationError{Message: "level2b"},
				),
			),
			want: true,
		},
		"DeeplyNestedWithToolError": {
			err: errors.Join(
				&SchemaValidationError{Message: "level1"},
				errors.Join(
					&SchemaValidationError{Message: "level2a"},
					errors.New("buried tool error"),
				),
			),
			want: false,
		},
		"PkgErrorsWrappedSchemaValidation": {
			// Simulates errors.Wrap from pkg/errors wrapping a SchemaValidationError
			err: &causedError{
				msg:   "wrapped",
				cause: &SchemaValidationError{Message: "inner"},
			},
			want: true,
		},
		"PkgErrorsWrappedToolError": {
			err: &causedError{
				msg:   "wrapped",
				cause: errors.New("tool error"),
			},
			want: false,
		},
		"PkgErrorsDoubleWrappedSchemaValidation": {
			err: &causedError{
				msg: "outer",
				cause: &causedError{
					msg:   "inner",
					cause: &SchemaValidationError{Message: "validation"},
				},
			},
			want: true,
		},
		"CrossplaneRuntimeWrappedSchemaValidation": {
			// This simulates the actual wrapping pattern used in diff_processor.go:
			// errors.Wrap(schemaValidationError, "cannot validate resources")
			err:  xperrors.Wrap(&SchemaValidationError{Message: "inner"}, "wrapped"),
			want: true,
		},
		"CrossplaneRuntimeDoubleWrappedSchemaValidation": {
			// errors.Wrap wrapping another errors.Wrap wrapping SchemaValidationError
			err:  xperrors.Wrap(xperrors.Wrap(&SchemaValidationError{Message: "inner"}, "inner wrap"), "outer wrap"),
			want: true,
		},
		"CrossplaneRuntimeWrappedToolError": {
			err:  xperrors.Wrap(errors.New("tool error"), "wrapped"),
			want: false,
		},
		"CrossplaneRuntimeJoinedSchemaValidationErrors": {
			// This simulates the actual pattern in PerformDiff where errors are joined
			err: xperrors.Join(
				xperrors.Wrapf(
					xperrors.Wrap(&SchemaValidationError{Message: "validation"}, "cannot validate resources"),
					"unable to process resource %s", "Kind/name",
				),
			),
			want: true,
		},
		"CrossplaneRuntimeJoinedMixedErrors": {
			// Mix of tool error and schema validation error via xperrors.Join
			err: xperrors.Join(
				xperrors.Wrap(errors.New("tool error"), "wrapped"),
				xperrors.Wrap(&SchemaValidationError{Message: "validation"}, "cannot validate resources"),
			),
			want: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := isOnlySchemaValidationErrors(tc.err)
			if got != tc.want {
				t.Errorf("isOnlySchemaValidationErrors() = %v, want %v", got, tc.want)
			}
		})
	}
}

// causedError is a test helper that implements the Cause() interface used by pkg/errors.
type causedError struct {
	msg   string
	cause error
}

func (e *causedError) Error() string { return e.msg }
func (e *causedError) Cause() error  { return e.cause }

func TestDetermineExitCode(t *testing.T) {
	tests := map[string]struct {
		err      error
		hasDiffs bool
		want     int
	}{
		"NoErrorNoDiff": {
			err:      nil,
			hasDiffs: false,
			want:     ExitCodeSuccess,
		},
		"NoErrorWithDiff": {
			err:      nil,
			hasDiffs: true,
			want:     ExitCodeDiffDetected,
		},
		"ToolError": {
			err:      errors.New("tool error"),
			hasDiffs: false,
			want:     ExitCodeToolError,
		},
		"ToolErrorWithDiff": {
			err:      errors.New("tool error"),
			hasDiffs: true,
			want:     ExitCodeToolError,
		},
		"SchemaValidationError": {
			err:      &SchemaValidationError{Message: "validation failed"},
			hasDiffs: false,
			want:     ExitCodeSchemaValidation,
		},
		"SchemaValidationErrorWithDiff": {
			err:      &SchemaValidationError{Message: "validation failed"},
			hasDiffs: true,
			want:     ExitCodeSchemaValidation,
		},
		"WrappedSchemaValidationError": {
			// A schema validation error wrapped with fmt.Errorf %w should still be detected
			// as a schema validation error (the wrapper doesn't introduce a new error type)
			err:      fmt.Errorf("context: %w", &SchemaValidationError{Message: "inner"}),
			hasDiffs: false,
			want:     ExitCodeSchemaValidation,
		},
		// Edge cases for batch processing with multiple errors
		"MultipleSchemaValidationErrors": {
			err: errors.Join(
				&SchemaValidationError{ResourceID: "Kind1/res1", Message: "field1 invalid"},
				&SchemaValidationError{ResourceID: "Kind2/res2", Message: "field2 invalid"},
				&SchemaValidationError{ResourceID: "Kind3/res3", Message: "field3 invalid"},
			),
			hasDiffs: false,
			want:     ExitCodeSchemaValidation,
		},
		"MultipleSchemaValidationErrorsWithDiffs": {
			err: errors.Join(
				&SchemaValidationError{ResourceID: "Kind1/res1", Message: "field1 invalid"},
				&SchemaValidationError{ResourceID: "Kind2/res2", Message: "field2 invalid"},
			),
			hasDiffs: true,
			want:     ExitCodeSchemaValidation,
		},
		"MixedToolAndSchemaValidationErrors_ToolErrorTakesPriority": {
			// When there's a mix of tool errors and schema validation errors,
			// tool error (exit code 1) should take priority over schema validation (exit code 2)
			err: errors.Join(
				errors.New("connection refused"),
				&SchemaValidationError{ResourceID: "Kind/res", Message: "invalid field"},
			),
			hasDiffs: false,
			want:     ExitCodeToolError,
		},
		"MixedToolAndSchemaValidationErrors_SchemaFirst": {
			// Order shouldn't matter - tool error still takes priority
			err: errors.Join(
				&SchemaValidationError{ResourceID: "Kind/res", Message: "invalid field"},
				errors.New("connection refused"),
			),
			hasDiffs: false,
			want:     ExitCodeToolError,
		},
		"MixedToolAndSchemaValidationErrorsWithDiffs": {
			err: errors.Join(
				errors.New("partial failure"),
				&SchemaValidationError{ResourceID: "Kind/res", Message: "invalid"},
			),
			hasDiffs: true,
			want:     ExitCodeToolError,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := DetermineExitCode(tc.err, tc.hasDiffs)
			if got != tc.want {
				t.Errorf("DetermineExitCode() = %v, want %v", got, tc.want)
			}
		})
	}
}
