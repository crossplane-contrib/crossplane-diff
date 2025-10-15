package crossplane

import (
	"context"
	"sort"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
)

const (
	// LabelCompositionName is the label key for the composition name on CompositionRevisions.
	LabelCompositionName = "crossplane.io/composition-name"
)

// CompositionRevisionClient handles operations related to CompositionRevisions.
type CompositionRevisionClient interface {
	core.Initializable

	// GetCompositionRevision gets a composition revision by name
	GetCompositionRevision(ctx context.Context, name string) (*apiextensionsv1.CompositionRevision, error)

	// ListCompositionRevisions lists all composition revisions in the cluster
	ListCompositionRevisions(ctx context.Context) ([]*apiextensionsv1.CompositionRevision, error)

	// GetLatestRevisionForComposition finds the latest revision for a given composition
	GetLatestRevisionForComposition(ctx context.Context, compositionName string) (*apiextensionsv1.CompositionRevision, error)

	// GetCompositionFromRevision extracts a Composition from a CompositionRevision
	GetCompositionFromRevision(revision *apiextensionsv1.CompositionRevision) *apiextensionsv1.Composition
}

// DefaultCompositionRevisionClient implements CompositionRevisionClient.
type DefaultCompositionRevisionClient struct {
	resourceClient kubernetes.ResourceClient
	logger         logging.Logger

	// Cache of composition revisions by name (for individual revision lookups)
	revisions map[string]*apiextensionsv1.CompositionRevision
	// Cache of revisions per composition (lazy-loaded on demand)
	revisionsByComposition map[string][]*apiextensionsv1.CompositionRevision
	gvks                   []schema.GroupVersionKind
}

// NewCompositionRevisionClient creates a new DefaultCompositionRevisionClient.
func NewCompositionRevisionClient(resourceClient kubernetes.ResourceClient, logger logging.Logger) CompositionRevisionClient {
	return &DefaultCompositionRevisionClient{
		resourceClient:         resourceClient,
		logger:                 logger,
		revisions:              make(map[string]*apiextensionsv1.CompositionRevision),
		revisionsByComposition: make(map[string][]*apiextensionsv1.CompositionRevision),
	}
}

// Initialize prepares the composition revision client for use.
func (c *DefaultCompositionRevisionClient) Initialize(ctx context.Context) error {
	c.logger.Debug("Initializing composition revision client")

	gvks, err := c.resourceClient.GetGVKsForGroupKind(ctx, "apiextensions.crossplane.io", "CompositionRevision")
	if err != nil {
		return errors.Wrap(err, "cannot get CompositionRevision GVKs")
	}

	c.gvks = gvks

	c.logger.Debug("Composition revision client initialized")

	return nil
}

// listCompositionRevisionsForComposition lists composition revisions for a specific composition using label selector.
func (c *DefaultCompositionRevisionClient) listCompositionRevisionsForComposition(ctx context.Context, compositionName string) ([]*apiextensionsv1.CompositionRevision, error) {
	c.logger.Debug("Listing composition revisions for composition", "compositionName", compositionName)

	// Define the composition revision GVK
	gvk := schema.GroupVersionKind{
		Group:   "apiextensions.crossplane.io",
		Version: "v1",
		Kind:    "CompositionRevision",
	}

	// Use label selector to filter server-side
	labelSelector := metav1.LabelSelector{
		MatchLabels: map[string]string{
			LabelCompositionName: compositionName,
		},
	}

	unRevisions, err := c.resourceClient.GetResourcesByLabel(ctx, gvk, "", labelSelector)
	if err != nil {
		c.logger.Debug("Failed to list composition revisions", "compositionName", compositionName, "error", err)
		return nil, errors.Wrapf(err, "cannot list composition revisions for composition %s", compositionName)
	}

	// Convert unstructured to typed
	revisions := make([]*apiextensionsv1.CompositionRevision, 0, len(unRevisions))
	for _, obj := range unRevisions {
		rev := &apiextensionsv1.CompositionRevision{}

		err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, rev)
		if err != nil {
			c.logger.Debug("Failed to convert composition revision from unstructured",
				"name", obj.GetName(),
				"error", err)

			return nil, errors.Wrap(err, "cannot convert unstructured to CompositionRevision")
		}

		revisions = append(revisions, rev)
	}

	c.logger.Debug("Successfully retrieved composition revisions", "compositionName", compositionName, "count", len(revisions))

	return revisions, nil
}

