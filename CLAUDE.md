# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Development Commands

### Building and Testing

```bash
# Build the binary for your platform
earthly +build

# Run unit and integration tests (fast, ~7s)
# Direct go test - fastest, immediate output
cd cmd/diff
go test ./...

# Via Earthly (ensures consistent environment, caches dependencies)
earthly +go-test

# Run a specific test
go test ./cmd/diff/diffprocessor -run TestCachedFunctionProvider -v

# Run a single test file
go test ./cmd/diff/diffprocessor/diff_processor_test.go -v

# Check test coverage
go test -cover ./cmd/diff/diffprocessor/... -coverprofile=/tmp/coverage.out
go tool cover -func=/tmp/coverage.out

# Pre-PR checks: linting, tests, generation (requires long timeout, can take several minutes)
earthly -P +reviewable

# Fetch Crossplane's CRDs at the crossplane version go.mod pins into cluster/gomod/crds. Required for unit and
# integration tests (run it in a fresh worktree, and again after a crossplane dependency bump). +go-test runs it.
earthly +fetch-crossplane-crds-gomod

# Fetch Crossplane's cluster directory for an image tag into cluster/<tag>. Only e2e uses it, because it must
# match the Crossplane image e2e runs.
earthly +fetch-crossplane-cluster --CROSSPLANE_IMAGE_TAG=main

# Tidy go modules
earthly +generate
```

**Earthly Output Notes:**
- By default, Earthly buffers stdout and stderr separately, which can cause interleaved output
- Use `2>&1` to merge streams for chronological output: `earthly +go-test 2>&1 | tee output.log`

### End-to-End Tests

E2E tests run against real kind clusters with Crossplane installed. They can take several minutes to complete.

```bash
# Full E2E matrix against multiple Crossplane versions (slow, runs serially)
earthly -P +e2e-matrix

# Single E2E test against specific Crossplane version
earthly +e2e --CROSSPLANE_IMAGE_TAG=main

# Overriding FLAGS replaces the default `-labels=crossplane-version=<tag>` filter, so pass it yourself when
# targeting release-1.20; otherwise main-only (v2) tests run against Crossplane v1 and fail
earthly -P +e2e --CROSSPLANE_IMAGE_TAG=release-1.20 --FLAGS="-labels=crossplane-version=release-1.20 -test.run ^TestDiffExistingClaim$"

# Run specific E2E test with verbose logging
earthly -P +e2e --FLAGS="-v=4 -test.run ^TestCompositionDiff"

# Debug E2E: stop on first failure and preserve kind cluster
earthly -i -P +e2e --FLAGS="-test.failfast -fail-fast -destroy-kind-cluster=false"

# Run E2E tests directly (without Earthly wrapper for easier debugging)
go test -c -o e2e ./test/e2e
./e2e -v=4 -test.v -test.failfast -destroy-kind-cluster=false -test.run ^TestSpecificTest
```

CI runs the `e2e-tests` job once per Crossplane version (`main` and `release-1.20`), as parallel matrix legs that each
call `+ci-e2e` with `CROSSPLANE_IMAGE_TAG` and the matching `-labels=crossplane-version=` filter. Each leg's tag selects
the `test/e2e/manifests/beta/diff/<tag>/` expectations, so changes under `release-1.20/` are verified in CI too.

**IMPORTANT**: Never interrupt running tests to try a simpler approach. E2E tests take a long time but that's expected. Killing them wastes the effort up to that point.

**Test Output Management**: Tests can take several minutes to run. Always save test output to an intermediate file before processing:
```bash
# Good: Save to file first, then query
earthly -P +e2e --test_name=TestFoo 2>&1 | tee /tmp/test-output.log
grep -A50 "FAIL" /tmp/test-output.log

# Bad: Pipe directly to grep (wastes test run if you need different info)
earthly -P +e2e --test_name=TestFoo 2>&1 | grep "FAIL"
```

**Debugging Test Failures**: When E2E tests fail, check `_output/tests/e2e-tests.xml` for complete, un-truncated failure output:
```bash
# Extract specific test failure from XML (includes full output)
grep -A 100 "TestDiffCompositionWithGetComposedResource" _output/tests/e2e-tests.xml

# Note: Earthly output quirks
# - First emits the failure
# - Then emits the log of the run
# - Finally repeats the failure with "*failed*" prepended to each line
```

### Running the CLI

