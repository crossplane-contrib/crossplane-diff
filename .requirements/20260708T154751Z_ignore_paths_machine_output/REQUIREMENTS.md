# Ignore-paths in machine-readable output modes

## As Is

`crossplane-diff` accepts `--ignore-paths` to skip fields when computing diffs.
The flag is honored uniformly for the classification step (whether a resource is
`equal`, `modified`, `added`, or `removed`) but is honored *inconsistently* for
the rendered content depending on output format.

### Diff-mode output (`--output diff`)

`GenerateDiffWithOptions` in `cmd/diff/renderer/diff_formatter.go`:

1. Runs `cleanupForDiff(...)` on deep-copies of both `current` and `desired`,
   passing `options.IgnorePaths`. `cleanupForDiff` strips:
   - user-specified paths from `IgnorePaths`
   - server-side / non-diff-relevant metadata:
     `resourceVersion`, `uid`, `generation`, `creationTimestamp`,
     `managedFields`, `selfLink`, `ownerReferences`
   - `spec.resourceRefs`, `spec.crossplane.resourceRefs`
   - the entire `status` block
2. If the cleaned objects are `equality.Semantic.DeepEqual`, returns a
   `DiffType == Equal` `ResourceDiff`.
3. Otherwise, marshals the *cleaned* objects to YAML and computes a
   line-by-line diff, storing the line diffs on `ResourceDiff.LineDiffs`.
4. Stores `Current` / `Desired` pointers on the returned `ResourceDiff` as
   references to the **raw** unstructured objects (uncleaned).

The human diff renderer (`FullDiffFormatter` / `CompactDiffFormatter`) reads
`ResourceDiff.LineDiffs`, so the human output only ever surfaces
non-ignored, non-server-side fields.

### Structured-mode output (`--output json` / `--output yaml`)

`StructuredDiffRenderer.buildDiffDetail` and
`resourceDiffToChangeDetail` (in `cmd/diff/renderer/structured_renderer.go`)
build the `diff` payload from `ResourceDiff.Current.Object` and
`ResourceDiff.Desired.Object` **directly** — the raw unstructured objects.

Consequence:

- `--ignore-paths` values still appear in `changes[].diff.old`,
  `changes[].diff.new`, and `changes[].diff.spec` for added/removed resources.
- Fields that `cleanupForDiff` strips unconditionally (`managedFields`,
  `resourceVersion`, `uid`, `generation`, `creationTimestamp`, `selfLink`,
  `ownerReferences`, `spec.resourceRefs`, `spec.crossplane.resourceRefs`,
  `status`) also leak into JSON/YAML output.

The same bug applies to composition-diff structured output
(`comp_diff_renderer.go` → `resourceDiffToChangeDetail` →
`buildDownstreamChanges`).

### Summary counts

Because `GenerateDiffWithOptions` runs `cleanupForDiff` **before** classifying
the diff type, resources whose only differences are in ignored/server-side
fields are correctly classified as `Equal`, and structured renderers skip
`Equal` diffs when building `summary.added / modified / removed`. A smoke test
confirms this for `ownerReferences`-only diffs: `DiffType == Equal`, count 0.

The reporter believed summary counts were also wrong. Investigation suggests
this claim is not reproducible at the renderer level; requirements below add
explicit test coverage to lock in the correct behavior.

## To Be

Structured output (`--output json` and `--output yaml`), for both `xr` and
`comp` commands, must:

1. Respect `--ignore-paths` and the built-in cleanup rules when populating
   `diff.old`, `diff.new`, and `diff.spec`. The emitted object bodies must
   match — semantically — what the human diff produces after cleanup.
2. Preserve the current correct behavior on `summary.added / modified /
   removed`: a resource whose only differences are in ignored or server-side
   fields must be reported as unchanged (i.e. omitted from `changes`, with
   summary counts unaffected).

The convention we're matching: ArgoCD `ignoreDifferences` and Terraform
`ignore_changes`, where ignore filtering is a semantic operation applied
before any output format is produced. See the survey attached to this task
in conversation.

## Requirements

**R1.** When `--output json` is set, `changes[].diff.old` and
`changes[].diff.new` for a modified resource MUST NOT contain any field
listed in `--ignore-paths`.

- **AC1.1.** Given a `Modified` diff with `--ignore-paths
  'metadata.annotations[argocd.argoproj.io/tracking-id]'` and a
  non-ignored change in `spec.forProvider.configData`, the JSON output's
  `changes[0].diff.old` and `changes[0].diff.new` must not contain the
  `argocd.argoproj.io/tracking-id` annotation key, and must contain both
  values of `spec.forProvider.configData`.

