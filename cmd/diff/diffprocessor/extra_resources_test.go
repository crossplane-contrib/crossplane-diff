package diffprocessor

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
)

func TestExtractGoTemplate(t *testing.T) {
	tests := map[string]struct {
		raw  []byte
		want string
	}{
		"GoTemplateWithInlineTemplate": {
			raw:  []byte(`{"apiVersion":"gotemplating.fn.crossplane.io/v1beta1","kind":"GoTemplate","inline":{"template":"hello world"}}`),
			want: "hello world",
		},
		"NonGoTemplateKind": {
			raw:  []byte(`{"apiVersion":"other.fn.crossplane.io/v1beta1","kind":"Other","inline":{"template":"hello"}}`),
			want: "",
		},
		"EmptyTemplate": {
			raw:  []byte(`{"kind":"GoTemplate","inline":{"template":""}}`),
			want: "",
		},
		"InvalidJSON": {
			raw:  []byte(`not json`),
			want: "",
		},
		"MissingInline": {
			raw:  []byte(`{"kind":"GoTemplate"}`),
			want: "",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := extractGoTemplate(tt.raw)
			if got != tt.want {
				t.Errorf("extractGoTemplate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractGVKsFromTemplate(t *testing.T) {
	tests := map[string]struct {
		tmpl string
		want []schema.GroupVersionKind
	}{
		"SingleExtraResourcesBlock": {
			tmpl: `---
apiVersion: meta.gotemplating.fn.crossplane.io/v1alpha1
kind: ExtraResources
requirements:
  eksRef:
    apiVersion: aws.example.com/v1alpha1
    kind: XMyEKS
    matchLabels:
      crossplane.io/claim-name: {{ $clusterId }}
---`,
			want: []schema.GroupVersionKind{
				{Group: "aws.example.com", Version: "v1alpha1", Kind: "XMyEKS"},
			},
		},
		"MultipleGVKsInOneBlock": {
			tmpl: `---
apiVersion: meta.gotemplating.fn.crossplane.io/v1alpha1
kind: ExtraResources
requirements:
  eksRef:
    apiVersion: aws.example.com/v1alpha1
    kind: XMyEKS
    matchLabels:
      crossplane.io/claim-name: test
  zones:
    apiVersion: route53.aws.upbound.io/v1beta1
    kind: Zone
    matchLabels:
      env: prod
---`,
			want: []schema.GroupVersionKind{
				{Group: "aws.example.com", Version: "v1alpha1", Kind: "XMyEKS"},
				{Group: "route53.aws.upbound.io", Version: "v1beta1", Kind: "Zone"},
			},
		},
		"CoreAPIGroupResource": {
			tmpl: `---
apiVersion: meta.gotemplating.fn.crossplane.io/v1alpha1
kind: ExtraResources
requirements:
  config:
    apiVersion: v1
    kind: ConfigMap
    matchLabels:
      app: test
---`,
			want: []schema.GroupVersionKind{
				{Group: "", Version: "v1", Kind: "ConfigMap"},
			},
		},
		"TemplateDirectivesStripped": {
			tmpl: `---
apiVersion: meta.gotemplating.fn.crossplane.io/v1alpha1
kind: ExtraResources
requirements:
  eksRef:
    apiVersion: aws.example.com/v1alpha1
    kind: XMyEKS
    matchLabels:
      crossplane.io/claim-name: {{ .observed.composite.resource.spec.claimRef.name }}
---`,
			want: []schema.GroupVersionKind{
				{Group: "aws.example.com", Version: "v1alpha1", Kind: "XMyEKS"},
			},
		},
		"NoExtraResourcesBlock": {
			tmpl: `---
apiVersion: example.org/v1
kind: SomeResource
metadata:
  name: test
---`,
			want: nil,
		},
		"EmptyTemplate": {
			tmpl: "",
			want: nil,
		},
		"ExtraResourcesBlockAtEndOfTemplate": {
			tmpl: `some yaml content
---
apiVersion: meta.gotemplating.fn.crossplane.io/v1alpha1
kind: ExtraResources
requirements:
  vpc:
    apiVersion: ec2.aws.upbound.io/v1beta1
    kind: VPCEndpoint
    matchLabels:
      type: gateway`,
			want: []schema.GroupVersionKind{
				{Group: "ec2.aws.upbound.io", Version: "v1beta1", Kind: "VPCEndpoint"},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := extractGVKsFromTemplate(tt.tmpl)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("extractGVKsFromTemplate() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestExtractExtraResourceGVKs(t *testing.T) {
	makeComposition := func(templates ...string) *apiextensionsv1.Composition {
		steps := make([]apiextensionsv1.PipelineStep, 0, len(templates))
		for i, tmpl := range templates {
			input := []byte(`{"kind":"GoTemplate","inline":{"template":` + mustJSON(tmpl) + `}}`)
			steps = append(steps, apiextensionsv1.PipelineStep{
				Step: "step-" + string(rune('a'+i)),
				Input: &runtime.RawExtension{
					Raw: input,
				},
			})
		}

		return &apiextensionsv1.Composition{
			Spec: apiextensionsv1.CompositionSpec{
				Pipeline: steps,
			},
		}
	}

	tests := map[string]struct {
		comp *apiextensionsv1.Composition
		want []schema.GroupVersionKind
	}{
		"SingleStepWithExtraResources": {
			comp: makeComposition(`---
apiVersion: meta.gotemplating.fn.crossplane.io/v1alpha1
kind: ExtraResources
requirements:
  ref:
    apiVersion: example.com/v1
    kind: MyResource
    matchLabels:
      app: test
---`),
			want: []schema.GroupVersionKind{
				{Group: "example.com", Version: "v1", Kind: "MyResource"},
			},
		},
		"MultipleStepsDeduplicateGVKs": {
			comp: makeComposition(
				`---
apiVersion: meta.gotemplating.fn.crossplane.io/v1alpha1
kind: ExtraResources
requirements:
  ref:
    apiVersion: example.com/v1
    kind: MyResource
    matchLabels:
      app: test
---`,
				`---
apiVersion: meta.gotemplating.fn.crossplane.io/v1alpha1
kind: ExtraResources
requirements:
  ref:
    apiVersion: example.com/v1
    kind: MyResource
    matchLabels:
      env: prod
---`,
			),
			want: []schema.GroupVersionKind{
				{Group: "example.com", Version: "v1", Kind: "MyResource"},
			},
		},
		"NoPipelineSteps": {
			comp: &apiextensionsv1.Composition{
				Spec: apiextensionsv1.CompositionSpec{},
			},
			want: nil,
		},
		"StepWithNilInput": {
			comp: &apiextensionsv1.Composition{
				Spec: apiextensionsv1.CompositionSpec{
					Pipeline: []apiextensionsv1.PipelineStep{
						{Step: "step-a", Input: nil},
					},
				},
			},
			want: nil,
		},
		"NonGoTemplateStep": {
			comp: &apiextensionsv1.Composition{
				Spec: apiextensionsv1.CompositionSpec{
					Pipeline: []apiextensionsv1.PipelineStep{
						{
							Step: "step-a",
							Input: &runtime.RawExtension{
								Raw: []byte(`{"kind":"Other","data":"value"}`),
							},
						},
					},
				},
			},
			want: nil,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := extractExtraResourceGVKs(tt.comp)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("extractExtraResourceGVKs() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseGVK(t *testing.T) {
	tests := map[string]struct {
		apiVersion string
		kind       string
		want       schema.GroupVersionKind
	}{
		"GroupedResource": {
			apiVersion: "aws.example.com/v1alpha1",
			kind:       "MyResource",
			want:       schema.GroupVersionKind{Group: "aws.example.com", Version: "v1alpha1", Kind: "MyResource"},
		},
		"CoreResource": {
			apiVersion: "v1",
			kind:       "ConfigMap",
			want:       schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		},
		"MultiPartGroup": {
			apiVersion: "route53.aws.upbound.io/v1beta1",
			kind:       "Zone",
			want:       schema.GroupVersionKind{Group: "route53.aws.upbound.io", Version: "v1beta1", Kind: "Zone"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := parseGVK(tt.apiVersion, tt.kind)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("parseGVK() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}

	return string(b)
}
