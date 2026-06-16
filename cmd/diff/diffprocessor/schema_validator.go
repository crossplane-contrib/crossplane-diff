package diffprocessor

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	pkgvalidate "github.com/crossplane/cli/v2/pkg/validate"
	clixr "github.com/crossplane/cli/v2/pkg/xr"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
)

// SchemaValidator handles validation of resources against CRD schemas.
type SchemaValidator interface {
	// ValidateResources validates resources using schema validation
	ValidateResources(ctx context.Context, xr *un.Unstructured, composed []cpd.Unstructured) error

	// EnsureComposedResourceCRDs ensures we have all required CRDs for validation
	EnsureComposedResourceCRDs(ctx context.Context, resources []*un.Unstructured) error

	// ValidateScopeConstraints validates that a resource has the appropriate namespace for its scope
	// and that namespaced resources match the expected namespace (no cross-namespace refs)
	ValidateScopeConstraints(ctx context.Context, resource *un.Unstructured, expectedNamespace string, isClaimRoot bool) error
}

// DefaultSchemaValidator implements SchemaValidator interface.
type DefaultSchemaValidator struct {
	defClient    xp.DefinitionClient
	schemaClient k8.SchemaClient
	logger       logging.Logger
}

// NewSchemaValidator creates a new DefaultSchemaValidator.
func NewSchemaValidator(sClient k8.SchemaClient, dClient xp.DefinitionClient, logger logging.Logger) SchemaValidator {
	return &DefaultSchemaValidator{
		defClient:    dClient,
		schemaClient: sClient,
		logger:       logger,
	}
}

// LoadCRDs loads CRDs from the cluster.
func (v *DefaultSchemaValidator) LoadCRDs(ctx context.Context) error {
	v.logger.Debug("Loading CRDs from cluster")

	// Get XRDs from the client (which will use its cache when available)
	xrds, err := v.defClient.GetXRDs(ctx)
	if err != nil {
		v.logger.Debug("Failed to get XRDs", "error", err)
		return errors.Wrap(err, "cannot get XRDs")
	}

	// Use SchemaClient to load CRDs from XRDs
	err = v.schemaClient.LoadCRDsFromXRDs(ctx, xrds)
	if err != nil {
		v.logger.Debug("Failed to load CRDs into schema client", "error", err)
		return errors.Wrap(err, "cannot load CRDs into schema client")
	}

	// Logging is handled internally by the schema client

	return nil
}

// GetCRDs returns the current CRDs.
func (v *DefaultSchemaValidator) GetCRDs() []*extv1.CustomResourceDefinition {
	return v.schemaClient.GetAllCRDs()
}

// ValidateResources validates resources using schema validation.
func (v *DefaultSchemaValidator) ValidateResources(ctx context.Context, xr *un.Unstructured, composed []cpd.Unstructured) error {
	v.logger.Debug("Validating resources",
		"xr", fmt.Sprintf("%s/%s", xr.GetKind(), xr.GetName()),
		"namespace", xr.GetNamespace(),
		"composedCount", len(composed))

	// Collect all resources that need to be validated. Real cluster CRDs
	// derived from XRDs declare spec.crossplane (Crossplane's CRD generator
	// emits the subtree), so the v2-style XR + the composed resources we
	// hand to SchemaValidation should pass strict validation against those
	// CRDs unmodified. Our integration-test CRD fixtures match the
	// cluster-derived shape — see testdata/{diff,comp}/crds — so no
	// preprocessing is needed here.
	resources := make([]*un.Unstructured, 0, len(composed)+1)

	resources = append(resources, xr)
	for i := range composed {
		resources = append(resources, &un.Unstructured{Object: composed[i].UnstructuredContent()})
	}

	// Ensure we have all the required CRDs
	v.logger.Debug("Ensuring required CRDs for validation",
		"cachedCRDs", len(v.schemaClient.GetAllCRDs()),
		"resourceCount", len(resources))

	err := v.EnsureComposedResourceCRDs(ctx, resources)
	if err != nil {
		return errors.Wrap(err, "unable to ensure CRDs")
	}

	// Apply CRD-level defaults in place before handing resources off to
	// the diff calculator. The structured SchemaValidate API
	// intentionally deep-copies inputs and does not surface defaults
	// to its caller, so without this step composed resources would
	// reach diff calculation undefaulted and produce spurious diffs
	// for fields the cluster's defaulter would have populated. The
	// previous SchemaValidation entry point fused this defaulting
	// into validation; doing it explicitly here makes the
	// dependency obvious.
	if err := v.applyCRDDefaults(ctx, resources); err != nil {
		return err
	}

	// SchemaValidate is the structured-result API: it returns a
	// *ValidationResult that callers inspect directly.
	v.logger.Debug("Performing schema validation", "resourceCount", len(resources))

	result, err := pkgvalidate.SchemaValidate(ctx, resources, v.schemaClient.GetAllCRDs())
	if err != nil {
		// SchemaValidate's error return is reserved for setup
		// failures (e.g. a CRD that can't be compiled into a
		// validator). Wrap it as a SchemaValidationError so
		// IsSchemaValidationError / DetermineExitCode classify it the
		// same way the pre-refactor SchemaValidation entry point
		// did — exit code 2 (schema validation), not 1 (tool error).
		// No Result is attached because the validator never produced
		// one.
		return NewSchemaValidationError("", "schema validation setup failed", err)
	}

	v.logResultDetails(result)

	if rerr := pkgvalidate.ResultError(result, true); rerr != nil {
		return NewSchemaValidationError("", formatValidationErrors(result), rerr).WithResult(result)
	}

	// Additionally validate resource scope constraints (namespace requirements and cross-namespace refs)
	expectedNamespace := xr.GetNamespace()
	isClaimRoot := v.defClient.IsClaimResource(ctx, xr)
	v.logger.Debug("Performing resource scope validation", "resourceCount", len(resources), "expectedNamespace", expectedNamespace, "isClaimRoot", isClaimRoot)

	for _, resource := range resources {
		err := v.ValidateScopeConstraints(ctx, resource, expectedNamespace, isClaimRoot)
		if err != nil {
			resourceID := fmt.Sprintf("%s/%s", resource.GetKind(), resource.GetName())
			return NewSchemaValidationError(resourceID, "resource scope validation failed", err)
		}
	}

	v.logger.Debug("Resources validated successfully")

	return nil
}

