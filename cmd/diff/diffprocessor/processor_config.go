package diffprocessor

import (
	"io"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	corev1 "k8s.io/api/core/v1"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// DefaultMaxRenderIterations is the default maximum render iterations.
const DefaultMaxRenderIterations = 20

// AnalyzeOn names the smallest composition change that triggers per-composite impact analysis.
// The values are ordered from least to most eager.
type AnalyzeOn string

const (
	// AnalyzeOnSpecChange evaluates composites only when the composition's spec changes. The
	// cheapest setting, and a deliberate user judgement: a metadata-only change still produces a new
	// CompositionRevision, whose identity a composition template can observe through the XR's
	// compositionRevisionRef, so choosing this asserts that none of your compositions do that.
	AnalyzeOnSpecChange AnalyzeOn = "spec-change"

	// AnalyzeOnAnyChange evaluates composites whenever anything Crossplane hashes into the
	// composition's identity differs — labels and annotations as well as spec. The default, because
	// whether a metadata-only change reaches rendered output can only be settled by rendering.
	AnalyzeOnAnyChange AnalyzeOn = "any-change"

	// AnalyzeOnAlways evaluates composites even when the composition is identical, which creates no
	// revision and so cannot change anything. Useful as a pre-edit "is my cluster converged?"
	// baseline; note that any delta it reports is by construction not caused by this composition.
	AnalyzeOnAlways AnalyzeOn = "always"
)

// DryRunOn selects which resources are round-tripped through the apiserver to compute their
// post-apply form. Like AnalyzeOn it is a depth/cost knob: every resource dry-run costs one
// apiserver round-trip and requires a permission, and a resource left un-dry-run is marked with a
// DryRunInfo so "we did not check" stays distinguishable from "we checked and it matches".
type DryRunOn string

const (
	// DryRunOnExisting dry-runs only resources that already exist in the cluster, leaving additions
	// as the render pipeline produced them. This was the only behaviour before crossplane-diff#334.
	// It needs no create permission, and costs one round-trip fewer per added resource.
	DryRunOnExisting DryRunOn = "existing"

	// DryRunOnAll additionally dry-run creates added resources, so their diffs pick up
	// apiserver-side defaulting and mutating admission — neither of which the local schema
	// validator's applyCRDDefaults can supply for a built-in type, and neither of which it can
	// supply at all for admission. The default. Where the create is not permitted, the resource
	// degrades to rendered output and says so rather than failing the run.
	DryRunOnAll DryRunOn = "all"
)

// ChangeScope is how much of a composition differs from its in-cluster version, in terms of what
// Crossplane hashes into the composition's identity (see Composition.Hash()).
type ChangeScope string

const (
	// ChangeScopeNone means nothing Crossplane hashes differs. No CompositionRevision is created, so
	// nothing can adopt anything.
	ChangeScopeNone ChangeScope = "none"

	// ChangeScopeMetadata means the labels or annotations differ but the spec does not. A new
	// CompositionRevision is still created, carrying an identical spec.
	ChangeScopeMetadata ChangeScope = "metadata"

	// ChangeScopeSpec means the spec differs.
	ChangeScopeSpec ChangeScope = "spec"
)

// triggersAnalysis reports whether a change of this scope should cause per-composite impact
// analysis at the given setting.
func (s ChangeScope) triggersAnalysis(on AnalyzeOn) bool {
	switch on {
	case AnalyzeOnAlways:
		return true
	case AnalyzeOnSpecChange:
		return s == ChangeScopeSpec
	case AnalyzeOnAnyChange, "":
		return s != ChangeScopeNone
	default:
		// Unrecognized values are rejected by the CLI; fall back to the default rather than
		// silently analysing nothing.
		return s != ChangeScopeNone
	}
}

// ProcessorConfig contains configuration for the DiffProcessor.
type ProcessorConfig struct {
	// Colorize determines whether to use colors in the diff output
	Colorize bool

	// Compact determines whether to show a compact diff format
	Compact bool

	// OutputFormat specifies the output format for diffs (diff, json, yaml)
	OutputFormat renderer.OutputFormat

	// MaxNestedDepth is the maximum depth for recursive nested XR processing
	MaxNestedDepth int

	// IncludeManual determines whether to include XRs with Manual update policy in composition diffs
	IncludeManual bool

	// MinimizeComposition collapses composition changes to a single marker line per
	// composition, omitting the full YAML diff body. Human renderer only; structured
	// output always includes full compositionChanges.
	MinimizeComposition bool

	// Warnings is the source of non-fatal advisories to include in structured output. It is the same
	// *WarningLogger that Logger is set to when the CLI wires one up; keeping a typed handle avoids
	// type-asserting the Logger back to its concrete type at drain time. Nil is valid and means no
	// warnings are collected — the human-visible stderr line is emitted by the WarningLogger itself,
	// so leaving this unset only affects structured output.
	Warnings *WarningLogger

	// AnalyzeOn is the smallest composition change that triggers per-composite impact analysis.
	// Evaluating a composite costs one function render, so this is a cost knob — it never suppresses
	// a reported consequence. RevisionImpact is populated at every setting, and composites left
	// unevaluated are marked ImpactAnalysisSkipped so "we did not look" stays distinguishable from
	// "we looked and found nothing".
	//
	// Zero value means AnalyzeOnAnyChange, matching the CLI default.
	AnalyzeOn AnalyzeOn

	// DryRunOn selects which resources are verified against the apiserver. Zero value means
	// DryRunOnAll, matching the CLI default.
	DryRunOn DryRunOn

	// EventualState enables iterative simulation to show eventual state after all reconciliation
	// cycles complete. Useful with function-sequencer which hides later stage resources.
	EventualState bool

	// MaxRenderIterations is the maximum number of render iterations when resolving requirements
	// or simulating eventual state. Higher values may be needed for complex pipelines.
	MaxRenderIterations int

	// IgnorePaths is a list of paths to ignore when calculating diffs
	IgnorePaths []string

	// FunctionCredentials holds Secret credentials to pass to Functions during rendering
	FunctionCredentials []corev1.Secret

	// FunctionRegistryOverride overrides the registry in all function package refs.
	FunctionRegistryOverride string

	// Stdout is the writer for diff output (defaults to os.Stdout)
	Stdout io.Writer

	// Stderr is the writer for error output (defaults to os.Stderr)
	Stderr io.Writer

	// Logger is the logger to use
	Logger logging.Logger

	// RenderFunc is the function to use for rendering resources. If left nil,
	// processors construct a default engine-backed RenderFn on initialization.
	RenderFunc RenderFn

	// CrossplaneRenderBinary, when non-empty, causes the default
	// engine-backed RenderFn to invoke a local `crossplane` binary at the
	// supplied path instead of the upstream docker engine. This is a test
	// affordance that gives integration tests a fast in-process render path;
	// production users should leave it empty so the docker engine pulls
	// xpkg.crossplane.io/crossplane/crossplane:stable. Ignored when
	// RenderFunc is set explicitly.
	CrossplaneRenderBinary string

	// CrossplaneVersion, when non-empty, causes the default engine-backed
	// RenderFn's docker engine to pull
	// xpkg.crossplane.io/crossplane/crossplane:<version> instead of :stable.
	// Mutually exclusive with CrossplaneImage and CrossplaneRenderBinary.
	// Ignored when RenderFunc is set explicitly.
	CrossplaneVersion string

	// CrossplaneImage, when non-empty, causes the default engine-backed
	// RenderFn's docker engine to pull this full image reference instead of
	// the default xpkg.crossplane.io/crossplane/crossplane:stable. Mutually
	// exclusive with CrossplaneVersion and CrossplaneRenderBinary. Ignored
	// when RenderFunc is set explicitly.
	CrossplaneImage string

	// Factories provide factory functions for creating components
	Factories ComponentFactories
}

// ComponentFactories contains factory functions for creating processor components.
type ComponentFactories struct {
	// ResourceManager creates a ResourceManager
	ResourceManager func(client k8.ResourceClient, defClient xp.DefinitionClient, treeClient xp.ResourceTreeClient, logger logging.Logger) ResourceManager

	// SchemaValidator creates a SchemaValidator
	SchemaValidator func(schema k8.SchemaClient, resource k8.ResourceClient, def xp.DefinitionClient, logger logging.Logger) SchemaValidator

	// DiffCalculator creates a DiffCalculator
	DiffCalculator func(apply k8.ApplyClient, access k8.AccessChecker, tree xp.ResourceTreeClient, resourceManager ResourceManager, logger logging.Logger, diffOptions renderer.DiffOptions, dryRunOn DryRunOn) DiffCalculator

	// DiffRenderer creates a DiffRenderer
	DiffRenderer func(logger logging.Logger, diffOptions renderer.DiffOptions) renderer.DiffRenderer

	// CompDiffRenderer creates a CompDiffRenderer for composition diffs
	CompDiffRenderer func(logger logging.Logger, diffRenderer renderer.DiffRenderer, opts renderer.DiffOptions) renderer.CompDiffRenderer

	// RequirementsProvider creates an ExtraResourceProvider
	RequirementsProvider func(res k8.ResourceClient, def xp.EnvironmentClient, logger logging.Logger) *RequirementsProvider

	// FunctionProvider creates a FunctionProvider
	FunctionProvider func(fnClient xp.FunctionClient, logger logging.Logger) FunctionProvider
}

// ProcessorOption defines a function that can modify a ProcessorConfig.
type ProcessorOption func(*ProcessorConfig)

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

// WithOutputFormat sets the output format for diffs.
func WithOutputFormat(format renderer.OutputFormat) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.OutputFormat = format
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

// WithWarnings sets the collector whose warnings are included in structured output. Pass the same
// *WarningLogger that was supplied to WithLogger.
func WithWarnings(warnings *WarningLogger) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Warnings = warnings
	}
}

