# crossplane-diff

A standalone tool for visualizing the differences between Crossplane resources in YAML files and their resulting state
when applied to a live Kubernetes cluster. Similar to `kubectl diff` but with specific enhancements for Crossplane
resources, particularly Composite Resources (XRs) and their composed resources.

## Overview

The `crossplane-diff` tool helps you:

- **Preview changes** before applying Crossplane resources to a cluster
- **Visualize differences** at multiple levels: both the XR itself and all downstream composed resources
- **Analyze composition impact** showing how composition changes affect existing XRs in the cluster
- **Support complex compositions** including functions, requirements, and environment configurations
- **Handle both Crossplane v1 and v2** resource definitions, including namespaced composite resources
- **Detect resources that would be removed** by your changes

## Installation

### Installing the CLI

You can install the latest version of **crossplane-diff** automatically using the provided install script:

```bash
curl -sL "https://raw.githubusercontent.com/crossplane-contrib/crossplane-diff/main/install.sh" | sh
```

The script detects your operating system and CPU architecture and downloads the latest release from GitHub.

### Installing the CLI with a specific Version

You can set the VERSION environment variable before running the script:

```bash
curl -sL "https://raw.githubusercontent.com/crossplane-contrib/crossplane-diff/main/install.sh" | VERSION=v0.3.1 sh
```

### Building the CLI

```bash
git clone https://github.com/crossplane-contrib/crossplane-diff.git
cd crossplane-diff
earthly -P +build
```

## Usage

### XR Diff - Preview Changes to Composite Resources

The `xr` command supports both Composite Resources (XRs) and Claims. You can pass either type directly as input.

```bash
# Show changes that would result from applying an XR from a file
crossplane-diff xr xr.yaml

# Show changes for a Claim
crossplane-diff xr claim.yaml

# Show changes from stdin
cat xr.yaml | crossplane-diff xr -

# Process multiple files (can mix XRs and Claims)
crossplane-diff xr xr1.yaml claim1.yaml xr2.yaml

# Use a specific kubeconfig context
crossplane-diff xr xr.yaml --context staging

# Show changes in a compact format with minimal context
crossplane-diff xr xr.yaml --compact

# Disable color output
crossplane-diff xr xr.yaml --no-color

# Output in JSON format (for CI/CD pipelines or programmatic processing)
crossplane-diff xr xr.yaml --output json

# Output in YAML format
crossplane-diff xr xr.yaml -o yaml

# Ignore specific fields in diffs (useful for filtering out metadata like ArgoCD annotations)
crossplane-diff xr xr.yaml \
  --ignore-paths 'metadata.annotations[argocd.argoproj.io/tracking-id]' \
  --ignore-paths 'metadata.labels[argocd.argoproj.io/instance]'

# Show eventual state with function-sequencer (all stages, not just first)
crossplane-diff xr xr.yaml --eventual-state

# Don't dry-run create added resources, so no 'create' permission is needed. Their +++ diffs
# then show the rendered output rather than what the apiserver would store (no server-side
# defaulting, no mutating admission), and say so via dryRun.skipReason in JSON/YAML output.
# Also available on comp. See Required Permissions.
crossplane-diff xr xr.yaml --dry-run-on=existing
```

If the XR's counterpart in the cluster is being deleted (it has a `metadata.deletionTimestamp`),
`xr` still emits the diff — you asked about that specific resource — but raises a warning noting that
the comparison is against a resource that is going away. `comp` takes the opposite approach and
excludes such composites from impact analysis entirely, since a deleting composite can never adopt
the composition change being diffed.

### Warnings

Some conditions are worth telling you about without invalidating the diff or stopping the run. These
are written to stderr as `WARNING:` lines, and — in `--output json`/`yaml` — also carried in a
top-level `warnings[]` array so CI tooling can read them:

```
WARNING: Resource already belongs to another composite. Applying this diff will assume ownership! (currentOwner=other-xr, newOwner=my-xr, resource=Bucket/my-bucket)
```

```json
{
  "warnings": [
    {
      "message": "Some function credential secrets could not be fetched from cluster",
      "context": {
        "composition": "xbuckets.example.org",
        "attempted": "2",
        "fetched": "1",
        "hint": "Use --function-credentials to provide secrets that don't exist on cluster"
      }
    }
  ]
}
```

