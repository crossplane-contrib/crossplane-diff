package diffprocessor

import (
	"sync"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// ProcessorConfig contains configuration for the DiffProcessor.
type ProcessorConfig struct {
	// Namespace is the namespace to use for resources
	Namespace string

	// Colorize determines whether to use colors in the diff output
	Colorize bool

	// Compact determines whether to show a compact diff format
	Compact bool

	// MaxNestedDepth is the maximum depth for recursive nested XR processing
	MaxNestedDepth int

	// IncludeManual determines whether to include XRs with Manual update policy in composition diffs
	IncludeManual bool

	// Logger is the logger to use
	Logger logging.Logger

	// RenderFunc is the function to use for rendering resources
	RenderFunc RenderFunc

	// RenderMutex is the mutex used to serialize render operations (for internal use)
	RenderMutex *sync.Mutex

	// Factories provide factory functions for creating components
	Factories ComponentFactories
}

// ComponentFactories contains factory functions for creating processor components.
type ComponentFactories struct {
	// ResourceManager creates a ResourceManager
	ResourceManager func(client k8.ResourceClient, defClient xp.DefinitionClient, logger logging.Logger) ResourceManager

	// SchemaValidator creates a SchemaValidator
	SchemaValidator func(schema k8.SchemaClient, def xp.DefinitionClient, logger logging.Logger) SchemaValidator

	// DiffCalculator creates a DiffCalculator
	DiffCalculator func(apply k8.ApplyClient, tree xp.ResourceTreeClient, resourceManager ResourceManager, logger logging.Logger, diffOptions renderer.DiffOptions) DiffCalculator

	// DiffRenderer creates a DiffRenderer
	DiffRenderer func(logger logging.Logger, diffOptions renderer.DiffOptions) renderer.DiffRenderer

	// RequirementsProvider creates an ExtraResourceProvider
	RequirementsProvider func(res k8.ResourceClient, def xp.EnvironmentClient, renderFunc RenderFunc, logger logging.Logger) *RequirementsProvider

	// FunctionProvider creates a FunctionProvider
	FunctionProvider func(fnClient xp.FunctionClient, logger logging.Logger) FunctionProvider

	// DiffProcessor creates a DiffProcessor (used by CompDiffProcessor)
	DiffProcessor func(k8Clients k8.Clients, xpClients xp.Clients, opts []ProcessorOption) DiffProcessor
}

// ProcessorOption defines a function that can modify a ProcessorConfig.
type ProcessorOption func(*ProcessorConfig)

// WithNamespace sets the namespace for the processor.
func WithNamespace(namespace string) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Namespace = namespace
	}
}

// WithColorize sets whether to use colors in diff output.
func WithColorize(colorize bool) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Colorize = colorize
	}
}

// WithCompact sets whether to use compact diff format.
func WithCompact(compact bool) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Compact = compact
	}
}

// WithMaxNestedDepth sets the maximum depth for recursive nested XR processing.
func WithMaxNestedDepth(depth int) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.MaxNestedDepth = depth
	}
}

// WithIncludeManual sets whether to include XRs with Manual update policy in composition diffs.
func WithIncludeManual(includeManual bool) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.IncludeManual = includeManual
	}
}

// WithLogger sets the logger for the processor.
func WithLogger(logger logging.Logger) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Logger = logger
	}
}

// WithRenderFunc sets the render function for the processor.
func WithRenderFunc(renderFn RenderFunc) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.RenderFunc = renderFn
	}
}

// WithRenderMutex sets the mutex for serializing render operations.
func WithRenderMutex(mu *sync.Mutex) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.RenderMutex = mu
	}
}

// WithResourceManagerFactory sets the ResourceManager factory function.
func WithResourceManagerFactory(factory func(k8.ResourceClient, xp.DefinitionClient, logging.Logger) ResourceManager) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Factories.ResourceManager = factory
	}
}

// WithSchemaValidatorFactory sets the SchemaValidator factory function.
func WithSchemaValidatorFactory(factory func(k8.SchemaClient, xp.DefinitionClient, logging.Logger) SchemaValidator) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Factories.SchemaValidator = factory
	}
}

// WithDiffCalculatorFactory sets the DiffCalculator factory function.
func WithDiffCalculatorFactory(factory func(k8.ApplyClient, xp.ResourceTreeClient, ResourceManager, logging.Logger, renderer.DiffOptions) DiffCalculator) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Factories.DiffCalculator = factory
	}
}

// WithDiffRendererFactory sets the DiffRenderer factory function.
func WithDiffRendererFactory(factory func(logging.Logger, renderer.DiffOptions) renderer.DiffRenderer) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Factories.DiffRenderer = factory
	}
}

// WithRequirementsProviderFactory sets the RequirementsProvider factory function.
func WithRequirementsProviderFactory(factory func(k8.ResourceClient, xp.EnvironmentClient, RenderFunc, logging.Logger) *RequirementsProvider) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Factories.RequirementsProvider = factory
	}
}

// WithFunctionProviderFactory sets the FunctionProvider factory function.
func WithFunctionProviderFactory(factory func(xp.FunctionClient, logging.Logger) FunctionProvider) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Factories.FunctionProvider = factory
	}
}

// WithDiffProcessorFactory sets the DiffProcessor factory function.
func WithDiffProcessorFactory(factory func(k8.Clients, xp.Clients, []ProcessorOption) DiffProcessor) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Factories.DiffProcessor = factory
	}
}

// GetDiffOptions returns DiffOptions based on the ProcessorConfig.
func (c *ProcessorConfig) GetDiffOptions() renderer.DiffOptions {
	opts := renderer.DefaultDiffOptions()
	opts.UseColors = c.Colorize
	opts.Compact = c.Compact

	return opts
}

// SetDefaultFactories sets default component factory functions if not already set.
func (c *ProcessorConfig) SetDefaultFactories() {
	if c.Factories.ResourceManager == nil {
		c.Factories.ResourceManager = NewResourceManager
	}

	if c.Factories.SchemaValidator == nil {
		c.Factories.SchemaValidator = NewSchemaValidator
	}

	if c.Factories.DiffCalculator == nil {
		c.Factories.DiffCalculator = NewDiffCalculator
	}

	if c.Factories.DiffRenderer == nil {
		c.Factories.DiffRenderer = renderer.NewDiffRenderer
	}

	if c.Factories.RequirementsProvider == nil {
		c.Factories.RequirementsProvider = NewRequirementsProvider
	}

	if c.Factories.FunctionProvider == nil {
		c.Factories.FunctionProvider = NewDefaultFunctionProvider
	}
}
