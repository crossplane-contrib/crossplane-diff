# Proposal: converge `comp` and `xr` per-XR structured output (#410)

## Today

| Axis            | `xr` — `xrs[]` entry (`xrDiffWire`)        | `comp` — `impactAnalysis[]` entry (`xrImpactWire`)         |
|-----------------|--------------------------------------------|------------------------------------------------------------|
| Identity        | nested `xr: {apiVersion, kind, name, namespace}` | `ObjectReference` inlined at the top of the entry      |
| Status          | `changed` / `unchanged` / `error`          | same, plus `filtered`                                       |
| Errors          | `errors: []OutputError` (with `validationFailures[]`) | `error: string`                                  |
| Changes         | sibling `summary` + `changes[]`, always present | `downstreamChanges: {summary, changes}`, only when changed |
| Filter info     | —                                          | `filterReason`, `filterDetail`                              |

The xr command also still emits the deprecated flat top-level `changes[]` (#405).

## Converged shape ("v2")

### One per-XR entry type, emitted by both commands

```go
// xrEntryWire replaces both xrDiffWire and xrImpactWire.
type xrEntryWire struct {
    XR           corev1.ObjectReference `json:"xr"`                     // projected to apiVersion/kind/name/namespace
    Status       XRStatus               `json:"status"`                 // changed | unchanged | error | filtered
    FilterReason FilterReason           `json:"filterReason,omitempty"` // comp only; set iff status == filtered
    FilterDetail string                 `json:"filterDetail,omitempty"` // comp only
    Summary      Summary                `json:"summary"`                // always present; zeros unless changed
    Changes      []ChangeDetail         `json:"changes"`                // always present; [] unless changed
    Errors       []dt.OutputError       `json:"errors,omitempty"`       // set iff status == error
}
```

Rules, all of which `xr` already follows:

- `summary` and `changes` are always present, so a consumer never has to tell "missing" from "empty". This drops
  `comp`'s `downstreamChanges` wrapper and the name "downstream" with it. The flat shape is better here because
  `status` already says whether there are changes, so a nullable wrapper adds nothing.
- `errors[]` carries the same `OutputError`s (same `resourceID`, `message`, `validationFailures`) that each command
  already puts into its top-level `errors[]`. The top-level list stays as the union, as it is today.
- `filterReason`/`filterDetail` are optional fields that only `comp` sets. `xr` never emits `status: filtered`.

### `xr` document (v2)

```json
{
  "outputVersion": "v2",
  "summary":  { "added": 1, "modified": 1, "removed": 0 },
  "xrs": [
    {
      "xr": { "apiVersion": "example.org/v1", "kind": "XBucket", "name": "bucket-1", "namespace": "default" },
      "status": "changed",
      "summary": { "added": 1, "modified": 1, "removed": 0 },
      "changes": [ ... ]
    }
  ],
  "errors":   [ ... ],
  "warnings": [ ... ]
}
```

Only change from v1: the deprecated flat `changes[]` is removed (issue item 5). The top-level `summary` stays: it
counts distinct resources, which `xrs[].summary` cannot give you when inputs overlap.

### `comp` document (v2)

```json
{
  "outputVersion": "v2",
  "compositions": [
    {
      "name": "xbuckets.example.org",
      "compositionChanges": { ... },
      "affectedResources": { "total": 4, "withChanges": 1, "unchanged": 1, "withErrors": 1, "filteredBySelector": 1 },
      "revisionImpact": { ... },
      "impactAnalysis": [
        {
          "xr": { "apiVersion": "example.org/v1", "kind": "XBucket", "name": "bucket-1" },
          "status": "changed",
          "summary": { "added": 1, "modified": 1, "removed": 0 },
          "changes": [ ... ]
        },
        {
          "xr": { "apiVersion": "example.org/v1", "kind": "XBucket", "name": "bucket-3" },
          "status": "error",
          "summary": { "added": 0, "modified": 0, "removed": 0 },
          "changes": [],
          "errors": [ { "resourceID": "XBucket/bucket-3", "message": "...", "validationFailures": [ ... ] } ]
        },
        {
          "xr": { "apiVersion": "example.org/v1", "kind": "XBucket", "name": "bucket-4" },
          "status": "filtered",
          "filterReason": "revision_selector_mismatch",
          "filterDetail": "compositionRevisionSelector {version: 0.0.1} does not match ...",
          "summary": { "added": 0, "modified": 0, "removed": 0 },
          "changes": []
        }
      ]
    }
  ],
  "errors":   [ ... ],
  "warnings": [ ... ]
}
```

With this, `.xrs[]` and `.compositions[].impactAnalysis[]` hold the same type, so one parser handles both.

### Decisions

1. **Keep the `impactAnalysis` key.** The issue asks for one entry type, and the key is only the path to it. The
   sibling fields `impactAnalysisSkipped` and `affectedResources` describe this array by that name, so renaming it
   would mean renaming them too, which breaks more for no parsing gain.
2. **Also converge the per-composition `error: string` into `errors: []OutputError`** in the same breaking change.
   It's the same legacy pattern one level up, and there's no point spending a second breaking release on it later.
   Like the per-XR entry, the field is set only when the composition failed. Unlike per-XR failures, a composition
   failure doesn't reach the top-level `errors[]` today (`comp_processor` only counts it). v2 adds it there, with
   `resourceID` `Composition/<name>` following the existing `Kind/Name` convention, so that the top-level list is the
   complete union in both commands. v1 is left as it is. In v2 a failed `compositions[]` entry reads:

   ```json
   {
     "name": "xbuckets.example.org",
     "errors": [ { "resourceID": "Composition/xbuckets.example.org", "message": "..." } ],
     "affectedResources": { "total": 0, "withChanges": 0, "unchanged": 0, "withErrors": 0 },
     "impactAnalysis": []
   }
   ```
3. **Add `outputVersion` to v1 output as well.** The field is additive in v1, and it means every document says which
   shape it is.
4. **Whether to warn on stderr when v1 is selected waits until v1 is actually deprecated.** Until then v1 behaves
   exactly as it does today.

## The switch flag

Yes, this is possible, and cheap. The processors already produce internal types (`XRDiffGroup`, `XRImpact`) that are
separate from the wire types. Wire construction happens only in the renderer (`buildXRGroups`,
`buildStructuredCompOutput`), so the flag just picks which builder runs.

```
--output-version=v1|v2   Structured (-o json|yaml) output schema version. Default v1.
                         Has no effect on -o diff.
```

- Defined once on the shared CLI struct in `main.go`, beside `--output`, as a kong `enum:"v1,v2"`, and passed through
  `DiffOptions` to the renderer.
- The document's top-level `outputVersion` repeats the chosen value, so a wrapper can detect the shape instead of
  guessing it.

### Suggested rollout

| Release        | Default | v1                                                    | v2         |
|----------------|---------|-------------------------------------------------------|------------|
| N (minor)      | v1      | current behaviour, unchanged                          | opt-in     |
| N+1 (breaking) | v2      | opt-in via `--output-version=v1`, deprecated (whether to warn at runtime is decided then) | default |
| N+2            | v2      | removed; the flag accepts only `v2`, so pinned scripts still run | only shape |

Release N breaks nobody, so #410 doesn't have to wait for a breaking `comp` change. The breaking part shrinks to
flipping a default, which consumers can prepare for one release ahead by pinning either value.

### Implementation sketch

1. `XRImpact` gets the `*dt.OutputError` instead of a bare `error`. `comp_processor` already builds it with
   `NewOutputError` for the top-level list, so build it once at impact creation and derive the union from it. The
   renderer can't call `NewOutputError` itself (it lives in `diffprocessor`), and keeping `error` would lose
   `validationFailures`. Do the same for `CompositionDiff.Error`, and in v2 append the composition's `OutputError`
   to the top-level list as well.
2. Renderer: add `xrEntryWire` with a single `toXREntry` builder that both commands use. Keep `xrDiffWire` and
   `xrImpactWire` behind v1, and mark them deprecated.
3. Tests: put the v2 entry type in `testutils/structured_assertions.go` and point `XRImpact` assertions at it when
   v2 is selected. Add table cases in the renderer tests for each status × version, plus one IT for each command with
   `--output-version=v2`.
4. Docs: README structured-output sections (including the "per-XR `error` string" note at the Validation Errors
   section, which v2 makes untrue), design doc §6.8 and §8.1, and the `diff-rendering-architecture.mermaid` diagram.
