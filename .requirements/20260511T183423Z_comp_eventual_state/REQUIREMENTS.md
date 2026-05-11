# Composition Diff: Eventual State Resolution

## As Is

`crossplane-diff xr` supports `--eventual-state` (added in PR #276, refined in PR #281). When this flag is set, the XR diff processor runs `RenderToStableState` with `synthesizeReady=true`, iteratively rendering until no new composed resources appear (or `MaxRenderIterations` is hit). This is essential for compositions that use `function-sequencer`, which hides later-stage resources until earlier stages become Ready.

`crossplane-diff comp` currently has **no** `--eventual-state` flag. When a composition uses `function-sequencer`, the impact analysis only shows the resources from the first stage, missing the true eventual impact.

Key facts about the existing plumbing:

- `DefaultCompDiffProcessor.collectXRDiffs` delegates per-XR rendering to `p.xrProc.DiffSingleResource` (where `xrProc` is the injected `DiffProcessor`).
- `DiffSingleResource` internally calls `RenderToStableState`, which honors `config.EventualState`.
- `ProcessorOption` → `WithEventualState(bool)` already exists.
- `makeDefaultCompProc` in `cmd/diff/comp.go` constructs the XR processor and the composition processor from a shared `opts` list — adding `WithEventualState` to that list propagates to both.
- `MaxRenderIterations` is already applied via `defaultProcessorOptions`, so no additional flag wiring is required.
- The XR integration test `EventualStateWithSequencer` in `cmd/diff/diff_integration_test.go` (line 1593) already proves the feature works end-to-end through the XR code path. The `eventualState bool` field on `IntegrationTestCase` (line 58) is currently gated by `testType == XRDiffTest` (line 231).

## To Be

`crossplane-diff comp` supports `--eventual-state` with semantics identical to the XR command:

- When `--eventual-state` is set, every impacted XR (including nested XRs encountered during rendering) is rendered in eventual-state mode.
- When unset, behavior is unchanged from today — single render, earliest-stage view.
- Help text, README, and CLI reference all mention the new flag.
- An integration test proves the flag flows from `CompCmd` through to the eventual-state loop, producing all-stage resources for a composition that uses `function-sequencer`.

## Requirements

1. **R1. Flag definition.** `CompCmd` exposes a boolean Kong flag named `--eventual-state` with `default:"false"`, matching `XRCmd`'s field tag verbatim (same help text).
2. **R2. Flag propagation.** `makeDefaultCompProc` passes `dp.WithEventualState(c.EventualState)` into the shared `opts` slice so that both the XR peer processor and the composition processor are configured consistently.
3. **R3. Help output.** `CompCmd.Help()` includes an example line demonstrating `--eventual-state`, mirroring `XRCmd.Help()`.
4. **R4. Documentation.** The README's "Composition Diff" section gains an `--eventual-state` example and the `comp` flags reference block gains an `--eventual-state` entry matching the `xr` block.
5. **R5. Integration test.** `TestCompDiffIntegration` gains a case (`EventualStateWithSequencer`) that (a) sets up a sequencer-based composition, (b) creates an existing XR that uses it, (c) runs with `--eventual-state`, and (d) asserts via structured JSON that both sequencer stages appear in the diff.
6. **R6. Test harness.** The `eventualState` gate in `runIntegrationTest` is extended to accept `CompositionDiffTest` so the new test can set `eventualState: true` and see `--eventual-state` appended to `args`.

### Acceptance Criteria

- **AC1 (R1).** `crossplane-diff comp --help` lists `--eventual-state` with the same help text as `xr`.
- **AC2 (R2).** A unit/integration test demonstrates that setting `--eventual-state` on the CLI causes `RenderToStableState` to iterate more than once for a sequencer composition (observable: multiple stages appear in the diff).
- **AC3 (R3).** `crossplane-diff comp --help` prints the new example line.
- **AC4 (R4).** README changes show the flag alongside other `comp` flags in the reference block and in the usage section.
- **AC5 (R5).** The new integration test passes; without the R2 wiring it MUST fail (verifying the red step of TDD).
- **AC6 (R6).** Running the existing XR `EventualStateWithSequencer` test remains green (no regression from the gate change).

## Testing Plan

Following TDD — red tests first.

### T1. Integration test — `EventualStateWithSequencer` for `comp`

**Location:** `cmd/diff/diff_integration_test.go`, inside `TestCompDiffIntegration`.

**Shape:** Mirror the XR test at line 1593 but adapted for `comp`:

- `eventualState: true`
- `outputFormat: "json"`
- `setupFiles`: XRD + sequencer composition + sequencer composition-revision + functions + a pre-existing XR (so `FindCompositesUsingComposition` returns something to diff against). The existing sequencer fixtures under `testdata/diff/resources/` can be reused.
- `inputFiles`: the updated composition file (path to `sequencer-composition.yaml` or a variant to force a change).
- `expectedStructuredOutput`: `ExpectCompDiff()` asserting both stage0 and stage1 resources appear in the XR's downstream diff.

**What it proves:**

1. Flag is parsed into `CompCmd`.
2. Flag propagates to the XR processor used by `collectXRDiffs`.
3. `RenderToStableState` runs multiple iterations and yields both stages.

**Red behavior:** Before implementing R1/R2/R6, the test MUST fail because either (a) Kong rejects the unknown flag, (b) the gate in `runIntegrationTest` ignores the flag for `CompositionDiffTest`, or (c) `RenderToStableState` runs single-iteration and stage1 is missing.

### T2. Help text assertion (covered implicitly by R3 diff review + manual verification)

Explicit unit test is overkill here; the help string is a static literal reviewed at edit time.

### T3. Regression check

Re-run `TestDiffIntegration/EventualStateWithSequencer` to ensure the gate change in R6 doesn't regress XR behavior.

### T4. Full package tests

`go test ./cmd/diff/...` after wiring is complete to catch any mocks or builder assumptions.

## Implementation Plan

Smallest possible steps, each paired with its verification.

### Step 1: Add failing integration test (RED)

**Change:** In `cmd/diff/diff_integration_test.go`:
1. Extend the gate at line 231 from `tt.eventualState && testType == XRDiffTest` to `tt.eventualState` (i.e., drop the XR-only restriction). Rationale: `--eventual-state` is now a valid comp flag too.
2. Add the new test case `EventualStateWithSequencer` inside `TestCompDiffIntegration`'s `tests` map. Follow the existing composition-diff test style (see line 2960-ish for shape), use `ExpectCompDiff()` helpers, and reuse `sequencer-composition.yaml` + sequencer fixtures.

**Verify (expect FAIL):** `go test ./cmd/diff -run TestCompDiffIntegration/EventualStateWithSequencer -v`

Expected failure mode: Kong parsing error (`unknown flag: --eventual-state`) because `CompCmd` doesn't declare it yet.

### Step 2: Declare the flag on CompCmd (still RED or partial-green)

**Change:** In `cmd/diff/comp.go`, add the `EventualState` field to `CompCmd`:

```go
EventualState bool `default:"false" help:"Show eventual state after all reconciliation cycles complete (useful with function-sequencer)." name:"eventual-state"`
```

**Verify:** `go build ./cmd/diff/...` succeeds. Re-run the test from Step 1 — expect it to still FAIL, but now because the flag is accepted by Kong but ignored (stage1 resources still missing).

### Step 3: Wire flag into processor options (GREEN)

**Change:** In `makeDefaultCompProc`, add `dp.WithEventualState(c.EventualState)` to the shared `opts` slice (modeled on `makeDefaultXRProc`).

**Verify:** Re-run the integration test — expect it to PASS.

### Step 4: Help text

**Change:** Add an example line to `CompCmd.Help()`:

```
  # Show eventual state with function-sequencer (all stages, not just first)
  crossplane-diff comp updated-composition.yaml --eventual-state
```

**Verify:** Visual inspection + `go test ./cmd/diff/...` to confirm no regressions.

### Step 5: README

**Change:** In `README.md`:
- Add an example under "Composition Diff" usage matching the XR example.
- Add the `--eventual-state` entry to the `comp` flags reference block, copying the text from the `xr` block for consistency.

**Verify:** Visual review.

### Step 6: Full test sweep

**Change:** None.

**Verify:**
- `go test ./cmd/diff/...` (includes both XR and comp integration tests).
- `earthly +go-test` if time permits for the full containerized run.

### Step 7: Regression check on XR

**Verify explicitly:** `go test ./cmd/diff -run TestDiffIntegration/EventualStateWithSequencer -v` remains green to confirm the Step 1 gate change didn't break XR.
