package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	run "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	cgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	xpextv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	xpextv2 "github.com/crossplane/crossplane/v2/apis/apiextensions/v2"
	pkgv1 "github.com/crossplane/crossplane/v2/apis/pkg/v1"
)

const (
	timeout = 60 * time.Second
)

// DiffTestType represents the type of diff test to run.
type DiffTestType string

const (
	XRDiffTest          DiffTestType = "diff"
	CompositionDiffTest DiffTestType = "comp"
)

// IntegrationTestCase represents a common test case structure for both XR and composition diff tests.
type IntegrationTestCase struct {
	reason                     string // Description of what this test validates
	setupFiles                 []string
	crossplaneManagedResources []HierarchicalOwnershipRelation // Resources applied via SSA with Crossplane field manager
	inputFiles                 []string                        // Input files to diff (XR YAML files or Composition YAML files)
	expectedOutput          string
	expectedError           bool
	expectedErrorContains   string
	noColor                 bool
	namespace               string        // For composition tests (optional)
	xrdAPIVersion           XrdAPIVersion // For XR tests (optional)
	ignorePaths             []string      // Paths to ignore in diffs
	skip                    bool
	skipReason              string
}

type XrdAPIVersion int

const (
	V2 XrdAPIVersion = iota // v2 comes first so that this is the default value
	V1
)

var versionNames = map[XrdAPIVersion]string{
	V1: "apiextensions.crossplane.io/v1",
	V2: "apiextensions.crossplane.io/v2",
}

func (s XrdAPIVersion) String() string {
	return versionNames[s]
}

// createTestScheme creates a runtime scheme with all required types registered.
// This can be shared across tests since it's just a type registry.
func createTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()

	// Register Kubernetes types
	_ = cgoscheme.AddToScheme(scheme)

	// Register Crossplane types
	_ = xpextv1.AddToScheme(scheme)
	_ = xpextv2.AddToScheme(scheme)
	_ = pkgv1.AddToScheme(scheme)
	_ = extv1.AddToScheme(scheme)

	return scheme
}

// runIntegrationTest runs a single integration test case for both XR and composition diff commands.
func runIntegrationTest(t *testing.T, testType DiffTestType, scheme *runtime.Scheme, tt IntegrationTestCase) {
	t.Helper()

	t.Parallel() // Enable parallel test execution

	// Skip test if requested
	if tt.skip {
		t.Skip(tt.skipReason)
		return
	}

	// Setup a brand new test environment for each test case
	_, thisFile, _, ok := run.Caller(0)
	if !ok {
		t.Fatal("failed to get caller information")
	}

	thisDir := filepath.Dir(thisFile)

	crdPaths := []string{
		filepath.Join(thisDir, "..", "..", "cluster", "main", "crds"),
		filepath.Join(thisDir, "testdata", string(testType), "crds"),
	}

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
		ErrorIfCRDPathMissing: true,
		Scheme:                scheme,
		// Note: Leaving ControlPlane unset (nil) allows envtest to create its own control plane
		// with random ports, which enables parallel test execution without port conflicts.
		// Each test gets its own isolated API server and etcd instance on random available ports.
	}

	// Start the test environment
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("failed to start test environment: %v", err)
	}

	// Ensure we clean up at the end of the test
	defer func() {
		err := testEnv.Stop()
		if err != nil {
			t.Logf("failed to stop test environment: %v", err)
		}
	}()

	// Create a controller-runtime client for setup operations
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	// Apply the setup resources
	if err := applyResourcesFromFiles(ctx, k8sClient, tt.setupFiles); err != nil {
		t.Fatalf("failed to setup resources: %v", err)
	}

	// Default to v2 API version for XR resources unless otherwise specified
	xrdAPIVersion := V2
	if tt.xrdAPIVersion != V2 {
		xrdAPIVersion = tt.xrdAPIVersion
	}

	// Apply Crossplane-managed resources (XRs, composed resources) using SSA with Crossplane field manager.
	// These resources simulate what Crossplane actually manages in production.
	if len(tt.crossplaneManagedResources) > 0 {
		err := applyHierarchicalOwnership(ctx, tu.TestLogger(t, false), k8sClient, xrdAPIVersion, tt.crossplaneManagedResources)
		if err != nil {
			t.Fatalf("failed to setup Crossplane-managed resources: %v", err)
		}
	}

	// Set up the test files
	var testFiles []string

	// Handle any additional input files
	// Note: NewCompositeLoader handles both individual files and directories,
	// so we can pass paths directly without special handling
	testFiles = append(testFiles, tt.inputFiles...)

	// Create a buffer to capture the output
	var stdout bytes.Buffer

	// Create command line args that match your pre-populated struct
	args := []string{
		fmt.Sprintf("--timeout=%s", timeout.String()),
	}

	// Add namespace if specified (for composition tests only)
	if tt.namespace != "" && testType == CompositionDiffTest {
		args = append(args, fmt.Sprintf("--namespace=%s", tt.namespace))
	}

	// Add no-color flag if true
	if tt.noColor {
		args = append(args, "--no-color")
	}

	// Add ignore-paths if specified
	if len(tt.ignorePaths) > 0 {
		for _, path := range tt.ignorePaths {
			args = append(args, fmt.Sprintf("--ignore-paths=%s", path))
		}
	}

	// Add files as positional arguments
	args = append(args, testFiles...)

	// Set up the appropriate command based on test type
	var cmd any
	if testType == CompositionDiffTest {
		cmd = &CompCmd{}
	} else {
		cmd = &XRCmd{}
	}

	logger := tu.TestLogger(t, true)
	// Create a Kong context with stdout
	parser, err := kong.New(cmd,
		kong.Writers(&stdout, &stdout),
		kong.Bind(cfg),
		kong.BindTo(logger, (*logging.Logger)(nil)),
	)
	if err != nil {
		t.Fatalf("failed to create kong parser: %v", err)
	}

	kongCtx, err := parser.Parse(args)
	if err != nil {
		t.Fatalf("failed to parse kong context: %v", err)
	}

	err = kongCtx.Run(cfg)

	if tt.expectedError && err == nil {
		t.Fatal("expected error but got none")
	}

	if !tt.expectedError && err != nil {
		t.Fatalf("expected no error but got: %v", err)
	}

	// Check for specific error message if expected
	if err != nil {
		if tt.expectedErrorContains != "" && strings.Contains(err.Error(), tt.expectedErrorContains) {
			// This is an expected error with the expected message
			t.Logf("Got expected error containing: %s", tt.expectedErrorContains)
		} else {
			t.Errorf("Expected no error or specific error message, got: %v", err)
		}
	}

	// For expected errors with specific messages, we've already checked above
	if tt.expectedError && tt.expectedErrorContains != "" {
		// Skip output check for expected error cases
		return
	}

	// Check the output
	outputStr := stdout.String()
	// Using TrimSpace because the output might have trailing newlines
	if !strings.Contains(strings.TrimSpace(outputStr), strings.TrimSpace(tt.expectedOutput)) {
		// Strings aren't equal, *including* ansi.  but we can compare ignoring ansi to determine what output to
		// show for the failure.  if the difference is only in color codes, we'll show escaped ansi codes.
		out := outputStr

		expect := tt.expectedOutput
		if tu.CompareIgnoringAnsi(strings.TrimSpace(outputStr), strings.TrimSpace(tt.expectedOutput)) {
			out = strconv.QuoteToASCII(outputStr)
			expect = strconv.QuoteToASCII(tt.expectedOutput)
		}

		t.Fatalf("expected output to contain:\n%s\n\nbut got:\n%s", expect, out)
	}
}

