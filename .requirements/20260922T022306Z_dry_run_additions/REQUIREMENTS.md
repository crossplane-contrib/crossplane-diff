# Dry-run added resources against the apiserver (higher-fidelity `+++` diffs)

- **Issue:** crossplane-contrib/crossplane-diff#334
- **Branch:** `optional-ssa`
- **Date (UTC):** 2026-09-22
- **Decisions locked (via AskUserQuestion):**
  - **Default:** **on** (`--dry-run-on=all`), degrading gracefully — *not* opt-in. Accuracy is not a mode;
    the flag is a depth/cost knob, which is the only flag category this project accepts for something that
    affects what gets reported.
  - **Cluster rejections** (validating webhook / apiserver `Invalid`): reuse `SchemaValidationError` →
    **exit 2** + typed `validationFailures[]` with `FieldValidationError.Type: "admission"`.
  - **RBAC denial:** **typed per-resource field** on the diff (not a bare warning), following the
    `ImpactAnalysisSkipped` / `FilterReason` precedent.
  - **Naming:** enum `--dry-run-on=existing|all`, structured field `dryRun`.
  - **403 discrimination:** **`SelfSubjectAccessReview` gate**, cached per GVR+namespace.
  - **Unreachable webhook (500):** degrade with `skipReason: webhookUnavailable`.
  - **Rejection reclassification applies to the existing-resource path too** (§3 R8) — a cluster rejection
    must not be reported under two different exit codes depending on whether the resource happens to exist.
  - **Rendered `status` is re-attached** to the dry-run result. Rationale (user): composition pipelines are
    explicitly allowed to write their own status, so the rendered status is *authored content*. Dropping it
    would be losing user-authored output, not gaining fidelity.
  - **`NewDiffCalculator` takes the new dependencies as constructor params**, accepting the test churn,
    rather than adding a post-construction setter.
  - **Rejection-path test instrument:** `ValidatingAdmissionPolicy` under envtest if available; otherwise
    **e2e against the real kind cluster** (not a unit-only fallback).

---

## 1. As Is

`DefaultDiffCalculator.CalculateDiff` (`cmd/diff/diffprocessor/diff_calculator.go:118-157`) computes the
"would-be" form of each resource by server-side-apply dry-run, but **only when the resource already exists**:

```go
wouldBeResult := desired
if current != nil {
    fieldOwner := k8.GetComposedFieldOwner(current)
    applyDesired := desired.DeepCopy()
    un.RemoveNestedField(applyDesired.Object, "metadata", "ownerReferences")
    wouldBeResult, err = c.applyClient.DryRunApply(ctx, applyDesired, fieldOwner)
}
```

For additions (`current == nil`) the diff emits `desired` straight from the render pipeline. README §RBAC
(lines 433–453) documents this as a deliberate limitation and links #334.

### The gap is narrower than the issue claims — CRD defaulting is already handled locally

**This correction was found by writing the RED test first, and it invalidated the headline test as
originally specified.** `DefaultSchemaValidator.ValidateResources` calls `applyCRDDefaults`
(`schema_validator.go:121`, `:306`) over the XR **and every composed resource**, before the diff calculator
runs, precisely so that "composed resources would [not] reach diff calculation undefaulted and produce
spurious diffs for fields the cluster's defaulter would have populated". `TestDiffIntegration`'s existing
`XRDDefaultsAppliedBeforeRendering` case already asserts a defaulted field appearing in a `+++` diff.

So the issue's first bullet — "CRD `default:` field values" — is **already covered** for any type backed by
a CRD, and a test asserting CRD defaulting on an addition would pass on unmodified `main`.

