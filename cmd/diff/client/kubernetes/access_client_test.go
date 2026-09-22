package kubernetes

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	kt "k8s.io/client-go/testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// newSSARClient builds a fake dynamic client that knows about the
// SelfSubjectAccessReview GVR. The GVR must be registered explicitly because an
// empty scheme gives the fake client nothing to guess from.
func newSSARClient() *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			ssarGVR(): ssarKind + "List",
		},
	)
}

// withSSARStatus installs a reactor answering every SelfSubjectAccessReview with
// the given status. This is mandatory rather than convenient: the fake dynamic
// client's default reaction echoes the submitted object back verbatim, so
// without a reactor status.allowed would be absent and the test would be
// measuring our handling of a malformed response rather than of a real answer.
func withSSARStatus(dc *fake.FakeDynamicClient, status map[string]any) {
	dc.PrependReactor("create", ssarGVR().Resource, func(action kt.Action) (bool, runtime.Object, error) {
		submitted, err := submittedSSAR(action)
		if err != nil {
			return true, nil, err
		}

		result := submitted.DeepCopy()
		result.Object["status"] = runtime.DeepCopyJSON(status)

		return true, result, nil
	})
}

// withSSARStatusPerVerb installs a reactor that answers according to the verb
// under review, so a test can model credentials that hold one verb but not
// another on the same resource. A verb with no entry fails loudly rather than
// falling back to a default, since a silent default here would let a
// verb-blind implementation look correct.
func withSSARStatusPerVerb(dc *fake.FakeDynamicClient, byVerb map[string]map[string]any) {
	dc.PrependReactor("create", ssarGVR().Resource, func(action kt.Action) (bool, runtime.Object, error) {
		submitted, err := submittedSSAR(action)
		if err != nil {
			return true, nil, err
		}

		verb, _, err := un.NestedString(submitted.Object, "spec", "resourceAttributes", "verb")
		if err != nil {
			return true, nil, err
		}

		status, ok := byVerb[verb]
		if !ok {
			return true, nil, errors.Errorf("test did not define an answer for verb %q", verb)
		}

		result := submitted.DeepCopy()
		result.Object["status"] = runtime.DeepCopyJSON(status)

		return true, result, nil
	})
}

// withSSAREcho installs a reactor that echoes the submitted review back with no
// status at all, reproducing a server that did not answer the question.
func withSSAREcho(dc *fake.FakeDynamicClient) {
	dc.PrependReactor("create", ssarGVR().Resource, func(action kt.Action) (bool, runtime.Object, error) {
		submitted, err := submittedSSAR(action)
		if err != nil {
			return true, nil, err
		}

		return true, submitted.DeepCopy(), nil
	})
}

// withSSARError installs a reactor that fails the review call itself.
func withSSARError(dc *fake.FakeDynamicClient, err error) {
	dc.PrependReactor("create", ssarGVR().Resource, func(kt.Action) (bool, runtime.Object, error) {
		return true, nil, err
	})
}

func submittedSSAR(action kt.Action) (*un.Unstructured, error) {
	create, ok := action.(kt.CreateAction)
	if !ok {
		return nil, errors.Errorf("expected a create action, got %T", action)
	}

	obj, ok := create.GetObject().(*un.Unstructured)
	if !ok {
		return nil, errors.Errorf("expected an unstructured object, got %T", create.GetObject())
	}

	return obj, nil
}

// ssarCreates returns every SelfSubjectAccessReview create recorded by the fake
// client, so tests can assert on the number of API calls actually issued.
func ssarCreates(dc *fake.FakeDynamicClient) []kt.CreateAction {
	var creates []kt.CreateAction

	for _, a := range dc.Actions() {
		// "create" here is the verb of the call that submits the review, not the
		// verb under review -- the latter lives in spec.resourceAttributes.
		if a.GetVerb() != "create" || a.GetResource() != ssarGVR() {
			continue
		}

		if create, ok := a.(kt.CreateAction); ok {
			creates = append(creates, create)
		}
	}

	return creates
}

// accessResult is one Can answer, in a cmp-comparable shape.
type accessResult struct {
	Allowed bool
	Reason  string
}

