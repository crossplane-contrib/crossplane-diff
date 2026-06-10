# Scope-aware composite render

## As Is

`crossplane-diff` builds `*composite.Unstructured` objects via `cmp.New()` (8 sites in `cmd/diff/diffprocessor/diff_processor.go`) and passes them as the `CompositeResource` input to `crossplane internal render`. None of the call sites set the wrapper's `Schema` field, so all composites default to `composite.SchemaModern` regardless of the underlying XRD version.

`composite.Unstructured`'s accessors (e.g., `SetResourceReferences`, `GetCompositionRevisionReference`) read/write at v2-style paths (`spec.crossplane.{resourceRefs,compositionRef,...}`) when `Schema == SchemaModern`, and at v1-style paths (`spec.{resourceRefs,compositionRef,claimRef,writeConnectionSecretToRef,...}`) when `Schema == SchemaLegacy`. The renderer (running inside `crossplane internal render`) uses these accessors internally, so it produces output at v2 paths whenever the input wrapper is `SchemaModern`.

This means: rendering a v1 (legacy) XR produces `spec.crossplane.resourceRefs` even though the cluster's CRD (derived from the v1 XRD) only declares `spec.resourceRefs`. The subsequent dry-run apply call hits the kube-apiserver, which rejects the patch with `.spec.crossplane.resourceRefs: field not declared in schema`, and the diff fails.

A previous workaround in `cmd/diff/diffprocessor/diff_calculator.go` reactively `RemoveNestedField(applyDesired.Object, "spec", "crossplane")` for resources carrying the `crossplane.io/composition-resource-name` annotation. That conditional misses the root XR (which has no such annotation), so v1 XRs at the root still hit the rejection. Two of our three E2E categories work today only because their XRDs are v2.

## To Be

When constructing a `*composite.Unstructured` for an XR or claim, `crossplane-diff` looks up the backing XRD via `DefinitionClient`, inspects its `apiVersion`, and sets the wrapper's `Schema`:

- `apiextensions.crossplane.io/v1` XRD → `composite.SchemaLegacy`
- `apiextensions.crossplane.io/v2` XRD → `composite.SchemaModern`

For v2 XRDs, the wrapper's `spec.scope` (`Cluster` or `Namespaced`) is informational only — both map to `SchemaModern`; only the legacy-vs-modern axis affects field paths.

The renderer then writes its outputs at the path the wrapper's accessors prescribe — `spec.resourceRefs` for v1 XRs and `spec.crossplane.resourceRefs` for v2 XRs. Subsequent dry-run apply calls succeed because the desired XR shape now matches the cluster CRD's schema. The diff produced reflects what the user would see if they applied the XR.

The reactive strip in `diff_calculator.go` is removed entirely — no post-render mutation is needed.

## Requirements

