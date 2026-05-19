# Composition Diff: `--resource` filter (Issue #321)

## As Is

`crossplane-diff comp <file>` always discovers the full set of composites (XRs and Claims) that use the supplied composition by listing every resource of the composition's target XR GVK (and its claim GVK, if the XRD defines one) in the cluster, then filtering those that reference the composition by name. The only narrowing knob today is `--namespace` (default `default`; empty = all namespaces) which is applied at list time.

For a user running `comp` in a CI / PR-validation context against a cluster with hundreds of XRs/Claims, every run does a full LIST + per-XR render. The runtime grows linearly with composite count, even when the reviewer only cares about a representative subset (or one specific Claim during composition development/debug).

Key facts about the existing plumbing:

- `CompCmd` (`cmd/diff/comp.go`) declares `Namespace string` and passes it as the third argument to `proc.DiffComposition(ctx, compositions, c.Namespace)`.
- `DefaultCompDiffProcessor.processSingleComposition` (`cmd/diff/diffprocessor/comp_processor.go:234`) calls `p.compositionClient.FindCompositesUsingComposition(ctx, name, namespace)` to discover affected composites.
- `CompositionClient.FindCompositesUsingComposition` (`cmd/diff/client/crossplane/composition_client.go:692`) returns the union of XRs (cluster-scoped or namespaced; v1 or v2) and Claims (always namespaced) that reference the composition. v1 vs v2 is transparent to callers — both are `*unstructured.Unstructured`.
- `ResourceClient.GetResource(ctx, gvk, namespace, name)` (`cmd/diff/client/kubernetes/resource_client.go:21`) already supports name-keyed lookups; we don't need a new low-level Kubernetes plumbing.
- Kong's default behavior for `[]string` flags: comma-separated **and** repeatable simultaneously (`tag.go:261` — `Sep` defaults to `,`). So one `[]string` field handles both `--resource=a --resource=b` and `--resource=a,b` with no extra tags.
- `IntegrationTestCase` (`cmd/diff/diff_integration_test.go:44`) already gates `namespace` to `CompositionDiffTest` (line 219). A parallel `resources []string` gate would mirror that pattern.

## To Be

`crossplane-diff comp` accepts a new `--resource` flag (singular, repeatable, also accepts comma-separated values) that constrains the impact analysis to a user-supplied set of named composites:

- Each value is `[namespace/]name`. Bare `name` (no slash) means cluster-scoped lookup (used by v1 XRs and v2 cluster-scoped XRs). `ns/name` is namespaced (used by v2 namespaced XRs and Claims).
- `--resource` and `--namespace` are **mutually exclusive**. Supplying both is a CLI usage error reported during Kong parsing / `AfterApply`.
- When `--resource` is set, the processor **direct-fetches** each named composite via `ResourceClient.GetResource` against (a) the composition's `compositeTypeRef` GVK, then (b) the claim GVK derived from the XRD (if any). Whichever returns 200 wins. Both 404 → resource is "not relevant to this composition".
- For each `(composition, resource)` pair: relevant means the cluster lookup succeeded AND the composite's `spec.compositionRef.name` (or v2 `spec.crossplane.compositionRef.name`) equals the composition's name.
- **Fail-fast preflight.** Before rendering any composition, every `--resource` ref is resolved against every supplied composition's `(XR GVK, Claim GVK)` pair. If any ref is relevant to **zero** of the supplied compositions, the command exits with `ExitCodeToolError` and an error naming the unmatched ref(s) — *no composition diffs are rendered*. This is a CLI/user-input error, distinct from downstream XR processing failures, so render-then-error doesn't apply.
- **Update-policy filtered composites are surfaced explicitly.** When a user-supplied `--resource` matches a composite that the existing `--include-manual` filter would drop (Manual update policy without `--include-manual`), that composite is included in the composition's `ImpactAnalysis` with a new status `XRStatusFilteredByPolicy` (`filtered_by_policy`) and zero diffs. This is visible in both text and structured (JSON/YAML) output. Users see the composite was matched but skipped, and the suggested remediation (`--include-manual`) is mentioned in stderr / the renderer.
- v1 (cluster-scoped XRs + namespaced Claims) and v2 (namespaced or cluster-scoped XRs + namespaced Claims) all work — the GVK pair derived from the composition + XRD covers all cases.
- Help text, README, and CLI examples advertise the new flag.

