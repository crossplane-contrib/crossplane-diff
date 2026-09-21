# Composition Revision Selector Awareness (issue #388)

GitHub issue: https://github.com/crossplane-contrib/crossplane-diff/issues/388

## As Is

crossplane-diff ignores `compositionRevisionSelector` when it decides which
CompositionRevision an XR would resolve to. This surfaces as **two distinct bugs
sharing one root cause**:

### Bug A — `comp` command (the filed issue)

`cmd/diff/diffprocessor/comp_processor.go` finds every XR whose `compositionRef.name`
matches the composition, then `partitionXRsByUpdatePolicy` splits them into kept/dropped
sets. That partition checks **only** `compositionUpdatePolicy` (Manual vs Automatic):

```go
switch policy {
case compositionUpdatePolicyManual:
    dropped = append(dropped, xr)   // dropped unless --include-manual
default: // Automatic or unset
    kept = append(kept, xr)
}
```

It never inspects `compositionRevisionSelector`. So an Automatic-policy XR whose
selector would **not** match the edited composition's labels is wrongly reported in the
affected-resources / impact-analysis sections. The XR would not actually pick up the new
revision in a real cluster.

### Bug B — `xr` command (latent, same root cause)

`cmd/diff/client/crossplane/composition_client.go` `resolveCompositionFromRevisions`,
under Automatic policy, calls `GetLatestRevisionForComposition(name)` which returns the
**highest-numbered revision of that composition**, unfiltered by the XR's
`compositionRevisionSelector`. So an XR pinned via selector to (e.g.) `channel: active`
is rendered against the newest revision even when that revision is `channel: preview`.

### Output model (to be changed)

`XRImpact.Status` is a single enum that conflates outcome with cause. The value
`filtered_by_policy` (`XRStatusFilteredByPolicy`) smuggles the *reason* (Manual policy)
into the *status*. `AffectedResourcesSummary.FilteredByPolicy` counts such XRs.

## To Be

crossplane-diff evaluates `compositionRevisionSelector` the way Crossplane does when
selecting a CompositionRevision under **Automatic** policy.

### Crossplane's authoritative selection logic

From `crossplane/crossplane` `internal/controller/apiextensions/composite/api.go`:

```go
if policy == Automatic && cr.GetCompositionRevisionSelector() != nil {
    ml = cr.GetCompositionRevisionSelector().MatchLabels
}
ml[LabelCompositionName] = comp.GetName()
// list revisions matching ml, pick the highest revision number
```

Model: **the selector matches a *set* of revisions; Crossplane picks the newest in that
set.** CompositionRevisions freeze the Composition's labels at each edit (label/annotation
changes even trigger a new revision).

### Bug A fix (`comp`)

Because a CompositionRevision inherits the Composition's labels, the **edited composition
file itself is the prediction of the new revision** — no extra cluster fetch is needed.
For each Automatic-policy XR that has a `compositionRevisionSelector`, evaluate that
selector against the edited composition's `metadata.labels`. If it does **not** match, the
XR is excluded from the affected set with reason `revision_selector_mismatch`.

### Bug B fix (`xr`)

Under Automatic policy, when the XR has a `compositionRevisionSelector`, resolve to the
**latest revision whose labels match the selector**, rather than the latest revision
overall. If no revision matches, fail the diff with a clear error (mirrors Crossplane
posting an error event / `Synced=False`; we prioritise accuracy over guessing).

### Output model (divorced)

- `XRImpact.Status` collapses the filtered outcome to `filtered`
  (`XRStatusFiltered`). Values become: `changed`, `unchanged`, `error`, `filtered`.
- New field `XRImpact.FilterReason` (`FilterReason` type), only meaningful when
  `Status == filtered`. Values: `manual_policy`, `revision_selector_mismatch`
  (extensible, e.g. `composition_selector_mismatch` later).
