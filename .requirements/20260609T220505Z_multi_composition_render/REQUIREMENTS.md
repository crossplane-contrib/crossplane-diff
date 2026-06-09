# Multi-Composition Render Support in EngineRenderFn

## As Is

`EngineRenderFn` is the default `RenderFn` implementation backing every render
performed by `DefaultDiffProcessor`. It owns one render engine + one set of
function runtime addresses for the lifetime of the processor.

Today's behaviour (post crossplane/cli v2.3.2 bump):

```go
func (e *EngineRenderFn) Render(ctx, log, in) (...) {
    e.mu.Lock(); defer e.mu.Unlock()

    if !e.started {
        cleanup, _ := e.engine.Setup(ctx, in.Functions)        // (1)
        e.networkCleanup = cleanup
        fnAddrs, _ := e.startRuntimes(ctx, log, in.Functions)  // (2)
        e.fnAddrs = fnAddrs
        e.started = true
    }

    req, _ := render.BuildCompositeRequest(render.CompositionInputs{
        FunctionAddrs: e.fnAddrs.Addresses(),  // (3) always the first batch's addrs
        ...
    })
    rsp, _ := e.engine.Render(ctx, req)
    ...
}
```

Single-composition flow works correctly: Setup creates a docker network N,
annotates `in.Functions` with N (via upstream's `injectNetworkAnnotation`),
StartFunctionRuntimes brings their containers up on N, addresses are cached.

**The bug.** When `DefaultDiffProcessor` processes multiple XRs in one
invocation that resolve to *different* compositions with overlapping but
non-identical function pipelines:

- Composition A is rendered first with `Functions=[F1, F2]` → Setup creates N,
  annotates F1+F2 with N, runtimes for F1+F2 started on N. `e.fnAddrs = {F1, F2}`.
- Composition B is rendered next with `Functions=[F1, F3]`. `e.started` is
  true → Setup is skipped → F3 is **never annotated with N**. The cached
  `e.fnAddrs` doesn't contain F3 either, so `BuildCompositeRequest` is called
  with `FunctionAddrs={F1, F2}` → the binary has no address for F3. If F3
  *is* started elsewhere it lands on the default Docker bridge network and is
  unreachable from the render container.

This is a real (if narrow) regression vs. the pre-EngineRenderFn world where
`render.Render()` was called once per XR and runtimes started fresh each time.

The realistic case is `crossplane-diff xr xr1.yaml xr2.yaml` against XRs from
different compositions (e.g., diffing a GitOps directory).

## To Be

`EngineRenderFn.Render` correctly handles renders whose `in.Functions` differs
across calls. Specifically:

1. Setup is still called exactly once (upstream's `dockerRenderEngine.Setup`
   creates a new network on every call → calling it twice would leak networks
   in v2.3.2).
2. After the first Setup, the engine captures the network name from the
   annotations upstream's Setup stamps onto the first batch of functions.
3. On subsequent renders, any function in `in.Functions` whose name has not
   yet been started is:
   - Annotated with the captured network name (so when its container is
     created by `StartFunctionRuntimes`, it joins the existing network).
   - Started via `StartFunctionRuntimes`.
   - Its address merged into the engine's cached address map.
4. Already-running functions are skipped (no redundant Start).
5. `BuildCompositeRequest` is given `FunctionAddrs` filtered to exactly the
   functions referenced by *this* render's composition.
6. `Cleanup` stops every runtime started across all renders, then runs the
   network cleanup.

## Requirements

1. **R1: First-Setup-only network creation.** `engine.Setup` MUST be called at
   most once over `EngineRenderFn`'s lifetime, regardless of how many `Render`
   invocations occur.
2. **R2: Network name capture.** After the first Setup call returns,
   `EngineRenderFn` MUST extract the value of the
   `render.AnnotationKeyRuntimeDockerNetwork` annotation from any of the
   functions passed to that Setup call and retain it for later use.
3. **R3: New-function annotation.** On any `Render` call after the first,
   for every function in `in.Functions` whose name is not already in the
   engine's started-functions set, `EngineRenderFn` MUST set the
   `render.AnnotationKeyRuntimeDockerNetwork` annotation to the captured
   value before passing the function to `StartFunctionRuntimes` — except
   when the function already has a non-empty value for that annotation, in
   which case the existing value MUST be preserved.
4. **R4: Started-function deduplication.** `StartFunctionRuntimes` MUST be
   called only with functions whose names are not yet in the engine's
   started-functions set.
5. **R5: Address map accumulation.** Address entries returned by every
   `StartFunctionRuntimes` call MUST be merged into a single map; no entry
   is overwritten by a later call.
6. **R6: Per-render address subset.** The `FunctionAddrs` passed to
   `BuildCompositeRequest` MUST be a map whose keys are exactly the names of
   the functions in `in.Functions` (filtered from the accumulated map).
7. **R7: Cleanup runs all stops.** `Cleanup` MUST invoke `stopRuntimes` for
   every `*FunctionAddresses` ever returned from `StartFunctionRuntimes`,
   then invoke the network cleanup function.
8. **R8: Backwards compatibility for single-composition.** A single-composition
   invocation (the common case) MUST exhibit the same observable behaviour
   as before this change: one Setup, one StartFunctionRuntimes for the
   composition's full function set, one stopRuntimes per cleanup.
9. **R9: Concurrent-render safety preserved.** The `sync.Mutex` serialization
   MUST continue to guarantee that no two `Render` calls run concurrently
   inside the engine, including across the new annotate-and-start path.
10. **R10: Cleanup idempotency preserved.** `Cleanup` called twice MUST run
    its body once; called before any `Render` MUST be a no-op.

## Acceptance Criteria

- **AC1 (R1, R8):** Single-render unit test asserts `engine.Setup` and
  `startRuntimes` were each invoked exactly once.
- **AC2 (R1, R4, R8):** Two-render same-functions unit test asserts
  `engine.Setup` was invoked once and `startRuntimes` was invoked once
  (second render reuses everything).
- **AC3 (R2, R3, R4, R5, R6):** Multi-composition unit test renders with
  `[F1, F2]` then `[F1, F3]` then `[F1, F2]` again. Asserts:
  - `startRuntimes` invoked exactly twice across all renders.
  - First call had functions `{F1, F2}`, second had `{F3}`, third never happens.
  - F3 carries the network annotation captured from the first batch.
- **AC4 (R3 preservation clause):** Multi-composition unit test where F3
  arrives with a pre-set network annotation `"custom"` asserts that F3's
  annotation remains `"custom"` after EngineRenderFn's annotate step.
- **AC5 (R7):** Cleanup test renders with `[F1, F2]` then `[F1, F3]`, then
  calls `Cleanup`. Asserts `stopRuntimes` is invoked twice (once per
  `*FunctionAddresses`).
- **AC6 (R9):** Existing serialization test (`TestEngineRenderFn_Serialization`)
  continues to pass.
- **AC7 (R10):** Existing cleanup-idempotency test
  (`TestEngineRenderFn_CleanupIdempotent`) continues to pass.
- **AC8 (correctness):** Full unit + integration test pass against the change
  (single-composition integration tests cover the don't-regress case;
  multi-composition is unit-test-only since integration tests against a
  shared composition).

## Testing Plan

- **Unit tests** in `cmd/diff/diffprocessor/render_engine_test.go`:
  - `TestEngineRenderFn_HappyPath` (existing) — keep, verifies AC1+AC2.
  - `TestEngineRenderFn_CleanupIdempotent` (existing) — keep, verifies AC7
    and re-verifies after the refactor.
  - `TestEngineRenderFn_Serialization` (existing) — keep, verifies AC6.
  - **NEW** `TestEngineRenderFn_MultiCompositionFunctionSet` — verifies AC3.
  - **NEW** `TestEngineRenderFn_PreservesExistingNetworkAnnotation` — verifies
    AC4.
  - **NEW** `TestEngineRenderFn_CleanupStopsAllFunctionAddresses` — verifies
    AC5.
- **Integration smoke** (`cmd/diff` test package) — single-composition
  integration tests must continue to pass post-refactor (AC8).
- **No E2E changes** — multi-composition `xr` invocation is not currently
  exercised in E2E. Filing a follow-up to add such a test is a noted
  limitation but not in scope.

## Implementation Plan

Each step is the smallest change that makes a single test pass.

### Step 1: Fix the AnnotationKeyRuntimeDockerNetwork import in the existing
test file

The test file already references `AnnotationKeyRuntimeDockerNetwork`
unqualified — it must be `render.AnnotationKeyRuntimeDockerNetwork`. This
also resolves the diagnostic that's currently making the package fail to
build. **Verify**: `go build ./diffprocessor/...` succeeds.

### Step 2: Make the multi-composition test compile and fail meaningfully

Run the new `TestEngineRenderFn_MultiCompositionFunctionSet`. With the
current production code it should fail (or panic) because:
- The mock's `MockSetup` annotates the first batch with `networkName`.
- The current EngineRenderFn caches `fnAddrs` from the first
  `startRuntimes` call (containing only F1+F2's empty FunctionAddresses).
- The second render with `[F1, F3]` does NOT call startRuntimes again
  (because `e.started == true`), so F3 is never started. The test asserts
  startRuntimes was called twice → fails.

**Verify**: `go test -run TestEngineRenderFn_MultiCompositionFunctionSet`
fails with the expected diagnostic about call count.

### Step 3: Restructure EngineRenderFn state

In `cmd/diff/diffprocessor/render_engine.go`, replace the single
`fnAddrs *render.FunctionAddresses` field with:
- `fnAddrsList []*render.FunctionAddresses` — every result returned by
  `startRuntimes`. Iterated by `Cleanup`.
- `addrs map[string]string` — accumulated address map, keyed by function
  name. Used to filter `BuildCompositeRequest`'s FunctionAddrs.
- `networkName string` — captured from first Setup.

`started` boolean stays.

**Verify**: `go build` succeeds. Existing tests that referenced the old
field need updating to match the new shape (deferred to step 5 if
needed).

### Step 4: Restructure Render() flow

```
newFns := slice of in.Functions whose name is NOT already a key in e.addrs
if !e.started:
    cleanup, err := e.engine.Setup(ctx, newFns)
    if err: return wrapped
    e.networkCleanup = cleanup
    if len(newFns) > 0:
        e.networkName = newFns[0].Annotations[render.AnnotationKeyRuntimeDockerNetwork]
    e.started = true
else if len(newFns) > 0 and e.networkName != "":
    for each newFn:
        if newFn doesn't have a non-empty AnnotationKeyRuntimeDockerNetwork:
            set it to e.networkName
if len(newFns) > 0:
    fa, err := e.startRuntimes(ctx, log, newFns)
    if err: unwind cleanup if first call, return wrapped
    e.fnAddrsList = append(e.fnAddrsList, fa)
    for k, v := range fa.Addresses(): e.addrs[k] = v

req := BuildCompositeRequest(FunctionAddrs: subset(e.addrs, in.Functions), ...)
```

**Verify**: `TestEngineRenderFn_MultiCompositionFunctionSet` passes.
Existing `TestEngineRenderFn_HappyPath` still passes.

### Step 5: Restructure Cleanup()

```
for fa := range e.fnAddrsList:
    e.stopRuntimes(e.log, fa)
e.fnAddrsList = nil
e.addrs = nil
e.networkName = ""
if e.networkCleanup != nil:
    e.networkCleanup()
    e.networkCleanup = nil
e.started = false
```

**Verify**: `TestEngineRenderFn_CleanupIdempotent` passes (cleanup before
render is no-op; first cleanup runs once; second cleanup is no-op).

### Step 6: Add `TestEngineRenderFn_PreservesExistingNetworkAnnotation`

Test where in.Functions[0] arrives with a pre-set network annotation.
Asserts that EngineRenderFn does not overwrite it.

**Verify**: new test passes.

### Step 7: Add `TestEngineRenderFn_CleanupStopsAllFunctionAddresses`

Test that after multi-comp renders, Cleanup invokes stopRuntimes for each
FunctionAddresses entry.

**Verify**: new test passes.

### Step 8: Run full unit + integration tests

`(cd cmd/diff && go test ./... && go test . -run TestDiffIntegration)` —
no regressions.

### Step 9: Add a workaround comment

Add a short comment near the new annotate logic pointing at:
- The upstream issue we'll file (link TBD until it's filed).
- The future PR that will unwind this.

### Step 10: File the upstream issue + the self-tracking unwind issue

External to the code change, but blocks the "completion" of this work:
- crossplane/cli upstream issue describing the multi-call Setup gap and
  proposing a clean API (idempotent Setup or `Engine.AnnotateFunctions`).
- crossplane-contrib/crossplane-diff issue tracking the unwind work
  (delete `networkName` capture + manual annotate path) once the upstream
  fix ships, with a dependency map.
