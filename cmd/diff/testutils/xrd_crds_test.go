package testutils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// The XRD CRD's file in PinnedCrossplaneCRDsDir, the same file the integration suite installs.
const xrdCRDFile = "apiextensions.crossplane.io_compositeresourcedefinitions.yaml"

// policyField is where a generated CRD declares one of Crossplane's policy fields, and the default it
// declares there. A zero value means the CRD does not declare the field at all.
type policyField struct {
	Path    string
	Default string
}

// crdShape is the part of a generated CRD these tests pin down: identity, scope, and the two policy
// fields whose defaults differ between an XR's CRD and its claim's.
type crdShape struct {
	Name                    string
	Scope                   extv1.ResourceScope
	CompositionUpdatePolicy policyField
	CompositeDeletePolicy   policyField
}

func TestCRDsForXRD(t *testing.T) {
	xrdCRD := loadXRDCRD(t)

	tests := map[string]struct {
		reason  string
		xrd     string
		want    []crdShape
		wantErr bool
	}{
		"V1XRDWithClaimNames": {
			reason: "A v1 XRD yields a legacy XR CRD that defaults compositionUpdatePolicy and has no compositeDeletePolicy, " +
				"and a claim CRD that does not default compositionUpdatePolicy but defaults compositeDeletePolicy, " +
				"both from the defaults the apiserver applies to the XRD itself",
			xrd: `
apiVersion: apiextensions.crossplane.io/v1
kind: CompositeResourceDefinition
metadata:
  name: xthings.example.org
spec:
  group: example.org
  names:
    kind: XThing
    plural: xthings
  claimNames:
    kind: Thing
    plural: things
  versions:
  - name: v1alpha1
    served: true
    referenceable: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              field:
                type: string
`,
			want: []crdShape{
				{
					Name:                    "xthings.example.org",
					Scope:                   extv1.ClusterScoped,
					CompositionUpdatePolicy: policyField{Path: "spec.compositionUpdatePolicy", Default: `"Automatic"`},
				},
				{
					Name:                    "things.example.org",
					Scope:                   extv1.NamespaceScoped,
					CompositionUpdatePolicy: policyField{Path: "spec.compositionUpdatePolicy"},
					CompositeDeletePolicy:   policyField{Path: "spec.compositeDeletePolicy", Default: `"Background"`},
				},
			},
		},
		"V1XRDWithExplicitPolicies": {
			reason: "Policies the XRD sets explicitly become the generated CRDs' defaults",
			xrd: `
apiVersion: apiextensions.crossplane.io/v1
kind: CompositeResourceDefinition
metadata:
  name: xthings.example.org
spec:
  group: example.org
  names:
    kind: XThing
    plural: xthings
  claimNames:
    kind: Thing
    plural: things
  defaultCompositionUpdatePolicy: Manual
  defaultCompositeDeletePolicy: Foreground
  versions:
  - name: v1alpha1
    served: true
    referenceable: true
    schema:
      openAPIV3Schema:
        type: object
`,
			want: []crdShape{
				{
					Name:                    "xthings.example.org",
					Scope:                   extv1.ClusterScoped,
					CompositionUpdatePolicy: policyField{Path: "spec.compositionUpdatePolicy", Default: `"Manual"`},
				},
				{
					Name:                    "things.example.org",
					Scope:                   extv1.NamespaceScoped,
					CompositionUpdatePolicy: policyField{Path: "spec.compositionUpdatePolicy"},
					CompositeDeletePolicy:   policyField{Path: "spec.compositeDeletePolicy", Default: `"Foreground"`},
				},
			},
		},
		"V2XRDWithoutScope": {
			reason: "A v2 XRD that omits scope is Namespaced (its own version's default, not v1's LegacyCluster), " +
				"nests the machinery under spec.crossplane, defaults compositionUpdatePolicy, and has no claim CRD",
			xrd: `
apiVersion: apiextensions.crossplane.io/v2
kind: CompositeResourceDefinition
metadata:
  name: xthings.example.org
spec:
  group: example.org
  names:
    kind: XThing
    plural: xthings
  versions:
  - name: v1alpha1
    served: true
    referenceable: true
    schema:
      openAPIV3Schema:
        type: object
`,
			want: []crdShape{
				{
					Name:                    "xthings.example.org",
					Scope:                   extv1.NamespaceScoped,
					CompositionUpdatePolicy: policyField{Path: "spec.crossplane.compositionUpdatePolicy", Default: `"Automatic"`},
				},
			},
		},
		"V2ClusterXRD": {
			reason: "A v2 Cluster XRD yields a cluster-scoped XR CRD that still nests the machinery under spec.crossplane",
			xrd: `
apiVersion: apiextensions.crossplane.io/v2
kind: CompositeResourceDefinition
metadata:
  name: xthings.example.org
spec:
  group: example.org
  scope: Cluster
  names:
    kind: XThing
    plural: xthings
  versions:
  - name: v1alpha1
    served: true
    referenceable: true
    schema:
      openAPIV3Schema:
        type: object
`,
			want: []crdShape{
				{
					Name:                    "xthings.example.org",
					Scope:                   extv1.ClusterScoped,
					CompositionUpdatePolicy: policyField{Path: "spec.crossplane.compositionUpdatePolicy", Default: `"Automatic"`},
				},
			},
		},
		"NotAnXRD": {
			reason: "Anything but an XRD is a caller error, never silently ignored",
			xrd: `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: xthings.example.org
`,
			wantErr: true,
		},
		"UnknownXRDVersion": {
			reason: "An XRD at a version the XRD CRD does not serve cannot be defaulted, so it is an error",
			xrd: `
apiVersion: apiextensions.crossplane.io/v9
kind: CompositeResourceDefinition
metadata:
  name: xthings.example.org
spec:
  group: example.org
  names:
    kind: XThing
    plural: xthings
  versions: []
`,
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			xrd := &un.Unstructured{}
			if err := yaml.Unmarshal([]byte(tt.xrd), &xrd.Object); err != nil {
				t.Fatalf("cannot parse test XRD: %v", err)
			}

			original := xrd.DeepCopy()

			crds, err := CRDsForXRD(xrd, xrdCRD)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("\n%s\nCRDsForXRD(...): want error, got nil", tt.reason)
				}

				return
			}

			if err != nil {
				t.Fatalf("\n%s\nCRDsForXRD(...): unexpected error: %v", tt.reason, err)
			}

			got := make([]crdShape, 0, len(crds))
			for _, crd := range crds {
				got = append(got, shapeOf(t, crd))
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("\n%s\nCRDsForXRD(...): -want, +got:\n%s", tt.reason, diff)
			}

			for _, crd := range crds {
				if refs := crd.GetOwnerReferences(); len(refs) != 0 {
					t.Errorf("\n%s\nCRDsForXRD(...): CRD %q has owner references %v; the XRD has no UID yet, so "+
						"an apiserver would reject them", tt.reason, crd.GetName(), refs)
				}
			}

			if diff := cmp.Diff(original.Object, xrd.Object); diff != "" {
				t.Errorf("\n%s\nCRDsForXRD(...) mutated its input: -before, +after:\n%s", tt.reason, diff)
			}
		})
	}
}

