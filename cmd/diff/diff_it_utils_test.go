package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	gyaml "gopkg.in/yaml.v3"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	xpextv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
)

// CrossplaneFieldManager is the field manager used for SSA when setting up Crossplane-managed resources.
// This simulates how Crossplane applies composed resources in production.
const CrossplaneFieldManager = "apiextensions.crossplane.io/composed/integration-test"

// CompositeReconcilerFieldManager stands for Crossplane's composite reconciler, which records the
// composition and revision it selects for an XR with an Update of the XR (crossplane
// internal/controller/apiextensions/composite/api.go), not as part of any composed apply.
const CompositeReconcilerFieldManager = "crossplane"

// compositeReconcilerFields are the fields the composite reconciler writes that way, at the v2 and v1
// paths: compositionRef and compositionRevisionRef.
var compositeReconcilerFields = [][]string{
	{"spec", "crossplane", "compositionRef"},
	{"spec", "crossplane", "compositionRevisionRef"},
	{"spec", "compositionRef"},
	{"spec", "compositionRevisionRef"},
}

// Testing data for integration tests

// createTestCompositionWithExtraResources creates a test Composition with a function-extra-resources step.
func createTestCompositionWithExtraResources() (*xpextv1.Composition, error) {
	pipelineMode := xpextv1.CompositionModePipeline

	// Create the extra resources function input
	extraResourcesInput := map[string]any{
		"apiVersion": "function.crossplane.io/v1beta1",
		"kind":       "ExtraResources",
		"spec": map[string]any{
			"extraResources": []any{
				map[string]any{
					"apiVersion": "example.org/v1",
					"kind":       "ExtraResource",
					"selector": map[string]any{
						"matchLabels": map[string]any{
							"app": "test-app",
						},
					},
				},
			},
		},
	}

	extraResourcesRaw, err := json.Marshal(extraResourcesInput)
	if err != nil {
		return nil, err
	}

	// Create template function input to create composed resources
	templateInput := map[string]any{
		"apiVersion": "apiextensions.crossplane.io/v1",
		"kind":       "Composition",
		"spec": map[string]any{
			"resources": []any{
				map[string]any{
					"name": "composed-resource",
					"base": map[string]any{
						"apiVersion": "example.org/v1",
						"kind":       "ComposedResource",
						"metadata": map[string]any{
							"name": "test-composed-resource",
							"labels": map[string]any{
								"app": "crossplane",
							},
						},
						"spec": map[string]any{
							"coolParam": "{{ .observed.composite.spec.coolParam }}",
							"replicas":  "{{ .observed.composite.spec.replicas }}",
							"extraData": "{{ index .observed.resources \"extra-resource-0\" \"spec\" \"data\" }}",
						},
					},
				},
			},
		},
	}

	templateRaw, err := json.Marshal(templateInput)
	if err != nil {
		return nil, err
	}

	return &xpextv1.Composition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-composition",
		},
		Spec: xpextv1.CompositionSpec{
			CompositeTypeRef: xpextv1.TypeReference{
				APIVersion: "example.org/v1",
				Kind:       "XExampleResource",
			},
			Mode: pipelineMode,
			Pipeline: []xpextv1.PipelineStep{
				{
					Step:        "extra-resources",
					FunctionRef: xpextv1.FunctionReference{Name: "function-extra-resources"},
					Input:       &runtime.RawExtension{Raw: extraResourcesRaw},
				},
				{
					Step:        "templating",
					FunctionRef: xpextv1.FunctionReference{Name: "function-patch-and-transform"},
					Input:       &runtime.RawExtension{Raw: templateRaw},
				},
			},
		},
	}, nil
}

// createTestXRD creates a test XRD for the XR.
func createTestXRD() *xpextv1.CompositeResourceDefinition {
	return tu.NewXRD("xexampleresources.example.org", "example.org", "XExampleResource").
		WithPlural("xexampleresources").
		WithSingular("xexampleresource").
		WithRawSchema([]byte(`{
			"type": "object",
			"properties": {
				"spec": {
					"type": "object",
					"properties": {
						"coolParam": {
							"type": "string"
						},
						"replicas": {
							"type": "integer"
						}
					}
				},
				"status": {
					"type": "object",
					"properties": {
						"coolStatus": {
							"type": "string"
						}
					}
				}
			}
		}`)).
		Build()
}

// createExtraResource creates a test extra resource.
func createExtraResource() *un.Unstructured {
	return &un.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.org/v1",
			"kind":       "ExtraResource",
			"metadata": map[string]any{
				"name": "test-extra-resource",
				"labels": map[string]any{
					"app": "test-app",
				},
			},
			"spec": map[string]any{
				"data": "extra-resource-data",
			},
		},
	}
}

// createExistingComposedResource creates an existing composed resource with different values.
func createExistingComposedResource() *un.Unstructured {
	return &un.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.org/v1",
			"kind":       "ComposedResource",
			"metadata": map[string]any{
				"name": "test-xr-composed-resource",
				"labels": map[string]any{
					"app":                     "crossplane",
					"crossplane.io/composite": "test-xr",
				},
				"annotations": map[string]any{
					"crossplane.io/composition-resource-name": "composed-resource",
				},
				"ownerReferences": []any{
					map[string]any{
						"apiVersion":         "example.org/v1",
						"kind":               "XExampleResource",
						"name":               "test-xr",
						"controller":         true,
						"blockOwnerDeletion": true,
					},
				},
			},
			"spec": map[string]any{
				"coolParam": "old-value", // Different from what will be rendered
				"replicas":  2,           // Different from what will be rendered
				"extraData": "old-data",  // Different from what will be rendered
			},
		},
	}
}

// createMatchingComposedResource creates a composed resource that matches what would be rendered.
func createMatchingComposedResource() *un.Unstructured {
	return &un.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.org/v1",
			"kind":       "ComposedResource",
			"metadata": map[string]any{
				"name": "test-xr-composed-resource",
				"labels": map[string]any{
					"app":                     "crossplane",
					"crossplane.io/composite": "test-xr",
				},
				"annotations": map[string]any{
					"crossplane.io/composition-resource-name": "composed-resource",
				},
				"ownerReferences": []any{
					map[string]any{
						"apiVersion":         "example.org/v1",
						"kind":               "XExampleResource",
						"name":               "test-xr",
						"controller":         true,
						"blockOwnerDeletion": true,
					},
				},
			},
			"spec": map[string]any{
				"coolParam": "test-value",          // Matches what would be rendered
				"replicas":  3,                     // Matches what would be rendered
				"extraData": "extra-resource-data", // Matches what would be rendered
			},
		},
	}
}

// HierarchicalOwnershipRelation represents an ownership tree structure.
type HierarchicalOwnershipRelation struct {
	OwnerFile  string                                    // The file containing the owner resource
	OwnedFiles map[string]*HierarchicalOwnershipRelation // Map of owned file paths to their own relationships
}

