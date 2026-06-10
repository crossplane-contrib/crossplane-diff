# Render Engine Migration — REQUIREMENTS

Migrate crossplane-diff from the retired `render.Render()` entry point (Crossplane ≤ v2.2.1) to the new Engine-based API introduced by Crossplane PR #7339 (merged to main as `01d5a09` on 2026-05-04).

## As Is

### Upstream surface we consume today

- `github.com/crossplane/crossplane/v2 v2.2.1` — pinned in `go.mod`
- Single entry point: `render.Render(ctx, log, render.Inputs) (render.Outputs, error)`
  - `Inputs.Functions []pkgv1.Function` — we pass CRs; upstream starts/stops the containers internally on every call
  - `Outputs.Requirements map[string]fnv1.Requirements` — step-keyed map of `Resources` / `ExtraResources` / `Schemas` selectors that functions requested but couldn't resolve
- Docker container reuse annotations are honored by `render.GetRuntimeDocker`:
  - `render.crossplane.io/runtime-docker-name`
  - `render.crossplane.io/runtime-docker-cleanup: Orphan`

### Our wiring

- `cmd/diff/diffprocessor/diff_processor.go:50–51` — `type RenderFunc func(ctx, log, render.Inputs) (render.Outputs, error)`
- `cmd/diff/diffprocessor/diff_processor.go:93` and `cmd/diff/diffprocessor/comp_processor.go:95` — `RenderFunc: render.Render` is the default injected into `ProcessorConfig`
- `cmd/diff/diffprocessor/diff_processor.go:1101` — the **only** production call site; invoked inside `RenderToStableState`'s iteration loop
- `cmd/diff/diffprocessor/diff_processor.go:104–108` — optionally wraps `RenderFunc` with `serial.RenderFunc` mutex for concurrent-safety
- `cmd/diff/serial/serial.go` — generic mutex wrapper around a `RenderFunc`
- `cmd/diff/diffprocessor/requirements_provider.go:113` — `ProvideRequirements(ctx, map[string]v1.Requirements, namespace) ([]*un.Unstructured, error)`
  - Flattens the step map and calls `processSelector` per selector; `stepName`/`resourceKey` are used only for debug logging
