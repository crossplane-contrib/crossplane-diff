package diffprocessor

import (
	"context"
	"fmt"
	"maps"
	"sync"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	"github.com/crossplane/cli/v2/cmd/crossplane/common/resource"
	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"
)

// DiffCalculator calculates differences between resources.
type DiffCalculator interface {
	// CalculateDiff computes the diff for a single resource
	CalculateDiff(ctx context.Context, composite *un.Unstructured, desired *un.Unstructured) (*dt.ResourceDiff, error)

	// CalculateDiffs computes all diffs including removals for the rendered resources.
	// This is the primary method that most code should use.
	CalculateDiffs(ctx context.Context, xr *cmp.Unstructured, desired render.CompositionOutputs) (map[string]*dt.ResourceDiff, error)

	// CalculateNonRemovalDiffs computes diffs for modified/added resources and returns
	// the set of rendered resource keys. This is used by nested XR processing.
	// parentComposite should be nil for root XRs, and the parent XR for nested XRs.
	// Returns: (diffs map, rendered resource keys, error)
	CalculateNonRemovalDiffs(ctx context.Context, xr *cmp.Unstructured, parentComposite *un.Unstructured, desired render.CompositionOutputs) (map[string]*dt.ResourceDiff, map[string]bool, error)

	// CalculateRemovedResourceDiffs identifies resources that exist in the cluster but are not
	// in the rendered set. This is called after nested XR processing is complete.
	CalculateRemovedResourceDiffs(ctx context.Context, xr *un.Unstructured, renderedResources map[string]bool) (map[string]*dt.ResourceDiff, error)
}

// DefaultDiffCalculator implements the DiffCalculator interface.
type DefaultDiffCalculator struct {
	treeClient      xp.ResourceTreeClient
	applyClient     k8.ApplyClient
	accessChecker   k8.AccessChecker
	resourceManager ResourceManager
	logger          logging.Logger
	diffOptions     renderer.DiffOptions
	dryRunOn        DryRunOn

	// warned deduplicates the user-facing warnings raised when a resource could not be verified
	// against the apiserver. A composition rendering forty new resources of one kind should say so
	// once, not forty times. Keyed by GVK + namespace + reason, and mutex-guarded because a single
	// calculator instance is reused across every XR in a run.
	warnedMu sync.Mutex
	warned   map[string]bool
}

// SetDiffOptions updates the diff options used by the calculator.
func (c *DefaultDiffCalculator) SetDiffOptions(options renderer.DiffOptions) {
	c.diffOptions = options
}

// NewDiffCalculator creates a new DefaultDiffCalculator.
func NewDiffCalculator(apply k8.ApplyClient, access k8.AccessChecker, tree xp.ResourceTreeClient, resourceManager ResourceManager, logger logging.Logger, diffOptions renderer.DiffOptions, dryRunOn DryRunOn) DiffCalculator {
	return &DefaultDiffCalculator{
		treeClient:      tree,
		applyClient:     apply,
		accessChecker:   access,
		resourceManager: resourceManager,
		logger:          logger,
		diffOptions:     diffOptions,
		dryRunOn:        dryRunOn,
		warned:          make(map[string]bool),
	}
}

