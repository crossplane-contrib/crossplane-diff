package diffprocessor

import (
	"context"
	"strings"
	"testing"

	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	gcmp "github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	cpd "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	cmp "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"
)

// Ensure MockDiffCalculator implements the DiffCalculator interface.
var _ DiffCalculator = &tu.MockDiffCalculator{}

func TestDefaultDiffCalculator_CalculateDiff(t *testing.T) {
	ctx := t.Context()

	// Create test resources
	existingResource := tu.NewResource("example.org/v1", "TestResource", "existing-resource").
		WithSpecField("field", "old-value").
		Build()

	modifiedResource := tu.NewResource("example.org/v1", "TestResource", "existing-resource").
		WithSpecField("field", "new-value").
		Build()

	newResource := tu.NewResource("example.org/v1", "TestResource", "new-resource").
		WithSpecField("field", "value").
		Build()

	const ParentXRName = "parent-xr"

	composedResource := tu.NewResource("example.org/v1", "ComposedResource", "cpd-resource").
		WithSpecField("field", "old-value").
		WithLabels(map[string]string{
			"crossplane.io/composite": ParentXRName,
		}).
		WithAnnotations(map[string]string{
			"crossplane.io/composition-resource-name": "resource-a",
		}).
		Build()

	// Parent XR
	parentXR := tu.NewResource("example.org/v1", "XR", ParentXRName).
		WithSpecField("field", "value").
		Build()

	tests := map[string]struct {
		setupMocks func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager)
		composite  *un.Unstructured
		desired    *un.Unstructured
		wantDiff   *dt.ResourceDiff
		wantNil    bool
		wantErr    bool

		// accessChecker is the authorizer consulted when a dry run comes back Forbidden. Nil means
		// "allows everything", which is what routes a Forbidden to the cluster-rejection path.
		accessChecker k8.AccessChecker

		// dryRunOn mirrors the CLI flag. The zero value means DryRunOnAll, as in ProcessorConfig, so
		// unset cases exercise the default behaviour rather than a special test-only mode.
		dryRunOn DryRunOn

		// wantDryRun is the expected DryRunInfo on the resulting diff. Nil asserts its ABSENCE, i.e.
		// that the desired state really did go through the apiserver.
		wantDryRun *dt.DryRunInfo

		// wantSchemaValidationErr asserts the error lands in the schema-validation exit-code tier,
		// which is how a cluster rejection is distinguished from a tool failure.
		wantSchemaValidationErr bool

		// wantErrContains, when set, requires the error message to contain this substring. Several
		// failure modes are all "an error" but must not be confused with each other.
		wantErrContains string

		// wantServerField, when set, requires spec.defaultedField in the DESIRED side of the diff to
		// hold this value. That field is never present in the rendered input, so it can only have come
		// from the apiserver's response — which is the whole claim this feature makes.
		wantServerField string
	}{
		"ExistingResourceModified": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create mock apply client
				applyClient := tu.NewMockApplyClient().
					WithSuccessfulDryRun().
					Build()

				// Create mock resource tree client (not used in this test)
				resourceTreeClient := tu.NewMockResourceTreeClient().Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					WithResourcesExist(existingResource).
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			composite: nil,
			desired:   modifiedResource,
			wantDiff: &dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Kind: "TestResource", Group: "example.org", Version: "v1"},
				ResourceName: "existing-resource",
				DiffType:     dt.DiffTypeModified,
			},
		},
		"NewResource": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// An addition is now dry-run CREATED, not left as rendered, so this mock must stub the
				// create path as well.
				applyClient := tu.NewMockApplyClient().
					WithSuccessfulDryRun().
					WithSuccessfulDryRunCreate().
					Build()

				// Create mock resource tree client (not used in this test)
				resourceTreeClient := tu.NewMockResourceTreeClient().Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					WithResourceNotFound().
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			composite: nil,
			desired:   newResource,
			wantDiff: &dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Kind: "TestResource", Group: "example.org", Version: "v1"},
				ResourceName: "new-resource",
				DiffType:     dt.DiffTypeAdded,
			},
		},
		"ComposedResource": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create mock apply client
				applyClient := tu.NewMockApplyClient().
					WithSuccessfulDryRun().
					Build()

				// Create mock resource tree client (not used in this test)
				resourceTreeClient := tu.NewMockResourceTreeClient().Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					WithResourcesExist(composedResource).
					WithResourcesFoundByLabel([]*un.Unstructured{composedResource}, "crossplane.io/composite", ParentXRName).
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			composite: parentXR,
			desired: tu.NewResource("example.org/v1", "ComposedResource", "cpd-resource").
				WithSpecField("field", "new-value").
				WithLabels(map[string]string{
					"crossplane.io/composite": ParentXRName,
				}).
				WithAnnotations(map[string]string{
					"crossplane.io/composition-resource-name": "resource-a",
				}).
				Build(),
			wantDiff: &dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Kind: "ComposedResource", Group: "example.org", Version: "v1"},
				ResourceName: "cpd-resource",
				DiffType:     dt.DiffTypeModified,
			},
		},
		"NoChanges": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create mock apply client
				applyClient := tu.NewMockApplyClient().
					WithSuccessfulDryRun().
					Build()

				// Create mock resource tree client (not used in this test)
				resourceTreeClient := tu.NewMockResourceTreeClient().Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					WithResourcesExist(existingResource).
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			composite: nil,
			desired:   existingResource.DeepCopy(),
			wantDiff: &dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Kind: "TestResource", Group: "example.org", Version: "v1"},
				ResourceName: "existing-resource",
				DiffType:     dt.DiffTypeEqual,
			},
		},
		// --- dry-run-create of additions (crossplane-diff#334) -------------------------------------
		//
		// These cases exercise every outcome of the addition path. The shared shape is a resource that
		// is NOT in the cluster, so CalculateDiff takes the create branch; what varies is what the
		// apiserver says and what the authorizer says about a Forbidden.

		"AdditionPicksUpServerSideChanges": {
			// The point of the whole feature: the apiserver's view of the object is what gets diffed, so
			// fields it defaulted or a mutating webhook injected appear in the addition diff. Asserted by
			// having the fake return a field the rendered object never had.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				applyClient := tu.NewMockApplyClient().
					WithDryRunCreate(func(_ context.Context, obj *un.Unstructured) (*un.Unstructured, error) {
						served := obj.DeepCopy()
						_ = un.SetNestedField(served.Object, "server-assigned", "spec", "defaultedField")

						return served, nil
					}).
					Build()

				resourceClient := tu.NewMockResourceClient().WithResourceNotFound().Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, tu.NewMockResourceTreeClient().Build(), resourceManager
			},
			composite: nil,
			desired:   newResource,
			wantDiff: &dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Kind: "TestResource", Group: "example.org", Version: "v1"},
				ResourceName: "new-resource",
				DiffType:     dt.DiffTypeAdded,
			},
			wantServerField: "server-assigned",
		},
		"AdditionNotDryRunWhenDisabled": {
			// --dry-run-on=existing reproduces the pre-#334 behaviour, and says so per-resource rather
			// than presenting rendered output as verified. DryRunCreate is deliberately left unstubbed:
			// if the flag were ignored the mock's "not implemented" error would fail the case.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				applyClient := tu.NewMockApplyClient().Build()
				resourceClient := tu.NewMockResourceClient().WithResourceNotFound().Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, tu.NewMockResourceTreeClient().Build(), resourceManager
			},
			composite: nil,
			desired:   newResource,
			dryRunOn:  DryRunOnExisting,
			wantDiff: &dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Kind: "TestResource", Group: "example.org", Version: "v1"},
				ResourceName: "new-resource",
				DiffType:     dt.DiffTypeAdded,
			},
			wantDryRun: &dt.DryRunInfo{SkipReason: dt.DryRunSkipDisabled},
		},
		"AdditionForbiddenByRBACDegrades": {
			// Forbidden + authorizer says we may NOT create: a property of our credentials, not a finding
			// about the resource. Fall back to rendered output and say so; do not fail the run.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				applyClient := tu.NewMockApplyClient().
					WithDryRunCreate(func(context.Context, *un.Unstructured) (*un.Unstructured, error) {
						return nil, apierrors.NewForbidden(schema.GroupResource{Group: "example.org", Resource: "testresources"}, "new-resource", errors.New("nope"))
					}).
					Build()

				resourceClient := tu.NewMockResourceClient().WithResourceNotFound().Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, tu.NewMockResourceTreeClient().Build(), resourceManager
			},
			composite:     nil,
			desired:       newResource,
			accessChecker: tu.NewMockAccessChecker().WithDenied("no create on testresources").Build(),
			wantDiff: &dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Kind: "TestResource", Group: "example.org", Version: "v1"},
				ResourceName: "new-resource",
				DiffType:     dt.DiffTypeAdded,
			},
			wantDryRun: &dt.DryRunInfo{SkipReason: dt.DryRunSkipForbidden, Detail: "no create on testresources"},
		},
		"AdditionForbiddenWhileAuthorizedIsARejection": {
			// Forbidden + authorizer says we MAY create: the refusal came from admission, quota, or a
			// missing namespace. A real finding, and the case that would be silently swallowed if every
			// 403 were treated as an RBAC problem.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				applyClient := tu.NewMockApplyClient().
					WithDryRunCreate(func(context.Context, *un.Unstructured) (*un.Unstructured, error) {
						return nil, apierrors.NewForbidden(schema.GroupResource{Group: "example.org", Resource: "testresources"}, "new-resource", errors.New("exceeded quota: storage"))
					}).
					Build()

				resourceClient := tu.NewMockResourceClient().WithResourceNotFound().Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, tu.NewMockResourceTreeClient().Build(), resourceManager
			},
			composite:               nil,
			desired:                 newResource,
			accessChecker:           tu.NewMockAccessChecker().WithAllowed().Build(),
			wantErr:                 true,
			wantSchemaValidationErr: true,
			wantErrContains:         "exceeded quota",
		},
		"AdditionInvalidIsARejection": {
			// A 422 needs no authorizer round-trip: it is unambiguously about the content.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				applyClient := tu.NewMockApplyClient().
					WithDryRunCreate(func(context.Context, *un.Unstructured) (*un.Unstructured, error) {
						return nil, apierrors.NewInvalid(
							schema.GroupKind{Group: "example.org", Kind: "TestResource"},
							"new-resource",
							field.ErrorList{field.Invalid(field.NewPath("spec", "field"), "value", "must be uppercase")},
						)
					}).
					Build()

				resourceClient := tu.NewMockResourceClient().WithResourceNotFound().Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, tu.NewMockResourceTreeClient().Build(), resourceManager
			},
			composite:               nil,
			desired:                 newResource,
			wantErr:                 true,
			wantSchemaValidationErr: true,
			wantErrContains:         "must be uppercase",
		},
		"AdditionWebhookUnavailableDegrades": {
			// The apiserver could not complete admission, so we cannot know what it would have done. Fall
			// back, but disclaim it — presenting rendered output as verified here would be the dishonest
			// option.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				applyClient := tu.NewMockApplyClient().
					WithDryRunCreate(func(context.Context, *un.Unstructured) (*un.Unstructured, error) {
						return nil, apierrors.NewInternalError(errors.New(`failed calling webhook "policy.example.org": connection refused`))
					}).
					Build()

				resourceClient := tu.NewMockResourceClient().WithResourceNotFound().Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, tu.NewMockResourceTreeClient().Build(), resourceManager
			},
			composite: nil,
			desired:   newResource,
			wantDiff: &dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Kind: "TestResource", Group: "example.org", Version: "v1"},
				ResourceName: "new-resource",
				DiffType:     dt.DiffTypeAdded,
			},
			wantDryRun: &dt.DryRunInfo{
				SkipReason: dt.DryRunSkipWebhookUnavailable,
				Detail:     `Internal error occurred: failed calling webhook "policy.example.org": connection refused`,
			},
		},
		"AdditionInMissingNamespaceDegrades": {
			// Regression test for the e2e failure this classification originally caused
			// (TestDiffConcurrentDirectory diffs 21 XRs into a namespace that is never created).
			//
			// A NotFound from a dry-run create is NamespaceLifecycle admission: the target namespace does
			// not exist yet. That must degrade, not fail. A namespace and the resources inside it are
			// routinely applied together, so the namespace's absence at diff time says nothing about
			// whether the apply will succeed — which is exactly what separates it from a quota or webhook
			// refusal, where the verdict describes the object and will still hold.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				applyClient := tu.NewMockApplyClient().
					WithDryRunCreate(func(context.Context, *un.Unstructured) (*un.Unstructured, error) {
						return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "does-not-exist")
					}).
					Build()

				resourceClient := tu.NewMockResourceClient().WithResourceNotFound().Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, tu.NewMockResourceTreeClient().Build(), resourceManager
			},
			composite: nil,
			desired:   newResource,
			wantDiff: &dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Kind: "TestResource", Group: "example.org", Version: "v1"},
				ResourceName: "new-resource",
				DiffType:     dt.DiffTypeAdded,
			},
			wantDryRun: &dt.DryRunInfo{
				SkipReason: dt.DryRunSkipNamespaceNotFound,
				Detail:     `namespaces "does-not-exist" not found`,
			},
		},
		"AdditionAlreadyExistsFailsLoudly": {
			// FetchCurrentObject said this resource does not exist and the apiserver says it does. The
			// diff would rest on a false premise, so this must not degrade — and must not be reported as
			// a cluster rejection of the user's content either.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				applyClient := tu.NewMockApplyClient().
					WithDryRunCreate(func(context.Context, *un.Unstructured) (*un.Unstructured, error) {
						return nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "example.org", Resource: "testresources"}, "new-resource")
					}).
					Build()

				resourceClient := tu.NewMockResourceClient().WithResourceNotFound().Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, tu.NewMockResourceTreeClient().Build(), resourceManager
			},
			composite:               nil,
			desired:                 newResource,
			wantErr:                 true,
			wantSchemaValidationErr: false,
			wantErrContains:         "already exists, but it was not found in the cluster",
		},
		"AdditionForbiddenWithUnusableAuthorizerFailsLoudly": {
			// We hold a 403 we cannot classify. Claiming a cluster rejection would assert an unestablished
			// finding; claiming RBAC would hide a real one. Saying "we don't know" is the only honest
			// option, and it must not be the schema-validation tier.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				applyClient := tu.NewMockApplyClient().
					WithDryRunCreate(func(context.Context, *un.Unstructured) (*un.Unstructured, error) {
						return nil, apierrors.NewForbidden(schema.GroupResource{Group: "example.org", Resource: "testresources"}, "new-resource", errors.New("denied"))
					}).
					Build()

				resourceClient := tu.NewMockResourceClient().WithResourceNotFound().Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, tu.NewMockResourceTreeClient().Build(), resourceManager
			},
			composite:               nil,
			desired:                 newResource,
			accessChecker:           tu.NewMockAccessChecker().WithFailedCheck(errors.New("ssar exploded")).Build(),
			wantErr:                 true,
			wantSchemaValidationErr: false,
			wantErrContains:         "cannot determine whether that was an authorization denial",
		},
		"ExistingResourceRejectionIsAValidationError": {
			// The #334 reclassification: a cluster rejection of a MODIFICATION is the same fact as one of
			// an addition, so it lands in the same exit-code tier rather than depending on whether the
			// resource happened to exist already.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				applyClient := tu.NewMockApplyClient().
					WithDryRunApply(func(context.Context, *un.Unstructured, string) (*un.Unstructured, error) {
						return nil, apierrors.NewForbidden(schema.GroupResource{Group: "example.org", Resource: "testresources"}, "existing-resource",
							errors.New(`admission webhook "policy.example.org" denied the request`))
					}).
					Build()

				resourceClient := tu.NewMockResourceClient().WithResourcesExist(existingResource).Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, tu.NewMockResourceTreeClient().Build(), resourceManager
			},
			composite:               nil,
			desired:                 modifiedResource,
			accessChecker:           tu.NewMockAccessChecker().WithAllowed().Build(),
			wantErr:                 true,
			wantSchemaValidationErr: true,
			wantErrContains:         "admission webhook",
		},
		"ExistingResourceMissingPatchIsAToolError": {
			// The mirror of AdditionForbiddenByRBACDegrades, and deliberately NOT symmetric with it. An
			// existing resource's diff depends on the apiserver's merge result for SSA field-removal
			// detection, so falling back to rendered output would be wrong rather than merely less
			// detailed. It must also not be reported as a rejection of the user's content.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				applyClient := tu.NewMockApplyClient().
					WithDryRunApply(func(context.Context, *un.Unstructured, string) (*un.Unstructured, error) {
						return nil, apierrors.NewForbidden(schema.GroupResource{Group: "example.org", Resource: "testresources"}, "existing-resource", errors.New("nope"))
					}).
					Build()

				resourceClient := tu.NewMockResourceClient().WithResourcesExist(existingResource).Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, tu.NewMockResourceTreeClient().Build(), resourceManager
			},
			composite:               nil,
			desired:                 modifiedResource,
			accessChecker:           tu.NewMockAccessChecker().WithDeniedVerb("patch", "no patch on testresources").Build(),
			wantErr:                 true,
			wantSchemaValidationErr: false,
			wantErrContains:         "requires the 'patch' verb",
		},
		"ErrorGettingCurrentObject": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create mock apply client (not used because test fails earlier)
				applyClient := tu.NewMockApplyClient().Build()

				// Create mock resource tree client (not used in this test)
				resourceTreeClient := tu.NewMockResourceTreeClient().Build()

				// Create mock resource client for resource manager that returns an error
				resourceClient := tu.NewMockResourceClient().
					WithGetResource(func(context.Context, schema.GroupVersionKind, string, string) (*un.Unstructured, error) {
						return nil, errors.New("resource not found")
					}).
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			composite: nil,
			desired:   existingResource,
			wantErr:   true,
		},
		"DryRunError": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create mock apply client that returns an error
				applyClient := tu.NewMockApplyClient().
					WithFailedDryRun("apply error").
					Build()

				// Create mock resource tree client (not used in this test)
				resourceTreeClient := tu.NewMockResourceTreeClient().Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					WithResourcesExist(existingResource).
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			composite: nil,
			desired:   modifiedResource,
			wantErr:   true,
		},
		"FieldOwnerExtractedFromManagedFields": {
			// This test verifies that the Crossplane composed field owner is extracted from
			// the existing resource's managedFields and passed to the dry-run apply.
			// This is critical for correct field removal detection with Server-Side Apply.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				const expectedFieldOwner = "apiextensions.crossplane.io/composed/abc123def456"

				// Create existing resource WITH managed fields containing Crossplane composed prefix
				existingWithManagedFields := tu.NewResource("example.org/v1", "TestResource", "existing-resource").
					WithSpecField("field", "old-value").
					WithFieldManagers("kubectl-client-side-apply", expectedFieldOwner, "other-controller").
					Build()

				// Create mock apply client that captures and verifies the field owner
				applyClient := tu.NewMockApplyClient().
					WithDryRunApply(func(_ context.Context, obj *un.Unstructured, fieldOwner string) (*un.Unstructured, error) {
						// Verify the field owner was correctly extracted
						if fieldOwner != expectedFieldOwner {
							t.Errorf("DryRunApply called with wrong field owner: got %q, want %q", fieldOwner, expectedFieldOwner)
						}

						return obj, nil
					}).
					Build()

				// Create mock resource tree client (not used in this test)
				resourceTreeClient := tu.NewMockResourceTreeClient().Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					WithResourcesExist(existingWithManagedFields).
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			composite: nil,
			desired: tu.NewResource("example.org/v1", "TestResource", "existing-resource").
				WithSpecField("field", "new-value").
				Build(),
			wantDiff: &dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Kind: "TestResource", Group: "example.org", Version: "v1"},
				ResourceName: "existing-resource",
				DiffType:     dt.DiffTypeModified,
			},
		},
		"FieldOwnerDefaultsWhenNotInManagedFields": {
			// This test verifies that when no Crossplane composed field owner is found
			// in the existing resource's managedFields, we use the default field owner.
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create existing resource WITHOUT Crossplane managed fields
				existingWithoutCrossplaneManagedFields := tu.NewResource("example.org/v1", "TestResource", "existing-resource").
					WithSpecField("field", "old-value").
					WithFieldManagers("kubectl-client-side-apply", "some-other-controller").
					Build()

				// Create mock apply client that captures and verifies the field owner is empty
				// (which means the default will be used)
				applyClient := tu.NewMockApplyClient().
					WithDryRunApply(func(_ context.Context, obj *un.Unstructured, fieldOwner string) (*un.Unstructured, error) {
						// Verify no Crossplane field owner was extracted (defaults to empty)
						if fieldOwner != "" {
							t.Errorf("DryRunApply called with unexpected field owner: got %q, want empty string", fieldOwner)
						}

						return obj, nil
					}).
					Build()

				// Create mock resource tree client (not used in this test)
				resourceTreeClient := tu.NewMockResourceTreeClient().Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					WithResourcesExist(existingWithoutCrossplaneManagedFields).
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			composite: nil,
			desired: tu.NewResource("example.org/v1", "TestResource", "existing-resource").
				WithSpecField("field", "new-value").
				Build(),
			wantDiff: &dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Kind: "TestResource", Group: "example.org", Version: "v1"},
				ResourceName: "existing-resource",
				DiffType:     dt.DiffTypeModified,
			},
		},
		"FindAndDiffResourceWithGenerateName": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// The composed resource with generateName
				composedWithGenName := tu.NewResource("example.org/v1", "ComposedResource", "").
					WithLabels(map[string]string{
						"crossplane.io/composite": ParentXRName,
					}).
					WithAnnotations(map[string]string{
						"crossplane.io/composition-resource-name": "resource-a",
					}).
					Build()

				// Set generateName instead of name
				composedWithGenName.SetGenerateName("test-resource-")

				// The existing resource on the cluster with a generated name
				existingComposed := tu.NewResource("example.org/v1", "ComposedResource", "test-resource-abc123").
					WithLabels(map[string]string{
						"crossplane.io/composite": ParentXRName,
					}).
					WithAnnotations(map[string]string{
						"crossplane.io/composition-resource-name": "resource-a",
					}).
					WithSpecField("field", "old-value").
					Build()

				// Create mock apply client
				applyClient := tu.NewMockApplyClient().
					WithSuccessfulDryRun().
					Build()

				// Create mock resource tree client (not used in this test)
				resourceTreeClient := tu.NewMockResourceTreeClient().Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					// Return "not found" for direct name lookup
					WithGetResource(func(_ context.Context, gvk schema.GroupVersionKind, _, name string) (*un.Unstructured, error) {
						// This should fail as the resource has generateName, not name
						if name == "test-resource-abc123" {
							return existingComposed, nil
						}

						return nil, apierrors.NewNotFound(
							schema.GroupResource{
								Group:    gvk.Group,
								Resource: strings.ToLower(gvk.Kind) + "s",
							},
							name,
						)
					}).
					// Return our existing resource when looking up by label
					WithGetResourcesByLabel(func(_ context.Context, _ schema.GroupVersionKind, _ string, sel metav1.LabelSelector) ([]*un.Unstructured, error) {
						// Verify we're looking up with the right composite owner label
						if owner, exists := sel.MatchLabels["crossplane.io/composite"]; exists && owner == ParentXRName {
							return []*un.Unstructured{existingComposed}, nil
						}

						return []*un.Unstructured{}, nil
					}).
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			composite: parentXR,
			desired: tu.NewResource("example.org/v1", "ComposedResource", "").
				WithLabels(map[string]string{
					"crossplane.io/composite": ParentXRName,
				}).
				WithAnnotations(map[string]string{
					"crossplane.io/composition-resource-name": "resource-a",
				}).
				WithSpecField("field", "new-value").
				WithGenerateName("test-resource-").
				Build(),
			wantDiff: &dt.ResourceDiff{
				Gvk:          schema.GroupVersionKind{Kind: "ComposedResource", Group: "example.org", Version: "v1"},
				ResourceName: "test-resource-abc123", // Should have found the existing resource name
				DiffType:     dt.DiffTypeModified,    // Should be modified, not added
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger := tu.TestLogger(t, false)

			// Setup mocks
			applyClient, resourceTreeClient, resourceManager := tt.setupMocks(t)

			accessChecker := tt.accessChecker
			if accessChecker == nil {
				accessChecker = tu.NewMockAccessChecker().Build()
			}

			// Setup the diff calculator with the mocks
			calculator := NewDiffCalculator(
				applyClient,
				accessChecker,
				resourceTreeClient,
				resourceManager,
				logger,
				renderer.DefaultDiffOptions(),
				tt.dryRunOn,
			)

			// Call the function under test
			diff, err := calculator.CalculateDiff(ctx, tt.composite, tt.desired)

			// Check error condition
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CalculateDiff() expected error but got none")
				}

				if tt.wantErrContains != "" && !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("CalculateDiff() error %q does not contain %q", err, tt.wantErrContains)
				}

				if got := IsSchemaValidationError(err); got != tt.wantSchemaValidationErr {
					t.Errorf("IsSchemaValidationError() = %v, want %v; a cluster rejection must land in the schema-validation exit-code tier and a tool failure must not: %v",
						got, tt.wantSchemaValidationErr, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("CalculateDiff() unexpected error: %v", err)
			}

			// Check nil diff case
			if tt.wantNil {
				if diff != nil {
					t.Errorf("CalculateDiff() expected nil diff but got: %v", diff)
				}

				return
			}

			// Check non-nil case
			if diff == nil {
				t.Fatalf("CalculateDiff() returned nil diff, expected non-nil")
			}

			// Check the basics of the diff
			if diff := gcmp.Diff(tt.wantDiff.Gvk, diff.Gvk); diff != "" {
				t.Errorf("Gvk mismatch (-want +got):\n%s", diff)
			}

			if diff := gcmp.Diff(tt.wantDiff.ResourceName, diff.ResourceName); diff != "" {
				t.Errorf("ResourceName mismatch (-want +got):\n%s", diff)
			}

			if diff := gcmp.Diff(tt.wantDiff.DiffType, diff.DiffType); diff != "" {
				t.Errorf("DiffType mismatch (-want +got):\n%s", diff)
			}

			// For modified resources, check that LineDiffs is populated
			if diff.DiffType == dt.DiffTypeModified && len(diff.LineDiffs) == 0 {
				t.Errorf("LineDiffs is empty for %s", name)
			}

			// Nil wantDryRun asserts absence, which is the positive claim that the desired state was
			// verified against the apiserver — not merely "we didn't check".
			if d := gcmp.Diff(tt.wantDryRun, diff.DryRun); d != "" {
				t.Errorf("DryRun mismatch (-want +got):\n%s", d)
			}

			if tt.wantServerField != "" {
				if diff.Desired.Raw == nil {
					t.Fatalf("expected the apiserver's object on the desired side, got no desired view at all")
				}

				got, found, err := un.NestedString(diff.Desired.Raw.Object, "spec", "defaultedField")
				switch {
				case err != nil:
					t.Errorf("spec.defaultedField: %v", err)
				case !found:
					t.Errorf("spec.defaultedField absent from the desired side: the apiserver's response was discarded, so the addition diff shows only rendered output")
				case got != tt.wantServerField:
					t.Errorf("spec.defaultedField = %q, want %q", got, tt.wantServerField)
				}
			}
		})
	}
}

