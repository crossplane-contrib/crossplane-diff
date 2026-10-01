package crossplane

import (
	"strings"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
)

var _ CredentialClient = (*tu.MockCredentialClient)(nil)

func TestDefaultCredentialClient_FetchCompositionCredentials(t *testing.T) {
	ctx := t.Context()

	// Define common secrets used across multiple tests
	// Two secrets in ns1 (for same-namespace tests), one in ns2 (for different-namespace tests)
	secret1Builder := tu.NewResource("v1", "Secret", "secret1").
		InNamespace("ns1").
		WithData(map[string][]byte{"key1": []byte("val1")})

	var secret1 corev1.Secret
	secret1Builder.BuildTyped(&secret1)

	secret2Builder := tu.NewResource("v1", "Secret", "secret2").
		InNamespace("ns1").
		WithData(map[string][]byte{"key2": []byte("val2")})

	var secret2 corev1.Secret
	secret2Builder.BuildTyped(&secret2)

	secret3Builder := tu.NewResource("v1", "Secret", "secret3").
		InNamespace("ns2").
		WithData(map[string][]byte{"key3": []byte("val3")})

	var secret3 corev1.Secret
	secret3Builder.BuildTyped(&secret3)

	tests := map[string]struct {
		reason       string
		composition  *apiextensionsv1.Composition
		mockResource tu.MockResourceClient
		wantSecrets  []corev1.Secret
		// wantAbsent is the set of referenced secrets reported as absent from the cluster. The client
		// reports the fact rather than warning about it: whether an absent secret is a problem depends on
		// whether the caller supplied it via --function-credentials, which only the caller can see.
		wantAbsent []k8stypes.NamespacedName
		// wantErr is a substring of the expected error. A failure that is NOT a NotFound means the
		// credential may exist and be relevant but could not be read; rendering without it would emit a
		// diff that silently does not reflect reality, so the fetch fails rather than degrading.
		wantErr string
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
					tu.WithCredentials("creds", "ns1", "secret1")).
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
					tu.WithCredentials("creds2", "ns2", "secret3")).
				Build(),
			mockResource: *tu.NewMockResourceClient().
				WithResourcesExist(secret1Builder.Build(), secret3Builder.Build()).
				Build(),
			wantSecrets: []corev1.Secret{secret1, secret3},
		},
		"CredentialNotFoundReported": {
			reason: "Should report, not fail on, credentials absent from the cluster (e.g., runtime-injected)",
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
			wantAbsent:  []k8stypes.NamespacedName{{Namespace: "crossplane-system", Name: "missing-secret"}},
		},
		"MixedFetchResults": {
			reason: "Should return successfully fetched credentials and report the absent one",
			composition: tu.NewComposition("test-comp").
				WithCompositeTypeRef("example.org/v1", "XR").
				WithPipelineMode().
				WithPipelineStep("step1", "function-one", nil,
					tu.WithCredentials("creds1", "ns1", "secret1")).
				WithPipelineStep("step2", "function-two", nil,
					tu.WithCredentials("creds2", "ns2", "missing")).
				Build(),
			// Only "secret1" is in the map; "missing" yields NotFound
			mockResource: *tu.NewMockResourceClient().
				WithResourcesExist(secret1Builder.Build()).
				Build(),
			wantSecrets: []corev1.Secret{secret1},
			wantAbsent:  []k8stypes.NamespacedName{{Namespace: "ns2", Name: "missing"}},
		},
		"SameAbsentSecretReferencedTwiceReportedOnce": {
			reason: "One absent secret referenced from two steps is one shortfall, not two",
			composition: tu.NewComposition("test-comp").
				WithCompositeTypeRef("example.org/v1", "XR").
				WithPipelineMode().
				WithPipelineStep("step1", "function-one", nil,
					tu.WithCredentials("creds1", "ns1", "shared-missing")).
				WithPipelineStep("step2", "function-two", nil,
					tu.WithCredentials("creds2", "ns1", "shared-missing")).
				Build(),
			mockResource: *tu.NewMockResourceClient().
				WithResourceNotFound().
				Build(),
			wantSecrets: nil,
			wantAbsent:  []k8stypes.NamespacedName{{Namespace: "ns1", Name: "shared-missing"}},
		},
		"ForbiddenFails": {
			reason: "RBAC denial leaves the credential state unknown, so the fetch must fail rather than render without it",
			composition: tu.NewComposition("test-comp").
				WithCompositeTypeRef("example.org/v1", "XR").
				WithPipelineMode().
				WithPipelineStep("step1", "function-test", nil,
					tu.WithCredentials("creds", "crossplane-system", "forbidden-secret")).
				Build(),
			mockResource: *tu.NewMockResourceClient().
				WithGetResourceError(apierrors.NewForbidden(
					schema.GroupResource{Resource: "secrets"}, "forbidden-secret", errors.New("nope"))).
				Build(),
			wantErr: `cannot read function credential secret crossplane-system/forbidden-secret referenced by pipeline step "step1"`,
		},
		"TransportFailureFails": {
			reason: "A transport error is equally unknowable; degrading to a warning would emit a possibly-wrong diff",
			composition: tu.NewComposition("test-comp").
				WithCompositeTypeRef("example.org/v1", "XR").
				WithPipelineMode().
				WithPipelineStep("step1", "function-test", nil,
					tu.WithCredentials("creds", "ns1", "secret1")).
				Build(),
			mockResource: *tu.NewMockResourceClient().
				WithGetResourceError(errors.New("connection refused")).
				Build(),
			wantErr: "connection refused",
		},
		"MultipleCredentialsInSameStep": {
			reason: "Should fetch multiple credentials from the same pipeline step",
			composition: tu.NewComposition("test-comp").
				WithCompositeTypeRef("example.org/v1", "XR").
				WithPipelineMode().
				WithPipelineStep("step1", "function-test", nil,
					tu.WithCredentials("creds1", "ns1", "secret1"),
					tu.WithCredentials("creds2", "ns1", "secret2")).
				Build(),
			mockResource: *tu.NewMockResourceClient().
				WithResourcesExist(secret1Builder.Build(), secret2Builder.Build()).
				Build(),
			wantSecrets: []corev1.Secret{secret1, secret2},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logger := tu.NewAdvisoryCapturingLogger(t)

			c := &DefaultCredentialClient{
				resourceClient: &tt.mockResource,
				logger:         logger,
			}

			result, err := c.FetchCompositionCredentials(ctx, tt.composition)

			switch {
			case tt.wantErr != "":
				if err == nil {
					t.Fatalf("\n%s\nFetchCompositionCredentials(): expected an error containing %q, got nil",
						tt.reason, tt.wantErr)
				}

				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("\n%s\nFetchCompositionCredentials(): error %q does not contain %q",
						tt.reason, err.Error(), tt.wantErr)
				}

				return
			case err != nil:
				t.Fatalf("\n%s\nFetchCompositionCredentials(): unexpected error: %v", tt.reason, err)
			}

			// The client reports shortfalls as data and raises no advisory of its own: the advisory lives
			// in resolveFunctionCredentials, which is the only layer that knows what --function-credentials
			// already supplied. Warning here would fire at users who had already solved the problem.
			if advisories := logger.Advisories(); len(advisories) != 0 {
				t.Errorf("\n%s\nexpected no advisory from the credential client, got %v", tt.reason, advisories)
			}

			if diff := cmp.Diff(tt.wantAbsent, result.Absent); diff != "" {
				t.Errorf("\n%s\nFetchCompositionCredentials() absent mismatch (-want +got):\n%s", tt.reason, diff)
			}

			got := result.Secrets

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
