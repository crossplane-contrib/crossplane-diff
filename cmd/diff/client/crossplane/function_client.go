package crossplane

import (
	"context"
	"fmt"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
)

// FunctionClient handles operations related to Functions.
type FunctionClient interface {
	core.Initializable

	// GetFunctionsFromPipeline gets functions used in a composition pipeline
	GetFunctionsFromPipeline(comp *apiextensionsv1.Composition) ([]pkgv1.Function, error)

	// ListFunctions lists all functions in the cluster
	ListFunctions(ctx context.Context) ([]pkgv1.Function, error)
}

// DefaultFunctionClient implements FunctionClient.
type DefaultFunctionClient struct {
	resourceClient kubernetes.ResourceClient
	logger         logging.Logger

	// Cache of functions
	functions map[string]pkgv1.Function
	gvks      []schema.GroupVersionKind
}

// NewFunctionClient creates a new DefaultFunctionClient.
func NewFunctionClient(resourceClient kubernetes.ResourceClient, logger logging.Logger) FunctionClient {
	return &DefaultFunctionClient{
		resourceClient: resourceClient,
		logger:         logger,
		functions:      make(map[string]pkgv1.Function),
	}
}

// Initialize loads functions into the cache.
func (c *DefaultFunctionClient) Initialize(ctx context.Context) error {
	c.logger.Debug("Initializing function client")

	gvks, err := c.resourceClient.GetGVKsForGroupKind(ctx, "apiextensions.crossplane.io", "Function")
	if err != nil {
		return errors.Wrap(err, "cannot get Function GVKs")
	}

	c.gvks = gvks

	// List functions to populate the cache
	fns, err := c.ListFunctions(ctx)
	if err != nil {
		return errors.Wrap(err, "cannot list functions")
	}

	// Store in cache
	for _, fn := range fns {
		c.functions[fn.GetName()] = fn
	}

	c.logger.Debug("Function client initialized", "functionsCount", len(c.functions))

	return nil
}

// ListFunctions lists all functions in the cluster.
func (c *DefaultFunctionClient) ListFunctions(ctx context.Context) ([]pkgv1.Function, error) {
	c.logger.Debug("Listing functions from cluster")

	// TODO:  we don't actually use our cached GVKs here -- but there's only one version of Function
	// and this part is strongly typed which will make a second version hard to handle

	// Define the function GVK
	gvk := schema.GroupVersionKind{
		Group:   "pkg.crossplane.io",
		Version: "v1",
		Kind:    "Function",
	}

	// Get all functions using the resource client
	unFns, err := c.resourceClient.ListResources(ctx, gvk, "")
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
func (c *DefaultFunctionClient) GetFunctionsFromPipeline(comp *apiextensionsv1.Composition) ([]pkgv1.Function, error) {
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
		fn, ok := c.functions[step.FunctionRef.Name]
		if !ok {
			c.logger.Debug("Function not found",
				"step", step.Step,
				"function_name", step.FunctionRef.Name)

			return nil, errors.Errorf("function %q referenced in pipeline step %q not found", step.FunctionRef.Name, step.Step)
		}

		c.logger.Debug("Found function for step",
			"step", step.Step,
			"function_name", fn.GetName())
		functions = append(functions, fn)
	}

	c.logger.Debug("Retrieved functions from pipeline",
		"functions_count", len(functions),
		"composition_name", comp.GetName())

	return functions, nil
}
