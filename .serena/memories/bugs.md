# Notable bugs & fixes

## 2026-07-23 — "observed resource ... has a controller ref but is not controlled by the XR" (issue #399)

**Symptom:** `crossplane-diff xr`/`comp` render fails against `crossplane render` Docker `:stable`
(now v2.3.4) with `invalid observed resources: [... has a controller ref but is not controlled by
the XR]`. Not a cluster or runner problem.

**Root cause:** version skew with upstream crossplane render. crossplane/crossplane#7544 (shipped in
v2.3.4, backport release-2.3) changed the render binary to (a) preserve a non-empty input XR UID
instead of always overwriting it with `SHA1(gvk+ns+name)`, and (b) add `CheckObservedResources`
(internal/render/composite/render.go), which HARD-ERRORS when an observed resource's controller-ref
UID != the XR's UID. Previously (<= v2.3.3) the binary silently DROPPED such observed resources.

Two facets in crossplane-diff, both exposed by the change:
1. `render_engine.go` had `alignObservedOwnerRefs`/`fakeXRUID` (from PR #326) that UNCONDITIONALLY
   rewrote observed owner-ref UIDs to the fake SHA1 value. Since diff_processor.go:328 already sends
   the XR with its real cluster UID (PR #145), the new binary preserved the real UID while the
   observed refs were rewritten to the fake one -> mismatch -> error. FIX: deleted both functions;
   pass `in.ObservedResources` unmodified.
2. `resource_manager.go extractComposedResourcesFromTree` recursively flattened the ENTIRE subtree,
   including grandchildren controlled by NESTED XRs (different UID than the top XR). Old binary
   tolerated the over-broad list by dropping non-matching entries; v2.3.4 rejects them. FIX: scope
   the returned observed set to resources this XR controls — keep only those with no controller ref
   OR controller-ref UID == xr.GetUID(). Nested XRs are rendered separately with their own observed
   set (recursive `diffSingleResourceInternal`), so each level only needs what it controls.

**Also:** bumped go.mod `crossplane/crossplane/v2` v2.3.3 -> v2.3.4; documented min render version
in README (Prerequisites). `google/uuid` became indirect after removing fakeXRUID.

**Prevention / tells:** when `crossplane render` behavior seems to have changed under you, check the
floating `:stable` tag version (`docker run --rm --entrypoint crossplane <img> core -v`) and diff
`internal/render/composite/render.go` across versions. The IT suite covers this: 11 IT subtests
(removal-hierarchy V1/V2 cluster+ns, nested-XR, sequencer/eventual-state, generateName, SSA field
removal) go red on a too-new binary if observed-resource handling regresses. Earthly `+go-test`
uses the docker render engine (no local binary) = tests against whatever `:stable` is.

**New test helper:** `testutils/mock_builder.go` gained `WithControllerReference(kind,name,apiVersion,uid)`
(sets Controller+BlockOwnerDeletion=true, so `metav1.GetControllerOf` sees it) and `WithUID(uid)`.
Unit regression: `TestDefaultResourceManager_FetchObservedResources/FiltersGrandchildrenControlledByNestedXR`.