// CalculateDiff calculates the diff for a single resource.
func (c *DefaultDiffCalculator) CalculateDiff(ctx context.Context, composite *un.Unstructured, desired *un.Unstructured) (*dt.ResourceDiff, error) {
	// Get resource identification information
	name := desired.GetName()
	generateName := desired.GetGenerateName()

	// Create a resource ID for logging purposes
	var resourceID string

	switch {
	case name != "":
		resourceID = fmt.Sprintf("%s/%s", desired.GetKind(), name)
	case generateName != "":
		resourceID = fmt.Sprintf("%s/%s(generated)", desired.GetKind(), generateName)
	default:
		resourceID = fmt.Sprintf("%s/<no-name>", desired.GetKind())
	}

	c.logger.Debug("Calculating diff", "resource", resourceID)

	// Fetch current object from cluster
	current, isNewObject, err := c.resourceManager.FetchCurrentObject(ctx, composite, desired)
	if err != nil {
		c.logger.Debug("Failed to fetch current object", "resource", resourceID, "error", err)
		return nil, errors.Wrap(err, "cannot fetch current object")
	}

	// Log the resource status
	if isNewObject {
		c.logger.Debug("Resource is new (not found in cluster)", "resource", resourceID)
	} else if current != nil {
		c.logger.Debug("Found existing resource",
			"resourceID", resourceID,
			"existingName", current.GetName(),
			"resourceVersion", current.GetResourceVersion(),
			"resource", current)
	}

	// Preserve existing resource identity for resources with generateName
	desired = c.preserveExistingResourceIdentity(current, desired, resourceID, name)

	// Preserve the composite label for ALL existing resources
	// This is critical because in Crossplane, all resources in a tree point to the ROOT composite,
	// not their immediate parent. We must never change this label.
	desired = c.preserveCompositeLabel(current, desired, resourceID)

	// Update owner references if needed (done after preserving existing labels)
	// IMPORTANT: For composed resources, the owner should be the XR, not a Claim.
	// When composite is the current XR from the cluster, we use it as the owner.
	// This ensures composed resources only have the XR as their controller owner.
	c.resourceManager.UpdateOwnerRefs(ctx, composite, desired)

	// Determine what the resource would look like after application. Both branches below assign it,
	// so it is declared without an initialiser.
	var wouldBeResult *un.Unstructured

	// dryRunInfo stays nil whenever the desired state did reach the apiserver, which is the success
	// case and the common one. See dt.DryRunInfo.
	var dryRunInfo *dt.DryRunInfo

	if current != nil {
		// Extract the Crossplane field owner from the existing object's managedFields.
		// This ensures our dry-run apply uses the same field owner as Crossplane,
		// which correctly handles field removal detection (SSA removes fields that
		// are owned by this manager but not present in the apply request).
		fieldOwner := k8.GetComposedFieldOwner(current)

		applyDesired := sanitizeForDryRun(desired)

		// Perform a dry-run apply to get the result after we'd apply
		c.logger.Debug("Performing dry-run apply",
			"resource", resourceID,
			"name", desired.GetName(),
			"fieldOwner", fieldOwner,
			"desired", applyDesired)

		wouldBeResult, err = c.applyClient.DryRunApply(ctx, applyDesired, fieldOwner)
		if err != nil {
			c.logger.Debug("Dry-run apply failed", "resource", resourceID, "error", err)
			return nil, c.classifyApplyFailure(ctx, desired, resourceID, err)
		}

		c.logger.Debug("Dry-run apply succeeded", "resource", resourceID, "result", wouldBeResult)
	} else {
		wouldBeResult, dryRunInfo, err = c.dryRunCreateAddition(ctx, desired, resourceID)
		if err != nil {
			return nil, err
		}
	}

	// Generate diff with the configured options
	diff, err := renderer.GenerateDiffWithOptions(ctx, current, wouldBeResult, c.logger, c.diffOptions)
	if err != nil {
		c.logger.Debug("Failed to generate diff", "resource", resourceID, "error", err)
		return nil, err
	}

	if diff != nil {
		diff.DryRun = dryRunInfo
	}

	// Log the outcome
	if diff != nil {
		c.logger.Debug("Diff generated",
			"resource", resourceID,
			"diffType", diff.DiffType,
			"hasChanges", diff.DiffType != dt.DiffTypeEqual)
	}

	return diff, nil
}