// EnsureComposedResourceCRDs checks if we have all the CRDs needed for the cpd resources
// and fetches any missing ones from the cluster.
func (v *DefaultSchemaValidator) EnsureComposedResourceCRDs(ctx context.Context, resources []*un.Unstructured) error {
	v.logger.Debug("Ensuring required CRDs for validation", "resourceCount", len(resources))

	// Collect unique GVKs from resources
	uniqueGVKs := make(map[schema.GroupVersionKind]bool)

	for _, res := range resources {
		gvk := res.GroupVersionKind()
		uniqueGVKs[gvk] = true
	}

	// Try to fetch each required CRD - GetCRD will use cache if already present
	var missingCRDs []string

	for gvk := range uniqueGVKs {
		// Skip resources that don't require CRDs
		if !v.schemaClient.IsCRDRequired(ctx, gvk) {
			v.logger.Debug("Skipping built-in resource type, no CRD required",
				"gvk", gvk.String())

			continue
		}

		// Try to get the CRD using the client's GetCRD method (which will cache it)
		_, err := v.schemaClient.GetCRD(ctx, gvk)
		if err != nil {
			v.logger.Debug("CRD not found",
				"gvk", gvk.String(),
				"error", err)
			missingCRDs = append(missingCRDs, gvk.String())
		}
	}

	// If any CRDs are missing, fail
	if len(missingCRDs) > 0 {
		return errors.Errorf("unable to find CRDs for: %v", missingCRDs)
	}

	v.logger.Debug("Finished ensuring CRDs")

	return nil
}

// getResourceScope returns the scope (Namespaced/Cluster) for a given GVK.
func (v *DefaultSchemaValidator) getResourceScope(ctx context.Context, gvk schema.GroupVersionKind) (string, error) {
	v.logger.Debug("Getting resource scope", "gvk", gvk.String())

	// Get the typed CRD directly
	crd, err := v.schemaClient.GetCRD(ctx, gvk)
	if err != nil {
		v.logger.Debug("Failed to get CRD for scope lookup", "gvk", gvk.String(), "error", err)
		return "", errors.Wrapf(err, "cannot get CRD for %s to determine scope", gvk.String())
	}

	scope := string(crd.Spec.Scope)
	v.logger.Debug("Retrieved scope from CRD", "gvk", gvk.String(), "scope", scope)

	return scope, nil
}

