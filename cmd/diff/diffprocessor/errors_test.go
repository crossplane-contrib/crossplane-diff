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
