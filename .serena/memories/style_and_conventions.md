# Code Style and Conventions

## General Principles
- Accuracy above all else - never silently continue on failures
- Avoid best-guesses that could compromise accuracy
- Fail completely rather than emit incorrect partial results
- Keep changes minimal and focused

## Error Handling
- All errors should cause complete failure of the diff
- Emit useful logging with contextual objects
- No partial results for a given XR
- Machine-readable errors go to BOTH stderr AND structured output

## Testing
- Use table-driven tests
- Mock external dependencies via `testutils/mock_builder.go`
- Fluent API: `tu.NewMockFunctionClient().WithSuccessfulFunctionsFetch(fns).Build()`
- E2E tests MUST end with `function-auto-ready`
- Prefer JSON assertions over ANSI golden files for new tests

## Backwards Compatibility
- Support both Crossplane v1 and v2 API structures
- Handle `spec.compositionUpdatePolicy` (v1) and `spec.crossplane.compositionUpdatePolicy` (v2)
- Default to `Automatic` update policy

## Design Patterns
- Dependency injection via functional options
- Interface-based design for testability
- Lazy loading and caching where appropriate
- Two-phase diff for nested XRs (non-removal first, then removal detection)