func TestAccessClient_Can(t *testing.T) {
	exampleGVK := schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "ExampleResource"}
	otherGVK := schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "OtherResource"}

	// step is one Can call plus its expectation. Steps run in order
	// against a single client, which is what makes the caching cases expressible.
	type step struct {
		gvk       schema.GroupVersionKind
		namespace string
		verb      string
		want      accessResult
		wantErr   string // substring; empty means no error expected
	}

	tests := map[string]struct {
		reason    string
		setup     func() (*fake.FakeDynamicClient, TypeConverter)
		steps     []step
		wantSSARs int
	}{
		"Allowed": {
			reason: "An SSAR that allows the action should report allowed with no error",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{"allowed": true})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "test-namespace", want: accessResult{Allowed: true}},
			},
			wantSSARs: 1,
		},
		"AllowedWithReason": {
			reason: "A reason accompanying an allow should be surfaced, not discarded",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{
					"allowed": true,
					"reason":  `RBAC: allowed by ClusterRoleBinding "cluster-admin"`,
				})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "test-namespace", want: accessResult{
					Allowed: true,
					Reason:  `RBAC: allowed by ClusterRoleBinding "cluster-admin"`,
				}},
			},
			wantSSARs: 1,
		},
		"DeniedWithReason": {
			reason: "A denied SSAR should report not-allowed and surface the SSAR's own reason",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{
					"allowed": false,
					"denied":  true,
					"reason":  "not permitted by any authorizer",
				})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "test-namespace", want: accessResult{
					Allowed: false,
					Reason:  "not permitted by any authorizer",
				}},
			},
			wantSSARs: 1,
		},
		"DeniedWithoutReason": {
			reason: "A denial with no reason should still produce a displayable reason naming the target",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{"allowed": false})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "test-namespace", want: accessResult{
					Allowed: false,
					Reason:  `not authorized to create exampleresources.example.org in namespace "test-namespace"`,
				}},
			},
			wantSSARs: 1,
		},
		"DeniedClusterScopedWithoutReason": {
			reason: "A cluster-scoped denial should not claim a namespace it was never asked about",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{"allowed": false})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "", want: accessResult{
					Allowed: false,
					Reason:  "not authorized to create exampleresources.example.org at cluster scope",
				}},
			},
			wantSSARs: 1,
		},
		"EvaluationErrorIsNotPermission": {
			reason: "An allow accompanied by an evaluation error must not be read as permission",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{
					"allowed":         true,
					"evaluationError": "webhook authorizer unreachable",
				})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "test-namespace", want: accessResult{
					Allowed: false,
					Reason:  "authorizer evaluation error: webhook authorizer unreachable",
				}},
			},
			wantSSARs: 1,
		},
		"EvaluationErrorAugmentsReason": {
			reason: "An evaluation error alongside a reason should keep both, since either may explain the denial",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{
					"allowed":         false,
					"reason":          "no opinion",
					"evaluationError": "webhook authorizer unreachable",
				})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "test-namespace", want: accessResult{
					Allowed: false,
					Reason:  "no opinion (authorizer evaluation error: webhook authorizer unreachable)",
				}},
			},
			wantSSARs: 1,
		},
		"SecondCallIsCached": {
			reason: "Two calls for the same GVR and namespace should issue exactly one API call",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{
					"allowed": false,
					"reason":  "not permitted by any authorizer",
				})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "test-namespace", want: accessResult{
					Allowed: false,
					Reason:  "not permitted by any authorizer",
				}},
				{gvk: exampleGVK, verb: "create", namespace: "test-namespace", want: accessResult{
					Allowed: false,
					Reason:  "not permitted by any authorizer",
				}},
			},
			wantSSARs: 1,
		},
		"RepeatedCallsIssueOneReview": {
			reason: "Every call after the first for one GVR and namespace should be served from the cache",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{"allowed": true})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "ns-a", want: accessResult{Allowed: true}},
				{gvk: exampleGVK, verb: "create", namespace: "ns-a", want: accessResult{Allowed: true}},
				{gvk: exampleGVK, verb: "create", namespace: "ns-a", want: accessResult{Allowed: true}},
			},
			wantSSARs: 1,
		},
		"DifferentNamespacesAreCachedSeparately": {
			reason: "Authorization is per-namespace, so a different namespace must issue its own call",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{"allowed": true})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "ns-a", want: accessResult{Allowed: true}},
				{gvk: exampleGVK, verb: "create", namespace: "ns-b", want: accessResult{Allowed: true}},
				{gvk: exampleGVK, verb: "create", namespace: "", want: accessResult{Allowed: true}},
				{gvk: exampleGVK, verb: "create", namespace: "ns-a", want: accessResult{Allowed: true}},
			},
			wantSSARs: 3,
		},
		"DifferentResourcesAreCachedSeparately": {
			reason: "Authorization is per-resource, so a different GVR must issue its own call",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{"allowed": true})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "ns-a", want: accessResult{Allowed: true}},
				{gvk: otherGVK, verb: "create", namespace: "ns-a", want: accessResult{Allowed: true}},
				{gvk: exampleGVK, verb: "create", namespace: "ns-a", want: accessResult{Allowed: true}},
				{gvk: otherGVK, verb: "create", namespace: "ns-a", want: accessResult{Allowed: true}},
			},
			wantSSARs: 2,
		},
		"ReviewCallFails": {
			reason: "An SSAR that itself errors must propagate an error rather than be read as a denial",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARError(dc, apierrors.NewForbidden(ssarGVR().GroupResource(), "", errors.New("no permission to review")))

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{
					gvk:       exampleGVK,
					verb:      "create",
					namespace: "test-namespace",
					wantErr:   `cannot create SelfSubjectAccessReview for create on exampleresources.example.org in namespace "test-namespace"`,
				},
			},
			wantSSARs: 1,
		},
		"ReviewFailureIsNotCached": {
			reason: "A failed review must not be memoized, since it never produced a decision",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARError(dc, errors.New("transient failure"))

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "test-namespace", wantErr: "transient failure"},
				{gvk: exampleGVK, verb: "create", namespace: "test-namespace", wantErr: "transient failure"},
			},
			wantSSARs: 2,
		},
		"MissingStatusAllowedFails": {
			reason: "status.allowed is required, so its absence is a broken response and must not be read as a decision",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSAREcho(dc)

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{
					gvk:       exampleGVK,
					verb:      "create",
					namespace: "test-namespace",
					wantErr:   "returned no status.allowed",
				},
			},
			wantSSARs: 1,
		},
		"MalformedStatusAllowedFails": {
			reason: "A non-boolean status.allowed is a broken response and must not be read as a decision",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{"allowed": "yes"})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{
					gvk:       exampleGVK,
					verb:      "create",
					namespace: "test-namespace",
					wantErr:   "cannot read status.allowed",
				},
			},
			wantSSARs: 1,
		},
		"DifferentVerbsAreCachedSeparately": {
			reason: "Holding patch but not create on one resource is the configuration this feature exists " +
				"for, so each verb must get its own review and its own answer",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatusPerVerb(dc, map[string]map[string]any{
					"create": {"allowed": false, "reason": "no create on exampleresources"},
					"patch":  {"allowed": true},
				})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{gvk: exampleGVK, verb: "create", namespace: "test-namespace", want: accessResult{
					Allowed: false,
					Reason:  "no create on exampleresources",
				}},
				{gvk: exampleGVK, verb: "patch", namespace: "test-namespace", want: accessResult{Allowed: true}},
				// Repeat both, reversed, to prove each verb's answer is cached
				// under its own key rather than overwriting the other's.
				{gvk: exampleGVK, verb: "patch", namespace: "test-namespace", want: accessResult{Allowed: true}},
				{gvk: exampleGVK, verb: "create", namespace: "test-namespace", want: accessResult{
					Allowed: false,
					Reason:  "no create on exampleresources",
				}},
			},
			wantSSARs: 2,
		},
		"EmptyVerbFails": {
			reason: "An empty verb is a caller bug that would look like an RBAC denial, so it must fail loudly",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{"allowed": true})

				return dc, tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build()
			},
			steps: []step{
				{
					gvk:       exampleGVK,
					verb:      "",
					namespace: "test-namespace",
					wantErr:   "no verb given",
				},
			},
			wantSSARs: 0,
		},
		"ConverterFailureFails": {
			reason: "Without a plural resource name there is no question to ask, so the failure must propagate",
			setup: func() (*fake.FakeDynamicClient, TypeConverter) {
				dc := newSSARClient()
				withSSARStatus(dc, map[string]any{"allowed": true})

				converter := tu.NewMockTypeConverter().
					WithGVKToGVR(func(context.Context, schema.GroupVersionKind) (schema.GroupVersionResource, error) {
						return schema.GroupVersionResource{}, errors.New("conversion error")
					}).Build()

				return dc, converter
			},
			steps: []step{
				{
					gvk:       exampleGVK,
					verb:      "create",
					namespace: "test-namespace",
					wantErr:   "cannot check create permission for example.org/v1, Kind=ExampleResource",
				},
			},
			wantSSARs: 0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dc, converter := tc.setup()
			c := NewAccessClient(&core.Clients{Dynamic: dc}, converter, tu.TestLogger(t, false))

			var want, got []accessResult

			for i, s := range tc.steps {
				allowed, gotReason, err := c.Can(t.Context(), s.gvk, s.namespace, s.verb)

				if s.wantErr != "" {
					switch {
					case err == nil:
						t.Errorf("\n%s\nCan(...) step %d: expected error but got none", tc.reason, i)
					case !strings.Contains(err.Error(), s.wantErr):
						t.Errorf("\n%s\nCan(...) step %d: expected error containing %q, got %q",
							tc.reason, i, s.wantErr, err.Error())
					}

					if allowed {
						t.Errorf("\n%s\nCan(...) step %d: an error must never report allowed", tc.reason, i)
					}

					continue
				}

				if err != nil {
					t.Errorf("\n%s\nCan(...) step %d: unexpected error: %v", tc.reason, i, err)
					continue
				}

				want = append(want, s.want)
				got = append(got, accessResult{Allowed: allowed, Reason: gotReason})
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("\n%s\nCan(...): -want, +got:\n%s", tc.reason, diff)
			}

			if gotSSARs := len(ssarCreates(dc)); gotSSARs != tc.wantSSARs {
				t.Errorf("\n%s\nCan(...): want %d SelfSubjectAccessReview API calls, got %d",
					tc.reason, tc.wantSSARs, gotSSARs)
			}
		})
	}
}