// WithAnalyzeOn sets the smallest composition change that triggers per-composite impact analysis
// (see ProcessorConfig.AnalyzeOn). An empty value leaves the default in place.
func WithAnalyzeOn(analyzeOn AnalyzeOn) ProcessorOption {
	return func(config *ProcessorConfig) {
		if analyzeOn != "" {
			config.AnalyzeOn = analyzeOn
		}
	}
}

// WithDryRunOn sets which resources are round-tripped through the apiserver. An empty value leaves
// the config's default in place, matching WithAnalyzeOn.
func WithDryRunOn(dryRunOn DryRunOn) ProcessorOption {
	return func(config *ProcessorConfig) {
		if dryRunOn != "" {
			config.DryRunOn = dryRunOn
		}
	}
}

// WithMinimizeComposition sets whether to collapse composition changes to a single marker line.
func WithMinimizeComposition(minimize bool) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.MinimizeComposition = minimize
	}
}

// WithEventualState sets whether to show eventual state after all reconciliation cycles complete.
// When enabled, the processor runs an iterative simulation that synthesizes Ready status on
// rendered resources until no new resources appear. This is useful with function-sequencer
// which hides later stage resources until earlier stages become Ready.
func WithEventualState(enabled bool) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.EventualState = enabled
	}
}

