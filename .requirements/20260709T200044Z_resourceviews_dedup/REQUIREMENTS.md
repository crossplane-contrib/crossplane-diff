# Refactor: ResourceViews (raw/clean) + generation-side cleanup dedup

## Context

Follow-up to the `--ignore-paths` structured-output fix (PR #370). Copilot review
clustered on a seam: the structured renderer re-runs `cleanupForDiff` (which we
suppress with a no-op logger). Root cause: cleanup is duplicated between diff
generation and structured rendering, and the human vs. structured renderers are
asymmetric (human gets pre-baked `LineDiffs`; structured re-cleans raw objects).

## As Is

- `ResourceDiff` carries `Current *un.Unstructured` and `Desired *un.Unstructured`
  (the raw, uncleaned objects).
- `GenerateDiffWithOptions` (diff_formatter.go), on the **modified** path, cleans
  each object **twice**: once for the after-cleanup equality check (lines 313-314,
  result discarded) and again inside `asString` (line 331) to marshal YAML for
  `LineDiffs`. Added/removed paths clean once (only `asString`).
- The structured renderer (`buildDiffDetail`, `resourceDiffToChangeDetail`,
  `buildDownstreamChanges`) re-runs `cleanupForDiff` on the raw objects at render
  time, threading `ignorePaths` and a no-op `renderCleanupLogger()` to suppress
  duplicate Debug logging.
- Raw objects are load-bearing beyond rendering:
  - `diff_calculator.go:300` — `composite := xrDiff.Current` (removal detection)
  - `diff_processor.go:443,447` — reconstruct existing XR from `xrDiff.Current`
  - `structured_renderer.go:268-271` — namespace extraction (either side)

## To Be

- New type `ResourceViews { Raw, Clean *un.Unstructured }` in renderer/types.
- `ResourceDiff.Current` and `.Desired` become `ResourceViews`.
  - `Raw` = the as-rendered / from-cluster object (what `Current`/`Desired` are today).
  - `Clean` = the post-`cleanupForDiff` object, populated by generation for the
    non-equal modified/added/removed cases; `nil` when the diff is equal (nothing
    to render).
- `GenerateDiffWithOptions` cleans each present object **once**, reuses the cleaned
  copy for both the equality check and YAML marshaling, and stores it in
  `.Clean`. Net: modified path 4→2 cleanups; added/removed unchanged (1 each).
- Structured renderer becomes a pure formatter: reads `diff.Current.Clean` /
  `diff.Desired.Clean` directly. Deletes all render-time `cleanupForDiff` calls,
  the `ignorePaths` params on `resourceDiffToChangeDetail`/`buildDownstreamChanges`,
  and `renderCleanupLogger()`.
- Human renderer unaffected (still consumes `LineDiffs`).

## Requirements

**R1.** `ResourceViews` type with `Raw` and `Clean` `*un.Unstructured` fields.
- AC1.1: `ResourceDiff.Current` and `.Desired` are of type `ResourceViews`.

**R2.** Generation populates both views.
- AC2.1: For a modified (non-equal-after-cleanup) diff, `Current.Raw`/`Desired.Raw`
  are the originals and `Current.Clean`/`Desired.Clean` are the cleaned copies.
- AC2.2: For added, `Desired.Raw`+`Desired.Clean` set, `Current` zero-value
  (both nil). For removed, symmetric.
- AC2.3: For equal diffs (`equalDiff`), `Raw` set on both sides, `Clean` nil
  (renderer skips equal diffs, so clean is never read).

**R3.** No redundant cleanup.
- AC3.1: On the modified path, `cleanupForDiff` is invoked at most once per object
  (verified by reading the code; each object cleaned once and reused).
- AC3.2: The structured renderer calls `cleanupForDiff` zero times.

**R4.** Structured output unchanged (behavioral equivalence).
- AC4.1: All existing structured-renderer tests pass unchanged in expectation
  (ignored paths + server-side fields still absent from `diff.old/new/spec`;
  summary counts unchanged).
- AC4.2: `renderCleanupLogger`, and the `ignorePaths`/`logger` params added to
  `resourceDiffToChangeDetail`/`buildDownstreamChanges` in the prior round, are
  removed.

**R5.** Raw consumers keep working.
- AC5.1: `diff_calculator.go` removal detection uses `xrDiff.Current.Raw`.
- AC5.2: `diff_processor.go` XR reconstruction uses `xrDiff.Current.Raw`.
- AC5.3: structured renderer namespace extraction uses `.Raw` and nil-checks
  `.Raw` for existence.

**R6.** Human diff output byte-identical (no golden-file changes).

## Testing Plan

- Unit (renderer): existing `TestStructuredDiffRenderer_RespectsIgnorePaths`,
  `TestGenerateDiffWithOptions`, `TestDefaultDiffRenderer_RenderDiffs`,
  `TestCompDiffOutput_JSONSchema`, `sharedDiffFixtures` — update fixture
  construction to the `ResourceViews` shape; expectations unchanged.
- Unit (diffprocessor): `diff_calculator_test.go`, `diff_processor_test.go` —
  update `wantDiff` fixtures + any `.Current`/`.Desired` access.
- New assertion: a generation test that a modified diff populates `.Clean` on
  both sides and that `.Clean` has ignored/server-side fields stripped while
  `.Raw` retains them (locks the split).
- Full `./cmd/diff/...` incl. the ~95s integration suite.
- Golden ANSI files: must pass untouched (R6).

## Implementation Plan

1. **Add `ResourceViews`** in `renderer/types/types.go`; change `ResourceDiff`
   fields to it. Build → compiler enumerates every break.
2. **Generation** (`diff_formatter.go`): hoist cleaned copies in
   `GenerateDiffWithOptions`, reuse for equality + `asString`, store in `.Clean`;
   set `.Raw` on both. Update `equalDiff` (Raw only). Update the return literal.
3. **Raw consumers**: `diff_calculator.go:300`, `diff_processor.go:443,447`,
   `structured_renderer.go:268-271` → `.Current.Raw` / `.Desired.Raw`.
4. **Structured renderer**: rewrite `buildDiffDetail` + `resourceDiffToChangeDetail`
   to read `.Clean.Object`; drop `ignorePaths` params + `buildDownstreamChanges`
   param; delete `renderCleanupLogger`. Update comp_diff_renderer call sites.
5. **Tests**: update fixtures across the 4 test files; add the raw/clean split
   assertion.
6. **Verify**: `go build ./...`, `go test ./cmd/diff/...`, `golangci-lint run`
   (v2.12.2), confirm golden ANSI untouched.
7. **Commit** as a distinct commit on PR #370 (`refactor(renderer): ...`),
   DCO-signed; it supersedes the no-op-logger approach from the prior round.

## Non-goals / notes

- Raw `Current`/`Desired` are NOT removed — load-bearing for removal detection
  and XR reconstruction. This is additive-then-regrouped, not a deletion.
- Tooling: use Serena symbolic edits (`replace_symbol_body`, `insert_*`) for
  function/struct bodies; let the compiler pin the reshape sites. `.Current` →
  `.Current.Raw` is a type reshape, not a symbol rename, so `rename_symbol`
  doesn't apply to those sites.
