package crossplane

import (
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
)

var _ CredentialClient = (*tu.MockCredentialClient)(nil)

func TestDefaultCredentialClient_FetchCompositionCredentials(t *testing.T) {
	ctx := t.Context()

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
				WithResourcesExist(
					tu.NewResource("v1", "Secret", "my-secret").
						InNamespace("crossplane-system").
						WithData(map[string][]byte{"token": []byte("secret-value")}).
						Build(),
				).
				Build(),
			wantSecrets: []corev1.Secret{
				{
					TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "crossplane-system",
						Name:      "my-secret",
					},
					Data: map[string][]byte{"token": []byte("secret-value")},
				},
			},
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
				WithResourcesExist(
					tu.NewResource("v1", "Secret", "secret1").InNamespace("ns1").WithData(map[string][]byte{"key1": []byte("val1")}).Build(),
					tu.NewResource("v1", "Secret", "secret2").InNamespace("ns2").WithData(map[string][]byte{"key2": []byte("val2")}).Build(),
				).
				Build(),
			wantSecrets: []corev1.Secret{
				{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "secret1"},
					Data:       map[string][]byte{"key1": []byte("val1")},
				},
				{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns2", Name: "secret2"},
					Data:       map[string][]byte{"key2": []byte("val2")},
				},
			},
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
				WithResourcesExist(
					tu.NewResource("v1", "Secret", "exists").InNamespace("ns1").WithData(map[string][]byte{"data": []byte("value")}).Build(),
				).
				Build(),
			wantSecrets: []corev1.Secret{
				{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "exists"},
					Data:       map[string][]byte{"data": []byte("value")},
				},
			},
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
				WithResourcesExist(
					tu.NewResource("v1", "Secret", "secret1").InNamespace("ns").WithData(map[string][]byte{"k1": []byte("v1")}).Build(),
					tu.NewResource("v1", "Secret", "secret2").InNamespace("ns").WithData(map[string][]byte{"k2": []byte("v2")}).Build(),
				).
				Build(),
			wantSecrets: []corev1.Secret{
				{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "secret1"},
					Data:       map[string][]byte{"k1": []byte("v1")},
				},
				{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "secret2"},
					Data:       map[string][]byte{"k2": []byte("v2")},
				},
			},
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
