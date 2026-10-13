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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"

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

// applyResourcesFromFiles creates the resources in YAML files in order, each filled in by d just before
// it is created, so it can derive from what precedes it. No resource should already exist.
func applyResourcesFromFiles(ctx context.Context, c client.Client, d *crossplaneDefaults, paths []string) error {
	for _, path := range paths {
		resources, err := readResourcesFromFile(path)
		if err != nil {
			return fmt.Errorf("failed to read resources from %s: %w", path, err)
		}

		for _, r := range resources {
			if err := d.fill(ctx, r, nil); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}

			if err := createResources(ctx, c, []*un.Unstructured{r}); err != nil {
				return err
			}
		}
	}

	return nil
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
func applyHierarchicalOwnership(ctx context.Context, _ logging.Logger, c client.Client, d *crossplaneDefaults, xrdAPIVersion XrdAPIVersion, hierarchies []HierarchicalOwnershipRelation) error {
	// Map to store created resources by file path
	createdResources := make(map[string]*un.Unstructured)
	// Map to track parent-child relationships for establishing resourceRefs
	parentChildRelationships := make(map[string]string) // child file -> parent file

	// First pass: Create all resources and collect parent-child relationships
	err := createAllResourcesInHierarchy(ctx, c, d, hierarchies, createdResources, parentChildRelationships)
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
func createAllResourcesInHierarchy(ctx context.Context, c client.Client, d *crossplaneDefaults,
	hierarchies []HierarchicalOwnershipRelation,
	createdResources map[string]*un.Unstructured,
	parentChildRelationships map[string]string,
) error {
	for _, hierarchy := range hierarchies {
		// Create the owner resource first. Only a root is still uncreated here, since a nested owner was
		// applied as its parent's composed resource before the recursion reached it. A root XR is
		// created the way a user's manifest would be, never under Crossplane's composed field manager:
		// its spec belongs to whoever applied it.
		owner, err := createResourceFromFile(ctx, c, d, hierarchy.OwnerFile, nil, createdResources, "")
		if err != nil {
			return err
		}

		// Create all owned resources without setting references yet
		for ownedFile, childHierarchy := range hierarchy.OwnedFiles {
			// Track the parent-child relationship
			parentChildRelationships[ownedFile] = hierarchy.OwnerFile

			// Create the owned resource without setting references
			_, err := createResourceFromFile(ctx, c, d, ownedFile, owner, createdResources, CrossplaneFieldManager)
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

				err := createAllResourcesInHierarchy(ctx, c, d, childHierarchies,
					createdResources, parentChildRelationships)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// createResourceFromFile creates the one resource a file holds, filled in by d as parent's composed
// resource (parent is nil for a root). With a fieldManager it is applied with Server-Side Apply (SSA)
// under that manager: pass CrossplaneFieldManager for a composed resource, as Crossplane applies composed
// resources in production, so tests see SSA-related behaviors like field removal. With none it is
// created plainly, as a setup file is.
func createResourceFromFile(ctx context.Context, c client.Client, d *crossplaneDefaults, path string,
	parent *un.Unstructured, createdResources map[string]*un.Unstructured, fieldManager string,
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

	if err := d.fill(ctx, resource, parent); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

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

// reconcileCompositionRevisions does what Crossplane's composition revision controller would, since
// envtest runs none: it gives each revision a controller reference to its Composition, and creates the
// revision of a Composition's current hash if no fixture declares it.
func reconcileCompositionRevisions(ctx context.Context, c client.Client) error {
	comps := &xpextv1.CompositionList{}
	revs := &xpextv1.CompositionRevisionList{}

	if err := c.List(ctx, comps); err != nil {
		return err
	}

	if err := c.List(ctx, revs); err != nil {
		return err
	}

	for i := range comps.Items {
		comp := &comps.Items[i]
		_, current := revisionIdentity(comp.GetName(), comp.Hash())
		owner := meta.AsController(meta.TypedReferenceTo(comp, xpextv1.CompositionGroupVersionKind))
		latest, found := int64(0), false

		for j := range revs.Items {
			rev := &revs.Items[j]
			if rev.GetLabels()[xpextv1.LabelCompositionName] != comp.GetName() {
				continue
			}

			latest = max(latest, rev.Spec.Revision)
			found = found || rev.GetLabels()[xpextv1.LabelCompositionHash] == current

			meta.AddOwnerReference(rev, owner)

			if err := c.Update(ctx, rev); err != nil {
				return err
			}
		}

		if !found {
			if err := c.Create(ctx, newCompositionRevision(comp, latest+1, owner)); err != nil {
				return err
			}
		}
	}

	return nil
}

// newCompositionRevision is crossplane's composition.NewCompositionRevision, which is internal.
func newCompositionRevision(comp *xpextv1.Composition, revision int64, owner metav1.OwnerReference) *xpextv1.CompositionRevision {
	name, hash := revisionIdentity(comp.GetName(), comp.Hash())

	rev := &xpextv1.CompositionRevision{Spec: (&xpextv1.GeneratedRevisionSpecConverter{}).ToRevisionSpec(comp.Spec)}
	rev.Spec.Revision = revision
	rev.SetName(name)
	rev.SetLabels(maps.Clone(comp.GetLabels()))
	meta.AddLabels(rev, map[string]string{xpextv1.LabelCompositionName: comp.GetName(), xpextv1.LabelCompositionHash: hash})
	rev.SetAnnotations(maps.Clone(comp.GetAnnotations()))
	meta.AddOwnerReference(rev, owner)

	return rev
}

// revisionIdentity is the name and hash label NewCompositionRevision gives the revision of a Composition
// with the given name and Composition.Hash().
func revisionIdentity(compName, hash string) (string, string) {
	hash = hash[:min(63, len(hash))]
	return compName + "-" + hash[:min(7, len(hash))], hash
}

// composedLabels are the labels RenderComposedResourceMetadata copies from an XR to what it composes.
var composedLabels = []string{"crossplane.io/composite", "crossplane.io/claim-name", "crossplane.io/claim-namespace"}

// crossplaneDefaults fills in, on each fixture a case sets up, what Crossplane would have written to it
// wherever the fixture leaves it out, just as envtestCRDs derives XR and claim CRDs from their XRDs. A
// value the fixture sets always wins: that is how a case sets up a state Crossplane wouldn't leave, such as
// a stale or pinned revision ref, or another XR's labels.
type crossplaneDefaults struct {
	c       client.Client
	xrKinds map[schema.GroupKind]bool // the XR kinds the case's XRDs define
}

func newCrossplaneDefaults(c client.Client, crds []*extv1.CustomResourceDefinition) *crossplaneDefaults {
	d := &crossplaneDefaults{c: c, xrKinds: map[schema.GroupKind]bool{}}

	for _, crd := range crds {
		if slices.Contains(crd.Spec.Names.Categories, "composite") {
			d.xrKinds[schema.GroupKind{Group: crd.Spec.Group, Kind: crd.Spec.Names.Kind}] = true
		}
	}

	return d
}

// fill fills in what Crossplane would have written to o, where o doesn't set it:
//   - a CompositionRevision's name and hash label, which NewCompositionRevision derives from the
//     Composition it was cut from: the one its crossplane.io/composition-name label names, with the
//     revision's own spec, labels and annotations;
//   - an XR's crossplane.io/composite label, its own name; or, on a resource an XR composes (a nested XR
//     included), the XR's composite and claim labels, as RenderComposedResourceMetadata copies them;
//   - the compositionRevisionRef of an XR that names a Composition but neither a revision nor a
//     revision selector: the Composition's current revision, which reconcileCompositionRevisions creates.
func (d *crossplaneDefaults) fill(ctx context.Context, o, parent *un.Unstructured) error {
	gk := o.GroupVersionKind().GroupKind()
	if gk == xpextv1.CompositionRevisionGroupVersionKind.GroupKind() && o.GetName() == "" {
		return d.nameRevision(ctx, o)
	}

	labels := map[string]string{}

	switch {
	case parent != nil:
		for _, k := range composedLabels {
			if v, ok := parent.GetLabels()[k]; ok {
				labels[k] = v
			}
		}
	case d.xrKinds[gk]:
		labels["crossplane.io/composite"] = o.GetName()
	}

	maps.Copy(labels, o.GetLabels())

	if len(labels) > 0 {
		o.SetLabels(labels)
	}

	// The refs are at spec.crossplane in a v2 XR, and at spec in a v1 one.
	for _, base := range [][]string{{"spec", "crossplane"}, {"spec"}} {
		field := func(name ...string) []string { return slices.Concat(base, name) }

		comp, named, _ := un.NestedString(o.Object, field("compositionRef", "name")...)
		_, hasRef, _ := un.NestedFieldNoCopy(o.Object, field("compositionRevisionRef")...)
		_, hasSelector, _ := un.NestedFieldNoCopy(o.Object, field("compositionRevisionSelector")...)

		if !d.xrKinds[gk] || !named || hasRef || hasSelector {
			continue
		}

		current := &xpextv1.Composition{}
		if err := d.c.Get(ctx, client.ObjectKey{Name: comp}, current); err != nil {
			return fmt.Errorf("cannot derive the compositionRevisionRef of %s %s: %w", o.GetKind(), o.GetName(), err)
		}

		name, _ := revisionIdentity(comp, current.Hash())

		return un.SetNestedField(o.Object, name, field("compositionRevisionRef", "name")...)
	}

	return nil
}

// nameRevision names and hash-labels a CompositionRevision as NewCompositionRevision would have, from the
// Composition it was cut from: its spec, as the apiserver defaults it, and its own labels and annotations.
func (d *crossplaneDefaults) nameRevision(ctx context.Context, o *un.Unstructured) error {
	compName := o.GetLabels()[xpextv1.LabelCompositionName]
	if compName == "" {
		return errors.Errorf("a CompositionRevision without a name needs a %s label to be named from", xpextv1.LabelCompositionName)
	}

	probe := o.DeepCopy()
	probe.SetGenerateName("revision-")

	if err := d.c.Create(ctx, probe, client.DryRunAll); err != nil {
		return err
	}

	rev := &xpextv1.CompositionRevision{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(probe.Object, rev); err != nil {
		return err
	}

	src := &xpextv1.Composition{Spec: (&xpextv1.GeneratedRevisionSpecConverter{}).FromRevisionSpec(rev.Spec)}
	labels := maps.Clone(rev.GetLabels())
	delete(labels, xpextv1.LabelCompositionName)
	delete(labels, xpextv1.LabelCompositionHash)

	if len(labels) > 0 {
		src.SetLabels(labels)
	}

	src.SetAnnotations(rev.GetAnnotations())

	name, hash := revisionIdentity(compName, src.Hash())
	o.SetName(name)
	meta.AddLabels(o, map[string]string{xpextv1.LabelCompositionHash: hash})

	return nil
}
