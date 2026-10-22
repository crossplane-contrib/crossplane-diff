package crossplane

import (
	"context"
	"fmt"
	"sync"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
)

// FunctionClient handles operations related to Functions.
type FunctionClient interface {
	core.Initializable

	// GetFunctionsFromPipeline gets functions used in a composition pipeline, fetching each one the
	// pipeline names from the cluster the first time it is asked for.
	GetFunctionsFromPipeline(ctx context.Context, comp *apiextensionsv1.Composition) ([]pkgv1.Function, error)

	// ListFunctions lists all functions in the cluster
	ListFunctions(ctx context.Context) ([]pkgv1.Function, error)
}

// DefaultFunctionClient implements FunctionClient.
type DefaultFunctionClient struct {
	resourceClient kubernetes.ResourceClient
	logger         logging.Logger

	// Cache of functions by name: those found, and the names the cluster has no Function for.
	// functionsMu guards both, and is never held across a cluster request.
	functions   map[string]pkgv1.Function
	missing     map[string]bool
	functionsMu sync.Mutex
	gvks        []schema.GroupVersionKind
}

// functionGVK returns the one Function version this client reads, as typed pkgv1.Functions.
func functionGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: CrossplanePkgGroup, Version: "v1", Kind: FunctionKind}
}

// NewFunctionClient creates a new DefaultFunctionClient.
func NewFunctionClient(resourceClient kubernetes.ResourceClient, logger logging.Logger) FunctionClient {
	return &DefaultFunctionClient{
		resourceClient: resourceClient,
		logger:         logger,
		functions:      make(map[string]pkgv1.Function),
		missing:        make(map[string]bool),
	}
}

// Initialize discovers the served Function versions. Functions are fetched on demand by
// GetFunctionsFromPipeline.
func (c *DefaultFunctionClient) Initialize(ctx context.Context) error {
	c.logger.Debug("Initializing function client")

	gvks, err := c.resourceClient.GetGVKsForGroupKind(ctx, CrossplanePkgGroup, FunctionKind)
	if err != nil {
		return errors.Wrap(err, "cannot get Function GVKs")
	}

	c.gvks = gvks

	c.logger.Debug("Function client initialized")

	return nil
}

// ListFunctions lists all functions in the cluster.
func (c *DefaultFunctionClient) ListFunctions(ctx context.Context) ([]pkgv1.Function, error) {
	c.logger.Debug("Listing functions from cluster")

	// TODO:  we don't actually use our cached GVKs here -- but there's only one version of Function
	// and this part is strongly typed which will make a second version hard to handle

	// Get all functions using the resource client
	unFns, err := c.resourceClient.ListResources(ctx, functionGVK(), "")
	if err != nil {
		c.logger.Debug("Failed to list functions", "error", err)
		return nil, errors.Wrap(err, "cannot list functions from cluster")
	}

	// Convert unstructured to typed
	functions := make([]pkgv1.Function, 0, len(unFns))
	for _, obj := range unFns {
		fn := pkgv1.Function{}

		err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &fn)
		if err != nil {
			c.logger.Debug("Failed to convert function from unstructured",
				"name", obj.GetName(),
				"error", err)

			return nil, errors.Wrap(err, "cannot convert unstructured to Function")
		}

		functions = append(functions, fn)
	}

	c.logger.Debug("Successfully retrieved functions", "count", len(functions))

	return functions, nil
}

// GetFunctionsFromPipeline gets functions used in a composition pipeline.
func (c *DefaultFunctionClient) GetFunctionsFromPipeline(ctx context.Context, comp *apiextensionsv1.Composition) ([]pkgv1.Function, error) {
	c.logger.Debug("Getting functions from pipeline", "composition_name", comp.GetName())

	if comp.Spec.Mode != apiextensionsv1.CompositionModePipeline {
		c.logger.Debug("Composition is not in pipeline mode",
			"composition_name", comp.GetName(),
			"mode", func() string {
				// TODO:  there was nil handling here that became invalid; make sure everything works as expected
				return string(comp.Spec.Mode)
			}())

		return nil, fmt.Errorf("unsupported composition Mode '%s'; supported types are [%s]", comp.Spec.Mode, apiextensionsv1.CompositionModePipeline)
		// TODO:  we used to check for nil, and if nil we'd say "no mode found"; is it valid to have no mode?
	}

	functions := make([]pkgv1.Function, 0, len(comp.Spec.Pipeline))
	c.logger.Debug("Processing pipeline steps", "steps_count", len(comp.Spec.Pipeline))

	for _, step := range comp.Spec.Pipeline {
		fn, ok, err := c.getFunction(ctx, step.FunctionRef.Name)
		if err != nil {
			return nil, err
		}

		if !ok {
			c.logger.Debug("Function not found",
				"step", step.Step,
				"function_name", step.FunctionRef.Name)

			return nil, errors.Errorf("function %q referenced in pipeline step %q not found", step.FunctionRef.Name, step.Step)
		}

		c.logger.Debug("Found function for step",
			"step", step.Step,
			"function_name", fn.GetName())
		// A copy, so a caller that modifies what it gets (a function's annotations, its package)
		// does not modify this cache, which concurrent callers share.
		functions = append(functions, *fn.DeepCopy())
	}

	c.logger.Debug("Retrieved functions from pipeline",
		"functions_count", len(functions),
		"composition_name", comp.GetName())

	return functions, nil
}

// getFunction returns the named Function, from the cache or else from the cluster, and whether it
// exists. Both outcomes are cached, so each name is fetched at most once.
func (c *DefaultFunctionClient) getFunction(ctx context.Context, name string) (pkgv1.Function, bool, error) {
	c.functionsMu.Lock()
	fn, found := c.functions[name]
	missing := c.missing[name]
	c.functionsMu.Unlock()

	if found || missing {
		return fn, found, nil
	}

	u, err := c.resourceClient.GetResource(ctx, functionGVK(), "", name)
	if apierrors.IsNotFound(err) {
		c.functionsMu.Lock()
		c.missing[name] = true
		c.functionsMu.Unlock()

		return pkgv1.Function{}, false, nil
	}

	if err != nil {
		return pkgv1.Function{}, false, errors.Wrapf(err, "cannot get function %q", name)
	}

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &fn); err != nil {
		return pkgv1.Function{}, false, errors.Wrap(err, "cannot convert unstructured to Function")
	}

	c.functionsMu.Lock()
	c.functions[name] = fn
	c.functionsMu.Unlock()

	return fn, true, nil
}