// CalculateNonRemovalDiffs computes diffs for modified/added resources and returns
// the set of rendered resource keys for removal detection.
//
// TWO-PHASE DIFF ALGORITHM:
// This method implements Phase 1 of our two-phase diff calculation. The two-phase
// approach is necessary to correctly handle nested XRs (Composite Resources that
// themselves create other Composite Resources).
//
// WHY TWO PHASES?
// When processing nested XRs, we must:
//  1. Phase 1 (this method): Calculate diffs for all rendered resources (adds/modifications)
//     and build a set of "rendered resource keys" that tracks what was generated
//  2. Phase 2 (CalculateRemovedResourceDiffs): Compare cluster state against rendered
//     resources to identify removals
//
// The separation is critical because:
//   - Nested XRs are processed recursively BETWEEN these phases
//   - Nested XRs generate additional composed resources that must be added to the
//     "rendered resources" set before removal detection
//   - If we detected removals too early, we'd falsely identify nested XR resources
//     as "to be removed" before they've been processed
//
// EXAMPLE SCENARIO:
//
//	Parent XR renders: [Resource-A, NestedXR-B]
//	NestedXR-B renders: [Resource-C, Resource-D]
//
//	Without two phases:
//	  - We'd see cluster has [Resource-A, Resource-C, Resource-D] from prior render
//	  - We'd see new render has [Resource-A, NestedXR-B]
//	  - We'd INCORRECTLY mark Resource-C and Resource-D as removed
//
//	With two phases:
//	  Phase 1: Calculate diffs for [Resource-A, NestedXR-B], track as rendered
//	  Process nested: Recurse into NestedXR-B, add Resource-C and Resource-D to rendered set
//	  Phase 2: Now see [Resource-A, NestedXR-B, Resource-C, Resource-D] as rendered
//	           No false removal detection!
//
// Returns: (diffs map, rendered resource keys, error).
func (c *DefaultDiffCalculator) CalculateNonRemovalDiffs(ctx context.Context, xr *cmp.Unstructured, parentComposite *un.Unstructured, desired render.CompositionOutputs) (map[string]*dt.ResourceDiff, map[string]bool, error) {
	xrName := xr.GetName()
	c.logger.Debug("Calculating diffs",
		"xr", xrName,
		"composedCount", len(desired.ComposedResources))

	diffs := make(map[string]*dt.ResourceDiff)

	var errs []error

	renderedResources := make(map[string]bool)

	// Determine if this is a nested XR or root XR, and select the appropriate XR to diff
	if desired.CompositeResource == nil {
		return nil, nil, errors.New("render produced no composite resource (possible fatal pipeline error)")
	}

	renderedXR := desired.CompositeResource.GetUnstructured()

	var (
		desiredXR       *un.Unstructured
		compositeParent *un.Unstructured
	)

	if renderedXR.GetAnnotations()["crossplane.io/composition-resource-name"] != "" {
		// NESTED XR: Use rendered XR (it's a composed resource from parent's composition)
		c.logger.Debug("Processing nested XR", "xr", xrName, "hasParent", parentComposite != nil)

		desiredXR = renderedXR
		compositeParent = parentComposite
	} else {
		// ROOT XR: Use input XR as-is (source of truth, don't use rendered metadata)
		c.logger.Debug("Processing root XR", "xr", xrName)

		desiredXR = xr.GetUnstructured()
		compositeParent = nil
	}

	// Calculate diff for the XR
	xrDiff, err := c.CalculateDiff(ctx, compositeParent, desiredXR)
	if err != nil || xrDiff == nil {
		return nil, nil, errors.Wrap(err, "cannot calculate diff for XR")
	}

	// Always store the XR diff, even when DiffTypeEqual.
	// We need xrDiff.Current.Raw for removal detection downstream, regardless of whether the XR itself changed.
	key := xrDiff.GetDiffKey()
	diffs[key] = xrDiff

	// Then calculate diffs for all composed resources
	for _, d := range desired.ComposedResources {
		un := &un.Unstructured{Object: d.UnstructuredContent()}

		// Generate a key to identify this resource
		apiVersion := un.GetAPIVersion()
		kind := un.GetKind()
		name := un.GetName()
		generateName := un.GetGenerateName()

		// For logging purposes - create a resource ID that might use generateName
		resourceID := fmt.Sprintf("%s/%s", kind, name)
		if name == "" && generateName != "" {
			resourceID = fmt.Sprintf("%s/%s*", kind, generateName)
		}

		// For resources using generateName, the name will be empty but we shouldn't skip them
		// Only skip if both name and generateName are empty (likely a template issue)
		if name == "" && generateName == "" {
			c.logger.Debug("Skipping resource with empty name and generateName",
				"kind", kind,
				"apiVersion", apiVersion)

			continue
		}

		// For new XRs (xrDiff.Current.Raw is nil) fall back to the input XR as the
		// composite parent so UpdateOwnerRefs can assign a placeholder UID to
		// composed resources' owner references. Without this, dry-run apply on
		// an existing composed resource fails with
		// `metadata.ownerReferences.uid: Invalid value: "": must not be empty`,
		// because the render pipeline emits empty UIDs when the XR has none.
		//
		// Skip for generateName-only XRs: they get a synthetic display name like
		// "foo(generated)" that is invalid as a label selector value.
		composite := xrDiff.Current.Raw
		if composite == nil && xr.GetName() != "" && xr.GetGenerateName() == "" {
			composite = xr.GetUnstructured()
		}

		diff, err := c.CalculateDiff(ctx, composite, un)
		if err != nil {
			c.logger.Debug("Error calculating diff for composed resource", "resource", resourceID, "error", err)
			errs = append(errs, errors.Wrapf(err, "cannot calculate diff for %s", resourceID))

			continue
		}

		diffKey := diff.GetDiffKey()
		if diff.DiffType != dt.DiffTypeEqual {
			diffs[diffKey] = diff
		}

		renderedResources[diffKey] = true
		c.logger.Debug("Added resource to renderedResources",
			"xr", xrName,
			"diffKey", diffKey,
			"diffType", diff.DiffType)
	}

	// Log a summary
	c.logger.Debug("Diff calculation complete",
		"totalDiffs", len(diffs),
		"renderedResourcesCount", len(renderedResources),
		"errors", len(errs),
		"xr", xrName)

	if len(errs) > 0 {
		return diffs, renderedResources, errors.Join(errs...)
	}

	return diffs, renderedResources, nil
}