- `AffectedResourcesSummary`: keep `FilteredByPolicy` (Manual) **and** add
  `FilteredBySelector` (revision-selector mismatch), so summary-level counts remain
  visible even in default-discovery mode where per-XR impacts are not surfaced.
  *(Open sub-decision — could instead be a single `Filtered` total; chosen split for
  default-mode visibility. Adjustable.)*
- Human renderer verbiage distinguishes "Filtered by policy" from
  "Filtered — revision selector mismatch".

This is a **breaking change** to the JSON/YAML structured-output contract
(`status: filtered_by_policy` → `status: filtered` + `filterReason: manual_policy`).

### `--include-manual` semantics

`--include-manual` governs **Manual policy only**. Selector-mismatched Automatic XRs stay
excluded even with the flag, because they are genuinely unreachable by the new revision.
(A general "show everything name-matched" escape hatch could be a future separate flag.)

### Considered and rejected: a selector-override flag

We deliberately do **not** add a `--ignore-revision-selector` (or similar) flag analogous
to `--include-manual`. Rationale:

- **`--include-manual` cannot be expressed by editing inputs** — a Manual XR's pin is
  intrinsic to what "Manual" means, so previewing "what if I bumped it" needs a tool
  affordance. The selector case is the opposite: it is **always** expressible by editing
  the composition's labels or the XR's selector, both of which are files the user already
  hands to the tool. No cluster round-trip makes this awkward.
- **A forced selector match produces a *dishonest* counterfactual** — rendering an XR
  against a revision it provably would never select, with no indication of which label
  edit would make it real. This contradicts the tool's "accuracy above all else"
  principle. `--include-manual` at least renders against a revision the XR *could* adopt
  with a one-field policy flip.
- **Precise revision pinning already exists**: `compositionRevisionRef` + Manual policy
  (surfaced via `--include-manual`) is the exact, honest tool for "diff against this
  specific revision."

### Supported CI pattern: preview vs. live via input labels (no tool semantics)

Because a CompositionRevision inherits the composition's labels, the **composition file's
labels are the authoritative prediction of the new revision**. This means the
"what about preview-labeled compositions?" workflow is a pure input-manipulation concern,
solved in the CI runner, with **zero tool semantics**:

- **Base case (the primary CI use):** run `comp` against the real composition with its
  real labels — "what happens when this PR merges" (real labels, real selectors, real
  gitops).
- **Preview matrix (optional second run):** mutate the composition's labels in the runner
  (`jq`/`yq`/kustomize) to the preview labels, then run `comp` again. Both sides then
  consistently describe the preview world — an *honest* counterfactual: a real
  would-be revision diffed against real selectors.

This is a documentation concern (README CI-usage section), **not** a code change. The
recipe only works while we hold the invariant that composition-file labels are
authoritative for the predicted revision in the `comp` path (see Bug A fix) — an in-tool
label override would reintroduce the dishonest-counterfactual footgun rejected above.

## Requirements

**R1.** Provide a reusable predicate that, given an XR and a set of composition labels,
reports whether the XR's `compositionRevisionSelector` (if any) matches those labels,
honoring Automatic-only semantics and both v2/v1 field paths.
- **AC1.1** XR with no `compositionRevisionSelector` → treated as "matches" (no
  restriction), regardless of policy.
- **AC1.2** Automatic-policy XR with selector `matchLabels` that is a subset of the
  composition labels → matches.
- **AC1.3** Automatic-policy XR with selector `matchLabels` not satisfied by the
  composition labels → does not match.
- **AC1.4** Selector `matchExpressions` (In/NotIn/Exists/DoesNotExist) are honored via
  `metav1.LabelSelectorAsSelector` + `labels.Set`.
- **AC1.5** v2 path `spec.crossplane.compositionRevisionSelector` and v1 path
  `spec.compositionRevisionSelector` are both read (v2 preferred), reusing
  `getCrossplaneRefPaths`.
