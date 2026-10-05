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

var _ SchemaDefaulter = (*tu.MockSchemaDefaulter)(nil)

func TestDefaultSchemaDefaulter_Default(t *testing.T) {
	sizedCRD := tu.NewCRD("sizeds.example.org", "example.org", "Sized").
		WithPlural("sizeds").
		WithVersion("v1", true, true).
		WithDefaultedStringFieldSchema("size", "small").
		Build()

	sizedGVK := schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Sized"}

	withCRD := func() *tu.MockSchemaClient {
		return tu.NewMockSchemaClient().
			WithFoundCRD("example.org", "Sized", sizedCRD).
			WithResourcesRequiringCRDs(sizedGVK).
			Build()
	}

	// Structural defaulting fills in fields of objects that exist, so the resource needs a spec for
	// spec.size to be defaulted into.
	unsized := tu.NewResource("example.org/v1", "Sized", "a").WithNestedField(map[string]any{}, "spec").Build()

	type want struct {
		out *un.Unstructured
		err bool
	}

	tests := map[string]struct {
		reason       string
		schemaClient *tu.MockSchemaClient
		in           *un.Unstructured
		want         want
	}{
		"AppliesCRDDefaults": {
			reason:       "A field the CRD defaults is filled in when the resource omits it.",
			schemaClient: withCRD(),
			in:           unsized,
			want: want{
				out: tu.NewResource("example.org/v1", "Sized", "a").WithSpecField("size", "small").Build(),
			},
		},
		"KeepsAValueTheResourceSets": {
			reason:       "A default never overrides a value the resource already carries.",
			schemaClient: withCRD(),
			in:           tu.NewResource("example.org/v1", "Sized", "a").WithSpecField("size", "large").Build(),
			want: want{
				out: tu.NewResource("example.org/v1", "Sized", "a").WithSpecField("size", "large").Build(),
			},
		},
		"SkipsBuiltInTypes": {
			reason: "A built-in type has no CRD to default from, so it comes back unchanged without a CRD lookup.",
			schemaClient: tu.NewMockSchemaClient().
				WithNoResourcesRequiringCRDs().
				WithGetCRD(func(context.Context, schema.GroupVersionKind) (*extv1.CustomResourceDefinition, error) {
					return nil, errors.New("GetCRD must not be called for a built-in type")
				}).
				Build(),
			in: tu.NewResource("v1", "ConfigMap", "cm").WithNestedField(map[string]any{"k": "v"}, "data").Build(),
			want: want{
				out: tu.NewResource("v1", "ConfigMap", "cm").WithNestedField(map[string]any{"k": "v"}, "data").Build(),
			},
		},
		"SkipsUnknownCRDs": {
			reason: "A resource whose CRD cannot be found comes back unchanged; the schema validator, not the defaulter, is the gate on missing CRDs.",
			schemaClient: tu.NewMockSchemaClient().
				WithAllResourcesRequiringCRDs().
				WithCRDNotFound().
				Build(),
			in:   unsized,
			want: want{out: unsized},
		},
		"FailsForAnAPIVersionTheCRDDoesNotDefine": {
			reason: "Defaulting against a version the CRD does not define cannot predict anything, so it fails rather than return the input as though defaulted.",
			schemaClient: tu.NewMockSchemaClient().
				WithFoundCRD("example.org", "Sized", sizedCRD).
				WithAllResourcesRequiringCRDs().
				Build(),
			in:   tu.NewResource("example.org/v2", "Sized", "a").WithNestedField(map[string]any{}, "spec").Build(),
			want: want{err: true},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			before := tt.in.DeepCopy()

			out, err := NewSchemaDefaulter(tt.schemaClient).Default(t.Context(), tt.in)

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

func TestWithoutLocalDefaults(t *testing.T) {
	// original is what the user wrote; defaulted is original after local defaulting. obj is what was
	// built from defaulted afterwards, which may carry more than either.
	tests := map[string]struct {
		reason               string
		obj, defaulted, orig map[string]any
		want                 map[string]any
	}{
		"RemovesAFieldOnlyDefaultingAdded": {
			reason:    "A field absent from the original and holding its defaulted value came from local defaulting alone.",
			obj:       map[string]any{"spec": map[string]any{"size": "large", "region": "us-east-1"}},
			defaulted: map[string]any{"spec": map[string]any{"size": "large", "region": "us-east-1"}},
			orig:      map[string]any{"spec": map[string]any{"size": "large"}},
			want:      map[string]any{"spec": map[string]any{"size": "large"}},
		},
		"KeepsAValueTheUserWroteEvenIfItEqualsTheDefault": {
			reason:    "A field present in the original is the user's, whatever its value.",
			obj:       map[string]any{"spec": map[string]any{"region": "us-east-1"}},
			defaulted: map[string]any{"spec": map[string]any{"region": "us-east-1"}},
			orig:      map[string]any{"spec": map[string]any{"region": "us-east-1"}},
			want:      map[string]any{"spec": map[string]any{"region": "us-east-1"}},
		},
		"KeepsWhatWasAddedAfterDefaulting": {
			reason:    "Rendering and merging legitimately add fields; only defaulting's own additions are removed.",
			obj:       map[string]any{"metadata": map[string]any{"uid": "u"}, "spec": map[string]any{"crossplane": map[string]any{"resourceRefs": []any{}}}},
			defaulted: map[string]any{"spec": map[string]any{}},
			orig:      map[string]any{"spec": map[string]any{}},
			want:      map[string]any{"metadata": map[string]any{"uid": "u"}, "spec": map[string]any{"crossplane": map[string]any{"resourceRefs": []any{}}}},
		},
		"KeepsADefaultedFieldWhoseValueChangedAfterwards": {
			reason:    "If something after defaulting set a different value, that value is not a local default any more.",
			obj:       map[string]any{"spec": map[string]any{"region": "eu-west-1"}},
			defaulted: map[string]any{"spec": map[string]any{"region": "us-east-1"}},
			orig:      map[string]any{"spec": map[string]any{}},
			want:      map[string]any{"spec": map[string]any{"region": "eu-west-1"}},
		},
		"RemovesAWholeDefaultedObject": {
			reason:    "An object-valued default is removed as a unit.",
			obj:       map[string]any{"spec": map[string]any{"settings": map[string]any{"enabled": true, "timeout": int64(30)}}},
			defaulted: map[string]any{"spec": map[string]any{"settings": map[string]any{"enabled": true, "timeout": int64(30)}}},
			orig:      map[string]any{"spec": map[string]any{}},
			want:      map[string]any{"spec": map[string]any{}},
		},
		"RemovesDefaultsNestedUnderAUserWrittenObject": {
			reason:    "Defaults inside an object the user partly wrote are removed, leaving the user's part.",
			obj:       map[string]any{"spec": map[string]any{"settings": map[string]any{"enabled": false, "retries": int64(3)}}},
			defaulted: map[string]any{"spec": map[string]any{"settings": map[string]any{"enabled": false, "retries": int64(3)}}},
			orig:      map[string]any{"spec": map[string]any{"settings": map[string]any{"enabled": false}}},
			want:      map[string]any{"spec": map[string]any{"settings": map[string]any{"enabled": false}}},
		},
		"RemovesDefaultsInsideListItems": {
			reason:    "Structural defaulting reaches into list items, so the removal must too.",
			obj:       map[string]any{"spec": map[string]any{"ports": []any{map[string]any{"name": "http", "protocol": "TCP"}}}},
			defaulted: map[string]any{"spec": map[string]any{"ports": []any{map[string]any{"name": "http", "protocol": "TCP"}}}},
			orig:      map[string]any{"spec": map[string]any{"ports": []any{map[string]any{"name": "http"}}}},
			want:      map[string]any{"spec": map[string]any{"ports": []any{map[string]any{"name": "http"}}}},
		},
		"LeavesAListAloneWhenItsLengthChanged": {
			reason:    "Items can no longer be paired with their defaulted counterparts once the list has changed length.",
			obj:       map[string]any{"spec": map[string]any{"ports": []any{map[string]any{"name": "http", "protocol": "TCP"}, map[string]any{"name": "https"}}}},
			defaulted: map[string]any{"spec": map[string]any{"ports": []any{map[string]any{"name": "http", "protocol": "TCP"}}}},
			orig:      map[string]any{"spec": map[string]any{"ports": []any{map[string]any{"name": "http"}}}},
			want:      map[string]any{"spec": map[string]any{"ports": []any{map[string]any{"name": "http", "protocol": "TCP"}, map[string]any{"name": "https"}}}},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			obj := &un.Unstructured{Object: tt.obj}
			before := obj.DeepCopy()

			got := withoutLocalDefaults(obj, &un.Unstructured{Object: tt.defaulted}, &un.Unstructured{Object: tt.orig})

			if d := cmp.Diff(tt.want, got.Object); d != "" {
				t.Errorf("%s\nwithoutLocalDefaults() (-want +got):\n%s", tt.reason, d)
			}

			if d := cmp.Diff(before, obj); d != "" {
				t.Errorf("%s\nwithoutLocalDefaults() mutated its input (-before +after):\n%s", tt.reason, d)
			}
		})
	}
}