// CalculateDiffs computes all diffs including removals for the rendered resources.
// This is the primary method that most code should use.
func (c *DefaultDiffCalculator) CalculateDiffs(ctx context.Context, xr *cmp.Unstructured, desired render.CompositionOutputs) (map[string]*dt.ResourceDiff, error) {
	// First calculate diffs for modified/added resources
	// parentComposite is nil because CalculateDiffs is only called for root XRs
	diffs, renderedResources, err := c.CalculateNonRemovalDiffs(ctx, xr, nil, desired)
	if err != nil {
		return nil, err
	}

	// Then detect removed resources
	removedDiffs, err := c.CalculateRemovedResourceDiffs(ctx, xr.GetUnstructured(), renderedResources)
	if err != nil {
		return nil, err
	}

	// Merge removed diffs into the main diffs map
	maps.Copy(diffs, removedDiffs)

	return diffs, nil
}

// CalculateRemovedResourceDiffs identifies resources that would be removed and calculates their diffs.
func (c *DefaultDiffCalculator) CalculateRemovedResourceDiffs(ctx context.Context, xr *un.Unstructured, renderedResources map[string]bool) (map[string]*dt.ResourceDiff, error) {
	xrName := xr.GetName()
	c.logger.Debug("Checking for resources to be removed",
		"xr", xrName,
		"renderedResourceCount", len(renderedResources))

	removedDiffs := make(map[string]*dt.ResourceDiff)

	// Try to get the resource tree
	resourceTree, err := c.treeClient.GetResourceTree(ctx, xr)
	if err != nil {
		c.logger.Debug("Cannot get resource tree; aborting", "error", err)
		return nil, errors.Wrap(err, "cannot get resource tree")
	}

	// Create a handler function to recursively traverse the tree and find composed resources
	var findRemovedResources func(node *resource.Resource)

	findRemovedResources = func(node *resource.Resource) {
		// Skip the root (XR) node
		if _, hasAnno := node.Unstructured.GetAnnotations()["crossplane.io/composition-resource-name"]; hasAnno {
			apiVersion := node.Unstructured.GetAPIVersion()
			kind := node.Unstructured.GetKind()
			name := node.Unstructured.GetName()
			resourceID := fmt.Sprintf("%s/%s", kind, name)

			// Use the same key format as in CalculateDiffs to check if this resource was rendered
			key := dt.MakeDiffKey(apiVersion, kind, node.Unstructured.GetNamespace(), name)

			if !renderedResources[key] {
				// This resource exists but wasn't rendered - it will be removed
				c.logger.Debug("Resource will be removed", "resource", resourceID)

				diff, err := renderer.GenerateDiffWithOptions(ctx, &node.Unstructured, nil, c.logger, c.diffOptions)
				if err != nil {
					c.logger.Debug("Cannot calculate removal diff (continuing)",
						"resource", resourceID,
						"error", err)

					return
				}

				if diff != nil {
					diffKey := diff.GetDiffKey()
					removedDiffs[diffKey] = diff
				}
			}
		}

		// Continue recursively traversing children
		for _, child := range node.Children {
			findRemovedResources(child)
		}
	}

	// Start the traversal from the root's children to skip the XR itself
	for _, child := range resourceTree.Children {
		findRemovedResources(child)
	}

	c.logger.Debug("Found resources to be removed", "count", len(removedDiffs))

	return removedDiffs, nil
}