func TestDefaultDiffCalculator_CalculateDiffs(t *testing.T) {
	ctx := t.Context()

	// Create test XR
	modifiedXr := tu.NewResource("example.org/v1", "XR", "test-xr").
		WithSpecField("field", "new-value").
		BuildUComposite()

	// Create test rendered resources
	renderedXR := tu.NewResource("example.org/v1", "XR", "test-xr").
		BuildUComposite()

	// Create rendered composed resources
	composedResource1 := tu.NewResource("example.org/v1", "Composed", "cpd-1").
		WithCompositeOwner("test-xr").
		WithCompositionResourceName("resource-1").
		WithSpecField("field", "new-value").
		BuildUComposed()

	// Create existing resources for the client to find
	existingXRBuilder := tu.NewResource("example.org/v1", "XR", "test-xr").
		WithSpecField("field", "old-value")
	existingXR := existingXRBuilder.Build()
	existingXrUComp := existingXRBuilder.BuildUComposite()

	existingComposed := tu.NewResource("example.org/v1", "Composed", "cpd-1").
		WithCompositeOwner("test-xr").
		WithCompositionResourceName("resource-1").
		WithSpecField("field", "old-value").
		Build()

	tests := map[string]struct {
		setupMocks    func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager)
		inputXR       *cmp.Unstructured
		renderedOut   render.CompositionOutputs
		expectedDiffs map[string]dt.DiffType // Map of expected keys and their diff types
		wantErr       bool
	}{
		"XRAndComposedResourceModifications": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create mock apply client
				applyClient := tu.NewMockApplyClient().
					WithSuccessfulDryRun().
					Build()

				// Create mock resource tree client
				resourceTreeClient := tu.NewMockResourceTreeClient().
					WithEmptyResourceTree().
					Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					WithResourcesExist(existingXR, existingComposed).
					WithResourcesFoundByLabel([]*un.Unstructured{existingComposed}, "crossplane.io/composite", "test-xr").
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			inputXR: modifiedXr,
			renderedOut: render.CompositionOutputs{
				CompositeResource: renderedXR,
				ComposedResources: []cpd.Unstructured{*composedResource1},
			},
			expectedDiffs: map[string]dt.DiffType{
				"example.org/v1/XR//test-xr":     dt.DiffTypeModified,
				"example.org/v1/Composed//cpd-1": dt.DiffTypeModified,
			},
			wantErr: false,
		},
		"XRNotModifiedComposedResourceModified": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create mock apply client
				applyClient := tu.NewMockApplyClient().
					WithSuccessfulDryRun().
					Build()

				// Create mock resource tree client
				resourceTreeClient := tu.NewMockResourceTreeClient().
					WithEmptyResourceTree().
					Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					WithResourcesExist(existingXR, existingComposed).
					WithResourcesFoundByLabel([]*un.Unstructured{existingComposed}, "crossplane.io/composite", "test-xr").
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			inputXR: existingXrUComp,
			renderedOut: render.CompositionOutputs{
				CompositeResource: func() *cmp.Unstructured {
					// Create XR with same values (no changes)
					sameXR := &cmp.Unstructured{}
					sameXR.SetUnstructuredContent(existingXR.UnstructuredContent())

					return sameXR
				}(),
				ComposedResources: []cpd.Unstructured{*composedResource1},
			},
			// XR diff is always stored (even when DiffTypeEqual) for removal detection
			expectedDiffs: map[string]dt.DiffType{
				"example.org/v1/Composed//cpd-1": dt.DiffTypeModified,
				"example.org/v1/XR//test-xr":     dt.DiffTypeEqual,
			},
			wantErr: false,
		},
		"ErrorCalculatingDiff": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create mock apply client that returns an error
				applyClient := tu.NewMockApplyClient().
					WithFailedDryRun("dry run error").
					Build()

				// Create mock resource tree client
				resourceTreeClient := tu.NewMockResourceTreeClient().
					Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					WithResourcesExist(existingXR, existingComposed).
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			inputXR: existingXrUComp,
			renderedOut: render.CompositionOutputs{
				CompositeResource: renderedXR,
				ComposedResources: []cpd.Unstructured{*composedResource1},
			},
			expectedDiffs: map[string]dt.DiffType{},
			wantErr:       true,
		},
		"ResourceTreeWithPotentialRemoval": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create a resource that isn't in the rendered output
				extraComposedResource := tu.NewResource("example.org/v1", "Composed", "cpd-2").
					WithCompositeOwner("test-xr").
					WithCompositionResourceName("resource-to-be-removed").
					WithSpecField("field", "value").
					Build()

				// Create mock apply client
				applyClient := tu.NewMockApplyClient().
					WithSuccessfulDryRun().
					Build()

				// Create mock resource tree client with the XR as root and some composed resources as children
				resourceTreeClient := tu.NewMockResourceTreeClient().
					WithResourceTreeFromXRAndComposed(existingXR, []*un.Unstructured{
						existingComposed,
						extraComposedResource,
					}).
					Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					WithResourcesExist(existingXR, existingComposed, extraComposedResource).
					WithResourcesFoundByLabel([]*un.Unstructured{existingComposed}, "crossplane.io/composite", "test-xr").
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			inputXR: modifiedXr,
			renderedOut: render.CompositionOutputs{
				CompositeResource: renderedXR,
				ComposedResources: []cpd.Unstructured{*composedResource1},
			},
			expectedDiffs: map[string]dt.DiffType{
				"example.org/v1/XR//test-xr":     dt.DiffTypeModified,
				"example.org/v1/Composed//cpd-1": dt.DiffTypeModified,
				"example.org/v1/Composed//cpd-2": dt.DiffTypeRemoved,
			},
			wantErr: false,
		},
		"ResourceRemovalDetection": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create existing version of the resource
				existingComposedWithOldValue := tu.NewResource("example.org/v1", "Composed", "cpd-1").
					WithCompositeOwner("test-xr").
					WithCompositionResourceName("resource-1").
					WithSpecField("field", "old-value").
					Build()

				// Create an extra resource that should be removed
				extraResource := tu.NewResource("example.org/v1", "Composed", "resource-to-remove").
					WithCompositeOwner("test-xr").
					WithCompositionResourceName("resource-to-remove").
					Build()

				// Create mock apply client
				applyClient := tu.NewMockApplyClient().
					WithSuccessfulDryRun().
					Build()

				// Create mock resource tree client
				resourceTreeClient := tu.NewMockResourceTreeClient().
					WithResourceTreeFromXRAndComposed(
						existingXR,
						[]*un.Unstructured{existingComposedWithOldValue, extraResource},
					).
					Build()

				// Create mock resource client for resource manager
				resourceClient := tu.NewMockResourceClient().
					WithResourcesExist(existingXR, existingComposedWithOldValue, extraResource).
					WithResourcesFoundByLabel(
						[]*un.Unstructured{existingComposedWithOldValue, extraResource},
						"crossplane.io/composite",
						"test-xr",
					).
					Build()

				// Create resource manager
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			inputXR: modifiedXr,
			renderedOut: render.CompositionOutputs{
				CompositeResource: renderedXR,
				// Include a modified version of composedResource1 with new value
				ComposedResources: []cpd.Unstructured{*tu.NewResource("example.org/v1", "Composed", "cpd-1").
					WithCompositeOwner("test-xr").
					WithCompositionResourceName("resource-1").
					WithSpecField("field", "new-value"). // Different value than existing
					BuildUComposed()},
			},
			expectedDiffs: map[string]dt.DiffType{
				"example.org/v1/XR//test-xr":                  dt.DiffTypeModified,
				"example.org/v1/Composed//cpd-1":              dt.DiffTypeModified,
				"example.org/v1/Composed//resource-to-remove": dt.DiffTypeRemoved,
			},
			wantErr: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger := tu.TestLogger(t, false)

			// Setup mocks
			applyClient, resourceTreeClient, resourceManager := tt.setupMocks(t)

			// Create a diff calculator with default options
			calculator := NewDiffCalculator(
				applyClient,
				tu.NewMockAccessChecker().Build(),
				resourceTreeClient,
				resourceManager,
				logger,
				renderer.DefaultDiffOptions(),
				DryRunOnAll,
			)

			// Call the function under test
			// Use CalculateDiffs which includes removal detection
			diffs, err := calculator.CalculateDiffs(ctx, tt.inputXR, tt.renderedOut)

			// Check error condition
			if tt.wantErr {
				if err == nil {
					t.Errorf("CalculateDiffs() expected error but got none")
				}

				return
			}

			if err != nil {
				t.Fatalf("CalculateDiffs() unexpected error: %v", err)
			}

			// Check that we have the expected number of diffs
			if diff := gcmp.Diff(len(tt.expectedDiffs), len(diffs)); diff != "" {
				t.Errorf("CalculateDiffs() number of diffs mismatch (-want +got):\n%s", diff)

				// Print what diffs we actually got to help debug
				for key, diff := range diffs {
					t.Logf("Found diff: %s of type %s", key, diff.DiffType)
				}
			}

			// Check each expected diff
			for expectedKey, expectedType := range tt.expectedDiffs {
				diff, found := diffs[expectedKey]
				if !found {
					t.Errorf("CalculateDiffs() missing expected diff for key %s", expectedKey)
					continue
				}

				if diff := gcmp.Diff(expectedType, diff.DiffType); diff != "" {
					t.Errorf("CalculateDiffs() diff type for key %s mismatch (-want +got):\n%s", expectedKey, diff)
				}

				// Check that LineDiffs is not empty for non-equal diffs
				// DiffTypeEqual entries may have empty LineDiffs since there are no changes
				if diff.DiffType != dt.DiffTypeEqual && len(diff.LineDiffs) == 0 {
					t.Errorf("CalculateDiffs() returned diff with empty LineDiffs for key %s", expectedKey)
				}
			}

			// Check for unexpected diffs
			for key := range diffs {
				if _, expected := tt.expectedDiffs[key]; !expected {
					t.Errorf("CalculateDiffs() returned unexpected diff for key %s", key)
				}
			}
		})
	}
}