What `applyCRDDefaults` does *not* cover, per its own doc comment ("Built-in types and resources whose CRD is
unknown are skipped"), and what nothing else covers:

| Gap | Covered locally today? |
|---|---|
| CRD `default:` on CRD-backed types | **Yes** — `applyCRDDefaults` |
| Defaults on **built-in** Kubernetes types (no CRD → `IsCRDRequired` false → skipped) | **No** |
| Mutating admission webhook output (any type) | **No** |
| Validating webhook / quota / namespace-lifecycle rejection | **No** |

Compositions rendering built-in types are a real, already-tested shape in this repo — see
`composition-with-configmap.yaml` and `TestDiffIntegration/RendersBuiltInResourceWithoutCRD`. A composed
`Deployment` gets none of `spec.strategy.type`, `spec.template.spec.restartPolicy`,
`spec.template.spec.dnsPolicy`, `schedulerName`, or container `imagePullPolicy` today.

This narrows the feature's value but does not eliminate it, and it sharpens what must be tested: **the
headline test MUST target a built-in type or a mutating webhook, never a CRD `default:`.** It also means the
README's framing of the gap needs correcting, not just extending (R14).

### Two premises recorded in the issue/code are wrong, and both simplify the work

**(a) The ownerReferences rationale does not exist.** The code comment at `diff_calculator.go:132-140`
justifies the gate partly as: *"for new resources (current == nil) we skip DryRunApply entirely so the
rendered ownerRefs still surface in the diff output."* But `cleanupForDiff`
(`cmd/diff/renderer/diff_formatter.go:659-667`) strips `ownerReferences` **unconditionally**, in the same
list as `resourceVersion`/`uid`/`generation`/`creationTimestamp`/`managedFields`/`selfLink`, for both sides
of every comparison. Rendered ownerRefs therefore never surface in diff output on *any* path. The stated
purpose of the gate describes behaviour that has never existed.

Two corollaries: the stripping can be **hoisted out of the branch** (both paths send the same sanitized
payload), and the server-assigned metadata a dry-run response carries (`uid`, `creationTimestamp`,
`managedFields`, `resourceVersion`) is **already stripped from display**, so this change introduces no new
diff-output noise and no display-vs-verdict tier decision.

**(b) SSA `Apply` cannot serve the addition path at all.** `DefaultApplyClient.DryRunApply`
(`cmd/diff/client/kubernetes/apply_client.go:100`) calls
`resourceClient.Apply(ctx, obj.GetName(), obj, applyOptions)`. On the existing path
`preserveExistingResourceIdentity` guarantees a name from the cluster copy. Additions using `generateName`
have `name == ""`, and SSA has no generateName equivalent — apply is a PUT-shaped request to a named path.
So the mechanism must be `Create` with `DryRun: [All]`. That is also the narrower RBAC ask (`create` alone,
versus `create`+`patch` for a *creating* SSA apply) and the semantically correct primitive for
"plan before apply".

### `Forbidden` is overloaded

A dry-run create can return 403 from at least four sources, and only the first is an environment limitation:

| 403 source | Meaning | Correct handling |
|---|---|---|
| Authorizer (RBAC) | We lack `create`. Nothing learned about the resource. | Degrade |
| `ResourceQuota` admission | The apply would genuinely be rejected. | **Report** |
| `NamespaceLifecycle` admission | Target namespace does not exist. | **Report** |
| Validating webhook using `Forbidden` | Genuinely rejected. | **Report** |

Treating every 403 as "degrade" would silently swallow three real findings. Sniffing the 403 message to
discriminate would rest a correctness decision on unversioned apiserver prose — the same class of coupling
this codebase has repeatedly been bitten by (embedding assumptions about another component's internal
behaviour instead of treating it as a versioned contract).

### Related hazard on the path being widened

`diff_calculator.go:244` (`desiredXR = renderedXR`, the nested-XR branch) feeds render output straight to
`CalculateDiff` with no `SetManagedFields(nil)` strip, unlike the root-XR branch in `diff_processor.go`.
client-go's dynamic `Apply` rejects any object with `metadata.managedFields` populated
(`cannot apply an object with managed fields already set`). This was reported as #452 and could not be
reproduced, and remains unexplained. Widening what reaches the apiserver makes it more live, and a single
sanitization point closes it for both paths without touching the branch logic.

---

## 2. To Be

Added resources are round-tripped through the apiserver by default, so `+++` diffs carry CRD defaulting and
mutating-admission output exactly as `~~~` diffs already do. The asymmetry is gone.

Where the round-trip cannot be made, the tool says so per-resource instead of silently presenting
render output as apiserver-faithful:

- **No `create` permission** → fall back to rendered desired, `dryRun.skipReason: forbidden`, one warning.
- **`--dry-run-on=existing`** → today's behaviour exactly, `dryRun.skipReason: disabled`, no warning.
- **Admission webhook unreachable** → fall back, `dryRun.skipReason: webhookUnavailable`, one warning.

Where the apiserver *rejects* the resource, that is reported as the finding it is — a cluster rejection,
exit 2, with typed per-resource detail — and identically whether the resource exists or not.

Flow:

```
                  ┌─ current != nil ──→ DryRunApply (behaviour unchanged)
CalculateDiff ────┤
                  └─ current == nil ──→ dryRunOn == existing? ──→ skipReason: disabled
                                            │ all
                                            ↓
                                     CanCreate(gvr, ns)?   ← cached SSAR
                                   no ──────┴────── yes
                                    ↓                ↓
                          skipReason:          DryRunCreate
                           forbidden                 │
                                        ┌────────────┼──────────────┐
                                       ok        403/422      webhook 500
                                        ↓            ↓             ↓
                                  merge result   REJECTION    skipReason:
                                   (§3 R6)        (exit 2)   webhookUnavailable
```

---

## 3. Requirements

**R1 (flag + config).** `CommonCmdFields` (`cmd/diff/main.go`) gains
`DryRunOn string` with `default:"all" enum:"existing,all" name:"dry-run-on"`. Because it lives on the shared
struct, both `xr` and `comp` get it. A `DryRunOn` string type with `DryRunOnExisting` / `DryRunOnAll`
constants is added alongside `AnalyzeOn` in `processor_config.go`, together with `ProcessorConfig.DryRunOn`
and a `WithDryRunOn` option, and is threaded into `NewDiffCalculator`.

Kong's inability to distinguish a defaulted flag from an explicitly-passed one (the `--analyze-on` lesson)
does **not** bite here: degradation is uniform, so no behaviour depends on whether the user typed the flag.
A plain `default:` tag is therefore correct.

**R2 (access checker).** A new `cmd/diff/client/kubernetes/access_client.go` defines:

```go
type AccessChecker interface {
    CanCreate(ctx context.Context, gvk schema.GroupVersionKind, namespace string) (allowed bool, reason string, err error)
}
```

implemented via `authorization.k8s.io/v1` `SelfSubjectAccessReview` created through the existing
`core.Clients.Dynamic` — no addition to `core.Clients`. Results MUST be memoized on GVR+namespace
(cluster-scoped resources key on `""`). `reason` carries the SSAR's `status.reason` for the `dryRun.detail`.

SSAR is reliably available: `create` on `selfsubjectaccessreviews` is granted to `system:authenticated` by
default through the `system:basic-user` ClusterRole. SSAR also reflects the *whole* authorizer chain (RBAC,
webhook, node), not only RBAC.

**R3 (dry-run create).** `ApplyClient` gains
`DryRunCreate(ctx context.Context, obj *un.Unstructured) (*un.Unstructured, error)`, using
`resourceClient.Create(ctx, obj, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: FieldOwnerDefault})`.

No `fieldOwner` parameter: a new object has no `managedFields`, so `GetComposedFieldOwner` has nothing to
read and creates always use `FieldOwnerDefault`. Errors MUST be wrapped such that `apierrors.Is*`
classification still works (crossplane-runtime's `errors.Wrap` preserves `Unwrap`, so `errors.As` walks it).

**R4 (single sanitization point).** A `sanitizeForDryRun(obj)` helper strips
`metadata.{resourceVersion,uid,creationTimestamp,generation,selfLink,managedFields,ownerReferences}` from
the deep-copied payload, applied on **both** the apply and create paths before the object leaves for the
apiserver. The existing in-branch `RemoveNestedField(..., "ownerReferences")` and its comment are replaced:
the comment's ownerRef-visibility rationale is false (§1a) and MUST NOT be carried forward.

Dropping `ownerReferences` on the create path is free (they are display-stripped regardless) and avoids
`OwnerReferencesPermissionEnforcement`, which is enabled by default on OpenShift and requires `delete` on
the owner. Stripping `managedFields` closes the #452 hazard (§1) for both paths.

**R5 (addition path).** When `current == nil`, `CalculateDiff` implements the §2 decision tree.

Classification of a `DryRunCreate` error MUST be by **status class, never by message text** — the whole point
of the SSAR gate is to avoid resting correctness on apiserver prose (§1):

| Predicate | Outcome |
|---|---|
| `nil` | merge per R6 |
| `apierrors.IsInvalid` (422) | rejection → R8 |
| `apierrors.IsForbidden` (403) — reachable only *after* SSAR allowed | rejection → R8 |
| `apierrors.IsInternalError` \|\| `IsServiceUnavailable` \|\| `IsTimeout` | degrade, `webhookUnavailable` |
| `apierrors.IsAlreadyExists` (409) | plain error → R10 |
| anything else (incl. no such GVR / `NoKindMatchError`) | plain error |

`webhookUnavailable` is named for its dominant cause but the predicate is broader: it means *the apiserver
could not complete the admission chain*, typically an unreachable webhook with `failurePolicy: Fail`. The
`Detail` field carries the apiserver's actual message, so the specific cause is never lost. The constant's
doc comment MUST say this, so the name is not read as a narrower claim than the predicate makes.

**R6 (result merge).** The dry-run-create response is authoritative for spec and metadata mutations, with
two fields restored from our side:

- **R6.1** When the request carried `generateName != "" && name == ""`, restore that identity
  (`generateName` set, `name` empty). The apiserver runs `names.Generator` in `rest.BeforeCreate`, before
  the dry-run short-circuit at the storage layer, so it invents a random name that would make `+++` output
  differ run-to-run. For *named* resources the server's name is kept, so a mutating webhook that rewrites a
  name still surfaces.
- **R6.2** When the rendered object had a `status`, re-attach it, **overwriting whatever the response
  carried** (including a non-empty defaulted status — the rendered one wins). Composition pipelines
  legitimately write their own status, so a rendered status is authored content; the apiserver contributes
  nothing to status on create (status subresource) and would return it empty. Taking the empty one would
  delete user-authored output under the banner of fidelity. When the rendered object had **no** `status`, the
  response's status (if any) is left as-is.

**R7 (typed per-resource field).** `renderer/types/types.go` gains:

```go
type DryRunSkipReason string
const (
    DryRunSkipDisabled           DryRunSkipReason = "disabled"
    DryRunSkipForbidden          DryRunSkipReason = "forbidden"
    DryRunSkipWebhookUnavailable DryRunSkipReason = "webhookUnavailable"
)
type DryRunInfo struct {
    Performed  bool             `json:"performed"`
    SkipReason DryRunSkipReason `json:"skipReason,omitempty"`
    Detail     string           `json:"detail,omitempty"`
}
```

with `DryRun *DryRunInfo` on `ResourceDiff`, surfaced as `dryRun` on `ChangeDetail`
(`renderer/structured_renderer.go:145`). `ChangeDetail` is the shared per-resource wire shape — `xr` reaches
it via `Changes`, `comp` via `DownstreamChanges.Changes` — so one field covers both commands.

The field MUST be emitted **only when the dry-run did not happen** (pointer + `omitempty`). Absence means
the desired state went through the apiserver. Removal diffs never carry it: they are produced by
`CalculateRemovedResourceDiffs` via a direct `GenerateDiffWithOptions` call and have no desired state to
preview. This asymmetry MUST be documented on the type.

`Performed` is therefore always `false` whenever the struct is present, which is deliberate rather than an
oversight: it keeps the emitted JSON self-describing, so a consumer reading a `dryRun` object does not have
to know that mere presence implies degradation. It also leaves room to emit the struct unconditionally later
without a schema break. The `Performed` field's doc comment MUST state this.

**R8 (rejection → exit 2, both paths).** `SchemaValidationError` gains
`WithFailures([]dt.ResourceValidationFailure)`, a sibling to `WithResult` for failures that do not originate
from the upstream validator. This avoids fabricating a `pkgvalidate.FieldValidationError.Type` value that
upstream does not define, and is consistent with `ResourceValidationFailure`'s own documented intent
("defined here so crossplane-diff's JSON output schema is owned by us"). `NewOutputError` gains a branch
preferring explicit failures over `Result`.

An apiserver rejection (`apierrors.IsInvalid`, or `IsForbidden` *after* SSAR allowed) becomes a
`SchemaValidationError` carrying one `ResourceValidationFailure{Status: "invalid"}` whose single
`FieldValidationError` has `Type: "admission"` and the apiserver message.

**This applies to the existing-resource path as well**, where such a rejection currently produces a plain
error and exit 1. Reporting the same cluster fact under two different exit codes depending on whether the
resource happens to exist would introduce a fresh asymmetry in the change whose purpose is removing one.
This is a deliberate behaviour change and MUST be called out in the PR description and the README
exit-code section.

**R9 (warnings, deduplicated).** `forbidden` and `webhookUnavailable` each emit a user-facing warning via
`c.logger.Info(...)`. No new constructor parameter is needed: `ProcessorConfig.Warnings` documents that it
"is the same `*WarningLogger` that `Logger` is set to when the CLI wires one up", so the calculator's
existing `logger` already *is* the `WarningLogger` in production, and `Info` becomes a stderr line plus a
`warnings[]` entry under the established dual-emission contract.

Warnings MUST be deduplicated on `gvk|namespace|skipReason` via a mutex-guarded set on the calculator, so a
composition rendering 40 new resources of one kind produces one warning, not 40. `disabled` emits **no**
warning: the user asked for it.

The warning is the human channel and the typed field is the machine channel. Neither replaces the other —
`warnings[]` carries no resource anchor, so it cannot tell a pipeline *which* additions were degraded.

**R10 (`AlreadyExists` fails loudly).** A 409 from the dry-run create means `FetchCurrentObject` failed to
match an existing resource, or a genuine race. Either way it indicates the diff is built on a false premise,
so it MUST be a plain error (exit 1), not a degradation. An SSAR call that itself errors is likewise a plain
error.

**R11 (no diff-body change).** The human-readable diff body is NOT annotated with fidelity information; the
warning carries it. This keeps the `.ansi` expectation files untouched.

**R12 (`--dry-run-on=existing` reproduces today's behaviour).** Apart from the additive `dryRun` field in
structured output, selecting `existing` MUST reproduce current output.

R4 is the one caveat and needs care rather than assumption. Widening the strip changes what the **apply**
path sends, not only what is displayed: today, if the user's input file carries `metadata.resourceVersion`
or `uid` (very plausible for a file exported with `kubectl get -o yaml`), those are sent to SSA apply, and a
stale `resourceVersion` makes the apiserver reject the request on optimistic concurrency. Stripping them is
therefore a strict improvement and may *fix* a latent failure — but it is still a behaviour change on an
existing path. If any existing test output changes under `--dry-run-on=existing`, that MUST be investigated
and explained, not accepted as expected churn.

Note also that `diff_processor.go`'s existing `mergedXR.SetManagedFields(nil)` becomes redundant once R4 is
in place. Leave it: it is harmless, and it guards the root-XR object before it reaches other consumers, not
only the apply call.

**R13 (claim path check).** Verify that the dummy backing XR synthesized for a new claim
(`synthesizeDummyBackingXRForNewClaim`) is never passed to `CalculateDiff`, and therefore never dry-run
created. The object diffed for a claim input is the claim itself, whose dry-run create is correct and
valuable (claim CRD defaults). If the synthesized XR *does* reach `CalculateDiff`, it MUST be excluded.

**R14 (docs sync).** Per repo CLAUDE.md triggers:
- README §RBAC (433–453) — rewrite: `create` now required for full fidelity; the degradation contract; the
  new flag. The existing text states the limitation as permanent and links this issue.
- README exit-code section — exit 2 now also covers cluster rejections (R8).
- README structured-output schema — `dryRun` on change entries.
- `design/design-doc-cli-diff.md` — §6.1.2 (`ProcessorConfig` field), §6.9 (new client), §8.1 (new flag),
  §12.1 (new file), plus `client-architecture.mermaid`, `layered-architecture.mermaid`,
  `conceptual-layers.mermaid`, with SVGs regenerated.

---

## 4. Acceptance Criteria

**AC-R1.** `crossplane-diff xr --help` shows `--dry-run-on` with values `existing,all` and default `all`;
same for `comp`. An invalid value fails at parse time. `grep -n "dry-run-on" cmd/diff/main.go` hits.

**AC-R2.** Unit test: two `CanCreate` calls for the same GVR+namespace issue **one** API call (asserted
against a fake dynamic client's action log). A denied SSAR returns `allowed == false` with the SSAR reason.

**AC-R3.** Unit test: `DryRunCreate` sends `dryRun=All`, and a resource with `generateName` and no `name`
is accepted (no empty-name request path).

**AC-R4.** `grep -n "ownerReferences" cmd/diff/diffprocessor/diff_calculator.go` shows the strip only inside
`sanitizeForDryRun`, and no comment claims rendered ownerRefs surface in output. The sanitized payload for
both paths lacks all seven fields (unit-asserted).

**AC-R5/R6 (the headline, integration, real apiserver).** A composition rendering a **built-in** Kubernetes
type — a `Deployment`, following the established `composition-with-configmap.yaml` pattern — as an *addition*:
- with `--dry-run-on=all` (default) apiserver-defaulted fields **appear** in the structured diff:
  `spec.strategy.type == "RollingUpdate"`, `spec.template.spec.restartPolicy == "Always"`,
  `spec.template.spec.dnsPolicy == "ClusterFirst"`;
- with `--dry-run-on=existing` they are **absent** and `dryRun.skipReason == "disabled"` is present.

A built-in type is required, not incidental: CRD-backed defaulting is already applied locally by
`applyCRDDefaults` (§1), so a CRD `default:` assertion would pass on unmodified `main` and prove nothing.

Assertions MUST be on **string-valued** defaults. `assertChangeFields` compares with `reflect.DeepEqual`
against values decoded from JSON, so a numeric default such as `revisionHistoryLimit: 10` arrives as
`float64` and an `int` literal in the expectation silently fails to match for the wrong reason.

This pair is the whole feature. The first assertion is impossible to satisfy on `main`.

**AC-R6.1.** Integration: an addition using `generateName` produces the same name/generateName in output
across two consecutive runs (no server-generated random suffix leaking in).

**AC-R6.2.** Integration: an addition whose composition writes `status` retains that status in the
`+++` diff under `--dry-run-on=all`.

**AC-R7.** `dryRun` is absent from structured output for successfully dry-run resources and for removals;
present with the right `skipReason` in each degraded case. Asserted for both `xr` and `comp` output.

**AC-R8.** An apiserver-rejected resource yields exit **2**, with `errors[].validationFailures[]` containing
one entry whose `errors[0].type == "admission"` and whose message includes the apiserver's rejection text.
Asserted for an addition **and** for a modification (the reclassification). Unit coverage for
`WithFailures` → `NewOutputError` → `DetermineExitCode`.

**AC-R9.** With N>1 additions of one kind denied by SSAR, exactly **one** warning appears on stderr and in
`warnings[]`. `--dry-run-on=existing` emits **zero** warnings.

**AC-R10.** A 409 from `DryRunCreate` produces exit 1 and a message naming the resource.

**AC-R11.** No `.ansi` expectation file changes because of a *rendering* decision — the diff body gains no
fidelity annotation, and warnings go to stderr.

But "zero `.ansi` churn" is **not** a valid acceptance criterion and MUST NOT be asserted: e2e now round-trips
additions through a real apiserver, so any e2e CRD that declares a `default:` will legitimately produce new
lines in its `+++` output. That is the feature working. The criterion is therefore: **every** `.ansi` change
is traced to a specific field the apiserver defaulted or mutated, and named as such in the PR. An `.ansi`
change that cannot be explained that way is a bug, and regenerating with `E2E_DUMP_EXPECTED=1` before
understanding it would hide exactly the failure this feature could introduce.

**AC-R12.** With `--dry-run-on=existing`, the full existing test suite passes with no expectation edits
beyond the additive `dryRun` field. Any change traceable to the R4 strip widening is explained in the PR
(most likely as a fixed latent `resourceVersion` conflict) rather than absorbed into an expectation update.

**AC-R13.** Documented finding (in the PR) that the synthesized backing XR does or does not reach
`CalculateDiff`, with an exclusion if it does.

**AC-R14.** `grep -n "334" README.md` no longer presents the gap as permanent; `--dry-run-on` is documented;
the exit-code table mentions cluster rejections; mermaid SVGs regenerated (`git status` shows the `.svg`
siblings updated alongside any edited `.mermaid`).

**AC (overall gate).** `earthly -P +reviewable` exits 0 (run bare — never piped through `tee`/`tail`, which
masks the exit code). E2E matrix green.

---

## 5. Testing Plan (TDD)

**RED first.** The first thing written is AC-R5's integration case: a composition rendering a built-in
`Deployment` as an addition, asserting apiserver-defaulted fields appear in the structured diff. On `main`
this **must fail** — that failure is the proof the feature is absent and the test is not vacuous. Capture the
failing output before touching `diff_calculator.go`.

This step already earned its keep: the first draft of this spec specified a CRD `default:` as the instrument,
and writing the test revealed `applyCRDDefaults` would have made it pass on `main` (§1). Do not substitute a
CRD-backed type back in for convenience.

Ordering:

1. **RED** — AC-R5 integration case (defaulting on an addition). Confirm failure.
2. **GREEN** — R2/R3/R4/R5/R6 minimal path until it passes.
3. Unit table over `CalculateDiff` covering all seven outcomes (existing→apply unchanged; addition+all+ok;
   addition+all+SSAR-denied; addition+all+403-after-allowed; addition+all+422; addition+all+webhook-500;
   addition+existing→disabled), with `AccessChecker` and `ApplyClient` mocks added to
   `testutils/mock_builder.go` using the existing fluent style. Expectations go in the want-struct and are
   compared with `cmp.Diff` over the whole value — no procedural validate hooks.
4. Unit: `DryRunCreate`, SSAR memoization (including the no-second-call assertion), `sanitizeForDryRun`,
   `WithFailures` → `NewOutputError`, `DetermineExitCode` for admission rejections.
5. Integration: AC-R5 pair, AC-R6.1 determinism, AC-R6.2 status retention, AC-R7 absence/presence for both
   commands, AC-R9 warning dedup, AC-R10.
6. **Rejection path (AC-R8).** A CEL `x-kubernetes-validations` rule is **not** a valid instrument: the
   local schema validator evaluates CEL and would reject the resource before the dry-run ever runs, so the
   test would pass for the wrong reason. Use a `ValidatingAdmissionPolicy` — native, GA since 1.30, no
   webhook server to stand up, and invisible to the local validator. Verify the repo's envtest apiserver
   supports VAP; **if it does not, write this as an e2e** against the real kind cluster with real Crossplane
   (per decision), not as unit-only coverage.
7. **Mutation-verify** every new assertion (house practice): invert the production behaviour and confirm
   the test fails. In particular AC-R5 and AC-R8 — a fidelity test that passes with the feature disabled is
   worthless.

**Environment notes.** Integration tests need a working render binary; the default docker `:stable` path has
been broken against `main` (`crossplane: error: unexpected argument internal`), so build from the checkout:
`go build -C /Users/jonathan.ogilvie/workspace/crossplane -o <worktree>/_output/bin/crossplane ./cmd/crossplane`.
Four requirements-resolution ITs (`EnvironmentConfigIncorporation`, `ExternalResourceDependencies`,
`CrossNamespaceResourceDependencies`, `EventualStateWithSequencerAndEnvironmentConfigs`) fail locally with a
self-built render binary; this is **pre-existing** — do not chase it.

Note also that neither suite reproduces client-written metadata by default (ITs `Create` fixtures verbatim;
e2e uses SSA with a `FieldOwner`). Not expected to matter here, but if any behaviour turns out to depend on
such a field, declare it in the fixture rather than shelling out to `kubectl`.

**envtest orphan hazard (operational, applies to every run in this plan).** envtest starts `kube-apiserver`
and `etcd` as children of the Go test binary, and cleanup lives *only* in `testEnv.Stop()`. If the test
binary dies by SIGKILL — `go test` timeout, harness kill, Ctrl-C, agent teardown — `Stop()` never runs, the
children are reparented to launchd, and they survive indefinitely at ~121 MB per pair. `defer testEnv.Stop()`
does **not** protect against this; SIGKILL is untrappable and Darwin has no `PR_SET_PDEATHSIG`. On
2026-09-21 this had accumulated 404 orphans holding 25.82 GB. The leak is active — no guard is installed.

This plan runs the integration suite many times, so: **never interrupt a running test** (the standing rule's
mechanical consequence here is a permanent leak, not just wasted work), and check for orphans before and
after a session with `ps -eo pid,ppid,command | grep io.kubebuilder.envtest/k8s/` filtered to `PPID == 1`
(both conditions required — a looser match can kill a *live* run). Verified zero orphans at spec time.

A pre-test reaper in the test bootstrap is the only mitigation that covers the SIGKILL case, but it is **out
of scope for this issue** and should be filed separately rather than smuggled into this PR.

---

## 6. Implementation Plan (smallest sequential steps)

**Step 1 — RED: the defaulting integration test (AC-R5).**
Add a CRD fixture with `default:` on a spec field and an IT asserting the defaulted value appears in an
addition's structured diff. Run it; capture the failure.
*Test:* the new IT fails on unmodified code.

**Step 2 — `AccessChecker` (R2).**
New `client/kubernetes/access_client.go`: interface, SSAR-backed implementation via `Clients.Dynamic`,
memoization on GVR+namespace. Unit tests including the single-API-call assertion.
*Test:* `go test ./cmd/diff/client/kubernetes/... -run TestAccess -count=1`.

**Step 3 — `DryRunCreate` (R3).**
Add the method to `ApplyClient` + `DefaultApplyClient`. Unit tests with a fake dynamic client, including a
`generateName`-only object.
*Test:* `go test ./cmd/diff/client/kubernetes/... -run TestDryRunCreate -count=1`.

**Step 4 — `sanitizeForDryRun` and hoist it (R4).**
Extract the strip, widen it to the seven fields, apply on both paths, delete the false comment.
*Test:* unit assertion on the sanitized payload; existing calculator tests still pass.

**Step 5 — `DryRunInfo` + wire field (R7).**
Add the types, the `ResourceDiff` field, and the `ChangeDetail` mapping with the pointer/`omitempty`
semantics and the documented removal-diff asymmetry.
*Test:* structured-renderer unit tests for presence/absence.

**Step 6 — `WithFailures` + rejection classification (R8).**
Add the constructor, the `NewOutputError` branch, and the apierror→`SchemaValidationError` conversion.
Apply it to **both** paths.
*Test:* unit tests through `NewOutputError` and `DetermineExitCode`.

**Step 7 — the addition path in `CalculateDiff` (R5, R6, R9, R10), and constructor threading (R1).**
Implement the decision tree, the result merge, deduped warnings, and the 409 error. Add `DryRunOn` /
`ProcessorConfig.DryRunOn` / `WithDryRunOn` / the kong flag, and extend `NewDiffCalculator` plus the
`ProcessorConfig.DiffCalculator` factory type with `accessChecker` and `dryRunOn`; update every test
supplying a factory.
*Test:* Step 1's IT turns **green**; the seven-case unit table passes.

**Step 8 — remaining integration coverage (AC-R6.1, R6.2, R7, R9, R10, R12).**
*Test:* `earthly +go-test` bare.

**Step 9 — rejection-path test (AC-R8).**
Attempt VAP under envtest; if unsupported, add the e2e instead.
*Test:* new case passes and is mutation-verified.

**Step 10 — R13 claim-path verification.**
Trace whether the synthesized backing XR reaches `CalculateDiff`; exclude it if so; record the finding.

**Step 11 — mutation verification sweep (§5.7).**
Invert behaviour per new assertion; confirm each fails.

**Step 12 — docs (R14).**
README §RBAC, exit codes, structured schema, flag reference; design doc sections; mermaid + regenerated
SVGs via `minlag/mermaid-cli`.

**Step 13 — full verification + PR.**
`earthly -P +reviewable` bare (never piped — a pipeline reports `tail`'s status and masks failures). Check
`git status` afterwards for lint autofixes in unrelated files; if present, commit them as a **separate**
lint-fix commit in the same PR rather than reverting. Then `git commit -s` (DCO), push the topic branch to
`origin` with an explicit refspec (`git push origin optional-ssa:refs/heads/optional-ssa`, because
`push.default=upstream` would otherwise target `main`), and open a **draft** PR following
`.github/PULL_REQUEST_TEMPLATE.md` — `Fixes #334`, and every checklist item either `[x]` or `[ ]` with the
text struck through using double tildes. Call out the R8 exit-code change explicitly.
