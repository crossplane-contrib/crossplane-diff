# REQUIREMENTS: Group `xr` structured output by input XR (#405)

This file is the single source of truth for the design and plan. Architecture
changes are reflected inline in `design/design-doc-cli-diff.md` (§6.8) and the
`diff-rendering-architecture.{mermaid,svg}` diagram.

## Design Decisions

1. **Back-compat: additive, always-present `xrs[]`, with documented deprecation
   of flat `changes[]`.** `xrs[]` is always emitted alongside the existing flat
   `summary`/`changes[]`/`errors[]`. `changes[]` is marked deprecated (Go doc
   comment + README + design doc) in favor of `xrs[]`; the aggregate `summary`
   and top-level `errors[]` union are retained. Removal of flat `changes[]` is
   deferred to a future major (`feat(xr)!:`). Rejected: a `--group-by-xr` flag
   (permanent dual path, can't express deprecation) and an immediate breaking
   reshape (breaks live consumers on the ship date). Deprecation signal is
   docs + Go doc comment only — no runtime stderr warning.

2. **Data flow: widen the `DiffRenderer` interface to carry per-XR groups.**
   `RenderDiffs([]dt.XRDiffGroup, []dt.OutputError)`. `PerformDiff` builds one
   group per input in input order (identity + diffs on success, or a
   pre-converted `*dt.OutputError` on failure) instead of merging into a flat
   map. `XRDiffGroup` lives in `renderer/types` (not `renderer`) so `testutils`
   can reference it without an import cycle through `renderer`'s in-package
   tests; the pre-converted error keeps `NewOutputError` (in `diffprocessor`)
   out of the renderer. This mirrors how `comp` already builds renderer-package
   types (`XRImpact`) in the processor. Application(diffprocessor)→Domain(renderer)
   is the sanctioned dependency direction.

3. **Human output also groups by input XR — but only for >1 XR.** Multiple input
   XRs render as per-XR sections (`=== Kind/name ===` + diffs + per-section
   summary, `No changes.` for unchanged, inline `Error:` for failed) plus an
   aggregate `Total: … across N XRs (…)` footer. A **single** XR renders flat,
   exactly as before this change — the common case is byte-for-byte unchanged,
   so existing single-XR E2E `.ansi` goldens do not churn. The seam:
   identity-bearing groups (set only by the `xr` command) get sections;
   identity-less groups (the composition renderer's internal reuse) render flat
   with no header, so `comp` output is unaffected.

4. **`status` reuses the existing `XRStatus` constants** (`changed`/`unchanged`/
   `error`); `filtered` does not apply to `xr`. Derivation: `error` if the group
   carries an error; else `changed` if it has ≥1 non-equal diff; else
   `unchanged`. Equal diffs are excluded from `changes[]` and counts (the XR
   stored as `DiffTypeEqual` for removal detection must not inflate counts).

5. **Do not consolidate the `comp` and `xr` per-XR paths now.** Both carry a
   per-XR bucket (`XRImpact` / `XRDiffGroup`) but diverge in four load-bearing
   ways: `comp`'s diffs are downstream-only (XR is the axis, not a change);
   `comp` has filter concepts meaningless to `xr`; `comp`'s wire error is a
   single string vs. `xr`'s richer `errors[]`/`validationFailures[]`; `comp` is
   two-level vs. `xr`'s one-level. Reuse the shared building blocks (`Summary`,
   `ChangeDetail`, `XRStatus`, the `renderDiffList` helper); defer full wire-shape
   unification to the next breaking `comp` window (bundled with comp's
   error-model upgrade and the flat-`changes[]` removal), when a breaking `comp`
   change is already on the table. See Future Work.

Correction of record: issue #405's text claims the human output "already renders
per-file/per-XR sections." It does not — before this change the human renderer
emitted a single flat sorted list with one summary.

## As Is

- `crossplane-diff xr` given N input XRs/claims diffs each in its own iteration of
  `DefaultDiffProcessor.PerformDiff` (`cmd/diff/diffprocessor/diff_processor.go`
  ~L204-273). Each returns its own `map[string]*dt.ResourceDiff`.
- Those maps are merged into a flat `allDiffs` via `maps.Copy` (L239), discarding
  the per-XR association. Per-input errors are collected into `outputErrors` via
  `NewOutputError(resourceID, err)` (L236).
- `PerformDiff` calls `p.diffRenderer.RenderDiffs(allDiffs, outputErrors)` (L245).
- `DiffRenderer` interface (`renderer/diff_renderer.go` L17-22):
  `RenderDiffs(diffs map[string]*dt.ResourceDiff, errs []dt.OutputError) error`.
- Two impls: `DefaultDiffRenderer` (human, `diff_renderer.go`) emits a single flat
  sorted list + one `Summary:` line; `StructuredDiffRenderer` (`structured_renderer.go`)
  emits flat `StructuredDiffOutput{Summary, Changes[], Errors[]}`.
- One mock: `MockDiffRenderer.RenderDiffs` (`testutils/mocks.go` L832).
- Two internal reuse sites in comp: `comp_diff_renderer.go` L142 and L271 call the
  human `RenderDiffs` with a flat map to render composition/downstream diff blocks.
- The `comp` command already groups per-XR via `CompDiffOutput → []CompositionDiff
  → []XRImpact` (types in `structured_renderer.go`), with a two-level JSON shape
  (`impactAnalysis[]` → `downstreamChanges`).

## To Be

- `xr` structured output (`-o json`/`-o yaml`) gains a top-level `xrs[]` array,
  always present, one entry per input XR/claim in input order, each with its own
  `xr` identity, `status`, `summary`, `changes[]`, and `errors[]`.
- The existing flat `summary` (aggregate) + `changes[]` + top-level `errors[]`
  (union) are preserved. `changes[]` is marked deprecated (Go doc comment + README
  + design doc); `summary` and `errors[]` are not deprecated.
- `xr` human output groups by input XR: a `=== Kind/name ===` section header per
  input XR, its diffs, a per-section summary; an aggregate footer when >1 input XR.
  Errored XRs get an inline `Error: ...` stdout section (errors still also go to
  stderr, unchanged).
- `comp` output (human and structured) is unchanged.
- `DiffRenderer.RenderDiffs` takes `[]XRDiffGroup` instead of a flat map. Comp's two
  reuse sites pass a single identity-less group; the human renderer renders
  identity-less groups flat (no header), preserving comp's output.

## Requirements

**R1.** Introduce `XRDiffGroup` in the `renderer` package:
`struct { XR corev1.ObjectReference; Diffs map[string]*dt.ResourceDiff; Err *dt.OutputError }`.

**R2.** Widen `DiffRenderer.RenderDiffs` to
`RenderDiffs(groups []XRDiffGroup, errs []dt.OutputError) error`. Update both impls
and the mock.

**R3.** `DefaultDiffProcessor.PerformDiff` builds `[]renderer.XRDiffGroup` — one per
input resource, in input order — capturing the input XR's `ObjectReference`
(apiVersion, kind, name, namespace), its diffs map on success, or its
`*dt.OutputError` on failure. It passes `groups` and the existing `outputErrors`
union to `RenderDiffs`. `hasDiffs` semantics unchanged.

**R4.** `StructuredDiffRenderer` emits, from `groups`:
- the deprecated flat `Changes[]` + aggregate `Summary` (merged across all groups,
  equivalent to today's flattened output);
- top-level `Errors[]` = the `errs` union param (unchanged);
- a new `Xrs []xrGroupJSON`, one per group in order, each with `xr` identity,
  `status`, per-group `summary`, per-group `changes[]`, and per-group `errors[]`.

**R5.** Per-group `status` derivation: `error` if `group.Err != nil`; else `changed`
if the group has ≥1 non-equal diff; else `unchanged`. Reuse the existing `XRStatus`
constants. Equal diffs are excluded from `changes[]` and from counts.

**R6.** `DefaultDiffRenderer.RenderDiffs` renders each identity-bearing group as a
section (header `=== <Kind>/<Name> ===`, its diffs via the extracted
`renderDiffList` helper, a per-section summary; `No changes.` when empty; inline
`Error: ...` when `Err != nil`), followed by an aggregate footer when >1
identity-bearing group. Identity-less groups (comp reuse) render flat with no
header/section-summary/footer — byte-for-byte as today. Sections in input order;
diffs within a section kind/name-sorted as today. Errors still written to stderr.

**R7.** Comp's two reuse sites (`comp_diff_renderer.go` L142, L271) wrap their flat
map in `[]renderer.XRDiffGroup{{Diffs: m}}` (identity-less). Comp output unchanged.

**R8.** Structured-assertion harness (`testutils/structured_assertions.go`) gains a
grouped-view builder (`WithXR(...)`, `WithXRStatus`, `WithXRSummary`, `WithXRChange`,
`AndXR`) reusing existing `WithField*`/`WithAnyName`/`WithNamePattern`; parse target
gains `Xrs`.

**R9.** Docs: Go doc deprecation comment on `StructuredDiffOutput.Changes`; doc on
`XRDiffGroup` + new wire types; README structured-output section (`xrs[]` +
deprecation note + human per-XR sections); design doc §6.8.1 (interface), §6.8.3
(output types), correct the "human already groups" misstatement; regenerate
`diff-rendering-architecture.svg`.

## Acceptance Criteria

- **R1/R2:** Code compiles; `DiffRenderer` has the new signature; `grep` finds no
  remaining `RenderDiffs(<map>` production call.
- **R3:** Unit test on `PerformDiff` (via mock renderer capturing `groups`): N inputs
  → N groups in input order; each group's `XR` matches its input's GVK+name+namespace;
  success group has its diffs and nil `Err`; errored group has nil/empty `Diffs` and
  non-nil `Err` whose `ResourceID` == `<Kind>/<Name>`.
- **R4:** Structured renderer unit test: multi-group input yields flat `changes[]`
  equal to the merge of all groups' non-equal diffs AND `xrs[]` with matching
  per-group `changes[]`; aggregate `summary` == sum of per-group summaries;
  top-level `errors[]` == union.
- **R5:** unchanged group → `status:"unchanged"`, empty `changes`, zero summary;
  errored group → `status:"error"`, entry `errors[]` populated; a group whose only
  diff is `DiffTypeEqual` → `unchanged` and contributes 0 to counts.
- **R6:** Human renderer unit test: 2 identity-bearing groups → two `===` headers in
  input order, two per-section summaries, one aggregate footer; a single
  identity-bearing group → one header, no footer; an identity-less group → no header,
  no footer, output identical to the pre-change flat rendering (golden string compare).
  Errored identity-bearing group → inline `Error:` line on stdout + error on stderr.
- **R7:** Existing comp renderer tests pass unchanged (regression guard).
- **R8:** New grouped assertions compile and pass against real structured output in
  the integration test.
- **R9:** `earthly -P +reviewable` passes (lint + generation clean); README/design
  doc updated; `.svg` regenerated.

## Testing Plan (TDD order)

Unit tests are written before the corresponding implementation and must fail first.

1. **T1 (R1/R2 scaffolding):** compile-level — add `XRDiffGroup`, change signature,
   update mock. Existing renderer tests updated to pass a single group. (Red: build
   fails until signature+impls updated.)
2. **T2 (R6 identity-less = flat):** `diff_renderer_test.go` — assert an identity-less
   group renders byte-identical to today's expected strings (reuse existing cases,
   wrapped). Red until human renderer handles groups.
3. **T3 (R6 grouped human):** new cases — 2 identity-bearing groups → headers +
   per-section summaries + aggregate footer; single group → header, no footer;
   errored group → inline `Error:`. Red until sectioning implemented.
4. **T4 (R4/R5 structured):** `structured_renderer_test.go` — multi-group → flat
   back-compat fields + `xrs[]`; unchanged group; errored group; equal-only group;
   empty input. Red until structured renderer builds `xrs[]`.
5. **T5 (R3 processor):** `diff_processor_test.go` — mock renderer captures `groups`;
   assert one-per-input, input order, identity, success/error split. Red until
   `PerformDiff` builds groups.
6. **T6 (R7 comp regression):** run existing `comp_diff_renderer_test.go` — must stay
   green after the reuse sites are updated.
7. **T7 (R8 harness):** unit-test the new grouped assertion builder against a
   hand-built structured output.
8. **T8 (R3/R4/R5 integration):** `diff_integration_test.go` — a multi-XR invocation
   on real envtest+docker; assert grouped JSON via the new harness (one XR changed,
   one unchanged).
9. **T9 (E2E goldens):** regenerate affected `xr` `.ansi` via `E2E_DUMP_EXPECTED=1`;
   review each `git diff` per the gate (only new headers/summaries/footer; no mangled
   ANSI/body/color/order changes) before staging.

## Implementation Plan (smallest sequential steps)

Each step: write/adjust the test first (Red), implement (Green), run the smallest
relevant `go test`.

- **S1 — Type + signature (T1).** Add `XRDiffGroup` to `renderer` (with doc comment).
  Change `DiffRenderer.RenderDiffs` signature. Update `MockDiffRenderer`. Update
  `DefaultDiffRenderer` and `StructuredDiffRenderer` signatures with a *temporary*
  flatten-then-existing-logic body so the build is green. Update existing renderer
  tests + comp reuse sites (S adds the `{{Diffs: m}}` wrap) minimally to compile.
  Test: `go test ./cmd/diff/renderer/... ./cmd/diff/diffprocessor/...` build+pass.

- **S2 — Human: extract `renderDiffList` + identity-less path (T2).** Refactor the
  existing per-diff loop into `renderDiffList(w, diffs, opts)`. `RenderDiffs` loops
  groups; identity-less group → `renderDiffList` only (no header/footer). Assert
  existing golden strings unchanged.
  Test: `go test ./cmd/diff/renderer/ -run RenderDiffs`.

- **S3 — Human: grouped sections + footer (T3).** For identity-bearing groups add
  header, per-section summary, `No changes.`, inline `Error:`, and aggregate footer
  (>1 group). Add new table cases.
  Test: `go test ./cmd/diff/renderer/ -run RenderDiffs`.

- **S4 — Structured: build `xrs[]` (T4).** Add `xrGroupJSON` wire type + `Xrs` field
  on `StructuredDiffOutput`; build flat (merged) + `xrs[]` from groups; status
  derivation (S5 logic). Deprecation doc comment on `Changes`.
  Test: `go test ./cmd/diff/renderer/ -run Structured`.

- **S5 — Processor: build groups (T5).** `PerformDiff` builds `[]XRDiffGroup` in the
  loop (identity from `res`; diffs on success; `&outErr` on failure) and passes them.
  Keep `outputErrors` union. 
  Test: `go test ./cmd/diff/diffprocessor/ -run PerformDiff`.

- **S6 — Comp regression (T6).** Confirm comp reuse sites already wrapped in S1 keep
  comp output identical.
  Test: `go test ./cmd/diff/renderer/ -run Comp`.

- **S7 — Harness (T7).** Add grouped builder methods + `Xrs` parse target + matchers.
  Test: `go test ./cmd/diff/testutils/...` (or the renderer test that exercises it).

- **S8 — Integration (T8).** Add a multi-XR grouped-JSON integration case.
  Test: the integration test target (envtest + docker).

- **S9 — Docs + goldens (T9/R9).** README, design doc §6.8, `.svg` regen; regenerate
  and human-review `.ansi` goldens per the gate.
  Test: `earthly -P +reviewable` (bare, no pipe — exit code must not be masked).

## Future Work

- **Unify the per-XR structured shape across `comp` and `xr`.** At the next
  breaking `comp` window — bundled with (a) upgrading `comp`'s wire error from
  `string` to the richer `OutputError`/`validationFailures[]` model `xr` uses,
  and (b) removing the deprecated flat `xr` `changes[]` — harmonize `XRImpact`
  and `XRDiffJSON` into one per-XR entry shape so CI wrappers parse both
  commands identically. Free to do then (a breaking `comp` change is already on
  the table); a separate breaking event now is not warranted. See Decision 5.

## Notes / constraints

- **Import direction:** `XRDiffGroup` lives in `renderer/types` (the leaf `dt`
  package), referenced by both `renderer` (domain) and `diffprocessor`
  (application). Placing it there (rather than in `renderer` alongside the
  `DiffRenderer` interface) lets `testutils` mock the interface without importing
  `renderer`, which would close an import cycle through `renderer`'s in-package
  tests. `Err` is a pre-converted `*dt.OutputError` (via `NewOutputError` in
  `diffprocessor`) to keep the conversion out of the renderer.
- **No `tee`/`tail`/`grep` on gate commands** (`earthly +reviewable`/`+go-test`) when
  the exit code matters — run bare (per project convention).
- **Consolidation with `comp` is deferred** (Decision 5 / Future Work) — do not
  refactor comp's `XRImpact` in this change; only reuse `Summary`/`ChangeDetail`/
  `XRStatus`/`renderDiffList`.