```bash
# Build and run locally
earthly +build
./_output/bin/darwin_arm64/crossplane-diff xr test-xr.yaml

# XR diff - compare XR against cluster state
crossplane-diff xr my-xr.yaml
crossplane-diff xr my-xr.yaml --compact --no-color

# Composition diff - see impact of composition changes on existing XRs
crossplane-diff comp updated-composition.yaml
crossplane-diff comp updated-composition.yaml -n production --include-manual
```

## Architecture

### High-Level Structure

The codebase follows a clean layered architecture with dependency injection and separation of concerns:

```
cmd/diff/
├── main.go                    # CLI entry point (kong-based argument parsing)
├── xr.go                      # XR diff command implementation
├── comp.go                    # Composition diff command implementation
├── client/                    # Kubernetes and Crossplane API clients
│   ├── crossplane/           # Crossplane-specific clients (Compositions, XRDs, Functions, etc.)
│   ├── kubernetes/           # Generic Kubernetes clients (CRDs, dynamic client)
│   └── core/                 # Core client interfaces
├── diffprocessor/            # Core diff logic (domain layer)
│   ├── diff_processor.go     # Main diff orchestration for XRs
│   ├── comp_processor.go     # Composition diff orchestration
│   ├── diff_calculator.go    # Calculates diffs between resources
│   ├── resource_manager.go   # Fetches current cluster state
│   ├── composite_resolver.go # Computes the effective composite Crossplane would reconcile
│   ├── schema_validator.go   # Validates resources against CRD schemas
│   ├── requirements_provider.go  # Resolves composition requirements
│   ├── function_provider.go  # Provides functions for composition pipeline
│   └── processor_config.go   # Configuration and dependency injection
├── renderer/                 # Crossplane render pipeline wrapper
├── testutils/                # Test helpers and mock builders
└── types/                    # Shared types and interfaces
```

### Key Architectural Patterns

1. **Dependency Injection via Factory Pattern**
   - Processors are configured using functional options pattern (`ProcessorOption`)
   - Dependencies flow inward: CLI layer → Application layer → Domain layer → Client layer
   - Factories at top level (CLI) control construction strategies (e.g., cached vs. default function providers)

2. **Interface-Based Design**
   - All major components defined as interfaces (`DiffProcessor`, `CompDiffProcessor`, `FunctionProvider`, etc.)
   - Enables easy mocking for unit tests via `testutils/mock_builder.go`
   - Mock builders use fluent API: `tu.NewMockFunctionClient().WithSuccessfulFunctionsFetch(fns).Build()`

3. **Lazy Loading and Caching**
   - `CachedFunctionProvider`: Lazy-loads functions per composition, caches by composition name
   - Docker container reuse: Adds annotations to enable container reuse across renders
   - Caching decisions made at CLI layer, not embedded in processors

4. **Composition Pipeline**
   - XR diff: Processes single XRs or multiple XRs independently
   - Comp diff: Finds all XRs using a composition, diffs each against updated composition
   - Nested XRs: Recursive processing with configurable depth limit (`--max-nested-depth`)
   - Requirements: Iterative rendering to resolve environment configs and external dependencies

### Critical Implementation Details

**Function Pipeline Integration**
- Functions fetched from cluster or provided via factory
- Function containers are removed by the processor's `Cleanup`, which also runs on the first Ctrl+C/SIGTERM. A run
  that dies without cleanup (SIGKILL, OOM, a second Ctrl+C) can leave them behind; reaping those is #525
- Functions are tied to compositions; cached provider reuses containers across XR renders

**Resource Validation**
- Schema validation against CRDs/XRDs before diffing
- Scope validation: Namespaced XRs cannot own cluster-scoped resources (except Claims)
- Namespace propagation: XR namespace propagates to managed resources in Crossplane v2

**Claim Label Behavior (Empirically Verified)**
Crossplane ALWAYS uses the XR name for the `crossplane.io/composite` label on composed resources, even when rendering from a Claim.

Evidence from empirical testing (2025-11-18):
```yaml
# Claim: my-test-claim (namespace: claim-test-ns)
# XR: my-test-claim-mjwln (cluster-scoped, auto-generated suffix)
# Composed NopResource labels:
labels:
  crossplane.io/claim-name: my-test-claim           # Points to Claim
  crossplane.io/claim-namespace: claim-test-ns      # Claim namespace
  crossplane.io/composite: my-test-claim-mjwln      # Points to XR, NOT Claim!
```