// WithMaxRenderIterations sets the maximum number of render iterations when resolving requirements
// or simulating eventual state.
func WithMaxRenderIterations(maxIterations int) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.MaxRenderIterations = maxIterations
	}
}

// WithIgnorePaths sets the paths to ignore when calculating diffs.
func WithIgnorePaths(ignorePaths []string) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.IgnorePaths = ignorePaths
	}
}

// WithFunctionCredentials sets the credentials to pass to Functions during rendering.
func WithFunctionCredentials(creds []corev1.Secret) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.FunctionCredentials = creds
	}
}

// WithFunctionRegistryOverride overrides the registry in all function package refs.
func WithFunctionRegistryOverride(registry string) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.FunctionRegistryOverride = registry
	}
}

// WithStdout sets the writer for diff output.
func WithStdout(w io.Writer) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Stdout = w
	}
}

// WithStderr sets the writer for error output.
func WithStderr(w io.Writer) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Stderr = w
	}
}

// WithLogger sets the logger for the processor.
func WithLogger(logger logging.Logger) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Logger = logger
	}
}

// WithRenderFunc sets the render function for the processor.
func WithRenderFunc(renderFn RenderFn) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.RenderFunc = renderFn
	}
}

// WithCrossplaneRenderBinary points the default engine-backed RenderFn at a
// local `crossplane` binary instead of the upstream docker engine. See
// ProcessorConfig.CrossplaneRenderBinary for the semantics — production
// callers should not use this; it exists for fast integration-test iteration.
func WithCrossplaneRenderBinary(path string) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.CrossplaneRenderBinary = path
	}
}