// ValidateScopeConstraints validates that a resource has the appropriate namespace for its scope
// and that namespaced resources match the expected namespace (no cross-namespace refs).
func (v *DefaultSchemaValidator) ValidateScopeConstraints(ctx context.Context, resource *un.Unstructured, expectedNamespace string, isClaimRoot bool) error {
	gvk := resource.GroupVersionKind()
	resourceID := fmt.Sprintf("%s/%s", gvk.Kind, resource.GetName())

	scope, err := v.getResourceScope(ctx, gvk)
	if err != nil {
		return errors.Wrapf(err, "cannot determine scope for %s", resourceID)
	}

	resourceNamespace := resource.GetNamespace()

	switch scope {
	case "Namespaced":
		if resourceNamespace == "" {
			return errors.Errorf("namespaced resource %s must have a namespace", resourceID)
		}

		if expectedNamespace != "" && resourceNamespace != expectedNamespace {
			return errors.Errorf("namespaced resource %s has namespace %s but expected %s (cross-namespace references not supported)",
				resourceID, resourceNamespace, expectedNamespace)
		}
	case "Cluster":
		if resourceNamespace != "" {
			return errors.Errorf("cluster-scoped resource %s cannot have a namespace", resourceID)
		}

		if expectedNamespace != "" && !isClaimRoot {
			return errors.Errorf("namespaced XR cannot own cluster-scoped managed resource %s", resourceID)
		}
		// Claims are allowed to create cluster-scoped managed resources even if the claim is namespaced
		if expectedNamespace != "" && isClaimRoot {
			v.logger.Debug("Allowing namespaced claim to create cluster-scoped managed resource",
				"resource", resourceID, "namespace", resourceNamespace, "claimNamespace", expectedNamespace)
		}
	default:
		v.logger.Debug("Unknown resource scope", "resource", resourceID, "namespace", resourceNamespace, "scope", scope)
	}

	return nil
}

// applyCRDDefaults applies CRD-derived defaults to each resource in
// place. Built-in types and resources whose CRD is unknown are
// skipped; in both cases the diff calculator already handles them
// without server-side defaulting. We treat a CRD lookup error here
// as a no-op rather than a failure: EnsureComposedResourceCRDs is
// the canonical gate on missing CRDs, and the subsequent SchemaValidate
// call surfaces the same condition through ValidationStatusMissingSchema.
func (v *DefaultSchemaValidator) applyCRDDefaults(ctx context.Context, resources []*un.Unstructured) error {
	for _, r := range resources {
		gvk := r.GroupVersionKind()
		if !v.schemaClient.IsCRDRequired(ctx, gvk) {
			continue
		}

		crd, err := v.schemaClient.GetCRD(ctx, gvk)
		if err != nil {
			v.logger.Debug("skipping defaulting; CRD not found", "gvk", gvk.String(), "error", err)
			continue
		}

		if err := clixr.ApplyCRDDefaults(r.Object, r.GetAPIVersion(), *crd); err != nil {
			return errors.Wrapf(err, "cannot apply CRD defaults for %s/%s", gvk.String(), r.GetName())
		}
	}

	return nil
}

// logResultDetails emits per-resource debug logging for a validation
// result. This replaces the incidental logging that the previous
// stdout-capturing implementation produced via a loggerwriter, and
// surfaces operational signals (defaulting failures, missing schemas)
// even when ResultError reports success overall.
func (v *DefaultSchemaValidator) logResultDetails(result *pkgvalidate.ValidationResult) {
	for _, r := range result.Resources {
		v.logger.Debug("validation result",
			"apiVersion", r.APIVersion,
			"kind", r.Kind,
			"name", r.Name,
			"status", string(r.Status),
			"errors", len(r.Errors),
		)

		for _, e := range r.Errors {
			v.logger.Debug("validation error",
				"apiVersion", r.APIVersion,
				"kind", r.Kind,
				"name", r.Name,
				"type", string(e.Type),
				"field", e.Field,
				"message", e.Message,
			)
		}
	}
}

// formatValidationErrors produces a multi-line, per-resource breakdown
// of the failures in a *ValidationResult. Each resource that has
// something to report contributes a header line "<gvk> <name>:"
// followed by one indented line per FieldValidationError, with the
// error type appended in brackets and the structured Value rendered as
// "(got <value>)" when it isn't already quoted into the message text.
// Missing-schema resources collapse to a single "<gvk> <name>: missing
// schema" line.
//
// Callers invoke this only after pkgvalidate.ResultError has already
// returned a non-nil error, so an empty result means "no per-resource
// detail to report" — we return "" and let the wrapping
// SchemaValidationError carry the underlying ResultError message.
func formatValidationErrors(result *pkgvalidate.ValidationResult) string {
	var blocks []string

	for _, r := range result.Resources {
		switch r.Status {
		case pkgvalidate.ValidationStatusMissingSchema:
			blocks = append(blocks, formatMissingSchemaBlock(r))
		case pkgvalidate.ValidationStatusInvalid:
			blocks = append(blocks, formatInvalidBlock(r))
		case pkgvalidate.ValidationStatusValid,
			pkgvalidate.ValidationStatusDefaultingFailed:
			// Valid: nothing to report. DefaultingFailed-only resources
			// are reported by ResultError as success, so this function
			// never sees them via the failure path; the cases are here
			// only to keep the switch exhaustive.
		}
	}

	return strings.Join(blocks, "\n")
}