- `cmd/diff/diffprocessor/function_provider.go:140–166` — `CachedFunctionProvider` annotates each Function CR with the two Docker-reuse annotations before handing them to render
- Tests inject custom `RenderFunc` closures via `WithRenderFunc` (diffprocessor/*_test.go); roughly 20+ sites

### Requirements-iteration loop (today)

`RenderToStableState` calls `RenderFunc` in a bounded loop (default 20 iterations). After each render it:
1. Reads `output.Requirements` (step-keyed map)
2. Calls `ProvideRequirements` to turn selectors into resources, fetching from cluster when the cache misses
3. Appends newly-fetched resources to `RequiredResources` and iterates until no new requirements appear (or the synthesize-ready stability check passes)

The fatal-render-error gate at `diff_processor.go:1120` was simplified in #295 and now reads just `if renderErr != nil && newReqCount == 0`.

## To Be

### Upstream surface we will consume

- `github.com/crossplane/crossplane/v2 @01d5a09` (pseudo-version via `go get`) until a tagged release includes PR #7339
- Engine pattern: caller constructs an `render.Engine` (Docker by default), manages function-runtime lifecycle explicitly, and calls `engine.Render(ctx, *renderv1alpha1.RenderRequest)`
- Helper functions we use: `render.BuildCompositeRequest`, `render.ParseCompositeResponse`, `render.StartFunctionRuntimes`, `render.StopFunctionRuntimes`
- Output type: `render.CompositionOutputs` with `CompositeResource`, `ComposedResources`, `RequiredResources []*fnv1.ResourceSelector`, `RequiredSchemas []*fnv1.SchemaSelector`, `Results`, `Context`

### Our wiring

- New file `cmd/diff/diffprocessor/render_engine.go`:
  - `RenderFn` — new render-function type whose shape hides engine/FunctionAddresses state from callers
  - `RenderInputs` — our input struct; `Functions []pkgv1.Function` stays in, `FunctionAddrs` does **not** surface to callers
  - `engineRenderFn` — default implementation holding the `render.Engine`, `FunctionAddresses`, network cleanup handle, and an internal mutex for serialization
- `RequirementsProvider.ResolveSelectors(ctx, []*fnv1.ResourceSelector, namespace)` replaces `ProvideRequirements`
- `RenderToStableState` checks `len(output.RequiredResources) > 0` and calls `ResolveSelectors`
- `cmd/diff/serial/` is deleted — serialization now lives inside `engineRenderFn`
- `ProcessorConfig.RenderMutex` and `WithRenderMutex` are removed; the mutex is implementation-internal
- `CachedFunctionProvider` is **unchanged** — annotations are still honored by `runtime_docker.go` (PR #7339 does not touch that file)

### Behaviour preserved end-to-end

- Iterative requirements resolution converges the same way (selectors → cluster fetch → re-render)
- Docker container reuse across XRs in `comp` diff mode still works
- One global render in flight at a time (mutex-serialized)
- Clean shutdown: containers and networks owned by the engine state are released in `Processor.Cleanup`

## Requirements

### R1. Dependency bump
Pull PR #7339 into `go.mod` via `go get github.com/crossplane/crossplane/v2@01d5a09`; `go mod tidy` succeeds; all transitive `sig.k8s.io/*` and proto deps resolve.

**Acceptance:**
- `go build ./...` passes
- `go mod tidy` leaves no unused requires
- `go list -m github.com/crossplane/crossplane/v2` reports a pseudo-version derived from `01d5a09`

### R2. New render abstraction
Introduce `RenderFn`, `RenderInputs`, and `engineRenderFn` in a new file `cmd/diff/diffprocessor/render_engine.go`. `RenderInputs` must not leak `FunctionAddrs` or engine state to callers; all engine/runtime lifecycle is internal to `engineRenderFn`.

**Acceptance:**
- `type RenderFn func(ctx, log, RenderInputs) (render.CompositionOutputs, error)` exists and is exported
- `RenderInputs` fields: `CompositeResource`, `Composition`, `Functions`, `FunctionCredentials`, `ObservedResources`, `RequiredResources`, `RequiredSchemas`
- `engineRenderFn.Render` satisfies `RenderFn` and is the default injected into `ProcessorConfig.RenderFunc`
- `engineRenderFn.Cleanup(ctx)` stops runtimes, tears down the network, and is idempotent

### R3. Serialization preserved, `serial/` removed
`engineRenderFn` serializes concurrent renders internally. The `cmd/diff/serial/` package is deleted and nothing references it.

**Acceptance:**
- Two goroutines calling `engineRenderFn.Render` concurrently never enter `engine.Render` at the same time (verified via unit test with a blocking fake engine)
- `git grep "cmd/diff/serial"` returns no hits in the final diff
- `cmd/diff/serial/` directory no longer exists

### R4. One engine per processor tree
The `comp_processor.go` nested `DiffProcessor` shares the same `engineRenderFn` instance as its parent — function runtimes are started once and reused across every XR in the comp diff run.

**Acceptance:**
- Unit test: `NewCompDiffProcessor` wired with a fake engine records exactly one `Setup` + `StartFunctionRuntimes` call across N inner XR renders for N ≥ 2
- E2E: a `comp` diff touching ≥ 2 XRs does not restart function containers between XRs (verified via `docker inspect` `StartedAt` timestamp)

### R5. Requirements loop uses the new selector list
`RenderToStableState` reads `output.RequiredResources` (the new flat `[]*fnv1.ResourceSelector`). `RequirementsProvider` exposes `ResolveSelectors(ctx, []*fnv1.ResourceSelector, namespace) ([]*un.Unstructured, error)`; the old `ProvideRequirements(map[string]v1.Requirements, ...)` method is removed.

**Acceptance:**
- `RenderToStableState` returns `lastOutput` when `len(output.RequiredResources) == 0` **and** stability checks pass
- Fatal-render-error gate at :1120 reads `if renderErr != nil && newReqCount == 0` — unchanged in shape, just feeds off the new field
- `ResolveSelectors` preserves the cache semantics of `processSelector` (same hit/miss behaviour and cache population)
- A render cycle that loops three times (two iterations producing new selectors, a third with zero) still converges and returns the third output

### R6. Docker container reuse annotations still honored
`CachedFunctionProvider` continues to annotate Function CRs with `runtime-docker-name` / `runtime-docker-cleanup`. `engineRenderFn.Render` passes those annotated Functions through to `StartFunctionRuntimes` unmodified.

**Acceptance:**
- Existing `CachedFunctionProvider` unit tests pass unchanged
- E2E: after two comp diffs against the same composition, `docker ps -a --filter name=<annotation-name>` shows **one** container per function, reused
- No changes to `function_provider.go` in the final diff

### R7. ProcessorConfig surface is updated
`ProcessorConfig.RenderFunc` is retyped to `RenderFn`. `WithRenderFunc` is retyped. `RenderMutex`/`WithRenderMutex` are removed. `RequirementsProvider` factory signature is updated to take the new `RenderFn`.

**Acceptance:**
- `go vet ./cmd/diff/...` clean
- No call sites reference `sync.Mutex` in `ProcessorConfig` or `WithRenderMutex`
- Existing `WithRenderFunc(...)` usages in tests compile after their closure signatures are updated

### R8. Test mocks migrated
Every `WithRenderFunc(...)` closure in the test suite is updated to the new signature. Fakes for `MockRequirementsProvider` are updated to mirror `ResolveSelectors`. Test expectations that construct `render.Outputs{Requirements: ...}` are rewritten to `render.CompositionOutputs{RequiredResources: ...}`.

**Acceptance:**
- `cd cmd/diff && go test ./...` passes
- No remaining references to `render.Outputs` in the codebase (except possibly a legacy doc/comment — grep and purge)

### R9. E2E parity
All existing E2E tests pass against the new engine. Tests that exercise requirements resolution (env-configs, extra-resources) and tests that exercise container reuse (comp diff) explicitly pass.

**Acceptance:**
- `earthly -P +e2e --FLAGS="-test.run TestDiffXR"` passes
- `earthly -P +e2e --FLAGS="-test.run TestCompositionDiff"` passes
- At least one test that drives the env-configs/extra-resources requirements loop passes end-to-end
- `earthly -P +e2e-matrix` passes (main + release-1.20 or whichever versions are wired)

### R10. Earthfile compatibility
If the new Crossplane SHA needs a fresh CRD snapshot, `earthly +fetch-crossplane-cluster --CROSSPLANE_IMAGE_TAG=main` regenerates cleanly and the `fetch-crossplane-clusters` target includes it.

**Acceptance:**
- `ls cluster/main/crds/` exists and is populated
- `earthly +fetch-crossplane-clusters` completes without error
- E2E matrix run uses the updated tags

## Testing Plan

TDD order: each test precedes the code that makes it pass. Tests live alongside the code they exercise unless noted.

### T1. `engineRenderFn` happy path (unit)
**File**: `cmd/diff/diffprocessor/render_engine_test.go` (new)
**Covers**: R2
**Fake**: a stub `render.Engine` + `FunctionAddresses` substitute. Since upstream exposes these as concrete types, we inject via a thin seam: `engineRenderFn` gets a private `newEngine func(logging.Logger) render.Engine` field (defaulting to the real one) so tests can plug in a fake. Likewise for `startFunctionRuntimes`.
**Assertions**:
- First `Render` call invokes `Setup` exactly once, then `StartFunctionRuntimes` exactly once
- Second `Render` call invokes neither again (runtimes reused)
- `Render` builds a request via `BuildCompositeRequest` — we verify by having the fake engine capture the incoming `*RenderRequest` and assert its `GetComposite().FunctionAddrs` matches the fake's `Addresses()`
- `Render` returns the `CompositionOutputs` parsed from the fake's response

### T2. `engineRenderFn.Cleanup` idempotence (unit)
**File**: same
**Covers**: R2
**Assertions**:
- After a successful render, `Cleanup` calls `StopFunctionRuntimes` once and the network-cleanup closure once
- A second `Cleanup` call is a no-op (no panic, no second Stop)
- `Cleanup` after zero renders is a no-op

### T3. Serialization (unit)
**File**: same
**Covers**: R3
**Assertions**:
- Inject a fake engine whose `Render` blocks on a channel. Kick off two goroutines calling `engineRenderFn.Render`. Verify only one goroutine reaches the fake at a time (via an atomic counter); second one enters only after the first returns.

### T4. One-engine-per-processor-tree (unit)
**File**: `cmd/diff/diffprocessor/comp_processor_test.go` (add case)
**Covers**: R4
**Assertions**:
- Build a `CompDiffProcessor` backed by a counting fake engine. Run a comp diff that produces 3 inner XRs. Assert the fake saw **exactly one** `Setup` and **one** `StartFunctionRuntimes` across all three.

### T5. `ResolveSelectors` replaces `ProvideRequirements` (unit)
**File**: `cmd/diff/diffprocessor/requirements_provider_test.go`
**Covers**: R5
**Assertions**:
- `ResolveSelectors(ctx, nil, "ns")` returns `(nil, nil)`
- Empty-slice input returns `(nil, nil)`
- Given two selectors where one hits the cache and one must be fetched, the method fetches once, caches the newly-fetched one, and returns both resources
- Error from the underlying `processSelector` propagates up

### T6. RenderToStableState iterates on the new field (unit)
**File**: `cmd/diff/diffprocessor/diff_processor_test.go`
**Covers**: R5
**Assertions**:
- A fake `RenderFn` returns `CompositionOutputs{RequiredResources: []*fnv1.ResourceSelector{one selector}}` on iteration 1, then empty on iteration 2. `RenderToStableState` iterates twice and returns the second output.
- A fake that never returns an empty `RequiredResources` hits the iteration ceiling and errors with the existing message
- Fatal render error + zero new requirements still fails fast (existing behavior preserved)

### T7. Docker annotations survive the pipeline (unit)
**File**: `cmd/diff/diffprocessor/function_provider_test.go` (existing; add case)
**Covers**: R6
**Assertions**:
- `CachedFunctionProvider.GetFunctionsForComposition` still emits the two annotations (existing tests)
- New: a canary test in `render_engine_test.go` verifies `engineRenderFn.Render` passes the annotated Functions through to its `startFunctionRuntimes` seam unmodified

### T8. ProcessorConfig surface (unit)
**File**: `cmd/diff/diffprocessor/processor_config_test.go` (existing; update)
**Covers**: R7
**Assertions**:
- Compile-time: `ProcessorConfig.RenderFunc` is typed as `RenderFn`
- `WithRenderFunc(fn)` applies the provided fn
- `WithRenderMutex` no longer exists (compile check — remove its test)

### T9. Bulk mock migration (unit)
**File**: all `cmd/diff/diffprocessor/*_test.go`
**Covers**: R8
**Assertions**:
- Every test that previously built `render.Outputs{...}` now builds `render.CompositionOutputs{...}`
- No reference to `render.Inputs` or `render.Outputs` remains in the test suite

### T10. Integration smoke (integration)
**File**: `cmd/diff/diff_integration_test.go` (envtest-based, existing)
**Covers**: R1, R2, R5, R7, R8 end-to-end
**Assertions**:
- The existing integration tests (e.g. `TestDiffIntegrationForExistingXRWithComposedResources`) still pass against the new engine without modification

### T11. E2E parity (e2e)
**Covers**: R9
**Command**: `earthly -P +e2e --FLAGS="-v=4 -test.run TestDiff"` (run each suite incrementally first, then full)
**Manual verification**: `docker ps -a --filter name=function-` after a comp diff shows the named containers; stop them manually, re-run, observe reuse

### T12. Earthfile check (smoke)
**Covers**: R10
**Command**: `earthly +fetch-crossplane-cluster --CROSSPLANE_IMAGE_TAG=main` — confirm `cluster/main/crds/` is populated. `earthly +build` — binary still builds from the new deps.

## Implementation Plan

Smallest-possible sequential steps. Each step lists the test(s) that validate it.

### S1. Bump dependency
**Action**: `go get github.com/crossplane/crossplane/v2@01d5a09 && go mod tidy`. Do **not** edit code yet; expected compile errors (old `render.Render`, `render.Inputs`, `render.Outputs`, old `RequirementsProvider` types) are evidence of the API change. Pin the go.mod pseudo-version; commit on a scratch branch if needed to reach a red baseline we can reason about.
**Test**: `go build ./...` — capture the error surface as a checklist. (Covers R1 gate.)
**Rollback**: revert go.mod/go.sum.

### S2. Refresh CRD snapshot tag
**Action**: `earthly +fetch-crossplane-cluster --CROSSPLANE_IMAGE_TAG=main`. Only update Earthfile if the `fetch-crossplane-clusters` list needs main added (it already does, per current Earthfile:11).
**Test**: `ls cluster/main/crds/` is populated; `earthly +fetch-crossplane-cluster --CROSSPLANE_IMAGE_TAG=main` completes clean. (T12)

### S3. Write `render_engine_test.go` (T1/T2/T3) — tests first
**Action**: Create `cmd/diff/diffprocessor/render_engine_test.go` with T1 happy-path, T2 cleanup idempotence, T3 serialization, using fake-engine seams. Tests **will not compile yet** because the production types don't exist — that's expected. Capture the desired call shape here.
**Test**: `go test ./cmd/diff/diffprocessor -run TestEngineRenderFn 2>&1` — expect compile errors naming the missing types; that's the "red" we want.

### S4. Create `render_engine.go` (R2)
**Action**: Add the new file with `RenderFn`, `RenderInputs`, `engineRenderFn`, `NewEngineRenderFn`, `.Render`, `.Cleanup`. Include test seams for `newEngine` and `startFunctionRuntimes`. Do not wire into the processor yet.
**Test**: `go test ./cmd/diff/diffprocessor -run TestEngineRenderFn -v` — T1, T2, T3 pass. `go vet` clean.

### S5. Add `ResolveSelectors` to `RequirementsProvider` alongside existing (T5)
**Action**: Add `ResolveSelectors(ctx, []*fnv1.ResourceSelector, namespace)` that wraps `processSelector` in a flat loop. Do **not** remove `ProvideRequirements` yet — the call site still uses it.
**Test**: `go test ./cmd/diff/diffprocessor -run TestResolveSelectors -v` — T5 passes. Existing tests still pass.

### S6. Adapt `RenderToStableState` to translate between shapes (temporarily)
**Action**: Inside `RenderToStableState`, between the `RenderFunc` call and `ProvideRequirements`, **temporarily** synthesize a single-key `map[string]fnv1.Requirements{"engine": {Resources: out.RequiredResources-as-map}}` — just so we can swap the RenderFunc type next without touching everything in one step. Also switch the fatal-error check to `output.RequiredResources` (field rename only).
**Test**: Existing diff_processor tests still pass with stub RenderFn returning new shape. We’ll assert the new shape flows through once the RenderFunc type changes.

Actually revise: this step is too intertwined. **Skip S6 and roll its work into S7/S8/S9** to avoid a throwaway translation layer.

### S6 (revised). Retype `ProcessorConfig.RenderFunc` and delete `serial/` in a single flip
**Action**: 
- Retype `ProcessorConfig.RenderFunc` to the new `RenderFn`.
- Retype `WithRenderFunc`.
- Remove `RenderMutex` field, `WithRenderMutex` option, and the wrap at `diff_processor.go:104–108`.
- Update `RequirementsProvider` factory signature (`processor_config.go:86`, `226`) to accept the new `RenderFn`.
- Delete `cmd/diff/serial/` directory and all its references.
- Build will break at the call site and in tests — that's S7 and S9.

**Test**: `go build ./cmd/diff/...` — error surface shrinks to (a) the call site at :1101, (b) `requirements_provider.go`'s use of `renderFn`, (c) test mocks. (T8 compile-level.)

### S7. Update the call site at `diff_processor.go:1101` (R5)
**Action**: 
- Replace `render.Inputs{...}` with `RenderInputs{...}` at :1101.
- Read `output.RequiredResources` instead of `output.Requirements` throughout `RenderToStableState` and `checkStability`.
- Replace `p.requirementsProvider.ProvideRequirements(ctx, output.Requirements, ...)` with `p.requirementsProvider.ResolveSelectors(ctx, output.RequiredResources, ...)`.
- Fatal-error gate at :1120 stays textually the same (it already dropped the `len(output.Requirements) == 0` condition in #295), but now sits over the new field semantically.
- `lastOutput` type changes from `render.Outputs` to `render.CompositionOutputs`; update return type of `RenderToStableState` accordingly.

**Test**: 
- Rewrite `diff_processor_test.go` closures that build `render.Outputs` to build `render.CompositionOutputs`. This is mechanical but large — do it in one pass.
- `go test ./cmd/diff/diffprocessor -run TestDefaultDiffProcessor -v` — T6 passes and existing iteration/stability tests pass.

### S8. Remove the now-unused `ProvideRequirements`
**Action**: Delete `ProvideRequirements` from `RequirementsProvider` (the step-map form). Delete its mock in `testutils`.
**Test**: `go vet` clean; `go test ./cmd/diff/...` still green.

### S9. Wire `engineRenderFn` into `NewDiffProcessor` and `NewCompDiffProcessor` defaults (R4)
**Action**: 
- In `NewDiffProcessor` (`diff_processor.go:86–94`), default `config.RenderFunc = NewEngineRenderFn(config.Logger).Render` and store a handle to the `engineRenderFn` on the processor so `Cleanup` can call it.
- In `NewCompDiffProcessor`, if the inner `DiffProcessor` was already passed in with a `RenderFunc`, respect it; otherwise create one `engineRenderFn` shared between inner + outer (pass via `WithRenderFunc` and share via struct capture).
- Wire `engineRenderFn.Cleanup` into `DefaultDiffProcessor.Cleanup` and `DefaultCompDiffProcessor.Cleanup`.

**Test**: T4 passes. `earthly +go-test` passes. Integration smoke T10 passes.

### S10. Update any remaining test mocks touched by shape change (R8)
**Action**: Sweep `cmd/diff/diffprocessor/*_test.go` and `cmd/diff/testutils/*.go`:
- Replace `render.Inputs` with `RenderInputs` in closure signatures
- Replace `render.Outputs` with `render.CompositionOutputs` in return expressions
- Any `MockRequirementsProvider.ProvideRequirementsFn` → `ResolveSelectorsFn`

**Test**: `earthly +go-test` fully green.

### S11. Pre-flight: build binary, run focused E2E
**Action**: `earthly +build`; then `earthly -P +e2e --FLAGS="-v=4 -test.run TestDiffXR"`; then `earthly -P +e2e --FLAGS="-v=4 -test.run TestCompositionDiff"`. Check for orphaned containers between runs with `docker ps -a`.
**Test**: T11.

### S12. Full E2E matrix + reviewable gate
**Action**: `earthly -P +e2e-matrix` then `earthly -P +reviewable`.
**Test**: All pass. Any ANSI golden file drift gets regenerated via `E2E_DUMP_EXPECTED=1` and reviewed.

### S13. Clean-up sweep
**Action**: `git grep "render.Inputs\|render.Outputs\|ProvideRequirements\|RenderMutex\|cmd/diff/serial"` must return zero hits in source files (may survive in commit messages or CHANGELOG if any).
**Test**: grep returns empty; final `earthly +go-test` green.

### Rollback strategy (all steps)
- Keep each logical step in its own commit on the `render-engine` branch.
- If any step fails in a way we can't quickly diagnose, revert the relevant commit — no cross-step coupling that would make partial rollback painful.
- If after S1 we find the Crossplane API doesn't behave as documented, `git checkout go.mod go.sum` pins us back to v2.2.1 and nothing else is impacted.