Key implications:
- When diffing Claims, the `crossplane.io/composite` label should NOT change between renders
- Crossplane templates use `{{ .observed.composite.resource.metadata.name }}` which is the XR name
- The XR is the actual composite owner; the Claim just references it via `spec.resourceRef`
- Test expectations must reflect this: NO label changes when modifying existing Claims
- This behavior is consistent across Crossplane versions

**The Effective Composite (Authored vs. Effective)**
Each composite being diffed is seen two ways, and they must not be confused:
- **Authored**: the input as written (plus a synthesized name for generateName-only XRs, and a nested XR's preserved
  identity). This is the server-side dry-run payload, because it is what a user's apply would send. Never default it
  locally: under SSA, including a field claims ownership of it, so locally derived defaults produce false diffs (#503).
- **Effective**: what Crossplane would actually reconcile. `CompositeResolver.Resolve`
  (`diffprocessor/composite_resolver.go`) computes it **once, before composition resolution**, and both composition
  resolution and render consume it. The order matters: inherit from the cluster → apply `comp`'s predicted revision
  (`XRDiffOptions.RevisionName`, root only) → default with the XR's CRD → resolve the composition → render. Defaulting
  before inheriting would let a default `Automatic` overwrite the cluster's `Manual`.
  - **XR**: the authored input plus each Crossplane-written field it omits, taken from the cluster copy
    (`compositionRef`, `compositionRevisionRef`, `compositionUpdatePolicy`, at the v1 or v2 path), plus the cluster UID.
    The UID is required: render keeps an input UID and checks observed resources' controller refs against it.
  - **Claim**: the backing XR named by the cluster claim's `spec.resourceRef`, with the claim's spec synced into it by
    `syncClaimSpec`, which mirrors upstream's claim syncers (e.g. the claim's `compositionRevisionRef` is only propagated
    to the XR under Manual). A fetch error for the backing XR is fatal.

**Defaulting**
- One non-mutating `Defaulter` predicts apiserver CRD defaulting. It is used strictly for the effective XR before
  render, and leniently (built-ins and unknown CRDs pass through) for local prediction of additions when no apiserver
  result is available (`--dry-run-on=existing`, missing namespace, unavailable webhook, forbidden).
- `SchemaValidator.ValidateResources` is read-only; it never defaults its inputs.
- A Claim is defaulted with **its own** CRD, never the XR's: by the apiserver on dry-run, or by the lenient `Defaulter`
  in local prediction. The two CRDs differ (crossplane-runtime `pkg/xcrd`): both carry the XRD's user schema defaults,
  but only the XR CRD defaults `compositionUpdatePolicy`, and only the Claim CRD defaults `compositeDeletePolicy`.

**New Claim Handling with spec.claimRef**
When diffing a new Claim (one that doesn't exist in the cluster yet, or isn't bound to an XR yet), compositions may
reference `spec.claimRef` fields like `{{ .observed.composite.resource.spec.claimRef.name }}`. Since `claimRef` is only
populated by Crossplane on the backing XR at runtime, `CompositeResolver` synthesizes a backing XR
(`synthesizeBackingXR`), delegating to upstream's `ConvertClaimToXR` helper from `crossplane/cli`. If the claim's
`resourceRef` names an XR that doesn't exist, it synthesizes one under that name (the XR the claim syncer would create)
and warns. The synthesized XR carries:
- The authoritative XR kind from the XRD's `spec.names.kind` (XRDs are not required to use the `"X" + claimKind`
  convention)
- The claim's name as the XR name (pinned, vs. the upstream default suffix — preserves cleaner diff output)
- A synthesized `spec.claimRef` containing the claim's apiVersion, kind, name, and namespace
- All spec fields from the claim copied to the XR
- The claim's annotations copied to the XR (matching real Crossplane behavior)
- `crossplane.io/claim-name` and `crossplane.io/claim-namespace` labels on the XR (matching real Crossplane behavior)
- A generated UID for the dummy XR

This allows compositions using `claimRef`, claim labels, or claim annotations to render correctly during diff operations
for new claims.

