package diffprocessor

import (
	"context"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	"github.com/google/go-cmp/cmp"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

var _ Defaulter = (*tu.MockDefaulter)(nil)

func TestCRDDefaulter_Default(t *testing.T) {
	// An XR, whose CRD is resolved through its XRD, by name.
	xrCRD := tu.NewCRD("xsizeds.example.org", "example.org", "XSized").
		WithPlural("xsizeds").
		WithVersion("v1", true, true).
		WithDefaultedStringFieldSchema("size", "small").
		Build()
	xrd := tu.NewXRD("xsizeds.example.org", "example.org", "XSized").
		WithPlural("xsizeds").
		WithVersion("v1", true, true).
		BuildAsUnstructured()
	xrGVK := schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "XSized"}

	// A composed resource, which is not an XR, so its CRD is resolved by GVK.
	mrCRD := tu.NewCRD("sizeds.example.org", "example.org", "Sized").
		WithPlural("sizeds").
		WithVersion("v1", true, true).
		WithDefaultedStringFieldSchema("size", "small").
		Build()
	mrGVK := schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Sized"}

	xrdsFor := func(gvk schema.GroupVersionKind) *tu.MockDefinitionClient {
		return tu.NewMockDefinitionClient().WithXRDForGVK(gvk, xrd).Build()
	}
	noXRDs := func() *tu.MockDefinitionClient {
		return tu.NewMockDefinitionClient().WithXRDForXRNotFound().Build()
	}

	// Each schema client only answers the lookup the case expects to be used, so a case also pins
	// which lookup resolved the CRD.
	xrCRDByName := func() *tu.MockSchemaClient {
		return tu.NewMockSchemaClient().
			WithSuccessfulCRDByNameFetch("xsizeds.example.org", xrCRD).
			WithGetCRD(func(context.Context, schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
				return nil, errors.New("an XR's CRD must be resolved through its XRD, not by GVK")
			}).
			Build()
	}
	mrCRDByGVK := func() *tu.MockSchemaClient {
		return tu.NewMockSchemaClient().
			WithFoundCRD("example.org", "Sized", mrCRD).
			WithResourcesRequiringCRDs(mrGVK).
			Build()
	}
	unknownCRD := func() *tu.MockSchemaClient {
		return tu.NewMockSchemaClient().WithAllResourcesRequiringCRDs().WithCRDNotFound().Build()
	}
	builtIn := func() *tu.MockSchemaClient {
		return tu.NewMockSchemaClient().
			WithNoResourcesRequiringCRDs().
			WithGetCRD(func(context.Context, schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
				return nil, errors.New("GetCRD must not be called for a built-in type")
			}).
			Build()
	}

	// Structural defaulting fills in fields of objects that exist, so a resource needs a spec for
	// spec.size to be defaulted into.
	unsizedXR := tu.NewResource("example.org/v1", "XSized", "xr").WithNestedField(map[string]any{}, "spec").Build()
	unsizedMR := tu.NewResource("example.org/v1", "Sized", "mr").WithNestedField(map[string]any{}, "spec").Build()
	configMap := tu.NewResource("v1", "ConfigMap", "cm").WithNestedField(map[string]any{"k": "v"}, "data").Build()

	type want struct {
		out *un.Unstructured
		err bool
	}

	tests := map[string]struct {
		reason       string
		policy       DefaultingPolicy
		schemaClient *tu.MockSchemaClient
		defClient    *tu.MockDefinitionClient
		in           *un.Unstructured
		want         want
	}{
		"StrictAppliesAnXRsDefaultsFromItsXRDsCRD": {
			reason:       "An XR is defaulted from the CRD its XRD names, which is what render needs to see.",
			policy:       StrictDefaulting,
			schemaClient: xrCRDByName(),
			defClient:    xrdsFor(xrGVK),
			in:           unsizedXR,
			want:         want{out: tu.NewResource("example.org/v1", "XSized", "xr").WithSpecField("size", "small").Build()},
		},
		"LenientAppliesAComposedResourcesDefaultsFromItsCRD": {
			reason:       "A resource that is not an XR is defaulted from the CRD found for its GVK.",
			policy:       LenientDefaulting,
			schemaClient: mrCRDByGVK(),
			defClient:    noXRDs(),
			in:           unsizedMR,
			want:         want{out: tu.NewResource("example.org/v1", "Sized", "mr").WithSpecField("size", "small").Build()},
		},
		"KeepsAValueTheResourceSets": {
			reason:       "A default never overrides a value the resource already carries.",
			policy:       StrictDefaulting,
			schemaClient: xrCRDByName(),
			defClient:    xrdsFor(xrGVK),
			in:           tu.NewResource("example.org/v1", "XSized", "xr").WithSpecField("size", "large").Build(),
			want:         want{out: tu.NewResource("example.org/v1", "XSized", "xr").WithSpecField("size", "large").Build()},
		},
		"StrictFailsWhenTheXRDsCRDIsMissing": {
			reason: "Rendering an XR without the defaults Crossplane would see could produce a wrong diff, so a missing CRD is an error.",
			policy: StrictDefaulting,
			schemaClient: tu.NewMockSchemaClient().
				WithGetCRDByName(func(name string) (*extv1.CustomResourceDefinition, error) {
					return nil, errors.Errorf("CRD with name %s not found in cache", name)
				}).
				Build(),
			defClient: xrdsFor(xrGVK),
			in:        unsizedXR,
			want:      want{err: true},
		},
		"LenientPassesThroughWhenTheXRDsCRDIsMissing": {
			reason: "A best-effort prediction leaves a resource it has no CRD for as it is.",
			policy: LenientDefaulting,
			schemaClient: tu.NewMockSchemaClient().
				WithGetCRDByName(func(name string) (*extv1.CustomResourceDefinition, error) {
					return nil, errors.Errorf("CRD with name %s not found in cache", name)
				}).
				Build(),
			defClient: xrdsFor(xrGVK),
			in:        unsizedXR,
			want:      want{out: unsizedXR},
		},
		"StrictFailsForAnUnknownCRD": {
			reason:       "With no XRD and no CRD for the GVK there is nothing to default from, which strict defaulting must not hide.",
			policy:       StrictDefaulting,
			schemaClient: unknownCRD(),
			defClient:    noXRDs(),
			in:           unsizedMR,
			want:         want{err: true},
		},
		"LenientPassesThroughAnUnknownCRD": {
			reason:       "The schema validator, not the Defaulter, is the gate on missing CRDs.",
			policy:       LenientDefaulting,
			schemaClient: unknownCRD(),
			defClient:    noXRDs(),
			in:           unsizedMR,
			want:         want{out: unsizedMR},
		},
		"StrictFailsForABuiltInType": {
			reason:       "A built-in type has no CRD, so it can never be a valid render input to default.",
			policy:       StrictDefaulting,
			schemaClient: builtIn(),
			defClient:    noXRDs(),
			in:           configMap,
			want:         want{err: true},
		},
		"LenientPassesThroughABuiltInType": {
			reason:       "A built-in type has no CRD to default from, so it comes back unchanged without a CRD lookup.",
			policy:       LenientDefaulting,
			schemaClient: builtIn(),
			defClient:    noXRDs(),
			in:           configMap,
			want:         want{out: configMap},
		},
		"LenientFailsForAnAPIVersionTheCRDDoesNotDefine": {
			reason: "A CRD that does not define the resource's version cannot predict anything; returning the input as though defaulted would hide that.",
			policy: LenientDefaulting,
			schemaClient: tu.NewMockSchemaClient().
				WithFoundCRD("example.org", "Sized", mrCRD).
				WithAllResourcesRequiringCRDs().
				Build(),
			defClient: noXRDs(),
			in:        tu.NewResource("example.org/v2", "Sized", "mr").WithNestedField(map[string]any{}, "spec").Build(),
			want:      want{err: true},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			before := tt.in.DeepCopy()

			out, err := NewDefaulter(tt.schemaClient, tt.defClient, tt.policy).Default(t.Context(), tt.in)

			if d := cmp.Diff(before, tt.in); d != "" {
				t.Errorf("%s\nDefault() mutated its input (-before +after):\n%s", tt.reason, d)
			}

			if (err != nil) != tt.want.err {
				t.Fatalf("%s\nDefault() error = %v, want error: %v", tt.reason, err, tt.want.err)
			}

			if d := cmp.Diff(tt.want.out, out); d != "" {
				t.Errorf("%s\nDefault() (-want +got):\n%s", tt.reason, d)
			}

			if out != nil && out == tt.in {
				t.Errorf("%s\nDefault() returned its input rather than a copy", tt.reason)
			}
		})
	}
}