- **AC1.6** Manual-policy XR: selector is **not** applied by this predicate (Manual XRs
  are governed by the existing policy filter / `--include-manual`); predicate returns
  "matches" so it never double-filters Manual XRs.
- **AC1.7** Empty/malformed selector (`matchLabels: {}` and no expressions) → matches
  (an empty selector selects everything, per k8s semantics).

**R2.** `comp` command: `partitionXRsByUpdatePolicy` (and its callers) exclude
Automatic-policy XRs whose `compositionRevisionSelector` does not match the edited
composition's labels, tagging them with reason `revision_selector_mismatch`.
- **AC2.1** Given a composition with labels `{version: 0.0.2}` and an Automatic XR with
  selector `matchLabels {version: 0.0.1}`, the XR is dropped and **not** in the affected
  set / impact analysis as changed.
- **AC2.2** Same composition, Automatic XR with selector `matchLabels {version: 0.0.2}` →
  XR is kept and evaluated.
- **AC2.3** Automatic XR with **no** selector → kept (unchanged from today).
- **AC2.4** Manual XR with a matching selector → still dropped by policy (unless
  `--include-manual`), reason `manual_policy` (selector does not rescue it).
- **AC2.5** `--include-manual` re-includes Manual XRs but does **not** re-include
  selector-mismatched Automatic XRs.
- **AC2.6** In `--resource` mode (surfaceFiltered), dropped selector-mismatch XRs appear
  in impact analysis with `Status=filtered`, `FilterReason=revision_selector_mismatch`.
- **AC2.7** Summary counts: `FilteredByPolicy` counts Manual drops; `FilteredBySelector`
  counts selector-mismatch drops; `Total` includes both.

**R3.** `xr` command: under Automatic policy with a `compositionRevisionSelector`, resolve
to the latest revision whose labels match the selector.
- **AC3.1** Given revisions rev1 `{channel: active}` (revision 1) and rev2
  `{channel: preview}` (revision 2, newest), an Automatic XR with selector
  `{channel: active}` resolves to **rev1**.
- **AC3.2** Same revisions, selector `{channel: preview}` → resolves to **rev2**.
- **AC3.3** Walking tag: revisions rev1 `{major: v1}` (rev 1) and rev2 `{major: v1}`
  (rev 2) → selector `{major: v1}` resolves to **rev2** (newest matching).
- **AC3.4** Automatic XR with **no** selector → latest revision overall (unchanged from
  today).
- **AC3.5** Selector matches **no** revision → diff fails with a clear, actionable error
  naming the selector and composition.
- **AC3.6** Manual policy path is unchanged (selector not consulted; pinned via
  `compositionRevisionRef`).

**R4.** Output model change is applied consistently across renderer, structured
assertions, README, and design doc.
- **AC4.1** JSON/YAML emits `status: filtered` plus `filterReason: <reason>` for filtered
  XRs; no remaining emission of `filtered_by_policy` as a status.
- **AC4.2** Human renderer prints distinct verbiage for the two filter reasons.
- **AC4.3** `structured_assertions.go` helpers assert `Status`+`FilterReason`.
- **AC4.4** README structured-output schema section and design doc §6/§7 updated.
- **AC4.5** README documents the preview-vs-live CI pattern (mutate composition labels in
  the runner, run `comp` twice) and notes the selector-override flag was intentionally
  omitted.
- **AC4.6** Human/structured output for a selector-mismatch exclusion names the concrete
  cause and the fix hint, e.g. "excluded: compositionRevisionSelector {version: 0.0.1}
  does not match composition labels {version: 0.0.2}", so "where did my XR go?" is
  self-service without a flag.

## Testing Plan (TDD)

Tests are written **before** each implementation step. Prefer table-driven unit tests,
fluent `ResourceBuilder` fixtures, and structured JSON assertions over ANSI goldens.