// Kubernetes verbs we ask the authorizer about when a dry run comes back Forbidden.
const (
	verbCreate = "create"
	verbPatch  = "patch"
)

// dryRunCreateAddition computes the post-apply form of a resource that does not yet exist, by
// dry-run creating it.
//
// On success it returns the apiserver's view of the object, which is the whole point: server-side
// defaulting and mutating admission are invisible to the render pipeline, and for a built-in type
// they are invisible to the schema validator's applyCRDDefaults too, since that skips anything
// without a CRD.
//
// When the dry run could not be performed it returns the rendered object unchanged plus a
// DryRunInfo saying why, rather than failing: a missing permission or an unreachable webhook is a
// property of the environment, not a finding about the resource, and an addition has a reasonable
// lower-fidelity fallback. It returns an error only when the cluster actually refused the resource
// (a real finding, routed to the schema-validation exit-code tier) or when something is wrong that
// we must not paper over.
func (c *DefaultDiffCalculator) dryRunCreateAddition(ctx context.Context, desired *un.Unstructured, resourceID string) (*un.Unstructured, *dt.DryRunInfo, error) {
	if c.dryRunOn == DryRunOnExisting {
		return desired, &dt.DryRunInfo{SkipReason: dt.DryRunSkipDisabled}, nil
	}

	createDesired := sanitizeForDryRun(desired)

	c.logger.Debug("Performing dry-run create", "resource", resourceID, "desired", createDesired)

	created, err := c.applyClient.DryRunCreate(ctx, createDesired)

	switch {
	case err == nil:
		c.logger.Debug("Dry-run create succeeded", "resource", resourceID, "result", created)
		return mergeDryRunCreateResult(desired, createDesired, created), nil, nil

	case errors.Is(err, k8.ErrUnresolvableGVK):
		// MUST precede every apierrors check below. The request never reached the apiserver: the GVK
		// could not be resolved to a resource. A discovery 404 is apierrors-NotFound, so without this
		// branch an unknown type would be reported as a missing namespace — the user would go looking
		// at the wrong thing entirely. Nor is it a degradation: we cannot diff a type the cluster does
		// not serve, and pretending otherwise would present rendered output for a resource that cannot
		// exist.
		return nil, nil, errors.Wrapf(err, "cannot dry-run create %s", resourceID)

	case apierrors.IsForbidden(err):
		// A 403 is ambiguous. It can mean we lack the create verb, in which case nothing was learned
		// about the resource and we must degrade quietly. It can equally come from ResourceQuota or
		// from a validating webhook, which are real findings the user needs. Only the authorizer can
		// tell the two apart, and deciding by pattern-matching the apiserver's message would rest that
		// distinction on unversioned prose.
		return c.resolveForbiddenCreate(ctx, desired, resourceID, err)

	case apierrors.IsNotFound(err):
		// The resource's target namespace does not exist yet. GVKToGVR has already resolved the
		// resource itself, so in practice this is NamespaceLifecycle admission and nothing else.
		//
		// This is a degradation, NOT a finding, and the distinction is the whole point: a quota or
		// webhook refusal describes the object and will still hold when the user applies, whereas a
		// missing namespace is a precondition they are very often about to satisfy in the SAME apply —
		// a Namespace and the resources inside it in one `kubectl apply -f ./manifests/` is routine.
		// crossplane-diff cannot know whether the namespace is part of that apply, so refusing to diff
		// would break a supported workflow to report something that may not be true by the time it
		// matters. TestDiffConcurrentDirectory diffs 21 XRs into a namespace that is never created and
		// is exactly this case.
		c.warnOnce(desired, dt.DryRunSkipNamespaceNotFound,
			"skipped apiserver verification of added resources: their namespace does not exist yet, so their diffs omit server-side defaulting and admission",
			"gvk", desired.GroupVersionKind().String(), "namespace", desired.GetNamespace(), "cause", err.Error())

		return desired, &dt.DryRunInfo{SkipReason: dt.DryRunSkipNamespaceNotFound, Detail: err.Error()}, nil

	case apierrors.IsInvalid(err):
		return nil, nil, NewAdmissionRejectionError(resourceID, desired, err)

	case apierrors.IsAlreadyExists(err):
		// We only get here because FetchCurrentObject reported no existing resource. Either its
		// matching logic is wrong or something created the resource underneath us; either way the
		// diff would be built on a false premise, so fail loudly instead of degrading.
		return nil, nil, errors.Wrapf(err, "dry-run create of %s reports it already exists, but it was not found in the cluster", resourceID)

	case apierrors.IsInternalError(err), apierrors.IsServiceUnavailable(err), apierrors.IsTimeout(err):
		// The apiserver could not complete the admission chain — classically an unreachable webhook
		// with failurePolicy: Fail. We cannot know what it would have done, so we must not present
		// rendered output as though it were verified.
		c.warnOnce(desired, dt.DryRunSkipWebhookUnavailable,
			"skipped apiserver verification of added resources: the cluster could not complete admission",
			"gvk", desired.GroupVersionKind().String(), "namespace", desired.GetNamespace(), "cause", err.Error())

		return desired, &dt.DryRunInfo{SkipReason: dt.DryRunSkipWebhookUnavailable, Detail: err.Error()}, nil

	default:
		return nil, nil, errors.Wrapf(err, "cannot dry-run create %s", resourceID)
	}
}