func TestDefaultDiffCalculator_CalculateRemovedResourceDiffs(t *testing.T) {
	ctx := t.Context()

	// Create a test XR
	xr := tu.NewResource("example.org/v1", "XR", "test-xr").
		Build()

	// Create a resource tree with two resources
	resourceToKeep := tu.NewResource("example.org/v1", "Composed", "resource-to-keep").
		WithCompositeOwner("test-xr").
		WithCompositionResourceName("resource-to-keep").
		Build()

	resourceToRemove := tu.NewResource("example.org/v1", "Composed", "resource-to-remove").
		WithCompositeOwner("test-xr").
		WithCompositionResourceName("resource-to-remove").
		Build()

	tests := map[string]struct {
		setupMocks        func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager)
		renderedResources map[string]bool
		expectedRemoved   []string
		wantErr           bool
	}{
		"IdentifiesRemovedResources": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create a mock apply client (not used in this test)
				applyClient := tu.NewMockApplyClient().Build()

				// Create a resource tree client that returns a tree with both resources
				resourceTreeClient := tu.NewMockResourceTreeClient().
					WithResourceTreeFromXRAndComposed(
						xr,
						[]*un.Unstructured{resourceToKeep, resourceToRemove},
					).
					Build()

				// Create a resource manager (not directly used in this test)
				resourceClient := tu.NewMockResourceClient().Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			// Only include the "resource-to-keep" in rendered resources
			renderedResources: map[string]bool{
				"example.org/v1/Composed//resource-to-keep": true,
				// "example.org/v1/Composed//resource-to-remove" intentionally not included
			},
			expectedRemoved: []string{"resource-to-remove"},
			wantErr:         false,
		},
		"NoRemovedResources": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create a mock apply client (not used in this test)
				applyClient := tu.NewMockApplyClient().Build()

				// Create a resource tree client that returns a tree with both resources
				resourceTreeClient := tu.NewMockResourceTreeClient().
					WithResourceTreeFromXRAndComposed(
						xr,
						[]*un.Unstructured{resourceToKeep, resourceToRemove},
					).
					Build()

				// Create a resource manager (not directly used in this test)
				resourceClient := tu.NewMockResourceClient().Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			// Include all resources in rendered resources (nothing to remove)
			renderedResources: map[string]bool{
				"example.org/v1/Composed//resource-to-keep":   true,
				"example.org/v1/Composed//resource-to-remove": true,
			},
			expectedRemoved: []string{},
			wantErr:         false,
		},
		"ErrorGettingResourceTree": {
			setupMocks: func(t *testing.T) (k8.ApplyClient, xp.ResourceTreeClient, ResourceManager) {
				t.Helper()

				// Create a mock apply client (not used in this test)
				applyClient := tu.NewMockApplyClient().Build()

				// Create a resource tree client that returns an error
				resourceTreeClient := tu.NewMockResourceTreeClient().
					WithFailedResourceTreeFetch("failed to get resource tree").
					Build()

				// Create a resource manager (not directly used in this test)
				resourceClient := tu.NewMockResourceClient().Build()
				resourceManager := NewResourceManager(resourceClient, tu.NewMockDefinitionClient().Build(), tu.NewMockResourceTreeClient().Build(), tu.TestLogger(t, false))

				return applyClient, resourceTreeClient, resourceManager
			},
			renderedResources: map[string]bool{},
			expectedRemoved:   []string{},
			wantErr:           true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger := tu.TestLogger(t, false)

			// Setup mocks
			applyClient, resourceTreeClient, resourceManager := tt.setupMocks(t)

			// Create a diff calculator with the mocks
			calculator := NewDiffCalculator(
				applyClient,
				tu.NewMockAccessChecker().Build(),
				resourceTreeClient,
				resourceManager,
				logger,
				renderer.DefaultDiffOptions(),
				DryRunOnAll,
			)

			// Call the method under test
			diffs, err := calculator.CalculateRemovedResourceDiffs(ctx, xr, tt.renderedResources)

			if tt.wantErr {
				if err == nil {
					t.Errorf("CalculateRemovedResourceDiffs() expected error but got none")
				}

				return
			}

			// Even if we expect a warning-level error, we shouldn't get an actual error return
			if err != nil {
				t.Errorf("CalculateRemovedResourceDiffs() unexpected error: %v", err)
				return
			}

			// Check that the correct resources were identified for removal
			if diff := gcmp.Diff(len(tt.expectedRemoved), len(diffs)); diff != "" {
				t.Errorf("CalculateRemovedResourceDiffs() number of removed resources mismatch (-want +got):\n%s", diff)

				// Log what we found for debugging
				for key := range diffs {
					t.Logf("Found resource to remove: %s", key)
				}

				return
			}

			// Verify each expected removed resource is in the result
			for _, name := range tt.expectedRemoved {
				found := false

				for key, diff := range diffs {
					if strings.Contains(key, name) && diff.DiffType == dt.DiffTypeRemoved {
						found = true
						break
					}
				}

				if !found {
					t.Errorf("Expected to find %s marked for removal but did not", name)
				}
			}
		})
	}
}

