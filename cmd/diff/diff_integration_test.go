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
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
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
	expectedOutput             string
	expectedError              bool
	expectedErrorContains      string
	expectedExitCode           int // Expected exit code (0=success, 1=tool error, 2=schema validation, 3=diff detected)
	noColor                    bool
	namespace                  string        // For composition tests (optional)
	xrdAPIVersion              XrdAPIVersion // For XR tests (optional)
	ignorePaths                []string      // Paths to ignore in diffs
	functionCredentials        string        // Path to function credentials file (optional)
	skip                       bool
	skipReason                 string
	// JSON output support: set outputFormat to "json" to use structured assertions
	outputFormat             string           // "json" or "" (default=visual diff)
	expectedStructuredOutput *tu.ExpectedDiff // for JSON output assertions
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
// Each parallel test needs its own scheme because envtest modifies it during CRD installation.
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
func runIntegrationTest(t *testing.T, testType DiffTestType, tt IntegrationTestCase) {
	t.Helper()

	t.Parallel() // Enable parallel test execution

	// Create a fresh scheme for each test to avoid concurrent map access.
	// Each parallel test needs its own scheme because envtest modifies it during CRD installation.
	scheme := createTestScheme()

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
	testFiles := make([]string, 0, len(tt.inputFiles))

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

	// Add JSON output format if specified
	if tt.outputFormat == "json" {
		args = append(args, "--output=json")
	}

	// Add ignore-paths if specified
	if len(tt.ignorePaths) > 0 {
		for _, path := range tt.ignorePaths {
			args = append(args, fmt.Sprintf("--ignore-paths=%s", path))
		}
	}

	// Add function-credentials if specified
	if tt.functionCredentials != "" {
		args = append(args, fmt.Sprintf("--function-credentials=%s", tt.functionCredentials))
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
	exitCode := &ExitCode{}

	// Create AppContext from the test environment's config
	appCtx, err := NewAppContext(cfg, logger)
	if err != nil {
		t.Fatalf("failed to create app context: %v", err)
	}

	// Create a Kong context with stdout
	parser, err := kong.New(cmd,
		kong.Writers(&stdout, &stdout),
		kong.Bind(appCtx),
		kong.Bind(exitCode),
		kong.BindTo(logger, (*logging.Logger)(nil)),
	)
	if err != nil {
		t.Fatalf("failed to create kong parser: %v", err)
	}

	kongCtx, err := parser.Parse(args)
	if err != nil {
		t.Fatalf("failed to parse kong context: %v", err)
	}

	err = kongCtx.Run()

	// Check exit code matches expected
	if exitCode.Code != tt.expectedExitCode {
		t.Errorf("expected exit code %d, got %d", tt.expectedExitCode, exitCode.Code)
	}

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

	// Handle JSON output format with structured assertions
	if tt.outputFormat == "json" && tt.expectedStructuredOutput != nil {
		tu.AssertStructuredDiff(t, outputStr, tt.expectedStructuredOutput)
		return
	}

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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"AutomaticNamespacePropagation": {
			reason:       "Validates automatic namespace propagation for namespaced managed resources",
			outputFormat: "json",
			inputFiles:   []string{"testdata/diff/new-xr.yaml"},
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition-no-namespace-patch.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(2, 0, 0).
				WithAddedResource("XDownstreamResource", "test-resource", "default").
				WithField("spec.forProvider.configData", "new-value").
				And().
				WithAddedResource("XNopResource", "test-resource", "default").
				WithField("spec.coolField", "new-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
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
			outputFormat:             "json",
			expectedStructuredOutput: tu.ExpectDiff().WithSummary(0, 0, 0),
			expectedError:            false,
		},
		"ModifiedXRCreatesDownstream": {
			reason:       "Shows diff when modified XR creates new downstream resource",
			outputFormat: "json",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition.yaml",
				"testdata/diff/resources/composition-revision-default.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/existing-xr.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-xr.yaml"},
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(1, 1, 0).
				WithAddedResource("XDownstreamResource", "test-resource", "default").
				WithField("spec.forProvider.configData", "modified-value").
				And().
				WithModifiedResource("XNopResource", "test-resource", "default").
				WithFieldChange("spec.coolField", "existing-value", "modified-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"EnvironmentConfigIncorporation": {
			reason:       "Validates EnvironmentConfig (v1beta1) incorporation in diff",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("XDownstreamEnvResource", "test-env-resource", "").
				WithFieldChange("spec.forProvider.configData", "existing-config-value", "modified-config-value").
				And().
				WithModifiedResource("XEnvResource", "test-env-resource", "").
				WithFieldChange("spec.configKey", "existing-config-value", "modified-config-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"ExternalResourceDependencies": {
			reason:       "Validates diff with external resource dependencies via fn-external-resources",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("XDownstreamResource", "test-resource", "default").
				WithFieldChange("spec.forProvider.configData", "existing-value", "testing-external-resource-data").
				WithFieldChange("spec.forProvider.roleName", "old-role-name", "external-named-clusterrole").
				And().
				WithModifiedResource("XNopResource", "test-resource", "default").
				WithFieldChange("spec.coolField", "existing-value", "modified-with-external-dep").
				WithFieldChange("spec.environment", "staging", "testing").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"TemplatedExtraResources": {
			reason:       "Validates diff with templated ExtraResources embedded in go-templating function",
			outputFormat: "json",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/external-resource-configmap.yaml",
				"testdata/diff/resources/external-res-gotpl-composition.yaml",
				"testdata/diff/resources/existing-xr-with-external-dep.yaml",
				"testdata/diff/resources/existing-downstream-with-external-dep.yaml",
			},
			inputFiles: []string{"testdata/diff/modified-xr-with-external-dep.yaml"},
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("XDownstreamResource", "test-resource", "default").
				WithFieldChange("spec.forProvider.configData", "existing-value", "modified-with-external-dep").
				WithFieldChange("spec.forProvider.roleName", "old-role-name", "templated-external-resource-testing").
				And().
				WithModifiedResource("XNopResource", "test-resource", "default").
				WithFieldChange("spec.coolField", "existing-value", "modified-with-external-dep").
				WithFieldChange("spec.environment", "staging", "testing").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"CrossNamespaceResourceDependencies": {
			reason:       "Validates cross-namespace resource dependencies via fn-external-resources",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("XDownstreamResource", "test-cross-ns-resource", "default").
				WithFieldChange("spec.forProvider.configData", "existing-cross-ns-data-existing-named-data-old-cross-ns-role", "cross-namespace-data-another-cross-namespace-data-external-named-clusterrole").
				And().
				WithModifiedResource("XNopResource", "test-cross-ns-resource", "default").
				WithFieldChange("spec.coolField", "existing-cross-ns-value", "modified-cross-ns-value").
				WithFieldChange("spec.environment", "staging", "production").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
		},
		// Issue #259: function-sequencer hides resources from later stages, but those
		// resources should be shown as removed if they exist in the cluster.
		// https://github.com/crossplane-contrib/crossplane-diff/issues/259
		"FunctionSequencerPreservesExistingResources": {
			reason: "Function-sequencer should NOT hide resources that already exist in the cluster (fix for issue #259)",
			crossplaneManagedResources: []HierarchicalOwnershipRelation{
				{
					OwnerFile: "testdata/diff/resources/sequencer-xr.yaml",
					OwnedFiles: map[string]*HierarchicalOwnershipRelation{
						// Stage 0 resource - exists in cluster
						"testdata/diff/resources/sequencer-stage0-downstream.yaml": nil,
						// Stage 1 resource - exists in cluster, should NOT be hidden by sequencer
						// because observed resources are passed correctly to the function pipeline
						"testdata/diff/resources/sequencer-stage1-downstream.yaml": nil,
					},
				},
			},
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/sequencer-composition.yaml",
				"testdata/diff/resources/sequencer-composition-revision.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			inputFiles:   []string{"testdata/diff/resources/sequencer-xr.yaml"},
			outputFormat: "json",
			// Expected: With the fix, observed resources ARE passed to function-sequencer.
			// Function-sequencer sees both resources exist and does NOT remove them from desired state.
			// Since existing resources match rendered resources, there should be no changes.
			// XR diff outputs nothing when there are no changes (unlike comp diff which says "No changes detected").
			expectedStructuredOutput: tu.ExpectDiff().WithSummary(0, 0, 0),
			expectedError:            false,
			expectedExitCode:         dp.ExitCodeSuccess,
		},
		// Issue #259: Validates that removal detection works even when the XR has no changes.
		// This is the core fix for the function-sequencer bug where existingXR was nil because
		// the XR diff wasn't stored when there were no changes.
		// https://github.com/crossplane-contrib/crossplane-diff/issues/259
		"ResourceRemovalWithUnmodifiedXR": {
			reason:       "Validates resource removal detection works when XR has no changes (uses existingXRFromCluster fallback)",
			outputFormat: "json",
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
			inputFiles: []string{"testdata/diff/unmodified-xr.yaml"}, // XR has NO changes
			// Expected: resource2 and its child are removed, but XR and resource1 have no changes
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 0, 2).
				WithRemovedResource("XDownstreamResource", "resource-to-be-removed", "default").
				WithField("spec.forProvider.configData", "existing-value").
				And().
				WithRemovedResource("XDownstreamResource", "resource-to-be-removed-child", "default").
				WithField("spec.forProvider.configData", "child-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"ResourceRemovalHierarchyV2Namespaced": {
			reason:       "Validates resource removal detection with hierarchy using v2 style resourceRefs and namespaced downstreams",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 2).
				WithModifiedResource("XDownstreamResource", "resource-to-be-kept", "default").
				WithFieldChange("spec.forProvider.configData", "existing-value", "modified-value").
				And().
				WithRemovedResource("XDownstreamResource", "resource-to-be-removed", "default").
				WithField("spec.forProvider.configData", "existing-value").
				And().
				WithRemovedResource("XDownstreamResource", "resource-to-be-removed-child", "default").
				WithField("spec.forProvider.configData", "child-value").
				And().
				WithModifiedResource("XNopResource", "test-resource", "default").
				WithFieldChange("spec.coolField", "existing-value", "modified-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"ResourceRemovalHierarchyV2ClusterScoped": {
			reason:       "Validates resource removal detection with hierarchy using v2 style resourceRefs and cluster scoped downstreams",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 2).
				WithModifiedResource("XDownstreamResource", "resource-to-be-kept", "").
				WithFieldChange("spec.forProvider.configData", "existing-value", "modified-value").
				And().
				WithRemovedResource("XDownstreamResource", "resource-to-be-removed", "").
				WithField("spec.forProvider.configData", "existing-value").
				And().
				WithRemovedResource("XDownstreamResource", "resource-to-be-removed-child", "").
				WithField("spec.forProvider.configData", "child-value").
				And().
				WithModifiedResource("XNopResource", "test-resource", "").
				WithFieldChange("spec.coolField", "existing-value", "modified-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"ResourceWithGenerateName": {
			reason:       "Validates handling of resources with generateName",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("XDownstreamResource", "test-resource-abc123", "default").
				WithFieldChange("spec.forProvider.configData", "existing-value", "new-value").
				And().
				WithModifiedResource("XNopResource", "test-resource", "default").
				WithFieldChange("spec.coolField", "existing-value", "new-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"NewXRWithGenerateName": {
			reason:       "Shows diff for new XR with generateName",
			outputFormat: "json",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition.yaml",
				"testdata/diff/resources/functions.yaml",
				// We don't add any existing XR, as we're testing creation of a new one
			},
			inputFiles: []string{"testdata/diff/generated-name-xr.yaml"},
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(2, 0, 0).
				WithAddedResource("XDownstreamResource", "", "default").
				WithNamePattern(`generated-xr-\(generated\)`).
				WithField("spec.forProvider.configData", "new-value").
				And().
				WithAddedResource("XNopResource", "", "default").
				WithNamePattern(`generated-xr-\(generated\)`).
				WithField("spec.coolField", "new-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"MultipleXRs": {
			reason:       "Validates diff for multiple XRs",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(2, 2, 0).
				WithAddedResource("XDownstreamResource", "first-resource", "default").
				WithField("spec.forProvider.configData", "first-value").
				And().
				WithModifiedResource("XDownstreamResource", "test-resource", "default").
				WithFieldChange("spec.forProvider.configData", "existing-value", "modified-value").
				And().
				WithAddedResource("XNopResource", "first-resource", "default").
				WithField("spec.coolField", "first-value").
				And().
				WithModifiedResource("XNopResource", "test-resource", "default").
				WithFieldChange("spec.coolField", "existing-value", "modified-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"SelectCompositionByDirectReference": {
			reason:       "Validates composition selection by direct reference",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(2, 0, 0).
				WithAddedResource("XDownstreamResource", "test-resource", "default").
				WithField("spec.forProvider.configData", "test-value").
				WithField("spec.forProvider.resourceTier", "production").
				And().
				WithAddedResource("XNopResource", "test-resource", "default").
				WithField("spec.coolField", "test-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"SelectCompositionByLabelSelector": {
			reason:       "Validates composition selection by label selector",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(2, 0, 0).
				WithAddedResource("XDownstreamResource", "test-resource", "default").
				WithField("spec.forProvider.configData", "test-value").
				WithField("spec.forProvider.resourceTier", "staging").
				And().
				WithAddedResource("XNopResource", "test-resource", "default").
				WithField("spec.coolField", "test-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
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
			expectedExitCode:      dp.ExitCodeToolError,
			expectedErrorContains: "ambiguous composition selection: multiple compositions match",
			noColor:               true,
		},
		"SchemaValidationError": {
			reason: "Validates exit code 2 for schema validation errors",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			inputFiles: []string{
				"testdata/diff/invalid-schema-xr.yaml",
			},
			expectedError:         true,
			expectedExitCode:      dp.ExitCodeSchemaValidation,
			expectedErrorContains: "schema validation",
			noColor:               true,
		},
		"NewClaimShowsDiff": {
			reason:       "Shows diff for new claim",
			outputFormat: "json",
			setupFiles: []string{
				"testdata/diff/resources/existing-namespace.yaml",
				// Add the necessary CRDs and compositions for claim diffing
				"testdata/diff/resources/claim-xrd.yaml",
				"testdata/diff/resources/claim-composition.yaml",
				"testdata/diff/resources/claim-composition-revision.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			inputFiles: []string{"testdata/diff/new-claim.yaml"},
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(2, 0, 0).
				WithAddedResource("NopClaim", "test-claim", "existing-namespace").
				WithField("spec.coolField", "new-value").
				And().
				WithAddedResource("XDownstreamResource", "test-claim", "").
				WithField("spec.forProvider.configData", "new-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"NewClaimWithClaimRefComposition": {
			reason:       "Shows diff for new claim when composition uses spec.claimRef - claimRef is synthesized for new claims",
			outputFormat: "json",
			setupFiles: []string{
				"testdata/diff/resources/existing-namespace.yaml",
				"testdata/diff/resources/claim-xrd.yaml",
				"testdata/diff/resources/claim-composition-with-claimref.yaml",
				"testdata/diff/resources/claim-composition-with-claimref-revision.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			inputFiles: []string{"testdata/diff/new-claim-with-claimref-composition.yaml"},
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(2, 0, 0).
				WithAddedResource("NopClaim", "test-claim", "existing-namespace").
				WithField("spec.coolField", "new-value").
				And().
				WithAddedResource("XDownstreamResource", "test-claim", "").
				WithField("spec.forProvider.configData", "existing-namespace/test-claim").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"ModifiedClaimShowsDiff": {
			reason:       "Shows diff for modified claim",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("NopClaim", "test-claim", "existing-namespace").
				WithFieldChange("spec.coolField", "existing-value", "modified-value").
				And().
				WithModifiedResource("XDownstreamResource", "test-claim-82crv", "").
				WithFieldChange("spec.forProvider.configData", "existing-value", "modified-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"XRDDefaultsAppliedBeforeRendering": {
			reason:       "Validates that XRD defaults are applied to XR before rendering",
			outputFormat: "json",
			inputFiles:   []string{"testdata/diff/xr-with-missing-defaults.yaml"},
			setupFiles: []string{
				"testdata/diff/resources/xrd-with-defaults.yaml",
				"testdata/diff/resources/composition-with-defaults.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(1, 0, 0).
				WithAddedResource("XTestDefaultResource", "test-resource-with-defaults", "default").
				WithField("spec.region", "us-east-1").
				WithField("spec.size", "large").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"XRDDefaultsNoOverride": {
			reason:       "Validates that XRD defaults do not override user-specified values",
			outputFormat: "json",
			inputFiles:   []string{"testdata/diff/xr-with-overridden-defaults.yaml"},
			setupFiles: []string{
				"testdata/diff/resources/xrd-with-defaults.yaml",
				"testdata/diff/resources/composition-with-defaults.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(1, 0, 0).
				WithAddedResource("XTestDefaultResource", "test-resource-with-overrides", "default").
				WithField("spec.region", "us-west-2").
				WithField("spec.size", "xlarge").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"ConcurrentRenderingMultipleXRs": {
			reason:       "Validates concurrent rendering with multiple functions and XRs from directory",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().WithSummary(30, 0, 0),
			expectedError:            false,
			expectedExitCode:         dp.ExitCodeDiffDetected,
		},
		"NewNestedXRCreatesChildren": {
			reason:       "Validates that new nested XR creates child XR and downstream resources",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(3, 0, 0).
				WithAddedResource("XChildResource", "test-parent-child", "default").
				WithField("spec.childField", "parent-value").
				And().
				WithAddedResource("XDownstreamResource", "test-parent-child-managed", "default").
				WithField("spec.forProvider.configData", "parent-value").
				And().
				WithAddedResource("XParentResource", "test-parent", "default").
				WithField("spec.parentField", "parent-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"ModifiedNestedXRPropagatesChanges": {
			reason:       "Validates that modified nested XR propagates changes through child XR to downstream resources",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 3, 0).
				WithModifiedResource("XChildResource", "test-parent-child", "default").
				WithFieldChange("spec.childField", "existing-value", "modified-value").
				And().
				WithModifiedResource("XDownstreamResource", "test-parent-child-managed", "default").
				WithFieldChange("spec.forProvider.configData", "existing-value", "modified-value").
				And().
				WithModifiedResource("XParentResource", "test-parent", "default").
				WithFieldChange("spec.parentField", "existing-value", "modified-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		// Composition Revision tests for v2 XRDs
		"V2ManualPolicyPinnedRevision": {
			reason:       "Validates v2 XR with Manual update policy stays on pinned revision",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("XDownstreamResource", "test-manual-v1", "default").
				WithFieldChange("spec.forProvider.configData", "v1-existing-value", "v1-modified-value").
				And().
				WithModifiedResource("XNopResource", "test-manual-v1", "default").
				WithFieldChange("spec.coolField", "existing-value", "modified-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"V2AutomaticPolicyLatestRevision": {
			reason:       "Validates v2 XR with Automatic update policy uses latest revision",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("XDownstreamResource", "test-automatic", "default").
				WithFieldChange("spec.forProvider.configData", "v1-existing-value", "v2-modified-value").
				And().
				WithModifiedResource("XNopResource", "test-automatic", "default").
				WithFieldChange("spec.coolField", "existing-value", "modified-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"V2ManualRevisionUpgradeDiff": {
			reason:       "Validates v2 XR changing revision in Manual mode shows upgrade diff",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("XDownstreamResource", "test-manual-v1", "default").
				WithFieldChange("spec.forProvider.configData", "v1-existing-value", "v2-modified-value").
				And().
				WithModifiedResource("XNopResource", "test-manual-v1", "default").
				WithFieldChange("spec.coolField", "existing-value", "modified-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"CompositionRevisionUpgradesResourceAPIVersion": {
			// NOTE: This test validates that resources are correctly matched across API version changes,
			// avoiding delete/recreate. The composition template changes from v1beta1 to v1beta2, but
			// Kubernetes automatically converts resources between served API versions. When we query for
			// the v1beta2 resource, Kubernetes finds the v1beta1 resource and returns it auto-converted
			// to v1beta2. From Kubernetes' perspective, the resource exists as both versions simultaneously,
			// so there's no apiVersion field change to show in the diff. The important thing is that the
			// resource is matched (shown as ~~~, not ---/+++), preventing delete/recreate operations.
			reason:       "Validates XR upgrading composition revision that changes resource API version shows as update not remove/add",
			outputFormat: "json",
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
			// Key assertion: both resources are MODIFIED (not added/removed), proving API version migration works
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("XApiMigrateResource", "test-api-version-xr-api-resource", "default").
				And().
				WithModifiedResource("XNopResource", "test-api-version-xr", "default").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"V2SwitchManualToAutomatic": {
			reason:       "Validates v2 XR switching from Manual to Automatic mode uses latest revision",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("XDownstreamResource", "test-manual-v1", "default").
				WithFieldChange("spec.forProvider.configData", "v1-existing-value", "v2-modified-value").
				And().
				WithModifiedResource("XNopResource", "test-manual-v1", "default").
				WithFieldChange("spec.coolField", "existing-value", "modified-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"V2NetNewManualNoRevRef": {
			reason:       "Validates v2 net new XR with Manual policy but no revision ref uses latest revision",
			outputFormat: "json",
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/composition-revision-v1.yaml",
				"testdata/diff/resources/composition-revision-v2.yaml",
				"testdata/diff/resources/composition-v2.yaml", // Current composition is v2
				"testdata/diff/resources/functions.yaml",
			},
			inputFiles: []string{"testdata/diff/new-xr-manual-no-ref.yaml"},
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(2, 0, 0).
				WithAddedResource("XDownstreamResource", "test-manual-no-ref", "default").
				WithField("spec.forProvider.configData", "v2-new-value").
				And().
				WithAddedResource("XNopResource", "test-manual-no-ref", "default").
				WithField("spec.coolField", "new-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		// Composition Revision tests for v1 XRDs (Crossplane 1.20 compatibility)
		"V1ManualRevisionUpgradeDiff": {
			reason:        "Validates v1 XR with Manual update policy changing revision shows upgrade diff",
			outputFormat:  "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("XDownstreamResource", "test-legacy-manual-v1", "").
				WithFieldChange("spec.forProvider.configData", "v1-existing-value", "v2-modified-value").
				And().
				WithModifiedResource("XNopResource", "test-legacy-manual-v1", "").
				WithFieldChange("spec.coolField", "existing-value", "modified-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		// v2 XRD with v1-style composition paths (issue #206)
		"V2XRDWithV1StyleCompositionPaths": {
			reason:       "Validates v2 XRD using v1-style spec.compositionRef paths are correctly recognized (issue #206)",
			outputFormat: "json",
			setupFiles: []string{
				"testdata/diff/resources/v2-xrd-with-v1-paths.yaml",
				"testdata/diff/resources/v2-xrd-with-v1-paths-composition.yaml",
				"testdata/diff/resources/v2-xrd-with-v1-paths-composition-revision.yaml",
				"testdata/diff/resources/functions.yaml",
				"testdata/diff/resources/existing-v2xrd-v1paths-xr.yaml",
				"testdata/diff/resources/existing-v2xrd-v1paths-downstream.yaml",
			},
			inputFiles: []string{"testdata/diff/resources/modified-v2xrd-v1paths-xr.yaml"},
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 2, 0).
				WithModifiedResource("XDownstreamResource", "test-v2xrd-v1paths", "default").
				WithFieldChange("spec.forProvider.configData", "existing-value", "modified-value").
				And().
				WithModifiedResource("XNopResource", "test-v2xrd-v1paths", "default").
				WithFieldChange("spec.coolField", "existing-value", "modified-value").
				And(),
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"ModifiedClaimWithNestedXRsShowsDiff": {
			reason:       "Validates that modified Claims with nested XRs show proper diff (3 modified resources)",
			outputFormat: "json",
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
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(0, 3, 0).
				WithModifiedResource("ClusterNopResource", "existing-parent-claim-82crv-nop", "").
				And().
				WithModifiedResource("ParentNopClaim", "existing-parent-claim", "default").
				WithFieldChange("spec.parentField", "existing-parent-value", "modified-parent-value").
				And().
				WithModifiedResource("XChildNopClaim", "existing-parent-claim-82crv-child", "").
				WithFieldChange("spec.childField", "existing-parent-value", "modified-parent-value").
				And(),
			xrdAPIVersion:    V1, // Use V1 style resourceRefs since XRDs have claims
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
		},
		"FunctionCredentialsAutoFetch": {
			reason:       "Successfully renders XR when composition references credentials that exist in cluster",
			outputFormat: "json",
			inputFiles:   []string{"testdata/diff/new-xr-with-creds.yaml"},
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/credentials/composition-with-creds.yaml",
				"testdata/diff/resources/credentials/function-credentials-secret.yaml",
				"testdata/diff/resources/functions.yaml",
			},
			expectedExitCode: dp.ExitCodeDiffDetected,
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(2, 0, 0).
				WithAddedResource("XDownstreamResource", "test-resource-with-creds", "default").
				WithField("spec.forProvider.configData", "test-value-creds").
				And().
				WithAddedResource("XNopResource", "test-resource-with-creds", "default").
				WithField("spec.coolField", "test-value-creds").
				And(),
			expectedError: false,
		},
		"FunctionCredentialsFromCLI": {
			reason:       "Successfully renders XR when credentials provided via --function-credentials flag",
			outputFormat: "json",
			inputFiles:   []string{"testdata/diff/new-xr-with-creds.yaml"},
			setupFiles: []string{
				"testdata/diff/resources/xrd.yaml",
				"testdata/diff/resources/credentials/composition-with-creds.yaml",
				// Note: NOT setting up the secret in cluster - testing CLI override
				"testdata/diff/resources/functions.yaml",
			},
			// Credentials loaded from CLI flag file
			functionCredentials: "testdata/diff/resources/credentials/cli-credentials.yaml",
			expectedExitCode:    dp.ExitCodeDiffDetected,
			expectedStructuredOutput: tu.ExpectDiff().
				WithSummary(2, 0, 0).
				WithAddedResource("XDownstreamResource", "test-resource-with-creds", "default").
				WithField("spec.forProvider.configData", "test-value-creds").
				And().
				WithAddedResource("XNopResource", "test-resource-with-creds", "default").
				WithField("spec.coolField", "test-value-creds").
				And(),
			expectedError: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			runIntegrationTest(t, XRDiffTest, tt)
		})
	}
}

// TestCompDiffIntegration runs an integration test for the composition diff command.
func TestCompDiffIntegration(t *testing.T) {
	t.Parallel()

	// Set up logger for controller-runtime (global setup, once per test function)
	tu.SetupKubeTestLogger(t)

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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          false,
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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
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
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
		},
		"SHA256DigestFunctionReference": {
			reason: "Validates composition diff works with functions referenced by SHA256 digest instead of tag",
			setupFiles: []string{
				"testdata/comp/resources/xrd.yaml",
				"testdata/comp/resources/sha256-composition.yaml",
				"testdata/comp/resources/functions-sha256.yaml",
				"testdata/comp/resources/existing-sha256-xr.yaml",
				"testdata/comp/resources/existing-sha256-downstream.yaml",
			},
			inputFiles: []string{"testdata/comp/updated-sha256-composition.yaml"},
			namespace:  "default",
			expectedOutput: `
=== Composition Changes ===

~~~ Composition/xnopresources-sha256.diff.example.org
  apiVersion: apiextensions.crossplane.io/v1
  kind: Composition
  metadata:
    name: xnopresources-sha256.diff.example.org
  spec:
    compositeTypeRef:
      apiVersion: ns.diff.example.org/v1alpha1
      kind: XNopResource
    mode: Pipeline
    pipeline:
    - functionRef:
        name: function-go-templating-sha256
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

  ⚠ XNopResource/sha256-test-resource (namespace: default)

Summary: 1 resource with changes

=== Impact Analysis ===

~~~ XDownstreamResource/sha256-test-resource
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
+     crossplane.io/composition-resource-name: nop-resource
      gotemplating.fn.crossplane.io/composition-resource-name: nop-resource
    labels:
      crossplane.io/composite: sha256-test-resource
    name: sha256-test-resource
    namespace: default
  spec:
    forProvider:
-     configData: sha256-value
-     resourceTier: basic
+     configData: updated-sha256-value
+     resourceTier: premium

---

Summary: 1 modified`,
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
		},
		"NestedXRUsesOwnComposition": {
			reason: "Validates that nested XRs use their own composition from the cluster, not the parent's CLI composition",
			// Set up XRDs, compositions, and functions
			setupFiles: []string{
				// Child XRD and composition (XNopResource uses xnopresources.diff.example.org)
				"testdata/comp/resources/xrd.yaml",
				"testdata/comp/resources/original-composition.yaml",
				"testdata/comp/resources/functions.yaml",
				// Parent XRD and composition
				"testdata/comp/resources/nested-xr/parent-xrd.yaml",
				"testdata/comp/resources/nested-xr/parent-composition.yaml",
			},
			// Use crossplaneManagedResources to set up proper ownership hierarchy
			crossplaneManagedResources: []HierarchicalOwnershipRelation{
				{
					// Parent XR owns both direct downstream and nested child XR
					OwnerFile: "testdata/comp/resources/nested-xr/existing-parent-xr.yaml",
					OwnedFiles: map[string]*HierarchicalOwnershipRelation{
						// Direct downstream owned by parent XR
						"testdata/comp/resources/nested-xr/existing-parent-direct-downstream.yaml": nil,
						// Nested child XR owned by parent XR, which owns its own downstream
						"testdata/comp/resources/nested-xr/existing-nested-child-xr.yaml": {
							OwnedFiles: map[string]*HierarchicalOwnershipRelation{
								// Child's downstream owned by nested child XR
								"testdata/comp/resources/nested-xr/existing-child-downstream.yaml": nil,
							},
						},
					},
				},
			},
			// Diff the updated parent composition
			inputFiles: []string{"testdata/comp/updated-parent-composition.yaml"},
			namespace:  "default",
			// Expected: Only the parent's direct downstream shows changes (tier change).
			// The nested child's downstream should NOT change because:
			// 1. The nested XR (XNopResource) uses its own composition (xnopresources.diff.example.org)
			// 2. That child composition was NOT modified
			// Bug scenario: If nested XR incorrectly uses parent composition, we'd see wrong changes to child's downstream.
			// Expected output: Only the parent's direct downstream should show in the Impact Analysis.
			// The child's downstream (XDownstreamResource/test-parent-nested) should NOT appear here.
			// If it did, it would mean the nested XR incorrectly used the parent's CLI composition.
			expectedOutput: `=== Impact Analysis ===

~~~ XDownstreamResource/test-parent-direct
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: direct-resource
    labels:
      crossplane.io/composite: test-parent
    name: test-parent-direct
    namespace: default
  spec:
    forProvider:
      configData: parent-parent-value
-     resourceTier: parent-tier
+     resourceTier: premium-tier

---

Summary: 1 modified`,
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
		},
		"CrossNamespaceResourceCollision": {
			reason: "Validates that resources with the same name in different namespaces are correctly distinguished",
			// This test catches a bug where the cache key didn't include namespace, causing
			// ConfigMaps with the same name in different namespaces to collide.
			// The XR in ns-a should get ConfigMap from ns-a (value: from-ns-a)
			// The XR in ns-b should get ConfigMap from ns-b (value: from-ns-b)
			// Bug: Both XRs would get the same ConfigMap (whichever was cached first)
			setupFiles: []string{
				// Create namespaces first
				"testdata/comp/resources/namespace-collision/namespace-ns-a.yaml",
				"testdata/comp/resources/namespace-collision/namespace-ns-b.yaml",
				"testdata/comp/resources/xrd.yaml",
				"testdata/comp/resources/functions.yaml",
				"testdata/comp/resources/namespace-collision/collision-composition.yaml",
				// ConfigMaps with same name in different namespaces
				"testdata/comp/resources/namespace-collision/configmap-ns-a.yaml",
				"testdata/comp/resources/namespace-collision/configmap-ns-b.yaml",
			},
			// XRs and their downstream resources in different namespaces
			crossplaneManagedResources: []HierarchicalOwnershipRelation{
				{
					OwnerFile: "testdata/comp/resources/namespace-collision/existing-xr-ns-a.yaml",
					OwnedFiles: map[string]*HierarchicalOwnershipRelation{
						"testdata/comp/resources/namespace-collision/existing-downstream-ns-a.yaml": nil,
					},
				},
				{
					OwnerFile: "testdata/comp/resources/namespace-collision/existing-xr-ns-b.yaml",
					OwnedFiles: map[string]*HierarchicalOwnershipRelation{
						"testdata/comp/resources/namespace-collision/existing-downstream-ns-b.yaml": nil,
					},
				},
			},
			inputFiles: []string{"testdata/comp/updated-collision-composition.yaml"},
			// Expected: Each XR's downstream should show the correct prefix applied to its namespace's ConfigMap value.
			// XR in ns-a: from-ns-a → prefix-from-ns-a
			// XR in ns-b: from-ns-b → prefix-from-ns-b
			// Bug scenario: If namespace collision occurs, ns-b would incorrectly show:
			//   from-ns-b → prefix-from-ns-a (wrong ConfigMap cached from ns-a)
			expectedOutput: `=== Impact Analysis ===

~~~ XDownstreamResource/xr-in-ns-a
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: collision-resource
    labels:
      crossplane.io/composite: xr-in-ns-a
    name: xr-in-ns-a
    namespace: ns-a
  spec:
    forProvider:
-     configData: from-ns-a
+     configData: prefix-from-ns-a

---
~~~ XDownstreamResource/xr-in-ns-b
  apiVersion: ns.nop.example.org/v1alpha1
  kind: XDownstreamResource
  metadata:
    annotations:
      crossplane.io/composition-resource-name: collision-resource
    labels:
      crossplane.io/composite: xr-in-ns-b
    name: xr-in-ns-b
    namespace: ns-b
  spec:
    forProvider:
-     configData: from-ns-b
+     configData: prefix-from-ns-b

---

Summary: 2 modified`,
			expectedError:    false,
			expectedExitCode: dp.ExitCodeDiffDetected,
			noColor:          true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			runIntegrationTest(t, CompositionDiffTest, tt)
		})
	}
}
