package crossplane

import (
	"context"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
)

// CredentialClient handles fetching credentials referenced by composition pipelines.
type CredentialClient interface {
	// FetchCompositionCredentials extracts credential refs from a composition's pipeline steps and
	// fetches the referenced secrets from the cluster. Secrets that do not exist are reported in the
	// result's Absent list rather than failing the fetch: they may be injected at runtime (e.g.
	// workload identity) or supplied by the caller via --function-credentials. Any other failure to
	// read a referenced secret is returned as an error.
	FetchCompositionCredentials(ctx context.Context, comp *apiextensionsv1.Composition) (types.CredentialFetchResult, error)
}

// DefaultCredentialClient implements CredentialClient.
type DefaultCredentialClient struct {
	resourceClient kubernetes.ResourceClient
	logger         logging.Logger
}

// NewCredentialClient creates a new DefaultCredentialClient.
func NewCredentialClient(resourceClient kubernetes.ResourceClient, logger logging.Logger) CredentialClient {
	return &DefaultCredentialClient{
		resourceClient: resourceClient,
		logger:         logger,
	}
}

// FetchCompositionCredentials extracts credential references from a composition's pipeline steps
// and fetches the referenced secrets from the cluster. This enables functions to receive
// credentials for authentication (e.g., Azure Workload Identity for function-msgraph).
//
// A referenced secret that does not exist is recorded in the result's Absent list and the fetch
// continues, because an absent secret is a condition the caller may legitimately have handled: it may
// be injected at runtime by the cluster (e.g. workload identity) or supplied via the CLI's
// --function-credentials flag.
//
// Every other failure is returned as an error. A Forbidden, a transport failure or an unconvertible
// payload means the secret may well exist and be relevant but could not be read, so rendering without
// it would produce a diff that silently does not reflect what the cluster would do. Per the project's
// accuracy-first stance that is a failure, not an advisory.
func (c *DefaultCredentialClient) FetchCompositionCredentials(ctx context.Context, comp *apiextensionsv1.Composition) (types.CredentialFetchResult, error) {
	if comp.Spec.Mode != apiextensionsv1.CompositionModePipeline {
		return types.CredentialFetchResult{}, nil
	}

	secretGVK := schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Secret",
	}

	var result types.CredentialFetchResult

	// A composition may reference one secret from several steps; report each absent secret once.
	absentSeen := map[k8stypes.NamespacedName]struct{}{}

	for _, step := range comp.Spec.Pipeline {
		for _, cred := range step.Credentials {
			if cred.Source != apiextensionsv1.FunctionCredentialsSourceSecret || cred.SecretRef == nil {
				continue
			}

			ref := k8stypes.NamespacedName{Namespace: cred.SecretRef.Namespace, Name: cred.SecretRef.Name}

			c.logger.Debug("Fetching function credential secret",
				"step", step.Step,
				"credentialName", cred.Name,
				"secretRef", ref.String())

			secretUnstructured, err := c.resourceClient.GetResource(ctx, secretGVK, ref.Namespace, ref.Name)

			switch {
			case apierrors.IsNotFound(err):
				c.logger.Debug("Function credential secret does not exist on cluster",
					"step", step.Step,
					"credentialName", cred.Name,
					"secretRef", ref.String())

				if _, dup := absentSeen[ref]; !dup {
					absentSeen[ref] = struct{}{}
					result.Absent = append(result.Absent, ref)
				}

				continue
			case err != nil:
				return types.CredentialFetchResult{}, errors.Wrapf(err,
					"cannot read function credential secret %s referenced by pipeline step %q", ref, step.Step)
			}

			secret := corev1.Secret{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(secretUnstructured.Object, &secret); err != nil {
				return types.CredentialFetchResult{}, errors.Wrapf(err,
					"cannot decode function credential secret %s referenced by pipeline step %q", ref, step.Step)
			}

			result.Secrets = append(result.Secrets, secret)
		}
	}

	c.logger.Debug("Fetched function credential secrets from cluster",
		"composition", comp.GetName(),
		"fetched", len(result.Secrets),
		"absent", len(result.Absent))

	return result, nil
}
