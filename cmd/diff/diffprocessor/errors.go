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
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
//
// Failures is the same information for rejections that did not come from
// pkg/validate at all — specifically, a resource the apiserver itself refused
// during a dry-run (a validating admission webhook, a ResourceQuota, a missing
// namespace). Those are the same kind of fact as a schema rejection ("the
// cluster will not accept this resource"), just detected by the apiserver
// instead of locally, so they belong in the same exit-code tier and the same
// output field. They are carried separately rather than synthesised into a
// pkgvalidate.ValidationResult because that would mean inventing a
// FieldErrorType value upstream does not define; ResourceValidationFailure is
// explicitly ours to populate (see its doc in renderer/types).
type SchemaValidationError struct {
	ResourceID string
	Message    string
	Err        error
	Result     *pkgvalidate.ValidationResult
	Failures   []dt.ResourceValidationFailure
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

// WithFailures attaches already-built per-resource failures for a rejection
// that did not originate from pkg/validate — an apiserver dry-run refusal. It
// is the sibling of WithResult; see the Failures field for why the two are
// separate. Returns the receiver for fluent chaining.
func (e *SchemaValidationError) WithFailures(failures []dt.ResourceValidationFailure) *SchemaValidationError {
	e.Failures = failures
	return e
}

// FieldErrorTypeAdmission is the FieldValidationError.Type for a rejection
// issued by the apiserver's admission chain rather than by local schema
// validation. It deliberately sits alongside upstream's "schema" / "cel" /
// "unknownField" / "defaulting" values in the same field, because to a consumer
// asking "why won't the cluster accept this?" it is the same kind of answer.
const FieldErrorTypeAdmission = "admission"

// NewOutputError builds a structured-output entry for err, tagged with
// resourceID. When err contains a *SchemaValidationError that carries either a
// pkgvalidate.ValidationResult or pre-built Failures, the returned OutputError
// also exposes a typed per-resource breakdown via ValidationFailures so machine
// consumers don't need to parse Message. Non-validation errors return
// an OutputError with only ResourceID and Message populated.
//
// Explicit Failures win over Result. The two are never both set in practice —
// one comes from pkg/validate, the other from an apiserver rejection — but
// preferring the explicit list means a caller that sets it cannot have it
// silently dropped.
func NewOutputError(resourceID string, err error) dt.OutputError {
	out := dt.OutputError{
		ResourceID: resourceID,
		Message:    err.Error(),
	}

	if sve, ok := errors.AsType[*SchemaValidationError](err); ok {
		switch {
		case len(sve.Failures) > 0:
			out.ValidationFailures = sve.Failures
		case sve.Result != nil:
			out.ValidationFailures = validationFailuresFromResult(sve.Result)
		}
	}

	return out
}

// NewAdmissionRejectionError wraps an apiserver dry-run rejection of obj as a
// SchemaValidationError, so it lands in the same exit-code tier and the same
// structured-output field as a local schema rejection.
//
// This is used for BOTH new and existing resources on purpose. Before this
// existed, a webhook rejection of an existing resource surfaced as a plain tool
// error (exit 1) while the identical rejection of an addition would have been a
// validation error (exit 2) — reporting the same cluster fact under two
// different exit codes depending only on whether the resource happened to exist
// already. See crossplane-diff#334.
//
// rejectedDesc describes the REJECTED resource (the calculator's per-resource
// id, e.g. "Bucket/my-bucket"), not the input XR. It is folded into the message
// because OutputError.ResourceID identifies the input the user supplied, which
// for a composed resource is the XR — without this, a human would be told their
// XR was rejected with no way to tell which composed resource the cluster
// actually refused.
//
// The inner SchemaValidationError deliberately gets an empty ResourceID: its
// Error() would otherwise prefix "schema validation error for <id>: ", which
// both duplicates the id that OutputError.FormatError already prints and
// mislabels an admission rejection as a schema problem.
func NewAdmissionRejectionError(rejectedDesc string, obj *un.Unstructured, err error) *SchemaValidationError {
	msg := fmt.Sprintf("the cluster rejected %s: %s", rejectedDesc, err.Error())

	return NewSchemaValidationError("", msg, err).WithFailures([]dt.ResourceValidationFailure{{
		APIVersion: obj.GetAPIVersion(),
		Kind:       obj.GetKind(),
		Name:       obj.GetName(),
		Namespace:  obj.GetNamespace(),
		// Reuse upstream's status vocabulary rather than a literal, so this row is
		// indistinguishable in shape from one produced by validationFailuresFromResult.
		Status: string(pkgvalidate.ValidationStatusInvalid),
		Errors: []dt.FieldValidationError{{
			Type:    FieldErrorTypeAdmission,
			Message: err.Error(),
		}},
	}})
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
