# Refactor: clean up the `--resource` filter architecture

Companion plan file: `~/.claude/plans/peppy-sniffing-valley.md`. This document is the authoritative source during implementation.

## As Is

PR #322 (commit `766011d` + `538a161` lint fixes) merged the `--resource [namespace/]name` flag for `crossplane-diff comp`, which limits impact analysis to specific composites. The implementation works end-to-end and all tests pass. However, several internal abstractions don't sit cleanly:

1. **Boolean mode flag with redundant signaling.** `processSingleComposition(ctx, newComp, namespace, preMatched []*Unstructured, resourceMode bool)` at `cmd/diff/diffprocessor/comp_processor.go:326` takes both a `preMatched` slice AND a `resourceMode` bool. The two parameters signal the same mode (`resourceMode==true ⇔ preMatched is the source of truth`), with the bool *additionally* gating an unrelated behavior — whether to surface filtered composites in `ImpactAnalysis` as `XRStatusFilteredByPolicy`.

2. **Interface bloat on `CompositionClient`.** Two methods overlap:
   - `FindCompositesUsingComposition(ctx, name string, namespace string) ([]*Unstructured, error)` — discovery via cluster listing
   - `GetCompositesByName(ctx, comp *Composition, refs []ResourceRef) ([]*Unstructured, []ResourceRef, error)` — lookup by user-named refs

   They answer the same question ("which composites in the cluster reference this composition?") with different lookup strategies. The interface now has 6 methods, tripping the `interfacebloat` linter and forcing a `//nolint:interfacebloat` directive at `composition_client.go:23`.

3. **Unused return value.** `GetCompositesByName` returns `(matched, unmatched, error)` but its only caller `preflightResourceRefs` at `comp_processor.go:279` ignores the `unmatched` slice (`matched, _, err := ...`) and re-derives unmatched-ness itself from `matched` (lines 286-303).

4. **Long, multi-phase function.** `GetCompositesByName` at `composition_client.go:838-942` is ~100 lines doing two distinct phases inline: (a) GVK resolution (XR GVK from composition spec, claim GVK from XRD) and (b) per-ref iteration with claim-GVK fallback.

5. **Home-grown name+namespace type.** `cmd/diff/types/types.go:33-45` defines `ResourceRef{Namespace, Name string}` with a `String()` method that returns bare `"foo"` for cluster-scoped (empty namespace) and `"default/foo"` for namespaced. `k8s.io/apimachinery/pkg/types.NamespacedName` already exists with the same data shape, but its `String()` always returns `Namespace + "/" + Name` — so cluster-scoped renders as `"/foo"`. To preserve the user-facing rendering, the swap requires a small `formatRef` helper.

6. **`cmd/diff/types/` package** also contains `CompositionProvider` — a `func(ctx, *Unstructured) (*Composition, error)` used by `diff_processor.go`, `comp_processor.go`, `mocks.go` etc. The package can NOT be deleted; only `ResourceRef` (and its `String()`) can be removed. The pre-existing `revive: var-naming: avoid meaningless package names` warning at `types.go:18` therefore stays — fixing it would require a package rename touching every import site, which is out of scope for this refactor.

## To Be

After the refactor:

1. **No mode flag with redundant signaling.** `processSingleComposition(ctx, newComp, affectedXRs []*Unstructured, surfaceFiltered bool)` always takes a pre-resolved slice. The remaining boolean `surfaceFiltered` honestly names what it controls (whether dropped-by-policy XRs go into `ImpactAnalysis` as `XRStatusFilteredByPolicy` entries, vs. only being counted in the summary). The discovery-vs-preMatched decision is hoisted up to `DiffComposition`.

2. **One unified method on `CompositionClient`.** `FindComposites(ctx, comp *Composition, opts FindCompositesOptions) ([]*Unstructured, error)`. `FindCompositesOptions` has `Namespace string` (for default discovery) and `Refs []NamespacedName` (for user-named lookup). The two old methods are gone. Interface size returns to 5 methods. `//nolint:interfacebloat` directive removed.

