package crossplane

import (
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
)

var _ CredentialClient = (*tu.MockCredentialClient)(nil)

func TestDefaultCredentialClient_FetchCompositionCredentials(t *testing.T) {
	ctx := t.Context()

	// Define common secrets used across multiple tests
	secret1Builder := tu.NewResource("v1", "Secret", "my-secret").
		InNamespace("crossplane-system").
		WithData(map[string][]byte{"token": []byte("secret-value")})
	var secret1 corev1.Secret
	secret1Builder.BuildTyped(&secret1)

	secret2Builder := tu.NewResource("v1", "Secret", "secret1").
		InNamespace("ns1").
		WithData(map[string][]byte{"key1": []byte("val1")})
	var secret2 corev1.Secret
	secret2Builder.BuildTyped(&secret2)

	secret3Builder := tu.NewResource("v1", "Secret", "secret2").
		InNamespace("ns2").
		WithData(map[string][]byte{"key2": []byte("val2")})
	var secret3 corev1.Secret
	secret3Builder.BuildTyped(&secret3)

	secret4Builder := tu.NewResource("v1", "Secret", "exists").
		InNamespace("ns1").
		WithData(map[string][]byte{"data": []byte("value")})
	var secret4 corev1.Secret
	secret4Builder.BuildTyped(&secret4)

	secret5Builder := tu.NewResource("v1", "Secret", "secret1").
		InNamespace("ns").
		WithData(map[string][]byte{"k1": []byte("v1")})
	var secret5 corev1.Secret
	secret5Builder.BuildTyped(&secret5)

	secret6Builder := tu.NewResource("v1", "Secret", "secret2").
		InNamespace("ns").
		WithData(map[string][]byte{"k2": []byte("v2")})
	var secret6 corev1.Secret
	secret6Builder.BuildTyped(&secret6)

	tests := map[string]struct {
		reason       string
		composition  *apiextensionsv1.Composition
		mockResource tu.MockResourceClient
		wantSecrets  []corev1.Secret
	}{
		"NonPipelineMode": {
			reason: "Should return nil for non-pipeline compositions",
			composition: tu.NewComposition("test-comp").
				WithCompositeTypeRef("example.org/v1", "XR").
				Build(), // Default is not pipeline mode
			mockResource: *tu.NewMockResourceClient().Build(),
			wantSecrets:  nil,
		},
		"PipelineModeNoCredentials": {
			reason: "Should return nil when pipeline has no credential refs",
			composition: tu.NewComposition("test-comp").
				WithCompositeTypeRef("example.org/v1", "XR").
				WithPipelineMode().
				WithPipelineStep("step1", "function-test", nil).
				Build(),
			mockResource: *tu.NewMockResourceClient().Build(),
			wantSecrets:  nil,
		},
		"SingleCredentialFetched": {
			reason: "Should fetch single credential secret from cluster",
			composition: tu.NewComposition("test-comp").
				WithCompositeTypeRef("example.org/v1", "XR").
				WithPipelineMode().
				WithPipelineStep("step1", "function-test", nil,
					tu.WithCredentials("creds", "crossplane-system", "my-secret")).
				Build(),
			mockResource: *tu.NewMockResourceClient().
				WithResourcesExist(secret1Builder.Build()).
				Build(),
			wantSecrets: []corev1.Secret{secret1},
		},
		"MultipleCredentialsFetched": {
			reason: "Should fetch multiple credential secrets from different steps",
			composition: tu.NewComposition("test-comp").
				WithCompositeTypeRef("example.org/v1", "XR").
				WithPipelineMode().
				WithPipelineStep("step1", "function-one", nil,
					tu.WithCredentials("creds1", "ns1", "secret1")).
				WithPipelineStep("step2", "function-two", nil,
					tu.WithCredentials("creds2", "ns2", "secret2")).
				Build(),
			mockResource: *tu.NewMockResourceClient().
				WithResourcesExist(secret2Builder.Build(), secret3Builder.Build()).
				Build(),
			wantSecrets: []corev1.Secret{secret2, secret3},
		},
		"CredentialNotFoundSkipped": {
			reason: "Should skip credentials that cannot be fetched (e.g., runtime-injected)",
			composition: tu.NewComposition("test-comp").
				WithCompositeTypeRef("example.org/v1", "XR").
				WithPipelineMode().
				WithPipelineStep("step1", "function-test", nil,
					tu.WithCredentials("creds", "crossplane-system", "missing-secret")).
				Build(),
			mockResource: *tu.NewMockResourceClient().
				WithResourceNotFound().
				Build(),
			wantSecrets: nil,
		},
		"MixedFetchResults": {
			reason: "Should return only successfully fetched credentials",
			composition: tu.NewComposition("test-comp").
				WithCompositeTypeRef("example.org/v1", "XR").
				WithPipelineMode().
				WithPipelineStep("step1", "function-one", nil,
					tu.WithCredentials("creds1", "ns1", "exists")).
				WithPipelineStep("step2", "function-two", nil,
					tu.WithCredentials("creds2", "ns2", "missing")).
				Build(),
			// Only "exists" is in the map; "missing" will return error
			mockResource: *tu.NewMockResourceClient().
				WithResourcesExist(secret4Builder.Build()).
				Build(),
			wantSecrets: []corev1.Secret{secret4},
		},
		"MultipleCredentialsInSameStep": {
			reason: "Should fetch multiple credentials from the same pipeline step",
			composition: tu.NewComposition("test-comp").
				WithCompositeTypeRef("example.org/v1", "XR").
				WithPipelineMode().
				WithPipelineStep("step1", "function-test", nil,
					tu.WithCredentials("creds1", "ns", "secret1"),
					tu.WithCredentials("creds2", "ns", "secret2")).
				Build(),
			mockResource: *tu.NewMockResourceClient().
				WithResourcesExist(secret5Builder.Build(), secret6Builder.Build()).
				Build(),
			wantSecrets: []corev1.Secret{secret5, secret6},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := &DefaultCredentialClient{
				resourceClient: &tt.mockResource,
				logger:         tu.TestLogger(t, false),
			}

			got := c.FetchCompositionCredentials(ctx, tt.composition)

			// Compare counts first
			if len(got) != len(tt.wantSecrets) {
				t.Errorf("\n%s\nFetchCompositionCredentials(): got %d secrets, want %d",
					tt.reason, len(got), len(tt.wantSecrets))

				return
			}

			// Compare each secret
			for i, wantSecret := range tt.wantSecrets {
				gotSecret := got[i]

				if diff := cmp.Diff(wantSecret.Namespace, gotSecret.Namespace); diff != "" {
					t.Errorf("\n%s\nFetchCompositionCredentials() secret[%d] namespace mismatch (-want +got):\n%s",
						tt.reason, i, diff)
				}

				if diff := cmp.Diff(wantSecret.Name, gotSecret.Name); diff != "" {
					t.Errorf("\n%s\nFetchCompositionCredentials() secret[%d] name mismatch (-want +got):\n%s",
						tt.reason, i, diff)
				}

				if diff := cmp.Diff(wantSecret.Data, gotSecret.Data); diff != "" {
					t.Errorf("\n%s\nFetchCompositionCredentials() secret[%d] data mismatch (-want +got):\n%s",
						tt.reason, i, diff)
				}
			}
		})
	}
}