// ListCompositionRevisions lists all composition revisions in the cluster.
func (c *DefaultCompositionRevisionClient) ListCompositionRevisions(ctx context.Context) ([]*apiextensionsv1.CompositionRevision, error) {
	c.logger.Debug("Listing composition revisions from cluster")

	// Define the composition revision GVK
	gvk := schema.GroupVersionKind{
		Group:   "apiextensions.crossplane.io",
		Version: "v1",
		Kind:    "CompositionRevision",
	}

	// Get all composition revisions using the resource client
	unRevisions, err := c.resourceClient.ListResources(ctx, gvk, "")
	if err != nil {
		c.logger.Debug("Failed to list composition revisions", "error", err)
		return nil, errors.Wrap(err, "cannot list composition revisions from cluster")
	}

	// Convert unstructured to typed
	revisions := make([]*apiextensionsv1.CompositionRevision, 0, len(unRevisions))
	for _, obj := range unRevisions {
		rev := &apiextensionsv1.CompositionRevision{}

		err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, rev)
		if err != nil {
			c.logger.Debug("Failed to convert composition revision from unstructured",
				"name", obj.GetName(),
				"error", err)

			return nil, errors.Wrap(err, "cannot convert unstructured to CompositionRevision")
		}

		revisions = append(revisions, rev)
	}

	c.logger.Debug("Successfully retrieved composition revisions", "count", len(revisions))

	return revisions, nil
}

// GetCompositionRevision gets a composition revision by name.
func (c *DefaultCompositionRevisionClient) GetCompositionRevision(ctx context.Context, name string) (*apiextensionsv1.CompositionRevision, error) {
	// Check cache first
	if rev, ok := c.revisions[name]; ok {
		return rev, nil
	}

	// Not in cache, fetch from cluster
	gvk := schema.GroupVersionKind{
		Group:   "apiextensions.crossplane.io",
		Version: "v1",
		Kind:    "CompositionRevision",
	}

	unRev, err := c.resourceClient.GetResource(ctx, gvk, "" /* CompositionRevisions are cluster scoped */, name)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot get composition revision %s", name)
	}

	// Convert to typed
	rev := &apiextensionsv1.CompositionRevision{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unRev.Object, rev); err != nil {
		return nil, errors.Wrap(err, "cannot convert unstructured to CompositionRevision")
	}

	// Update cache
	c.revisions[name] = rev

	return rev, nil
}

// GetLatestRevisionForComposition finds the latest revision for a given composition.
func (c *DefaultCompositionRevisionClient) GetLatestRevisionForComposition(ctx context.Context, compositionName string) (*apiextensionsv1.CompositionRevision, error) {
	c.logger.Debug("Finding latest revision for composition", "compositionName", compositionName)

	// Check if we've already loaded revisions for this composition
	matchingRevisions, cached := c.revisionsByComposition[compositionName]
	if !cached {
		// Load revisions for this specific composition using label selector
		c.logger.Debug("Loading revisions for composition", "compositionName", compositionName)

		revisions, err := c.listCompositionRevisionsForComposition(ctx, compositionName)
		if err != nil {
			return nil, errors.Wrap(err, "cannot list composition revisions")
		}

		matchingRevisions = revisions

		// Cache by name for individual lookups
		for _, rev := range matchingRevisions {
			c.revisions[rev.GetName()] = rev
		}

		// Cache the filtered list even if empty (to avoid re-querying)
		c.revisionsByComposition[compositionName] = matchingRevisions
	}

	if len(matchingRevisions) == 0 {
		return nil, errors.Errorf("no composition revisions found for composition %s", compositionName)
	}

	// Sort by revision number (highest first)
	sort.Slice(matchingRevisions, func(i, j int) bool {
		return matchingRevisions[i].Spec.Revision > matchingRevisions[j].Spec.Revision
	})

	latest := matchingRevisions[0]

	// Validate that we don't have duplicate revision numbers (would indicate a serious error)
	if len(matchingRevisions) > 1 && matchingRevisions[0].Spec.Revision == matchingRevisions[1].Spec.Revision {
		return nil, errors.Errorf(
			"multiple composition revisions found with the same revision number %d for composition %s (revisions: %s, %s) - this indicates a serious error in the Crossplane runtime",
			latest.Spec.Revision, compositionName, matchingRevisions[0].GetName(), matchingRevisions[1].GetName())
	}

	c.logger.Debug("Found latest revision",
		"compositionName", compositionName,
		"revisionName", latest.GetName(),
		"revisionNumber", latest.Spec.Revision)

	return latest, nil
}

// GetCompositionFromRevision extracts a Composition from a CompositionRevision.
// CompositionRevision contains the full Composition spec, so we construct a Composition object.
func (c *DefaultCompositionRevisionClient) GetCompositionFromRevision(revision *apiextensionsv1.CompositionRevision) *apiextensionsv1.Composition {
	if revision == nil {
		return nil
	}

	comp := &apiextensionsv1.Composition{
		Spec: apiextensionsv1.CompositionSpec{
			CompositeTypeRef:                  revision.Spec.CompositeTypeRef,
			Mode:                              revision.Spec.Mode,
			Pipeline:                          revision.Spec.Pipeline,
			WriteConnectionSecretsToNamespace: revision.Spec.WriteConnectionSecretsToNamespace,
		},
	}

	// Copy metadata from the revision to the composition
	// Use the composition name from the label if available
	if labels := revision.GetLabels(); labels != nil {
		if compositionName := labels[LabelCompositionName]; compositionName != "" {
			comp.SetName(compositionName)
		}
	}

	// If we couldn't get the name from labels, use the revision name (minus the hash suffix)
	if comp.GetName() == "" {
		comp.SetName(revision.GetName())
	}

	return comp
}