3. **No unused return.** `FindComposites` returns `([]*Unstructured, error)`. Globally-unmatched derivation stays in the processor where it belongs (it's a cross-composition concept, not a per-call concept).

4. **`findByRefs` split into two clear phases:** `resolveCompositeTypes(ctx, comp)` returns a `compositeTypes{xrGVK, claimGVK}` struct, and `lookupRef(ctx, ref, types, compName)` does the per-ref XR-then-claim lookup.

5. **`NamespacedName` everywhere `ResourceRef` was used.** All `[]dtypes.ResourceRef` slices become `[]k8stypes.NamespacedName`. `ResourceRef` is removed from `cmd/diff/types/types.go`; the package itself stays (it still hosts `CompositionProvider`). A small `formatRef(NamespacedName) string` helper renders cluster-scoped refs as bare `name` (preserving the current contract). Production `ref.String()` call sites that were rendering for human consumption switch to `formatRef(ref)`. Function signatures updated; tests updated; mocks updated.

6. **Lint clean.** `interfacebloat` and `revive: package types` issues both resolved. `golangci-lint run ./cmd/diff/...` produces zero new issues over the post-PR-#322 baseline.

User-visible CLI behavior is **unchanged**. All E2E tests should continue to pass without modification.

## Requirements

### R1 — `ResourceRef` is replaced by `k8s.io/apimachinery/pkg/types.NamespacedName`

The `ResourceRef` struct and its `String()` method are removed from `cmd/diff/types/types.go`. The `CompositionProvider` declaration in that file stays (it's used elsewhere). Every reference to `dtypes.ResourceRef` in production and test code switches to `k8stypes.NamespacedName` (alias `k8stypes "k8s.io/apimachinery/pkg/types"`). A `formatRef(NamespacedName) string` helper is added in `cmd/diff/comp.go` to preserve the bare-name rendering for cluster-scoped refs in user-facing strings (error messages, logs).

### R2 — `CompositionClient` exposes a single `FindComposites` method with options

The interface methods `FindCompositesUsingComposition` and `GetCompositesByName` are removed. A new method `FindComposites(ctx, comp *Composition, opts FindCompositesOptions) ([]*Unstructured, error)` replaces both. `FindCompositesOptions` contains:
- `Namespace string` — scopes default-discovery
- `Refs []NamespacedName` — limits results to user-named refs

When `len(opts.Refs) > 0`, the implementation performs ref-based lookup; otherwise it performs default discovery scoped to `opts.Namespace`.

### R3 — Internal phase split on the ref-based lookup

The implementation has two private helpers: `resolveCompositeTypes(ctx, comp) (compositeTypes, error)` and `lookupRef(ctx, ref, types, compName) (*Unstructured, error)`. The composite `compositeTypes` struct holds `{xrGVK, claimGVK}`. `lookupRef` returns nil for "not found or wrong composition" and propagates non-404 cluster errors.

### R4 — `processSingleComposition` takes pre-resolved XRs and a single mode flag

Signature: `processSingleComposition(ctx, newComp *Unstructured, affectedXRs []*Unstructured, surfaceFiltered bool) (*CompositionDiff, error)`. The caller (`DiffComposition`) decides which set of XRs to pass and whether `surfaceFiltered` should be true. `processSingleComposition` no longer calls `FindCompositesUsingComposition`/`FindComposites` itself.

### R5 — Discovery branch lives in `DiffComposition`'s per-composition loop, structured with `switch` not `if/else`

For each Composition (after the `comp.GetKind() != "Composition"` filter), `DiffComposition`:
- If `len(resources) > 0`: takes `affectedXRs` from `preflightMatches[comp.GetName()]`; sets `surfaceFiltered=true`
- Else: calls `p.compositionClient.FindComposites(ctx, typedComp, FindCompositesOptions{Namespace: namespace})`; on error, logs at debug level and uses `nil` (net-new composition graceful path); sets `surfaceFiltered=false`

The branching uses `switch` blocks at both the outer (resource-mode vs default) and inner (find-error vs ok) levels. No `if/else` chains.

### R6 — Mocks and tests are migrated to the new method

`MockCompositionClient` has `FindCompositesFn` (replacing `FindCompositesUsingCompositionFn` and `GetCompositesByNameFn`). `MockCompositionClientBuilder` has `WithFindComposites(fn)`. The convenience wrappers `WithResourcesForComposition` and `WithFindResourcesError` keep their existing public surface but route through `FindCompositesFn`, disambiguating from ref-mode by checking `len(opts.Refs) == 0`.

All affected tests (`comp_processor_test.go`, `composition_client_test.go`, `comp_test.go`) pass with the new types and mock API. Test data shape switches from `dtypes.ResourceRef` to `k8stypes.NamespacedName`. Expected `String()` outputs are unchanged.

### R7 — `//nolint:interfacebloat` is removed

The directive at `composition_client.go:23` is deleted; lint passes without it.

## Acceptance Criteria

### AC for R1 (NamespacedName replaces ResourceRef)

- AC1.1: `cmd/diff/types/types.go` no longer declares a `ResourceRef` type or its `String()` method. `CompositionProvider` is still present.
- AC1.2: `grep -rn "dtypes\.ResourceRef\|\\btypes\\.ResourceRef\\b" cmd/diff/` returns no matches (production or test).
- AC1.3: `grep -rn "k8stypes\\.NamespacedName" cmd/diff/` returns matches in: `comp.go`, `comp_processor.go`, `composition_client.go`, `comp_test.go`, `comp_processor_test.go`, `composition_client_test.go`, `mocks.go`, `mock_builder.go`.
- AC1.4: A `formatRef` function exists in `cmd/diff/comp.go` and renders cluster-scoped (empty namespace) as bare `Name` and namespaced as `Namespace + "/" + Name`. Unit tests cover both cases.
- AC1.5: All existing user-facing rendering of refs (error messages and structured logs) is identical to today: `default/foo` for namespaced, `foo` (NOT `/foo`) for cluster-scoped. Verified by adapted `TestParseResourceRef` (string outputs unchanged) and at least one preflight error-message test exercising the cluster-scoped case.

### AC for R2 (unified FindComposites)

- AC2.1: `CompositionClient` interface in `composition_client.go` has exactly 5 methods (was 6). `FindComposites` is one of them; `FindCompositesUsingComposition` and `GetCompositesByName` are absent.
- AC2.2: `FindCompositesOptions` struct exists in `composition_client.go` with exported fields `Namespace string` and `Refs []k8stypes.NamespacedName`.
- AC2.3: A unit test calls `FindComposites` with `opts.Refs == nil` and gets default-discovery behavior (matches the old `FindCompositesUsingComposition` semantics).
- AC2.4: A unit test calls `FindComposites` with `opts.Refs != nil` and gets ref-based lookup behavior (matches the old `GetCompositesByName` semantics, minus the `unmatched` return).

### AC for R3 (internal phase split)

- AC3.1: `composition_client.go` defines a `compositeTypes` struct with `xrGVK` and `claimGVK schema.GroupVersionKind`.
- AC3.2: `composition_client.go` defines private methods `resolveCompositeTypes(ctx, comp) (compositeTypes, error)` and `lookupRef(ctx, ref, types, compName) (*Unstructured, error)` on `*DefaultCompositionClient`.
- AC3.3: `findByRefs` body is concise (no inline GVK resolution, no inline per-ref claim-fallback logic) — those phases are delegated.
- AC3.4: A unit test exercises `lookupRef` directly with mocked resource client (XR-found-and-matches, XR-not-found-claim-found-and-matches, both-not-found-returns-nil, XR-found-but-wrong-composition-returns-nil, cluster-error-propagates).

### AC for R4 (processSingleComposition takes pre-resolved XRs)

- AC4.1: `processSingleComposition` signature is `(ctx, newComp *Unstructured, affectedXRs []*Unstructured, surfaceFiltered bool) (*CompositionDiff, error)`.
- AC4.2: `processSingleComposition` body contains no `FindComposites*` or discovery calls.
- AC4.3: A unit test calls `processSingleComposition` directly with `surfaceFiltered=true` and verifies that Manual-policy XRs in `affectedXRs` are surfaced as `XRStatusFilteredByPolicy` impacts.
- AC4.4: A unit test calls `processSingleComposition` directly with `surfaceFiltered=false` and verifies that Manual-policy XRs are *not* surfaced as impacts (only counted in summary).

### AC for R5 (DiffComposition orchestration with switch)

- AC5.1: `DiffComposition`'s per-composition loop uses `switch { case ...: ...; default: ...; }` for the resource-mode-vs-default branch (no `else`).
- AC5.2: The find-error branch within default uses `switch { case findErr != nil: ...; default: ... }` (no `else`).
- AC5.3: The graceful net-new behavior is preserved: when `FindComposites` returns an error, `affectedXRs = nil` and processing continues, producing a result with empty `ImpactAnalysis`.
- AC5.4: An integration test (existing in `diff_integration_test.go`) for net-new composition default-discovery still passes without modification.

### AC for R6 (mocks migrated)

- AC6.1: `MockCompositionClient` struct has `FindCompositesFn` field; `FindCompositesUsingCompositionFn` and `GetCompositesByNameFn` fields are absent.
- AC6.2: `MockCompositionClientBuilder` has `WithFindComposites(fn)` method; `WithFindCompositesUsingComposition` and `WithGetCompositesByName` are absent.
- AC6.3: Convenience wrappers `WithResourcesForComposition` and `WithFindResourcesError` are still present and behave identically to before, but their internal implementation routes through `FindCompositesFn`.
- AC6.4: All tests in `comp_processor_test.go` and `composition_client_test.go` compile and pass with the new mock API.

### AC for R7 (lint clean)

- AC7.1: `//nolint:interfacebloat` directive in `composition_client.go` is gone.
- AC7.2: `golangci-lint run ./cmd/diff/...` reports zero `interfacebloat` issues.
- AC7.3: `golangci-lint run ./cmd/diff/...` reports no NEW `revive: package types` issues. The two pre-existing `revive: package types` issues at `cmd/diff/types/types.go:18` and `cmd/diff/renderer/types/types.go:2` remain — they are out of scope for this refactor (fixing them requires a package rename touching every import site).

### AC overall

- AC-OVR-1: `earthly -P +reviewable` passes (unit tests + lint + generation).
- AC-OVR-2: `earthly -P +e2e --CROSSPLANE_IMAGE_TAG=main --FLAGS="-test.run TestCompositionDiff"` passes (composition diff E2E — smoke that user-visible behavior is unchanged).
- AC-OVR-3: User-visible behavior verified by spot check on a real cluster: default discovery, `--resource=ns/name`, `--resource=does/not-exist` (preflight error), bare-name cluster-scoped ref.

## Testing Plan

This refactor is heavily test-supported. Almost all behavior is already covered by the test suite from PR #322; we adapt those tests rather than write new ones from scratch. New tests are added to validate the smaller helpers introduced in R3.

### Layer 1 — Compile-driven (shape changes)

Tests that fail at compile time when the type/signature is wrong. These guide the structural changes:

- **TC-R1.1**: Existing `TestParseResourceRef` in `comp_test.go` — change expected return type from `dtypes.ResourceRef` to `k8stypes.NamespacedName`. No assertion changes (string outputs identical).
- **TC-R6.1**: Existing mock setups in `comp_processor_test.go` — migrate to `WithFindComposites`. Compile failure if the new method doesn't exist or has wrong signature.

### Layer 2 — Direct unit tests (new, for new helpers)

- **TC-R3.1**: `TestResolveCompositeTypes` (new) — table-driven tests in `composition_client_test.go`:
  - case: composition with valid `compositeTypeRef` and matching XRD → returns both XR GVK and claim GVK
  - case: composition with valid `compositeTypeRef` but XRD lookup fails → returns XR GVK, empty claim GVK, no error
  - case: composition with valid `compositeTypeRef` but XRD has no `claimNames` → returns XR GVK, empty claim GVK, no error
  - case: composition with malformed `compositeTypeRef.apiVersion` → returns error
- **TC-R3.2**: `TestLookupRef` (new) — table-driven tests in `composition_client_test.go`:
  - case: ref matches via XR GVK and uses target composition → returns object
  - case: ref matches via XR GVK but uses different composition → returns nil, no error (preserves current bug for follow-up F1)
  - case: ref XR-404, then matches via claim GVK and uses target composition → returns object
  - case: ref XR-404, claim GVK 404 → returns nil, no error
  - case: ref XR-404 and claim GVK is empty (XRD missing) → returns nil, no error
  - case: ref XR returns non-404 cluster error → propagates error
- **TC-R4.3, TC-R4.4**: New tests in `comp_processor_test.go` for `processSingleComposition` directly with `surfaceFiltered=true/false` and Manual-policy XRs.

### Layer 3 — Adapted unit tests (existing, signatures change)

- **TC-R2.3, TC-R2.4**: Existing tests for `GetCompositesByName` in `composition_client_test.go` (named `TestGetCompositesByName`) → migrate to `TestFindComposites_WithRefs` (call the new method with `opts.Refs` populated). Existing tests for `FindCompositesUsingComposition` → migrate to `TestFindComposites_DefaultDiscovery` (call with `opts.Refs == nil`).
- **TC-R6.4**: Other tests in `comp_processor_test.go` that hit the affected code paths (preflight, default discovery in `DiffComposition`) → re-check after migration.

### Layer 4 — Integration tests (existing, minimal changes)

- **TC-R5.4**: `TestDiffCompositionWithGetComposedResource` and similar in `diff_integration_test.go` should pass without modification since they exercise CLI behavior, not internal Go signatures.

### Layer 5 — Lint signal

- **TC-R7.2, TC-R7.3**: `earthly -P +reviewable` passes cleanly (no new lint issues).

### TDD ordering principle

For each implementation step in the plan below, the test changes go first, then the implementation. The test should fail initially (red), then pass after the implementation step (green). Where an existing test is adapted, the adaptation IS the failing test — we change expectations, then refactor production code to satisfy them.

## Implementation Plan

The change is one logical refactor but is implemented in 5 small steps, each independently testable. The user has stated they will commit themselves; we land everything in one staged working tree at the end.

### Step 1 — Type swap: `ResourceRef` → `NamespacedName` + add `formatRef`

**Goal:** All `dtypes.ResourceRef` references replaced by `k8stypes.NamespacedName`. `ResourceRef` (and only `ResourceRef`) removed from `cmd/diff/types/types.go`. New `formatRef` helper preserves the bare-name rendering for cluster-scoped refs in user-facing strings. No other behavioral changes.

**Test-first:**
- TC-R1.1: Update `comp_test.go` `TestParseResourceRef` — change `wantRef` field type and expected values from `types.ResourceRef{...}` to `k8stypes.NamespacedName{...}`. Add at least one assertion exercising `formatRef(got) == tt.wantString` to lock in the rendering contract (cluster-scoped: bare `name`; namespaced: `ns/name`).
- TC-R1.2 (new): Add `TestFormatRef` table-driven test in `comp_test.go` — covers `{Name: "foo"}` → `"foo"`, `{Namespace: "default", Name: "foo"}` → `"default/foo"`, and the empty edge case `{}` → `""`.
- Run `go test ./cmd/diff/...` — should fail at compile time on production code that still uses `dtypes.ResourceRef`.

**Implementation:**
- Add `formatRef(n k8stypes.NamespacedName) string` to `cmd/diff/comp.go` (uses a `switch` block).
- Add `import k8stypes "k8s.io/apimachinery/pkg/types"` everywhere `cmd/diff/types` was imported as `dtypes` (or where `types.ResourceRef` was used).
- Mechanical rename: `dtypes.ResourceRef` → `k8stypes.NamespacedName` across all production and test files.
- Update `parseResourceRef` return type and constructors.
- In `cmd/diff/types/types.go`: delete the `ResourceRef` struct and its `String()` method. Keep `CompositionProvider`. Update the package doc comment if it mentions both.
- Remove the import alias `dtypes "github.com/crossplane-contrib/crossplane-diff/cmd/diff/types"` from files that no longer need anything from it (some files may keep importing the package as `types` for `CompositionProvider`).
- Replace user-facing `ref.String()` calls with `formatRef(ref)`. Specifically: the error-message construction in `preflightResourceRefs` at `comp_processor.go:306-311` (the loop that builds `names` for the `--resource ref(s) not relevant` error). Internal-only `ref.String()` calls used as map keys (e.g., `matchedAtLeastOnce[ref.String()]` at line 291, 300) can stay — those just need stable string keys, and `NamespacedName.String()` works fine for that purpose.

**Verify:** `go test ./cmd/diff/...` passes. AC1.1, AC1.2, AC1.3, AC1.4, AC1.5.

### Step 2 — Introduce `FindComposites` and `FindCompositesOptions`

**Goal:** New unified method available; old methods still present (added, not yet swapped). Preserves behavior of both old methods.

**Test-first:**
- Add a new test `TestFindComposites_DefaultDiscovery` in `composition_client_test.go` that calls `FindComposites(ctx, comp, FindCompositesOptions{Namespace: ns})` and asserts the same behavior as existing `TestFindCompositesUsingComposition`.
- Add a new test `TestFindComposites_WithRefs` that calls `FindComposites(ctx, comp, FindCompositesOptions{Refs: refs})` and asserts the same behavior as existing `TestGetCompositesByName` (matched-only return).

**Implementation:**
- Add `FindCompositesOptions` struct to `composition_client.go`.
- Add `FindComposites(ctx, comp, opts)` method to the `CompositionClient` interface AND to `DefaultCompositionClient`. Body branches via `switch` on `len(opts.Refs)`:
  - `case len(opts.Refs) > 0:` → call new private `findByRefs` (extract from existing `GetCompositesByName` body, drop `unmatched` return)
  - `default:` → call new private `findByListing` (extract from existing `FindCompositesUsingComposition` body)
- Add `FindCompositesFn` to `MockCompositionClient`.
- Add `WithFindComposites` to `MockCompositionClientBuilder`.

**Verify:** New tests pass. Old tests still pass. AC2.1 (partial — interface still has 7 methods at this stage), AC2.2, AC2.3, AC2.4.

### Step 3 — Migrate callers and remove old methods

**Goal:** Production callers (`preflightResourceRefs`, `processSingleComposition`) now call `FindComposites`. Old interface methods removed.

**Test-first:**
- Migrate `comp_processor_test.go` mock setups: `WithFindCompositesUsingComposition(fn)` → `WithFindComposites(fn)` (with appropriate `opts.Refs == nil` check inside). `WithGetCompositesByName(fn)` → `WithFindComposites(fn)` (with `opts.Refs != nil` check).
- The convenience wrappers `WithResourcesForComposition` and `WithFindResourcesError` get re-routed internally to set `FindCompositesFn` (with `len(opts.Refs) == 0` check); their public API stays the same so test sites that use them don't change.
- Existing tests for `TestFindCompositesUsingComposition` and `TestGetCompositesByName` get renamed to `TestFindComposites_DefaultDiscovery` and `TestFindComposites_WithRefs` respectively. The new tests added in Step 2 either replace these or merge with them.
- Run `go test ./cmd/diff/...` — production code should fail compilation.

**Implementation:**
- In `comp_processor.go`: change `preflightResourceRefs` to call `p.compositionClient.FindComposites(ctx, typedComp, FindCompositesOptions{Refs: refs})`, capturing only `(matched, err)`.
- In `comp_processor.go`: change `processSingleComposition`'s discovery branch to call `p.compositionClient.FindComposites(ctx, typedComp, FindCompositesOptions{Namespace: namespace})`. (We'll move this branch up to `DiffComposition` in Step 5; for now, just swap the call.)
- Remove `FindCompositesUsingComposition` and `GetCompositesByName` from the `CompositionClient` interface and from `DefaultCompositionClient`.
- Remove `FindCompositesUsingCompositionFn` and `GetCompositesByNameFn` fields from `MockCompositionClient`.
- Remove `WithFindCompositesUsingComposition` and `WithGetCompositesByName` from `MockCompositionClientBuilder`.
- Remove `//nolint:interfacebloat` directive from `composition_client.go:23`.

**Verify:** `go test ./cmd/diff/...` passes. `golangci-lint run ./cmd/diff/...` shows no `interfacebloat` issue. AC2.1 (now satisfied), AC6.1, AC6.2, AC6.3, AC6.4, AC7.1, AC7.2.

### Step 4 — Split `findByRefs` into phases

**Goal:** `findByRefs` body is short; `resolveCompositeTypes` and `lookupRef` are private helpers with their own tests.

**Test-first:**
- Add `TestResolveCompositeTypes` (table-driven, 4 cases per testing plan TC-R3.1).
- Add `TestLookupRef` (table-driven, 6 cases per testing plan TC-R3.2). Use `MockResourceClient` and `MockDefinitionClient` from existing testutils.

**Implementation:**
- Extract `resolveCompositeTypes(ctx, comp) (compositeTypes, error)` from current `findByRefs`. Move XR GVK parsing + XRD lookup + claim GVK extraction here.
- Extract `lookupRef(ctx, ref, types, compName) (*Unstructured, error)` from current `findByRefs`. Move per-ref XR-then-claim lookup logic here.
- Rewrite `findByRefs` body to: call `resolveCompositeTypes`, loop over refs calling `lookupRef`, accumulate non-nil results.

**Verify:** New tests pass. Existing `TestFindComposites_WithRefs` still passes. AC3.1, AC3.2, AC3.3, AC3.4.

### Step 5 — Hoist discovery into `DiffComposition`, drop `resourceMode`

**Goal:** `processSingleComposition` takes pre-resolved `affectedXRs` and a `surfaceFiltered` bool. `DiffComposition` decides the source via `switch` blocks.

**Test-first:**
- Existing tests in `comp_processor_test.go` that call `processSingleComposition` directly (lines 908, 941 per earlier exploration) need their argument lists updated to the new signature. The two existing test cases already exercise both modes (`(nil, false)` for discovery, `([]Unstructured{manualXR}, true)` for resource).
- Adapt these to call the new signature directly with pre-resolved XRs. For the discovery-mode test, the discovery is no longer in `processSingleComposition` — so the equivalent test becomes "call `DiffComposition` with no `--resource` and observe the same end result," OR we rewrite to test `processSingleComposition` with explicitly pre-resolved XRs.
- New direct tests **TC-R4.3, TC-R4.4** exercise the `surfaceFiltered=true/false` semantics independently of any discovery.

**Implementation:**
- Change `processSingleComposition` signature to `(ctx, newComp, affectedXRs, surfaceFiltered)`.
- Inside `processSingleComposition`: replace the discovery branch with direct use of `affectedXRs`. Remove the `if resourceMode { ... } else { discover... }` block. Rename `if resourceMode` (around line 383) → `if surfaceFiltered`. Body of that block unchanged.
- In `DiffComposition`'s per-composition loop, add the resolution `switch`:
  ```go
  var (
      affectedXRs     []*un.Unstructured
      surfaceFiltered bool
  )
  switch {
  case len(resources) > 0:
      affectedXRs = preflightMatches[comp.GetName()]
      surfaceFiltered = true
  default:
      typedComp := &apiextensionsv1.Composition{}
      if err := runtime.DefaultUnstructuredConverter.FromUnstructured(comp.Object, typedComp); err != nil {
          // existing error handling
      }
      discovered, findErr := p.compositionClient.FindComposites(ctx, typedComp, FindCompositesOptions{Namespace: namespace})
      switch {
      case findErr != nil:
          p.config.Logger.Debug("Cannot find composites using composition (likely net-new)",
              "composition", comp.GetName(), "error", findErr)
          affectedXRs = nil
      default:
          affectedXRs = discovered
      }
      surfaceFiltered = false
  }
  ```
- Update the call: `compResult, err := p.processSingleComposition(ctx, comp, affectedXRs, surfaceFiltered)`.
- Update `comp_processor_test.go`'s direct `processSingleComposition` calls to match the new signature.

**Verify:** All unit tests pass. AC4.1, AC4.2, AC4.3, AC4.4, AC5.1, AC5.2, AC5.3, AC5.4.

### Final verification

After all 5 steps:
- `cd cmd/diff && go test ./...` — all unit tests pass (fast feedback).
- `earthly -P +reviewable` — full lint + test + generation passes (AC-OVR-1, AC7.2, AC7.3).
- `earthly -P +e2e --CROSSPLANE_IMAGE_TAG=main --FLAGS="-test.run TestCompositionDiff"` — comp diff E2E passes (AC-OVR-2).
- Spot-check binary against a real cluster (AC-OVR-3) — the user can do this themselves before committing.

### Code review checkpoints

After each step, invoke the `superpowers:code-reviewer` subagent (in lieu of Lad MCP, which is not available in this environment) to review the diff for:
- Adherence to CLAUDE.md (machine-readable error handling, accuracy-above-all)
- Adherence to project style (switch over else, table-driven tests, etc.)
- Catch any CLAUDE.md-mandated patterns I missed

Apply review feedback before moving to the next step.