// resourceHeader formats the per-resource header used by both the
// missing-schema and invalid blocks. Cluster-scoped resources omit the
// namespace prefix; namespaced resources produce "<ns>/<name>". A
// resource without metadata.name collapses to just the GVK regardless
// of namespace — the namespace alone isn't a useful identifier for a
// nameless resource and emitting "<ns>/" would leave a stray trailing
// slash.
func resourceHeader(r pkgvalidate.ResourceValidationResult) string {
	if r.Name == "" {
		return fmt.Sprintf("%s/%s", r.APIVersion, r.Kind)
	}

	name := r.Name
	if r.Namespace != "" {
		name = r.Namespace + "/" + r.Name
	}

	return fmt.Sprintf("%s/%s %s", r.APIVersion, r.Kind, name)
}

// formatMissingSchemaBlock renders a single line for a resource whose
// CRD/XRD wasn't found. The resource was never validated, so there are
// no per-error lines to indent under it.
func formatMissingSchemaBlock(r pkgvalidate.ResourceValidationResult) string {
	return resourceHeader(r) + ": missing schema"
}

// formatInvalidBlock renders a header line plus one indented line per
// surfaced error.
//
// Defaulting entries are suppressed only when an actionable (schema /
// CEL / unknown-field) error is present on the same resource: those
// already convey the failure and the defaulting line would just be
// noise. Upstream's statusFromErrors today guarantees an Invalid
// resource has at least one non-defaulting error, but if a future
// change ever produced an Invalid resource whose Errors are all
// defaulting we'd rather emit them than silently drop everything.
func formatInvalidBlock(r pkgvalidate.ResourceValidationResult) string {
	hasActionable := false

	for _, e := range r.Errors {
		if e.Type != pkgvalidate.FieldErrorTypeDefaulting {
			hasActionable = true
			break
		}
	}

	lines := []string{resourceHeader(r) + ":"}

	for _, e := range r.Errors {
		if hasActionable && e.Type == pkgvalidate.FieldErrorTypeDefaulting {
			continue
		}

		lines = append(lines, "  "+formatErrorLine(e))
	}

	return strings.Join(lines, "\n")
}

// formatErrorLine renders one FieldValidationError as
// "<message>[ (got <value>)] [<type>]". The bad value tail is omitted
// when Value is nil or already present in the message. Duplication
// detection is type-aware to avoid false positives:
//
//   - String values are checked in their %q (quoted) form, matching
//     how k8s validators emit them (`Invalid value: "five"`); the
//     surrounding quotes give the search natural delimiters so
//     Value="k" against message "spec.kind: Required" doesn't
//     suppress the tail just because "k" sits inside "kind".
//
//   - Non-string values use a word-boundary regex so Value=42 against
//     message "...Invalid value: 420..." doesn't suppress the tail
//     just because "42" is a prefix of "420".
func formatErrorLine(e pkgvalidate.FieldValidationError) string {
	msg := e.Message
	if rendered := renderBadValue(e.Value); rendered != "" && !valueAlreadyInMessage(msg, e.Value, rendered) {
		msg = fmt.Sprintf("%s (got %s)", msg, rendered)
	}

	return fmt.Sprintf("%s [%s]", msg, e.Type)
}

// valueAlreadyInMessage reports whether the rendered form of a bad
// value is already present in the validator's message text. The check
// shape depends on the value's Go type — see formatErrorLine's
// comment for the rationale.
func valueAlreadyInMessage(msg string, value any, rendered string) bool {
	if _, ok := value.(string); ok {
		// rendered already includes surrounding %q quotes; substring
		// search is delimiter-safe.
		return strings.Contains(msg, rendered)
	}

	// For non-strings rendered is unquoted, so anchor the search at
	// word boundaries to avoid 42 matching inside 420.
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(rendered) + `\b`).MatchString(msg)
}

// renderBadValue formats a FieldValidationError.Value for display.
// Returns the empty string for a nil Value so callers can use it as a
// presence check.
//
// Strings get %q (quoted), matching how k8s validation messages embed
// them (`Invalid value: "five"`). Other types use %v: numbers and
// booleans read naturally unquoted, and structs render via their
// default Go representation.
//
// Duplication detection against the validator's message text is done
// separately by valueAlreadyInMessage, which knows how to anchor the
// search at the right boundaries for each value type.
func renderBadValue(value any) string {
	if value == nil {
		return ""
	}

	if s, ok := value.(string); ok {
		return fmt.Sprintf("%q", s)
	}

	return fmt.Sprintf("%v", value)
}