## Requirements

1. **R1. Flag definition.** `CompCmd` declares `Resources []string` with Kong tag `name:"resource"` and a singular help text (e.g., `"Limit impact analysis to specific composites in [namespace/]name format. Repeatable or comma-separated."`). It uses singular `--resource` because each value is exactly one composite.
2. **R2. Mutual exclusion.** `CompCmd.AfterApply` (or `Run`) returns a clear error if both `c.Namespace != ""` and `len(c.Resources) > 0`. The error mentions both flag names.
3. **R3. Resource value parsing.** A helper parses each `--resource` value into `(namespace, name)` honoring the `[namespace/]name` rule. Empty string, more than one `/`, or empty `name` is a parse error reported with the offending value. Whitespace around values is trimmed.
4. **R4. New CompositionClient method.** `CompositionClient` gains `GetCompositesByName(ctx, comp *apiextensionsv1.Composition, refs []ResourceRef) (matched []*un.Unstructured, unmatched []ResourceRef, err error)`. Implementation:
   - Resolve the composition's XR GVK from `comp.Spec.CompositeTypeRef` (passed as a typed argument so the method works for net-new compositions not yet in the cluster).
   - Resolve the claim GVK via `definitionClient.GetXRDForXR` + `getClaimTypeFromXRD` (may be empty if the XRD is missing or doesn't define claims; that's OK, just skip claim lookups).
   - For each input `ResourceRef`, call `resourceClient.GetResource` against the XR GVK (using the ref's namespace as-is), then if NotFound try the claim GVK.
   - On 200, run the existing `resourceUsesComposition` check; relevant only if it returns true.
   - 404 from both lookups, OR 200 but `resourceUsesComposition == false`, marks the ref as "not relevant for this composition" — returned in the unmatched slice, not as an error.
   - Any non-404 transport error from `GetResource` IS an error (return it).
   - Note: this method does NOT itself apply update-policy filtering — that remains the processor's responsibility (see R5/R5b).
5. **R5. CompDiffProcessor preflight.** Before per-composition processing, `DefaultCompDiffProcessor.DiffComposition` performs a preflight pass: for every supplied composition, it calls `GetCompositesByName` to resolve the user's refs. It builds a `map[compositionName][]*Unstructured` of resolved composites and a global "unmatched" set (refs that no composition matched). If the global unmatched set is non-empty, return an error immediately — *no rendering*.
5a. **R5a. Per-composition processing uses preflight result.** When `--resource` mode is active, `processSingleComposition` skips `FindCompositesUsingComposition` entirely and uses the preflight map's entry for that composition. Composition diff calculation and downstream rendering otherwise unchanged.
5b. **R5b. Filtered-by-policy entries surfaced.** When `--resource` mode is active, the existing `filterXRsByUpdatePolicy` step produces TWO sets: kept (passed to `collectXRDiffs`) and dropped-by-policy. The dropped set is added to the composition's `ImpactAnalysis` with `Status = XRStatusFilteredByPolicy` and zero diffs. The summary `FilteredByPolicy` count still increments so the total is consistent.
   - Default-discovery mode (no `--resource`): existing behavior preserved — filtered composites are NOT added to `ImpactAnalysis`, only counted. Keeps this PR's blast radius tight; we can unify later if desired.
6. **R6. New status enum value.** `cmd/diff/renderer/structured_renderer.go`: add `XRStatusFilteredByPolicy XRStatus = "filtered_by_policy"`. Update the renderer's status handling:
   - Text renderer (`comp_diff_renderer.go::buildXRStatusList`): add a `case XRStatusFilteredByPolicy` branch with a clear visual marker (e.g., `⊘ <kind>/<name> (filtered: Manual update policy)`). The renderer also notes "use --include-manual to include these" in the section header when any filtered entries are present.
   - JSON/YAML renderer: status string serializes to `"filtered_by_policy"`; downstream changes section is omitted (the field is `omitempty`).
   - `CompositionDiff.HasChanges()` returns false for `XRStatusFilteredByPolicy` (filtered composites are not "changes").
7. **R7. Help & docs.** `CompCmd.Help()` adds usage examples mirroring the issue's description, plus a note about Manual policy / `--include-manual`. `README.md` (in the `comp` flags / examples block) documents the flag and includes one example covering the filtered-by-policy case.
8. **R8. Test harness extension.** `IntegrationTestCase` gains a `resources []string` field gated to `CompositionDiffTest` (parallel to the existing `namespace` gate). `runIntegrationTest` appends `--resource=<v>` args one per entry (so the test exercises the repeatable form by default). A second harness option (`resourcesCSV string`) handles the comma-separated test variant; alternatively, a single test case toggles the join style at arg-construction time.

### Acceptance Criteria

- **AC1 (R1).** `crossplane-diff comp --help` lists `--resource` with `[namespace/]name` example syntax and notes that values are repeatable / comma-separated.
- **AC2 (R2).** Running `comp foo.yaml -n bar --resource=baz/qux` exits with a non-zero exit code and stderr includes both `--namespace` and `--resource` in the error message. Unit/integration test asserts.
- **AC3 (R3).** Unit tests cover:
  - `name` → `("", "name")`
  - `ns/name` → `("ns", "name")`
  - ` ns/name ` (whitespace) → `("ns", "name")`
  - `""` → error
  - `ns/` → error (empty name)
  - `ns/name/extra` → error (too many slashes)
  - `/name` → error (empty namespace) — preferable to silently treating as cluster-scoped because the user's intent is clearly namespaced.
- **AC4 (R4).** Unit tests for `GetCompositesByName` with mocked `resourceClient` cover: XR-only hit, Claim-only hit, both-404 (returns ref as unmatched), 200-but-uses-different-composition (unmatched), transport-error propagation. v1 cluster-scoped XR (empty namespace ref) and v2 namespaced XR/Claim are both exercised.
- **AC5 (R5).** Integration test `ResourceFilterScopesAffectedXRs`: cluster has 3 XRs using composition `c`; running `comp c.yaml --resource=default/xr-1 --resource=default/xr-2` shows impact only for those two and ignores `xr-3`. Asserts via `tu.ExpectCompDiff()` JSON output.
- **AC6 (R5).** Integration test `ResourceFilterCommaSeparated`: same scenario but invoked with `--resource=default/xr-1,default/xr-2`. Equivalent result. Confirms kong's auto-parsing.
- **AC7 (R5).** Integration test `ResourceFilterClusterScoped`: a v1 XRD with cluster-scoped XR; `--resource=xr-1` (no namespace) matches.
- **AC8 (R5).** Integration test `ResourceFilterClaim`: composition with claim-bearing XRD; `--resource=ns/my-claim` looks up via the claim GVK (XR GVK 404s in this case).
- **AC9 (R5).** Integration test `ResourceFilterUnmatched`: `--resource=default/does-not-exist` produces `ExitCodeToolError` (non-zero), the error message names the unmatched ref, and **no composition impact analysis is rendered** (preflight fails early). Stderr carries the error in human-readable form; structured-output mode still emits a valid empty/error-only JSON object so CI tooling can parse it.
- **AC10 (R5b/R6).** Integration test `ResourceFilterRespectsManualPolicy`: setup includes a Manual-policy XR (`spec.crossplane.compositionUpdatePolicy: Manual` on v2, or v1 equivalent). Run with `--resource=default/manual-xr` and WITHOUT `--include-manual`. Assert via JSON: the impact analysis contains exactly one entry for `manual-xr` with `status == "filtered_by_policy"` and no downstream changes. Re-run with `--include-manual` and assert the same XR shows up with `status == "changed"` (or `"unchanged"`) and an evaluated diff.
- **AC11 (R7).** README diff under `comp` reference block shows the new flag and includes the filtered-by-policy note; `--help` smoke test in CI passes.
- **AC12 (R8).** Existing comp tests remain green (regression). Existing test `CompositionChangeImpactsXRs` still uses `namespace: "default"` and continues to pass (mutex check only fires when BOTH `--namespace` and `--resource` are set).
- **AC13.** If a `--resource` value names a composite that exists but uses a *different* composition (not present in the supplied files), it is treated as "not relevant" — not a special-case error. Existing `resourceUsesComposition` handles this. The ref is reported as unmatched if no other supplied composition claims it.

## Testing Plan

TDD — red tests first, smallest steps. Tests live alongside the code they exercise.

### T1. Unit: `parseResourceRef` (R3 / AC3)

**Location:** new file `cmd/diff/comp_test.go` (or extend an existing one if a `comp_test.go` exists; otherwise create alongside `comp.go`).

**Cases:** the seven enumerated in AC3 above. Table-driven.

**Red:** test fails to compile until `parseResourceRef` exists; first commit adds the test + minimal stub returning errors for everything; second commit fills in logic to pass each case.

### T2. Unit: `CompCmd.AfterApply` mutex (R2 / AC2)

**Location:** `cmd/diff/comp_test.go`.

**Shape:** construct a `CompCmd{Namespace: "x", Resources: []string{"y/z"}}`, run `AfterApply` with a stub kong context, assert error contains both `--namespace` and `--resource`. (If `AfterApply` is too dependent on kong wiring, place this validation in a helper and unit-test the helper directly.)

### T3. Unit: `GetCompositesByName` on `DefaultCompositionClient` (R4 / AC4)

**Location:** `cmd/diff/client/crossplane/composition_client_test.go`.

**Mocks:** existing `MockResourceClient` (or build one if absent under `cmd/diff/testutils`). Cases:
1. XR-GVK GET 200, refs the composition → returned, no unmatched.
2. XR-GVK GET 404, claim-GVK GET 200, refs the composition → returned, no unmatched.
3. XR-GVK GET 404, claim-GVK GET 404 → unmatched.
4. XR-GVK GET 200, but `resourceUsesComposition == false` → unmatched.
5. XR-GVK GET returns transport error (not NotFound) → propagated as error.
6. v1 cluster-scoped XR (empty-namespace ref) → `GetResource(gvk, "", name)` is called.
7. Composition with no claim GVK → only XR-GVK lookup is attempted; XR-GVK 404 → unmatched (does not crash).

### T4. Unit: composition processor wiring (R5/R5a/R5b / partial AC5, AC10)

**Location:** `cmd/diff/diffprocessor/comp_processor_test.go`.

**Shape:** with mocked `compositionClient`, prove that:
- When the resource list is empty, `FindCompositesUsingComposition` is called (no behavior change).
- When the resource list is non-empty, the preflight loop calls `GetCompositesByName` for every supplied composition and `FindCompositesUsingComposition` is NOT called.
- Unmatched refs are aggregated across compositions and surface as an error from `DiffComposition` BEFORE any rendering occurs. Mock the renderer; assert it was NOT called when the unmatched-error path fires.
- In `--resource` mode, when a matched composite has Manual update policy and `--include-manual` is false, the resulting `CompositionDiff.ImpactAnalysis` contains an entry with `Status == XRStatusFilteredByPolicy` and the kept set passed to `collectXRDiffs` excludes that composite.
- In default-discovery mode (no `--resource`), filtered composites are NOT added to `ImpactAnalysis` (regression: existing behavior preserved).

### T5. Integration: `ResourceFilterScopesAffectedXRs` (AC5)

**Location:** `cmd/diff/diff_integration_test.go`, inside `TestCompDiffIntegration`.

Reuse `testdata/comp/resources/xrd.yaml`, `original-composition.yaml`, `existing-xr-1.yaml`, `existing-xr-2.yaml`, plus a third XR fixture (or a copy). Setup all three; diff `updated-composition.yaml`; pass `resources: []string{"default/xr-1", "default/xr-2"}`. Use `outputFormat: "json"` and assert via `tu.ExpectCompDiff()` that the impact analysis includes exactly those two and not the third.

### T6. Integration: `ResourceFilterCommaSeparated` (AC6)

Same as T5 but with the test harness configured to emit a single `--resource=a,b` arg (a small variant on the gate added in R8). This is mainly a kong-parsing smoke test; one such case is enough.

### T7. Integration: `ResourceFilterClusterScoped` (AC7)

Reuse a v1 cluster-scoped XR fixture (or add one if not present under `testdata/comp/resources/`). `resources: []string{"xr-1"}` (no namespace). Assert it matches.

### T8. Integration: `ResourceFilterClaim` (AC8)

Use a setup with a claim-defining XRD + a Claim resource. `resources: []string{"default/my-claim"}`. Assert it matches via the claim-GVK lookup branch.

### T9. Integration: `ResourceFilterUnmatched` (AC9)

`resources: []string{"default/does-not-exist"}`. Expect non-zero exit code (`ExitCodeToolError`), `expectedErrorContains: "does-not-exist"`. Confirms preflight fail-fast (R5).

### T9b. Integration: `ResourceFilterRespectsManualPolicy` (AC10)

Setup: composition + matching XR with `spec.crossplane.compositionUpdatePolicy: Manual` (or v1 equivalent). Run twice:
1. `resources: []string{"default/manual-xr"}` (no `--include-manual`) → JSON output asserts a single `XRImpact` with `status: "filtered_by_policy"`. Exit code reflects no diffs detected (filtered != changed).
2. `resources: []string{"default/manual-xr"}` + `--include-manual` → JSON output asserts the XR shows up with `status: "changed"` (or `"unchanged"`) and an evaluated diff.

### T10. Regression — full `cmd/diff/...` test sweep

`go test ./cmd/diff/...` after each step. Existing comp tests must stay green.

## Implementation Plan

Smallest possible steps, each paired with its verification.

### Step 1: Failing unit test for `parseResourceRef` (RED)

**Change:** Add `cmd/diff/comp_test.go` (if absent) with the table-driven test for `parseResourceRef` (T1). Reference the parser symbol, which doesn't exist yet — compile error confirms RED.

**Verify:** `go test ./cmd/diff -run TestParseResourceRef` fails to compile.

### Step 2: Add `parseResourceRef` (GREEN for Step 1)

**Change:** In `cmd/diff/comp.go`, add a `ResourceRef` struct (`{Namespace, Name string}`) and a `parseResourceRef(value string) (ResourceRef, error)` helper covering all AC3 cases.

**Verify:** `go test ./cmd/diff -run TestParseResourceRef` passes.

### Step 3: Failing unit test for mutex validation (RED)

**Change:** Add `TestCompCmd_NamespaceAndResourceMutuallyExclusive` to `cmd/diff/comp_test.go`. Construct a `CompCmd` with both fields populated; call the validation helper (which doesn't exist yet) and expect an error.

**Verify:** Compile fail or test fail until Step 4.

### Step 4: Add mutex validation + Resources field (GREEN for Step 3)

**Change:**
1. Add `Resources []string` field to `CompCmd` with the kong tag (R1).
2. Add `validateFlags()` method (or inline in `AfterApply`) that returns an error if both `Namespace` and `Resources` are set.

**Verify:** Step 3 test passes. `crossplane-diff comp --help` (manual or smoke test) shows the new flag.

### Step 5: Failing unit test for `GetCompositesByName` (RED)

**Change:** Add `TestGetCompositesByName` in `cmd/diff/client/crossplane/composition_client_test.go` with the seven cases from T3. Expect the symbol not to exist yet.

**Verify:** Compile fail.

### Step 6: Implement `GetCompositesByName` (GREEN for Step 5)

**Change:** In `cmd/diff/client/crossplane/composition_client.go`:
1. Extend the `CompositionClient` interface with `GetCompositesByName(ctx context.Context, comp *apiextensionsv1.Composition, refs []ResourceRef) (matched []*un.Unstructured, unmatched []ResourceRef, err error)`. (Place `ResourceRef` in a stable package — likely `cmd/diff/types/` or co-located with the interface. Decide during implementation; favor `cmd/diff/types/` to avoid cyclic imports if the type is shared with `comp.go` parsing.)
2. Implement on `DefaultCompositionClient`: derive XR GVK from `comp.Spec.CompositeTypeRef` and claim GVK via XRD lookup once per call, then loop refs calling `resourceClient.GetResource` with NotFound-tolerance, then `resourceUsesComposition` filter against `comp.GetName()`.
3. Update `MockCompositionClient` + builder under `cmd/diff/testutils/` with the new method (matching existing patterns).

**Verify:** All cases in Step 5 pass. `go test ./cmd/diff/client/crossplane/...` green.

### Step 7: Add `XRStatusFilteredByPolicy` (RED on a focused renderer test)

**Change:** Failing test first — extend an existing comp renderer test (`cmd/diff/renderer/comp_diff_renderer_test.go`) or add a small unit test asserting:
1. JSON output for an `XRImpact` with `Status = XRStatusFilteredByPolicy` serializes `"status": "filtered_by_policy"` and omits `downstreamChanges`.
2. Text renderer prints a recognizable filtered-by-policy line (`⊘` or similar marker + `(Manual update policy)` suffix).
3. `CompositionDiff.HasChanges()` returns false when the only impacts are `XRStatusFilteredByPolicy`.

Then add `XRStatusFilteredByPolicy` const in `structured_renderer.go` and the renderer cases in `comp_diff_renderer.go::buildXRStatusList` and JSON conversion path. Update `HasChanges()` to short-circuit appropriately.

**Verify:** Renderer tests pass.

### Step 8: Failing processor preflight test (RED)

**Change:** Add T4 cases to `cmd/diff/diffprocessor/comp_processor_test.go`. The test calls a not-yet-existing variant of `DiffComposition` (or sets a not-yet-existing config field). Cases must include:
1. Empty resources → `FindCompositesUsingComposition` called, `GetCompositesByName` NOT called.
2. Non-empty resources, all match → `GetCompositesByName` called per composition, `FindCompositesUsingComposition` NOT called.
3. Non-empty resources, one ref globally unmatched → preflight returns error and renderer is NEVER invoked (mock the renderer).
4. Non-empty resources, one match has Manual policy without `--include-manual` → `ImpactAnalysis` includes it with `XRStatusFilteredByPolicy`; `collectXRDiffs` is called only with the kept set.

**Verify:** Compile fail or assertion fail.

### Step 9: Wire `[]ResourceRef` + preflight (GREEN for Step 8)

**Change:**
1. Update `CompDiffProcessor` interface: `DiffComposition(ctx, compositions, namespace, resources []ResourceRef) (bool, error)`. Update all callers (`comp.go`, tests). Single signature change beats dual-path config plumbing.
2. In `DefaultCompDiffProcessor.DiffComposition`:
   - If `len(resources) > 0`: convert each composition once to typed (reusing existing pattern in `collectXRDiffs`), then for each composition call `GetCompositesByName(ctx, typedComp, refs)`. Build `perCompMatched map[string][]*Unstructured` and accumulate `globallyUnmatched []ResourceRef` (a ref globally unmatched only if every composition returned it as unmatched).
   - If `len(globallyUnmatched) > 0`: return `ExitCodeToolError` error naming the unmatched refs immediately (before any rendering).
3. `processSingleComposition` accepts an optional `preMatched []*Unstructured` (or reads from the preflight map). When provided, skip `FindCompositesUsingComposition` and use the pre-matched set.
4. Update `filterXRsByUpdatePolicy` (or its caller) to also return the *dropped* set when in `--resource` mode. Add the dropped set to `ImpactAnalysis` with `XRStatusFilteredByPolicy`. Default-discovery mode keeps the existing count-only behavior.
5. In `cmd/diff/comp.go::Run`, parse `c.Resources` via `parseResourceRef` (errors → `ExitCodeToolError`), then pass into `DiffComposition`.

**Verify:** Step 8 tests pass; existing `comp_processor_test.go` cases still pass.

### Step 10: Failing integration test `ResourceFilterScopesAffectedXRs` (RED)

**Change:** Add T5 case + extend `IntegrationTestCase` with `resources []string` and gate at `runIntegrationTest` (R8): when set and `testType == CompositionDiffTest`, append `--resource=<v>` for each entry.

**Verify:** Run `go test ./cmd/diff -run TestCompDiffIntegration/ResourceFilterScopesAffectedXRs -v` — expect failure or pass depending on Step 9 completeness.

### Step 11: GREEN + remaining integration tests

For each of T6, T7, T8, T9, T9b in turn: add the test (RED if it surfaces a gap), then make GREEN. These exercise comma-separated form, cluster-scoped XR, claim, unmatched-error semantics, and Manual-policy filtering respectively.

**Verify:** Each test passes; full `TestCompDiffIntegration` green.

### Step 12: Help text + README

**Change:**
- Add example lines to `CompCmd.Help()`:
  ```
    # Limit impact analysis to specific composites
    crossplane-diff comp updated-composition.yaml --resource=default/my-claim
    crossplane-diff comp updated-composition.yaml --resource=default/xr-1,default/xr-2

    # Note: --resource cannot be combined with --namespace.
    # Composites with Manual update policy are shown as "filtered_by_policy"
    # unless --include-manual is also passed.
  ```
- Add `--resource` to the README's `comp` flags reference and an example under "Composition Diff" usage covering the filtered-by-policy callout.

**Verify:** Visual review; `go test ./cmd/diff/...`.

### Step 13: Full sweep

**Verify:**
- `go test ./cmd/diff/...`
- `earthly +go-test` if time permits
- Spot-check: run `crossplane-diff comp --help` against the locally-built binary to confirm flag presence and help formatting.

## Open Questions / Notes

- **Multi-composition + per-composition unmatched semantics (R5).** A ref is "globally unmatched" only if every supplied composition rejects it. Single-composition usage (the issue's primary case) collapses to "this ref doesn't apply to this composition → error" — exactly what users expect. Preflight runs to completion across all compositions before erroring so the error message can list every unmatched ref at once (better UX than failing on the first miss).
- **Why fail-fast over render-then-error.** The render-then-error pattern at `comp_processor.go:211-222` exists so that *partial* impact analysis stays visible when *some* XRs fail rendering. That's about downstream processing failures. An unmatched `--resource` is a CLI input error — there's no useful partial information to show. Fail-fast also prevents misleading "0 affected" output on typos.
- **Why `XRStatusFilteredByPolicy` instead of a boolean.** The composite's evaluation state is genuinely "we did not evaluate this" — distinct from "evaluated and unchanged" or "evaluated and changed". A boolean alongside one of those statuses would mislead JSON consumers. A new status accurately models the state and is mutually exclusive with the others, matching the existing `Changed`/`Unchanged`/`Error` model.
- **Default-discovery mode preserves existing behavior.** Filtered-by-policy entries are NOT added to `ImpactAnalysis` when running without `--resource` — only counted in the summary. Keeps the PR scope focused on the new flag's UX. A future change can unify if users want listing-by-default.
- **Net-new compositions** (composition file doesn't exist in cluster yet): `GetCompositesByName` takes the typed composition as an argument so it doesn't need to fetch from the cluster. XR GVK comes from the file's `compositeTypeRef`. The XRD lookup is the only cluster dependency for deriving the claim GVK; if the XRD also doesn't exist, claim-lookup is skipped and only the XR-GVK branch runs.
