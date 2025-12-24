package kubernetes

import (
	"context"
	"fmt"
	"strings"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

const (
	// FieldOwnerComposedPrefix is the prefix for field owners of composed resources.
	// This matches Crossplane's FieldOwnerComposedPrefix in composition_functions.go.
	FieldOwnerComposedPrefix = "apiextensions.crossplane.io/composed"

	// FieldOwnerDefault is the default field owner when no specific owner is provided.
	FieldOwnerDefault = "crossplane-diff"
)

// GetComposedFieldOwner extracts the Crossplane composed resource field owner from
// an existing object's managedFields. Returns empty string if not found.
// This is used to ensure dry-run apply uses the same field owner as Crossplane,
// which correctly handles field removal detection.
func GetComposedFieldOwner(obj *un.Unstructured) string {
	if obj == nil {
		return ""
	}

	for _, mf := range obj.GetManagedFields() {
		if strings.HasPrefix(mf.Manager, FieldOwnerComposedPrefix) {
			return mf.Manager
		}
	}

	return ""
}

// ApplyClient handles server-side apply operations.
type ApplyClient interface {
	// DryRunApply performs a dry-run server-side apply.
	// If fieldOwner is empty, uses the default field owner.
	DryRunApply(ctx context.Context, obj *un.Unstructured, fieldOwner string) (*un.Unstructured, error)
}

// DefaultApplyClient implements ApplyClient.
type DefaultApplyClient struct {
	dynamicClient dynamic.Interface
	typeConverter TypeConverter
	logger        logging.Logger
}

// NewApplyClient creates a new DefaultApplyClient.
func NewApplyClient(clients *core.Clients, converter TypeConverter, logger logging.Logger) ApplyClient {
	return &DefaultApplyClient{
		dynamicClient: clients.Dynamic,
		typeConverter: converter,
		logger:        logger,
	}
}

// DryRunApply performs a dry-run server-side apply.
// If fieldOwner is empty, uses the default field owner.
func (c *DefaultApplyClient) DryRunApply(ctx context.Context, obj *un.Unstructured, fieldOwner string) (*un.Unstructured, error) {
	resourceID := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())

	// Use default field owner if not specified
	if fieldOwner == "" {
		fieldOwner = FieldOwnerDefault
	}

	c.logger.Debug("Performing dry-run apply", "resource", resourceID, "fieldOwner", fieldOwner)

	// Get the GVK from the object
	gvk := obj.GroupVersionKind()

	// Convert GVK to GVR
	gvr, err := c.typeConverter.GVKToGVR(ctx, gvk)
	if err != nil {
		c.logger.Debug("Failed to convert GVK to GVR", "gvk", gvk.String(), "error", err)
		return nil, errors.Wrapf(err, "cannot perform dry-run apply for %s", resourceID)
	}

	// Get the resource client for the namespace
	resourceClient := c.dynamicClient.Resource(gvr).Namespace(obj.GetNamespace())

	// Create apply options for a dry run with the specified field owner
	applyOptions := metav1.ApplyOptions{
		FieldManager: fieldOwner,
		Force:        true,
		DryRun:       []string{metav1.DryRunAll},
	}

	// Perform a dry-run server-side apply
	result, err := resourceClient.Apply(ctx, obj.GetName(), obj, applyOptions)
	if err != nil {
		c.logger.Debug("Dry-run apply failed", "resource", resourceID, "error", err)

		return nil, errors.Wrapf(err, "failed to apply resource %s/%s",
			obj.GetNamespace(), obj.GetName())
	}

	c.logger.Debug("Dry-run apply successful", "resource", resourceID, "resourceVersion", result.GetResourceVersion())

	return result, nil
}