// TestAccessClient_ReviewRequest pins the shape of the review we send. Asking
// the wrong question (for example by sending the Kind instead of the plural
// resource, or by attaching the namespace to the review object rather than to
// its resourceAttributes) would yield a confidently wrong answer, which is the
// one failure mode this client must not have.
func TestAccessClient_ReviewRequest(t *testing.T) {
	tests := map[string]struct {
		reason    string
		gvk       schema.GroupVersionKind
		namespace string
		verb      string
		want      *un.Unstructured
	}{
		"NamespacedResource": {
			reason:    "A namespaced request should carry the namespace in resourceAttributes",
			gvk:       schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "ExampleResource"},
			namespace: "test-namespace",
			verb:      "create",
			want: &un.Unstructured{Object: map[string]any{
				"apiVersion": "authorization.k8s.io/v1",
				"kind":       "SelfSubjectAccessReview",
				"spec": map[string]any{
					"resourceAttributes": map[string]any{
						"group":     "example.org",
						"resource":  "exampleresources",
						"namespace": "test-namespace",
						"verb":      "create",
					},
				},
			}},
		},
		"ClusterScopedResource": {
			reason:    "A cluster-scoped request should carry an empty namespace",
			gvk:       schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "ClusterResource"},
			namespace: "",
			verb:      "create",
			want: &un.Unstructured{Object: map[string]any{
				"apiVersion": "authorization.k8s.io/v1",
				"kind":       "SelfSubjectAccessReview",
				"spec": map[string]any{
					"resourceAttributes": map[string]any{
						"group":     "example.org",
						"resource":  "clusterresources",
						"namespace": "",
						"verb":      "create",
					},
				},
			}},
		},
		"CoreGroupResource": {
			reason:    "A core-group resource should send an empty group, as the API expects",
			gvk:       schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
			namespace: "test-namespace",
			verb:      "create",
			want: &un.Unstructured{Object: map[string]any{
				"apiVersion": "authorization.k8s.io/v1",
				"kind":       "SelfSubjectAccessReview",
				"spec": map[string]any{
					"resourceAttributes": map[string]any{
						"group":     "",
						"resource":  "configmaps",
						"namespace": "test-namespace",
						"verb":      "create",
					},
				},
			}},
		},
		"PatchVerb": {
			reason:    "The verb under review should be whatever the caller asked about, not a hardcoded create",
			gvk:       schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "ExampleResource"},
			namespace: "test-namespace",
			verb:      "patch",
			want: &un.Unstructured{Object: map[string]any{
				"apiVersion": "authorization.k8s.io/v1",
				"kind":       "SelfSubjectAccessReview",
				"spec": map[string]any{
					"resourceAttributes": map[string]any{
						"group":     "example.org",
						"resource":  "exampleresources",
						"namespace": "test-namespace",
						"verb":      "patch",
					},
				},
			}},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dc := newSSARClient()
			withSSARStatus(dc, map[string]any{"allowed": true})

			c := NewAccessClient(&core.Clients{Dynamic: dc},
				tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build(),
				tu.TestLogger(t, false))

			if _, _, err := c.Can(t.Context(), tc.gvk, tc.namespace, tc.verb); err != nil {
				t.Fatalf("\n%s\nCan(...): unexpected error: %v", tc.reason, err)
			}

			creates := ssarCreates(dc)
			if len(creates) != 1 {
				t.Fatalf("\n%s\nCan(...): want 1 SelfSubjectAccessReview API call, got %d", tc.reason, len(creates))
			}

			// The review is cluster-scoped; a namespace on the action itself
			// would mean we had scoped the resource client by mistake.
			if ns := creates[0].GetNamespace(); ns != "" {
				t.Errorf("\n%s\nCan(...): SelfSubjectAccessReview must be cluster-scoped, got namespace %q", tc.reason, ns)
			}

			if diff := cmp.Diff(tc.want, creates[0].GetObject()); diff != "" {
				t.Errorf("\n%s\nCan(...): -want review, +got review:\n%s", tc.reason, diff)
			}
		})
	}
}