func loadXRDCRD(t *testing.T) *extv1.CustomResourceDefinition {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(PinnedCrossplaneCRDsDir(t), xrdCRDFile))
	if err != nil {
		t.Fatalf("cannot read the XRD CRD: %v", err)
	}

	crd := &extv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, crd); err != nil {
		t.Fatalf("cannot parse the XRD CRD: %v", err)
	}

	return crd
}

func shapeOf(t *testing.T, crd *extv1.CustomResourceDefinition) crdShape {
	t.Helper()

	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("CRD %q: want exactly one version, got %d", crd.GetName(), len(crd.Spec.Versions))
	}

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]

	return crdShape{
		Name:                    crd.GetName(),
		Scope:                   crd.Spec.Scope,
		CompositionUpdatePolicy: findPolicyField(spec, "compositionUpdatePolicy"),
		CompositeDeletePolicy:   findPolicyField(spec, "compositeDeletePolicy"),
	}
}

// findPolicyField looks for a policy field where Crossplane puts it: directly under spec for a legacy XR
// or a claim, or under spec.crossplane for a modern XR.
func findPolicyField(spec extv1.JSONSchemaProps, name string) policyField {
	for _, parent := range []struct {
		path  string
		props extv1.JSONSchemaProps
	}{
		{path: "spec", props: spec},
		{path: "spec.crossplane", props: spec.Properties["crossplane"]},
	} {
		field, ok := parent.props.Properties[name]
		if !ok {
			continue
		}

		f := policyField{Path: parent.path + "." + name}
		if field.Default != nil {
			f.Default = string(field.Default.Raw)
		}

		return f
	}

	return policyField{}
}
