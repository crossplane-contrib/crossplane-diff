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
)

// Exit codes for crossplane-diff CLI.
const (
	// ExitCodeSuccess indicates no diff and no errors.
	ExitCodeSuccess = 0

	// ExitCodeToolError indicates a tool execution error (e.g., kube access, internal failure).
	ExitCodeToolError = 1

	// ExitCodeSchemaValidation indicates a schema validation error.
	ExitCodeSchemaValidation = 2

	// ExitCodeDiffDetected indicates that differences were detected.
	ExitCodeDiffDetected = 3
)

// SchemaValidationError indicates schema validation failed.
// Used to distinguish validation errors from other tool errors for exit code handling.
type SchemaValidationError struct {
	ResourceID string
	Message    string
	Err        error
}

// Error implements the error interface.
func (e *SchemaValidationError) Error() string {
	if e.ResourceID != "" {
		return fmt.Sprintf("schema validation error for %s: %s", e.ResourceID, e.Message)
	}

	return e.Message
}

// Unwrap returns the wrapped error for errors.Is/As compatibility.
func (e *SchemaValidationError) Unwrap() error {
	return e.Err
}

// NewSchemaValidationError creates a new SchemaValidationError.
func NewSchemaValidationError(resourceID, message string, err error) *SchemaValidationError {
	return &SchemaValidationError{
		ResourceID: resourceID,
		Message:    message,
		Err:        err,
	}
}

// IsSchemaValidationError checks if any error in the chain is a schema validation error.
func IsSchemaValidationError(err error) bool {
	var sve *SchemaValidationError
	return errors.As(err, &sve)
}

// isOnlySchemaValidationErrors checks if ALL errors in the error tree are schema validation errors.
// Returns false if there are any non-schema-validation errors in the chain.
// This is used to determine exit code priority: tool errors take precedence over schema validation errors.
func isOnlySchemaValidationErrors(err error) bool {
	if err == nil {
		return true
	}

	// Check if this is a join error (contains multiple errors) - check this FIRST
	// before checking if it's a SchemaValidationError, because errors.As would
	// traverse into the join and find schema validation errors even if there are
	// non-schema-validation errors in the same join.
	type unwrapMultiple interface {
		Unwrap() []error
	}
	if joinErr, ok := err.(unwrapMultiple); ok {
		for _, e := range joinErr.Unwrap() {
			if !isOnlySchemaValidationErrors(e) {
				return false
			}
		}

		return true
	}

	// Check if this specific error (not its wrapped errors) is a SchemaValidationError
	// Use type assertion instead of errors.As to avoid traversing into wrapped errors
	schemaValidationError := &SchemaValidationError{}
	if errors.As(err, &schemaValidationError) {
		// A SchemaValidationError IS a schema validation error - its wrapped Err field
		// contains the underlying cause/detail (e.g., the original validation library error),
		// not a different error type. So we return true immediately.
		return true
	}

	// Check if this wraps another error using standard Go 1.13+ Unwrap()
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		return isOnlySchemaValidationErrors(unwrapped)
	}

	// Check if this wraps another error using pkg/errors Cause() interface
	// (used by github.com/crossplane/crossplane-runtime/v2/pkg/errors)
	type causer interface {
		Cause() error
	}
	if cause, ok := err.(causer); ok {
		return isOnlySchemaValidationErrors(cause.Cause())
	}

	// This is a leaf error that's not a SchemaValidationError
	return false
}

// DetermineExitCode determines the appropriate exit code based on the error and diff status.
// Priority: tool error (1) > schema validation error (2) > diff detected (3) > success (0).
func DetermineExitCode(err error, hasDiffs bool) int {
	if err != nil {
		// If ALL errors are schema validation errors, return schema validation exit code.
		// If there are ANY non-schema-validation errors (tool errors), return tool error exit code.
		// Tool errors take priority because they indicate infrastructure/connectivity issues
		// that prevent the tool from functioning correctly.
		if isOnlySchemaValidationErrors(err) {
			return ExitCodeSchemaValidation
		}

		return ExitCodeToolError
	}

	if hasDiffs {
		return ExitCodeDiffDetected
	}

	return ExitCodeSuccess
}