**Diff Calculation**
- Compares rendered resources against cluster state via server-side dry-run
- Detects additions, modifications, and removals
- Handles `generateName` by matching via labels/annotations (`crossplane.io/composition-resource-name`)
- Uses two-phase approach to correctly handle nested XRs:
  1. **Phase 1 - Non-removal diffs**: `CalculateNonRemovalDiffs` computes diffs for all rendered resources
  2. **Phase 2 - Removal detection**: `CalculateRemovedResourceDiffs` identifies resources to be removed
  - This separation is critical because nested XRs must be processed before detecting removals
  - Nested XRs may render additional composed resources that shouldn't be marked as removals

**Resource Management**
- `ResourceManager` handles all resource fetching and cluster state operations
- Key responsibilities:
  - `FetchCurrentObject`: Retrieves existing resource from cluster (for identity preservation)
  - `FetchObservedResources`: Fetches resource tree to find all composed resources (including nested)
  - `UpdateOwnerReferences`: Updates owner references with dry-run annotations
- Separation of concerns: `DiffCalculator` focuses on diff logic, `ResourceManager` handles cluster I/O, and
  `CompositeResolver` turns cluster state into the effective composite (see "The Effective Composite" above)
- Identity preservation: nested XRs reuse the existing object their parent's observed resources already found (passed
  to `CompositeResolver.Resolve` instead of re-fetching), so they keep their cluster identity across renders

## Design Principles

### Accuracy Above All Else

The tool prioritizes **accuracy over convenience**:

- Never silently continues in the face of failures
- Avoids making best-guesses that could compromise accuracy
- Fails completely rather than emit potentially incorrect partial results
- For multiple XRs: Emit results only for those that succeed, call attention to failures
- Reaches extensively into cluster for all information needed (functions, compositions, requirements, CRDs)
- Caches resources only to avoid API throttling

### Error Handling Philosophy

- All errors should cause complete failure of the diff
- Emit useful logging with appropriate contextual objects attached
- Do not emit partial results for a given XR
- When diffing multiple resources, it's acceptable to emit results for successful ones while reporting failures for others

### Machine-Readable Error Handling

When using structured output (`--output json` or `--output yaml`):

1. **Errors go to BOTH places**: Write errors to stderr (for human visibility) AND include them in the structured output (for machine parsing). This ensures:
   - CI/CD tools can parse errors programmatically from stdout
   - Humans see errors immediately in stderr
   - The structured output is always valid JSON/YAML, even when errors occur

2. **Consistent error structure**: Use the same `OutputError` type (defined in `renderer/types/types.go`) across all commands:
   ```go
   type OutputError struct {
       ResourceID string `json:"resourceID,omitempty"` // optional - identifies which resource failed
       Message    string `json:"message"`              // required - the error message
   }
   ```
   Note: Only JSON tags are used because `sigs.k8s.io/yaml` uses JSON tags for YAML serialization.

3. **Always render output**: Even if all operations fail, render valid structured output with errors. Never return before rendering in structured output mode.

### Testing Requirements

**E2E Test Composition Structure**
- Every test composition MUST end with `function-auto-ready`
- This causes status conditions to bubble up from child resources
- Required for proper setup and teardown to work correctly

**Test Coverage Expectations**
- New code should have comprehensive unit test coverage
- Use table-driven tests for multiple scenarios
- Mock external dependencies using `testutils/mock_builder.go`
- Integration tests use `envtest` for realistic cluster interactions

**Integration-Test XR and Claim CRDs Are Generated, Not Hand-Written**

In a real cluster Crossplane generates each XR's (and claim's) CRD from its XRD. The integration harness does the
same: for every XRD a test case's `setupFiles` declare, `envtestCRDs` (in `cmd/diff/diff_it_utils_test.go`) calls
`testutils.CRDsForXRD`, which runs upstream crossplane-runtime `pkg/xcrd` (`ForCompositeResource`, plus
`ForCompositeResourceClaim` when `spec.claimNames` is set) after defaulting the XRD as the apiserver would. So:

- To give an XR or claim a field, add it to the XRD fixture. There is no CRD to update.
- Don't declare Crossplane's machinery fields (`compositionRef`, `compositionUpdatePolicy`, `compositeDeletePolicy`,
  `resourceRefs`, `spec.crossplane`, …) in an XRD fixture unless that is what the test is about: xcrd generates them,
  and a user-declared field xcrd doesn't overwrite lands on the generated CRD exactly as it would in a cluster.
