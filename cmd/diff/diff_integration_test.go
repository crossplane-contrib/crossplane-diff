package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
	setupFiles              []string
	setupFilesWithOwnerRefs []HierarchicalOwnershipRelation
	inputFiles              []string // Input files to diff (XR YAML files or Composition YAML files)
	expectedOutput          string
	expectedError           bool
	expectedErrorContains   string
	noColor                 bool
	namespace               string        // For composition tests (optional)
	xrdAPIVersion           XrdAPIVersion // For XR tests (optional)
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

// runIntegrationTest runs a common integration test for both XR and composition diff commands.
func runIntegrationTest(t *testing.T, testType DiffTestType, tests map[string]IntegrationTestCase) {
	t.Helper()
	// Create a scheme with both Kubernetes and Crossplane types
	scheme := runtime.NewScheme()

	// Register Kubernetes types
	_ = cgoscheme.AddToScheme(scheme)

	// Register Crossplane types
	_ = xpextv1.AddToScheme(scheme)
	_ = xpextv2.AddToScheme(scheme)
	_ = pkgv1.AddToScheme(scheme)
	_ = extv1.AddToScheme(scheme)

	tu.SetupKubeTestLogger(t)

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Skip test if requested
			if tt.skip {
				t.Skip(tt.skipReason)
				return
			}

			// Setup a brand new test environment for each test case
			_, thisFile, _, _ := run.Caller(0)
			thisDir := filepath.Dir(thisFile)

			crdPaths := []string{
				filepath.Join(thisDir, "..", "..", "cluster", "main", "crds"),
				filepath.Join(thisDir, "testdata", string(testType), "crds"),
			}

			testEnv := &envtest.Environment{
				CRDDirectoryPaths:     crdPaths,
				ErrorIfCRDPathMissing: true,
				Scheme:                scheme,
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

			// Apply resources with owner references
			if len(tt.setupFilesWithOwnerRefs) > 0 {
				err := applyHierarchicalOwnership(ctx, tu.TestLogger(t, false), k8sClient, xrdAPIVersion, tt.setupFilesWithOwnerRefs)
				if err != nil {
					t.Fatalf("failed to setup owner references: %v", err)
				}
			}

			// Set up the test files
			tempDir := t.TempDir()

			var testFiles []string

			// Handle any additional input files
			for i, inputFile := range tt.inputFiles {
				testFile := filepath.Join(tempDir, fmt.Sprintf("test_%d.yaml", i))

				content, err := os.ReadFile(inputFile)
				if err != nil {
					t.Fatalf("failed to read input file: %v", err)
				}

				err = os.WriteFile(testFile, content, 0o644)
				if err != nil {
					t.Fatalf("failed to write test file: %v", err)
				}

				testFiles = append(testFiles, testFile)
			}

			// Create a buffer to capture the output
			var stdout bytes.Buffer

			// Create command line args that match your pre-populated struct
			args := []string{
				fmt.Sprintf("--timeout=%s", timeout.String()),
			}

			// Add namespace if specified (for composition tests)
			if tt.namespace != "" {
				args = append(args, fmt.Sprintf("--namespace=%s", tt.namespace))
			} else if testType == XRDiffTest {
				// For XR tests, always add default namespace
				args = append(args, "--namespace=default")
			}

			// Add no-color flag if true
			if tt.noColor {
				args = append(args, "--no-color")
			}

			// Add files as positional arguments
			args = append(args, testFiles...)

			// Set up the appropriate command based on test type
			var cmd interface{}
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
		})
	}
}