func TestDefaultDiffCalculator_preserveCompositeLabel(t *testing.T) {
	tests := []struct {
		name              string
		current           *un.Unstructured
		desired           *un.Unstructured
		expectedLabel     string
		expectLabelExists bool
	}{
		{
			name: "PreservesCompositeLabelFromExistingResourceWithFullName",
			current: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name": "existing-resource",
						"labels": map[string]any{
							"crossplane.io/composite": "root-xr-name",
						},
					},
				},
			},
			desired: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name": "existing-resource",
						"annotations": map[string]any{
							"crossplane.io/composition-resource-name": "composed-resource",
						},
						"labels": map[string]any{
							"crossplane.io/composite": "child-xr-name",
						},
					},
				},
			},
			expectedLabel:     "root-xr-name",
			expectLabelExists: true,
		},
		{
			name: "PreservesCompositeLabelFromExistingResourceWithGenerateName",
			current: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"generateName": "existing-resource-",
						"name":         "existing-resource-abc123",
						"labels": map[string]any{
							"crossplane.io/composite": "root-xr-name",
						},
					},
				},
			},
			desired: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"generateName": "existing-resource-",
						"annotations": map[string]any{
							"crossplane.io/composition-resource-name": "composed-resource",
						},
						"labels": map[string]any{
							"crossplane.io/composite": "child-xr-name",
						},
					},
				},
			},
			expectedLabel:     "root-xr-name",
			expectLabelExists: true,
		},
		{
			name:    "NoPreservationWhenCurrentIsNil",
			current: nil,
			desired: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name": "new-resource",
						"labels": map[string]any{
							"crossplane.io/composite": "child-xr-name",
						},
					},
				},
			},
			expectedLabel:     "child-xr-name",
			expectLabelExists: true,
		},
		{
			name: "NoPreservationWhenCurrentHasNoCompositeLabel",
			current: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name": "existing-resource",
						"labels": map[string]any{
							"some-other-label": "value",
						},
					},
				},
			},
			desired: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name": "existing-resource",
						"labels": map[string]any{
							"crossplane.io/composite": "child-xr-name",
						},
					},
				},
			},
			expectedLabel:     "child-xr-name",
			expectLabelExists: true,
		},
		{
			name: "NoPreservationWhenCurrentHasNoLabels",
			current: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name": "existing-resource",
					},
				},
			},
			desired: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name": "existing-resource",
						"labels": map[string]any{
							"crossplane.io/composite": "child-xr-name",
						},
					},
				},
			},
			expectedLabel:     "child-xr-name",
			expectLabelExists: true,
		},
		{
			name: "CreatesLabelsMapWhenDesiredHasNoLabels",
			current: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name": "existing-resource",
						"labels": map[string]any{
							"crossplane.io/composite": "root-xr-name",
						},
					},
				},
			},
			desired: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name": "existing-resource",
						"annotations": map[string]any{
							"crossplane.io/composition-resource-name": "composed-resource",
						},
					},
				},
			},
			expectedLabel:     "root-xr-name",
			expectLabelExists: true,
		},
		{
			name: "PreservesOtherLabelsOnDesiredResource",
			current: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name": "existing-resource",
						"labels": map[string]any{
							"crossplane.io/composite": "root-xr-name",
						},
					},
				},
			},
			desired: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name": "existing-resource",
						"annotations": map[string]any{
							"crossplane.io/composition-resource-name": "nop-resource",
						},
						"labels": map[string]any{
							"crossplane.io/composite": "child-xr-name",
							"custom-label":            "custom-value",
							"another-label":           "another-value",
						},
					},
				},
			},
			expectedLabel:     "root-xr-name",
			expectLabelExists: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a DiffCalculator instance
			logger := tu.TestLogger(t, false)
			calc := &DefaultDiffCalculator{
				logger: logger,
			}

			// Call the function
			result := calc.preserveCompositeLabel(tt.current, tt.desired, "test-resource")

			// Verify the result
			labels := result.GetLabels()
			if tt.expectLabelExists {
				if labels == nil {
					t.Fatal("Expected labels map to exist, but it was nil")
				}

				actualLabel, exists := labels["crossplane.io/composite"]
				if !exists {
					t.Fatal("Expected crossplane.io/composite label to exist, but it did not")
				}

				if actualLabel != tt.expectedLabel {
					t.Errorf("Expected composite label to be %q, got %q", tt.expectedLabel, actualLabel)
				}

				// Verify that result is a deep copy only when we actually preserved a label
				// (i.e., when current had a composite label to preserve)
				if tt.current != nil && tt.current.GetLabels() != nil {
					if _, hasCompositeLabel := tt.current.GetLabels()["crossplane.io/composite"]; hasCompositeLabel {
						if result == tt.desired {
							t.Error("Expected result to be a deep copy when preserving label, but got the same pointer")
						}
					}
				}

				// Verify other labels are preserved
				if tt.name == "PreservesOtherLabelsOnDesiredResource" {
					if labels["custom-label"] != "custom-value" {
						t.Errorf("Expected custom-label to be preserved as 'custom-value', got %q", labels["custom-label"])
					}

					if labels["another-label"] != "another-value" {
						t.Errorf("Expected another-label to be preserved as 'another-value', got %q", labels["another-label"])
					}
				}
			}
		})
	}
}