- An XR or claim kind exists only in a test that applies its XRD, as in a cluster.
- `testdata/{diff,comp}/crds/` holds only plain CRDs that no XRD defines (managed-resource stand-ins such as
  `XDownstreamResource`). The harness fails if a CRD there is also generated from an XRD, so a hand-written copy of a
  generated CRD can't linger.
- Crossplane's own CRDs (Composition, XRD, Function, …) are installed from `cluster/gomod/crds`
  (`testutils.PinnedCrossplaneCRDsDir`), which `earthly +fetch-crossplane-crds-gomod` fills. That target asks the go
  binary in the build which `github.com/crossplane/crossplane/v2` version go.mod selects and clones crossplane at that
  tag, so the CRDs match the xcrd and other Crossplane code under test, and a dependency bump (e.g. by Renovate) moves
  them with it. e2e instead uses `cluster/<image tag>` from `+fetch-crossplane-cluster`, matching the image it runs.

e2e tests need none of this: they run real Crossplane, which generates the CRDs itself.

**Neither Test Suite Reproduces What a Real Client Adds**

No fixture in either suite carries the fields a real client writes, because of how each applies manifests:

- Integration tests (`applyResourcesFromFiles` → `createResources`) use `c.Create` with the fixture verbatim.
- E2E tests (`funcs.ApplyResources` → `ApplyHandler`) use `client.Apply` with a `FieldOwner`, i.e. **server-side**
  apply, which deliberately omits `kubectl.kubernetes.io/last-applied-configuration`.

So a bug that only manifests when a field is present that *only* a client-side `kubectl apply` (or Argo, Flux, …) would
add is invisible to both suites by default. Issue #467 is the worked example: `comp` mishandled every kubectl-applied
composition, and the whole test suite was green.

The fix is cheap, so reach for it whenever a code path's behaviour depends on client-written metadata: **declare the
field in the setup fixture**. Fixtures are applied as written, so a fixture can carry anything a real client would,
without shelling out to `kubectl`. What a real client uniquely does is *produce* the field — reproducing the bug never
requires that. See `cmd/diff/testdata/comp/resources/original-composition-kubectl-applied.yaml` and
`TestCompDiffIntegration/UnchangedCompositionAppliedWithKubectlSkipsImpactAnalysis`.

Prefer an integration test for this (seconds, and it exercises the real CLI wiring) over an e2e — e2e's SSA gives it the
same blind spot, so it would need the same hand-placed fixture field at minutes-scale for no extra coverage.

**Orphaned envtest Servers**

