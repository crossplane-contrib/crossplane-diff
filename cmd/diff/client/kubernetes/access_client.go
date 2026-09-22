package kubernetes

import (
	"context"
	"fmt"
	"sync"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

const (
	// ssarAPIVersion and ssarKind identify the SelfSubjectAccessReview type we
	// POST through the dynamic client. SelfSubjectAccessReview is used (rather
	// than SubjectAccessReview) because create on selfsubjectaccessreviews is
	// granted to system:authenticated by default via the system:basic-user
	// ClusterRole, so the check itself needs no special permission.
	ssarAPIVersion = "authorization.k8s.io/v1"
	ssarKind       = "SelfSubjectAccessReview"
)

// ssarGVR returns the GroupVersionResource of SelfSubjectAccessReview. The
// resource is cluster-scoped, so requests are made without a namespace even when
// the resourceAttributes being asked about are namespaced.
func ssarGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "authorization.k8s.io",
		Version:  "v1",
		Resource: "selfsubjectaccessreviews",
	}
}

// AccessChecker answers authorization questions about the current credentials.
type AccessChecker interface {
	// Can reports whether the current credentials may perform verb on objects
	// of the given GVK in the given namespace (empty namespace for
	// cluster-scoped resources). verb is an API verb such as "create" or
	// "patch" and must not be empty. When allowed is false, reason carries a
	// human-readable explanation suitable for display. A non-nil error means
	// the question could not be answered at all, which callers must treat as a
	// tool error rather than as a denial.
	Can(ctx context.Context, gvk schema.GroupVersionKind, namespace, verb string) (allowed bool, reason string, err error)
}

// accessCacheKey identifies a single authorization question. All three
// components matter: authorization is per-resource (plural) rather than
// per-Kind, per-namespace (cluster-scoped resources key on the empty
// namespace), and per-verb. The verb in particular cannot be dropped -- holding
// patch but not create on one GVK is precisely the configuration this feature
// exists to cope with, so reusing one verb's answer for another would invert
// the decision it is being asked to make.
type accessCacheKey struct {
	gvr       schema.GroupVersionResource
	namespace string
	verb      string
}

// accessDecision is a memoized answer to one authorization question.
type accessDecision struct {
	allowed bool
	reason  string
}

// DefaultAccessClient implements AccessChecker via SelfSubjectAccessReview,
// memoizing each answer for the lifetime of the client.
type DefaultAccessClient struct {
	dynamicClient dynamic.Interface
	typeConverter TypeConverter
	logger        logging.Logger

	// Decision caching. Reviews are consulted lazily -- only once a dry-run has
	// actually come back 403 -- so the happy path costs nothing. On the unhappy
	// path a composition rendering 40 objects of one kind would otherwise issue
	// 40 identical SSARs; memoization makes it one.
	//
	// The mutex is released across the API call, so two goroutines racing on a
	// cold key can both issue the SSAR. That is benign -- they compute the same
	// decision and the second write is idempotent -- and is preferred over
	// holding a lock across network I/O.
	cache      map[accessCacheKey]accessDecision
	cacheMutex sync.RWMutex
}

// NewAccessClient creates a new DefaultAccessClient.
func NewAccessClient(clients *core.Clients, converter TypeConverter, logger logging.Logger) AccessChecker {
	return &DefaultAccessClient{
		dynamicClient: clients.Dynamic,
		typeConverter: converter,
		logger:        logger,
		cache:         make(map[accessCacheKey]accessDecision),
	}
}

// Can reports whether the current credentials may perform verb on objects of the
// given GVK in the given namespace.
func (c *DefaultAccessClient) Can(ctx context.Context, gvk schema.GroupVersionKind, namespace, verb string) (bool, string, error) {
	// An empty verb would ask the apiserver about the verb "", which no rule
	// grants, so it would come back denied and look like a legitimate RBAC
	// limitation. Refuse it instead: silently degrading on a caller bug is the
	// failure mode this client is built to avoid.
	if verb == "" {
		return false, "", errors.Errorf("cannot check permission for %s: no verb given", gvk.String())
	}

	// SSAR's resourceAttributes are expressed in terms of the plural resource
	// name, not the Kind, so the GVK must be resolved first. DefaultTypeConverter
	// memoizes this itself, so a cache hit below costs no API call either.
	gvr, err := c.typeConverter.GVKToGVR(ctx, gvk)
	if err != nil {
		c.logger.Debug("Failed to convert GVK to GVR", "gvk", gvk.String(), "error", err)
		return false, "", errors.Wrapf(err, "cannot check %s permission for %s", verb, gvk.String())
	}

	key := accessCacheKey{gvr: gvr, namespace: namespace, verb: verb}

	c.cacheMutex.RLock()

	if decision, ok := c.cache[key]; ok {
		c.cacheMutex.RUnlock()
		c.logger.Debug("Using cached permission", "verb", verb, "resource", gvr.String(), "namespace", namespace, "allowed", decision.allowed)

		return decision.allowed, decision.reason, nil
	}

	c.cacheMutex.RUnlock()

	decision, err := c.review(ctx, gvr, namespace, verb)
	if err != nil {
		return false, "", err
	}

	c.cacheMutex.Lock()
	c.cache[key] = decision
	c.cacheMutex.Unlock()

	return decision.allowed, decision.reason, nil
}

