package diffprocessor

import (
	"context"
	"fmt"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"

	"github.com/crossplane/crossplane/v2/cmd/crank/beta/validate"
	"github.com/crossplane/crossplane/v2/cmd/crank/common/crd"
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

	// IsClaimResource checks if the root resource is a claim type
	IsClaimResource(ctx context.Context, resource *un.Unstructured) bool
}

// DefaultSchemaValidator implements SchemaValidator interface.
type DefaultSchemaValidator struct {
	defClient    xp.DefinitionClient
	schemaClient k8.SchemaClient
	logger       logging.Logger
	crds         []*extv1.CustomResourceDefinition
}

// NewSchemaValidator creates a new DefaultSchemaValidator.
func NewSchemaValidator(sClient k8.SchemaClient, dClient xp.DefinitionClient, logger logging.Logger) SchemaValidator {
	return &DefaultSchemaValidator{
		defClient:    dClient,
		schemaClient: sClient,
		logger:       logger,
		crds:         []*extv1.CustomResourceDefinition{},
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

	// Convert XRDs to CRDs
	crds, err := crd.ConvertToCRDs(xrds)
	if err != nil {
		v.logger.Debug("Failed to convert XRDs to CRDs", "error", err)
		return errors.Wrap(err, "cannot convert XRDs to CRDs")
	}

	v.crds = crds
	v.logger.Debug("Loaded CRDs", "count", len(crds))
	return nil
}

// SetCRDs sets the CRDs directly, useful for testing or when CRDs are pre-loaded.
func (v *DefaultSchemaValidator) SetCRDs(crds []*extv1.CustomResourceDefinition) {
	v.crds = crds
	v.logger.Debug("Set CRDs directly", "count", len(crds))
}

// GetCRDs returns the current CRDs.
func (v *DefaultSchemaValidator) GetCRDs() []*extv1.CustomResourceDefinition {
	return v.crds
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
		"cachedCRDs", len(v.crds),
		"resourceCount", len(resources))

	if err := v.EnsureComposedResourceCRDs(ctx, resources); err != nil {
		return errors.Wrap(err, "unable to ensure CRDs")
	}

	// Create a logger writer to capture output
	loggerWriter := loggerwriter.NewLoggerWriter(v.logger)

	// Validate using the CRD schemas
	// Use skipSuccessLogs=true to avoid cluttering the output with success messages
	v.logger.Debug("Performing schema validation", "resourceCount", len(resources))
	if err := validate.SchemaValidation(ctx, resources, v.crds, true, true, loggerWriter); err != nil {
		return errors.Wrap(err, "schema validation failed")
	}

	// Additionally validate resource scope constraints (namespace requirements and cross-namespace refs)
	expectedNamespace := xr.GetNamespace()
	isClaimRoot := v.IsClaimResource(ctx, xr)
	v.logger.Debug("Performing resource scope validation", "resourceCount", len(resources), "expectedNamespace", expectedNamespace, "isClaimRoot", isClaimRoot)
	for _, resource := range resources {
		if err := v.ValidateScopeConstraints(ctx, resource, expectedNamespace, isClaimRoot); err != nil {
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
	// Create a map of existing CRDs by GVK for quick lookup
	existingCRDs := make(map[schema.GroupVersionKind]bool)
	for _, crd := range v.crds {
		for _, version := range crd.Spec.Versions {
			gvk := schema.GroupVersionKind{
				Group:   crd.Spec.Group,
				Version: version.Name,
				Kind:    crd.Spec.Names.Kind,
			}
			existingCRDs[gvk] = true
		}
	}

	// Collect GVKs from resources that aren't already covered
	missingGVKs := make(map[schema.GroupVersionKind]bool)
	for _, res := range resources {
		gvk := res.GroupVersionKind()
		if !existingCRDs[gvk] {
			missingGVKs[gvk] = true
		}
	}

	// If we have all the CRDs already, we're done
	if len(missingGVKs) == 0 {
		v.logger.Debug("All required CRDs are already cached")
		return nil
	}

	v.logger.Debug("Fetching additional CRDs", "missingCount", len(missingGVKs))

	// Fetch missing CRDs
	for gvk := range missingGVKs {
		// Skip resources that don't require CRDs
		if !v.schemaClient.IsCRDRequired(ctx, gvk) {
			v.logger.Debug("Skipping built-in resource type, no CRD required",
				"gvk", gvk.String())
			continue
		}

		// Try to get the CRD using the client's GetCRD method
		crdObj, err := v.schemaClient.GetCRD(ctx, gvk)
		if err != nil {
			v.logger.Debug("CRD not found (continuing)",
				"gvk", gvk.String(),
				"error", err)
			return errors.New("unable to find CRD for " + gvk.String())
		}

		// Convert to CRD
		crd := &extv1.CustomResourceDefinition{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(crdObj.Object, crd); err != nil {
			v.logger.Debug("Error converting CRD (continuing)",
				"gvk", gvk.String(),
				"error", err)
			continue
		}

		// Add to our cache
		v.crds = append(v.crds, crd)
		v.logger.Debug("Added CRD to cache", "crdName", crd.Name)
	}

	v.logger.Debug("Finished ensuring CRDs", "totalCRDs", len(v.crds))
	return nil
}

// getResourceScope returns the scope (Namespaced/Cluster) for a given GVK.
func (v *DefaultSchemaValidator) getResourceScope(ctx context.Context, gvk schema.GroupVersionKind) (string, error) {
	v.logger.Debug("Getting resource scope", "gvk", gvk.String())

	// First check if we have the CRD in our cache
	for _, crd := range v.crds {
		for _, version := range crd.Spec.Versions {
			if crd.Spec.Group == gvk.Group &&
				version.Name == gvk.Version &&
				crd.Spec.Names.Kind == gvk.Kind {
				scope := string(crd.Spec.Scope)
				v.logger.Debug("Found scope in cached CRDs", "gvk", gvk.String(), "scope", scope)
				return scope, nil
			}
		}
	}

	// If not in cache, try to fetch the CRD
	crdObj, err := v.schemaClient.GetCRD(ctx, gvk)
	if err != nil {
		v.logger.Debug("Failed to get CRD for scope lookup", "gvk", gvk.String(), "error", err)
		return "", errors.Wrapf(err, "cannot get CRD for %s to determine scope", gvk.String())
	}

	// Convert to CRD and extract scope
	crd := &extv1.CustomResourceDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(crdObj.Object, crd); err != nil {
		v.logger.Debug("Error converting CRD for scope lookup", "gvk", gvk.String(), "error", err)
		return "", errors.Wrapf(err, "cannot convert CRD for %s", gvk.String())
	}

	scope := string(crd.Spec.Scope)
	v.logger.Debug("Retrieved scope from CRD", "gvk", gvk.String(), "scope", scope)

	// Add to our cache for future use
	v.crds = append(v.crds, crd)

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

// IsClaimResource checks if the resource is a claim type by attempting to find an XRD that defines it as a claim.
func (v *DefaultSchemaValidator) IsClaimResource(ctx context.Context, resource *un.Unstructured) bool {
	gvk := resource.GroupVersionKind()

	// Try to find an XRD that defines this resource type as a claim
	_, err := v.defClient.GetXRDForClaim(ctx, gvk)
	if err != nil {
		v.logger.Debug("Resource is not a claim type", "gvk", gvk.String(), "error", err)
		return false
	}

	v.logger.Debug("Resource is a claim type", "gvk", gvk.String())
	return true
}
