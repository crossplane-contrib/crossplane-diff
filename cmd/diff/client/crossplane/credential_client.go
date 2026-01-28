package crossplane

import (
	"context"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
)

// CredentialClient handles fetching credentials referenced by composition pipelines.
type CredentialClient interface {
	// FetchCompositionCredentials extracts credential refs from a composition's pipeline steps
	// and fetches the referenced secrets from the cluster. Secrets that cannot be fetched
	// are silently skipped (they may be injected at runtime, e.g., workload identity).
	FetchCompositionCredentials(ctx context.Context, comp *apiextensionsv1.Composition) []corev1.Secret
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
// Secrets that cannot be fetched are silently skipped since they may be:
// - Injected at runtime by the cluster (e.g., workload identity)
// - Provided via CLI --function-credentials flag.
func (c *DefaultCredentialClient) FetchCompositionCredentials(ctx context.Context, comp *apiextensionsv1.Composition) []corev1.Secret {
	if comp.Spec.Mode != apiextensionsv1.CompositionModePipeline {
		return nil
	}

	secretGVK := schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Secret",
	}

	var secrets []corev1.Secret

	// Track unique credential references we attempted to fetch (for logging)
	var attempted, fetched int

	for _, step := range comp.Spec.Pipeline {
		for _, cred := range step.Credentials {
			if cred.Source != apiextensionsv1.FunctionCredentialsSourceSecret || cred.SecretRef == nil {
				continue
			}

			attempted++

			c.logger.Debug("Fetching function credential secret",
				"step", step.Step,
				"credentialName", cred.Name,
				"secretRef", cred.SecretRef.Namespace+"/"+cred.SecretRef.Name)

			secretUnstructured, err := c.resourceClient.GetResource(ctx, secretGVK, cred.SecretRef.Namespace, cred.SecretRef.Name)
			if err != nil {
				c.logger.Debug("Could not fetch function credential secret, skipping",
					"step", step.Step,
					"credentialName", cred.Name,
					"secretRef", cred.SecretRef.Namespace+"/"+cred.SecretRef.Name,
					"error", err)

				continue
			}

			secret := corev1.Secret{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(secretUnstructured.Object, &secret); err != nil {
				c.logger.Debug("Could not convert secret to corev1.Secret, skipping",
					"step", step.Step,
					"credentialName", cred.Name,
					"error", err)

				continue
			}

			secrets = append(secrets, secret)
			fetched++
		}
	}

	// Log summary based on what happened:
	// - No credentials referenced: no log (nothing to fetch)
	// - All credentials fetched successfully: debug log
	// - Some credentials couldn't be fetched: info log (user should know)
	if attempted > 0 {
		if fetched == attempted {
			c.logger.Debug("Fetched all function credential secrets from cluster",
				"composition", comp.GetName(),
				"count", fetched)
		} else {
			c.logger.Info("Some function credential secrets could not be fetched from cluster",
				"composition", comp.GetName(),
				"attempted", attempted,
				"fetched", fetched,
				"hint", "Use --function-credentials to provide secrets that don't exist on cluster")
		}
	}

	return secrets
}