// TestDiffIntegration runs an integration test for the diff command.
func TestDiffIntegration(t *testing.T) {
	t.Parallel()

	// Set up logger for controller-runtime (global setup, once per test function)
	tu.SetupKubeTestLogger(t)

	scheme := createTestScheme()

	tests := map[string]IntegrationTestCase{
		"NewResourceDiff": {
			reason:     "Shows color diff for new resources",
			inputFiles: []string{"testdata/diff/new-xr.yaml"},
			setupFiles: []string{
				// TODO: For v2, we need to upgrade the XRD apiversion to v2 + put the xr in a namespace.
				// this might be better served as a separate test(s).
				// since we aren't actually creating resources (there's no crossplane actually running in unit tests),
				// we don't need to worry about upgrading providers or anything.
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			expectedOutput: strings.Join([]string{
				`+++ XDownstreamResource/test-resource
`, tu.Green(`+ apiVersion: ns.nop.example.org/v1alpha1
+ kind: XDownstreamResource
+ metadata:
+   annotations:
+     crossplane.io/composition-resource-name: nop-resource
+   labels:
+     crossplane.io/composite: test-resource
+   name: test-resource
+   namespace: default
+ spec:
+   forProvider:
+     configData: new-value
`), `
---
+++ XNopResource/test-resource
`, tu.Green(`+ apiVersion: ns.diff.example.org/v1alpha1
+ kind: XNopResource
+ metadata:
+   name: test-resource
+   namespace: default
+ spec:
+   coolField: new-value
`), `
---
`,
			}, ""),
			expectedError: false,
		},
		"AutomaticNamespacePropagation": {
			reason:     "Validates automatic namespace propagation for namespaced managed resources",
			inputFiles: []string{"testdata/diff/new-xr.yaml"},
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition-no-namespace-patch.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			expectedOutput: strings.Join([]string{
				`+++ XDownstreamResource/test-resource
`, tu.Green(`+ apiVersion: ns.nop.example.org/v1alpha1
+ kind: XDownstreamResource
+ metadata:
+   annotations:
+     crossplane.io/composition-resource-name: nop-resource
+   labels:
+     crossplane.io/composite: test-resource
+   name: test-resource
+   namespace: default
+ spec:
+   forProvider:
+     configData: new-value
`), `
---
+++ XNopResource/test-resource
`, tu.Green(`+ apiVersion: ns.diff.example.org/v1alpha1
+ kind: XNopResource
+ metadata:
+   name: test-resource
+   namespace: default
+ spec:
+   coolField: new-value
`), `
---
`,
			}, ""),
			expectedError: false,
		},
		"ModifiedResourceDiff": {
			reason: "Shows color diff for modified resources",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition.yaml",
				"testdata/diff/resources/composition-revision-default.yaml",
				"testdata/diff/resources/functions.yaml",
				// put an existing XR in the cluster to diff against
				"testdata/diff/resources/existing-downstream-resource.yaml",
				"testdata/diff/resources/existing-xr.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-xr.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/test-resource
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: nop-resource
    generateName: test-resource-
    labels:
      crossplane.io/composite: test-resource
    name: test-resource
    namespace: default
  spec:
    forProvider:
` + tu.Red("-     configData: existing-value") + `
` + tu.Green("+     configData: modified-value") + `

---
~~~ XNopResource/test-resource
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-resource
    namespace: default
  spec:
` + tu.Red("-   coolField: existing-value") + `
` + tu.Green("+   coolField: modified-value") + `

---

Summary: 2 modified`,
			expectedError: false,
		},
		"IgnorePathsArgoCD": {
			reason: "Ignores ArgoCD annotations and labels when --ignore-paths is specified",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition.yaml",
				"testdata/diff/resources/composition-revision-default.yaml",
				"testdata/diff/resources/functions.yaml",
				// put an existing resource with different ArgoCD annotations
				"testdata/diff/resources/existing-downstream-resource-with-argocd.yaml",
				"testdata/diff/resources/existing-xr-with-argocd.yaml",
			},
			inputFiles: []string{"testdata/diff/xr-with-argocd-annotations.yaml"},
			ignorePaths: []string{
				"metadata.annotations[argocd.argoproj.io/tracking-id]",
				"metadata.labels[argocd.argoproj.io/instance]",
			},
			expectedOutput: ``,
			expectedError:  false,
		},
		"ModifiedXRCreatesDownstream": {
			reason: "Shows color diff when modified XR creates new downstream resource",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition.yaml",
				"testdata/diff/resources/composition-revision-default.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/existing-xr.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-xr.yaml"},
			expectedOutput: `
+++ XDownstreamResource/test-resource
` + tu.Green(`+ apiVersion: ns.nop.example.org/v1alpha1
+ kind: XDownstreamResource
+ metadata:
+   annotations:
+     crossplane.io/composition-resource-name: nop-resource
+   labels:
+     crossplane.io/composite: test-resource
+   name: test-resource
+   namespace: default
+ spec:
+   forProvider:
+     configData: modified-value
`) + `
---
~~~ XNopResource/test-resource
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-resource
    namespace: default
  spec:
` + tu.Red("-   coolField: existing-value") + `
` + tu.Green("+   coolField: modified-value") + `

---

Summary: 1 added, 1 modified`,
			expectedError: false,
		},
		"EnvironmentConfigIncorporation": {
			reason: "Validates EnvironmentConfig (v1beta1) incorporation in diff",
			setupFiles: []string{
				"testdata/diff/resources/xdownstreamenvresource-xrd.yaml",
				"testdata/diff/resources/env-xrd.yaml",
				"testdata/diff/resources/env-composition.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/environment-config-v1beta1.yaml",
				"testdata/diff/resources/existing-env-downstream-resource.yaml",
				"testdata/diff/resources/existing-env-xr.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-env-xr.yaml"},
			expectedOutput: `
~~~ XDownstreamEnvResource/test-env-resource
  apiVersion: nop.example.org/v1alpha1
  kind: XDownstreamEnvResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: env-resource
    generateName: test-env-resource-
    labels:
      crossplane.io/composite: test-env-resource
    name: test-env-resource
  spec:
    compositionUpdatePolicy: Automatic
    forProvider:
-     configData: existing-config-value
+     configData: modified-config-value
      environment: staging
      region: us-west-2
      serviceLevel: premium

---
~~~ XEnvResource/test-env-resource
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XEnvResource
  metadata:
    name: test-env-resource
  spec:
    compositionUpdatePolicy: Automatic
-   configKey: existing-config-value
+   configKey: modified-config-value

---
`,
			expectedError: false,
			noColor:       true,
		},
		"ExternalResourceDependencies": {
			reason: "Validates diff with external resource dependencies via fn-external-resources",
			// this test does a weird thing where it changes the XR but all the downstream changes come from external
			// resources, including a field path from the XR itself.
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/external-resource-configmap.yaml",
				"testdata/diff/resources/external-res-fn-composition.yaml",
				"testdata/diff/resources/existing-xr-with-external-dep.yaml",
				"testdata/diff/resources/existing-downstream-with-external-dep.yaml",
				"testdata/diff/resources/external-named-clusterrole.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-xr-with-external-dep.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/test-resource
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: nop-resource
    generateName: test-resource-
    labels:
      crossplane.io/composite: test-resource
    name: test-resource
    namespace: default
  spec:
    forProvider:
-     configData: existing-value
-     roleName: old-role-name
+     configData: testing-external-resource-data
+     roleName: external-named-clusterrole

---
~~~ XNopResource/test-resource
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-resource
    namespace: default
  spec:
-   coolField: existing-value
-   environment: staging
+   coolField: modified-with-external-dep
+   environment: testing

---
`,
			expectedError: false,
			noColor:       true,
		},
		"TemplatedExtraResources": {
			reason: "Validates diff with templated ExtraResources embedded in go-templating function",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/external-resource-configmap.yaml",
				"testdata/diff/resources/external-res-gotpl-composition.yaml",
				"testdata/diff/resources/existing-xr-with-external-dep.yaml",
				"testdata/diff/resources/existing-downstream-with-external-dep.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-xr-with-external-dep.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/test-resource
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: nop-resource
    generateName: test-resource-
    labels:
      crossplane.io/composite: test-resource
    name: test-resource
    namespace: default
  spec:
    forProvider:
-     configData: existing-value
-     roleName: old-role-name
+     configData: modified-with-external-dep
+     roleName: templated-external-resource-testing

---
~~~ XNopResource/test-resource
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-resource
    namespace: default
  spec:
-   coolField: existing-value
-   environment: staging
+   coolField: modified-with-external-dep
+   environment: testing

---
`,
			expectedError: false,
			noColor:       true,
		},
		"CrossNamespaceResourceDependencies": {
			reason: "Validates cross-namespace resource dependencies via fn-external-resources",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/cross-namespace-configmap.yaml",
				"testdata/diff/resources/cross-namespace-fn-composition.yaml",
				"testdata/diff/resources/existing-cross-ns-xr.yaml",
				"testdata/diff/resources/existing-cross-ns-downstream.yaml",
				"testdata/diff/resources/external-named-clusterrole.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-cross-ns-xr.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/test-cross-ns-resource
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: cross-ns-resource
    generateName: test-cross-ns-resource-
    labels:
      crossplane.io/composite: test-cross-ns-resource
    name: test-cross-ns-resource
    namespace: default
  spec:
    forProvider:
-     configData: existing-cross-ns-data-existing-named-data-old-cross-ns-role
+     configData: cross-namespace-data-another-cross-namespace-data-external-named-clusterrole

---
~~~ XNopResource/test-cross-ns-resource
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-cross-ns-resource
    namespace: default
  spec:
-   coolField: existing-cross-ns-value
-   environment: staging
+   coolField: modified-cross-ns-value
+   environment: production

---
`,
			expectedError: false,
			noColor:       true,
		},
		"ResourceRemovalHierarchyV1ClusterScoped": {
			reason:        "Validates resource removal detection with hierarchy using v1 style resourceRefs and cluster scoped downstreams",
			xrdAPIVersion: V1,
			crossplaneManagedResources: []HierarchicalOwnershipRelation{
				{
					OwnerFile: "testdata/diff/resources/existing-legacy-xr.yaml",
					OwnedFiles: map[string]*HierarchicalOwnershipRelation{
						"testdata/diff/resources/removal-test-legacycluster-downstream-resource1.yaml": nil, // Will be kept
						"testdata/diff/resources/removal-test-legacycluster-downstream-resource2.yaml": {
							// This resource will be removed and has a child
							OwnedFiles: map[string]*HierarchicalOwnershipRelation{
								"testdata/diff/resources/removal-test-legacycluster-downstream-resource2-child.yaml": nil, // Child will also be removed
							},
						},
					},
				},
			},
			setupFiles: []string{
				"testdata/diff/resources/legacy-xrd.yaml",
				"testdata/diff/resources/removal-test-legacy-composition.yaml",
				"testdata/diff/resources/removal-test-legacy-composition-revision.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-legacy-xr.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/resource-to-be-kept
  apiVersion: legacycluster.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: resource1
    generateName: test-resource-
    labels:
      crossplane.io/composite: test-resource
    name: resource-to-be-kept
  spec:
    forProvider:
-     configData: existing-value
+     configData: modified-value

---
--- XDownstreamResource/resource-to-be-removed
- apiVersion: legacycluster.nop.example.org/v1alpha1
- kind: XDownstreamResource
- metadata:
-   annotations:
-     crossplane.io/composition-resource-name: resource2
-   generateName: test-resource-
-   labels:
-     crossplane.io/composite: test-resource
-   name: resource-to-be-removed
- spec:
-   forProvider:
-     configData: existing-value

---
--- XDownstreamResource/resource-to-be-removed-child
- apiVersion: legacycluster.nop.example.org/v1alpha1
- kind: XDownstreamResource
- metadata:
-   annotations:
-     crossplane.io/composition-resource-name: resource2-child
-   generateName: test-resource-child-
-   labels:
-     crossplane.io/composite: test-resource
-   name: resource-to-be-removed-child
- spec:
-   forProvider:
-     configData: child-value

---
~~~ XNopResource/test-resource
  apiVersion: legacycluster.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-resource
  spec:
    compositionUpdatePolicy: Automatic
-   coolField: existing-value
+   coolField: modified-value

---

Summary: 2 modified, 2 removed`,
			expectedError: false,
			noColor:       true,
		},
		"ResourceRemovalHierarchyV2Namespaced": {
			reason: "Validates resource removal detection with hierarchy using v2 style resourceRefs and namespaced downstreams",
			crossplaneManagedResources: []HierarchicalOwnershipRelation{
				{
					OwnerFile: "testdata/diff/resources/existing-xr.yaml",
					OwnedFiles: map[string]*HierarchicalOwnershipRelation{
						"testdata/diff/resources/removal-test-ns-downstream-resource1.yaml": nil, // Will be kept
						"testdata/diff/resources/removal-test-ns-downstream-resource2.yaml": {
							// This resource will be removed and has a child
							OwnedFiles: map[string]*HierarchicalOwnershipRelation{
								"testdata/diff/resources/removal-test-ns-downstream-resource2-child.yaml": nil, // Child will also be removed
							},
						},
					},
				},
			},
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/removal-test-composition.yaml",
				"testdata/diff/resources/removal-test-composition-revision.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-xr.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/resource-to-be-kept
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: resource1
    generateName: test-resource-
    labels:
      crossplane.io/composite: test-resource
    name: resource-to-be-kept
    namespace: default
  spec:
    forProvider:
-     configData: existing-value
+     configData: modified-value

---
--- XDownstreamResource/resource-to-be-removed
- apiVersion: ns.nop.example.org/v1alpha1
- kind: XDownstreamResource
- metadata:
-   annotations:
-     crossplane.io/composition-resource-name: resource2
-   generateName: test-resource-
-   labels:
-     crossplane.io/composite: test-resource
-   name: resource-to-be-removed
-   namespace: default
- spec:
-   forProvider:
-     configData: existing-value

---
--- XDownstreamResource/resource-to-be-removed-child
- apiVersion: ns.nop.example.org/v1alpha1
- kind: XDownstreamResource
- metadata:
-   annotations:
-     crossplane.io/composition-resource-name: resource2-child
-   generateName: test-resource-child-
-   labels:
-     crossplane.io/composite: test-resource
-   name: resource-to-be-removed-child
-   namespace: default
- spec:
-   forProvider:
-     configData: child-value

---
~~~ XNopResource/test-resource
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-resource
    namespace: default
  spec:
-   coolField: existing-value
+   coolField: modified-value

---

Summary: 2 modified, 2 removed`,
			expectedError: false,
			noColor:       true,
		},
		"ResourceRemovalHierarchyV2ClusterScoped": {
			reason: "Validates resource removal detection with hierarchy using v2 style resourceRefs and cluster scoped downstreams",
			crossplaneManagedResources: []HierarchicalOwnershipRelation{
				{
					OwnerFile: "testdata/diff/resources/existing-cluster-xr.yaml",
					OwnedFiles: map[string]*HierarchicalOwnershipRelation{
						"testdata/diff/resources/removal-test-cluster-downstream-resource1.yaml": nil, // Will be kept
						"testdata/diff/resources/removal-test-cluster-downstream-resource2.yaml": {
							// This resource will be removed and has a child
							OwnedFiles: map[string]*HierarchicalOwnershipRelation{
								"testdata/diff/resources/removal-test-cluster-downstream-resource2-child.yaml": nil, // Child will also be removed
							},
						},
					},
				},
			},
			setupFiles: []string{
				"testdata/diff/resources/cluster-xrd.yaml",
				"testdata/diff/resources/removal-test-cluster-composition.yaml",
				"testdata/diff/resources/removal-test-cluster-composition-revision.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-cluster-xr.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/resource-to-be-kept
  apiVersion: nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: resource1
    generateName: test-resource-
    labels:
      crossplane.io/composite: test-resource
    name: resource-to-be-kept
  spec:
    forProvider:
-     configData: existing-value
+     configData: modified-value

---
--- XDownstreamResource/resource-to-be-removed
- apiVersion: nop.example.org/v1alpha1
- kind: XDownstreamResource
- metadata:
-   annotations:
-     crossplane.io/composition-resource-name: resource2
-   generateName: test-resource-
-   labels:
-     crossplane.io/composite: test-resource
-   name: resource-to-be-removed
- spec:
-   forProvider:
-     configData: existing-value

---
--- XDownstreamResource/resource-to-be-removed-child
- apiVersion: nop.example.org/v1alpha1
- kind: XDownstreamResource
- metadata:
-   annotations:
-     crossplane.io/composition-resource-name: resource2-child
-   generateName: test-resource-child-
-   labels:
-     crossplane.io/composite: test-resource
-   name: resource-to-be-removed-child
- spec:
-   forProvider:
-     configData: child-value

---
~~~ XNopResource/test-resource
  apiVersion: diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-resource
  spec:
-   coolField: existing-value
+   coolField: modified-value

---

Summary: 2 modified, 2 removed`,
			expectedError: false,
			noColor:       true,
		},
		"ResourceWithGenerateName": {
			reason: "Validates handling of resources with generateName",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/generated-name-composition.yaml",
			},
			crossplaneManagedResources: []HierarchicalOwnershipRelation{
				{
					// Set up the XR as the owner of the generated composed resource
					OwnerFile: "testdata/diff/resources/existing-xr.yaml",
					OwnedFiles: map[string]*HierarchicalOwnershipRelation{
						// This file has a generated name and is owned by the XR
						"testdata/diff/resources/existing-downstream-with-generated-name.yaml": nil,
					},
				},
			},
			// Use a composition that uses generateName for composed resources
			inputFiles: []string{"testdata/diff/new-xr.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/test-resource-abc123
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: nop-resource
    generateName: test-resource-
    labels:
      crossplane.io/composite: test-resource
    name: test-resource-abc123
    namespace: default
  spec:
    forProvider:
-     configData: existing-value
+     configData: new-value

---
~~~ XNopResource/test-resource
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-resource
    namespace: default
  spec:
-   coolField: existing-value
+   coolField: new-value

---
`,
			expectedError: false,
			noColor:       true,
		},
		"NewXRWithGenerateName": {
			reason: "Shows diff for new XR with generateName",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition.yaml",
				"testdata/diff/resources/functions.yaml",
				// We don't add any existing XR, as we're testing creation of a new one
			},
			inputFiles: []string{"testdata/diff/generated-name-xr.yaml"},
			expectedOutput: `
+++ XDownstreamResource/generated-xr-(generated)
+ apiVersion: ns.nop.example.org/v1alpha1
+ kind: XDownstreamResource
+ metadata:
+   annotations:
+     crossplane.io/composition-resource-name: nop-resource
+   labels:
+     crossplane.io/composite: generated-xr-(generated)
+   name: generated-xr-(generated)
+   namespace: default
+ spec:
+   forProvider:
+     configData: new-value

---
+++ XNopResource/generated-xr-(generated)
+ apiVersion: ns.diff.example.org/v1alpha1
+ kind: XNopResource
+ metadata:
+   generateName: generated-xr-
+   namespace: default
+ spec:
+   coolField: new-value

---
`,
			expectedError: false,
			noColor:       true,
		},
		"MultipleXRs": {
			reason: "Validates diff for multiple XRs",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition.yaml",
				"testdata/diff/resources/composition-revision-default.yaml",
				"testdata/diff/resources/functions.yaml",
				// Add an existing XR and downstream resource to test modification
				"testdata/diff/resources/existing-xr.yaml",
				"testdata/diff/resources/existing-downstream-resource.yaml",
			},
			inputFiles: []string{
				"testdata/diff/first-xr.yaml",
				"testdata/diff/modified-xr.yaml",
			},
			expectedOutput: `
+++ XDownstreamResource/first-resource
+ apiVersion: ns.nop.example.org/v1alpha1
+ kind: XDownstreamResource
+ metadata:
+   annotations:
+     crossplane.io/composition-resource-name: nop-resource
+   labels:
+     crossplane.io/composite: first-resource
+   name: first-resource
+   namespace: default
+ spec:
+   forProvider:
+     configData: first-value

---
~~~ XDownstreamResource/test-resource
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: nop-resource
    generateName: test-resource-
    labels:
      crossplane.io/composite: test-resource
    name: test-resource
    namespace: default
  spec:
    forProvider:
-     configData: existing-value
+     configData: modified-value

---
+++ XNopResource/first-resource
+ apiVersion: ns.diff.example.org/v1alpha1
+ kind: XNopResource
+ metadata:
+   name: first-resource
+   namespace: default
+ spec:
+   coolField: first-value

---
~~~ XNopResource/test-resource
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-resource
    namespace: default
  spec:
-   coolField: existing-value
+   coolField: modified-value

---

Summary: 2 added, 2 modified
`,
			expectedError: false,
			noColor:       true,
		},
		"SelectCompositionByDirectReference": {
			reason: "Validates composition selection by direct reference",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/functions.yaml",
				// Add multiple compositions for the same XR type
				"testdata/diff/resources/default-composition.yaml",
				"testdata/diff/resources/production-composition.yaml",
				"testdata/diff/resources/staging-composition.yaml",
			},
			inputFiles: []string{
				"testdata/diff/xr-with-composition-ref.yaml",
			},
			expectedOutput: `
+++ XDownstreamResource/test-resource
+ apiVersion: ns.nop.example.org/v1alpha1
+ kind: XDownstreamResource
+ metadata:
+   annotations:
+     crossplane.io/composition-resource-name: production-resource
+   labels:
+     crossplane.io/composite: test-resource
+   name: test-resource
+   namespace: default
+ spec:
+   forProvider:
+     configData: test-value
+     resourceTier: production

---
+++ XNopResource/test-resource
+ apiVersion: ns.diff.example.org/v1alpha1
+ kind: XNopResource
+ metadata:
+   name: test-resource
+   namespace: default
+ spec:
+   coolField: test-value
+   crossplane:
+     compositionRef:
+       name: production-composition
`,
			expectedError: false,
			noColor:       true,
		},
		"SelectCompositionByLabelSelector": {
			reason: "Validates composition selection by label selector",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/functions.yaml",
				// Add multiple compositions for the same XR type
				"testdata/diff/resources/default-composition.yaml",
				"testdata/diff/resources/production-composition.yaml",
				"testdata/diff/resources/staging-composition.yaml",
			},
			inputFiles: []string{
				"testdata/diff/xr-with-composition-selector.yaml",
			},
			expectedOutput: `
+++ XDownstreamResource/test-resource
+ apiVersion: ns.nop.example.org/v1alpha1
+ kind: XDownstreamResource
+ metadata:
+   annotations:
+     crossplane.io/composition-resource-name: staging-resource
+   labels:
+     crossplane.io/composite: test-resource
+   name: test-resource
+   namespace: default
+ spec:
+   forProvider:
+     configData: test-value
+     resourceTier: staging

---
+++ XNopResource/test-resource
+ apiVersion: ns.diff.example.org/v1alpha1
+ kind: XNopResource
+ metadata:
+   name: test-resource
+   namespace: default
+ spec:
+   coolField: test-value
+   crossplane:
+     compositionSelector:
+       matchLabels:
+         environment: staging
+         provider: aws

`,
			expectedError: false,
			noColor:       true,
		},
		"AmbiguousCompositionSelection": {
			reason: "Validates error on ambiguous composition selection",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/functions.yaml",
				// Add multiple compositions for the same XR type
				"testdata/diff/resources/default-composition.yaml",
				"testdata/diff/resources/production-composition.yaml",
				"testdata/diff/resources/staging-composition.yaml",
			},
			inputFiles: []string{
				"testdata/diff/xr-with-ambiguous-selector.yaml",
			},
			expectedError:         true,
			expectedErrorContains: "ambiguous composition selection: multiple compositions match",
			noColor:               true,
		},
		"NewClaimShowsDiff": {
			reason: "Shows diff for new claim",
			setupFiles: []string{
				"testdata/diff/resources/existing-namespace.yaml",
				// Add the necessary CRDs and compositions for claim diffing
				"testdata/diff/resources/claim-xrd.yaml",
				"testdata/diff/resources/claim-composition.yaml",
				"testdata/diff/resources/claim-composition-revision.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			inputFiles: []string{"testdata/diff/new-claim.yaml"},
			expectedOutput: `
+++ NopClaim/test-claim
+ apiVersion: diff.example.org/v1alpha1
+ kind: NopClaim
+ metadata:
+   name: test-claim
+   namespace: existing-namespace
+ spec:
+   compositeDeletePolicy: Background
+   compositionRef:
+     name: claim-composition
+   compositionUpdatePolicy: Automatic
+   coolField: new-value

---
+++ XDownstreamResource/test-claim
+ apiVersion: nop.example.org/v1alpha1
+ kind: XDownstreamResource
+ metadata:
+   annotations:
+     crossplane.io/composition-resource-name: nop-resource
+   labels:
+     crossplane.io/composite: test-claim
+   name: test-claim
+ spec:
+   forProvider:
+     configData: new-value

---

Summary: 2 added`,
			expectedError: false,
			noColor:       true,
		},
		"ModifiedClaimShowsDiff": {
			reason: "Shows diff for modified claim",
			setupFiles: []string{
				"testdata/diff/resources/existing-namespace.yaml",
				// Add necessary CRDs and composition
				"testdata/diff/resources/claim-xrd.yaml",
				"testdata/diff/resources/claim-composition.yaml",
				"testdata/diff/resources/claim-composition-revision.yaml",
				"testdata/diff/resources/functions.yaml",
				// Add existing resources for comparison
				"testdata/diff/resources/existing-claim.yaml",
				"testdata/diff/resources/existing-claim-downstream-resource.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-claim.yaml"},
			expectedOutput: `
~~~ NopClaim/test-claim
  apiVersion: diff.example.org/v1alpha1
  kind: NopClaim
  metadata:
    name: test-claim
    namespace: existing-namespace
  spec:
    compositeDeletePolicy: Background
    compositionRef:
      name: claim-composition
    compositionUpdatePolicy: Automatic
-   coolField: existing-value
+   coolField: modified-value

---
~~~ XDownstreamResource/test-claim-82crv
  apiVersion: nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: nop-resource
    generateName: test-claim-82crv-
    labels:
      crossplane.io/claim-name: test-claim
      crossplane.io/claim-namespace: existing-namespace
      crossplane.io/composite: test-claim-82crv
    name: test-claim-82crv
  spec:
    forProvider:
-     configData: existing-value
+     configData: modified-value

---

Summary: 2 modified`,
			expectedError: false,
			noColor:       true,
		},
		"XRDDefaultsAppliedBeforeRendering": {
			reason:     "Validates that XRD defaults are applied to XR before rendering",
			inputFiles: []string{"testdata/diff/xr-with-missing-defaults.yaml"},
			setupFiles: []string{
				"testdata/diff/resources/xrd-with-defaults.yaml",
				"testdata/diff/resources/composition-with-defaults.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			expectedOutput: strings.Join([]string{
				`+++ XTestDefaultResource/test-resource-with-defaults
+ apiVersion: ns.diff.example.org/v1alpha1
+ kind: XTestDefaultResource
+ metadata:
+   name: test-resource-with-defaults
+   namespace: default
+ spec:
+   region: us-east-1
+   settings:
+     enabled: true
+     retries: 3
+     timeout: 30
+   size: large
+   tags:
+     environment: development

---

Summary: 1 added`,
			}, ""),
			expectedError: false,
			noColor:       true,
		},
		"XRDDefaultsNoOverride": {
			reason:     "Validates that XRD defaults do not override user-specified values",
			inputFiles: []string{"testdata/diff/xr-with-overridden-defaults.yaml"},
			setupFiles: []string{
				"testdata/diff/resources/xrd-with-defaults.yaml",
				"testdata/diff/resources/composition-with-defaults.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			expectedOutput: strings.Join([]string{
				`+++ XTestDefaultResource/test-resource-with-overrides
+ apiVersion: ns.diff.example.org/v1alpha1
+ kind: XTestDefaultResource
+ metadata:
+   name: test-resource-with-overrides
+   namespace: default
+ spec:
+   region: us-west-2
+   settings:
+     enabled: false
+     retries: 3
+     timeout: 60
+   size: xlarge
+   tags:
+     environment: production
+     team: platform

---

Summary: 1 added`,
			}, ""),
			expectedError: false,
			noColor:       true,
		},
		"ConcurrentRenderingMultipleXRs": {
			reason: "Validates concurrent rendering with multiple functions and XRs from directory",
			// This test reproduces issue #59 - concurrent function startup failures
			// when processing multiple XR files from a directory
			inputFiles: []string{
				"testdata/diff/concurrent-xrs", // Pass the directory containing all XR files
			},
			setupFiles: []string{
				"testdata/diff/resources/xrd-concurrent.yaml",
				"testdata/diff/resources/composition-multi-functions.yaml",
				"testdata/diff/resources/composition-revision-multi-functions.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			// We expect successful processing of all 5 XRs
			// Each XR should produce 3 base resources + 2 additional resources = 5 resources per XR
			// Plus the XR itself = 6 additions per XR
			// Total: 5 XRs * 6 additions = 30 additions
			expectedOutput: "Summary: 30 added",
			expectedError:  false,
			noColor:        true,
		},
		"NewNestedXRCreatesChildren": {
			reason: "Validates that new nested XR creates child XR and downstream resources",
			setupFiles: []string{
				// XRDs for parent and child
				"testdata/diff/resources/nested/parent-xrd.yaml",
				"testdata/diff/resources/nested/child-xrd.yaml",
				// Compositions for parent and child
				"testdata/diff/resources/nested/parent-composition.yaml",
				"testdata/diff/resources/nested/child-composition.yaml",
				// XRD for downstream managed resource
				"testdata/diff/resources/xdownstreamenvresource-xrd.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			inputFiles: []string{"testdata/diff/new-nested-xr.yaml"},
			expectedOutput: `
+++ XChildResource/test-parent-child
+ apiVersion: ns.nested.example.org/v1alpha1
+ kind: XChildResource
+ metadata:
+   annotations:
+     crossplane.io/composition-resource-name: child-xr
+   labels:
+     crossplane.io/composite: test-parent
+   name: test-parent-child
+   namespace: default
+ spec:
+   childField: parent-value

---
+++ XDownstreamResource/test-parent-child-managed
+ apiVersion: ns.nop.example.org/v1alpha1
+ kind: XDownstreamResource
+ metadata:
+   annotations:
+     crossplane.io/composition-resource-name: managed-resource
+   labels:
+     crossplane.io/composite: test-parent-child
+   name: test-parent-child-managed
+   namespace: default
+ spec:
+   forProvider:
+     configData: parent-value

---
+++ XParentResource/test-parent
+ apiVersion: ns.nested.example.org/v1alpha1
+ kind: XParentResource
+ metadata:
+   name: test-parent
+   namespace: default
+ spec:
+   parentField: parent-value

---

Summary: 3 added`,
			expectedError: false,
			noColor:       true,
		},
		"ModifiedNestedXRPropagatesChanges": {
			reason: "Validates that modified nested XR propagates changes through child XR to downstream resources",
			setupFiles: []string{
				// XRDs for parent and child
				"testdata/diff/resources/nested/parent-xrd.yaml",
				"testdata/diff/resources/nested/child-xrd.yaml",
				// Compositions for parent and child
				"testdata/diff/resources/nested/parent-composition.yaml",
				"testdata/diff/resources/nested/parent-composition-revision.yaml",
				"testdata/diff/resources/nested/child-composition.yaml",
				"testdata/diff/resources/nested/child-composition-revision.yaml",
				// XRD for downstream managed resource
				"testdata/diff/resources/xdownstreamenvresource-xrd.yaml",
				"testdata/diff/resources/functions.yaml",
				// Existing resources
				"testdata/diff/resources/nested/existing-parent-xr.yaml",
				"testdata/diff/resources/nested/existing-child-xr.yaml",
				"testdata/diff/resources/nested/existing-managed-resource.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-nested-xr.yaml"},
			expectedOutput: `
~~~ XChildResource/test-parent-child
  apiVersion: ns.nested.example.org/v1alpha1
  kind: XChildResource
  metadata:
+   annotations:
+     crossplane.io/composition-resource-name: child-xr
+   labels:
+     crossplane.io/composite: test-parent
    name: test-parent-child
    namespace: default
  spec:
-   childField: existing-value
+   childField: modified-value

---
~~~ XDownstreamResource/test-parent-child-managed
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
-     gotemplating.fn.crossplane.io/composition-resource-name: managed-resource
+     crossplane.io/composition-resource-name: managed-resource
+     gotemplating.fn.crossplane.io/composition-resource-name: managed-resource
+   labels:
+     crossplane.io/composite: test-parent-child
    name: test-parent-child-managed
    namespace: default
  spec:
    forProvider:
-     configData: existing-value
+     configData: modified-value
  

---
~~~ XParentResource/test-parent
  apiVersion: ns.nested.example.org/v1alpha1
  kind: XParentResource
  metadata:
    name: test-parent
    namespace: default
  spec:
-   parentField: existing-value
+   parentField: modified-value

---

Summary: 3 modified`,
			expectedError: false,
			noColor:       true,
		},
		// Composition Revision tests for v2 XRDs
		"V2ManualPolicyPinnedRevision": {
			reason: "Validates v2 XR with Manual update policy stays on pinned revision",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition-revision-v1.yaml",
				"testdata/diff/resources/composition-revision-v2.yaml",
				"testdata/diff/resources/composition-v2.yaml", // Current composition is v2
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/existing-xr-manual-v1.yaml",
				"testdata/diff/resources/existing-downstream-manual-v1.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-xr-manual-v1.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/test-manual-v1
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: test-manual-v1
    name: test-manual-v1
    namespace: default
  spec:
    forProvider:
-     configData: v1-existing-value
+     configData: v1-modified-value

---
~~~ XNopResource/test-manual-v1
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-manual-v1
    namespace: default
  spec:
-   coolField: existing-value
+   coolField: modified-value
    crossplane:
      compositionRef:
        name: xnopresources.diff.example.org
      compositionRevisionRef:
        name: xnopresources.diff.example.org-abc123
      compositionUpdatePolicy: Manual

---
`,
			expectedError: false,
			noColor:       true,
		},
		"V2AutomaticPolicyLatestRevision": {
			reason: "Validates v2 XR with Automatic update policy uses latest revision",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition-revision-v1.yaml",
				"testdata/diff/resources/composition-revision-v2.yaml",
				"testdata/diff/resources/composition-v2.yaml", // Current composition is v2
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/existing-xr-automatic.yaml",         // Still on v1
				"testdata/diff/resources/existing-downstream-automatic.yaml", // v1 data
			},
			inputFiles: []string{"testdata/diff/modified-xr-automatic.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/test-automatic
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: test-automatic
    name: test-automatic
    namespace: default
  spec:
    forProvider:
-     configData: v1-existing-value
+     configData: v2-modified-value

---
~~~ XNopResource/test-automatic
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-automatic
    namespace: default
  spec:
-   coolField: existing-value
+   coolField: modified-value
    crossplane:
      compositionRef:
        name: xnopresources.diff.example.org
      compositionRevisionRef:
        name: xnopresources.diff.example.org-abc123
      compositionUpdatePolicy: Automatic

---
`,
			expectedError: false,
			noColor:       true,
		},
		"V2ManualRevisionUpgradeDiff": {
			reason: "Validates v2 XR changing revision in Manual mode shows upgrade diff",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition-revision-v1.yaml",
				"testdata/diff/resources/composition-revision-v2.yaml",
				"testdata/diff/resources/composition-v2.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/existing-xr-manual-v1.yaml",
				"testdata/diff/resources/existing-downstream-manual-v1.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-xr-manual-upgrade-to-v2.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/test-manual-v1
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: test-manual-v1
    name: test-manual-v1
    namespace: default
  spec:
    forProvider:
-     configData: v1-existing-value
+     configData: v2-modified-value

---
~~~ XNopResource/test-manual-v1
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-manual-v1
    namespace: default
  spec:
-   coolField: existing-value
+   coolField: modified-value
    crossplane:
      compositionRef:
        name: xnopresources.diff.example.org
      compositionRevisionRef:
-       name: xnopresources.diff.example.org-abc123
+       name: xnopresources.diff.example.org-def456
      compositionUpdatePolicy: Manual

---
`,
			expectedError: false,
			noColor:       true,
		},
		"CompositionRevisionUpgradesResourceAPIVersion": {
			// NOTE: This test validates that resources are correctly matched across API version changes,
			// avoiding delete/recreate. The composition template changes from v1beta1 to v1beta2, but
			// Kubernetes automatically converts resources between served API versions. When we query for
			// the v1beta2 resource, Kubernetes finds the v1beta1 resource and returns it auto-converted
			// to v1beta2. From Kubernetes' perspective, the resource exists as both versions simultaneously,
			// so there's no apiVersion field change to show in the diff. The important thing is that the
			// resource is matched (shown as ~~~, not ---/+++), preventing delete/recreate operations.
			reason: "Validates XR upgrading composition revision that changes resource API version shows as update not remove/add",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				// NOTE: xapimigrate CRD is auto-loaded from testdata/diff/crds/
				// We don't include xapimigrate-xrd.yaml because XApiMigrateResource is a regular
				// managed resource (not a composite), and including the XRD would make it composite.
				"testdata/diff/resources/api-version-composition-revision-v1.yaml",
				"testdata/diff/resources/api-version-composition-revision-v2.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/existing-api-version-xr-rev1.yaml",
				"testdata/diff/resources/existing-api-version-downstream-v1beta1.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-api-version-xr-rev2.yaml"},
			expectedOutput: `
~~~ XApiMigrateResource/test-api-version-xr-api-resource
  apiVersion: diff.example.org/v1beta2
  kind: XApiMigrateResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: api-migrate-resource
      gotemplating.fn.crossplane.io/composition-resource-name: api-migrate-resource
    labels:
      crossplane.io/composite: test-api-version-xr
    name: test-api-version-xr-api-resource
    namespace: default
  spec:
    forProvider:
      configData: test-value

---
~~~ XNopResource/test-api-version-xr
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-api-version-xr
    namespace: default
  spec:
    coolField: test-value
    crossplane:
      compositionRef:
        name: xapimigrateresources.example.org
      compositionRevisionRef:
-       name: xapimigrateresources.example.org-v1
+       name: xapimigrateresources.example.org-v2
      compositionUpdatePolicy: Manual

---
`,
			expectedError: false,
			noColor:       true,
		},
		"V2SwitchManualToAutomatic": {
			reason: "Validates v2 XR switching from Manual to Automatic mode uses latest revision",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition-revision-v1.yaml",
				"testdata/diff/resources/composition-revision-v2.yaml",
				"testdata/diff/resources/composition-v2.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/existing-xr-manual-v1.yaml",
				"testdata/diff/resources/existing-downstream-manual-v1.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-xr-switch-to-automatic.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/test-manual-v1
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: test-manual-v1
    name: test-manual-v1
    namespace: default
  spec:
    forProvider:
-     configData: v1-existing-value
+     configData: v2-modified-value

---
~~~ XNopResource/test-manual-v1
  apiVersion: ns.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-manual-v1
    namespace: default
  spec:
-   coolField: existing-value
+   coolField: modified-value
    crossplane:
      compositionRef:
        name: xnopresources.diff.example.org
      compositionRevisionRef:
        name: xnopresources.diff.example.org-abc123
-     compositionUpdatePolicy: Manual
+     compositionUpdatePolicy: Automatic

---
`,
			expectedError: false,
			noColor:       true,
		},
		"V2NetNewManualNoRevRef": {
			reason: "Validates v2 net new XR with Manual policy but no revision ref uses latest revision",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition-revision-v1.yaml",
				"testdata/diff/resources/composition-revision-v2.yaml",
				"testdata/diff/resources/composition-v2.yaml", // Current composition is v2
				"testdata/diff/resources/functions.yaml",
			},
			inputFiles: []string{"testdata/diff/new-xr-manual-no-ref.yaml"},
			expectedOutput: `
+++ XDownstreamResource/test-manual-no-ref
+ apiVersion: ns.nop.example.org/v1alpha1
+ kind: XDownstreamResource
+ metadata:
+   annotations:
+     crossplane.io/composition-resource-name: nop-resource
+   labels:
+     crossplane.io/composite: test-manual-no-ref
+   name: test-manual-no-ref
+   namespace: default
+ spec:
+   forProvider:
+     configData: v2-new-value

---
+++ XNopResource/test-manual-no-ref
+ apiVersion: ns.diff.example.org/v1alpha1
+ kind: XNopResource
+ metadata:
+   name: test-manual-no-ref
+   namespace: default
+ spec:
+   coolField: new-value
+   crossplane:
+     compositionRef:
+       name: xnopresources.diff.example.org
+     compositionUpdatePolicy: Manual

---

Summary: 2 added`,
			expectedError: false,
			noColor:       true,
		},
		// Composition Revision tests for v1 XRDs (Crossplane 1.20 compatibility)
		"V1ManualRevisionUpgradeDiff": {
			reason:        "Validates v1 XR with Manual update policy changing revision shows upgrade diff",
			xrdAPIVersion: V1,
			setupFiles: []string{
				"testdata/diff/resources/legacy-xrd.yaml",
				"testdata/diff/resources/legacy-composition-revision-v1.yaml",
				"testdata/diff/resources/legacy-composition-revision-v2.yaml",
				"testdata/diff/resources/legacy-composition-v2.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/existing-legacy-xr-manual-v1.yaml",
				"testdata/diff/resources/existing-legacy-downstream-manual-v1.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-legacy-xr-manual-upgrade-to-v2.yaml"},
			expectedOutput: `
~~~ XDownstreamResource/test-legacy-manual-v1
  apiVersion: legacycluster.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: test-legacy-manual-v1
    name: test-legacy-manual-v1
  spec:
    forProvider:
-     configData: v1-existing-value
+     configData: v2-modified-value

---
~~~ XNopResource/test-legacy-manual-v1
  apiVersion: legacycluster.diff.example.org/v1alpha1
  kind: XNopResource
  metadata:
    name: test-legacy-manual-v1
  spec:
    compositionRef:
      name: xlegacynopresources.diff.example.org
    compositionRevisionRef:
-     name: xlegacynopresources.diff.example.org-abc123
-   compositionUpdatePolicy: Manual
-   coolField: existing-value
+     name: xlegacynopresources.diff.example.org-def456
+   compositionUpdatePolicy: Manual
+   coolField: modified-value
  
  
  

---
`,
			expectedError: false,
			noColor:       true,
		},
		"ModifiedClaimWithNestedXRsShowsDiff": {
			reason: "Validates that modified Claims with nested XRs show proper diff (3 modified resources)",
			setupFiles: []string{
				"testdata/diff/resources/existing-namespace.yaml",
				// NOTE: CRDs for parent/child Claims/XRs are auto-loaded from testdata/diff/crds/
				// XRDs for parent and child Claims
				"testdata/diff/resources/claim-nested/parent-definition.yaml",
				"testdata/diff/resources/claim-nested/child-definition.yaml",
				// Compositions for parent and child
				"testdata/diff/resources/claim-nested/parent-composition.yaml",
				"testdata/diff/resources/claim-nested/child-composition.yaml",
				"testdata/diff/resources/functions.yaml",
				// Claim is set up separately (not via owner refs - it uses spec.resourceRef to link to backing XR)
				"testdata/diff/resources/claim-nested/existing-claim.yaml",
			},
			// Owner ref hierarchy matches Crossplane's actual ownership model:
			// - Backing XR is the root of the owner ref tree (no owner refs)
			// - Nested XR has owner ref to backing XR
			// - Managed resource has owner ref to nested XR
			// Note: Claim is NOT in this hierarchy - it links to backing XR via spec.resourceRef, not owner refs
			crossplaneManagedResources: []HierarchicalOwnershipRelation{
				{
					// Backing XR is the root of owner ref tree
					OwnerFile: "testdata/diff/resources/claim-nested/existing-parent-xr.yaml",
					OwnedFiles: map[string]*HierarchicalOwnershipRelation{
						// Nested XR owned by backing XR
						"testdata/diff/resources/claim-nested/existing-child-xr.yaml": {
							// Managed resource owned by nested XR
							OwnedFiles: map[string]*HierarchicalOwnershipRelation{
								"testdata/diff/resources/claim-nested/existing-managed-resource.yaml": nil,
							},
						},
					},
				},
			},
			inputFiles: []string{"testdata/diff/modified-claim-nested.yaml"},
			expectedOutput: `
~~~ ClusterNopResource/existing-parent-claim-82crv-nop
  apiVersion: nop.crossplane.io/v1alpha1
  kind: ClusterNopResource
  metadata:
    annotations:
-     child-field: existing-parent-value
+     child-field: modified-parent-value
      crossplane.io/composition-resource-name: nop-resource
    generateName: existing-parent-claim-82crv-
    labels:
      crossplane.io/claim-name: existing-parent-claim
      crossplane.io/claim-namespace: default
      crossplane.io/composite: existing-parent-claim-82crv
    name: existing-parent-claim-82crv-nop
  spec:
    forProvider:
      conditionAfter:
      - conditionStatus: "True"
        conditionType: Ready
        time: 0s

---
~~~ ParentNopClaim/existing-parent-claim
  apiVersion: claimnested.diff.example.org/v1alpha1
  kind: ParentNopClaim
  metadata:
+   labels:
+     new-label: added-value
    name: existing-parent-claim
    namespace: default
  spec:
    compositeDeletePolicy: Background
    compositionRef:
      name: parent-nop-claim-composition
    compositionUpdatePolicy: Automatic
-   parentField: existing-parent-value
+   parentField: modified-parent-value
    resourceRef:
      apiVersion: claimnested.diff.example.org/v1alpha1
      kind: XParentNopClaim
      name: existing-parent-claim-82crv

---
~~~ XChildNopClaim/existing-parent-claim-82crv-child
  apiVersion: claimnested.diff.example.org/v1alpha1
  kind: XChildNopClaim
  metadata:
    annotations:
      crossplane.io/composition-resource-name: child-xr
    generateName: existing-parent-claim-82crv-
    labels:
      crossplane.io/claim-name: existing-parent-claim
      crossplane.io/claim-namespace: default
      crossplane.io/composite: existing-parent-claim-82crv
    name: existing-parent-claim-82crv-child
  spec:
-   childField: existing-parent-value
+   childField: modified-parent-value
    compositionRef:
      name: child-nop-claim-composition
    compositionUpdatePolicy: Automatic

---

Summary: 3 modified`,
			xrdAPIVersion: V1, // Use V1 style resourceRefs since XRDs have claims
			expectedError: false,
			noColor:       true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			runIntegrationTest(t, XRDiffTest, scheme, tt)
		})
	}
}