// TestDiffIntegration runs an integration test for the diff command.
func TestDiffIntegration(t *testing.T) {
	tests := map[string]IntegrationTestCase{
		"New resource shows color diff": {
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
		"Automatic namespace propagation for namespaced managed resources": {
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
		"Modified resource shows color diff": {
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition.yaml",
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
`,
			expectedError: false,
		},
		"Modified XR that creates new downstream resource shows color diff": {
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition.yaml",
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
`,
			expectedError: false,
		},
		"EnvironmentConfig (v1beta1) incorporation in diff": {
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
		"Diff with external resource dependencies via fn-external-resources": {
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
		"Diff with templated ExtraResources embedded in go-templating function": {
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
		"Cross-namespace resource dependencies via fn-external-resources": {
			// Skip this test until function-extra-resources supports the namespace field
			// This test documents the intended cross-namespace functionality and will work
			// once function-extra-resources is updated to support Crossplane v2 namespace specification
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
+   coolField: modified-cross-ns-value
-   environment: staging
+   environment: production

---
`,
			expectedError: false,
			noColor:       true,
			skip:          true,
			skipReason:    "function-extra-resources does not yet support namespace field for cross-namespace resource access",
		},
		"Resource removal detection with hierarchy (v1 style resourceRefs; cluster scoped downstreams)": {
			xrdAPIVersion: V1,
			setupFilesWithOwnerRefs: []HierarchicalOwnershipRelation{
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
`,
			expectedError: false,
			noColor:       true,
		},
		"Resource removal detection with hierarchy (v2 style resourceRefs; namespaced downstreams)": {
			setupFilesWithOwnerRefs: []HierarchicalOwnershipRelation{
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
`,
			expectedError: false,
			noColor:       true,
		},
		"Resource removal detection with hierarchy (v2 style resourceRefs; cluster scoped downstreams)": {
			setupFilesWithOwnerRefs: []HierarchicalOwnershipRelation{
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
`,
			expectedError: false,
			noColor:       true,
		},
		"Resource with generateName": {
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/generated-name-composition.yaml",
			},
			setupFilesWithOwnerRefs: []HierarchicalOwnershipRelation{
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
		"New XR with generateName": {
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
		"Multiple XRs": {
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition.yaml",
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
		"Error on ambiguous composition selection": {
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
			setupFiles: []string{
				"testdata/diff/resources/existing-namespace.yaml",
				// Add the necessary CRDs and compositions for claim diffing
				"testdata/diff/resources/claim-xrd.yaml",
				"testdata/diff/resources/claim-composition.yaml",
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
			setupFiles: []string{
				"testdata/diff/resources/existing-namespace.yaml",
				// Add necessary CRDs and composition
				"testdata/diff/resources/claim-xrd.yaml",
				"testdata/diff/resources/claim-composition.yaml",
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
-   generateName: test-claim-82crv-
    labels:
      crossplane.io/claim-name: test-claim
      crossplane.io/claim-namespace: existing-namespace
-     crossplane.io/composite: test-claim-82crv
-   name: test-claim-82crv
+     crossplane.io/composite: test-claim
+   name: test-claim
  spec:
    forProvider:
-     configData: existing-value
+     configData: modified-value

---

Summary: 2 modified`,
			expectedError: false,
			noColor:       true,
		},
		"XRD defaults should be applied to XR before rendering": {
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
		"XRD defaults should not override user-specified values": {
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
	}

	runIntegrationTest(t, XRDiffTest, tests)
}

// TestCompDiffIntegration runs an integration test for the composition diff command.
func TestCompDiffIntegration(t *testing.T) {
	tests := map[string]IntegrationTestCase{
		"Composition change impacts existing XRs": {
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

- XNopResource/another-resource (namespace: default)
- XNopResource/test-resource (namespace: default)

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
		"Composition diff with custom namespace": {
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

- XNopResource/custom-namespace-resource (namespace: custom-namespace)

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
		"Multiple composition diff shows impact on existing XRs": {
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

- XNopResource/another-resource (namespace: default)
- XNopResource/test-resource (namespace: default)

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
		"Net-new composition with no downstream impact": {
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
	}

	runIntegrationTest(t, CompositionDiffTest, tests)
}
