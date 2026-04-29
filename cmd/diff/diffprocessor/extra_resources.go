package diffprocessor

import (
	"context"
	"regexp"
	"strings"

	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
)

var (
	templateDirectiveRe = regexp.MustCompile(`\{\{.*?\}\}`)

	extraResourcesBlockRe = regexp.MustCompile(
		`(?ms)kind:\s+ExtraResources\s+requirements:\s*\n(.*?)(?:^---|\z)`,
	)

	gvkPairRe = regexp.MustCompile(`apiVersion:\s+(\S+)\s*\n\s+kind:\s+(\S+)`)
)

// extractExtraResourceGVKs scans a composition's pipeline steps for
// function-go-templating ExtraResources requirements and returns the
// referenced GVKs. These identify resource types that must be pre-fetched
// so the render function's FilteringFetcher can satisfy them.
func extractExtraResourceGVKs(comp *apiextensionsv1.Composition) []schema.GroupVersionKind {
	seen := make(map[schema.GroupVersionKind]bool)

	var gvks []schema.GroupVersionKind

	for _, step := range comp.Spec.Pipeline {
		if step.Input == nil || len(step.Input.Raw) == 0 {
			continue
		}

		tmpl := extractGoTemplate(step.Input.Raw)
		if tmpl == "" {
			continue
		}

		for _, gvk := range extractGVKsFromTemplate(tmpl) {
			if !seen[gvk] {
				seen[gvk] = true
				gvks = append(gvks, gvk)
			}
		}
	}

	return gvks
}

// extractGoTemplate parses the raw function input and returns the Go template
// string if the input is a function-go-templating GoTemplate.
func extractGoTemplate(raw []byte) string {
	var input struct {
		Kind   string `json:"kind"`
		Inline struct {
			Template string `json:"template"`
		} `json:"inline"`
	}

	if err := yaml.Unmarshal(raw, &input); err != nil {
		return ""
	}

	if input.Kind != "GoTemplate" {
		return ""
	}

	return input.Inline.Template
}

// extractGVKsFromTemplate finds ExtraResources requirements blocks in a
// Go template string and extracts apiVersion/kind pairs. Template directives
// are stripped first so they don't interfere with YAML-like pattern matching.
func extractGVKsFromTemplate(tmpl string) []schema.GroupVersionKind {
	stripped := templateDirectiveRe.ReplaceAllString(tmpl, "")

	blocks := extraResourcesBlockRe.FindAllStringSubmatch(stripped, -1)
	if len(blocks) == 0 {
		return nil
	}

	var gvks []schema.GroupVersionKind

	for _, block := range blocks {
		reqContent := block[1]
		pairs := gvkPairRe.FindAllStringSubmatch(reqContent, -1)

		for _, pair := range pairs {
			apiVersion, kind := strings.TrimSpace(pair[1]), strings.TrimSpace(pair[2])
			gvk := parseGVK(apiVersion, kind)
			gvks = append(gvks, gvk)
		}
	}

	return gvks
}

func parseGVK(apiVersion, kind string) schema.GroupVersionKind {
	group, version := parseAPIVersion(apiVersion)
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

// prefetchExtraResources lists resources of the given GVKs from the cluster.
// For namespaced resources, queries are scoped to xrNamespace when non-empty
// (Crossplane v2 namespaced XRs) to avoid expensive cluster-wide list calls.
// Errors for individual GVKs are logged and skipped so that missing CRDs
// don't block rendering of compositions that don't depend on them.
func (p *RequirementsProvider) prefetchExtraResources(
	ctx context.Context,
	comp *apiextensionsv1.Composition,
	xrNamespace string,
) []*un.Unstructured {
	gvks := extractExtraResourceGVKs(comp)
	if len(gvks) == 0 {
		return nil
	}

	var all []*un.Unstructured

	for _, gvk := range gvks {
		namespace := ""

		if xrNamespace != "" {
			namespaced, err := p.client.IsNamespacedResource(ctx, gvk)
			if err != nil {
				p.logger.Debug("Cannot determine resource scope, listing cluster-wide",
					"gvk", gvk.String(), "error", err)
			} else if namespaced {
				namespace = xrNamespace
			}
		}

		p.logger.Debug("Pre-fetching ExtraResources for composition",
			"gvk", gvk.String(),
			"namespace", namespace,
			"composition", comp.GetName())

		resources, err := p.client.ListResources(ctx, gvk, namespace)
		if err != nil {
			p.logger.Debug("Cannot pre-fetch ExtraResources, skipping",
				"gvk", gvk.String(), "error", err)

			continue
		}

		all = append(all, resources...)
	}

	if len(all) > 0 {
		p.logger.Debug("Pre-fetched ExtraResources",
			"composition", comp.GetName(),
			"resourceCount", len(all),
			"gvkCount", len(gvks))
		p.cacheResources(all)
	}

	return all
}
