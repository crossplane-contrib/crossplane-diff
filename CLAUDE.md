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

# Fetch Crossplane cluster CRDs (required after Crossplane API changes or for integration tests)
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

# Run specific E2E test with verbose logging
earthly -P +e2e --FLAGS="-v=4 -test.run ^TestCompositionDiff"

# Debug E2E: stop on first failure and preserve kind cluster
earthly -i -P +e2e --FLAGS="-test.failfast -fail-fast -destroy-kind-cluster=false"

# Run E2E tests directly (without Earthly wrapper for easier debugging)
go test -c -o e2e ./test/e2e
./e2e -v=4 -test.v -test.failfast -destroy-kind-cluster=false -test.run ^TestSpecificTest
```

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
- Docker containers may be orphaned after diff (TODO: cleanup mechanism)
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

**New Claim Handling with spec.claimRef**
When diffing a new Claim (one that doesn't exist in the cluster yet), compositions may reference `spec.claimRef` fields like `{{ .observed.composite.resource.spec.claimRef.name }}`. Since `claimRef` is only populated by Crossplane on the backing XR at runtime, we synthesize a dummy backing XR with:
- The XR kind/apiVersion derived from the XRD
- A synthesized `spec.claimRef` containing the claim's apiVersion, kind, name, and namespace
- All spec fields from the claim copied to the XR
- A generated UID for the dummy XR

This allows compositions using claimRef to render correctly during diff operations for new claims.

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
- Separation of concerns: `DiffCalculator` focuses on diff logic, `ResourceManager` handles cluster I/O
- Identity preservation: Fetches existing nested XRs to maintain their cluster identity across renders

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

## Code Modification Guidelines

### Minimizing Change Footprint
- ALWAYS prefer editing existing files over creating new ones
- When refactoring, back out changes that don't directly support the new architecture
- Keep processors simple: inject dependencies rather than constructing them internally
- Reuse injected instances (e.g., single `DiffProcessor` for all XRs) rather than creating new ones per operation

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

## Related Documentation

- [Design Document](design/design-doc-cli-diff.md): Comprehensive technical design and architecture
- [E2E Test Guide](test/e2e/README.md): Details on E2E test structure and execution
- [README](README.md): User-facing documentation and usage examples

## Upstream Dependencies

### Crossplane Render Nested XR Fix

**Upstream PR:** https://github.com/crossplane/crossplane/pull/6957

**Issue:** `crossplane render`'s `SetComposedResourceMetadata` function doesn't properly propagate the root composite's identity through nested XR trees. It always uses `xr.GetName()` for the `crossplane.io/composite` label instead of checking if the XR already has the label set (which would indicate it's a nested XR that inherited the root's identity).

**Current Workaround:** We have `propagateCompositeLabelInClaimContext` in `diff_processor.go` that fixes composite labels on composed resources of nested XRs after rendering.

**Simplifications After Upstream Merge:**

Once the upstream PR merges and we update our Crossplane dependency:

1. **Remove `propagateCompositeLabelInClaimContext`** - The render pipeline will automatically propagate the correct composite label through nested XR trees.

2. **Simplify nested XR processing** - No post-render label fixups needed; rendering a nested XR with its inherited labels will produce composed resources with correct labels.

3. **Test updates** - E2E tests for nested XRs in Claim trees should pass without any special handling code.

The fix works by checking `xr.GetLabels()[AnnotationKeyCompositeName]` before falling back to `xr.GetName()`. This mirrors the behavior in Crossplane's actual `RenderComposedResourceMetadata` function in `internal/controller/apiextensions/composite/composition_render.go`.