// resolveForbiddenCreate asks the authorizer whether a Forbidden from a dry-run create was an
// authorization denial (degrade) or an admission/quota refusal (report).
func (c *DefaultDiffCalculator) resolveForbiddenCreate(ctx context.Context, desired *un.Unstructured, resourceID string, createErr error) (*un.Unstructured, *dt.DryRunInfo, error) {
	// Can reports whether we MAY create, so a denial is !allowed. Reading this the wrong way round
	// inverts the whole feature: an authorized user would silently lose fidelity, and an unauthorized
	// one would be told the cluster rejected a resource it never saw.
	allowed, reason, ssarErr := c.accessChecker.Can(ctx, desired.GroupVersionKind(), desired.GetNamespace(), verbCreate)

	switch {
	case ssarErr != nil:
		// We hold a 403 we cannot classify. Reporting it as a cluster rejection would assert a finding
		// we have not established; swallowing it as a permission problem would hide one. Say we do not
		// know.
		return nil, nil, errors.Wrapf(createErr, "cannot dry-run create %s, and cannot determine whether that was an authorization denial (%v)", resourceID, ssarErr)

	case !allowed:
		c.warnOnce(desired, dt.DryRunSkipForbidden,
			"skipped apiserver verification of added resources: not authorized to create them, so their diffs omit server-side defaulting and admission",
			"gvk", desired.GroupVersionKind().String(), "namespace", desired.GetNamespace(), "reason", reason)

		return desired, &dt.DryRunInfo{SkipReason: dt.DryRunSkipForbidden, Detail: reason}, nil

	default:
		// Authorized to create, yet refused: admission or quota. A real finding.
		return nil, nil, NewAdmissionRejectionError(resourceID, desired, createErr)
	}
}

// classifyApplyFailure turns a failed dry-run apply of an EXISTING resource into the right kind of
// error. A cluster rejection is reported as such — the same fact, in the same exit-code tier, as a
// rejection of an addition, rather than depending on whether the resource happened to exist already
// (crossplane-diff#334).
//
// Unlike an addition this path never degrades. An existing resource's diff depends on the
// apiserver's merge result for SSA field-removal detection, so one computed without it would be
// wrong rather than merely less detailed — and crossplane-diff has always required the patch verb
// (see the README's RBAC section).
func (c *DefaultDiffCalculator) classifyApplyFailure(ctx context.Context, desired *un.Unstructured, resourceID string, applyErr error) error {
	switch {
	case errors.Is(applyErr, k8.ErrUnresolvableGVK):
		// MUST precede the apierrors checks, for the reason given in dryRunCreateAddition: a
		// discovery-layer failure is shaped like an admission-layer one, and reporting "the cluster
		// rejected this" for a type the cluster does not serve would be actively misleading.
		return errors.Wrapf(applyErr, "cannot dry-run apply %s", resourceID)

	case apierrors.IsForbidden(applyErr):
		// As in resolveForbiddenCreate: Can reports whether we MAY patch, so a denial is !allowed.
		allowed, reason, ssarErr := c.accessChecker.Can(ctx, desired.GroupVersionKind(), desired.GetNamespace(), verbPatch)

		switch {
		case ssarErr != nil:
			return errors.Wrapf(applyErr, "cannot dry-run apply desired object %s, and cannot determine whether that was an authorization denial (%v)", resourceID, ssarErr)
		case !allowed:
			return errors.Wrapf(applyErr, "not authorized to dry-run apply %s (%s); crossplane-diff requires the 'patch' verb on every GVK it diffs", resourceID, reason)
		default:
			return NewAdmissionRejectionError(resourceID, desired, applyErr)
		}

	case apierrors.IsInvalid(applyErr):
		return NewAdmissionRejectionError(resourceID, desired, applyErr)

	default:
		return errors.Wrap(applyErr, "cannot dry-run apply desired object")
	}
}