// TestDefaultDiffCalculator_preserveExistingResourceIdentity tests that the identity
// of existing resources is preserved correctly, especially for nested XRs where
// the composition may transform the name but not use generateName.
func TestDefaultDiffCalculator_preserveExistingResourceIdentity(t *testing.T) {
	tests := []struct {
		name                 string
		current              *un.Unstructured
		desired              *un.Unstructured
		renderedName         string
		expectedName         string
		expectedGenerateName string
		expectPreserved      bool // whether identity should be preserved (deep copy made)
	}{
		{
			name: "PreservesIdentityWhenNameSetButGenerateNameEmpty",
			// This is the key scenario fixed: nested XRs where composition transforms
			// the name (e.g., adds -sg suffix) but doesn't use generateName
			current: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "platform.example.org/v1alpha1",
					"kind":       "XSecurityGroup",
					"metadata": map[string]any{
						"name": "dub-glass-redis-sg", // Actual cluster name with suffix
						// NOTE: generateName is intentionally NOT set (null/empty)
						"labels": map[string]any{
							"crossplane.io/composite": "root-xr",
						},
					},
				},
			},
			desired: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "platform.example.org/v1alpha1",
					"kind":       "XSecurityGroup",
					"metadata": map[string]any{
						"name": "dub-glass-redis", // Rendered name without suffix
						"annotations": map[string]any{
							"crossplane.io/composition-resource-name": "security-group",
						},
					},
				},
			},
			renderedName:         "dub-glass-redis",
			expectedName:         "dub-glass-redis-sg", // Should preserve cluster name
			expectedGenerateName: "",                   // Should preserve empty generateName
			expectPreserved:      true,
		},
		{
			name: "PreservesIdentityWithGenerateName",
			// Traditional generateName scenario - should still work
			current: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name":         "my-resource-abc123",
						"generateName": "my-resource-",
						"labels": map[string]any{
							"crossplane.io/composite": "root-xr",
						},
					},
				},
			},
			desired: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"generateName": "my-resource-",
						"annotations": map[string]any{
							"crossplane.io/composition-resource-name": "nop-resource",
						},
					},
				},
			},
			renderedName:         "",
			expectedName:         "my-resource-abc123",
			expectedGenerateName: "my-resource-",
			expectPreserved:      true,
		},
		{
			name:    "NoPreservationWhenCurrentIsNil",
			current: nil,
			desired: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"name": "new-resource",
					},
				},
			},
			renderedName:         "new-resource",
			expectedName:         "new-resource",
			expectedGenerateName: "",
			expectPreserved:      false,
		},
		{
			name: "NoPreservationWhenCurrentHasNoName",
			current: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"generateName": "my-resource-",
						// No name set yet (resource not created)
					},
				},
			},
			desired: &un.Unstructured{
				Object: map[string]any{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]any{
						"generateName": "my-resource-",
					},
				},
			},
			renderedName:         "",
			expectedName:         "",
			expectedGenerateName: "my-resource-",
			expectPreserved:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create the calculator
			calculator := &DefaultDiffCalculator{
				logger: tu.TestLogger(t, false),
			}

			// Call the method
			result := calculator.preserveExistingResourceIdentity(tt.current, tt.desired, "test-resource", tt.renderedName)

			// Check the resulting name
			if result.GetName() != tt.expectedName {
				t.Errorf("Expected name %q, got %q", tt.expectedName, result.GetName())
			}

			// Check the resulting generateName
			if result.GetGenerateName() != tt.expectedGenerateName {
				t.Errorf("Expected generateName %q, got %q", tt.expectedGenerateName, result.GetGenerateName())
			}

			// Check if a deep copy was made when expected
			if tt.expectPreserved {
				if result == tt.desired {
					t.Error("Expected result to be a deep copy when preserving identity, but got the same pointer")
				}
			} else {
				if result != tt.desired {
					t.Error("Expected result to be the same pointer when not preserving identity, but got a different pointer")
				}
			}
		})
	}
}

