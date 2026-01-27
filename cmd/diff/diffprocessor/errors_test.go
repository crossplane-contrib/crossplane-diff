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
	"testing"

	gcmp "github.com/google/go-cmp/cmp"
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
			err:      errors.Join(errors.New("outer"), &SchemaValidationError{Message: "inner"}),
			hasDiffs: false,
			want:     ExitCodeSchemaValidation,
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