// TestAccessClient_CanIsConcurrencySafe checks the decision cache under
// concurrent use; the calculator holding this client may process several XRs.
// The real assertion here is made by -race: an unguarded map write would be
// reported. Concurrent cold-key callers may each issue a review, which is
// benign, so the call count is bounded rather than pinned.
func TestAccessClient_CanIsConcurrencySafe(t *testing.T) {
	const goroutines = 16

	reason := "Concurrent callers should get one consistent decision with no data race"

	dc := newSSARClient()
	withSSARStatus(dc, map[string]any{"allowed": false, "reason": "not permitted by any authorizer"})

	c := NewAccessClient(&core.Clients{Dynamic: dc},
		tu.NewMockTypeConverter().WithDefaultGVKToGVR().Build(),
		tu.TestLogger(t, false))

	gvk := schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "ExampleResource"}

	results := make([]accessResult, goroutines)
	errs := make([]error, goroutines)

	var wg sync.WaitGroup

	for i := range goroutines {
		wg.Go(func() {
			allowed, gotReason, err := c.Can(t.Context(), gvk, "test-namespace", "create")
			results[i] = accessResult{Allowed: allowed, Reason: gotReason}
			errs[i] = err
		})
	}

	wg.Wait()

	want := make([]accessResult, goroutines)
	for i := range want {
		want[i] = accessResult{Allowed: false, Reason: "not permitted by any authorizer"}
	}

	for i, err := range errs {
		if err != nil {
			t.Fatalf("\n%s\nCan(...) goroutine %d: unexpected error: %v", reason, i, err)
		}
	}

	if diff := cmp.Diff(want, results); diff != "" {
		t.Errorf("\n%s\nCan(...): -want, +got:\n%s", reason, diff)
	}

	if got := len(ssarCreates(dc)); got < 1 || got > goroutines {
		t.Errorf("\n%s\nCan(...): want between 1 and %d reviews, got %d", reason, goroutines, got)
	}
}