// composes is the one-level HierarchicalOwnershipRelation: an XR and the composed resources it
// controls. Spell out the struct only for a tree deeper than that, i.e. one with nested XRs.
func composes(xrFile string, composedFiles ...string) HierarchicalOwnershipRelation {
	owned := make(map[string]*HierarchicalOwnershipRelation, len(composedFiles))
	for _, f := range composedFiles {
		owned[f] = nil
	}

	return HierarchicalOwnershipRelation{OwnerFile: xrFile, OwnedFiles: owned}
}

// setOwnerReference adds an owner reference to the resource.
func setOwnerReference(resource, owner *un.Unstructured) {
	// Create owner reference
	ownerRef := metav1.OwnerReference{
		APIVersion:         owner.GetAPIVersion(),
		Kind:               owner.GetKind(),
		Name:               owner.GetName(),
		UID:                owner.GetUID(),
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}

	// Set the owner reference
	resource.SetOwnerReferences([]metav1.OwnerReference{ownerRef})
}

// addResourceRef adds a reference to the child resource in the parent's resourceRefs array.
func addResourceRef(parent, child *un.Unstructured, xrAPIVersion XrdAPIVersion) error {
	// Create the resource reference
	ref := map[string]any{
		"apiVersion": child.GetAPIVersion(),
		"kind":       child.GetKind(),
		"name":       child.GetName(),
	}

	// A namespaced XR composes only in its own namespace, so its resourceRefs have no namespace field
	// (xcrd omits it from the schema); a cluster-scoped XR's name the namespace of a namespaced child.
	if ns := child.GetNamespace(); ns != "" && parent.GetNamespace() == "" {
		ref["namespace"] = ns
	}

	path := resourceRefsPath(xrAPIVersion)

	// Get current resourceRefs or initialize if not present
	resourceRefs, found, err := un.NestedSlice(parent.Object, path...)
	if err != nil {
		return errors.Wrap(err, "cannot get resourceRefs from parent")
	}

	if !found || resourceRefs == nil {
		resourceRefs = []any{}
	}

	// A fixture may already list the reference, as Crossplane would have written it; don't repeat it.
	for _, existing := range resourceRefs {
		if m, ok := existing.(map[string]any); ok &&
			m["apiVersion"] == ref["apiVersion"] && m["kind"] == ref["kind"] &&
			m["name"] == ref["name"] && m["namespace"] == ref["namespace"] {
			return nil
		}
	}

	// Add the new reference and update the parent
	resourceRefs = append(resourceRefs, ref)

	return un.SetNestedSlice(parent.Object, resourceRefs, path...)
}

// applyResourcesFromFiles loads and applies resources from YAML files
// Under the assumption that no resource should already exist.
func applyResourcesFromFiles(ctx context.Context, c client.Client, paths []string) error {
	// Collect all resources from all files first
	var allResources []*un.Unstructured

	for _, path := range paths {
		resources, err := readResourcesFromFile(path)
		if err != nil {
			return fmt.Errorf("failed to read resources from %s: %w", path, err)
		}

		allResources = append(allResources, resources...)
	}

	// Apply all resources as new resources
	return createResources(ctx, c, allResources)
}

// deleteResourcesFromFiles issues a delete for every resource in the supplied files. Resources
// carrying a finalizer survive the call with metadata.deletionTimestamp set, which is how tests
// observe a resource mid-deletion under envtest (no controllers run to clear the finalizer).
func deleteResourcesFromFiles(ctx context.Context, c client.Client, paths []string) error {
	for _, path := range paths {
		resources, err := readResourcesFromFile(path)
		if err != nil {
			return fmt.Errorf("failed to read resources from %s: %w", path, err)
		}

		for _, resource := range resources {
			if err := c.Delete(ctx, resource.DeepCopy()); err != nil {
				return fmt.Errorf("failed to delete resource %s/%s: %w",
					resource.GetNamespace(), resource.GetName(), err)
			}
		}
	}

	return nil
}

// awaitAdmissionDenial blocks until every resource in the supplied manifest is refused by a
// ValidatingAdmissionPolicy on a dry-run create. An empty path is a no-op.
//
// A VAP does not take effect the instant it is created: the apiserver's admission plugin learns
// about policies and bindings through an informer, so for a short window after setup the cluster
// still accepts what the policy will later refuse. Any test that depends on a rejection must wait
// for that window to close, or it silently becomes a test of nothing.
//
// The gate is deliberately narrow: only an error naming ValidatingAdmissionPolicy counts as
// "enforcing". Accepting any rejection would let an unrelated failure (a schema error in the canary,
// a missing namespace) masquerade as a working policy.
func awaitAdmissionDenial(ctx context.Context, c client.Client, path string) error {
	if path == "" {
		return nil
	}

	resources, err := readResourcesFromFile(path)
	if err != nil {
		return fmt.Errorf("failed to read resources from %s: %w", path, err)
	}

	// Bounded independently of the caller's context so a slow policy rollout fails here with a
	// clear message instead of consuming the whole test's budget and timing out inside the diff.
	waitCtx, cancel := context.WithTimeout(ctx, admissionPolicyPropagationTimeout)
	defer cancel()

	for _, r := range resources {
		if err := awaitOneAdmissionDenial(waitCtx, c, r); err != nil {
			return err
		}
	}

	return nil
}

// admissionPolicyPropagationTimeout bounds the wait for a ValidatingAdmissionPolicy to start
// enforcing. Measured at ~600ms on envtest 1.32; the margin is for loaded CI machines.
const admissionPolicyPropagationTimeout = 30 * time.Second

