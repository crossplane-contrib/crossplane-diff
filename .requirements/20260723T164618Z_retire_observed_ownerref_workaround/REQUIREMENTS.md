# Retire observed-owner-ref alignment workaround (fix "not controlled by the XR")

- **Issue:** crossplane-contrib/crossplane-diff#399
- **Branch:** `fix-not-controlled-by-xr`
- **Date (UTC):** 2026-07-23
- **Decisions locked (via AskUserQuestion):**
  - Fix strategy: **Option B — retire the workaround** (`alignObservedOwnerRefs` + `fakeXRUID`).
  - Version safety: **document minimum version only** (bump `go.mod` pin + README; no runtime version probe).
- **Environment note:** The `tdd` skill prescribes Lad MCP `system_design_review` / `code_review`. The Lad
  MCP server is **not available** in this environment. Substituting the repo's established review discipline:
  `earthly +reviewable` (lint + tests + generation) locally, and Copilot review on the draft PR.

---

## 1. As Is

`crossplane-diff` renders XRs by shelling out to the `crossplane render` binary inside Docker
(`xpkg.crossplane.io/crossplane/crossplane:stable`), passing the input XR plus observed composed
resources fetched from the cluster.

To make observed resources usable by that binary, `cmd/diff/diffprocessor/render_engine.go` contains a
workaround (introduced in PR #326):

- `fakeXRUID(xr)` — replicates the deterministic UID the render binary historically assigned to the XR:
  `SHA1(gvk + "\x00" + namespace + "\x00" + name)`.
- `alignObservedOwnerRefs(xr, observed)` — for every observed resource, rewrites any owner reference
  matching the XR by `APIVersion + Kind + Name` so its `UID` becomes `fakeXRUID`. Called unconditionally
  at `EngineRenderFn.Render` when building the composite request
  (`ObservedResources: alignObservedOwnerRefs(in.CompositeResource, in.ObservedResources)`).

This existed because crossplane render **≤ v2.3.3** *always* overwrote the input XR's UID with the SHA1
value and then, in `ExistingComposedResourceObserver.ObserveComposedResources`, silently **dropped** any
observed resource whose controller-ref UID didn't equal that (fake) XR UID. Rewriting observed refs to the
fake UID kept them visible to the function pipeline.

Separately, `diff_processor.go:328` already sets the input XR's UID to the **real cluster UID**
(`xr.SetUID(existingXRFromCluster.GetUID())`, from PR #145, to preserve nested-XR identity). Observed
resources are only ever populated when the (backing/nested) XR **exists** in the cluster, so in practice
observed controller-refs always carry the real XR UID.

**The bug:** crossplane render **v2.3.4** (upstream crossplane/crossplane#7544, `backport release-2.3`,
now what `:stable` resolves to) changed two things:
1. The XR UID is set **only when empty** (`if xr.GetUID() == "" { xr.SetUID(SHA1…) }`) — a non-empty
   input UID is now **preserved**.
2. It added `CheckObservedResources`, which **hard-errors** (instead of silently dropping) when an
   observed resource's controller-ref UID ≠ the XR UID.

So on v2.3.4: crossplane-diff sends the XR with its real UID → the binary preserves it → but
`alignObservedOwnerRefs` has already rewritten the observed refs to the SHA1 `fakeXRUID` → the two no
longer match → `CheckObservedResources` fails with:

```
cannot render composite resource: invalid observed resources:
[observed resource <name> has a controller ref but is not controlled by the XR, ...]
```

The workaround now *corrupts* refs that the newer binary would have accepted unchanged.

### Confirmed failing baseline (red tests)

`earthly -P +go-test` (containerized, docker render `:stable` = v2.3.4, log at `/tmp/it-earthly-v234.log`)
reproduces the bug. Unique failing IT subtests (a floor — Go aborts the package before completing all):

- `TestDiffIntegration/ResourceRemovalHierarchyV1ClusterScoped`
- `TestDiffIntegration/ResourceRemovalHierarchyV2ClusterScoped`
- `TestDiffIntegration/ResourceRemovalHierarchyV2Namespaced`
- `TestDiffIntegration/ResourceRemovalWithUnmodifiedXR`
- `TestDiffIntegration/FunctionSequencerPreservesExistingResources`
- `TestDiffIntegration/ModifiedClaimWithNestedXRsShowsDiff`
- `TestDiffIntegration/ResourceWithGenerateName`
- `TestCompDiffIntegration/NestedXRUsesOwnComposition`
- `TestCompDiffIntegration/SSAFieldRemovalDetection`
- `TestCompDiffIntegration/CompSequencerGatedStageAppearsWithEventualState`
- `TestCompDiffIntegration/CompConditionalGatedStageAppearsWithEventualState`

All fail with the identical `has a controller ref but is not controlled by the XR` cause (37 occurrences).
Every failing case feeds real cluster-observed resources into render; new-resource-only cases pass. **These
red tests are our TDD failing tests** (per the user's instruction — no bespoke repro test is required).

---

## 2. To Be

crossplane-diff relies on crossplane render's corrected v2.3.4+ behavior instead of working around the old
bug:

- Observed composed resources are passed to the render binary **unmodified**. Their controller-ref UIDs
  (the real cluster XR UID) match the XR UID the binary now preserves, so `CheckObservedResources` passes
  and `ExistingComposedResourceObserver` reads them.
- The `alignObservedOwnerRefs` / `fakeXRUID` workaround is **removed**.
- The minimum supported crossplane render version (**v2.3.4**) is pinned in `go.mod` and documented in the
  README. No runtime version probe (per decision).
- All 11 previously-red ITs pass; no other unit/IT tests regress.

---

## 3. Requirements

**R1.** `EngineRenderFn.Render` MUST pass `in.ObservedResources` to `render.BuildCompositeRequest`
unmodified (no owner-ref UID rewriting).

**R2.** `alignObservedOwnerRefs` and `fakeXRUID` (and any now-unused imports/helpers they alone pulled in)
MUST be removed from `render_engine.go`.

**R3.** The `go.mod` pin for the crossplane render libraries MUST be ≥ v2.3.4. Specifically bump
`github.com/crossplane/crossplane/v2` from `v2.3.3` to `v2.3.4` (the `apis/v2` and `cli/v2` /
`crossplane-runtime/v2` modules are already at ≥ v2.4.0-rc / v2.5.0-rc and need no change; verify `go mod
tidy` stays consistent). The go.mod pin governs the *library* code compiled in; the render *binary* version
is the docker `:stable` image, which is already ≥ v2.3.4.

**R4.** The README MUST state that crossplane-diff requires a `crossplane` render image/binary ≥ v2.3.4,
with a one-line rationale (older images silently drop observed resources / produce incorrect removals).

**R5.** Any comments in `render_engine.go` that describe the removed workaround or reference the (now
non-existent) `TestDiffCompositionWithGetComposedResource` MUST be removed or corrected so no stale
guidance remains.

**R6.** No behavioral change for the *new-XR* path (empty UID): the render binary still derives the SHA1
UID itself; crossplane-diff sends no observed resources in that case, so removing the alignment is a no-op
there.

**R7 (docs sync).** Per repo CLAUDE.md doc-sync triggers, evaluate `design/design-doc-cli-diff.md` for the
render-engine behavior change and update if the removed workaround is described there.

---

## 4. Acceptance Criteria

**AC-R1 / AC-R2:**
- `render_engine.go` no longer contains the identifiers `alignObservedOwnerRefs` or `fakeXRUID`
  (`grep -c` → 0).
- The `BuildCompositeRequest` call uses `ObservedResources: in.ObservedResources`.
- `go build ./...` and `go vet ./...` succeed (no unused-import / undefined-symbol errors).

**AC-R3:**
- `go.mod` shows `github.com/crossplane/crossplane/v2 v2.3.4` (or higher).
- `go mod tidy` produces no further diff; `go.sum` is consistent.

**AC-R4:**
- README contains an explicit "requires crossplane render ≥ v2.3.4" statement discoverable via
  `grep -n "2.3.4" README.md`.

**AC-R5:**
- No occurrence of `TestDiffCompositionWithGetComposedResource` remains in the codebase
  (`grep -rn` → 0), and no comment describes owner-ref UID "alignment" as live behavior.

**AC (overall / the real gate):**
- `earthly -P +go-test` exits 0. In particular, all 11 previously-red ITs (§1) PASS, and the full unit +
  IT suite shows no new failures relative to a clean `main` baseline.
- `earthly -P +reviewable` passes (lint + tests + generation clean).

---

## 5. Testing Plan (TDD)

**Failing tests already exist** — the 11 red ITs in §1 are the RED state. Per the user's instruction we do
not author a new bespoke reproduction test; the fix must turn these green.

1. **RED (captured):** `/tmp/it-earthly-v234.log` — 11 ITs failing with `not controlled by the XR` on
   v2.3.4. This is our baseline.
2. **GREEN (target):** After the change, `earthly -P +go-test` → exit 0; the 11 ITs pass. This is the
   primary acceptance signal.
3. **No-regression:** The full package (`TestDiffIntegration`, `TestCompDiffIntegration`, and all
   `diffprocessor`/`renderer`/`client` unit tests) passes. Compare the green run's subtest set against a
   clean `main` list to ensure nothing that passed before now fails.
4. **Unit-level sanity (existing):** `render_engine_test.go` tests (`HappyPath`, `Serialization`,
   `MultiCompositionFunctionSet`, `CleanupIdempotent`, `PreservesExistingNetworkAnnotation`,
   `CleanupStopsAllFunctionAddresses`) must still pass — they exercise `EngineRenderFn.Render` plumbing and
   must not depend on the removed alignment. If any asserted alignment behavior, that assertion is removed
   with the workaround (verified: no `_test.go` references `alignObservedOwnerRefs`/`fakeXRUID`, so none do).
5. **Fast inner loop:** `go build ./... && go vet ./...` after the code edit; `go test ./cmd/diff/diffprocessor/... -run TestEngineRenderFn -count=1` for the unit slice before paying for the full containerized IT run.

**Version-skew note (not automated):** we deliberately do NOT add a test pinning an old (<v2.3.4) binary,
since (a) there's no supported user path to select one and (b) decision is "document min version only". The
green run against `:stable` (v2.3.4) is the authoritative signal.

---

## 6. Implementation Plan (smallest sequential steps)

Ordered smallest-first; each step states how it's tested.

**Step 1 — Remove the workaround call site (R1).**
In `EngineRenderFn.Render`, change
`ObservedResources: alignObservedOwnerRefs(in.CompositeResource, in.ObservedResources),`
to `ObservedResources: in.ObservedResources,`.
*Test:* `go build ./...` will now warn/error that `alignObservedOwnerRefs`/`fakeXRUID` are unused — expected;
resolved in Step 2. (Serena `replace_content` for the one line.)

**Step 2 — Delete `fakeXRUID` and `alignObservedOwnerRefs` and their doc comments (R2, R5).**
Remove both functions (~lines 249–312) and the stale `TestDiffCompositionWithGetComposedResource` comment.
Remove any import made unused (candidates: `github.com/google/uuid`, `k8s.io/apimachinery/pkg/types`,
`composed` — verify each is still used elsewhere in the file before removing; `composed` is still used by
`RenderInputs.ObservedResources` type, so keep it).
*Test:* `go build ./... && go vet ./...` clean. `go test ./cmd/diff/diffprocessor/... -run TestEngineRenderFn -count=1` passes.

**Step 3 — Bump go.mod crossplane pin to v2.3.4 (R3).**
Edit `go.mod`: `github.com/crossplane/crossplane/v2 v2.3.3` → `v2.3.4`. Run `go mod tidy`.
*Test:* `go build ./...` clean; `go mod tidy` yields no further diff; `git diff go.sum` is consistent.

**Step 4 — Document minimum version in README (R4).**
Add a short "Requirements / crossplane render ≥ v2.3.4" note near the existing render/usage docs.
*Test:* `grep -n "2.3.4" README.md` shows the statement.

**Step 5 — Doc-sync check (R7).**
Grep `design/design-doc-cli-diff.md` for any description of observed-owner-ref alignment / fakeXRUID; update
if present. If absent, note "no drift" in the PR.
*Test:* `grep -in "alignObserved\|fakeXRUID\|controller ref" design/design-doc-cli-diff.md`.

**Step 6 — Full verification (all ACs).**
Run `earthly -P +go-test` (must exit 0; 11 red ITs now green) then `earthly -P +reviewable`.
*Test:* both exit 0; diff the green subtest list vs. `main` for no regressions.

**Step 7 — Commit (DCO) + push + draft PR.**
`git commit -s`, push topic branch to origin via explicit refspec, open **draft** PR referencing #399 using
the repo PR template. If `+reviewable` autofixed unrelated files, commit them as a separate lint-fix commit.
