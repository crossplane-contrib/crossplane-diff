# Pin the crossplane render image version/reference (issue #402)

- **Issue:** crossplane-contrib/crossplane-diff#402
- **Branch:** `crossplane-render-version-pin` (worktree, from origin/main @ fba42f6, post-#401)
- **Date (UTC):** 2026-07-23
- **Decisions locked (from #402 discussion + AskUserQuestion):**
  - Surface **both** knobs: `--crossplane-version` (tag) and `--crossplane-image` (full ref), user-facing.
  - `--crossplane-render-binary` stays hidden/test-only. All three mutually exclusive via kong `xor`.
  - **v2.3.4 is a codified hard minimum.** Single source of truth constant in diffprocessor.
  - Enforce the floor by **validating an explicit `--crossplane-version` only** (cheap semver compare) —
    no runtime probe of `:stable` or a custom `--crossplane-image`. Floor check at the **CLI layer**,
    fail-fast before any cluster/render work.
  - `--crossplane-version=auto` (match-the-cluster) is **out of scope** here (deferred; note the existing
    `versioncmd.FetchCrossplaneVersion` helper as the building block if pursued later).
- **Environment note:** the `tdd` skill prescribes Lad MCP review; Lad is unavailable. Substituting
  `earthly +reviewable` + Copilot on the draft PR, as in #401.

---

## 1. As Is

`crossplane-diff` renders via the upstream `crossplane/cli` render engine. `NewEngineRenderFn(log, binaryPath)`
(`render_engine.go`) constructs `render.EngineFlags` with only `CrossplaneBinary` (from the hidden
`--crossplane-render-binary` flag) and `CrossplaneDockerNetwork` (from the `CROSSPLANE_DIFF_DOCKER_NETWORK`
env var). When `CrossplaneBinary` is empty, the docker engine pulls the floating
`xpkg.crossplane.io/crossplane/crossplane:stable`.

Upstream `render.EngineFlags` already defines a mutually-exclusive (`xor:"crossplane-selector"`) selector
with three members — `CrossplaneVersion`, `CrossplaneImage`, `CrossplaneBinary` — but crossplane-diff only
wires `CrossplaneBinary`. There is **no** user-facing way to pin the render version/image, so a floating
`:stable` can change render behavior underneath an invocation (root cause of #399).

Plumbing path today (for the binary override):
`main.go CommonCmdFields.CrossplaneRenderBinary` (kong flag, hidden)
→ `cmd_utils.go defaultProcessorOptions` appends `dp.WithCrossplaneRenderBinary(...)`
→ `processor_config.go` sets `ProcessorConfig.CrossplaneRenderBinary`
→ `diff_processor.go:108` `NewEngineRenderFn(config.Logger, config.CrossplaneRenderBinary)`
→ `render.EngineFlags{CrossplaneBinary: ...}`.

`defaultProcessorOptions(fields) []dp.ProcessorOption` returns no error. Both `xr` and `comp` embed
`CommonCmdFields` and call it. `Masterminds/semver v1.5.0` is already a dep, used in
`internal/versioninfo/version.go` via `semver.NewVersion` / `semver.NewConstraint`.

## 2. To Be

Users can pin the render version or image on `xr` and `comp`:

- `--crossplane-version VERSION` → renders `xpkg.crossplane.io/crossplane/crossplane:VERSION`.
- `--crossplane-image IMAGE` → renders the given full image reference.
- `--crossplane-render-binary PATH` → unchanged (hidden/test-only).
- The three are mutually exclusive (kong `xor` + upstream `xor`); default (none set) stays `:stable`.
- `--crossplane-version` below **v2.3.4** fails fast with a clear CLI error before any cluster/render work.
- README documents the two new flags and the v2.3.4 minimum.

## 3. Requirements

**R1.** Add `--crossplane-version` and `--crossplane-image` to `CommonCmdFields` (surface on both `xr` and
`comp`), user-facing (not hidden), named to match upstream. Add kong `xor` grouping across
`CrossplaneVersion`, `CrossplaneImage`, `CrossplaneRenderBinary` so kong rejects combinations pre-flight.

**R2.** Thread both new fields through the existing plumbing: `defaultProcessorOptions` → new
`ProcessorOption`s (`WithCrossplaneVersion`, `WithCrossplaneImage`) → new `ProcessorConfig` fields →
`NewEngineRenderFn` → `render.EngineFlags{CrossplaneVersion, CrossplaneImage}`.

**R3.** `NewEngineRenderFn` must accept the version and image (in addition to the existing binaryPath) and
set them on `EngineFlags`. Preserve the existing xor semantics (only one of version/image/binary non-empty
in normal use; kong guards user input, upstream guards the engine).

**R4.** Codify `MinCrossplaneRenderVersion = "v2.3.4"` as a single exported constant in the diffprocessor
package. Provide an exported `ValidateMinRenderVersion(version string) error` helper that returns a clear
error when the given version parses and is below the minimum. Non-parseable input: return a clear
"cannot parse" error (defensive; kong passes the raw string).

**R5.** Enforce R4 at the CLI layer, failing fast before cluster/render work. Use a kong `Validate() error`
hook on `CommonCmdFields` (kong invokes it automatically before `Run`) that calls
`dp.ValidateMinRenderVersion(CrossplaneVersion)` when that flag is set. Only `--crossplane-version` is
floor-checked; `--crossplane-image` carries no comparable version and is not checked (documented).

**R6.** README: document `--crossplane-version` / `--crossplane-image` in the command-options section and
state the v2.3.4 minimum (link the rationale to #399's silent-drop behavior on older renders).

**R7 (docs sync).** Per CLAUDE.md triggers: new CLI flags → update design doc §8.1 examples and, if the
new `ProcessorConfig` fields count, §6.1.2. Update README (R6). Evaluate whether a mermaid diagram needs
changes (unlikely — no interface/layer change; the RenderFn seam is unchanged in shape).

## 4. Acceptance Criteria

**AC-R1:** `crossplane-diff xr --help` and `comp --help` list `--crossplane-version` and `--crossplane-image`
(not hidden); `--crossplane-render-binary` remains hidden. Passing two of the three at once produces a kong
error naming the conflict (unit-testable via kong parse).

**AC-R2/R3:** With `--crossplane-version=v2.4.0`, the constructed `render.EngineFlags.CrossplaneVersion ==
"v2.4.0"` (and `CrossplaneImage`/`CrossplaneBinary` empty); analogously for `--crossplane-image`. Verified by
a unit test on the wiring (either asserting `ProcessorConfig` fields after `defaultProcessorOptions`, or the
`EngineFlags` built by `NewEngineRenderFn`). `go build ./... && go vet ./...` clean.

**AC-R4:** `ValidateMinRenderVersion` returns nil for `>= v2.3.4` (e.g. `v2.3.4`, `v2.4.0`, `v3.0.0`), a
below-minimum error for `< v2.3.4` (e.g. `v2.3.3`, `v2.0.0`, `v1.20.0`), and a parse error for junk
(e.g. `stable`, `latest`, `""`-should-not-reach-it). Table-driven unit test. Note semver v1.5.0 tolerates a
leading `v`.

**AC-R5:** Running `xr`/`comp` with `--crossplane-version=v2.3.3` exits non-zero with a message naming the
minimum, and does so **before** any cluster connection or render (assert no network/tree calls; a
CLI-layer/kong-Validate unit test suffices). `--crossplane-version=v2.3.4` passes validation.

**AC-R6/R7:** README grep shows both flags + "v2.3.4"; design doc §8.1 shows the flags. `go build`/tests green.

**AC (overall):** `earthly -P +go-test` exits 0 (no regressions; new unit tests pass); `earthly -P
+reviewable` exits 0.

## 5. Testing Plan (TDD)

New feature (not a bug-repro), so tests are written first per requirement:

1. **`ValidateMinRenderVersion` (R4)** — table-driven unit test in diffprocessor: valid ≥min, below-min,
   unparseable. Write RED first (helper returns nil / undefined), implement to green. *Fast, pure.*
2. **Wiring (R2/R3)** — unit test that `defaultProcessorOptions` with `CrossplaneVersion`/`CrossplaneImage`
   set yields a `ProcessorConfig` carrying those values (apply the returned options to a fresh config and
   assert). Optionally assert `NewEngineRenderFn` sets the right `EngineFlags` (may require exposing the
   flags for test, or asserting via a seam — prefer the ProcessorConfig-level assertion to avoid engine
   internals). RED → implement → green.
3. **kong parse / mutual exclusion + Validate (R1/R5)** — unit test parsing args through kong: (a) both
   `--crossplane-version` and `--crossplane-image` → parse error; (b) `--crossplane-version=v2.3.3` →
   Validate() error naming the minimum; (c) `--crossplane-version=v2.4.0` → ok. RED → implement → green.
4. **No IT/E2E needed** — this is CLI/plumbing + a pure validator; the docker render path is unchanged in
   shape (we only pass extra EngineFlags upstream already supports). Full `earthly +go-test` still run for
   no-regression. (If an existing IT can cheaply assert `--crossplane-version` reaches the engine, consider
   it, but do not add a new docker-dependent IT solely for this.)

## 6. Implementation Plan (smallest sequential steps)

**Step 1 — `MinCrossplaneRenderVersion` const + `ValidateMinRenderVersion` (R4).**
Add to diffprocessor (e.g. a small `render_version.go` or into `render_engine.go`). Write the RED table test
(`render_version_test.go`) first. Implement using `semver.NewVersion` + compare against
`semver.NewVersion(MinCrossplaneRenderVersion)` (or a `>=` constraint). *Test:* `go test -run
TestValidateMinRenderVersion`.

**Step 2 — `NewEngineRenderFn` accepts version + image (R3).**
Change signature to `NewEngineRenderFn(log, binaryPath, version, image string)` and set
`EngineFlags.CrossplaneVersion`/`CrossplaneImage`. Update the sole production caller
(`diff_processor.go:108`) and any test callers. Update the function's doc comment. *Test:* `go build ./...`;
existing `TestEngineRenderFn_*` still green.

**Step 3 — `ProcessorConfig` fields + options (R2).**
Add `CrossplaneVersion`, `CrossplaneImage` to `ProcessorConfig`; add `WithCrossplaneVersion`,
`WithCrossplaneImage`; pass them into `NewEngineRenderFn` at the default-RenderFn construction site. *Test:*
wiring unit test (apply options → assert config); build.

**Step 4 — kong flags + xor + Validate (R1/R5).**
Add `CrossplaneVersion`/`CrossplaneImage` to `CommonCmdFields` with help text and
`xor:"..."` shared with `CrossplaneRenderBinary`; append `WithCrossplaneVersion/Image` in
`defaultProcessorOptions` when set. Add a `Validate() error` method on `CommonCmdFields` calling
`dp.ValidateMinRenderVersion`. *Test:* kong-parse unit tests (mutual exclusion; below-min rejected;
ok-version accepted).

**Step 5 — README + design doc (R6/R7).**
Document both flags + v2.3.4 minimum in README command-options; add to design doc §8.1 (and §6.1.2 if the
config fields warrant). *Test:* grep; build.

**Step 6 — Full verification.** `earthly -P +go-test` (0), `earthly -P +reviewable` (0). Check `git status`
for autofix write-backs; if any tooling churn appears, **commit it** (national-park model), do not revert.

**Step 7 — Commit (DCO -s), push topic branch via explicit refspec, open draft PR referencing #402.**