func awaitOneAdmissionDenial(ctx context.Context, c client.Client, canary *un.Unstructured) error {
	for attempts := 1; ; attempts++ {
		err := c.Create(ctx, canary.DeepCopy(), client.DryRunAll)

		switch {
		case err != nil && strings.Contains(err.Error(), "ValidatingAdmissionPolicy"):
			return nil
		case err != nil:
			return fmt.Errorf("dry-run create of canary %s/%s failed for a reason unrelated to admission policy: %w",
				canary.GetKind(), canary.GetName(), err)
		}

		if ctx.Err() != nil {
			return fmt.Errorf("canary %s/%s was still accepted after %d attempts; no ValidatingAdmissionPolicy is enforcing: %w",
				canary.GetKind(), canary.GetName(), attempts, ctx.Err())
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// readResourcesFromFile reads YAML resources from a file.
func readResourcesFromFile(path string) ([]*un.Unstructured, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", path, err)
	}

	// Use a YAML decoder to handle multiple documents
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	var resources []*un.Unstructured

	for {
		resource := &un.Unstructured{}

		err := decoder.Decode(resource)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, fmt.Errorf("failed to decode YAML document from %s: %w", path, err)
		}

		// Skip empty documents
		if len(resource.Object) == 0 {
			continue
		}

		resources = append(resources, resource)
	}

	return resources, nil
}

// xrdCRDName is the CRD that defines CompositeResourceDefinitions. It must be among the plain CRDs.
const xrdCRDName = "compositeresourcedefinitions.apiextensions.crossplane.io"

// envtestCRDs returns every CRD an integration test's apiserver must serve: the plain CRDs under crdDirs,
// plus the CRDs Crossplane generates from each XRD that setupFiles declare. In a real cluster an XR's or a
// claim's CRD exists only because its XRD does, and Crossplane generates it; deriving them here the same
// way keeps test CRDs from drifting away from what a cluster would serve.
//
// A CRD declared twice is an error rather than a silent overwrite (envtest would install the later copy over
// the earlier one). Most likely it is a hand-written copy of a CRD that is now generated, which must go.
func envtestCRDs(crdDirs, setupFiles []string) ([]*extv1.CustomResourceDefinition, error) {
	opts := envtest.CRDInstallOptions{Paths: crdDirs, ErrorIfPathMissing: true}
	if err := envtest.ReadCRDFiles(&opts); err != nil {
		return nil, fmt.Errorf("cannot read CRDs from %v: %w", crdDirs, err)
	}

	var xrdCRD *extv1.CustomResourceDefinition

	// Keyed by both CRD name and kind.group: two CRDs of different names that serve the same kind
	// collide just as surely (the apiserver never establishes the second).
	sources := make(map[string]string, 2*len(opts.CRDs))
	for _, crd := range opts.CRDs {
		source := fmt.Sprintf("a CRD file under %v", crdDirs)
		sources[crd.GetName()] = source
		sources[crd.Spec.Names.Kind+"."+crd.Spec.Group] = source

		if crd.GetName() == xrdCRDName {
			xrdCRD = crd
		}
	}

	if xrdCRD == nil {
		return nil, fmt.Errorf("no CRD %q under %v", xrdCRDName, crdDirs)
	}

	crds := opts.CRDs

	for _, path := range setupFiles {
		resources, err := readResourcesFromFile(path)
		if err != nil {
			return nil, err
		}

		for _, r := range resources {
			if r.GroupVersionKind().GroupKind() != xpextv1.CompositeResourceDefinitionGroupVersionKind.GroupKind() {
				continue
			}

			generated, err := tu.CRDsForXRD(r, xrdCRD)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}

			for _, crd := range generated {
				source := fmt.Sprintf("XRD %q in %s", r.GetName(), path)

				for _, key := range []string{crd.GetName(), crd.Spec.Names.Kind + "." + crd.Spec.Group} {
					if existing, ok := sources[key]; ok {
						return nil, fmt.Errorf("CRD %q (%s) is generated from %s but %s also declares it; "+
							"a generated CRD must not also be hand-written or generated twice",
							crd.GetName(), key, source, existing)
					}

					sources[key] = source
				}

				crds = append(crds, crd)
			}
		}
	}

	return crds, nil
}

// createResources creates all resources in the cluster
// Assumes resources don't already exist - fails if they do.
func createResources(ctx context.Context, c client.Client, resources []*un.Unstructured) error {
	for _, resource := range resources {
		err := c.Create(ctx, resource.DeepCopy())
		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("resource %s/%s of kind %s already exists - test setup error",
					resource.GetNamespace(), resource.GetName(), resource.GetKind())
			}

			return fmt.Errorf("failed to create resource %s/%s: %w",
				resource.GetNamespace(), resource.GetName(), err)
		}
	}

	return nil
}

// applyResourceWithSSA applies a resource using Server-Side Apply with the specified field manager.
// This simulates how Crossplane applies composed resources, allowing us to test field ownership behavior.
func applyResourceWithSSA(ctx context.Context, c client.Client, resource *un.Unstructured, fieldManager string) error {
	// Use Apply to perform Server-Side Apply
	resourceCopy := resource.DeepCopy()

	// For SSA, we need to set the resource version to empty to indicate this is an apply
	resourceCopy.SetResourceVersion("")

	applyConfig := client.ApplyConfigurationFromUnstructured(resourceCopy)

	err := c.Apply(ctx, applyConfig, client.FieldOwner(fieldManager), client.ForceOwnership)
	if err != nil {
		return fmt.Errorf("failed to apply resource %s/%s with field manager %s: %w",
			resource.GetNamespace(), resource.GetName(), fieldManager, err)
	}

	return nil
}

// applyHierarchicalOwnership applies a hierarchical ownership structure.
func applyHierarchicalOwnership(ctx context.Context, _ logging.Logger, c client.Client, xrdAPIVersion XrdAPIVersion, hierarchies []HierarchicalOwnershipRelation) error {
	// Map to store created resources by file path
	createdResources := make(map[string]*un.Unstructured)
	// Map to track parent-child relationships for establishing resourceRefs
	parentChildRelationships := make(map[string]string) // child file -> parent file

	// First pass: Create all resources and collect parent-child relationships
	err := createAllResourcesInHierarchy(ctx, c, hierarchies, createdResources, parentChildRelationships)
	if err != nil {
		return err
	}

	// Second pass: Apply all owner references and resource refs between parents and children
	err = applyAllRelationships(ctx, c, createdResources, parentChildRelationships, xrdAPIVersion)
	if err != nil {
		return err
	}

	// Debug logging disabled by default. Uncomment to debug hierarchical setup issues.
	// if err := LogResourcesAsYAML(ctx, log, c, createdResources); err != nil {
	// 	log.Info(fmt.Sprintf("Warning: Failed to log resources as YAML: %v\n", err))
	// }

	return nil
}

// Unused but useful for debugging; leave it here.
// LogResourcesAsYAML fetches the latest version of each resource and logs it as YAML.
func LogResourcesAsYAML(ctx context.Context, log logging.Logger, c client.Client, createdResources map[string]*un.Unstructured) error {
	log.Info("\n===== FINAL STATE OF CREATED RESOURCES =====\n\n")

	// Sort the file paths for consistent output order
	filePaths := make([]string, 0, len(createdResources))
	for filePath := range createdResources {
		filePaths = append(filePaths, filePath)
	}

	sort.Strings(filePaths)

	for _, filePath := range filePaths {
		resource := createdResources[filePath]

		// Fetch the latest version of the resource
		latest := &un.Unstructured{}
		latest.SetGroupVersionKind(resource.GroupVersionKind())

		if err := c.Get(ctx, client.ObjectKey{
			Name:      resource.GetName(),
			Namespace: resource.GetNamespace(),
		}, latest); err != nil {
			return fmt.Errorf("failed to get latest version of resource %s/%s: %w",
				resource.GetNamespace(), resource.GetName(), err)
		}

		// Convert to YAML
		yamlData, err := gyaml.Marshal(latest.Object)
		if err != nil {
			return fmt.Errorf("failed to marshal resource to YAML: %w", err)
		}

		// Print the resource file path and its YAML representation
		log.Info(fmt.Sprintf("--- Source: %s\nResourceName: %s/%s\n%s\n\n",
			filePath, latest.GetKind(), latest.GetName(), string(yamlData)))
	}

	log.Info("===== END OF RESOURCES =====\n\n")

	return nil
}