// TestCompDiffIntegration runs an integration test for the composition diff command.
func TestCompDiffIntegration(t *testing.T) {
	t.Parallel()

	// Set up logger for controller-runtime (global setup, once per test function)
	tu.SetupKubeTestLogger(t)

	scheme := createTestScheme()

	tests := map[string]IntegrationTestCase{
		"CompositionChangeImpactsXRs": {
			reason: "Validates composition change impacts existing XRs",
			// Set up existing XRs that use the original composition
			setupFiles: []string{
				"testdata/comp/resources/xrd.yaml",
				"testdata/comp/resources/original-composition.yaml",
				"testdata/comp/resources/functions.yaml",
				// Add existing XRs that use the composition
				"testdata/comp/resources/existing-xr-1.yaml",
				"testdata/comp/resources/existing-downstream-1.yaml",
				"testdata/comp/resources/existing-xr-2.yaml",
				"testdata/comp/resources/existing-downstream-2.yaml",
			},
			// New composition files that will be diffed
			inputFiles: []string{"testdata/comp/updated-composition.yaml"},
			namespace:  "default",
			expectedOutput: `
=== Composition Changes ===

~~~ Composition/xnopresources.diff.example.org
  apiVersion: apiextensions.crossplane.io/v1
  kind: Composition
  metadata:
    name: xnopresources.diff.example.org
  spec:
    compositeTypeRef:
      apiVersion: ns.diff.example.org/v1alpha1
      kind: XNopResource
    mode: Pipeline
    pipeline:
    - functionRef:
        name: function-go-templating
      input:
        apiVersion: template.fn.crossplane.io/v1beta1
        inline:
          template: |
            apiVersion: ns.nop.example.org/v1alpha1
            kind: XDownstreamResource
            metadata:
              name: {{ .observed.composite.resource.metadata.name }}
              namespace: {{ .observed.composite.resource.metadata.namespace }}
              annotations:
                gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
            spec:
              forProvider:
-               configData: {{ .observed.composite.resource.spec.coolField }}
-               resourceTier: basic
+               configData: updated-{{ .observed.composite.resource.spec.coolField }}
+               resourceTier: premium
        kind: GoTemplate
        source: Inline
      step: generate-resources
    - functionRef:
        name: function-auto-ready
      step: automatically-detect-ready-composed-resources

---

Summary: 1 modified

=== Affected Composite Resources ===

  ⚠ XNopResource/another-resource (namespace: default)
  ⚠ XNopResource/test-resource (namespace: default)

Summary: 2 resources with changes

=== Impact Analysis ===

~~~ XDownstreamResource/another-resource
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: another-resource
    name: another-resource
    namespace: default
  spec:
    forProvider:
-     configData: another-existing-value
-     resourceTier: basic
+     configData: updated-another-existing-value
+     resourceTier: premium

---
~~~ XDownstreamResource/test-resource
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: test-resource
    name: test-resource
    namespace: default
  spec:
    forProvider:
-     configData: existing-value
-     resourceTier: basic
+     configData: updated-existing-value
+     resourceTier: premium

---

Summary: 2 modified`,
			expectedError: false,
			noColor:       true,
		},
		"CompositionDiffIgnorePaths": {
			reason: "Validates that ArgoCD annotations are ignored in composition diffs",
			setupFiles: []string{
				"testdata/comp/resources/xrd.yaml",
				"testdata/comp/resources/original-composition.yaml",
				"testdata/comp/resources/functions.yaml",
				// Add existing XR with ArgoCD annotations
				"testdata/comp/resources/existing-xr-with-argocd.yaml",
				"testdata/comp/resources/existing-downstream-with-argocd.yaml",
			},
			inputFiles: []string{"testdata/comp/composition-no-changes.yaml"},
			namespace:  "default",
			ignorePaths: []string{
				"metadata.annotations[argocd.argoproj.io/tracking-id]",
				"metadata.labels[argocd.argoproj.io/instance]",
			},
			expectedOutput: `
=== Composition Changes ===

No changes detected in composition xnopresources.diff.example.org

=== Affected Composite Resources ===

  ✓ XNopResource/test-resource (namespace: default)

Summary: 1 resource unchanged

=== Impact Analysis ===

All composite resources are up-to-date. No downstream resource changes detected.

`,
			expectedError: false,
			noColor:       true,
		},
		"CompositionDiffCustomNamespace": {
			reason: "Validates composition diff with custom namespace",
			// Set up existing XRs in both custom and default namespaces to validate filtering
			setupFiles: []string{
				"testdata/comp/resources/xrd.yaml",
				"testdata/comp/resources/original-composition.yaml",
				"testdata/comp/resources/functions.yaml",
				// Create the custom namespace first
				"testdata/comp/resources/custom-namespace.yaml",
				// Add existing XRs in custom namespace (should be included in output)
				"testdata/comp/resources/existing-custom-ns-xr.yaml",
				"testdata/comp/resources/existing-custom-ns-downstream.yaml",
				// Add existing XRs in default namespace (should be filtered out)
				"testdata/comp/resources/existing-xr-1.yaml",
				"testdata/comp/resources/existing-downstream-1.yaml",
			},
			// New composition files that will be diffed
			inputFiles: []string{"testdata/comp/updated-composition.yaml"},
			namespace:  "custom-namespace",
			// Expected output should only show custom-namespace resources, NOT default namespace resources
			// This validates that namespace filtering works correctly
			expectedOutput: `
=== Composition Changes ===

~~~ Composition/xnopresources.diff.example.org
  apiVersion: apiextensions.crossplane.io/v1
  kind: Composition
  metadata:
    name: xnopresources.diff.example.org
  spec:
    compositeTypeRef:
      apiVersion: ns.diff.example.org/v1alpha1
      kind: XNopResource
    mode: Pipeline
    pipeline:
    - functionRef:
        name: function-go-templating
      input:
        apiVersion: template.fn.crossplane.io/v1beta1
        inline:
          template: |
            apiVersion: ns.nop.example.org/v1alpha1
            kind: XDownstreamResource
            metadata:
              name: {{ .observed.composite.resource.metadata.name }}
              namespace: {{ .observed.composite.resource.metadata.namespace }}
              annotations:
                gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
            spec:
              forProvider:
-               configData: {{ .observed.composite.resource.spec.coolField }}
-               resourceTier: basic
+               configData: updated-{{ .observed.composite.resource.spec.coolField }}
+               resourceTier: premium
        kind: GoTemplate
        source: Inline
      step: generate-resources
    - functionRef:
        name: function-auto-ready
      step: automatically-detect-ready-composed-resources

---

Summary: 1 modified

=== Affected Composite Resources ===

  ⚠ XNopResource/custom-namespace-resource (namespace: custom-namespace)

Summary: 1 resource with changes

=== Impact Analysis ===

~~~ XDownstreamResource/custom-namespace-resource
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: custom-namespace-resource
    name: custom-namespace-resource
    namespace: custom-namespace
  spec:
    forProvider:
-     configData: custom-ns-existing-value
-     resourceTier: basic
+     configData: updated-custom-ns-existing-value
+     resourceTier: premium

---

Summary: 1 modified`,
			expectedError: false,
			noColor:       true,
		},
		"MultipleCompositionDiffImpact": {
			reason: "Validates multiple composition diff shows impact on existing XRs",
			// Set up existing XRs that use both compositions
			setupFiles: []string{
				"testdata/comp/resources/xrd.yaml",
				"testdata/comp/resources/original-composition.yaml",
				"testdata/comp/resources/original-composition-2.yaml",
				"testdata/comp/resources/functions.yaml",
				// Add existing XRs that use the compositions
				"testdata/comp/resources/existing-xr-1.yaml",
				"testdata/comp/resources/existing-downstream-1.yaml",
				"testdata/comp/resources/existing-xr-2.yaml",
				"testdata/comp/resources/existing-downstream-2.yaml",
			},
			// Multiple composition files that will be diffed
			inputFiles: []string{
				"testdata/comp/updated-composition.yaml",
				"testdata/comp/updated-composition-2.yaml",
			},
			namespace: "default",
			expectedOutput: `
=== Composition Changes ===

~~~ Composition/xnopresources.diff.example.org
  apiVersion: apiextensions.crossplane.io/v1
  kind: Composition
  metadata:
    name: xnopresources.diff.example.org
  spec:
    compositeTypeRef:
      apiVersion: ns.diff.example.org/v1alpha1
      kind: XNopResource
    mode: Pipeline
    pipeline:
    - functionRef:
        name: function-go-templating
      input:
        apiVersion: template.fn.crossplane.io/v1beta1
        inline:
          template: |
            apiVersion: ns.nop.example.org/v1alpha1
            kind: XDownstreamResource
            metadata:
              name: {{ .observed.composite.resource.metadata.name }}
              namespace: {{ .observed.composite.resource.metadata.namespace }}
              annotations:
                gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
            spec:
              forProvider:
-               configData: {{ .observed.composite.resource.spec.coolField }}
-               resourceTier: basic
+               configData: updated-{{ .observed.composite.resource.spec.coolField }}
+               resourceTier: premium
        kind: GoTemplate
        source: Inline
      step: generate-resources
    - functionRef:
        name: function-auto-ready
      step: automatically-detect-ready-composed-resources

---

Summary: 1 modified

=== Affected Composite Resources ===

  ⚠ XNopResource/another-resource (namespace: default)
  ⚠ XNopResource/test-resource (namespace: default)

Summary: 2 resources with changes

=== Impact Analysis ===

~~~ XDownstreamResource/another-resource
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: another-resource
    name: another-resource
    namespace: default
  spec:
    forProvider:
-     configData: another-existing-value
-     resourceTier: basic
+     configData: updated-another-existing-value
+     resourceTier: premium

---
~~~ XDownstreamResource/test-resource
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: test-resource
    name: test-resource
    namespace: default
  spec:
    forProvider:
-     configData: existing-value
-     resourceTier: basic
+     configData: updated-existing-value
+     resourceTier: premium

---

Summary: 2 modified

================================================================================

=== Composition Changes ===

No changes detected in composition xnopresources-v2.diff.example.org

No XRs found using composition xnopresources-v2.diff.example.org`,
			expectedError: false,
			noColor:       true,
		},
		"CompositionDiffFiltersManualXRs": {
			reason: "Validates composition diff filters Manual policy XRs by default",
			// Set up existing XRs - one with Automatic policy and one with Manual policy
			setupFiles: []string{
				"testdata/comp/resources/xrd.yaml",
				"testdata/comp/resources/original-composition.yaml",
				"testdata/comp/resources/composition-revision-v1.yaml",
				"testdata/comp/resources/functions.yaml",
				// Add existing XR with Automatic policy (should be included)
				"testdata/comp/resources/existing-xr-1.yaml",
				"testdata/comp/resources/existing-downstream-1.yaml",
				// Add existing XR with Manual policy (should be filtered out by default)
				"testdata/comp/resources/existing-xr-manual.yaml",
				"testdata/comp/resources/existing-downstream-manual.yaml",
			},
			// Updated composition
			inputFiles: []string{"testdata/comp/updated-composition.yaml"},
			namespace:  "default",
			// Expected output should only show test-resource (Automatic), NOT manual-resource (Manual)
			expectedOutput: `
=== Composition Changes ===

~~~ Composition/xnopresources.diff.example.org
  apiVersion: apiextensions.crossplane.io/v1
  kind: Composition
  metadata:
    name: xnopresources.diff.example.org
  spec:
    compositeTypeRef:
      apiVersion: ns.diff.example.org/v1alpha1
      kind: XNopResource
    mode: Pipeline
    pipeline:
    - functionRef:
        name: function-go-templating
      input:
        apiVersion: template.fn.crossplane.io/v1beta1
        inline:
          template: |
            apiVersion: ns.nop.example.org/v1alpha1
            kind: XDownstreamResource
            metadata:
              name: {{ .observed.composite.resource.metadata.name }}
              namespace: {{ .observed.composite.resource.metadata.namespace }}
              annotations:
                gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
            spec:
              forProvider:
-               configData: {{ .observed.composite.resource.spec.coolField }}
-               resourceTier: basic
+               configData: updated-{{ .observed.composite.resource.spec.coolField }}
+               resourceTier: premium
        kind: GoTemplate
        source: Inline
      step: generate-resources
    - functionRef:
        name: function-auto-ready
      step: automatically-detect-ready-composed-resources

---

Summary: 1 modified

=== Affected Composite Resources ===

  ⚠ XNopResource/test-resource (namespace: default)

Summary: 1 resource with changes

=== Impact Analysis ===

~~~ XDownstreamResource/test-resource
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: test-resource
    name: test-resource
    namespace: default
  spec:
    forProvider:
-     configData: existing-value
-     resourceTier: basic
+     configData: updated-existing-value
+     resourceTier: premium

---

Summary: 1 modified`,
			expectedError: false,
			noColor:       true,
		},
		"CompositionUpgradesResourceAPIVersion": {
			// NOTE: This test validates that resources are correctly matched across API version changes,
			// avoiding delete/recreate. The composition template changes from v1beta1 to v1beta2, but
			// Kubernetes automatically converts resources between served API versions. When we query for
			// the v1beta2 resource, Kubernetes finds the v1beta1 resource and returns it auto-converted
			// to v1beta2. From Kubernetes' perspective, the resource exists as both versions simultaneously,
			// so there's no apiVersion field change to show in the diff. The important thing is that the
			// resource is matched (shown as ~~~, not ---/+++), preventing delete/recreate operations.
			// The composition diff itself WILL show the template change from v1beta1 to v1beta2.
			reason: "Validates composition upgrade that changes resource API version shows as update not remove/add",
			setupFiles: []string{
				"testdata/comp/resources/xrd.yaml",
				// NOTE: xapimigrate CRD is auto-loaded from testdata/comp/crds/
				// We don't include xapimigrate-xrd.yaml because XApiMigrateResource is a regular
				// managed resource (not a composite), and including the XRD would make it composite,
				// causing infinite recursion.
				"testdata/comp/resources/api-version-original-composition.yaml",
				"testdata/comp/resources/functions.yaml",
				"testdata/comp/resources/existing-api-version-xr.yaml",
				"testdata/comp/resources/existing-api-version-downstream-v1beta1.yaml",
			},
			inputFiles: []string{"testdata/comp/api-version-updated-composition.yaml"},
			namespace:  "default",
			expectedOutput: `
=== Composition Changes ===

~~~ Composition/xapimigrateresources.example.org
  apiVersion: apiextensions.crossplane.io/v1
  kind: Composition
  metadata:
    name: xapimigrateresources.example.org
  spec:
    compositeTypeRef:
      apiVersion: ns.diff.example.org/v1alpha1
      kind: XNopResource
    mode: Pipeline
    pipeline:
    - functionRef:
        name: function-go-templating
      input:
        apiVersion: template.fn.crossplane.io/v1beta1
        inline:
          template: |
-           apiVersion: comp.example.org/v1beta1
+           apiVersion: comp.example.org/v1beta2
            kind: XApiMigrateResource
            metadata:
              name: {{ .observed.composite.resource.metadata.name }}-api-resource
              namespace: {{ .observed.composite.resource.metadata.namespace }}
              annotations:
                gotemplating.fn.crossplane.io/composition-resource-name: api-migrate-resource
            spec:
              forProvider:
                configData: {{ .observed.composite.resource.spec.coolField }}
        kind: GoTemplate
        source: Inline
      step: generate-api-versioned-resources
    - functionRef:
        name: function-auto-ready
      step: automatically-detect-ready-composed-resources

---

Summary: 1 modified

=== Affected Composite Resources ===

  ⚠ XNopResource/test-api-version (namespace: default)

Summary: 1 resource with changes

=== Impact Analysis ===

~~~ XApiMigrateResource/test-api-version-api-resource
  apiVersion: comp.example.org/v1beta2
  kind: XApiMigrateResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: api-migrate-resource
      gotemplating.fn.crossplane.io/composition-resource-name: api-migrate-resource
    labels:
      crossplane.io/composite: test-api-version
    name: test-api-version-api-resource
    namespace: default
  spec:
    forProvider:
      configData: test-value

---

Summary: 1 modified`,
			expectedError: false,
			noColor:       true,
		},
		"NetNewCompositionNoImpact": {
			reason: "Validates net-new composition with no downstream impact",
			// Set up cluster with existing resources but no composition that matches the new one
			setupFiles: []string{
				"testdata/comp/resources/xrd.yaml",
				"testdata/comp/resources/original-composition.yaml",
				"testdata/comp/resources/functions.yaml",
				// Add existing XRs that use different compositions (won't be affected)
				"testdata/comp/resources/existing-xr-1.yaml",
				"testdata/comp/resources/existing-downstream-1.yaml",
			},
			// Net-new composition file that doesn't exist in cluster and targets different XR type
			inputFiles: []string{"testdata/comp/net-new-composition.yaml"},
			namespace:  "default",
			expectedOutput: `
=== Composition Changes ===

+++ Composition/xnewresources.diff.example.org
+ apiVersion: apiextensions.crossplane.io/v1
+ kind: Composition
+ metadata:
+   labels:
+     environment: staging
+     provider: aws
+   name: xnewresources.diff.example.org
+ spec:
+   compositeTypeRef:
+     apiVersion: staging.diff.example.org/v1alpha1
+     kind: XNewResource
+   mode: Pipeline
+   pipeline:
+   - functionRef:
+       name: function-go-templating
+     input:
+       apiVersion: template.fn.crossplane.io/v1beta1
+       inline:
+         template: |
+           apiVersion: staging.nop.example.org/v1alpha1
+           kind: XStagingResource
+           metadata:
+             name: {{ .observed.composite.resource.metadata.name }}
+             namespace: {{ .observed.composite.resource.metadata.namespace }}
+             annotations:
+               gotemplating.fn.crossplane.io/composition-resource-name: staging-resource
+           spec:
+             forProvider:
+               environment: staging
+               configData: new-{{ .observed.composite.resource.spec.newField }}
+               resourceTier: premium
+       kind: GoTemplate
+       source: Inline
+     step: generate-new-resources
+   - functionRef:
+       name: function-auto-ready
+     step: automatically-detect-ready-composed-resources

---

Summary: 1 added

No XRs found using composition xnewresources.diff.example.org`,
			expectedError: false,
			noColor:       true,
		},
		"CompositionChangeWithUnchangedDownstreamResources": {
			reason: "Validates status indicators for unchanged downstream resources",
			// Set up existing XRs that use the original composition
			setupFiles: []string{
				"testdata/comp/resources/xrd.yaml",
				"testdata/comp/resources/status-indicator-composition.yaml",
				"testdata/comp/resources/functions.yaml",
				// Add existing XRs that use the composition
				"testdata/comp/resources/status-xr-1.yaml",
				"testdata/comp/resources/status-downstream-1.yaml",
				"testdata/comp/resources/status-xr-2.yaml",
				"testdata/comp/resources/status-downstream-2.yaml",
			},
			// Updated composition with only metadata change (no downstream impact)
			inputFiles: []string{"testdata/comp/status-indicator-updated-composition.yaml"},
			namespace:  "default",
			expectedOutput: `
=== Composition Changes ===

~~~ Composition/xstatus.diff.example.org
  apiVersion: apiextensions.crossplane.io/v1
  kind: Composition
  metadata:
+   labels:
+     environment: production
    name: xstatus.diff.example.org
  spec:
    compositeTypeRef:
      apiVersion: ns.diff.example.org/v1alpha1
      kind: XNopResource
    mode: Pipeline
    pipeline:
    - functionRef:
        name: function-go-templating
      input:
        apiVersion: template.fn.crossplane.io/v1beta1
        inline:
          template: |
            apiVersion: ns.nop.example.org/v1alpha1
            kind: XDownstreamResource
            metadata:
              name: {{ .observed.composite.resource.metadata.name }}
              namespace: {{ .observed.composite.resource.metadata.namespace }}
              annotations:
                gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
            spec:
              forProvider:
                configData: stable-value
                resourceTier: standard
        kind: GoTemplate
        source: Inline
      step: generate-resources
    - functionRef:
        name: function-auto-ready
      step: automatically-detect-ready-composed-resources

---

Summary: 1 modified

=== Affected Composite Resources ===

  ✓ XNopResource/status-test-xr-1 (namespace: default)
  ✓ XNopResource/status-test-xr-2 (namespace: default)

Summary: 2 resources unchanged

=== Impact Analysis ===

All composite resources are up-to-date. No downstream resource changes detected.`,
			expectedError: false,
			noColor:       true,
		},
		"CompositionChangeWithMixedStatusAndColors": {
			reason: "Validates status indicators with colorization for mixed changed/unchanged XRs",
			setupFiles: []string{
				"testdata/comp/resources/xrd.yaml",
				"testdata/comp/resources/mixed-status-composition.yaml",
				"testdata/comp/resources/functions.yaml",
				// XR 1 with downstream resources (standard tier - will change)
				"testdata/comp/resources/mixed-xr-1.yaml",
				"testdata/comp/resources/mixed-downstream-db-1.yaml",
				"testdata/comp/resources/mixed-downstream-storage-1.yaml",
				"testdata/comp/resources/mixed-downstream-network-1.yaml",
				// XR 2 with downstream resources (standard tier - will change)
				"testdata/comp/resources/mixed-xr-2.yaml",
				"testdata/comp/resources/mixed-downstream-db-2.yaml",
				"testdata/comp/resources/mixed-downstream-storage-2.yaml",
				"testdata/comp/resources/mixed-downstream-network-2.yaml",
				// XR 3 with downstream resources (already premium tier - no change)
				"testdata/comp/resources/mixed-xr-3.yaml",
				"testdata/comp/resources/mixed-downstream-db-3.yaml",
				"testdata/comp/resources/mixed-downstream-storage-3.yaml",
				"testdata/comp/resources/mixed-downstream-network-3.yaml",
			},
			inputFiles: []string{"testdata/comp/mixed-status-updated-composition.yaml"},
			namespace:  "default",
			expectedOutput: strings.Join([]string{
				`
=== Composition Changes ===

~~~ Composition/xmixed.diff.example.org
  apiVersion: apiextensions.crossplane.io/v1
  kind: Composition
  metadata:
    name: xmixed.diff.example.org
  spec:
    compositeTypeRef:
      apiVersion: ns.diff.example.org/v1alpha1
      kind: XNopResource
    mode: Pipeline
    pipeline:
    - functionRef:
        name: function-go-templating
      input:
        apiVersion: template.fn.crossplane.io/v1beta1
        inline:
          template: |
            ---
            apiVersion: ns.nop.example.org/v1alpha1
            kind: XDownstreamResource
            metadata:
              name: {{ .observed.composite.resource.metadata.name }}-database
              namespace: {{ .observed.composite.resource.metadata.namespace }}
              annotations:
                gotemplating.fn.crossplane.io/composition-resource-name: database
            spec:
              forProvider:
                resourceType: database
`, tu.Red(`-               tier: standard`), `
`, tu.Green(`+               tier: premium`), `
`, tu.Green(`+               backupEnabled: true`), `
            ---
            apiVersion: ns.nop.example.org/v1alpha1
            kind: XDownstreamResource
            metadata:
              name: {{ .observed.composite.resource.metadata.name }}-storage
              namespace: {{ .observed.composite.resource.metadata.namespace }}
              annotations:
                gotemplating.fn.crossplane.io/composition-resource-name: storage
            spec:
              forProvider:
                resourceType: storage
                tier: standard
            ---
            apiVersion: ns.nop.example.org/v1alpha1
            kind: XDownstreamResource
            metadata:
              name: {{ .observed.composite.resource.metadata.name }}-network
              namespace: {{ .observed.composite.resource.metadata.namespace }}
              annotations:
                gotemplating.fn.crossplane.io/composition-resource-name: network
            spec:
              forProvider:
                resourceType: network
                tier: standard
        kind: GoTemplate
        source: Inline
      step: generate-resources
    - functionRef:
        name: function-auto-ready
      step: automatically-detect-ready-composed-resources

---

Summary: 1 modified

=== Affected Composite Resources ===

`, tu.Yellow(`  ⚠ XNopResource/mixed-test-xr-1 (namespace: default)`), `
`, tu.Yellow(`  ⚠ XNopResource/mixed-test-xr-2 (namespace: default)`), `
`, tu.Green(`  ✓ XNopResource/mixed-test-xr-3 (namespace: default)`), `

Summary: 2 resources with changes, 1 resource unchanged

=== Impact Analysis ===

~~~ XDownstreamResource/mixed-test-xr-1-database
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: database
      gotemplating.fn.crossplane.io/composition-resource-name: database
    labels:
      crossplane.io/composite: mixed-test-xr-1
    name: mixed-test-xr-1-database
    namespace: default
  spec:
    forProvider:
`, tu.Red(`-     resourceType: database`), `
`, tu.Red(`-     tier: standard`), `
`, tu.Green(`+     backupEnabled: true`), `
`, tu.Green(`+     resourceType: database`), `
`, tu.Green(`+     tier: premium`), `
  
  

---
~~~ XDownstreamResource/mixed-test-xr-2-database
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: database
      gotemplating.fn.crossplane.io/composition-resource-name: database
    labels:
      crossplane.io/composite: mixed-test-xr-2
    name: mixed-test-xr-2-database
    namespace: default
  spec:
    forProvider:
`, tu.Red(`-     resourceType: database`), `
`, tu.Red(`-     tier: standard`), `
`, tu.Green(`+     backupEnabled: true`), `
`, tu.Green(`+     resourceType: database`), `
`, tu.Green(`+     tier: premium`), `
  
  

---

Summary: 2 modified
`,
			}, ""),
			expectedError: false,
			noColor:       false,
		},
		"CompositionChangeImpactsClaims": {
			reason: "Validates composition change impacts existing Claims (issue #120)",
			// Set up existing Claims and their XRs that use the original composition
			setupFiles: []string{
				// XRD, composition, and functions
				"testdata/comp/resources/claim-xrd.yaml",
				"testdata/comp/resources/claim-composition.yaml",
				"testdata/comp/resources/functions.yaml",
				// Test namespace
				"testdata/comp/resources/test-namespace.yaml",
				// Existing Claims and their corresponding XRs
				"testdata/comp/resources/existing-claim-1.yaml",
				"testdata/comp/resources/existing-claim-1-xr.yaml",
				"testdata/comp/resources/existing-claim-downstream-1.yaml",
				"testdata/comp/resources/existing-claim-2.yaml",
				"testdata/comp/resources/existing-claim-2-xr.yaml",
				"testdata/comp/resources/existing-claim-downstream-2.yaml",
			},
			// Updated composition that will be diffed
			inputFiles: []string{"testdata/comp/updated-claim-composition.yaml"},
			namespace:  "test-namespace",
			expectedOutput: `
=== Composition Changes ===

~~~ Composition/nopclaims.diff.example.org
  apiVersion: apiextensions.crossplane.io/v1
  kind: Composition
  metadata:
    name: nopclaims.diff.example.org
  spec:
    compositeTypeRef:
      apiVersion: diff.example.org/v1alpha1
      kind: XNop
    mode: Pipeline
    pipeline:
    - functionRef:
        name: function-go-templating
      input:
        apiVersion: template.fn.crossplane.io/v1beta1
        inline:
          template: |
            apiVersion: nop.example.org/v1alpha1
            kind: XDownstreamResource
            metadata:
              annotations:
                gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
              name: {{ .observed.composite.resource.metadata.name }}
            spec:
              forProvider:
-               configData: {{ .observed.composite.resource.spec.coolField }}
-               resourceTier: basic
+               configData: updated-{{ .observed.composite.resource.spec.coolField }}
+               resourceTier: premium
        kind: GoTemplate
        source: Inline
      step: generate-resources
    - functionRef:
        name: function-auto-ready
      step: automatically-detect-ready-composed-resources

---

Summary: 1 modified

=== Affected Composite Resources ===

  ⚠ NopClaim/test-claim-1 (namespace: test-namespace)
  ⚠ NopClaim/test-claim-2 (namespace: test-namespace)

Summary: 2 resources with changes

=== Impact Analysis ===

~~~ XDownstreamResource/test-claim-1-xr
  apiVersion: nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/claim-name: test-claim-1
      crossplane.io/claim-namespace: test-namespace
      crossplane.io/composite: test-claim-1-xr
    name: test-claim-1-xr
  spec:
    forProvider:
-     configData: claim-value-1
-     resourceTier: basic
+     configData: updated-claim-value-1
+     resourceTier: premium

---
~~~ XDownstreamResource/test-claim-2-xr
  apiVersion: nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/claim-name: test-claim-2
      crossplane.io/claim-namespace: test-namespace
      crossplane.io/composite: test-claim-2-xr
    name: test-claim-2-xr
  spec:
    forProvider:
-     configData: claim-value-2
-     resourceTier: basic
+     configData: updated-claim-value-2
+     resourceTier: premium

---

Summary: 2 modified`,
			expectedError: false,
			noColor:       true,
		},
		"SSAFieldRemovalDetection": {
			reason: "Validates that field removals are correctly detected when resources are created via Server-Side Apply with Crossplane's field manager (issue #121)",
			// Set up XRD, functions, and original composition with the removable field
			setupFiles: []string{
				"testdata/comp/resources/xrd.yaml",
				"testdata/comp/resources/functions.yaml",
				"testdata/comp/resources/field-removal/composition-with-field.yaml",
			},
			// XR and downstream resource applied via SSA with Crossplane field manager
			crossplaneManagedResources: []HierarchicalOwnershipRelation{
				{
					OwnerFile: "testdata/comp/resources/field-removal/existing-xr.yaml",
					OwnedFiles: map[string]*HierarchicalOwnershipRelation{
						"testdata/comp/resources/field-removal/existing-downstream.yaml": nil,
					},
				},
			},
			// Updated composition that removes the removableField
			inputFiles: []string{"testdata/comp/field-removal-updated-composition.yaml"},
			namespace:  "default",
			expectedOutput: `
=== Composition Changes ===

~~~ Composition/xfieldremoval.diff.example.org
  apiVersion: apiextensions.crossplane.io/v1
  kind: Composition
  metadata:
    name: xfieldremoval.diff.example.org
  spec:
    compositeTypeRef:
      apiVersion: ns.diff.example.org/v1alpha1
      kind: XNopResource
    mode: Pipeline
    pipeline:
    - functionRef:
        name: function-go-templating
      input:
        apiVersion: template.fn.crossplane.io/v1beta1
        inline:
          template: |
            apiVersion: ns.nop.example.org/v1alpha1
            kind: XDownstreamResource
            metadata:
              name: {{ .observed.composite.resource.metadata.name }}
              namespace: {{ .observed.composite.resource.metadata.namespace }}
              annotations:
                gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
            spec:
              forProvider:
                configData: {{ .observed.composite.resource.spec.coolField }}
-               removableField: will-be-removed
        kind: GoTemplate
        source: Inline
      step: generate-resources
    - functionRef:
        name: function-auto-ready
      step: automatically-detect-ready-composed-resources

---

Summary: 1 modified

=== Affected Composite Resources ===

  ⚠ XNopResource/field-removal-test (namespace: default)

Summary: 1 resource with changes

=== Impact Analysis ===

~~~ XDownstreamResource/field-removal-test
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: nop-resource
-     gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: field-removal-test
    name: field-removal-test
    namespace: default
  spec:
    forProvider:
      configData: test-value
-     removableField: will-be-removed

---

Summary: 1 modified`,
			expectedError: false,
			noColor:       true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			runIntegrationTest(t, CompositionDiffTest, scheme, tt)
		})
	}
}