1. **R1: Schema-discovery helper.** A `DefinitionClient` method (or near-equivalent) returns the appropriate `composite.Schema` for a given XR or claim GVK by looking up the XRD and reading its `apiVersion`.
2. **R2: Schema applied at render-input boundary.** Every `*composite.Unstructured` that is passed in to the render engine — directly or indirectly via XR-input construction — has its `Schema` set to the value returned by R1.
3. **R3: Schema preserved on render output.** The `*composite.Unstructured` produced from the render output (i.e., what we read back to compute diffs) has the same `Schema` as the input. (Today, `composite.New()` is invoked without options when wrapping the renderer's output composite map, so the wrapper defaults to `SchemaModern` even for legacy XRs. Setting it correctly ensures downstream accessors read the right paths.)
4. **R4: Reactive strip removed.** The `un.RemoveNestedField(applyDesired.Object, "spec", "crossplane")` block in `diff_calculator.go` (and its surrounding conditional) is deleted. No post-render mutation of `spec.crossplane.*` happens.
5. **R5: Backward-compatible behavior for v2 fixtures.** Existing v2 XRD integration tests (notably `CompositionRevisionUpgradesResourceAPIVersion`) keep passing — the diff still surfaces `spec.crossplane.compositionRevisionRef` changes.
6. **R6: New v1-XRD coverage.** A new integration test exercises a v1 (legacy) XR to assert the renderer produces v1-shape output (`spec.resourceRefs`, no `spec.crossplane.resourceRefs`), the dry-run apply succeeds, and the resulting diff is v1-shape.
7. **R7: Schema lookup is cached / cheap.** Repeated XRD lookups for the same GVK don't re-fetch from the apiserver each time — the existing XRD cache in `DefaultDefinitionClient` is sufficient if we route through it.

## Acceptance Criteria

For each requirement above:

- **R1:** A method `GetCompositeSchema(ctx, gvk) (composite.Schema, error)` (or similar) exists on `DefinitionClient`. It returns `SchemaLegacy` for an XRD whose `apiVersion == "apiextensions.crossplane.io/v1"` and `SchemaModern` for `apiextensions.crossplane.io/v2`. Unknown apiVersions return an error. There is a unit test asserting both branches plus the error path. Claim-typed resources resolve via the claim path; XR-typed resources resolve via the XR path.
- **R2:** All `cmp.New()` call sites in `cmd/diff/diffprocessor/diff_processor.go` that produce a composite destined for the renderer pass `cmp.WithSchema(s)` where `s` comes from R1. A unit/integration test asserts that for a v1 XR fixture, the wrapper passed to the render fn has `Schema == SchemaLegacy`.
- **R3:** Wherever we wrap the renderer's output composite (e.g., reading back the rendered XR), the wrapper carries the same Schema as the input. Either the schema flows through `RenderInputs`/`render.CompositionOutputs` or we re-look-up after render — whichever is simpler. A unit test asserts the readback wrapper's Schema matches the input wrapper's Schema for both v1 and v2 fixtures.
- **R4:** `git diff diff_calculator.go` shows the `RemoveNestedField` block and its conditional removed. No new strip code introduced. The unit test for `CalculateDiff` no longer asserts that `spec.crossplane` is absent from the dry-run apply input.
- **R5:** `cd cmd/diff && CROSSPLANE_RENDER_BINARY=...crossplane go test -run 'TestDiffIntegration/CompositionRevisionUpgradesResourceAPIVersion' ./...` passes.
- **R6:** A new integration test (e.g., `LegacyXRRendersV1Shape`) installs a v1 XRD + matching legacy CRD + a legacy XR, runs the diff, and asserts (a) no `spec.crossplane.resourceRefs` ends up on the rendered XR, (b) `spec.resourceRefs` does, (c) the diff exit code is 3 with at least one ADDED composed resource. The test runs via the same envtest harness as the rest of `TestDiffIntegration`.
- **R7:** Schema-discovery does not measurably increase test runtime (≤ 1 second total over a `TestDiffIntegration` run). The `DefaultDefinitionClient`'s existing cache is exercised — observable via the absence of repeated dynamic-client `Get` calls for the same XRD GVK in a single render loop.

## Testing Plan (TDD)

Tests are introduced in this order, each landing red, then green, before the next is added.

### T1 — Unit test for `GetCompositeSchema` (R1)

Location: `cmd/diff/client/crossplane/definition_client_test.go`.

Cases:
- v1 XRD (apiVersion `apiextensions.crossplane.io/v1`) → returns `SchemaLegacy`.
- v2 XRD (apiVersion `apiextensions.crossplane.io/v2`) → returns `SchemaModern`.
- GVK that doesn't resolve to any XRD → returns error.

Builds on existing `getMockResourceClient` patterns in the file. No real cluster.

### T2 — Unit test asserting render-input Schema is set (R2)

Location: `cmd/diff/diffprocessor/diff_processor_test.go` (or a new `*_test.go` file alongside `RenderToStableState`).

Approach: a mock `RenderFn` records the `Schema` of `inputs.CompositeResource`. The test runs `RenderToStableState` with a mock `defClient` that returns a v1 XRD for the test GVK, and asserts the mock saw `SchemaLegacy` on the input. A second case uses a v2 XRD and asserts `SchemaModern`.

### T3 — Unit test asserting Schema is preserved on the render output (R3)

Location: same file as T2 (or a small companion).

Approach: mock `RenderFn` returns an output composite. The test asserts that the wrapper used for the subsequent diff calculation has the right Schema (mirrors the input). Implemented by reading the wrapper's accessor — e.g., setting `resourceRefs` via the wrapper after readback and asserting it landed at the legacy path.

### T4 — Integration test: legacy XR renders to v1 shape (R6)

Location: `cmd/diff/diff_integration_test.go` (new entry in the test table).

Fixture: a v1 XRD + matching v1 CRD (declaring `spec.resourceRefs`, `spec.compositionRef`, etc., per the upstream legacy schema) + a legacy XR + a composition that maps to a single composed resource. The composition's pipeline produces deterministic output (no requirements iteration → not blocked on the upstream FATAL issue).

Assertions:
- Dry-run apply succeeds (no schema rejection).
- Rendered XR has `spec.resourceRefs` populated.
- Rendered XR has no `spec.crossplane.resourceRefs`.
- The diff exit code is 3 (changes detected).
- One added composed resource appears in the structured diff output.

### T5 — Existing integration tests stay green (R5)

Re-run the full `TestDiffIntegration` suite (skip-respecting). Should still report 36 PASS, 0 FAIL, 22 SKIP.

### T6 — `diff_calculator_test.go` updates for R4

Find any test that asserts `spec.crossplane` is/was stripped from `applyDesired`. Remove/adjust those assertions (the strip is gone; the assertion is no longer meaningful). New test: dry-run apply input for a v2 XR retains `spec.crossplane.compositionRef` (to confirm we no longer mutate it).

## Implementation Plan

Smallest sequential changes, each with its own tests run.

### S1 — Add `GetCompositeSchema` to `DefinitionClient` (T1)

1. Add the interface method in `definition_client.go`.
2. Implement on `DefaultDefinitionClient`: try `GetXRDForXR(ctx, gvk)` first, fall back to `GetXRDForClaim(ctx, gvk)`. Read the XRD's `apiVersion`. Map `v1` → `SchemaLegacy`, `v2` → `SchemaModern`, anything else → error.
3. Update the existing `MockDefinitionClient` in `cmd/diff/testutils/mocks.go` (and any generated mock builders) to satisfy the new method.
4. Implement T1 test cases (red), then implement S1 to make them pass (green).

### S2 — Use the new helper at the XR-input construction site for the renderer (T2)

1. In `RenderToStableState` (or wherever we call `renderFn`), look up the schema for the XR's GVK via the new helper, then re-wrap the input composite with `cmp.WithSchema(s)`.
2. T2 verifies via the mock `RenderFn` that the schema flows through.

### S3 — Preserve schema on render output read-back (T3)

1. Identify where we wrap the renderer's response composite (`render.CompositionOutputs.CompositeResource`). Re-construct that wrapper with the same Schema we just set on the input.
2. T3 verifies the readback wrapper accessor lands at legacy paths for legacy XRs.

### S4 — Remove the reactive strip in `diff_calculator.go` (T6)

1. Delete the `applyDesired := desired.DeepCopy(); un.RemoveNestedField(applyDesired.Object, "spec", "crossplane")` block and the `if desired.GetAnnotations()[...] != ""` conditional around it.
2. Adjust any unit tests that asserted on the stripped state.
3. Re-run `cmd/diff/diffprocessor/...` unit tests.

### S5 — Add the legacy-XR integration test (T4)

1. Create test fixtures (XRD + CRD + composition + XR) under `cmd/diff/testdata/diff/...` for a legacy XR scenario.
2. Wire a new entry into `TestDiffIntegration`'s table with assertions per T4.
3. Run only that one subtest first to confirm it passes; then run the full integration suite (T5) to confirm no regression.

### S6 — Sweep for any remaining `cmp.New()` site that flows to the renderer (R2)

After S2, audit the remaining 7 `cmp.New()` call sites in `diff_processor.go`:
- For each, determine whether the resulting composite is fed to `renderFn` (directly or via copy).
- If yes, apply the same `WithSchema` plumbing.
- If no (e.g., used purely for cluster-state lookup with no spec.crossplane access), leave a comment justifying.

### S7 — Run lint + reviewable

`earthly +go-lint` and `earthly -P +reviewable` must remain green.

### S8 — Re-run E2E smoke

`earthly -P +e2e --CROSSPLANE_IMAGE_TAG=main --FLAGS="-test.run ^TestDiffExistingComposition$"` should now pass (or fail for a different reason that's not the spec.crossplane.resourceRefs schema rejection).

## Out of Scope

- Cat 1 (crossplane v1 cluster image with no `internal render` subcommand). Handling that requires a parallel render path; tracked separately.
- User-facing `--crossplane-image` / `--crossplane-version` / `--crossplane-binary` CLI flags. Tracked separately (task #27).
- Filing the upstream FATAL-with-requirements issue. Tracked separately (task #25).
