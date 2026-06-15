package diffprocessor

import (
	"context"
	"fmt"
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
		return errors.Wrap(err, "schema validation failed")
	}

	v.logResultDetails(result)

	if rerr := pkgvalidate.ResultError(result, true); rerr != nil {
		return NewSchemaValidationError("", formatValidationErrors(result), rerr)
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

// formatValidationErrors produces a single-line, semicolon-joined string
// describing the failures in a *ValidationResult. It mirrors the
// previously extracted text-renderer output (one entry per
// FieldValidationError, with a "could not find CRD/XRD for ..." entry
// per missing-schema resource) so existing callers and tests that
// match on these substrings keep working.
func formatValidationErrors(result *pkgvalidate.ValidationResult) string {
	var msgs []string

	for _, r := range result.Resources {
		gvk := fmt.Sprintf("%s, Kind=%s", r.APIVersion, r.Kind)

		switch r.Status {
		case pkgvalidate.ValidationStatusMissingSchema:
			msgs = append(msgs, "could not find CRD/XRD for: "+gvk)
		case pkgvalidate.ValidationStatusInvalid:
			for _, e := range r.Errors {
				if e.Type == pkgvalidate.FieldErrorTypeDefaulting {
					// ResultError treats a defaulting error as a
					// failure only when accompanied by a schema-class
					// error on the same resource. The schema-class
					// error already gets its own entry; emitting the
					// defaulting line too would just be noise.
					continue
				}

				msgs = append(msgs, fmt.Sprintf("schema validation error %s, %s : %s", gvk, r.Name, e.Message))
			}
		case pkgvalidate.ValidationStatusValid,
			pkgvalidate.ValidationStatusDefaultingFailed:
			// Valid: nothing to report. DefaultingFailed-only resources
			// are reported by ResultError as success, so this function
			// never sees them via the failure path; the case is here
			// only to keep the switch exhaustive.
		}
	}

	if len(msgs) == 0 {
		return "schema validation failed"
	}

	return strings.Join(msgs, "; ")
}