**R2.** When `--output json` is set, `changes[].diff.old` and
`changes[].diff.new` for a modified resource MUST NOT contain the
server-side / non-diff-relevant fields already stripped by
`cleanupForDiff`: `metadata.resourceVersion`, `metadata.uid`,
`metadata.generation`, `metadata.creationTimestamp`,
`metadata.managedFields`, `metadata.selfLink`, `metadata.ownerReferences`,
`spec.resourceRefs`, `spec.crossplane.resourceRefs`, and top-level `status`.

- **AC2.1.** Given a modified diff whose `current` has `managedFields`,
  `resourceVersion`, `uid`, and `status`, the JSON `diff.old` must have
  none of those fields.
- **AC2.2.** Given a modified diff whose `current` has
  `metadata.ownerReferences`, the JSON `diff.old` must not contain
  `ownerReferences` even without user-supplied `--ignore-paths`.

**R3.** When `--output json` is set for an `Added` resource,
`changes[].diff.spec` MUST NOT contain ignored paths or unconditional-cleanup
fields.

- **AC3.1.** Given an Added diff for a resource whose desired body contains
  `metadata.annotations[argocd.argoproj.io/tracking-id]` and
  `--ignore-paths` covers that annotation, the JSON `diff.spec` must not
  contain the annotation.

**R4.** When `--output json` is set for a `Removed` resource,
`changes[].diff.spec` MUST NOT contain ignored paths or unconditional-cleanup
fields.

- **AC4.1.** Symmetric to AC3.1 for a Removed diff (using `Current`).

**R5.** `summary.added`, `summary.modified`, `summary.removed` in JSON
output MUST NOT increment for resources whose only differences are in
ignored or unconditional-cleanup fields.

- **AC5.1.** Given a `Modified`-classified pair where only
  `metadata.ownerReferences` differs, JSON output must show `summary =
  {added: 0, modified: 0, removed: 0}` and empty `changes`.
- **AC5.2.** Given a `Modified`-classified pair where only a
  user-supplied `--ignore-paths` field differs, JSON output must show
  `summary = {added: 0, modified: 0, removed: 0}` and empty `changes`.
- **AC5.3.** Given `--ignore-paths X` and a resource with a change to
  `X` AND a change to a non-ignored field, JSON `summary.modified == 1`
  and `changes[0]` reflects only the non-ignored change.

**R6.** All of R1–R5 also apply to `--output yaml`. (Same code path; a
single format-agnostic assertion is sufficient.)

**R7.** All of R1–R5 also apply to the `comp` command's structured
output, including nested `impactAnalysis[].downstreamChanges` payloads.

- **AC7.1.** For a `comp` JSON output where an XR's downstream resource
  is modified with a mix of ignored and non-ignored fields, the
  `impactAnalysis[N].downstreamChanges.changes[M].diff.old / .diff.new`
  entries must not contain ignored paths.

**R8.** The fix must not change human-readable (`--output diff`) output
in any way. All existing golden `.ansi` expectation files must continue
to pass.

- **AC8.1.** The full unit + integration test suite passes, including
  every ANSI golden file.

## Testing Plan

### Unit tests (renderer package)

Add to `cmd/diff/renderer/structured_renderer_test.go`:

- `TestStructuredDiffRenderer_ModifiedRespectsIgnorePaths` — table-driven,
  building `ResourceDiff` fixtures directly, running the renderer against
  captured `bytes.Buffer`, asserting on parsed JSON:
  - `case_only_ignored_annotation`: only ignored annotation differs →
    summary all zero, changes empty. Covers AC5.2.
  - `case_only_owner_references`: only `metadata.ownerReferences` differs
    → same expectation, no user `--ignore-paths` needed. Covers AC5.1
    and AC2.2.
  - `case_ignored_plus_non_ignored`: ignored path AND `spec.forProvider`
    differ → summary.modified=1, changes[0].diff.old/new both lack
    ignored path, both contain the non-ignored change. Covers AC1.1
    and AC5.3.
  - `case_server_side_fields_stripped`: current has managedFields,
    resourceVersion, uid, status; JSON diff.old contains none of these.
    Covers AC2.1.

- `TestStructuredDiffRenderer_AddedRespectsIgnorePaths` — AC3.1: added
  resource with ignored annotation → JSON `changes[0].diff.spec` lacks the
  annotation.

- `TestStructuredDiffRenderer_RemovedRespectsIgnorePaths` — AC4.1.

- `TestStructuredDiffRenderer_YAMLRespectsIgnorePaths` — one YAML variant
  of the mixed case to satisfy R6.

The renderer needs `IgnorePaths` visible on `DiffOptions`, which it
already is (line 55–58 of `diff_formatter.go`).

### Composition renderer tests