Each integration case starts its own envtest `kube-apiserver` and `etcd` as children of the test binary, and only a
deferred `testEnv.Stop()` ends them. A run that times out, is killed or is interrupted leaves its servers running,
reparented to PID 1, until the next run starts (#524; hundreds once piled up and starved the machine). At suite start
`TestMain` calls `envtestreaper.Reap` (`cmd/diff/testutils/envtestreaper`, built on gopsutil), which SIGKILLs every
`kube-apiserver`/`etcd` whose parent is PID 1 and whose executable is in an envtest directory (`KUBEBUILDER_ASSETS`,
setup-envtest's store, `/usr/local/kubebuilder/bin`), logging `envtest reaper: ...` to stderr. A Linux session under `systemd --user` reparents orphans to that subreaper
rather than PID 1, so the reaper does not see them there.

**Working with ANSI Escape Codes in Test Expectations**

E2E test expectation files (`.ansi` files) contain actual ANSI escape sequences as binary data. These are extremely fragile when editing with shell tools.

**CRITICAL**: Never use `sed`, `perl`, or other shell text tools to directly edit ANSI codes in expectation files. They often:
- Double or triple escape sequences (e.g., `\x1b\x1b\x1b[32m` instead of `\x1b[32m`)
- Convert binary escape bytes to literal text (e.g., `[32m` instead of `\x1b[32m`)
- Corrupt the files in hard-to-debug ways

**Recommended Approach**: Use the Python script approach for reliable ANSI code manipulation:

```python
#!/usr/bin/env python3
# Script: scripts/fix-ansi-codes.py
import sys

def fix_ansi_codes(filepath):
    """Fix ANSI escape codes by removing duplicate escape bytes."""
    with open(filepath, 'rb') as f:
        content = f.read()

    # Replace triple/double escapes with single
    content = content.replace(b'\x1b\x1b\x1b[', b'\x1b[')
    content = content.replace(b'\x1b\x1b[', b'\x1b[')

    with open(filepath, 'wb') as f:
        f.write(content)
    print(f"Fixed {filepath}")

if __name__ == '__main__':
    for filepath in sys.argv[1:]:
        fix_ansi_codes(filepath)
```

Usage:
```bash
# Fix ANSI codes in test expectation files
python3 scripts/fix-ansi-codes.py test/e2e/manifests/beta/diff/main/*/expect/*.ansi

# Verify ANSI codes are correct (should show single \x1b before each [)
hexdump -C test/e2e/manifests/beta/diff/main/comp/expect/existing-xr.ansi | grep "1b 5b 33"
```

**Better Alternative**: Use `E2E_DUMP_EXPECTED=1` to auto-generate correct expectation files:
```bash
# Run tests with auto-dump to regenerate expectation files
earthly -P +e2e --FLAGS="-test.run TestSpecificTest" --E2E_DUMP_EXPECTED=1
```

**Structured JSON Testing (Preferred for New Tests)**

For semantic validation of diff output, prefer structured JSON assertions over ANSI golden files. JSON assertions:
- Eliminate ANSI escape code fragility
- Enable field-level assertions with exact old/new values
- Provide clearer error messages (e.g., `spec.forProvider.configData: expected 'old', got 'new'`)
- Support array indexing via k8s jsonpath syntax (e.g., `spec.pipeline[0].functionRef.name`)

The test helpers in `cmd/diff/testutils/structured_assertions.go` provide a fluent builder API. **Do not
climb back to the root just to finish a chain**: `AssertStructuredDiff` / `AssertStructuredCompDiff` accept
any builder in the chain, so end the expression wherever the last expectation lands. Use `And()` (and the
`AndXR()` / `AndComp()` / `AndComposition(name)` variants) only when you have a *sibling* to add and
genuinely need to step back up.

```go
// XR diff assertions
tu.AssertStructuredDiff(t, jsonOutput, tu.ExpectDiff().
    WithSummary(1, 1, 0).  // added, modified, removed
    WithAddedResource("XDownstreamResource", "test-resource", "default").
    WithField("spec.forProvider.configData", "new-value").
    And().  // step back up to add a second resource
    WithModifiedResource("XNopResource", "test-resource", "default").
    WithFieldChange("spec.coolField", "existing-value", "modified-value").
    And().
    // Advisory-channel warnings (the warnings[] payload), matched on a message substring
    WithWarning("being deleted in the cluster").
    WithWarningContext(map[string]string{"resource": "XNopResource/test-resource"}))

// Composition diff assertions with downstream field-level changes
tu.AssertStructuredCompDiff(t, jsonOutput, tu.ExpectCompDiff().
    WithComposition("xbuckets.example.org").
    WithCompositionModified().  // Assert composition itself is modified
    WithAffectedResources(2, 2, 0, 0).  // total, changed, unchanged, errors
    WithXRImpact("XBucket", "test-xr", "", "changed").
    WithDownstreamSummary(0, 1, 0).  // downstream adds, mods, removes
    WithDownstreamResource("modified", "Bucket", "", "").
    WithAnyName().  // Generated names with random suffix
    WithFieldChange("spec.forProvider.region", "us-east-1", "us-west-2").
    WithFieldAdded("spec.newSetting", "value").      // Field was added
    WithFieldRemoved("spec.deprecatedSetting", "old-value").  // Field was removed
    // Use bracket notation for keys with dots (like Kubernetes annotations)
    WithFieldValuePattern("metadata.annotations['example.org/ref']", `resource-[a-z0-9]+`).
    // A second composition in the same invocation: AndComposition starts its expectations from
    // wherever the previous chain ended, so no climb back to ExpectCompDiff is needed.
    AndComposition("xbuckets-v2.example.org").
    WithCompositionModified().
    WithXRImpact("XBucket", "v2-xr", "", "changed"))
```

**When to use which approach:**
- **JSON assertions**: Semantic tests (add/modify/remove resources, field-level changes, composition diffs)
- **ANSI golden files**: Visual formatting tests (color codes, `+++`/`~~~`/`---` markers, compact mode)

Keep a small set of ANSI tests (~5-7) to smoke test visual output formatting.

## Code Modification Guidelines

### Minimizing Change Footprint
- ALWAYS prefer editing existing files over creating new ones
- When refactoring, back out changes that don't directly support the new architecture
- Keep processors simple: inject dependencies rather than constructing them internally
- Reuse injected instances (e.g., single `DiffProcessor` for all XRs) rather than creating new ones per operation

### Don't Cite Requirements Specs in Code
- Never refer to a `.requirements/` spec from code, comments, test names, fixtures, or docs. That means no IDs like
  `R7`, `AC2.1` or `T3`, and no paths to a `REQUIREMENTS.md`.
- Each spec numbers independently, so every one starts at `R1`/`AC1`, and specs are frozen once their feature ships. A
  bare ID is therefore ambiguous (several specs use `AC3.1` for unrelated things) and goes stale as the code moves on
  while the spec does not.
- Write down the reason the ID stood for. If a comment reads fine with its leading label deleted, delete the label;
  otherwise the comment was relying on the ID and needs the reason spelled out.
- Issue and PR numbers (`#334`) are fine: they are globally unique and do not get renumbered.

### Backwards Compatibility
- Support both Crossplane v1 and v2 API structures
- Handle both `spec.compositionUpdatePolicy` (v1) and `spec.crossplane.compositionUpdatePolicy` (v2)
- Default to `Automatic` update policy when not specified

### Correctness and Edge Cases
- Ensure every code modification strictly preserves correctness
- Robustly handle edge/corner cases related to the problem statement
- Avoid blanket or "quick fix" solutions that might hide errors
- Always strive to diagnose and address root causes, not symptoms
- Empty strings, nil maps, missing fields must all be handled correctly

## Keeping Documentation in Sync

The design doc at `design/design-doc-cli-diff.md` is hand-maintained and drifts quickly if it isn't updated alongside the
code. Before completing a change, check whether any of these triggers apply and update the corresponding section.

**Triggers and where to update**:

| Code change                                                                    | Update                                                                     |
|--------------------------------------------------------------------------------|----------------------------------------------------------------------------|
| Interface in `cmd/diff/diffprocessor/` (`DiffProcessor`, `CompDiffProcessor`, `DiffCalculator`, `ResourceManager`, `SchemaValidator`, `FunctionProvider`) — added/removed/renamed methods or changed signatures | `§6` of the design doc (the relevant interface block); `diff-processor-architecture.mermaid` and `package-overview-fixed.mermaid` |
| New or removed Kubernetes/Crossplane client in `cmd/diff/client/`              | `§5.2.4`, `§6.9`; `client-architecture.mermaid`, `layered-architecture.mermaid`, `conceptual-layers.mermaid` |
| Renderer changes (`cmd/diff/renderer/`): new `OutputFormat`, new structured-output type, change to error-emission contract | `§6.8`; `diff-rendering-architecture.mermaid`                              |
| New CLI flag, renamed flag, removed flag, or new subcommand                    | `§8.1` examples; if it's a `ProcessorConfig` field too, `§6.1.2`           |
| Project layout: new top-level package under `cmd/diff/`, removed file/dir      | `§12.1` directory tree                                                     |
| Changes to the iterative render loop, eventual-state behaviour, or how `RequiredResources`/`RequiredSchemas` are consumed | `§9.5.6` (esp. `§9.5.6.2`)                                                 |
| Behavioural change to a domain component without a signature change (e.g. switching the schema-validation backend, moving defaulting in/out of validation, changing what errors propagate) | The relevant `§6` narrative paragraph; if it touches integration with upstream Crossplane libs, the matching `§9.5.x` block too |
| Workflow change: nested-XR handling, two-phase diff split, cleanup semantics, claim handling | `§7`; `diff-call-sequence.mermaid`                                         |
| New integration-test category (e.g. a new structured-assertion pattern, a new claim edge case) | `§4` test-case list                                                        |
| A future-work item ships, or a new follow-up is identified                     | `§10` future enhancements                                                  |

**Diagrams**: the `*.mermaid` sources under `design/design-doc-cli-diff/` are the source of truth. After editing a
mermaid file, regenerate its `.svg` so the rendered doc matches. The upstream mermaid-cli project publishes Docker
images on both Docker Hub (`minlag/mermaid-cli`) and GitHub Container Registry
(`ghcr.io/mermaid-js/mermaid-cli/mermaid-cli`); either works. The Docker Hub image:

```bash
cd design/design-doc-cli-diff
for f in *.mermaid; do
  docker run --rm -u "$(id -u):$(id -g)" -v "$PWD:/data" minlag/mermaid-cli \
    -i "/data/$f" -o "/data/${f%.mermaid}.svg"
done
```

**README and E2E docs**: `README.md` documents user-facing CLI behaviour and output formats; update it for any change a
user would notice in their workflow — flag/subcommand changes, exit-code changes, structured-output schema changes
(new fields under `errors[]`, `validationFailures[]`, `impactAnalysis[]`, etc.), and human-readable rendering changes
that meaningfully alter what hits stdout/stderr. `test/e2e/README.md` covers E2E structure; update it when adding a new
test category, changing expectation-file conventions, or adding a new helper to the assertion harness.

**When in doubt**, prefer updating the doc in the same PR as the code change. A single PR that ships drift is much
cheaper than retroactively auditing several PRs' worth of changes (as happened in the doc-refresh on 2026-06-15).

## Git, Commits, Issues, and Pull Requests

A few project-wide conventions apply to every change you submit to this repository:

- **Sign every commit (DCO)**. This repo enforces the Developer Certificate of Origin. Always commit with `git commit -s`
  so a `Signed-off-by:` trailer is added; PRs without DCO sign-off on every commit will be blocked from merging. If you
  need to fix a missing sign-off after the fact, use `git commit --amend -s --no-edit` (or `git rebase --signoff` for a
  range of commits).
- **Open PRs as drafts**. Always use `gh pr create --draft …` (or pass `draft: true` via the API). The maintainer can
  mark the PR ready for review once they've assessed it; opening as a draft prevents premature reviewer notifications
  and lets CI run before review starts.
- **Follow the repository's issue and PR templates**. The PR body must follow `.github/PULL_REQUEST_TEMPLATE.md` —
  including the `### Description of your changes` heading, the `Fixes #` line (with a real issue number, or omit the
  line entirely if there is none), and the "I have:" checklist. The template's instruction is **either** `[x]` (checked,
  no strike-through) **or** `[ ] ~~strike through~~` (unchecked box with the item text wrapped in `~~…~~`) — never
  both at once. What isn't acceptable is leaving an item as a plain unchecked `[ ]` with no strike-through. If a
  checklist item legitimately doesn't apply (e.g. no e2e tests for a docs-only change), strike it through with `~~…~~`
  while leaving the checkbox unchecked. Note: GitHub Flavored Markdown requires double tildes for strikethrough — the
  single-tilde notation in the template's HTML comment (`~strike through~`) is informal intent and won't actually
  render as strike-through if copied literally. Issues filed via `gh issue create` must use the matching template
  under `.github/ISSUE_TEMPLATE/` (`bug_report.md` or `feature_request.md`); pass it via `--template bug_report.md`
  and fill in every section.
- **Triage across commands, not just the one the issue names**. Bugs are usually filed against the single command the
  reporter hit (`comp` or `xr`), but the two commands often share underlying logic (composition/revision resolution,
  update-policy handling, schema validation, requirements resolution). Before scoping a fix, check whether the same
  root cause also affects the *other* command's code path, and decide deliberately whether to fix both or file a
  follow-up for the mirror case. Example: issue #388 was filed against `comp` (affected-XR filtering ignored
  `compositionRevisionSelector`), but the same omission also made `xr` resolve the wrong CompositionRevision — one
  root cause, two commands.

## Related Documentation

- [Design Document](design/design-doc-cli-diff.md): Comprehensive technical design and architecture
- [E2E Test Guide](test/e2e/README.md): Details on E2E test structure and execution
- [README](README.md): User-facing documentation and usage examples

## Upstream Dependencies

### Crossplane Render Nested XR Fix (Resolved)

**Upstream PR:** https://github.com/crossplane/crossplane/pull/6957 (backported to v2.1.4 via #6974)

**Issue:** `crossplane render`'s `SetComposedResourceMetadata` function didn't properly propagate the root composite's identity through nested XR trees. It always used `xr.GetName()` for the `crossplane.io/composite` label instead of checking if the XR already has the label set.

**Resolution:** This was fixed upstream and backported to Crossplane v2.1.4. The fix checks `xr.GetLabels()[AnnotationKeyCompositeName]` before falling back to `xr.GetName()`, mirroring the behavior in Crossplane's actual `RenderComposedResourceMetadata` function.

As of crossplane-diff's update to Crossplane v2.1.4, no workarounds are needed - the render pipeline correctly propagates composite labels through nested XR trees.