Warnings deliberately do **not** affect the exit code — gate CI on `errors[]`, not `warnings[]`. They
are emitted when raised rather than at the end of the run, so a warning is still reported if a later
step fails, and appears in step with the work that produced it. Today they cover: a composed resource
that belongs to a different composite (applying would take ownership), a nested XR whose composition
could not be found (it will compose nothing), function credentials that could not be fetched (the
render may not reflect reality), leftover function containers, an added resource that could not be
verified against the apiserver (see [Required Permissions](#required-permissions)), and the
deleting-XR case above.

### Composition Diff - Analyze Impact of Composition Changes

The `comp` command analyzes how composition changes affect all Composite Resources (both XRs and Claims) that use the composition. The tool automatically discovers and displays impacts on both direct XRs and any Claims that reference them.

```bash
# Show impact of updated composition on all XRs and Claims using it
crossplane-diff comp updated-composition.yaml

# Show impact of multiple composition changes
crossplane-diff comp comp1.yaml comp2.yaml comp3.yaml

# Use a specific kubeconfig context
crossplane-diff comp updated-composition.yaml --context production

# Show impact only on XRs in a specific namespace
crossplane-diff comp updated-composition.yaml -n production

# Limit impact analysis to specific composites — useful for fast PR-time validation
# against a representative subset of XRs/Claims, or for debugging against a single composite.
# Format is [namespace/]name; bare name means cluster-scoped (v1 XRs and v2 cluster-scoped XRs).
crossplane-diff comp updated-composition.yaml --resource=default/my-claim
crossplane-diff comp updated-composition.yaml --resource=default/xr-1,default/xr-2
# Note: --resource cannot be combined with --namespace. Composites that would not adopt the diffed
# composition are surfaced with status "filtered" and a "filterReason": Manual update policy
# ("manual_policy") unless --include-manual is passed, a compositionRevisionSelector that does
# not match the composition's labels ("revision_selector_mismatch"), or the composite being
# deleted ("deleting").

# Include XRs with Manual update policy (pinned revisions).
# Note: --include-manual only affects Manual-policy XRs. An Automatic XR whose
# compositionRevisionSelector does not match the composition's labels stays filtered even with this
# flag, because it would not select the resulting revision. To preview against a different revision
# label (e.g. a "preview" channel), edit the composition's labels in your CI runner (jq/yq) and run
# comp again — the composition file's labels are the authoritative prediction of the new revision.
crossplane-diff comp updated-composition.yaml --include-manual

# Choose the smallest composition change that triggers per-composite impact analysis. The default is
# any-change: the composites are evaluated whenever anything Crossplane hashes into the composition's
# identity differs — labels and annotations as well as spec.
#
# Evaluate affected composites even when the composition is identical to the cluster's.
# By default that analysis is skipped: applying an unchanged composition creates no new
# CompositionRevision, so nothing would adopt it, and any downstream delta found would be caused
# by something else (drift, convergence lag, or a modeling artifact of this tool) while being
# presented as this composition's impact. Opt in for a pre-edit "is my cluster converged?" baseline.
crossplane-diff comp unchanged-composition.yaml --analyze-on=always

# Only evaluate composites when the composition's *spec* changes, skipping the render-per-composite
# cost for a metadata-only edit. Note that such an edit still creates a new CompositionRevision that
# Automatic composites re-point to, so this asserts none of your compositions can observe a revision's
# identity (via the XR's compositionRevisionRef, or a compositionRevisionSelector matching revision
# labels). Either way revisionImpact in JSON/YAML output reports the revision.
crossplane-diff comp updated-composition.yaml --analyze-on=spec-change

# Collapse each changed composition to a single change-marker line (human output only;
# JSON/YAML keeps full detail), keeping the affected XRs and their downstream diffs
crossplane-diff comp updated-composition.yaml --minimize-composition

# Ignore specific fields in diffs (useful for filtering out metadata like ArgoCD annotations)
crossplane-diff comp updated-composition.yaml \
  --ignore-paths 'metadata.annotations[argocd.argoproj.io/tracking-id]' \
  --ignore-paths 'metadata.labels[argocd.argoproj.io/instance]'

# Output in JSON format (for CI/CD pipelines or programmatic processing)
crossplane-diff comp updated-composition.yaml --output json

# Output in YAML format
crossplane-diff comp updated-composition.yaml -o yaml

# Show eventual state with function-sequencer (all stages, not just first)
crossplane-diff comp updated-composition.yaml --eventual-state
```

### Command Options

#### `xr` - Diff Composite Resources

```
crossplane-diff xr [<files> ...] [flags]

Arguments:
  [<files> ...]    YAML files containing Composite Resources (XRs) or Claims to diff.

Flags:
  -h, --help                   Show context-sensitive help.
      --verbose                Print verbose logging statements.
      --context=STRING         Kubernetes context to use (defaults to current context).
  -o, --output=diff            Output format: diff (human-readable), json, or yaml.
      --no-color               Disable colorized output.
      --compact                Show compact diffs with minimal context.
      --max-nested-depth=10    Maximum depth for nested XR recursion.
      --max-iterations=20      Maximum render iterations for requirements resolution
                               or eventual-state simulation. Increase for complex
                               pipelines that need more cycles to converge.
      --timeout=1m             How long to run before timing out.
      --ignore-paths=STRING,... Paths to ignore in diffs. Supports simple paths
                               (e.g., 'metadata.annotations') and map key paths with
                               bracket notation (e.g., 'metadata.annotations[key]').
                               Can be specified multiple times.
      --function-credentials=PATH  Path to YAML file or directory containing Secret
                               resources to pass as function credentials. Overrides
                               auto-fetched credentials from cluster.
      --function-registry-override=STRING
                               Override the registry for all function images
                               (e.g., 'my-company.registry.io'). Useful when
                               pulling functions from a mirror or private
                               registry.
      --eventual-state         Show eventual state after all reconciliation cycles
                               complete. Useful with function-sequencer which hides
                               later stage resources until earlier stages become Ready.
      --dry-run-on=all         Which resources to verify against the apiserver with a
                               dry run: "all" also dry-run creates added resources so
                               their diffs include server-side defaulting and admission
                               (needs the 'create' verb; degrades per-resource without
                               it), "existing" only resources already in the cluster.
      --crossplane-version=VERSION
                               Pin the crossplane render version; the docker engine
                               pulls xpkg.crossplane.io/crossplane/crossplane:<version>.
                               Minimum v2.3.4 (older renders drop cluster-observed
                               resources; see Prerequisites). Mutually exclusive with
                               --crossplane-image.
      --crossplane-image=IMAGE Override the full crossplane render image reference
                               (e.g. for a private mirror). Mutually exclusive with
                               --crossplane-version.
```

**Note**: XR namespaces are read directly from the YAML files being diffed, not from command-line flags.

**Render version**: When neither `--crossplane-version` nor `--crossplane-image` is set, rendering uses the floating `xpkg.crossplane.io/crossplane/crossplane:stable` tag. Pin `--crossplane-version` for reproducible diffs or to hold a known-good version; `--crossplane-image` targets a mirrored/air-gapped registry. Only `--crossplane-version` is floor-checked against the v2.3.4 minimum — a full image reference carries no comparable version.

**Ignored Paths**: By default, `metadata.annotations[kubectl.kubernetes.io/last-applied-configuration]` is always hidden from the diff, since it is just a serialization of the object itself. Note that it is hidden from the *output* only: for `comp`, a composition differing solely in that annotation still counts as changed, because applying it creates a new CompositionRevision. Additional paths can be specified with `--ignore-paths`. This is useful for filtering out metadata added by tools like ArgoCD (e.g., tracking IDs, sync waves) that shouldn't affect diff results. The `--ignore-paths` flag applies uniformly across all output modes: the human diff, JSON, and YAML output all strip ignored fields, and summary counts are computed after ignore-filtering so a resource whose only changes are in ignored fields is not counted as modified.

#### `comp` - Diff Composition Impact

```
crossplane-diff comp [<files> ...] [flags]

Arguments:
  [<files> ...]    YAML files containing updated Composition(s).

Flags:
  -h, --help                   Show context-sensitive help.
      --verbose                Print verbose logging statements.
      --context=STRING         Kubernetes context to use (defaults to current context).
  -o, --output=diff            Output format: diff (human-readable), json, or yaml.
      --no-color               Disable colorized output.
      --compact                Show compact diffs with minimal context.
      --max-nested-depth=10    Maximum depth for nested XR recursion.
      --max-iterations=20      Maximum render iterations for requirements resolution
                               or eventual-state simulation. Increase for complex
                               pipelines that need more cycles to converge.
      --timeout=1m             How long to run before timing out.
  -n, --namespace=""           Namespace to find Composites (empty = all namespaces).
      --include-manual         Include Composites with Manual update policy (default:
                               only Automatic policy Composites)
      --minimize-composition   Collapse each changed composition to a single
                               change-marker line instead of the full YAML diff.
                               Affects human-readable output only; JSON/YAML keeps
                               full detail. Errors and no-change compositions still
                               print in full.
      --ignore-paths=STRING,... Paths to ignore in diffs. Supports simple paths
                               (e.g., 'metadata.annotations') and map key paths with
                               bracket notation (e.g., 'metadata.annotations[key]').
                               Can be specified multiple times.
      --function-credentials=PATH  Path to YAML file or directory containing Secret
                               resources to pass as function credentials. Overrides
                               auto-fetched credentials from cluster.
      --function-registry-override=STRING
                               Override the registry for all function images
                               (e.g., 'my-company.registry.io'). Useful when
                               pulling functions from a mirror or private
                               registry.
      --eventual-state         Show eventual state after all reconciliation cycles
                               complete. Useful with function-sequencer which hides
                               later stage resources until earlier stages become Ready.
      --resource=STRING,...    Limit impact analysis to specific composites in
                               [namespace/]name format. Repeatable or comma-separated.
                               Bare name means cluster-scoped. Mutually exclusive with
                               --namespace. Composites matched by --resource but excluded
                               (because they would not adopt the diffed composition) are
                               reported in the impact analysis with status "filtered" and a
                               "filterReason": "manual_policy" (use --include-manual to
                               evaluate them instead), "revision_selector_mismatch" (their
                               compositionRevisionSelector does not match the composition's
                               labels), or "deleting" (the composite is being deleted).
                               --include-manual does not re-include the latter two.
      --analyze-on=STRING      Smallest composition change that triggers per-composite impact
                               analysis: "spec-change", "any-change", or "always". Defaults to
                               "any-change", which evaluates the composites whenever anything
                               Crossplane hashes into the composition's identity differs —
                               labels and annotations as well as spec. "spec-change" skips the
                               render-per-composite cost for a metadata-only edit, which still
                               creates a new CompositionRevision that Automatic composites
                               re-point to; choosing it asserts none of your compositions can
                               observe a revision's identity. "always" also evaluates a
                               composition identical to its in-cluster version, which creates no
                               revision and so cannot change anything — useful for a pre-edit
                               convergence baseline. This is a cost knob, not a correctness mode:
                               "revisionImpact" is reported at every setting, and composites left
                               unevaluated are marked with "impactAnalysisSkipped": true in
                               structured output, so "we did not look" stays distinguishable from
                               "we looked and found nothing".
      --analyze-unchanged      Deprecated: equivalent to --analyze-on=always. Still
                               honoured, but passing it together with a conflicting --analyze-on
                               value is an error.
      --dry-run-on=all         Which resources to verify against the apiserver with a
                               dry run: "all" also dry-run creates added resources so
                               their diffs include server-side defaulting and admission
                               (needs the 'create' verb; degrades per-resource without
                               it), "existing" only resources already in the cluster.
      --crossplane-version=VERSION
                               Pin the crossplane render version; the docker engine
                               pulls xpkg.crossplane.io/crossplane/crossplane:<version>.
                               Minimum v2.3.4 (older renders drop cluster-observed
                               resources; see Prerequisites). Mutually exclusive with
                               --crossplane-image.
      --crossplane-image=IMAGE Override the full crossplane render image reference
                               (e.g. for a private mirror). Mutually exclusive with
                               --crossplane-version.
```

**Note**: The `diff` subcommand is deprecated. Use `xr` instead.

**Ignored Paths**: By default, `metadata.annotations[kubectl.kubernetes.io/last-applied-configuration]` is always hidden from the diff, since it is just a serialization of the object itself. Note that it is hidden from the *output* only: for `comp`, a composition differing solely in that annotation still counts as changed, because applying it creates a new CompositionRevision. Additional paths can be specified with `--ignore-paths`. This is useful for filtering out metadata added by tools like ArgoCD (e.g., tracking IDs, sync waves) that shouldn't affect diff results. The `--ignore-paths` flag applies uniformly across all output modes: the human diff, JSON, and YAML output all strip ignored fields, and summary counts are computed after ignore-filtering so a resource whose only changes are in ignored fields is not counted as modified.

### Prerequisites

- A running Kubernetes cluster with Crossplane installed
- `kubectl` configured to access your cluster
- Appropriate RBAC permissions (see [Required Permissions](#required-permissions))
- A `crossplane` render image/binary of **v2.3.4 or newer** (the tool renders via
  `xpkg.crossplane.io/crossplane/crossplane:stable` by default, which already satisfies this). Older
  images silently drop cluster-observed composed resources from the render pipeline, which produces
  incorrect diffs (e.g. missing removals). If a locally cached `:stable` image predates v2.3.4, re-pull it.

## How It Works

The tool performs the following steps:

1. **Load resources** from YAML files or stdin
2. **Find matching compositions** for each Composite Resource
3. **Render resources** using the same composition pipeline as Crossplane
4. **Resolve requirements** iteratively (environment configs, external resources)
5. **Propagate namespaces** from XRs to managed resources (for Crossplane v2)
6. **Validate resources** against their schemas and enforce scope constraints
7. **Calculate diffs** by comparing rendered resources against current cluster state
8. **Display formatted output** showing what would change

## Function Credentials

Some Crossplane functions require credentials to operate (e.g., `function-msgraph` for Microsoft Graph API access). These credentials are typically referenced in composition pipelines via `credentials[].secretRef`.

### Automatic Credential Fetching

By default, `crossplane-diff` automatically fetches credentials referenced in compositions from the cluster:

```yaml
# In your composition
spec:
  pipeline:
    - step: call-graph-api
      functionRef:
        name: function-msgraph
      credentials:
        - name: azure-creds
          source: Secret
          secretRef:
            namespace: crossplane-system
            name: msgraph-credentials
```

When diffing an XR that uses this composition, the tool will automatically fetch `msgraph-credentials` from the cluster and pass it to the function.

### Providing Credentials via CLI

For cases where credentials don't exist in the cluster (e.g., when using workload identity that's injected at runtime, or testing locally), you can provide credentials via the `--function-credentials` flag:

```bash
# Provide credentials from a file
crossplane-diff xr xr.yaml --function-credentials ./secrets/credentials.yaml

# Provide credentials from a directory (all YAML files containing Secrets)
crossplane-diff xr xr.yaml --function-credentials ./secrets/
```

The credentials file should contain Kubernetes Secret resources:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: msgraph-credentials
  namespace: crossplane-system
type: Opaque
data:
  credentials: <base64-encoded-credentials>
```

**Note**: CLI-provided credentials take precedence over auto-fetched credentials from the cluster. This allows you to override cluster secrets for testing or development purposes.

## Crossplane v2 Support

This tool fully supports both Crossplane v1 and v2, including:

- **Cluster-scoped XRDs**: Traditional cluster-wide resources
- **Namespaced XRDs**: Resources confined to specific namespaces for better isolation
- **Automatic scope detection**: Determines whether XRDs are cluster or namespace scoped
- **Namespace propagation**: Ensures managed resources inherit appropriate namespaces
- **Scope validation**: Enforces rules like "namespaced XRs cannot own cluster-scoped managed resources"

**Note**: Namespace information is read directly from the XR definitions in your YAML files, not from command-line
flags.

## Required Permissions

The tool reads from the cluster to gather definitions and current state, and round-trips every resource it is about to diff through the apiserver with `dryRun=All` to compute the post-apply form — picking up server-side defaulting and mutating-admission output, and surfacing any rejection as a diff-time error. Two different requests are used, because they are two different operations:

- **Resources that already exist** are server-side applied (`patch`).
- **Additions** are created (`create`), which is what `--dry-run-on=all` (the default) does. Server-side apply cannot serve this path: it is a PUT-shaped request to a named path, and an addition that relies on `metadata.generateName` has no name yet.

Although nothing is ever persisted, the apiserver authorizes a dry run exactly as it would the real request, so read-only access is **not** sufficient.

### Read-only (`get`, `list`, `watch`)

For the definition and configuration plane, which is only fetched:

- `apiextensions.k8s.io`: `customresourcedefinitions`
- `apiextensions.crossplane.io`: `compositeresourcedefinitions`, `compositions`, `compositionrevisions`, `environmentconfigs`
- `pkg.crossplane.io`: `functions`

### Read + `patch` (required)

On every API group containing resources you want to diff — XRs, Claims, and any managed resource GVKs the compositions render. The `patch` verb is what authorizes the SSA dry-run against a resource that already exists.

This one is not optional and does not degrade. An existing resource's diff depends on the apiserver's merge result — that is how field *removals* under server-side apply are detected — so a diff computed without it would be wrong rather than merely less detailed. Without `patch`, the run fails with a message naming the resource and the missing verb.

### Read + `create` (required for full-fidelity addition diffs)

On the same API groups. The `create` verb authorizes the dry-run create behind `--dry-run-on=all`, which is what makes a `+++` diff show the resource as the *apiserver* would store it rather than as the render pipeline emitted it. Two things are only visible this way:

- **Defaults on built-in Kubernetes types.** For CRD-backed types the tool already applies the CRD's `default:` values locally before diffing, so those have never been missing. A built-in type (a `ConfigMap`, a `Deployment`) has no CRD to read them from, so its defaults — `spec.strategy.type`, `spec.template.spec.restartPolicy`, `imagePullPolicy`, and so on — only appear once the apiserver has seen the object.
- **Mutating admission output, for any type.** Nothing local can predict what a mutating webhook will do.

Unlike `patch`, this one **degrades per-resource rather than failing the run**. A resource that could not be verified falls back to the rendered output and is marked in structured output with a `dryRun` object saying why (see [Structured Output](#structured-output-jsonyaml)); each distinct reason raises one warning per GVK + namespace, not one per resource. The reasons are:

| `dryRun.skipReason` | Cause |
|---------------------|-------|
| `forbidden` | The authorizer says these credentials may not create this kind here. Nothing was learned about the resource; this is a property of the credentials, not a finding about the resource. |
| `webhookUnavailable` | The apiserver could not complete the admission chain — classically an unreachable webhook with `failurePolicy: Fail`. `dryRun.detail` carries the apiserver's own message. |
| `namespaceNotFound` | The resource's target namespace does not exist yet, so the apiserver would not admit it. |
| `disabled` | You passed `--dry-run-on=existing`. No warning is raised: you asked for it. |

`namespaceNotFound` is a degradation rather than a finding on purpose. A Namespace and the resources inside it are routinely applied together, so the namespace being absent when you *diff* says nothing about whether the apply will succeed — `crossplane-diff` cannot know whether that Namespace is part of the same apply, and refusing to diff would break previewing a bootstrap. If you do want to treat it as a failure, gate on that `skipReason` value.

If the cluster *rejects* an addition — a validating webhook, a `ValidatingAdmissionPolicy`, a `ResourceQuota` — that is a finding, not a degradation, and it is reported as one: exit code 2 with a typed `validationFailures[]` entry (see [Exit Codes](#exit-codes)). The difference is whether the verdict describes the object: a quota or policy refusal will still hold when you apply, whereas a missing namespace is a precondition you may be about to satisfy.

A 403 alone cannot tell those two apart: the apiserver returns it both when RBAC denies the verb and when quota or a webhook refuses the object. Rather than pattern-match the apiserver's prose, the tool resolves the ambiguity with a `SelfSubjectAccessReview`, consulted only after a 403 has actually come back. `create` on `selfsubjectaccessreviews` is granted to `system:authenticated` by default through the built-in `system:basic-user` ClusterRole, so this normally needs no rule of its own.

### Opting out: `--dry-run-on`

`--dry-run-on` is available on both `xr` and `comp`:

- `all` (default) — also dry-run creates additions, as described above.
- `existing` — only dry-runs resources already in the cluster, which is the behaviour of releases before this flag existed. Needs no `create` permission and costs one apiserver round-trip fewer per added resource. Additions carry `dryRun.skipReason: "disabled"`.

This is a depth/cost knob, not an accuracy toggle: at either setting every unverified resource says so in structured output, so "we did not check" stays distinguishable from "we checked and this is what the cluster would store".

### Optional: `get` on `secrets`

Only required if you use the [auto-fetch credentials](#automatic-credential-fetching) feature. Skip this if you always supply credentials via `--function-credentials` or your compositions don't use credentialed functions.

### Example `ClusterRole`

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: crossplane-diff-runner
rules:
# Definition plane — read-only
- apiGroups: [apiextensions.k8s.io]
  resources: [customresourcedefinitions]
  verbs: [get, list, watch]
- apiGroups: [apiextensions.crossplane.io]
  resources: [compositeresourcedefinitions, compositions, compositionrevisions, environmentconfigs]
  verbs: [get, list, watch]
- apiGroups: [pkg.crossplane.io]
  resources: [functions]
  verbs: [get, list, watch]
# Diffable resources — read + patch + create. List the provider/XR API groups you
# use; you can combine them in one rule (as below) or split them into separate rules.
# patch authorizes the dry-run apply against existing resources and is required.
# create authorizes the dry-run create behind --dry-run-on=all; drop it and
# additions degrade per-resource (dryRun.skipReason: "forbidden") instead of failing.
- apiGroups:
  - example.org                       # your XR groups
  - s3.aws.upbound.io                 # provider groups
  - s3.aws.m.upbound.io               # namespaced variants (Crossplane v2)
  resources: ['*']
  verbs: [get, list, watch, patch, create]
# Optional: function credential auto-fetch
- apiGroups: ['']
  resources: [secrets]
  verbs: [get]
```

If your compositions render **built-in** Kubernetes types (a `ConfigMap`, a `Deployment`, a `Service`), add a rule
covering those groups too — the core group is `''`, apps is `apps`, and so on. Built-in types are exactly the case where
the dry-run create earns its keep, since their defaults cannot be read from a CRD.

## Kubernetes Configuration

All `crossplane-diff` commands (`xr`, `comp`, `version`) resolve their target
cluster the same way, following the standard CLI convention:

1. `$KUBECONFIG` env var, if set.
2. `~/.kube/config`, if present.
3. Otherwise, fall back to the pod's in-cluster ServiceAccount (with a
   one-line warning on stderr).

The `--context` flag overrides the kubeconfig's `current-context`.

### Running in a pod

A common pattern is to run `crossplane-diff` inside a pod (for example, a
GitHub Actions runner) to post PR comments using the pod's existing RBAC.
Two supported modes:

- **Use the pod's ServiceAccount** — simplest: build an image without
  `~/.kube/config`, don't set `KUBECONFIG`. The tool falls back to
  in-cluster automatically.
- **Target a different cluster from inside the pod** — set up a kubeconfig
  (e.g. via `kubectl config use-context <arn>`) and `crossplane-diff` will
  honor it, including `--context` overrides. This matches the behavior of
  `kubectl` and other Kubernetes CLIs, and is why a `~/.kube/config` that
  exists in the pod always takes precedence over the pod's ServiceAccount.

#### Reaching function containers from inside Docker

`crossplane-diff` runs composition functions as Docker containers via the host's Docker socket. When `crossplane-diff`
itself runs inside a container (a GitHub Actions container job, a dind setup, etc.), the function containers it spawns
land on the default Docker bridge network and are unreachable from the caller's network. Set
`CROSSPLANE_DIFF_DOCKER_NETWORK` to the network the caller is on, and the tool will stamp every function package with
the `render.crossplane.io/runtime-docker-network` annotation so the Crossplane render runtime joins each function
container to that network.

GitHub Actions example:

```yaml
jobs:
  diff:
    runs-on: ubuntu-latest
    container:
      image: my-org/crossplane-diff:latest
    env:
      CROSSPLANE_DIFF_DOCKER_NETWORK: ${{ job.container.network }}
    steps:
      - run: crossplane-diff xr xr.yaml
```

The env var is read on every `GetFunctionsForComposition` call, so it works correctly even with the cached function
provider (cached function packages are re-annotated on cache hits). If the env var is unset, the tool leaves the
annotation alone — appropriate for the common case of running `crossplane-diff` directly on a host with Docker
installed. Any value the user has already set on a function package is preserved.

## Output Format

### Human-Readable Diff (default)

The default output follows familiar diff conventions with colorized output (unless disabled):

```diff
+++ Resource/new-resource-(generated)
+ apiVersion: nop.crossplane.io/v1alpha1
+ kind: NopResource
+ metadata:
+   name: new-resource
+ spec:
+   forProvider:
+     field: value

---
--- XNopResource/removed-resource
- apiVersion: diff.example.org/v1alpha1
- kind: XNopResource
- spec:
-   coolField: goodbye!

~~~
~~~ Resource/modified-resource
  metadata:
    name: modified-resource
- spec:
-   oldValue: something
+ spec:
+   newValue: something-else
---

Summary: 1 added, 1 modified, 1 removed
```

### Structured Output (JSON/YAML)

For CI/CD pipelines or programmatic processing, use `--output json` or `--output yaml`:

**XR Diff JSON output** (`crossplane-diff xr xr.yaml -o json`):

```json
{
  "summary": {
    "added": 1,
    "modified": 1,
    "removed": 0
  },
  "changes": [
    {
      "type": "added",
      "apiVersion": "nop.crossplane.io/v1alpha1",
      "kind": "NopResource",
      "name": "new-resource",
      "diff": { "spec": { "apiVersion": "...", "kind": "...", "metadata": { ... }, "spec": { ... } } }
    },
    {
      "type": "modified",
      "apiVersion": "nop.crossplane.io/v1alpha1",
      "kind": "NopResource",
      "name": "modified-resource",
      "diff": { "old": { ... }, "new": { ... } }
    }
  ],
  "xrs": [
    {
      "xr": { "apiVersion": "example.org/v1", "kind": "XNopResource", "name": "my-xr", "namespace": "default" },
      "status": "changed",
      "summary": { "added": 1, "modified": 1, "removed": 0 },
      "changes": [
        {
          "type": "added",
          "apiVersion": "nop.crossplane.io/v1alpha1",
          "kind": "NopResource",
          "name": "new-resource",
          "diff": { "spec": { ... } }
        },
        {
          "type": "modified",
          "apiVersion": "nop.crossplane.io/v1alpha1",
          "kind": "NopResource",
          "name": "modified-resource",
          "diff": { "old": { ... }, "new": { ... } }
        }
      ]
    }
  ]
}
```

**Per-input-XR grouping (`xrs[]`).** When you diff more than one XR/claim in a
single invocation, the flat `changes[]` list gives no indication of which input
XR produced each change. The `xrs[]` array solves this: it carries one entry per
input XR/claim (in input order), each with the input XR's identity, a `status`
(`"changed"`, `"unchanged"`, or `"error"`), its own `summary`, its own
`changes[]`, and — for a failed XR — its own `errors[]`. This is the
authoritative grouping the tool already computes while rendering each XR's
composed tree, so wrappers no longer need to invoke `xr` once per resource to
get per-XR output. Unchanged XRs appear with `status: "unchanged"` and an empty
`changes[]`; errored XRs appear with `status: "error"` and their error in the
entry's `errors[]` (and also in the top-level `errors[]`).

> **Deprecation:** the top-level flat `changes[]` is **deprecated** in favor of
> `xrs[]` and will be removed in a future major release. The aggregate
> `summary` and top-level `errors[]` remain. New consumers should read `xrs[]`;
> existing consumers of `changes[]` keep working during the deprecation window.

The human-readable (`diff`) output likewise groups by input XR when more than
one is supplied: each XR gets a `=== Kind/name ===` section with its own diffs
and summary, followed by an aggregate `Total: … across N XRs (…)` footer. A
single-XR invocation renders flat, exactly as before.

**Composition Diff JSON output** (`crossplane-diff comp composition.yaml -o json`):

```json
{
  "compositions": [
    {
      "name": "xbuckets.example.org",
      "compositionChanges": {
        "type": "modified",
        "apiVersion": "apiextensions.crossplane.io/v1",
        "kind": "Composition",
        "name": "xbuckets.example.org",
        "diff": { "old": { ... }, "new": { ... } }
      },
      "revisionImpact": {
        "changeScope": "spec",
        "createsRevision": true,
        "repointedComposites": 4
      },
      "affectedResources": {
        "total": 6,
        "withChanges": 2,
        "unchanged": 1,
        "withErrors": 1,
        "filteredBySelector": 1,
        "filteredByDeletion": 1
      },
      "impactAnalysis": [
        {
          "apiVersion": "example.org/v1",
          "kind": "XBucket",
          "name": "bucket-1",
          "status": "changed",
          "downstreamChanges": {
            "summary": { "added": 1, "modified": 1, "removed": 0 },
            "changes": [ ... ]
          }
        },
        {
          "apiVersion": "example.org/v1",
          "kind": "XBucket",
          "name": "bucket-2",
          "status": "unchanged"
        },
        {
          "apiVersion": "example.org/v1",
          "kind": "XBucket",
          "name": "bucket-3",
          "status": "error",
          "error": "render failed: ..."
        },
        {
          "apiVersion": "example.org/v1",
          "kind": "XBucket",
          "name": "bucket-4",
          "status": "filtered",
          "filterReason": "revision_selector_mismatch",
          "filterDetail": "compositionRevisionSelector {version: 0.0.1} does not match composition labels {version: 0.0.2}"
        },
        {
          "apiVersion": "example.org/v1",
          "kind": "XBucket",
          "name": "bucket-5",
          "status": "filtered",
          "filterReason": "deleting",
          "filterDetail": "deletionTimestamp: 2026-09-07T11:25:03Z"
        }
      ]
    }
  ]
}
```

The structured output includes:
- **Change types**: each entry's `type` field carries the word form — one of `"added"`, `"modified"`, or `"removed"`. (Unchanged resources are filtered out of structured output and never appear in `changes[]`. The `+` / `~` / `-` symbols appear only in the human-readable diff format described above.)
- **Full resource details**: apiVersion, kind, name, namespace
- **Diff content**: for modifications, `diff.old` and `diff.new` carry the full current/desired resource objects (apiVersion/kind/metadata/spec/status, etc.) — not just the diffing subset. For additions/removals, the full resource object lives under `diff.spec` (the JSON key is literally `spec` but the value is the entire resource, not its spec subtree).
- **Dry-run fidelity**: a `dryRun` object on a change entry says that this resource's desired state did **not** go
  through the apiserver, and why. It is **absent whenever the resource was verified** — that is the success case and the
  common one, so a consumer checks for the key's presence, not for a value inside it. Removal diffs never carry it
  either: they are computed straight from cluster state and have no desired state to preview, so read absence as
  "nothing was skipped" rather than as a positive fidelity guarantee. The fields are `performed` (always `false` when the
  object is present — the redundancy keeps the emitted JSON self-describing), `skipReason` (one of `"disabled"`,
  `"forbidden"`, `"webhookUnavailable"`, `"namespaceNotFound"` — see [Required Permissions](#required-permissions) for what
  each means), and
  `detail`, the cluster's own explanation (the `SelfSubjectAccessReview`'s reason, or the apiserver's error message).
  `dryRun` lives on the shared per-resource change shape, so it appears in `xr`'s `changes[]` and `xrs[].changes[]` and
  in `comp`'s `impactAnalysis[].downstreamChanges.changes[]` alike:

  ```json
  {
    "type": "added",
    "apiVersion": "v1",
    "kind": "ConfigMap",
    "name": "new-config",
    "namespace": "default",
    "diff": { "spec": { ... } },
    "dryRun": {
      "performed": false,
      "skipReason": "forbidden",
      "detail": "not authorized to create configmaps in namespace \"default\""
    }
  }
  ```
- **Impact analysis** (comp only): which XRs are affected by composition changes and their status. When the composition
  change is smaller than `--analyze-on` asked to analyse — by default, a composition identical to its in-cluster
  version — its composites are not evaluated and the entry carries `"impactAnalysisSkipped": true` alongside an empty
  `impactAnalysis`, so a consumer can tell "not evaluated" from "no affected composites found". Raise `--analyze-on` to
  evaluate them anyway.
- **Revision impact** (comp only): a `revisionImpact` object per composition, recording what applying it does to
  CompositionRevisions regardless of whether anything renders differently. It carries `changeScope` (`"none"`,
  `"metadata"` or `"spec"` — how much of the composition differs, in the terms Crossplane's `Composition.Hash()` uses,
  which covers labels and annotations as well as spec), `createsRevision` (whether applying the composition produces a
  new CompositionRevision), and `repointedComposites` (how many composites would adopt that revision and reconcile as a
  result — those not excluded by update policy, revision selector, or deletion). It is **always present**, including
  when `impactAnalysisSkipped` is true: that is the point of it, and it is what keeps `--analyze-on` a cost knob rather
  than a correctness mode. Note that a composite re-pointing to a new revision is not the same as its rendered output
  changing; re-pointing alone usually renders identically.
- **Masked changes** (comp only): `"maskedChangesOnly": true` says an absent `compositionChanges` does *not* mean the
  composition is unchanged — it differs only in fields excluded from the diff (your `--ignore-paths`, or the fields
  suppressed for readability). Applying it still creates a new CompositionRevision, which `revisionImpact` reports.
- **Warnings**: A top-level `warnings` array of non-fatal advisories, each with a `message` and an optional `context` map of
  the key/value pairs from the emitting call site. Distinct from `errors` and with no effect on the exit code; see
  [Warnings](#warnings) above.
- **Errors**: A top-level `errors` array of `OutputError` objects (see [Validation Errors](#validation-errors) below for the schema and an example), plus per-XR `error` fields in `impactAnalysis` for composition diffs

### Validation Errors

When validation fails on the input XR or any rendered composed resource, `crossplane-diff` reports the failure in both human-readable and machine-readable form. Two things land here: local schema validation against the CRD/XRD, and the cluster's own refusal of a resource during the dry run (a validating webhook, a `ValidatingAdmissionPolicy`, a `ResourceQuota`). Both answer the same question — "will the cluster accept this?" — so they share the exit-code tier and the structured output field, and are told apart by `type` (see the `FieldValidationError` table below).

Exit-code precedence (per `DetermineExitCode`): any error in the run beats diff detection, so a partially-failed run never returns exit code 3 even if some XRs produced diffs. Among errors, tool errors (exit code 1) beat validation errors (exit code 2). Exit code 2 therefore requires *every* error in the run to be a validation error. See the [Exit Codes](#exit-codes) table below.

**Human-readable output** (`crossplane-diff xr invalid-xr.yaml`):

Per Unix convention, errors go to stderr and diff content goes to stdout. When validation is the only failure, stdout is empty and the structured failure detail appears on stderr, prefixed by an `ERROR: <resourceID>:` marker:

```
# stderr
ERROR: XNopResource/invalid-schema-xr: cannot validate resources: ns.diff.example.org/v1alpha1/XNopResource default/invalid-schema-xr:
  spec.coolField: Invalid value: "number": ... [schema]
ns.nop.example.org/v1alpha1/XDownstreamResource default/invalid-schema-xr:
  spec.forProvider.configData: Invalid value: "boolean": ... [schema]

# stdout
(empty)
```

The `cannot validate resources:` prefix is added by `DefaultDiffProcessor`'s `errors.Wrap` around the inner `SchemaValidationError` — every *local schema* validation failure carries that anchor. A cluster rejection reaches the same exit-code tier by a different route (it is raised from diff calculation, not from the validator), so it carries a different anchor: `the cluster rejected <Kind>/<name>:`, followed by the apiserver's own message. Match on either, or on the structured `errors[].validationFailures[]`, rather than on `cannot validate resources:` alone.

Each per-resource block starts with a header that includes the resource identity, followed by indented error lines:

- Cluster-scoped resource: `<apiVersion>/<Kind> <name>:`
- Namespaced resource: `<apiVersion>/<Kind> <namespace>/<name>:`
- Resource without `metadata.name` (e.g. a resource discovered missing a schema before it was named): collapses to just `<apiVersion>/<Kind>:`

Each indented error line has the shape `<message> [<type>]`, where `<type>` is one of `[schema]`, `[cel]`, `[unknownField]`, or `[defaulting]`. A bad value is appended as `(got <value>)` when it isn't already substring-present in the message. When some inputs in a batched run succeed and others fail validation, the successful diffs appear on stdout and the failing inputs' `ERROR:` blocks appear on stderr.

This per-resource block form belongs to the local validator. A cluster rejection has nothing to break into field-level lines — the apiserver hands back one message — so it prints as a single `ERROR:` line and carries no `[<type>]` suffix. Its `"admission"` type is visible in structured output only.

**Machine-readable output** (`crossplane-diff xr invalid-xr.yaml --output json`):

```json
{
  "summary": { "added": 0, "modified": 0, "removed": 0 },
  "changes": [],
  "errors": [
    {
      "resourceID": "XNopResource/invalid-schema-xr",
      "message": "cannot validate resources: ns.diff.example.org/v1alpha1/XNopResource default/invalid-schema-xr:\n  spec.coolField: Invalid value: \"number\": ... [schema]\nns.nop.example.org/v1alpha1/XDownstreamResource default/invalid-schema-xr:\n  spec.forProvider.configData: Invalid value: \"boolean\": ... [schema]",
      "validationFailures": [
        {
          "apiVersion": "ns.diff.example.org/v1alpha1",
          "kind": "XNopResource",
          "name": "invalid-schema-xr",
          "namespace": "default",
          "status": "invalid",
          "errors": [
            {
              "type": "schema",
              "field": "spec.coolField",
              "message": "spec.coolField: Invalid value: \"number\": ...",
              "value": "number"
            }
          ]
        },
        {
          "apiVersion": "ns.nop.example.org/v1alpha1",
          "kind": "XDownstreamResource",
          "name": "invalid-schema-xr",
          "namespace": "default",
          "status": "invalid",
          "errors": [
            {
              "type": "schema",
              "field": "spec.forProvider.configData",
              "message": "spec.forProvider.configData: Invalid value: \"boolean\": ...",
              "value": "boolean"
            }
          ]
        }
      ]
    }
  ]
}
```

The `OutputError` schema:

| Field | Type | Description |
|-------|------|-------------|
| `resourceID` | string | Identifies which user-supplied input the diff was processing (one entry per batched run). Format: `<Kind>/<name>`. |
| `message` | string | Human-readable error string — the same text written to stderr. |
| `validationFailures` | `[]ResourceValidationFailure`, optional | Structured per-resource breakdown. Set for local schema-validation failures and for cluster rejections during the dry run; `nil` for tool, IO, render, and scope-check errors. |

`ResourceValidationFailure` carries `apiVersion`, `kind`, `name`, `namespace`, `status` (one of `"invalid"` or `"missingSchema"` — `"valid"` rows are filtered out), and `errors`, a list of `FieldValidationError` records:

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | `"schema"`, `"cel"`, `"unknownField"`, `"defaulting"`, or `"admission"`. The first four come from local validation against the CRD/XRD. `"admission"` means the *apiserver* refused the resource during the dry run — a validating webhook, a `ValidatingAdmissionPolicy`, a `ResourceQuota`. A *missing namespace* is deliberately NOT here: it degrades instead, because it is a precondition you may be about to satisfy rather than a verdict on the object (see `dryRun.skipReason: namespaceNotFound`). It sits in the same field on purpose: to a consumer asking "why won't the cluster accept this?", it is the same kind of answer. |
| `field` | string, optional | JSONPath of the offending field, when locatable. Absent for `"admission"`: the apiserver reports one rejection, not a field list. |
| `message` | string | Validator-emitted human-readable description; for k8s-derived schema errors this typically already embeds the field path and bad value. For `"admission"` it is the apiserver's rejection message verbatim. |
| `value` | any, optional | The offending value as the validator saw it. Type-preserved (string, number, bool, struct). |

An `"admission"` failure is reported the same way whether the resource already exists or is being added — the cluster's verdict does not depend on that, so neither does the exit code. See the note under [Exit Codes](#exit-codes) for the behaviour change this represents.

`resourceID` and `validationFailures` are intentionally complementary: `resourceID` anchors the failure to one user-supplied input, while `validationFailures` enumerates every resource (the input itself plus any composed resource) that failed validation under that input. They overlap on `kind`+`name` when the input itself is among the failing resources — that's deliberate, so consumers iterating `validationFailures` never miss an XR-level rejection.

For `comp` output the same `OutputError` structure (including `validationFailures`) appears only in the top-level `errors[]` of the composition-diff JSON. Per-composition and per-XR failures use a simpler `error` string field (under `compositions[]` and `impactAnalysis[]` respectively) — they don't carry the structured validation breakdown.

## Exit Codes

The tool returns different exit codes to indicate the result of the diff operation, making it easy to use in CI/CD pipelines and scripts:

| Exit Code | Meaning |
|-----------|---------|
| 0 | Success - no differences detected |
| 1 | Tool error - execution failed (e.g., cluster access issues, invalid input) |
| 2 | Validation error - the cluster will not accept a resource: it failed local validation against its CRD/XRD schema, **or** the apiserver rejected it during the dry run (validating webhook, `ValidatingAdmissionPolicy`, `ResourceQuota`) |
| 3 | Diff detected - differences were found between input and cluster state |

Exit codes are ordered by severity. When processing multiple resources, the highest severity exit code is returned:

```bash
# Example: Use exit codes in CI/CD
crossplane-diff xr my-xr.yaml
case $? in
  0) echo "No changes needed" ;;
  1) echo "Error running diff" ; exit 1 ;;
  2) echo "Cluster would reject a resource" ; exit 1 ;;
  3) echo "Changes detected - review required" ;;
esac
```

> **Behaviour change: cluster rejections now exit 2, not 1.** Exit code 2 used to mean only *local* schema validation.
> A resource refused by a validating webhook during the dry-run apply surfaced as a plain tool error — **exit 1**. It is
> now exit 2, with an `errors[].validationFailures[]` entry of `type: "admission"`.
>
> This deliberately covers **modifications of existing resources**, not just the newly dry-run additions. Reclassifying
> only additions would have made the exit code for one cluster fact depend on whether the resource happened to exist
> already, rather than on what the cluster said about it. A pipeline that treats exit 1 as "the tool broke, retry or
> escalate" and exit 2 as "the input is bad, tell the author" now gets the right bucket for a webhook rejection; one that
> was matching on exit 1 to detect rejections needs updating.

**Revision churn does not set exit code 3.** For `comp`, exit code 3 means something renders differently: the composition's own diff is non-empty, or at least one composite's downstream resources change. A composition that only creates a new CompositionRevision without either — one differing solely in fields excluded from the diff, reported as `"maskedChangesOnly": true` — exits 0, so a GitOps loop re-applying the same manifests doesn't fail its diff gate on every run. A pipeline that *does* want to gate on revision churn reads `revisionImpact.createsRevision` from the structured output.

## Guiding Principles

The tool prioritizes **accuracy above all else**:

- Never silently continues in the face of failures
- Avoids making best-guesses that could compromise accuracy
- Fails completely rather than emit potentially incorrect partial results
- Reaches extensively into the cluster for all information needed to produce accurate diffs
- Caches resources only to avoid API throttling

### Caveats

The tool does not take a snapshot of cluster state before processing, so changes made to the cluster during execution
may affect results.

## Documentation

- **[Design Document](design/design-doc-cli-diff.md)**: Comprehensive technical design and architecture
- **[CLAUDE.md](CLAUDE.md)**: Instructions for Claude.  Contains development principles and guidelines for LLMs.

## Development

### Local Development Setup

**Prerequisites:**
- Go 1.24+
- Docker
- Earthly ([install instructions](https://earthly.dev/get-earthly))
- kubectl

**Setup Steps:**

1. **Install required tools:**
   ```bash
   go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20
   setup-envtest use 1.30.3 # or whatever cluster version we're using now
   ```

2. **Generate test manifests:**
   ```bash
   earthly +go-generate --CROSSPLANE_IMAGE_TAG=main
   ```

3. **Build the project:**
   ```bash
   earthly +build
   ```

### Running Tests

**Unit and Integration Tests (fast):**
```bash
cd cmd/diff
go test ./...
```

**Development Checks (linting, tests, generation):**
```bash
# Run before opening any PR
earthly +reviewable
```

**End-to-End Tests:**
```bash
# Full test matrix against multiple Crossplane versions (slower)
earthly -P +e2e-matrix

# Single E2E test against specific version
earthly +e2e --CROSSPLANE_IMAGE_TAG=main
```

### Build and Development Commands

```bash
# Build binary
earthly +build

# Run linting
earthly +lint

# Generate code/manifests
earthly +generate

# All available targets
earthly --help
```

### Architecture

The tool follows a clean layered architecture:

- **Command Layer**: CLI argument parsing and coordination
- **Application Layer**: Process orchestration and resource loading
- **Domain Layer**: Core diff logic, rendering, and validation
- **Client Layer**: Kubernetes and Crossplane API interactions

### Testing

The project includes comprehensive testing:

- **Unit tests**: Fast, isolated component testing
- **Integration tests**: Using `envtest` for realistic cluster interactions
- **E2E tests**: Full end-to-end scenarios across v1, v2-cluster, and v2-namespaced configurations

## Contributing

Contributions are welcome! Please see our [contributing guidelines](CONTRIBUTING.md) for detailed setup instructions and development workflow.

**Quick Start for Contributors:**

1. Fork and clone the repository
2. Run setup: `earthly +go-generate --CROSSPLANE_IMAGE_TAG=main`
3. Make your changes
4. Test your changes: `earthly +reviewable && earthly -P +e2e-matrix`
5. Open a pull request

See [CONTRIBUTING.md](CONTRIBUTING.md) for complete guidelines and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for community standards.

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