- **T1 (unit, R1):** New test for the selector predicate — table over AC1.1–AC1.7. Build
  XRs via `ResourceBuilder`; may need a `WithNestedField`-based selector or a new
  `WithCompositionRevisionSelector` builder method (extend the builder per convention).
- **T2 (unit, R2):** Extend `TestDefaultCompDiffProcessor_partitionXRsByUpdatePolicy`
  (currently `...filterXRsByUpdatePolicy`) with selector cases AC2.1–AC2.5. The partition
  now needs the edited composition's labels, so the method signature/inputs change — the
  test supplies composition labels. Assert kept/dropped **and** drop reason.
- **T3 (unit, R2 summary):** Table test on `processSingleComposition` /
  `buildImpactAnalysis` covering AC2.6–AC2.7 (impact entries + summary counters).
- **T4 (unit, R3):** Extend the composition-client revision-resolution tests with
  AC3.1–AC3.6 (selector-filtered latest-revision selection, no-match error). Mock the
  revision client to return a labeled revision set.
- **T5 (unit, R4):** Renderer + structured-assertion tests for the new `status`/
  `filterReason` shape (AC4.1–AC4.3).
- **T6 (integration, optional):** `diff_integration_test.go` — a `comp` run with a
  selector-mismatched Automatic XR asserts it is absent from impact analysis.
- **T7 (e2e, deferred/optional):** an e2e manifest set exercising selector filtering, if
  the maintainer wants coverage there; likely a follow-up given cost.

## Implementation Plan (smallest sequential steps)

Each step: write/adjust the test first (red), implement (green), run the smallest
relevant `go test`, then broaden.

1. **Selector predicate (R1 / T1).** Add a helper (e.g. `revisionSelectorMatches(xr,
   compLabels) (matches bool, hasSelector bool, err error)`) reading v2/v1 paths via
   `getCrossplaneRefPaths`, building a `metav1.LabelSelector` from the nested map, and
   evaluating with `metav1.LabelSelectorAsSelector` + `labels.Set(compLabels)`. Test with
   T1. (No behavior change to callers yet.)

2. **Output-model types (R4 types only / T5 partial).** Add `XRStatusFiltered` and the
   `FilterReason` type + constants; add `XRImpact.FilterReason` and
   `xrImpactJSON.filterReason`; add `AffectedResourcesSummary.FilteredBySelector`. Keep
   `XRStatusFilteredByPolicy` temporarily only if needed to compile; goal is to remove it.
   Update structured-assertion helpers. Red→green on renderer tests.

3. **`comp` partition uses the predicate (R2 / T2, T3).** Thread the edited composition's
   labels into `partitionXRsByUpdatePolicy` (or split policy-filter from selector-filter).
   Manual → dropped (reason `manual_policy`); Automatic + non-matching selector → dropped
   (reason `revision_selector_mismatch`); `--include-manual` only rescues Manual. Update
   `processSingleComposition` to record reasons and the two summary counters. Green on
   T2/T3.

4. **Human + structured renderer verbiage (R4 / T5).** Distinct output for the two
   reasons; remove `filtered_by_policy` status emission. Green on renderer tests.

5. **`xr` revision resolution (R3 / T4).** In `resolveCompositionFromRevisions` Automatic
   branch, when the XR has a selector, filter the composition's revisions by the selector
   and pick the newest; error if none match. Likely add a
   `GetLatestRevisionForCompositionMatchingSelector`-style method or filter in-place using
   the existing revision list + the R1 predicate against each revision's labels. Green on
   T4.

6. **Integration test (R2 / T6).** Add/extend `diff_integration_test.go`.

7. **Docs (R4 / AC4.4).** Update README structured-output schema and
   `design/design-doc-cli-diff.md` (§6 interfaces if signatures changed; §7 workflow;
   §4 test-case list). Regenerate any affected mermaid `.svg` only if a diagram changed.

8. **Full `earthly +reviewable`** (lint + tests + generate) before marking done; check
   `git status` for autofix side effects on unrelated files and commit those separately.