// createAllResourcesInHierarchy creates all resources in a hierarchy and tracks relationships.
func createAllResourcesInHierarchy(ctx context.Context, c client.Client,
	hierarchies []HierarchicalOwnershipRelation,
	createdResources map[string]*un.Unstructured,
	parentChildRelationships map[string]string,
) error {
	for _, hierarchy := range hierarchies {
		// Create the owner resource first. Only a root is still uncreated here, since a nested owner was
		// applied as its parent's composed resource before the recursion reached it. A root XR is
		// created the way a user's manifest would be, never under Crossplane's composed field manager:
		// its spec belongs to whoever applied it.
		_, err := createResourceFromFile(ctx, c, hierarchy.OwnerFile, createdResources, "")
		if err != nil {
			return err
		}

		// Create all owned resources without setting references yet
		for ownedFile, childHierarchy := range hierarchy.OwnedFiles {
			// Track the parent-child relationship
			parentChildRelationships[ownedFile] = hierarchy.OwnerFile

			// Create the owned resource without setting references
			_, err := createResourceFromFile(ctx, c, ownedFile, createdResources, CrossplaneFieldManager)
			if err != nil {
				return err
			}

			// Process nested hierarchies recursively
			if childHierarchy != nil && len(childHierarchy.OwnedFiles) > 0 {
				childHierarchies := []HierarchicalOwnershipRelation{
					{
						OwnerFile:  ownedFile,
						OwnedFiles: childHierarchy.OwnedFiles,
					},
				}

				err := createAllResourcesInHierarchy(ctx, c, childHierarchies,
					createdResources, parentChildRelationships)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// createResourceFromFile creates the one resource a file holds. With a fieldManager it is applied with
// Server-Side Apply (SSA) under that manager: pass CrossplaneFieldManager for a composed resource, as
// Crossplane applies composed resources in production, so tests see SSA-related behaviors like field
// removal. With none it is created plainly, as a setup file is.
func createResourceFromFile(ctx context.Context, c client.Client, path string,
	createdResources map[string]*un.Unstructured, fieldManager string,
) (*un.Unstructured, error) {
	// Check if we've already processed this resource
	if resource, exists := createdResources[path]; exists {
		return resource, nil
	}

	// Read the resource from file
	resources, err := readResourcesFromFile(path)
	if err != nil {
		return nil, err
	}

	// The hierarchy is keyed by file, so a second document would silently go unowned.
	if len(resources) != 1 {
		return nil, fmt.Errorf("file %s holds %d resources; an ownership hierarchy needs exactly one per file", path, len(resources))
	}

	resource := resources[0]

	// A nested XR's composition refs are the nested XR's own reconciler's, never its parent's: the
	// parent's composed apply doesn't carry them, so it must not own them, or a diff of the parent's
	// render would report them removed. Apply them separately, as that reconciler writes them.
	applied := resource

	var reconcilerFields map[int]any
	if fieldManager == CrossplaneFieldManager {
		applied, reconcilerFields = withoutCompositeReconcilerFields(resource)
	}

	if fieldManager != "" {
		if err := applyResourceWithSSA(ctx, c, applied, fieldManager); err != nil {
			return nil, fmt.Errorf("failed to apply resource with SSA: %w", err)
		}
	} else if err := createResources(ctx, c, resources); err != nil {
		return nil, err
	}

	// Get the resource back from the server
	serverResource := &un.Unstructured{}
	serverResource.SetGroupVersionKind(resource.GroupVersionKind())

	if err := c.Get(ctx, client.ObjectKey{
		Name:      resource.GetName(),
		Namespace: resource.GetNamespace(),
	}, serverResource); err != nil {
		return nil, fmt.Errorf("failed to get created resource: %w", err)
	}

	if len(reconcilerFields) > 0 {
		for i, v := range reconcilerFields {
			if err := un.SetNestedField(serverResource.Object, v, compositeReconcilerFields[i]...); err != nil {
				return nil, fmt.Errorf("cannot set %v on %s: %w", compositeReconcilerFields[i], path, err)
			}
		}

		if err := c.Update(ctx, serverResource, client.FieldOwner(CompositeReconcilerFieldManager)); err != nil {
			return nil, fmt.Errorf("failed to write composite reconciler fields of %s: %w", path, err)
		}
	}

	// Store and return the created resource
	createdResources[path] = serverResource

	return serverResource, nil
}

// withoutCompositeReconcilerFields returns a copy of resource without the compositeReconcilerFields it
// sets, and those fields' values keyed by their index in compositeReconcilerFields. resource is not
// mutated.
func withoutCompositeReconcilerFields(resource *un.Unstructured) (*un.Unstructured, map[int]any) {
	out := resource.DeepCopy()
	removed := map[int]any{}

	for i, path := range compositeReconcilerFields {
		if v, found, _ := un.NestedFieldCopy(out.Object, path...); found {
			removed[i] = v
			un.RemoveNestedField(out.Object, path...)
		}
	}

	return out, removed
}

// applyAllRelationships applies all owner references and resource refs.
func applyAllRelationships(ctx context.Context, c client.Client,
	createdResources map[string]*un.Unstructured,
	parentChildRelationships map[string]string,
	xrdAPIVersion XrdAPIVersion,
) error {
	// Process all parent-child relationships
	for childFile, parentFile := range parentChildRelationships {
		childResource := createdResources[childFile]
		parentResource := createdResources[parentFile]

		if childResource == nil || parentResource == nil {
			return fmt.Errorf("missing resource in relationship: parent=%s, child=%s",
				parentFile, childFile)
		}

		// 1. Set the owner reference in the child's metadata
		err := setOwnerReferenceAndUpdate(ctx, c, parentResource, childResource)
		if err != nil {
			return err
		}

		// 2. Add the child resource reference to the parent
		err = addResourceRefAndUpdate(ctx, c, parentResource, childResource, xrdAPIVersion)
		if err != nil {
			return err
		}
	}

	return nil
}

// setOwnerReferenceAndUpdate sets the owner reference in the child and updates it.
// Uses a separate field manager for owner references to avoid conflicts with SSA-created resources.
func setOwnerReferenceAndUpdate(ctx context.Context, c client.Client,
	owner *un.Unstructured, child *un.Unstructured,
) error {
	// Get the latest version of the child
	latestChild := &un.Unstructured{}
	latestChild.SetGroupVersionKind(child.GroupVersionKind())

	err := c.Get(ctx, client.ObjectKey{
		Name:      child.GetName(),
		Namespace: child.GetNamespace(),
	}, latestChild)
	if err != nil {
		return fmt.Errorf("failed to get child resource: %w", err)
	}

	// Set the owner reference
	setOwnerReference(latestChild, owner)

	// Use a separate field manager for owner references to avoid conflicts with the
	// Crossplane field manager that created the resource via SSA.
	err = c.Update(ctx, latestChild, client.FieldOwner("diff.test"))
	if err != nil {
		return fmt.Errorf("failed to update child with owner reference: %w", err)
	}

	return nil
}

// addResourceRefAndUpdate adds a resource reference to the owner and updates it.
func addResourceRefAndUpdate(ctx context.Context, c client.Client,
	owner *un.Unstructured, owned *un.Unstructured, xrdAPIVersion XrdAPIVersion,
) error {
	// Get the latest version of the owner
	latestOwner := &un.Unstructured{}
	latestOwner.SetGroupVersionKind(owner.GroupVersionKind())

	err := c.Get(ctx, client.ObjectKey{
		Name:      owner.GetName(),
		Namespace: owner.GetNamespace(),
	}, latestOwner)
	if err != nil {
		return fmt.Errorf("failed to get owner for updating references: %w", err)
	}

	// Add the resource reference
	err = addResourceRef(latestOwner, owned, xrdAPIVersion)
	if err != nil {
		return fmt.Errorf("unable to add resource ref: %w", err)
	}

	// Use a separate field manager for resource refs to avoid conflicts with the
	// Crossplane field manager that created the resource via SSA.
	want, _, _ := un.NestedSlice(latestOwner.Object, resourceRefsPath(xrdAPIVersion)...)

	err = c.Update(ctx, latestOwner, client.FieldOwner("diff.test"))
	if err != nil {
		return fmt.Errorf("failed to update owner with resource reference: %w", err)
	}

	// The apiserver prunes a field its schema lacks, so resourceRefs written at the wrong API version's
	// path would vanish silently and leave the XR composing nothing.
	if got, _, _ := un.NestedSlice(latestOwner.Object, resourceRefsPath(xrdAPIVersion)...); len(got) != len(want) {
		return fmt.Errorf("%s %s kept %d of %d resourceRefs at %v: is the test case's xrdAPIVersion that of the XRD?",
			owner.GetKind(), owner.GetName(), len(got), len(want), resourceRefsPath(xrdAPIVersion))
	}

	return nil
}

// resourceRefsPath is where an XR of the given XRD API version lists its composed resources.
func resourceRefsPath(xrdAPIVersion XrdAPIVersion) []string {
	if xrdAPIVersion == V1 {
		return []string{"spec", "resourceRefs"}
	}

	return []string{"spec", "crossplane", "resourceRefs"}
}

// localCrossplaneBinary returns the absolute path to a locally-built
// `crossplane` binary at _output/bin/crossplane (relative to cmd/diff/) when
// it exists, or "" when it doesn't. The binary, when present, contains the
// `crossplane internal render` subcommand introduced by upstream PR #7339
// and is exec'd directly for fast in-process rendering. When absent, callers
// pass the empty string through to the diff command, which falls through to
// the docker render engine (slower but works wherever Docker does — including
// CI's WITH DOCKER block in the go-test target).
//
// Local developers wanting the fast path can build the binary with:
//
//	go build -o _output/bin/crossplane github.com/crossplane/crossplane/v2/cmd/crossplane
//
// (run from the repo root). The path is overridable via the
// CROSSPLANE_DIFF_RENDER_BINARY env var when a different layout is needed.
//
// Callers thread the returned path into the diff command via
// --crossplane-render-binary=<path> rather than via a process-global env var,
// so concurrent t.Parallel() tests don't race on shared state.
func localCrossplaneBinary(t *testing.T) string {
	t.Helper()

	if override := os.Getenv("CROSSPLANE_DIFF_RENDER_BINARY"); override != "" {
		return override
	}

	absPath, err := filepath.Abs("../../_output/bin/crossplane")
	if err != nil {
		t.Fatalf("cannot resolve crossplane binary path: %v", err)
	}

	if _, err := os.Stat(absPath); err != nil {
		t.Logf("local crossplane binary not present at %s; falling back to docker render engine. Build it for faster iteration: go build -o _output/bin/crossplane github.com/crossplane/crossplane/v2/cmd/crossplane", absPath)
		return ""
	}

	return absPath
}

// reconcileCompositionRevisions leaves the setup's Compositions and CompositionRevisions as Crossplane's
// composition revision controller does (crossplane internal/controller/apiextensions/composition): every
// revision is controlled by its Composition, and a Composition's current hash is its latest revision,
// creating that revision if no fixture declares it. envtest runs no controllers, so without this a case
// would see revisions no cluster has: uncontrolled ones, or none at all for a Composition.
//
// It also refuses a fixture no cluster could hold: a revision whose Composition the case doesn't declare
// (Crossplane garbage collects a revision with its Composition), or one not named and labelled from the
// hash of the Composition it was cut from, as NewCompositionRevision names it.
func reconcileCompositionRevisions(ctx context.Context, c client.Client) error {
	comps := &xpextv1.CompositionList{}
	if err := c.List(ctx, comps); err != nil {
		return errors.Wrap(err, "cannot list Compositions")
	}

	revs := &xpextv1.CompositionRevisionList{}
	if err := c.List(ctx, revs); err != nil {
		return errors.Wrap(err, "cannot list CompositionRevisions")
	}

	byName := make(map[string]*xpextv1.Composition, len(comps.Items))
	for i := range comps.Items {
		byName[comps.Items[i].GetName()] = &comps.Items[i]
	}

	var problems []string

	for i := range revs.Items {
		rev := &revs.Items[i]
		compName := rev.GetLabels()[xpextv1.LabelCompositionName]

		if _, ok := byName[compName]; !ok {
			problems = append(problems, fmt.Sprintf("CompositionRevision %s belongs to Composition %q, which the case doesn't declare; "+
				"a revision is garbage collected with its Composition", rev.GetName(), compName))

			continue
		}

		name, hash := revisionIdentity(compName, revisionSource(rev).Hash())
		if rev.GetName() != name || rev.GetLabels()[xpextv1.LabelCompositionHash] != hash {
			problems = append(problems, fmt.Sprintf("CompositionRevision %s (hash label %q) is not named from the hash of the "+
				"Composition it was cut from: Crossplane would name it %s with hash label %q",
				rev.GetName(), rev.GetLabels()[xpextv1.LabelCompositionHash], name, hash))
		}
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}

	for i := range comps.Items {
		if err := reconcileRevisionsOf(ctx, c, &comps.Items[i], revs.Items); err != nil {
			return err
		}
	}

	return nil
}

// reconcileRevisionsOf does for one Composition what the composition revision controller's Reconcile
// does: control each of its revisions, then make its current hash its latest revision.
func reconcileRevisionsOf(ctx context.Context, c client.Client, comp *xpextv1.Composition, all []xpextv1.CompositionRevision) error {
	owner := metav1.OwnerReference{
		APIVersion:         xpextv1.CompositionGroupVersionKind.GroupVersion().String(),
		Kind:               xpextv1.CompositionGroupVersionKind.Kind,
		Name:               comp.GetName(),
		UID:                comp.GetUID(),
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}

	var (
		latest int64
		mine   []*xpextv1.CompositionRevision
	)

	for i := range all {
		if all[i].GetLabels()[xpextv1.LabelCompositionName] != comp.GetName() {
			continue
		}

		mine = append(mine, &all[i])
		latest = max(latest, all[i].Spec.Revision)
	}

	_, current := revisionIdentity(comp.GetName(), comp.Hash())
	matched := false

	for _, rev := range mine {
		rev.SetOwnerReferences([]metav1.OwnerReference{owner})

		// A revision matching the current Composition that isn't the latest is promoted to be the latest.
		if rev.GetLabels()[xpextv1.LabelCompositionHash] == current {
			matched = true

			if rev.Spec.Revision != latest {
				rev.Spec.Revision = latest + 1
			}
		}

		if err := c.Update(ctx, rev); err != nil {
			return errors.Wrapf(err, "cannot update CompositionRevision %s", rev.GetName())
		}
	}

	if matched {
		return nil
	}

	rev := newCompositionRevision(comp, latest+1)
	rev.SetOwnerReferences([]metav1.OwnerReference{owner})

	return errors.Wrapf(c.Create(ctx, rev), "cannot create the CompositionRevision of Composition %s", comp.GetName())
}

// newCompositionRevision is crossplane's composition.NewCompositionRevision, which is internal.
func newCompositionRevision(comp *xpextv1.Composition, revision int64) *xpextv1.CompositionRevision {
	name, hash := revisionIdentity(comp.GetName(), comp.Hash())

	labels := maps.Clone(comp.GetLabels())
	if labels == nil {
		labels = map[string]string{}
	}

	labels[xpextv1.LabelCompositionName] = comp.GetName()
	labels[xpextv1.LabelCompositionHash] = hash

	spec := (&xpextv1.GeneratedRevisionSpecConverter{}).ToRevisionSpec(comp.Spec)
	spec.Revision = revision

	rev := &xpextv1.CompositionRevision{Spec: spec}
	rev.SetName(name)
	rev.SetLabels(labels)

	if a := comp.GetAnnotations(); len(a) > 0 {
		rev.SetAnnotations(maps.Clone(a))
	}

	return rev
}

// revisionSource is the Composition a revision was cut from, as far as its hash is concerned: the
// revision's spec, and its labels and annotations, which NewCompositionRevision copies from the
// Composition, without the two labels it stamps.
func revisionSource(rev *xpextv1.CompositionRevision) *xpextv1.Composition {
	comp := &xpextv1.Composition{Spec: (&xpextv1.GeneratedRevisionSpecConverter{}).FromRevisionSpec(rev.Spec)}

	labels := maps.Clone(rev.GetLabels())
	delete(labels, xpextv1.LabelCompositionName)
	delete(labels, xpextv1.LabelCompositionHash)

	if len(labels) > 0 {
		comp.SetLabels(labels)
	}

	comp.SetAnnotations(rev.GetAnnotations())

	return comp
}

// xrType is one composite resource type an XRD in the case defines, and its claim type if it has one.
type xrType struct {
	xr, claim schema.GroupVersionKind // claim is zero when the XRD offers no claim
}

// validateReconciledState refuses a case whose cluster holds what Crossplane would never leave, so a test
// can't exercise a tree no real cluster has (#562). It checks every XR, claim and composed resource the
// setup created, every Composition's Functions, and reports each object it would refuse as
// "<Kind>/<namespace>/<name>: <why>". unreconciled names the objects a case deliberately sets up in a state
// Crossplane would not leave, each with the reason; listing one that isn't refused is itself refused, so
// a stale exemption can't linger.
func validateReconciledState(ctx context.Context, c client.Client, unreconciled map[string]string) error {
	v := &realismValidator{ctx: ctx, c: c, problems: map[string][]string{}}
	if err := v.load(); err != nil {
		return err
	}

	v.checkFunctions()
	v.checkXRs()
	v.checkClaims()

	var out []string

	for id, why := range v.problems {
		if _, ok := unreconciled[id]; ok {
			continue
		}

		for _, w := range why {
			out = append(out, id+": "+w)
		}
	}

	for id := range unreconciled {
		if _, ok := v.problems[id]; !ok {
			out = append(out, id+": listed as deliberately unreconciled, but nothing about it is")
		}
	}

	if len(out) == 0 {
		return nil
	}

	sort.Strings(out)

	return errors.New(strings.Join(out, "\n"))
}

type realismValidator struct {
	ctx      context.Context
	c        client.Client
	problems map[string][]string

	types     []xrType
	comps     map[string]*xpextv1.Composition
	revs      map[string]*xpextv1.CompositionRevision
	functions map[string]bool
	xrs       []*un.Unstructured
	xrsByUID  map[string]*un.Unstructured
}

func objectID(o *un.Unstructured) string {
	return o.GetKind() + "/" + o.GetNamespace() + "/" + o.GetName()
}

func (v *realismValidator) refuse(o *un.Unstructured, format string, args ...any) {
	id := objectID(o)
	v.problems[id] = append(v.problems[id], fmt.Sprintf(format, args...))
}

func (v *realismValidator) list(gvk schema.GroupVersionKind) ([]*un.Unstructured, error) {
	l := &un.UnstructuredList{}
	l.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))

	if err := v.c.List(v.ctx, l); err != nil {
		return nil, errors.Wrapf(err, "cannot list %s", gvk)
	}

	out := make([]*un.Unstructured, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, &l.Items[i])
	}

	return out, nil
}

func (v *realismValidator) load() error {
	for _, version := range []string{"v1", "v2"} {
		xrds, err := v.list(schema.GroupVersionKind{Group: "apiextensions.crossplane.io", Version: version, Kind: "CompositeResourceDefinition"})
		if err != nil {
			return err
		}

		for _, xrd := range xrds {
			if version == "v2" && xrd.GetAnnotations()["internal.crossplane.io/converted-from"] != "" {
				continue
			}

			t := xrTypeOf(xrd)
			if !slices.Contains(v.types, t) {
				v.types = append(v.types, t)
			}
		}
	}

	comps := &xpextv1.CompositionList{}
	if err := v.c.List(v.ctx, comps); err != nil {
		return errors.Wrap(err, "cannot list Compositions")
	}

	v.comps = map[string]*xpextv1.Composition{}
	for i := range comps.Items {
		v.comps[comps.Items[i].GetName()] = &comps.Items[i]
	}

	revs := &xpextv1.CompositionRevisionList{}
	if err := v.c.List(v.ctx, revs); err != nil {
		return errors.Wrap(err, "cannot list CompositionRevisions")
	}

	v.revs = map[string]*xpextv1.CompositionRevision{}
	for i := range revs.Items {
		v.revs[revs.Items[i].GetName()] = &revs.Items[i]
	}

	fns, err := v.list(schema.GroupVersionKind{Group: "pkg.crossplane.io", Version: "v1", Kind: "Function"})
	if err != nil {
		return err
	}

	v.functions = map[string]bool{}
	for _, fn := range fns {
		v.functions[fn.GetName()] = true
	}

	v.xrsByUID = map[string]*un.Unstructured{}

	for _, t := range v.types {
		xrs, err := v.list(t.xr)
		if err != nil {
			return err
		}

		for _, xr := range xrs {
			v.xrs = append(v.xrs, xr)
			v.xrsByUID[string(xr.GetUID())] = xr
		}
	}

	return nil
}

// xrTypeOf reads the XR and claim types an XRD (v1 or v2, as unstructured) defines.
func xrTypeOf(xrd *un.Unstructured) xrType {
	group, _, _ := un.NestedString(xrd.Object, "spec", "group")
	kind, _, _ := un.NestedString(xrd.Object, "spec", "names", "kind")
	claimKind, _, _ := un.NestedString(xrd.Object, "spec", "claimNames", "kind")

	version := ""

	versions, _, _ := un.NestedSlice(xrd.Object, "spec", "versions")
	for _, raw := range versions {
		if m, ok := raw.(map[string]any); ok && m["referenceable"] == true {
			version, _ = m["name"].(string)
		}
	}

	t := xrType{
		xr: schema.GroupVersionKind{Group: group, Version: version, Kind: kind},
	}

	if claimKind != "" {
		t.claim = schema.GroupVersionKind{Group: group, Version: version, Kind: claimKind}
	}

	return t
}

// machinery reads a Crossplane machinery field of an XR or claim at whichever of spec.crossplane.<field>
// and spec.<field> it has.
func machinery(o *un.Unstructured, path ...string) (any, bool) {
	for _, prefix := range [][]string{{"spec", "crossplane"}, {"spec"}} {
		if val, found, _ := un.NestedFieldNoCopy(o.Object, append(append([]string{}, prefix...), path...)...); found {
			return val, true
		}
	}

	return nil, false
}

func machineryString(o *un.Unstructured, path ...string) string {
	val, _ := machinery(o, path...)
	s, _ := val.(string)

	return s
}

func (v *realismValidator) checkFunctions() {
	for _, comp := range v.comps {
		for _, step := range comp.Spec.Pipeline {
			if !v.functions[step.FunctionRef.Name] {
				o := &un.Unstructured{}
				o.SetKind("Composition")
				o.SetName(comp.GetName())
				v.refuse(o, "step %q uses Function %q, which the case doesn't declare", step.Step, step.FunctionRef.Name)
			}
		}
	}
}

// rootOf is the XR at the top of xr's composition tree: xr itself unless an XR controls it.
func (v *realismValidator) rootOf(xr *un.Unstructured) *un.Unstructured {
	for range 32 {
		ctrl := metav1.GetControllerOf(xr)
		if ctrl == nil {
			return xr
		}

		parent, ok := v.xrsByUID[string(ctrl.UID)]
		if !ok {
			return xr
		}

		xr = parent
	}

	return xr
}

func (v *realismValidator) checkXRs() {
	for _, xr := range v.xrs {
		v.checkXRMetadata(xr)
		v.checkCompositionRefs(xr)
		v.checkComposed(xr)
	}
}

func (v *realismValidator) checkXRMetadata(xr *un.Unstructured) {
	// Every XR is labelled with the name of the XR at the root of its tree (composite/api.go,
	// APINamingConfigurator, and RenderComposedResourceMetadata for a nested XR).
	if want := v.rootOf(xr).GetName(); xr.GetLabels()["crossplane.io/composite"] != want {
		v.refuse(xr, "label crossplane.io/composite is %q; Crossplane labels it %q", xr.GetLabels()["crossplane.io/composite"], want)
	}

	// An XR bound to a claim names it in claimRef and carries its claim labels.
	claimName := xr.GetLabels()["crossplane.io/claim-name"]

	ref, hasRef := machinery(xr, "claimRef")
	if (claimName != "") != hasRef && metav1.GetControllerOf(xr) == nil {
		v.refuse(xr, "claim labels and spec.claimRef disagree about whether a claim is bound (claim-name label %q, claimRef %v)", claimName, ref)
	}
}

func (v *realismValidator) checkCompositionRefs(xr *un.Unstructured) {
	targeted := false

	for _, comp := range v.comps {
		if comp.Spec.CompositeTypeRef.APIVersion == xr.GetAPIVersion() && comp.Spec.CompositeTypeRef.Kind == xr.GetKind() {
			targeted = true
		}
	}

	compName := machineryString(xr, "compositionRef", "name")

	switch {
	case compName == "" && targeted:
		v.refuse(xr, "has no compositionRef, which Crossplane's composite reconciler writes once it selects a Composition")
		return
	case compName == "":
		return
	}

	comp, ok := v.comps[compName]
	if !ok {
		v.refuse(xr, "compositionRef names Composition %q, which the case doesn't declare", compName)
		return
	}

	if comp.Spec.CompositeTypeRef.APIVersion != xr.GetAPIVersion() || comp.Spec.CompositeTypeRef.Kind != xr.GetKind() {
		v.refuse(xr, "compositionRef names Composition %q, which composes %s %s", compName, comp.Spec.CompositeTypeRef.APIVersion, comp.Spec.CompositeTypeRef.Kind)
	}

	revName := machineryString(xr, "compositionRevisionRef", "name")
	if revName == "" {
		if !v.noRevisionSelectable(xr, compName) {
			v.refuse(xr, "has no compositionRevisionRef, which Crossplane's composite reconciler writes once it selects a CompositionRevision")
		}

		return
	}

	rev, ok := v.revs[revName]
	if !ok {
		v.refuse(xr, "compositionRevisionRef names CompositionRevision %q, which the case doesn't declare", revName)
		return
	}

	if rev.GetLabels()[xpextv1.LabelCompositionName] != compName {
		v.refuse(xr, "compositionRevisionRef names %q, a revision of %q rather than of its Composition %q", revName, rev.GetLabels()[xpextv1.LabelCompositionName], compName)
	}
}

// noRevisionSelectable reports whether Crossplane could select no revision of compName for xr, so it
// writes no compositionRevisionRef: its compositionRevisionSelector matches none.
func (v *realismValidator) noRevisionSelectable(xr *un.Unstructured, compName string) bool {
	raw, ok := machinery(xr, "compositionRevisionSelector")
	if !ok {
		return false
	}

	ls := &metav1.LabelSelector{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw.(map[string]any), ls); err != nil {
		return false
	}

	sel, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {
		return false
	}

	for _, rev := range v.revs {
		if rev.GetLabels()[xpextv1.LabelCompositionName] == compName && sel.Matches(labels.Set(rev.GetLabels())) {
			return false
		}
	}

	return true
}

// checkComposed checks each resource xr's resourceRefs name against the metadata Crossplane's
// RenderComposedResourceMetadata gives a composed resource.
func (v *realismValidator) checkComposed(xr *un.Unstructured) {
	refs, _ := machinery(xr, "resourceRefs")
	list, _ := refs.([]any)

	for _, raw := range list {
		ref, _ := raw.(map[string]any)
		apiVersion, _ := ref["apiVersion"].(string)
		kind, _ := ref["kind"].(string)
		name, _ := ref["name"].(string)
		namespace, _ := ref["namespace"].(string)

		if namespace == "" {
			namespace = xr.GetNamespace()
		}

		cd := &un.Unstructured{}
		cd.SetAPIVersion(apiVersion)
		cd.SetKind(kind)

		if err := v.c.Get(v.ctx, client.ObjectKey{Namespace: namespace, Name: name}, cd); err != nil {
			v.refuse(xr, "resourceRefs names %s %s/%s, which the case doesn't declare (%v)", kind, namespace, name, err)
			continue
		}

		if ctrl := metav1.GetControllerOf(cd); ctrl == nil || ctrl.UID != xr.GetUID() {
			v.refuse(cd, "is named in %s's resourceRefs, but that XR doesn't control it", objectID(xr))
			continue
		}

		annotations := cd.GetAnnotations()
		if annotations["crossplane.io/composition-resource-name"] == "" {
			v.refuse(cd, "has no crossplane.io/composition-resource-name annotation, which Crossplane sets on every composed resource")
		}

		if _, ok := annotations["gotemplating.fn.crossplane.io/composition-resource-name"]; ok {
			v.refuse(cd, "carries gotemplating.fn.crossplane.io/composition-resource-name, which function-go-templating strips from what it renders")
		}

		cdLabels := cd.GetLabels()
		for _, key := range []string{"crossplane.io/composite", "crossplane.io/claim-name", "crossplane.io/claim-namespace"} {
			if cdLabels[key] != xr.GetLabels()[key] {
				v.refuse(cd, "label %s is %q, but its XR's is %q; Crossplane copies it from the XR", key, cdLabels[key], xr.GetLabels()[key])
			}
		}

		if xr.GetNamespace() != "" && cd.GetNamespace() != "" && cd.GetNamespace() != xr.GetNamespace() {
			v.refuse(cd, "is in namespace %q, but a namespaced XR composes only in its own (%q)", cd.GetNamespace(), xr.GetNamespace())
		}
	}
}

// checkClaims checks each claim against its backing XR, as Crossplane's claim syncer leaves the pair
// (crossplane internal/controller/apiextensions/claim/syncer_ssa.go).
func (v *realismValidator) checkClaims() {
	for _, t := range v.types {
		if t.claim.Empty() {
			continue
		}

		claims, err := v.list(t.claim)
		if err != nil {
			continue
		}

		for _, cm := range claims {
			v.checkClaim(cm)
		}
	}
}

func (v *realismValidator) checkClaim(cm *un.Unstructured) {
	ref, _, _ := un.NestedMap(cm.Object, "spec", "resourceRef")
	if ref == nil {
		v.refuse(cm, "has no spec.resourceRef: the claim syncer binds a claim to its XR before it creates the XR")
		return
	}

	var xr *un.Unstructured

	for _, candidate := range v.xrs {
		if candidate.GetAPIVersion() == ref["apiVersion"] && candidate.GetKind() == ref["kind"] && candidate.GetName() == ref["name"] {
			xr = candidate
		}
	}

	if xr == nil {
		v.refuse(cm, "spec.resourceRef names %v %v %v, which the case doesn't declare", ref["apiVersion"], ref["kind"], ref["name"])
		return
	}

	claimRef, _, _ := un.NestedMap(xr.Object, "spec", "claimRef")
	if claimRef["name"] != cm.GetName() || claimRef["namespace"] != cm.GetNamespace() || claimRef["kind"] != cm.GetKind() {
		v.refuse(cm, "its XR %s names claim %v in spec.claimRef", objectID(xr), claimRef)
	}

	if xr.GetLabels()["crossplane.io/claim-name"] != cm.GetName() || xr.GetLabels()["crossplane.io/claim-namespace"] != cm.GetNamespace() {
		v.refuse(cm, "its XR %s is not labelled with its name and namespace", objectID(xr))
	}

	// A composition ref propagates XR -> claim when the claim has none, claim -> XR when it has one.
	if xrComp := machineryString(xr, "compositionRef", "name"); xrComp != "" {
		if got := machineryString(cm, "compositionRef", "name"); got != xrComp {
			v.refuse(cm, "compositionRef is %q, but its XR's is %q, which the claim syncer keeps in step", got, xrComp)
		}
	}

	xrRev := machineryString(xr, "compositionRevisionRef", "name")
	claimRev := machineryString(cm, "compositionRevisionRef", "name")

	switch machineryString(xr, "compositionUpdatePolicy") {
	case "Manual":
		// Under Manual the claim's ref propagates claim -> XR.
		if claimRev != "" && claimRev != xrRev {
			v.refuse(cm, "compositionRevisionRef is %q, but under Manual the claim syncer gives its XR the same ref (it has %q)", claimRev, xrRev)
		}
	default:
		// Under Automatic the XR's ref propagates XR -> claim.
		if xrRev != "" && claimRev != xrRev {
			v.refuse(cm, "compositionRevisionRef is %q, but under Automatic the claim syncer copies its XR's, %q", claimRev, xrRev)
		}
	}

	// The claim syncer copies the claim's spec, minus the claim-only fields, onto the XR.
	spec, _, _ := un.NestedMap(cm.Object, "spec")
	for key, val := range spec {
		switch key {
		case "resourceRef", "compositeDeletePolicy", "compositionRef", "compositionRevisionRef", "compositionSelector",
			"compositionRevisionSelector", "compositionUpdatePolicy", "writeConnectionSecretToRef", "publishConnectionDetailsTo":
			continue
		}

		if got, found, _ := un.NestedFieldNoCopy(xr.Object, "spec", key); !found || !equality.Semantic.DeepEqual(got, val) {
			v.refuse(cm, "spec.%s is %v, but its XR's is %v; the claim syncer copies it onto the XR", key, val, got)
		}
	}
}

// revisionIdentity is the name and hash label NewCompositionRevision gives the revision of a Composition
// with the given name and Composition.Hash().
func revisionIdentity(compName, hash string) (string, string) {
	if len(hash) >= 63 {
		hash = hash[:63]
	}

	suffix := hash
	if len(suffix) >= 7 {
		suffix = suffix[:7]
	}

	return compName + "-" + suffix, hash
}