// review issues one SelfSubjectAccessReview and interprets its status.
func (c *DefaultAccessClient) review(ctx context.Context, gvr schema.GroupVersionResource, namespace, verb string) (accessDecision, error) {
	c.logger.Debug("Checking permission", "verb", verb, "resource", gvr.String(), "namespace", namespace)

	target := describeTarget(gvr, namespace)

	ssar := &un.Unstructured{
		Object: map[string]any{
			"apiVersion": ssarAPIVersion,
			"kind":       ssarKind,
			"spec": map[string]any{
				"resourceAttributes": map[string]any{
					"group":     gvr.Group,
					"resource":  gvr.Resource,
					"namespace": namespace,
					"verb":      verb,
				},
			},
		},
	}

	// SelfSubjectAccessReview is cluster-scoped -- no .Namespace() here. The
	// namespace being asked about travels in spec.resourceAttributes instead.
	result, err := c.dynamicClient.Resource(ssarGVR()).Create(ctx, ssar, metav1.CreateOptions{})
	if err != nil {
		c.logger.Debug("SelfSubjectAccessReview failed", "verb", verb, "resource", gvr.String(), "namespace", namespace, "error", err)

		return accessDecision{}, errors.Wrapf(err, "cannot create SelfSubjectAccessReview for %s on %s", verb, target)
	}

	allowed, found, err := un.NestedBool(result.Object, "status", "allowed")
	if err != nil {
		return accessDecision{}, errors.Wrapf(err, "cannot read status.allowed from SelfSubjectAccessReview for %s on %s", verb, target)
	}

	// status.allowed is a required, non-omitempty field of
	// SubjectAccessReviewStatus, so a real apiserver always sends it -- false
	// included. Its absence therefore means we did not get an authorization
	// answer at all, which is a broken contract rather than a denial. Defaulting
	// to false here would silently degrade every dry-run against such a server
	// and mislabel it "forbidden"; defaulting to true would be far worse. Fail.
	if !found {
		return accessDecision{}, errors.Errorf("SelfSubjectAccessReview for %s on %s returned no status.allowed", verb, target)
	}

	reason, _, err := un.NestedString(result.Object, "status", "reason")
	if err != nil {
		return accessDecision{}, errors.Wrapf(err, "cannot read status.reason from SelfSubjectAccessReview for %s on %s", verb, target)
	}

	evalErr, _, err := un.NestedString(result.Object, "status", "evaluationError")
	if err != nil {
		return accessDecision{}, errors.Wrapf(err, "cannot read status.evaluationError from SelfSubjectAccessReview for %s on %s", verb, target)
	}

	// status.evaluationError means part of the authorizer chain failed to
	// evaluate. Kubernetes documents that allowed may still be meaningful in
	// that case, but "may still be" is not good enough here: this check exists so
	// that a 403 from a dry-run can be attributed to admission rather than to
	// RBAC. A wrongly-allowed answer converts an RBAC
	// denial into a reported cluster rejection (exit 2) -- a false finding --
	// whereas a wrongly-denied answer only costs fidelity on that resource.
	// So an evaluation error is never read as permission.
	//
	// It is also not returned as an error: the authorizer answered, just
	// incompletely. Failing the whole diff over a partial authorizer hiccup
	// would be a worse outcome than the graceful degradation this feature is
	// designed around. The message is surfaced in reason so the cause is visible.
	if evalErr != "" {
		allowed = false

		if reason == "" {
			reason = fmt.Sprintf("authorizer evaluation error: %s", evalErr)
		} else {
			reason = fmt.Sprintf("%s (authorizer evaluation error: %s)", reason, evalErr)
		}
	}

	// The apiserver is not obliged to give a reason, but reason is displayed to
	// the user, so an empty denial needs saying something.
	if !allowed && reason == "" {
		reason = fmt.Sprintf("not authorized to %s %s", verb, target)
	}

	c.logger.Debug("Permission checked", "verb", verb, "resource", gvr.String(), "namespace", namespace, "allowed", allowed, "reason", reason)

	return accessDecision{allowed: allowed, reason: reason}, nil
}

// describeTarget renders the subject of an authorization question for messages.
func describeTarget(gvr schema.GroupVersionResource, namespace string) string {
	if namespace == "" {
		return fmt.Sprintf("%s at cluster scope", gvr.GroupResource().String())
	}

	return fmt.Sprintf("%s in namespace %q", gvr.GroupResource().String(), namespace)
}
