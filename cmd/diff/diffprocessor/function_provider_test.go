/*
Copyright 2025 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package diffprocessor

import (
	"strings"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	gcmp "github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
)

func TestNewDefaultFunctionProvider(t *testing.T) {
	fnClient := tu.NewMockFunctionClient().Build()
	logger := tu.TestLogger(t, false)

	provider := NewDefaultFunctionProvider(fnClient, logger)

	if provider == nil {
		t.Fatal("NewDefaultFunctionProvider() returned nil")
	}

	if _, ok := provider.(*DefaultFunctionProvider); !ok {
		t.Errorf("NewDefaultFunctionProvider() returned wrong type: %T", provider)
	}
}

func TestDefaultFunctionProvider_GetFunctionsForComposition(t *testing.T) {
	tests := map[string]struct {
		setupMocks func() *tu.MockFunctionClientBuilder
		wantErr    bool
		wantCount  int
	}{
		"SuccessfulFetch": {
			setupMocks: func() *tu.MockFunctionClientBuilder {
				functions := []pkgv1.Function{
					{ObjectMeta: metav1.ObjectMeta{Name: "function-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "function-2"}},
				}

				return tu.NewMockFunctionClient().
					WithSuccessfulFunctionsFetch(functions)
			},
			wantErr:   false,
			wantCount: 2,
		},
		"FailedFetch": {
			setupMocks: func() *tu.MockFunctionClientBuilder {
				return tu.NewMockFunctionClient().
					WithFailedFunctionsFetch("function fetch error")
			},
			wantErr:   true,
			wantCount: 0,
		},
		"NoFunctions": {
			setupMocks: func() *tu.MockFunctionClientBuilder {
				return tu.NewMockFunctionClient().
					WithNoFunctions()
			},
			wantErr:   false,
			wantCount: 0,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fnClient := tt.setupMocks().Build()
			logger := tu.TestLogger(t, false)

			provider := NewDefaultFunctionProvider(fnClient, logger)

			comp := &apiextensionsv1.Composition{
				ObjectMeta: metav1.ObjectMeta{Name: "test-composition"},
			}

			fns, err := provider.GetFunctionsForComposition(comp)

			if (err != nil) != tt.wantErr {
				t.Errorf("GetFunctionsForComposition() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if len(fns) != tt.wantCount {
				t.Errorf("GetFunctionsForComposition() returned %d functions, want %d", len(fns), tt.wantCount)
			}
		})
	}
}

func TestNewCachedFunctionProvider(t *testing.T) {
	fnClient := tu.NewMockFunctionClient().Build()
	logger := tu.TestLogger(t, false)

	provider := NewCachedFunctionProvider(fnClient, logger)

	if provider == nil {
		t.Fatal("NewCachedFunctionProvider() returned nil")
	}

	cached, ok := provider.(*CachedFunctionProvider)
	if !ok {
		t.Fatalf("NewCachedFunctionProvider() returned wrong type: %T", provider)
	}

	if cached.cache == nil {
		t.Error("NewCachedFunctionProvider() did not initialize cache")
	}

	if len(cached.cache) != 0 {
		t.Errorf("NewCachedFunctionProvider() cache not empty, got %d entries", len(cached.cache))
	}
}

func TestCachedFunctionProvider_GetFunctionsForComposition_LazyLoading(t *testing.T) {
	functions := []pkgv1.Function{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "function-test"},
			Spec: pkgv1.FunctionSpec{
				PackageSpec: pkgv1.PackageSpec{
					Package: "xpkg.io/crossplane/function-go-templating:v0.11.0",
				},
			},
		},
	}

	fnClient := tu.NewMockFunctionClient().
		WithSuccessfulFunctionsFetch(functions).
		Build()

	logger := tu.TestLogger(t, false)
	provider := NewCachedFunctionProvider(fnClient, logger)

	comp := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "test-composition"},
	}

	fns, err := provider.GetFunctionsForComposition(comp)
	if err != nil {
		t.Fatalf("GetFunctionsForComposition() error = %v", err)
	}

	if len(fns) != 1 {
		t.Fatalf("GetFunctionsForComposition() returned %d functions, want 1", len(fns))
	}

	// The provider only caches: naming and cleaning up function containers is the render engine's job,
	// so the functions come back exactly as the cluster returned them.
	if diff := gcmp.Diff(functions, fns); diff != "" {
		t.Errorf("GetFunctionsForComposition() -want, +got:\n%s", diff)
	}
}

func TestCachedFunctionProvider_GetFunctionsForComposition_CacheHit(t *testing.T) {
	functions := []pkgv1.Function{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "function-test"},
			Spec: pkgv1.FunctionSpec{
				PackageSpec: pkgv1.PackageSpec{
					Package: "xpkg.io/test/function:v1.0.0",
				},
			},
		},
	}

	fetchCount := 0
	fnClient := tu.NewMockFunctionClient().
		WithFunctionsFetchCallback(func() ([]pkgv1.Function, error) {
			fetchCount++
			return functions, nil
		}).
		Build()

	logger := tu.TestLogger(t, false)
	provider := NewCachedFunctionProvider(fnClient, logger)

	comp := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "test-composition"},
	}

	// First call should fetch
	fns1, err := provider.GetFunctionsForComposition(comp)
	if err != nil {
		t.Fatalf("First GetFunctionsForComposition() error = %v", err)
	}

	if fetchCount != 1 {
		t.Errorf("First call: fetch count = %d, want 1", fetchCount)
	}

	// Second call should use cache
	fns2, err := provider.GetFunctionsForComposition(comp)
	if err != nil {
		t.Fatalf("Second GetFunctionsForComposition() error = %v", err)
	}

	if fetchCount != 1 {
		t.Errorf("Second call: fetch count = %d, want 1 (should use cache)", fetchCount)
	}

	// Verify both calls return the same data
	if len(fns1) != len(fns2) {
		t.Errorf("Cache returned different number of functions: first=%d, second=%d", len(fns1), len(fns2))
	}

	if fns1[0].Name != fns2[0].Name {
		t.Errorf("Cache returned different function: first=%s, second=%s", fns1[0].Name, fns2[0].Name)
	}
}

func TestCachedFunctionProvider_GetFunctionsForComposition_Error(t *testing.T) {
	fnClient := tu.NewMockFunctionClient().
		WithFailedFunctionsFetch("fetch error").
		Build()

	logger := tu.TestLogger(t, false)
	provider := NewCachedFunctionProvider(fnClient, logger)

	comp := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "test-composition"},
	}

	_, err := provider.GetFunctionsForComposition(comp)
	if err == nil {
		t.Fatal("GetFunctionsForComposition() expected error, got nil")
	}

	if !strings.Contains(err.Error(), "cannot get functions from pipeline") {
		t.Errorf("GetFunctionsForComposition() error = %v, want error containing 'cannot get functions from pipeline'", err)
	}
}

func TestCachedFunctionProvider_GetFunctionsForComposition_MultipleCompositions(t *testing.T) {
	fetchCalls := make(map[string]int)

	fnClient := tu.NewMockFunctionClient().
		WithGetFunctionsFromPipeline(func(comp *apiextensionsv1.Composition) ([]pkgv1.Function, error) {
			fetchCalls[comp.Name]++

			if comp.Name == "composition-1" {
				return []pkgv1.Function{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "function-1"},
						Spec: pkgv1.FunctionSpec{
							PackageSpec: pkgv1.PackageSpec{
								Package: "xpkg.io/test/function-one:v1.0.0",
							},
						},
					},
				}, nil
			}

			return []pkgv1.Function{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "function-2"},
					Spec: pkgv1.FunctionSpec{
						PackageSpec: pkgv1.PackageSpec{
							Package: "xpkg.io/test/function-two:v2.0.0",
						},
					},
				},
			}, nil
		}).
		Build()

	logger := tu.TestLogger(t, false)
	provider := NewCachedFunctionProvider(fnClient, logger)

	comp1 := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "composition-1"},
	}
	comp2 := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "composition-2"},
	}

	// Fetch for composition 1
	fns1, err := provider.GetFunctionsForComposition(comp1)
	if err != nil {
		t.Fatalf("GetFunctionsForComposition(comp1) error = %v", err)
	}

	// Fetch for composition 2
	fns2, err := provider.GetFunctionsForComposition(comp2)
	if err != nil {
		t.Fatalf("GetFunctionsForComposition(comp2) error = %v", err)
	}

	// Fetch comp1 again (should hit cache)
	fns1Again, err := provider.GetFunctionsForComposition(comp1)
	if err != nil {
		t.Fatalf("GetFunctionsForComposition(comp1 again) error = %v", err)
	}

	// Verify each composition gets its own functions
	if fns1[0].Name == fns2[0].Name {
		t.Error("Different compositions should have different functions")
	}

	// Verify cache works for comp1 second call
	if fns1[0].Name != fns1Again[0].Name {
		t.Error("Cache miss for composition-1 second call")
	}

	// Verify we only fetched once per composition
	if fetchCalls["composition-1"] != 1 {
		t.Errorf("composition-1 fetched %d times, want 1", fetchCalls["composition-1"])
	}

	if fetchCalls["composition-2"] != 1 {
		t.Errorf("composition-2 fetched %d times, want 1", fetchCalls["composition-2"])
	}
}

func TestReplaceRegistry(t *testing.T) {
	tests := map[string]struct {
		pkg         string
		newRegistry string
		want        string
	}{
		"StandardPackage": {
			pkg:         "xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.11.0",
			newRegistry: "registry.example.com",
			want:        "registry.example.com/crossplane-contrib/function-go-templating:v0.11.0",
		},
		"DifferentRegistry": {
			pkg:         "ghcr.io/crossplane/function-auto-ready:v1.2.3",
			newRegistry: "registry.example.com",
			want:        "registry.example.com/crossplane/function-auto-ready:v1.2.3",
		},
		"SHA256Digest": {
			pkg:         "old.registry.io/org/func@sha256:abc123",
			newRegistry: "new.registry.io",
			want:        "new.registry.io/org/func@sha256:abc123",
		},
		"NoExplicitRegistry": {
			// First segment lacks '.', ':' or "localhost", so it is treated as
			// part of the path (per OCI rules), and newRegistry is prepended.
			pkg:         "crossplane-contrib/function-auto-ready:v1.0.0",
			newRegistry: "new.registry.io",
			want:        "new.registry.io/crossplane-contrib/function-auto-ready:v1.0.0",
		},
		"NewRegistryWithTrailingSlash": {
			pkg:         "xpkg.crossplane.io/foo/bar:v1",
			newRegistry: "new.registry.io/",
			want:        "new.registry.io/foo/bar:v1",
		},
		"LocalhostWithPort": {
			pkg:         "localhost:5000/foo/bar:v1",
			newRegistry: "new.registry.io",
			want:        "new.registry.io/foo/bar:v1",
		},
		"NoSlash": {
			pkg:         "bare-image:v1",
			newRegistry: "new.registry.io",
			want:        "new.registry.io/bare-image:v1",
		},
		"DeepPath": {
			pkg:         "registry.io/a/b/c/func:v1",
			newRegistry: "other.io",
			want:        "other.io/a/b/c/func:v1",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := replaceRegistry(tt.pkg, tt.newRegistry)
			if got != tt.want {
				t.Errorf("replaceRegistry(%q, %q) = %q, want %q", tt.pkg, tt.newRegistry, got, tt.want)
			}
		})
	}
}

func TestRegistryOverrideFunctionProvider(t *testing.T) {
	functions := []pkgv1.Function{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "function-go-templating"},
			Spec: pkgv1.FunctionSpec{
				PackageSpec: pkgv1.PackageSpec{
					Package: "original.registry.io/crossplane-contrib/function-go-templating:v0.11.0",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "function-auto-ready"},
			Spec: pkgv1.FunctionSpec{
				PackageSpec: pkgv1.PackageSpec{
					Package: "other.registry.io/crossplane-contrib/function-auto-ready:v1.0.0",
				},
			},
		},
	}

	fnClient := tu.NewMockFunctionClient().
		WithSuccessfulFunctionsFetch(functions).
		Build()

	logger := tu.TestLogger(t, false)
	inner := NewDefaultFunctionProvider(fnClient, logger)

	provider := NewRegistryOverrideFunctionProvider(inner, "registry.example.com", logger)

	comp := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "test-composition"},
	}

	fns, err := provider.GetFunctionsForComposition(comp)
	if err != nil {
		t.Fatalf("GetFunctionsForComposition() error = %v", err)
	}

	if len(fns) != 2 {
		t.Fatalf("GetFunctionsForComposition() returned %d functions, want 2", len(fns))
	}

	if fns[0].Spec.Package != "registry.example.com/crossplane-contrib/function-go-templating:v0.11.0" {
		t.Errorf("function[0].Spec.Package = %q, want overridden registry", fns[0].Spec.Package)
	}

	if fns[1].Spec.Package != "registry.example.com/crossplane-contrib/function-auto-ready:v1.0.0" {
		t.Errorf("function[1].Spec.Package = %q, want overridden registry", fns[1].Spec.Package)
	}
}
