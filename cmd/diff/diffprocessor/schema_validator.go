package diffprocessor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane/cli/v2/cmd/crossplane/common/loggerwriter"
	"github.com/crossplane/cli/v2/cmd/crossplane/validate"
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

	// Collect all resources that need to be validated. The XR is passed as a
	// sanitized deep-copy (spec.crossplane stripped) because the real
	// composite reconciler populates that subtree with Crossplane-managed
	// runtime state (compositionRef, resourceRefs, ...). Real cluster-derived
	// CRDs declare spec.crossplane (Crossplane's CRD generator emits it),
	// but our integration-test CRD fixtures are hand-rolled and don't always
	// have it — strict unknown-field validation against those fixtures would
	// reject the field. Stripping on a copy keeps the original XR (used
	// downstream for diffing against cluster state) intact.
	// Managed resources pass through unchanged so defaults-in-place still
	// applies to their spec fields.
	resources := make([]*un.Unstructured, 0, len(composed)+1)

	resources = append(resources, v.stripCrossplaneManagedFields(xr))
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

	// Create a buffer to capture validation output for error messages,
	// and a MultiWriter to also send output to debug logs
	var validationOutput bytes.Buffer

	multiWriter := io.MultiWriter(&validationOutput, loggerwriter.NewLoggerWriter(v.logger))

	// SchemaValidation applies defaults IN-PLACE to the resources it sees,
	// mutating the underlying Object map. For each managed resource we wrap
	// `&un.Unstructured{Object: composed[i].UnstructuredContent()}` — the
	// embedded unstructured.Unstructured returns its Object map by reference,
	// so defaults applied here propagate back to the caller's
	// `composed[i]` via the shared map. That preserves pre-existing
	// defaulting behaviour for downstream diff calculation.
	//
	// The XR here is a deep-copied stripped variant (see
	// stripCrossplaneManagedFields above), so any defaults applied to it
	// stay on the copy — the real composite reconciler in the render
	// pipeline already applied XRD schema defaults before we got here, so
	// there's nothing on the XR side that needs to fold back to the caller.
	v.logger.Debug("Performing schema validation", "resourceCount", len(resources))

	err = validate.SchemaValidation(ctx, resources, v.schemaClient.GetAllCRDs(), true, true, multiWriter)
	if err != nil {
		// Parse and extract only the error lines from validation output
		details := extractValidationErrors(validationOutput.String())
		return NewSchemaValidationError("", details, err)
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

// extractValidationErrors parses validation output and returns clean error messages.
// It extracts lines starting with [x] (validation errors) and [!] (warnings/missing schemas),
// stripping the prefixes for cleaner display.
func extractValidationErrors(output string) string {
	var validationErrs []string

	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		// Use CutPrefix to check for prefix and strip it in one operation
		if cleaned, found := strings.CutPrefix(line, "[x]"); found {
			validationErrs = append(validationErrs, strings.TrimSpace(cleaned))
		} else if cleaned, found := strings.CutPrefix(line, "[!]"); found {
			validationErrs = append(validationErrs, strings.TrimSpace(cleaned))
		}
	}

	if len(validationErrs) == 0 {
		return "schema validation failed"
	}

	return strings.Join(validationErrs, "; ")
}

// stripCrossplaneManagedFields creates a copy of the resource with Crossplane-managed fields removed
// These fields are set by Crossplane controllers and may not be present in the CRD schema.
func (v *DefaultSchemaValidator) stripCrossplaneManagedFields(resource *un.Unstructured) *un.Unstructured {
	// Create a deep copy to avoid modifying the original. The caller still
	// needs the original (with spec.crossplane.*) for downstream diff
	// calculation against cluster state — which, once applied, will also
	// carry those fields.
	sanitized := resource.DeepCopy()

	// spec.crossplane is populated by the Crossplane composite reconciler
	// (compositionRef, compositionRevisionRef, resourceRefs, ...). Real
	// cluster CRDs derived from XRDs declare it because Crossplane's CRD
	// generator emits the subtree, but our hand-rolled integration-test CRD
	// fixtures don't always include it. Strict unknown-field validation
	// against those fixtures would reject the field, so we strip it on the
	// copy. (E2Es run against real Crossplane in kind and don't go through
	// this path.)
	un.RemoveNestedField(sanitized.Object, "spec", "crossplane")

	return sanitized
}
