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
	"context"
	"strings"
	"testing"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
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
					WithSuccessfulFunctionsFetch([]pkgv1.Function{})
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

	// Verify Docker reuse annotations were added
	if fns[0].Annotations == nil {
		t.Fatal("GetFunctionsForComposition() did not add annotations")
	}

	// Container name should have format: function-go-templating-v0.11.0-comp-<instanceID>
	expectedPrefix := "function-go-templating-v0.11.0-comp-"

	gotContainerName := fns[0].Annotations["render.crossplane.io/runtime-docker-name"]
	if !strings.HasPrefix(gotContainerName, expectedPrefix) {
		t.Errorf("Container name = %q, want prefix %q", gotContainerName, expectedPrefix)
	}

	// Verify the instance ID suffix is present and has expected length (8 hex chars)
	instanceID := strings.TrimPrefix(gotContainerName, expectedPrefix)
	if len(instanceID) != 8 {
		t.Errorf("Instance ID length = %d, want 8 (got container name: %q)", len(instanceID), gotContainerName)
	}

	gotCleanup := fns[0].Annotations["render.crossplane.io/runtime-docker-cleanup"]
	if gotCleanup != "Orphan" {
		t.Errorf("Cleanup policy = %q, want %q", gotCleanup, "Orphan")
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

func TestCachedFunctionProvider_Cleanup(t *testing.T) {
	tests := map[string]struct {
		setupContainers func() []string // Returns container names to create
		expectCleanup   bool            // Whether cleanup should be attempted
	}{
		"NoContainers": {
			setupContainers: func() []string {
				return []string{}
			},
			expectCleanup: false,
		},
		"WithContainers": {
			setupContainers: func() []string {
				// We'll track container names but won't actually create them
				// The test will verify the cleanup logic is called correctly
				return []string{"test-container-1-comp", "test-container-2-comp"}
			},
			expectCleanup: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fnClient := tu.NewMockFunctionClient().Build()
			logger := tu.TestLogger(t, false)
			provider := &CachedFunctionProvider{
				fnClient:       fnClient,
				cache:          make(map[string][]pkgv1.Function),
				containerNames: tt.setupContainers(),
				logger:         logger,
			}

			ctx := context.Background()
			err := provider.Cleanup(ctx)
			// Cleanup should never return an error (graceful degradation)
			if err != nil {
				t.Errorf("Cleanup() returned unexpected error: %v", err)
			}
		})
	}
}

func TestCachedFunctionProvider_TracksContainerNames(t *testing.T) {
	functions := []pkgv1.Function{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "function-1"},
			Spec: pkgv1.FunctionSpec{
				PackageSpec: pkgv1.PackageSpec{
					Package: "xpkg.io/test/function-one:v1.0.0",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "function-2"},
			Spec: pkgv1.FunctionSpec{
				PackageSpec: pkgv1.PackageSpec{
					Package: "xpkg.io/test/function-two:v2.0.0",
				},
			},
		},
	}

	fnClient := tu.NewMockFunctionClient().
		WithSuccessfulFunctionsFetch(functions).
		Build()

	logger := tu.TestLogger(t, false)
	provider := NewCachedFunctionProvider(fnClient, logger).(*CachedFunctionProvider)

	comp := &apiextensionsv1.Composition{
		ObjectMeta: metav1.ObjectMeta{Name: "test-composition"},
	}

	_, err := provider.GetFunctionsForComposition(comp)
	if err != nil {
		t.Fatalf("GetFunctionsForComposition() error = %v", err)
	}

	// Verify container names were tracked
	if len(provider.containerNames) != 2 {
		t.Errorf("Expected 2 container names tracked, got %d", len(provider.containerNames))
	}

	// Container names should have format: <function-name>-<version>-comp-<instanceID>
	expectedPrefixes := []string{"function-one-v1.0.0-comp-", "function-two-v2.0.0-comp-"}
	for i, expectedPrefix := range expectedPrefixes {
		if i >= len(provider.containerNames) {
			t.Errorf("Missing container name at index %d", i)
			continue
		}

		if !strings.HasPrefix(provider.containerNames[i], expectedPrefix) {
			t.Errorf("Container name[%d] = %q, want prefix %q", i, provider.containerNames[i], expectedPrefix)
		}

		// Verify instance ID is present
		instanceID := strings.TrimPrefix(provider.containerNames[i], expectedPrefix)
		if len(instanceID) != 8 {
			t.Errorf("Instance ID length at index %d = %d, want 8 (got: %q)", i, len(instanceID), provider.containerNames[i])
		}
	}
}

func TestGenerateContainerName(t *testing.T) {
	const testInstanceID = "test1234"

	tests := map[string]struct {
		pkg  string
		want string
	}{
		"StandardPackage": {
			pkg:  "xpkg.io/crossplane-contrib/function-go-templating:v0.11.0",
			want: "function-go-templating-v0.11.0-comp-test1234",
		},
		"DifferentRegistry": {
			pkg:  "ghcr.io/crossplane/function-auto-ready:v1.2.3",
			want: "function-auto-ready-v1.2.3-comp-test1234",
		},
		"ShortPackage": {
			pkg:  "function-test:v1.0.0",
			want: "function-test-v1.0.0-comp-test1234",
		},
		"NoVersion": {
			pkg:  "xpkg.io/org/function-name",
			want: "function-name-comp-test1234",
		},
		"EmptyPackage": {
			pkg:  "",
			want: "unknown-comp-test1234",
		},
		"OnlyName": {
			pkg:  "my-function",
			want: "my-function-comp-test1234",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := generateContainerName(tt.pkg, testInstanceID)
			if got != tt.want {
				t.Errorf("generateContainerName(%q, %q) = %q, want %q", tt.pkg, testInstanceID, got, tt.want)
			}
		})
	}
}