// mergeDryRunCreateResult combines the apiserver's view of a dry-run created object with the two
// things that must come from our side instead.
//
// BOTH restorations are currently LATENT — correct when reached, but not reachable on any path today.
// They are kept as guards because each protects against a genuinely wrong output if the surrounding
// code changes, and neither costs anything. Do not read either as live behaviour.
//
// Identity, when we sent a generateName and no name: the apiserver runs names.Generator in
// rest.BeforeCreate, ahead of the dry-run short-circuit at the storage layer, so it invents a random
// name, which would make an addition's diff differ on every run. For a *named* resource we keep the
// server's name, so a mutating webhook that rewrites a name still surfaces.
// Latent because nothing reaches here with an empty name: prepareXRForDiff synthesizes a
// deterministic name for generateName XRs (see SynthesizeGeneratedName), and the render binary names
// generateName composed resources itself.
//
// Status, whenever the rendered object had one: composition pipelines are allowed to write their own
// status, so a rendered status is authored content. The apiserver contributes nothing to status on
// create (it is a subresource) and hands back an empty one, so taking the server's would delete the
// user's output under the banner of fidelity.
// Latent because status never reaches any rendered diff in the first place: cleanupForDiff
// (renderer/diff_formatter.go) deletes metadata.status unconditionally from both sides, and Clean is
// the only view renderers read. That makes this the same shape of mistake as the ownerReferences
// comment this change deleted — a restoration justified by an output effect that does not exist — so
// it is labelled rather than presented as load-bearing. Whether composition-authored status SHOULD be
// visible in a diff is a rendering decision, tracked separately.
func mergeDryRunCreateResult(rendered, sent, created *un.Unstructured) *un.Unstructured {
	out := created.DeepCopy()

	if sent.GetGenerateName() != "" && sent.GetName() == "" {
		out.SetName("")
		out.SetGenerateName(sent.GetGenerateName())
	}

	if status, found, err := un.NestedFieldCopy(rendered.Object, "status"); err == nil && found {
		_ = un.SetNestedField(out.Object, status, "status")
	}

	return out
}

// warnOnce raises a user-facing warning about a resource that could not be verified against the
// apiserver, at most once per GVK + namespace + reason.
//
// c.logger is the CLI's *WarningLogger in production (ProcessorConfig.Warnings documents that it is
// the same value Logger is set to), so Info both writes a line to stderr and collects the warning
// for structured output. The typed DryRunInfo on the diff is the machine-readable half and is NOT
// interchangeable with this: warnings carry no resource anchor, so they cannot tell a pipeline which
// additions were degraded.
func (c *DefaultDiffCalculator) warnOnce(obj *un.Unstructured, reason dt.DryRunSkipReason, msg string, keysAndValues ...any) {
	key := fmt.Sprintf("%s|%s|%s", obj.GroupVersionKind(), obj.GetNamespace(), reason)

	c.warnedMu.Lock()

	// Tolerate a struct-literal construction (tests) that skipped NewDiffCalculator; writing to a nil
	// map would panic.
	if c.warned == nil {
		c.warned = make(map[string]bool)
	}

	if c.warned[key] {
		c.warnedMu.Unlock()
		return
	}

	c.warned[key] = true
	c.warnedMu.Unlock()

	c.logger.Info(msg, keysAndValues...)
}

