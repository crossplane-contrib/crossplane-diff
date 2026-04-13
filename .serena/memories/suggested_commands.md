# Suggested Commands

## Build
```bash
earthly +build
```

## Testing
```bash
# Fast unit/integration tests
cd cmd/diff && go test ./...

# Via Earthly
earthly +go-test

# Specific test
go test ./cmd/diff/diffprocessor -run TestCachedFunctionProvider -v

# Coverage
go test -cover ./cmd/diff/diffprocessor/... -coverprofile=/tmp/coverage.out
go tool cover -func=/tmp/coverage.out
```

## E2E Tests
```bash
# Full E2E matrix (slow)
earthly -P +e2e-matrix

# Single E2E test
earthly +e2e --CROSSPLANE_IMAGE_TAG=main

# Specific test with verbose
earthly -P +e2e --FLAGS="-v=4 -test.run ^TestCompositionDiff"

# Debug mode
earthly -i -P +e2e --FLAGS="-test.failfast -fail-fast -destroy-kind-cluster=false"
```

## Pre-PR Checks
```bash
earthly -P +reviewable  # linting, tests, generation
```

## Running CLI
```bash
./_output/bin/darwin_arm64/crossplane-diff xr my-xr.yaml
./_output/bin/darwin_arm64/crossplane-diff comp updated-composition.yaml
```

## Module Management
```bash
earthly +generate  # tidy go modules
```