Add to `cmd/diff/renderer/comp_diff_renderer_test.go`:

- `TestCompDiffRenderer_DownstreamRespectsIgnorePaths` — build a
  `CompDiffOutput` with one composition, one XR impact, one
  `Modified` downstream diff having ignored + non-ignored changes.
  Assert JSON `impactAnalysis[0].downstreamChanges.changes[0].diff.old`
  lacks the ignored path. Covers AC7.1.

### Integration tests

Extend `cmd/diff/diff_integration_test.go`:

- Extend the existing `IgnorePathsArgoCD` test (currently only covers
  the "only-ignored" case with expected summary 0,0,0) — split into two:
  - Keep the existing case as-is (regression coverage for count
    correctness).
  - Add `IgnorePathsMixedChanges`: same fixtures but with an XR that
    also changes a non-ignored `spec.forProvider.configData` field.
    Assert summary is 1 modified (or 2, depending on downstream) AND
    that the JSON `diff.old`/`diff.new` for the affected resource does
    not contain the ignored annotation key.

- Optionally: add one similar case to `TestCompDiffIntegration`
  (already has `CompositionDiffIgnorePaths` — extend or add a mixed
  variant).

### Regression tests

Existing tests must still pass:
- `TestGenerateDiffWithOptions` (renderer)
- All `TestDiffIntegration` and `TestCompDiffIntegration` subtests
- Golden `.ansi` files for human-diff mode

## Implementation Plan

Following TDD: write failing test, implement, verify. Each step below is
a discrete commit-worthy change.

### Step 1 — Extract a shared helper for structured-mode cleanup

Introduce (or reuse) a callable that produces a cleaned deep-copy suitable
for structured emission.

Two approaches; take the simplest that works:

- **Approach A** — export `cleanupForDiff` from the `renderer` package
  under its existing name (it's already package-level, just currently
  lowercase). Since callers are all inside the same package
  (`structured_renderer.go`, `comp_diff_renderer.go`), no renaming or
  export needed — the helper is already reachable. Confirm this
  quickly, then skip to Step 2.

- **Approach B** — if there's any cross-package call site that needs the
  helper, add a thin `CleanupForDiff` exported wrapper.

Approach A is expected to be sufficient because both files are in
`package renderer`. No test needed for this step.

### Step 2 — Failing unit test for R1/R5 (mixed ignored + non-ignored)

Add `TestStructuredDiffRenderer_ModifiedRespectsIgnorePaths` with the
`case_ignored_plus_non_ignored` subcase only. Expected to fail against
current code (ignored path present in JSON diff.old/new).

### Step 3 — Fix `buildDiffDetail` and `resourceDiffToChangeDetail`

Modify both functions in `structured_renderer.go` so that:

- For `DiffTypeAdded`: `detail[DiffKeySpec] = cleanupForDiff(diff.Desired.DeepCopy(), logger, opts.IgnorePaths).Object`
- For `DiffTypeRemoved`: `detail[DiffKeySpec] = cleanupForDiff(diff.Current.DeepCopy(), logger, opts.IgnorePaths).Object`
- For `DiffTypeModified`: same treatment for both `Current` and `Desired`.

`buildDiffDetail` is a method on `StructuredDiffRenderer` and already has
access to `r.opts.IgnorePaths` and `r.logger`. `resourceDiffToChangeDetail`
is a free function; it will need `IgnorePaths` and a logger. Add
parameters to its signature and update the two call sites in
`comp_diff_renderer.go`.

Run Step 2's test — should now pass.

### Step 4 — Fill in remaining unit test cases

Add the other subcases from the testing plan
(`case_only_ignored_annotation`, `case_only_owner_references`,
`case_server_side_fields_stripped`, plus the Added / Removed / YAML tests).
Confirm all pass.

### Step 5 — Composition renderer test

Add `TestCompDiffRenderer_DownstreamRespectsIgnorePaths`. Verify it
passes with the Step 3 fix.

### Step 6 — Integration test coverage

Add `IgnorePathsMixedChanges` to `TestDiffIntegration`. Confirm it
passes.

### Step 7 — Full suite regression

Run `go test ./...` and confirm all tests still pass.

### Step 8 — Docs

Per project CLAUDE.md keep-docs-in-sync policy:
- Design doc §6.8 (renderer contract) — note that structured output
  respects `--ignore-paths` and server-side cleanup, matching human diff.
- README `--ignore-paths` mention — clarify that ignore-paths apply to
  all output modes.

No mermaid diagrams need updating (no architectural change).

### Step 9 — Commit + PR

- Sign every commit with `-s` (DCO).
- Open PR as draft, follow the PR template exactly.
- Fixes reference: none yet — link the reporter's message in PR body if
  no GH issue.