// TestDefaultDiffCalculator_DegradationWarnsOncePerGroup covers AC-R9: a degraded dry run raises a
// user-facing advisory, and raises it once per GVK + namespace + reason rather than once per
// resource. A composition rendering forty new resources of one kind must not produce forty identical
// warnings.
//
// This is a property of a calculator INSTANCE across calls, which is why it cannot be a row in the
// per-call table above: a single calculator is reused for every resource in a run, and that reuse is
// exactly what the deduplication depends on.
func TestDefaultDiffCalculator_DegradationWarnsOncePerGroup(t *testing.T) {
	ctx := t.Context()

	forbidden := func(context.Context, *un.Unstructured) (*un.Unstructured, error) {
		return nil, apierrors.NewForbidden(schema.GroupResource{Group: "example.org", Resource: "testresources"}, "", errors.New("nope"))
	}

	// Three resources of one kind in one namespace, plus one of the same kind in another namespace and
	// one of a different kind. Correct behaviour is three advisories, not five.
	desired := []*un.Unstructured{
		tu.NewResource("example.org/v1", "TestResource", "a").WithSpecField("f", "1").Build(),
		tu.NewResource("example.org/v1", "TestResource", "b").WithSpecField("f", "2").Build(),
		tu.NewResource("example.org/v1", "TestResource", "c").WithSpecField("f", "3").Build(),
		tu.NewResource("example.org/v1", "OtherResource", "d").WithSpecField("f", "4").Build(),
	}
	desired[0].SetNamespace("ns-a")
	desired[1].SetNamespace("ns-a")
	desired[2].SetNamespace("ns-b")
	desired[3].SetNamespace("ns-a")

	logger := tu.NewAdvisoryCapturingLogger(t)

	calculator := NewDiffCalculator(
		tu.NewMockApplyClient().WithDryRunCreate(forbidden).Build(),
		tu.NewMockAccessChecker().WithDenied("no create").Build(),
		tu.NewMockResourceTreeClient().Build(),
		NewResourceManager(
			tu.NewMockResourceClient().WithResourceNotFound().Build(),
			tu.NewMockDefinitionClient().Build(),
			tu.NewMockResourceTreeClient().Build(),
			tu.TestLogger(t, false),
		),
		logger,
		renderer.DefaultDiffOptions(),
		DryRunOnAll,
	)

	for _, d := range desired {
		diff, err := calculator.CalculateDiff(ctx, nil, d)
		if err != nil {
			t.Fatalf("CalculateDiff(%s/%s) unexpected error: %v", d.GetKind(), d.GetName(), err)
		}

		// Every degraded resource must carry the typed field; the advisory is the human channel and
		// cannot substitute for it, because a warning has no resource anchor.
		if diff.DryRun == nil || diff.DryRun.SkipReason != dt.DryRunSkipForbidden {
			t.Errorf("%s/%s: expected dryRun.skipReason=forbidden, got %+v", d.GetKind(), d.GetName(), diff.DryRun)
		}
	}

	// 3 = (TestResource, ns-a), (TestResource, ns-b), (OtherResource, ns-a).
	if got := len(logger.Advisories()); got != 3 {
		t.Errorf("got %d advisories for 4 degraded resources across 3 GVK+namespace groups, want 3: %v",
			got, logger.Advisories())
	}
}

func TestCalculateNonRemovalDiffs_NilCompositeResource(t *testing.T) {
	calculator := &DefaultDiffCalculator{
		logger: tu.TestLogger(t, false),
	}

	xr := cmp.New()
	xr.SetAPIVersion("example.org/v1")
	xr.SetKind("XMyResource")
	xr.SetName("test-xr")

	_, _, err := calculator.CalculateNonRemovalDiffs(
		t.Context(),
		xr,
		nil,
		render.CompositionOutputs{CompositeResource: nil},
	)
	if err == nil {
		t.Fatal("expected error when CompositeResource is nil, got nil")
	}

	want := "render produced no composite resource (possible fatal pipeline error)"
	if err.Error() != want {
		t.Errorf("error message = %q, want %q", err.Error(), want)
	}
}
