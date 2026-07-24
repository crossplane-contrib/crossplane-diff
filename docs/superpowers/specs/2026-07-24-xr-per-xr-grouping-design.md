# Design: Group `xr` structured output by input XR

- **Issue:** [crossplane-diff#405](https://github.com/crossplane-contrib/crossplane-diff/issues/405)
- **Date:** 2026-07-24
- **Status:** Approved (pending spec review)

## Problem

`crossplane-diff xr` given multiple XRs/claims in one invocation emits a single
**flat** `StructuredDiffOutput` (`-o json` / `-o yaml`):

```jsonc
{ "summary": { "added": N, "modified": N, "removed": N },
  "changes": [ { type, apiVersion, kind, name, namespace, diff }, ... ],
  "errors":  [ { resourceID, message, validationFailures? }, ... ] }
```

`changes[]` is a flat list of composed/managed resources with **no link back to
the input XR that produced each one**. Consumers that want per-XR output (e.g.
the Sanyaku CI runner, which posts a PR comment with one collapsible section per
XR) are forced to invoke `xr` once per resource. That serial per-resource
invocation is the dominant cost: each process spins up and tears down the
render container plus every function container, with no reuse across
invocations (container names embed a per-process random instanceID, and each
process runs `Cleanup` on exit). A ~36-resource PR pays ~36× full Docker
spin-up/teardown.

Invoking `xr` **once per file** would let the cached function provider reuse
containers across all XRs in that call — a large wall-clock win — but today the
flat output loses the per-XR grouping the wrapper needs. The tool already knows
the XR→composed-resource mapping authoritatively (it renders each XR and tracks
its composed tree); it is discarded at aggregation time.

The `comp` command already solves this shape: its structured output is XR-grouped
(`impactAnalysis[]` → per-XR `downstreamChanges`). The `xr` command has no
equivalent nesting. This design adds it.

### Root cause (where the grouping is lost)

In `DefaultDiffProcessor.PerformDiff` (`cmd/diff/diffprocessor/diff_processor.go`),
each input XR is diffed in its own loop iteration and returns its own
`map[string]*dt.ResourceDiff`. Those maps are merged into one flat `allDiffs`
via `maps.Copy` before `RenderDiffs` is called — discarding the per-XR
association. Errors are already tagged per-input via `resourceID`. The grouping
is therefore available exactly at the merge point and simply flattened away.

## Decisions

1. **Back-compat strategy: additive, always present, with documented
   deprecation of the flat view.** Add an `xrs[]` array alongside the existing
   flat `summary` / `changes[]` / `errors[]`, always populated (no flag). Mark
   the flat `changes[]` field **deprecated** (README, design doc, Go doc
   comment) pointing consumers to `xrs[]`. The aggregate `summary` and
   top-level `errors[]` are **not** deprecated. Removal of flat `changes[]` is
   deferred to a future major release (`feat(xr)!:`), decoupling the
   replacement's ship date from consumers' migration schedules. A `--group-by-xr`
   flag was rejected: it creates a permanent second code path and cannot express
   deprecation. An immediate breaking reshape was rejected: it would break live
   consumers on the same day the replacement ships.

2. **Deprecation signal depth: docs + Go doc comment only.** No runtime stderr
   warning.

3. **Data flow: widen the `DiffRenderer` interface to carry per-XR groups.**
   `PerformDiff` builds `[]XRDiffGroup` and passes it to `RenderDiffs`. The human
   renderer consumes the groups (per-XR sections); the structured renderer emits
   both the deprecated flat view (merged) and the new `xrs[]` (per group). This
   was chosen over an optional second interface (`GroupedDiffRenderer` +
   type-assert), which would leave a permanent dual path and keep the flat
   interface first-class after the deprecation window. It mirrors the precedent
   `comp` established: the application-layer processor builds a rich grouped
   representation (`CompDiffOutput`/`XRImpact`, both defined in `renderer`) and
   the domain-layer renderer formats it. `XRDiffGroup` is the `xr`-side analog of
   `XRImpact`.

4. **Human output groups by input XR too.** Rather than flatten the groups back
   for the human renderer, the human output gains per-input-XR sections (header,
   diffs, per-section summary) plus an aggregate footer. Strict byte-for-byte
   back-compat of the human output is **not** a requirement (contract consumers
   use the machine formats); output ergonomics win.

5. **Do not consolidate the `comp` and `xr` per-XR paths in this change; reuse
   shared building blocks and defer full unification.** Both commands now carry a
   per-XR bucket of `{identity, status, diffs, error}` (`comp`'s `XRImpact`,
   `xr`'s `XRDiffGroup`), but they diverge in four load-bearing ways: (a)
   `comp`'s `Diffs` are downstream/composed resources only — the XR is the axis,
   not a change — whereas `xr`'s diffs include the input XR itself as a change;
   (b) `comp` has filter concepts (`FilterReason`/`FilterDetail`) meaningless to
   `xr`; (c) `comp`'s wire error is a single `string`, `xr`'s is the richer
   `errors[]` with `validationFailures[]`; (d) `comp` is two-level (composition →
   XRs → downstream, wrapped in `downstreamChanges`) while `xr` is one-level.
   Unifying the wire shapes now would force either a breaking `comp` change
   (out of scope) or contorting `xr` into `comp`'s less-rich shape. So: reuse the
   already-shared building blocks (`Summary`, `ChangeDetail`, the `XRStatus`
   constants, and the human per-section rendering helper where it falls out
   cleanly), keep the paths otherwise separate, and record a future-work item to
   unify the per-XR structured shape at the next breaking `comp` window — bundled
   with `comp`'s error-model upgrade (string → `OutputError`) and the flat
   `changes[]` removal, when a breaking `comp` change is already on the table.

## JSON/YAML Output Shape (§1)

```jsonc
{
  "summary":  { "added": N, "modified": N, "removed": N },   // aggregate — kept, not deprecated
  "changes":  [ { type, apiVersion, kind, name, namespace, diff }, ... ], // DEPRECATED (shape unchanged)
  "errors":   [ { resourceID, message, validationFailures? }, ... ],      // union of per-XR errors — kept
  "xrs": [                                                   // NEW, always present
    {
      "xr":      { "apiVersion": "...", "kind": "...", "name": "...", "namespace": "..." },
      "status":  "changed" | "unchanged" | "error",
      "summary": { "added": N, "modified": N, "removed": N },  // this XR's counts
      "changes": [ { type, apiVersion, kind, name, namespace, diff }, ... ],
      "errors":  [ { resourceID, message, validationFailures? }, ... ] // this XR's errors (usually 0 or 1)
    },
    ...
  ]
}
```

Semantics:

- **One `xrs[]` entry per input XR/claim**, in **input order** (stable — matches
  the order files/resources were loaded). Includes unchanged XRs
  (`status: unchanged`, empty `changes`) and errored XRs (`status: error`, its
  error in the entry's `errors[]`, empty `changes`).
- `xrs[i].xr` is the **input** XR's identity, not a composed resource.
- `xrs[i].changes[]` uses the same `ChangeDetail` shape as the flat list, sorted
  by kind/name within the group (reusing the existing sort), so consumers reuse
  existing parsing.
- Top-level `errors[]` remains the **union** of all per-XR errors (back-compat:
  today's consumers read errors here). Each errored XR *also* surfaces its error
  inside its own `xrs[]` entry. This intentional overlap mirrors the existing
  documented overlap between `OutputError.ResourceID` and `ValidationFailures`:
  the top-level list stays complete, the grouped view is additive.
- `status` reuses the existing `XRStatus` constants (`changed` / `unchanged` /
  `error`) already defined for `comp`. `filtered` does not apply to `xr` (no
  composition-adoption filtering).
- `status` derivation per group: `error` if the group carries an error; else
  `changed` if the group has ≥1 non-equal diff; else `unchanged`.
- Equal diffs are skipped in both flat and grouped `changes[]`. A group with
  only equal diffs (e.g. the XR stored as `DiffTypeEqual` for removal detection)
  → `status: unchanged`, empty `changes`, and does not inflate counts.

## Internal Types & Data Flow (§2)

**New type** — defined in `renderer` (alongside the `DiffRenderer` interface it
parameterizes; matches where `comp`'s `XRImpact` lives):

```go
// XRDiffGroup pairs one input XR/claim with the diffs its render produced
// (or the error that prevented them). Built in PerformDiff and passed to the
// renderer so structured output can group by input XR, and the human renderer
// can render per-XR sections.
type XRDiffGroup struct {
    XR    corev1.ObjectReference       // input XR/claim identity; zero-valued for comp's internal reuse
    Diffs map[string]*dt.ResourceDiff  // this XR's diff tree; nil/empty when Err != nil or unchanged
    Err   *dt.OutputError              // nil unless this XR failed; pre-converted (see below)
}
```

`Err` is the already-converted `*dt.OutputError`, not a raw `error`, because
`NewOutputError` (which extracts typed validation failures) lives in
`diffprocessor`. Having the renderer convert a raw error would require a
`renderer → diffprocessor` back-edge (an import cycle and a layering
violation). `PerformDiff` performs the conversion exactly as it does today; the
shared `dt.OutputError` type lives in the leaf `renderer/types` package imported
by both layers.

**Interface change** (`cmd/diff/renderer/diff_renderer.go`):

```go
// before
RenderDiffs(diffs map[string]*dt.ResourceDiff, errs []dt.OutputError) error
// after
RenderDiffs(groups []XRDiffGroup, errs []dt.OutputError) error
```

`errs` stays the top-level/global (union) error slice, preserving the existing
contract.

**`PerformDiff` change** (`diff_processor.go`): instead of `maps.Copy`-merging
each iteration's diffs into one flat `allDiffs`, build `groups []renderer.XRDiffGroup`
— one per loop iteration — capturing the input XR's `ObjectReference`, its
returned diffs map (success) or its `*dt.OutputError` (failure). Continue to also
accumulate `outputErrors` for the top-level union param. Call
`RenderDiffs(groups, outputErrors)`. `hasDiffs` computation is unchanged (scan
all groups' diffs for a non-equal entry).

**Call-site updates** (production):

- `diff_processor.go` — passes the built `groups`.
- `comp_diff_renderer.go:142` and `:271` — each renders a flat downstream map
  internally; wrap it in a single **identity-less** group
  (`[]renderer.XRDiffGroup{{Diffs: m}}`). Comp output stays unchanged (see §3
  seam).
- `MockDiffRenderer.RenderDiffs` (`testutils/mocks.go`) — signature update.

**Blast radius:** 2 interface impls (human, structured) + 1 mock + 3 production
call sites + their tests.

**Shared building blocks (DRY without over-coupling).** `XRDiffGroup` reuses the
existing shared types rather than introducing parallel ones: `Summary`,
`ChangeDetail`, and the `XRStatus` constants are all already defined in
`renderer` and used by `comp`. The human per-section rendering (`renderDiffList`)
is written to serve both the `xr` grouped path and `comp`'s existing downstream
block. This is the bounded, non-breaking overlap; the fuller per-XR-shape
unification with `comp` is deferred (see Future Work / Decision 5).

## Human Renderer: Group by Input XR (§3)

**The seam — "identity ⟹ section".** `XRDiffGroup.XR` identity is populated only
by the `xr` command. Comp's internal reuse passes identity-less groups. The human
renderer's rule:

- **Group has XR identity** → print a section header, the group's diffs, and a
  per-section summary.
- **Group has no identity** (comp reuse) → render diffs as a flat block, no
  header, no section summary — exactly today's behavior.

This is self-consistent (no header can be printed without identity) and keeps
comp's downstream-diff output unchanged without a second interface method.
Implementation: extract the existing per-diff render loop into an unexported
`renderDiffList(diffs, w)` helper; `RenderDiffs` iterates groups and calls it per
section.

**`xr` human output (illustrative):**

```
=== XNopResource/my-xr ===
~~~ Bucket/my-xr-abc123
<diff body>
---
Summary: 1 modified

=== XNopResource/other-xr ===
No changes.

=== XNopResource/broken-xr ===
Error: cannot get composition: no CompositionRevision matches ...

================================================================================
Total: 0 added, 1 modified, 0 removed across 3 XRs (1 unchanged, 1 error)
```

- **Sections in input order**; diffs within a section stay kind/name-sorted.
- **Per-section summary** reuses today's summary line; unchanged sections print
  `No changes.`
- **Aggregate footer** only when >1 identity-bearing group (a single `xr` file
  gets just its section).
- A single-XR invocation gains a section header vs. today. Accepted trade.

**Errored XRs in human output:** errors continue to go to stderr (unchanged,
back-compat via the `errs` param). Additionally, an errored XR gets an inline
`Error: ...` section on stdout, mirroring what `comp` already does (composition
errors inline on stdout *and* top-level errors to stderr). Without it a grouped
run would silently omit the broken XR from stdout.

## Testing (§4)

**Structured-assertion harness** (`testutils/structured_assertions.go`) — add a
grouped-view builder mirroring the existing `ExpectDiff()` style. The existing
flat assertions keep working (flat fields persist); a parallel path asserts
`xrs[]`:

```go
tu.AssertStructuredDiff(t, jsonOutput, tu.ExpectDiff().
    WithSummary(1, 1, 0).                    // aggregate (flat, still asserted)
    WithXR("XNopResource", "my-xr", "").      // opens an xrs[] entry
        WithXRStatus("changed").
        WithXRSummary(1, 0, 0).
        WithXRChange("modified", "Bucket", "my-xr-abc123", "").
            WithFieldChange("spec.forProvider.region", "us-east-1", "us-west-2").
        AndXR().
    WithXR("XNopResource", "other-xr", "").
        WithXRStatus("unchanged").
        AndXR().
    And())
```

Reuses the existing `WithAnyName()` / `WithNamePattern()` / `WithField*`
vocabulary on grouped changes. The parse target gains an `Xrs` field.

**Coverage:**

- **Unit — `structured_renderer_test.go`:** multi-group input asserting both the
  flat view (back-compat) and `xrs[]`; unchanged group; errored group (status +
  entry `errors[]` + top-level union); single-group; empty input.
- **Unit — `diff_renderer_test.go`:** identity-bearing groups produce headers +
  per-section summaries + aggregate footer; identity-less group (comp reuse)
  stays flat (regression guard for comp).
- **Unit — `diff_processor_test.go`:** `PerformDiff` builds one group per input
  in order; success/error mix.
- **Integration — `diff_integration_test.go`** (real envtest + docker functions):
  a multi-XR invocation asserting grouped JSON via the new harness — the
  real-cluster proof the grouping is authoritative. Preferred over E2E per the
  repo's IT-over-E2E preference for its own code.
- **E2E ANSI goldens:** the human output changes (headers/footer), so affected
  `xr` `.ansi` fixtures must be regenerated. **Gated review (see below).**

### E2E golden regeneration gate

`E2E_DUMP_EXPECTED=1` regenerates candidate goldens but does **not** make them
correct — it blesses whatever the code currently emits, bugs included. A
human-reviewed diff is what authorizes each commit. The committed `.ansi` files
are the baseline, so `git diff` after regeneration shows the delta directly.
Procedure:

1. Make the code change; run with `E2E_DUMP_EXPECTED=1` to regenerate goldens.
2. For each regenerated `.ansi`, review the `git diff` and confirm **every**
   delta is an expected consequence of this design — new `=== Kind/name ===`
   section headers, per-section summary lines, aggregate footer — and nothing
   else. Watch specifically for: doubled/mangled ANSI escape bytes, unexpected
   diff-body content changes, color/marker regressions, reordered resources.
3. Stage a golden **only** after its diff is explained. Any unexplained delta is
   treated as a bug to investigate, not rubber-stamped.

## Documentation Updates

Per CLAUDE.md "Keeping Documentation in Sync":

- **Design doc `design/design-doc-cli-diff.md`:** `§6.8.3` (new `xrs[]` output
  type + `XRDiffGroup`); `§6.8.1` `DiffRenderer` interface block (widened
  signature); `§6.8.2` if the error contract narrative needs the per-XR `errors[]`
  note; correct any "human output already renders per-file/per-XR sections"
  misstatement (it does not, today). Regenerate `diff-rendering-architecture.svg`
  from its `.mermaid` source.
- **README.md:** document `xrs[]` in the structured-output schema section; mark
  flat `changes[]` deprecated; note the new human per-XR sections and aggregate
  footer.
- **Go doc comments:** deprecation notice on `StructuredDiffOutput.Changes`; doc
  on `XRDiffGroup` and the new `xrs[]` wire types.

```go
// Changes is the flat, ungrouped list of all resource changes across every
// input XR.
//
// Deprecated: use Xrs for per-input-XR grouping. This field is retained for
// backward compatibility and will be removed in a future major release.
Changes []ChangeDetail `json:"changes"`
```

## Git / PR conventions

- DCO sign-off on every commit (`git commit -s`).
- Open the PR as a draft (`gh pr create --draft`), following
  `.github/PULL_REQUEST_TEMPLATE.md` (Description heading, `Fixes #405`, the
  "I have:" checklist with `[x]` or `[ ] ~~struck~~` per item).
- Feature commit shape: `feat(xr): group structured output by input XR (#405)`.

## Out of Scope

- Removal of the deprecated flat `changes[]` (future major, `feat(xr)!:`).
- Any change to the `comp` command's output.
- Runtime deprecation warnings.
- Changes to the Sanyaku runner (separate repo; consumes this once shipped).
- **Unifying the `comp` and `xr` per-XR structured shapes** (see Future Work).

## Future Work

- **Unify the per-XR structured shape across `comp` and `xr`.** Both commands
  will carry a per-XR bucket (`XRImpact` / `XRDiffGroup`); today they diverge in
  wire shape, error model, filter concepts, and nesting depth (see Decision 5).
  At the next breaking `comp` window — bundled with (a) upgrading `comp`'s wire
  error from `string` to the richer `OutputError`/`validationFailures[]` model
  `xr` uses, and (b) removing the deprecated flat `xr` `changes[]` — harmonize the
  two into one per-XR entry shape so CI wrappers can parse both commands
  identically. Doing it then costs nothing extra (a breaking `comp` change is
  already on the table) and avoids a separate breaking event now.
