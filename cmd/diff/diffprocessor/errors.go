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

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	pkgvalidate "github.com/crossplane/cli/v2/pkg/validate"
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
// Used to distinguish validation errors from other tool errors for exit
// code handling.
//
// Result, when non-nil, carries the structured per-resource validation
// outcome that produced this error. Output renderers use it to surface
// typed FieldValidationError records under OutputError.ValidationFailures
// in JSON / YAML output. It is left nil for paths that fail validation
// without a *pkgvalidate.ValidationResult in hand — for example
// scope-validation errors raised after schema validation succeeded — so
// the absence of structured detail is observable rather than fabricated.
type SchemaValidationError struct {
	ResourceID string
	Message    string
	Err        error
	Result     *pkgvalidate.ValidationResult
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

// NewSchemaValidationError creates a new SchemaValidationError without
// a structured Result. Use WithResult on the returned value to attach
// one when the failure originated from pkg/validate.SchemaValidate.
func NewSchemaValidationError(resourceID, message string, err error) *SchemaValidationError {
	return &SchemaValidationError{
		ResourceID: resourceID,
		Message:    message,
		Err:        err,
	}
}

// WithResult attaches the structured *pkgvalidate.ValidationResult
// that produced this error so downstream renderers can emit typed
// per-resource failures alongside the human-readable Message. Returns
// the receiver for fluent chaining.
func (e *SchemaValidationError) WithResult(result *pkgvalidate.ValidationResult) *SchemaValidationError {
	e.Result = result
	return e
}

// NewOutputError builds a structured-output entry for err, tagged with
// resourceID. When err contains a *SchemaValidationError that carries a
// pkgvalidate.ValidationResult, the returned OutputError also exposes a
// typed per-resource breakdown via ValidationFailures so machine
// consumers don't need to parse Message. Non-validation errors return
// an OutputError with only ResourceID and Message populated.
func NewOutputError(resourceID string, err error) dt.OutputError {
	out := dt.OutputError{
		ResourceID: resourceID,
		Message:    err.Error(),
	}

	var sve *SchemaValidationError
	if errors.As(err, &sve) && sve.Result != nil {
		out.ValidationFailures = validationFailuresFromResult(sve.Result)
	}

	return out
}

// validationFailuresFromResult maps a pkgvalidate.ValidationResult into
// the wire types crossplane-diff exposes through OutputError. Resources
// with status Valid are filtered out — ValidationFailures is "what went
// wrong", not "the full audit log". Resources with status
// DefaultingFailed are filtered out too: pkgvalidate.ResultError treats
// defaulting-only failures as success, so a SchemaValidationError
// reaching this code path should not advertise them as failures.
func validationFailuresFromResult(result *pkgvalidate.ValidationResult) []dt.ResourceValidationFailure {
	if result == nil {
		return nil
	}

	var out []dt.ResourceValidationFailure

	for _, r := range result.Resources {
		switch r.Status {
		case pkgvalidate.ValidationStatusValid,
			pkgvalidate.ValidationStatusDefaultingFailed:
			continue
		case pkgvalidate.ValidationStatusInvalid,
			pkgvalidate.ValidationStatusMissingSchema:
			// fall through
		}

		out = append(out, dt.ResourceValidationFailure{
			APIVersion: r.APIVersion,
			Kind:       r.Kind,
			Name:       r.Name,
			Namespace:  r.Namespace,
			Status:     string(r.Status),
			Errors:     fieldValidationErrorsFromUpstream(r.Errors),
		})
	}

	return out
}

// fieldValidationErrorsFromUpstream converts the cli's per-field error
// slice into our wire shape. Defaulting entries are filtered out when
// at least one actionable (schema / CEL / unknown-field) error is
// present on the same resource, mirroring the suppression policy of
// formatValidationErrors so the typed and human-readable views agree.
func fieldValidationErrorsFromUpstream(errs []pkgvalidate.FieldValidationError) []dt.FieldValidationError {
	if len(errs) == 0 {
		return nil
	}

	hasActionable := false

	for _, e := range errs {
		if e.Type != pkgvalidate.FieldErrorTypeDefaulting {
			hasActionable = true
			break
		}
	}

	out := make([]dt.FieldValidationError, 0, len(errs))

	for _, e := range errs {
		if hasActionable && e.Type == pkgvalidate.FieldErrorTypeDefaulting {
			continue
		}

		out = append(out, dt.FieldValidationError{
			Type:    string(e.Type),
			Field:   e.Field,
			Message: e.Message,
			Value:   e.Value,
		})
	}

	return out
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
