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
```

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
# Note: --resource cannot be combined with --namespace. Composites with Manual update policy
# are surfaced with status "filtered_by_policy" unless --include-manual is also passed.

# Include XRs with Manual update policy (pinned revisions)
crossplane-diff comp updated-composition.yaml --include-manual

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
```

**Note**: XR namespaces are read directly from the YAML files being diffed, not from command-line flags.

**Ignored Paths**: By default, `metadata.annotations[kubectl.kubernetes.io/last-applied-configuration]` is always ignored. Additional paths can be specified with `--ignore-paths`. This is useful for filtering out metadata added by tools like ArgoCD (e.g., tracking IDs, sync waves) that shouldn't affect diff results. Ignore-paths apply uniformly across all output modes: the human diff, JSON, and YAML output all strip ignored fields, and summary counts are computed after ignore-filtering so a resource whose only changes are in ignored fields is not counted as modified.

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
                               by the update-policy filter are reported in the impact
                               analysis with status "filtered_by_policy" (use
                               --include-manual to evaluate them instead).
```

**Note**: The `diff` subcommand is deprecated. Use `xr` instead.

**Ignored Paths**: By default, `metadata.annotations[kubectl.kubernetes.io/last-applied-configuration]` is always ignored. Additional paths can be specified with `--ignore-paths`. This is useful for filtering out metadata added by tools like ArgoCD (e.g., tracking IDs, sync waves) that shouldn't affect diff results. Ignore-paths apply uniformly across all output modes: the human diff, JSON, and YAML output all strip ignored fields, and summary counts are computed after ignore-filtering so a resource whose only changes are in ignored fields is not counted as modified.

### Prerequisites

- A running Kubernetes cluster with Crossplane installed
- `kubectl` configured to access your cluster
- Appropriate RBAC permissions (see [Required Permissions](#required-permissions))

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

The tool reads from the cluster to gather definitions and current state, and performs a server-side apply with `dryRun=All` against existing resources to compute the post-apply form — picking up CRD defaulting and mutating-webhook output, and surfacing any validating-webhook rejection as a diff-time error. Although nothing is ever persisted, the apiserver still authorizes SSA dry-run with the `patch` verb, so read-only access is **not** sufficient.

### Read-only (`get`, `list`, `watch`)

For the definition and configuration plane, which is only fetched:

- `apiextensions.k8s.io`: `customresourcedefinitions`
- `apiextensions.crossplane.io`: `compositeresourcedefinitions`, `compositions`, `compositionrevisions`, `environmentconfigs`
- `pkg.crossplane.io`: `functions`

### Read + `patch`

On every API group containing resources you want to diff — XRs, Claims, and any managed resource GVKs the compositions render. The `patch` verb is what authorizes the SSA dry-run.

### Optional: `get` on `secrets`

Only required if you use the [auto-fetch credentials](#automatic-credential-fetching) feature. Skip this if you always supply credentials via `--function-credentials` or your compositions don't use credentialed functions.

### `create` is **not** required

The dry-run SSA only runs against resources that already exist in the cluster; for additions, the tool emits the rendered output directly without round-tripping through the apiserver. This keeps the RBAC surface smaller at the cost of slightly less faithful addition diffs (no apiserver defaulting or webhook mutation). See [#334](https://github.com/crossplane-contrib/crossplane-diff/issues/334) for the tracking issue on optionally enabling this.

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
# Diffable resources — read + patch. List the provider/XR API groups you use;
# you can combine them in one rule (as below) or split them into separate rules.
- apiGroups:
  - example.org                       # your XR groups
  - s3.aws.upbound.io                 # provider groups
  - s3.aws.m.upbound.io               # namespaced variants (Crossplane v2)
  resources: ['*']
  verbs: [get, list, watch, patch]
# Optional: function credential auto-fetch
- apiGroups: ['']
  resources: [secrets]
  verbs: [get]
```

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
  ]
}
```

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
      "affectedResources": {
        "total": 5,
        "withChanges": 2,
        "unchanged": 2,
        "withErrors": 1
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
- **Impact analysis** (comp only): which XRs are affected by composition changes and their status
- **Errors**: A top-level `errors` array of `OutputError` objects (see [Validation Errors](#validation-errors) below for the schema and an example), plus per-XR `error` fields in `impactAnalysis` for composition diffs

### Validation Errors

When schema validation fails on the input XR or any rendered composed resource, `crossplane-diff` reports the failure in both human-readable and machine-readable form. Exit-code precedence (per `DetermineExitCode`): any error in the run beats diff detection, so a partially-failed run never returns exit code 3 even if some XRs produced diffs. Among errors, tool errors (exit code 1) beat schema-validation errors (exit code 2). Exit code 2 therefore requires *every* error in the run to be a schema-validation error. See the [Exit Codes](#exit-codes) table below.

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

The `cannot validate resources:` prefix is added by `DefaultDiffProcessor`'s `errors.Wrap` around the inner `SchemaValidationError` — every schema-validation failure carries that anchor.

Each per-resource block starts with a header that includes the resource identity, followed by indented error lines:

- Cluster-scoped resource: `<apiVersion>/<Kind> <name>:`
- Namespaced resource: `<apiVersion>/<Kind> <namespace>/<name>:`
- Resource without `metadata.name` (e.g. a resource discovered missing a schema before it was named): collapses to just `<apiVersion>/<Kind>:`

Each indented error line has the shape `<message> [<type>]`, where `<type>` is one of `[schema]`, `[cel]`, `[unknownField]`, or `[defaulting]`. A bad value is appended as `(got <value>)` when it isn't already substring-present in the message. When some inputs in a batched run succeed and others fail validation, the successful diffs appear on stdout and the failing inputs' `ERROR:` blocks appear on stderr.

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
| `validationFailures` | `[]ResourceValidationFailure`, optional | Structured per-resource breakdown. Set only for schema-validation failures; `nil` for tool, IO, render, and scope-check errors. |

`ResourceValidationFailure` carries `apiVersion`, `kind`, `name`, `namespace`, `status` (one of `"invalid"` or `"missingSchema"` — `"valid"` rows are filtered out), and `errors`, a list of `FieldValidationError` records:

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | `"schema"`, `"cel"`, `"unknownField"`, or `"defaulting"`. |
| `field` | string, optional | JSONPath of the offending field, when locatable. |
| `message` | string | Validator-emitted human-readable description; for k8s-derived schema errors this typically already embeds the field path and bad value. |
| `value` | any, optional | The offending value as the validator saw it. Type-preserved (string, number, bool, struct). |

`resourceID` and `validationFailures` are intentionally complementary: `resourceID` anchors the failure to one user-supplied input, while `validationFailures` enumerates every resource (the input itself plus any composed resource) that failed validation under that input. They overlap on `kind`+`name` when the input itself is among the failing resources — that's deliberate, so consumers iterating `validationFailures` never miss an XR-level rejection.

For `comp` output the same `OutputError` structure (including `validationFailures`) appears only in the top-level `errors[]` of the composition-diff JSON. Per-composition and per-XR failures use a simpler `error` string field (under `compositions[]` and `impactAnalysis[]` respectively) — they don't carry the structured validation breakdown.

## Exit Codes

The tool returns different exit codes to indicate the result of the diff operation, making it easy to use in CI/CD pipelines and scripts:

| Exit Code | Meaning |
|-----------|---------|
| 0 | Success - no differences detected |
| 1 | Tool error - execution failed (e.g., cluster access issues, invalid input) |
| 2 | Schema validation error - resources failed validation against their CRD/XRD schemas |
| 3 | Diff detected - differences were found between input and cluster state |

Exit codes are ordered by severity. When processing multiple resources, the highest severity exit code is returned:

```bash
# Example: Use exit codes in CI/CD
crossplane-diff xr my-xr.yaml
case $? in
  0) echo "No changes needed" ;;
  1) echo "Error running diff" ; exit 1 ;;
  2) echo "Schema validation failed" ; exit 1 ;;
  3) echo "Changes detected - review required" ;;
esac
```

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

