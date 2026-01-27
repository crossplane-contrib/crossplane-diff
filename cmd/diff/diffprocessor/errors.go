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

// DetermineExitCode determines the appropriate exit code based on the error and diff status.
// Priority: tool error (1) > schema validation error (2) > diff detected (3) > success (0).
func DetermineExitCode(err error, hasDiffs bool) int {
	if err != nil {
		if IsSchemaValidationError(err) {
			return ExitCodeSchemaValidation
		}

		return ExitCodeToolError
	}

	if hasDiffs {
		return ExitCodeDiffDetected
	}

	return ExitCodeSuccess
}