// WithCrossplaneVersion pins the crossplane render version the default
// engine-backed RenderFn's docker engine pulls (…/crossplane:<version>).
// See ProcessorConfig.CrossplaneVersion. Mutually exclusive with
// WithCrossplaneImage and WithCrossplaneRenderBinary.
func WithCrossplaneVersion(version string) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.CrossplaneVersion = version
	}
}

// WithCrossplaneImage pins the full crossplane render image reference the
// default engine-backed RenderFn's docker engine pulls. See
// ProcessorConfig.CrossplaneImage. Mutually exclusive with
// WithCrossplaneVersion and WithCrossplaneRenderBinary.
func WithCrossplaneImage(image string) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.CrossplaneImage = image
	}
}

// WithResourceManagerFactory sets the ResourceManager factory function.
func WithResourceManagerFactory(factory func(k8.ResourceClient, xp.DefinitionClient, xp.ResourceTreeClient, logging.Logger) ResourceManager) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Factories.ResourceManager = factory
	}
}

// WithSchemaValidatorFactory sets the SchemaValidator factory function.
func WithSchemaValidatorFactory(factory func(k8.SchemaClient, k8.ResourceClient, xp.DefinitionClient, logging.Logger) SchemaValidator) ProcessorOption {
	return func(config *ProcessorConfig) {
		config.Factories.SchemaValidator = factory
	}
}

// WithDiffCalculatorFactory sets the DiffCalculator factory function.
func WithDiffCalculatorFactory(factory func(k8.ApplyClient, k8.AccessChecker, xp.ResourceTreeClient, ResourceManager, logging.Logger, renderer.DiffOptions, DryRunOn) DiffCalculator) ProcessorOption {
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
func WithRequirementsProviderFactory(factory func(k8.ResourceClient, xp.EnvironmentClient, logging.Logger) *RequirementsProvider) ProcessorOption {
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

// GetDiffOptions returns DiffOptions based on the ProcessorConfig.
func (c *ProcessorConfig) GetDiffOptions() renderer.DiffOptions {
	opts := renderer.DefaultDiffOptions()
	opts.UseColors = c.Colorize
	opts.Compact = c.Compact
	opts.MinimizeComposition = c.MinimizeComposition

	opts.IgnorePaths = c.IgnorePaths
	if c.OutputFormat != "" {
		opts.Format = c.OutputFormat
	}

	// Use config's Stdout/Stderr if set, otherwise keep defaults (os.Stdout/os.Stderr)
	if c.Stdout != nil {
		opts.Stdout = c.Stdout
	}

	if c.Stderr != nil {
		opts.Stderr = c.Stderr
	}

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
		// Set the appropriate renderer factory based on output format
		switch c.OutputFormat {
		case renderer.OutputFormatJSON, renderer.OutputFormatYAML:
			c.Factories.DiffRenderer = renderer.NewStructuredDiffRenderer
		case renderer.OutputFormatDiff:
			c.Factories.DiffRenderer = renderer.NewDiffRenderer
		default:
			c.Factories.DiffRenderer = renderer.NewDiffRenderer
		}
	}

	if c.Factories.CompDiffRenderer == nil {
		// Set the appropriate renderer factory based on output format
		switch c.OutputFormat {
		case renderer.OutputFormatJSON, renderer.OutputFormatYAML:
			c.Factories.CompDiffRenderer = func(logger logging.Logger, _ renderer.DiffRenderer, opts renderer.DiffOptions) renderer.CompDiffRenderer {
				return renderer.NewStructuredCompDiffRenderer(logger, opts)
			}
		case renderer.OutputFormatDiff:
			fallthrough
		default:
			c.Factories.CompDiffRenderer = renderer.NewDefaultCompDiffRenderer
		}
	}

	if c.Factories.RequirementsProvider == nil {
		c.Factories.RequirementsProvider = NewRequirementsProvider
	}

	if c.Factories.FunctionProvider == nil {
		// Use CachedFunctionProvider by default for container reuse across renders.
		// This prevents container proliferation with --eventual-state (multiple iterations)
		// and enables reuse when diffing multiple XRs with the same composition.
		c.Factories.FunctionProvider = NewCachedFunctionProvider
	}
}
