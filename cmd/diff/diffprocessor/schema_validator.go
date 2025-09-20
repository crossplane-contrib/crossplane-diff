package diffprocessor

import (
	"context"
	"fmt"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"

	"github.com/crossplane/crossplane/v2/cmd/crank/beta/validate"
	"github.com/crossplane/crossplane/v2/cmd/crank/common/loggerwriter"
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
		"composedCount", len(composed))

	// Collect all resources that need to be validated
	resources := make([]*un.Unstructured, 0, len(composed)+1)

	// Add the XR to the validation list
	resources = append(resources, xr)

	// Add cpd resources to validation list
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

	// Create a logger writer to capture output
	loggerWriter := loggerwriter.NewLoggerWriter(v.logger)

	// Validate using the CRD schemas
	// Use skipSuccessLogs=true to avoid cluttering the output with success messages
	v.logger.Debug("Performing schema validation", "resourceCount", len(resources))

	err = validate.SchemaValidation(ctx, resources, v.schemaClient.GetAllCRDs(), true, true, loggerWriter)
	if err != nil {
		return errors.Wrap(err, "schema validation failed")
	}

	// Additionally validate resource scope constraints (namespace requirements and cross-namespace refs)
	expectedNamespace := xr.GetNamespace()
	isClaimRoot := v.defClient.IsClaimResource(ctx, xr)
	v.logger.Debug("Performing resource scope validation", "resourceCount", len(resources), "expectedNamespace", expectedNamespace, "isClaimRoot", isClaimRoot)

	for _, resource := range resources {
		err := v.ValidateScopeConstraints(ctx, resource, expectedNamespace, isClaimRoot)
		if err != nil {
			return errors.Wrapf(err, "resource scope validation failed for %s/%s",
				resource.GetKind(), resource.GetName())
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
				"resource", resourceID, "claimNamespace", expectedNamespace)
		}
	default:
		v.logger.Debug("Unknown resource scope", "resource", resourceID, "scope", scope)
	}

	return nil
}