// sanitizeForDryRun returns a deep copy of obj with the server-owned metadata removed, ready to send
// to the apiserver for a dry run. The copy matters: the caller's desired object is also what
// downstream diff comparison reads, so it must not be mutated.
//
// Every field stripped is either assigned by the server or rejected outright on input, so sending it
// is never useful and is sometimes fatal.
//
// ownerReferences is in this list, and a note on why, because the code it replaces claimed the
// opposite. The previous comment here justified stripping ownerRefs on the update path only, on the
// grounds that "for new resources we skip DryRunApply entirely so the rendered ownerRefs still
// surface in the diff output". They never surface: cleanupForDiff (renderer/diff_formatter.go)
// removes metadata.ownerReferences unconditionally from both sides of every comparison, ungated by
// forVerdict. So dropping them costs nothing in output, while keeping them risks two real failures —
// an empty owner UID, which the apiserver rejects outright, and
// OwnerReferencesPermissionEnforcement (enabled by default on OpenShift), which demands delete
// permission on the owner. The SSA multi-controller hazard the original comment described is also
// still avoided, since leaving ownerRefs out of an apply preserves whatever the cluster already has.
//
// managedFields matters for a different reason: client-go's dynamic Apply refuses any object that
// has it populated ("cannot apply an object with managed fields already set"). diff_processor.go
// guards the root XR against this, but the nested-XR branch below feeds render output straight
// through with no such guard — the unexplained failure mode in crossplane-diff#452. Stripping here
// closes it for every path at the one point where objects leave for the apiserver.
//
// resourceVersion is the same shape of problem: a stale value from a user's input file (very
// plausible for anything exported with `kubectl get -o yaml`) makes the apiserver reject the request
// on optimistic concurrency, and it is illegal on a create. diff_processor.go already clears it for
// the root XR; composed resources had no equivalent.
func sanitizeForDryRun(obj *un.Unstructured) *un.Unstructured {
	out := obj.DeepCopy()

	for _, field := range []string{
		"resourceVersion",
		"uid",
		"creationTimestamp",
		"generation",
		"selfLink",
		"managedFields",
		"ownerReferences",
	} {
		un.RemoveNestedField(out.Object, "metadata", field)
	}

	return out
}

// preserveExistingResourceIdentity preserves the identity (name, generateName, labels) from an existing
// resource to ensure dry-run apply works on the correct resource identity.
// This is critical for:
// - Claim scenarios where the rendered name differs from the generated name
// - Nested XRs where the composition adds environment-specific prefixes to names.
func (c *DefaultDiffCalculator) preserveExistingResourceIdentity(current, desired *un.Unstructured, resourceID, renderedName string) *un.Unstructured {
	// Only preserve identity for existing resources with a name
	// Note: We don't require generateName because nested XRs and other resources may have
	// fixed names from the composition that differ from the cluster name due to prefixes.
	if current == nil || current.GetName() == "" {
		return desired
	}

	currentName := current.GetName()
	currentGenerateName := current.GetGenerateName()

	c.logger.Debug("Using existing resource identity for dry-run apply",
		"resource", resourceID,
		"renderedName", renderedName,
		"currentName", currentName,
		"currentGenerateName", currentGenerateName)

	// Create a copy of the desired object and set the identity from the current resource
	// This ensures dry-run apply works on the correct resource identity
	desiredCopy := desired.DeepCopy()
	desiredCopy.SetName(currentName)
	desiredCopy.SetGenerateName(currentGenerateName)

	// Preserve important labels from the existing resource, particularly the composite label
	// This ensures the dry-run apply gets the right labels and preserves them correctly
	CopyLabels(current, desiredCopy, LabelComposite)

	if compositeLabel := current.GetLabels()[LabelComposite]; compositeLabel != "" {
		c.logger.Debug("Preserved composite label for dry-run apply",
			"resource", resourceID,
			"compositeLabel", compositeLabel)
	}

	return desiredCopy
}

// preserveCompositeLabel preserves the crossplane.io/composite label from an existing composed resource.
// This is critical because in Crossplane, all composed resources (including nested XRs) point to the ROOT composite.
// We must never change this label for existing composed resources.
// Root XRs don't have the composition-resource-name annotation and shouldn't have this label.
func (c *DefaultDiffCalculator) preserveCompositeLabel(current, desired *un.Unstructured, resourceID string) *un.Unstructured {
	// Only preserve for composed resources (those with composition-resource-name annotation)
	if current == nil || desired.GetAnnotations()["crossplane.io/composition-resource-name"] == "" {
		return desired
	}

	compositeLabel := current.GetLabels()[LabelComposite]
	if compositeLabel == "" {
		return desired
	}

	// Preserve the composite label
	desiredCopy := desired.DeepCopy()
	CopyLabels(current, desiredCopy, LabelComposite)

	c.logger.Debug("Preserved composite label from existing composed resource",
		"resource", resourceID,
		"compositeLabel", compositeLabel)

	return desiredCopy
}
