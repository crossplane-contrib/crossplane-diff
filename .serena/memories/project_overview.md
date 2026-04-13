# crossplane-diff Project Overview

## Purpose
CLI tool for diffing Crossplane resources against cluster state. Shows impact of XR changes or composition changes before applying.

## Key Commands
- `crossplane-diff xr <file.yaml>` - Diff XR against cluster state
- `crossplane-diff comp <composition.yaml>` - Show impact of composition changes on existing XRs

## Tech Stack
- Go 1.26.2
- Kong CLI framework
- Earthly for builds
- Crossplane v1 and v2 API support

## Architecture
```
cmd/diff/
├── main.go                    # CLI entry point (kong-based)
├── xr.go                      # XR diff command
├── comp.go                    # Composition diff command
├── client/                    # Kubernetes and Crossplane API clients
├── diffprocessor/            # Core diff logic (domain layer)
├── renderer/                 # Crossplane render pipeline wrapper
├── testutils/                # Test helpers and mock builders
└── types/                    # Shared types and interfaces
```

## Key Design Patterns
- Dependency injection via functional options (`ProcessorOption`)
- Interface-based design for all major components
- Lazy loading and caching (CachedFunctionProvider)
- Two-phase diff calculation for nested XRs

## Critical Implementation Details
- Functions fetched from cluster or via factory
- Schema validation against CRDs/XRDs before diffing
- Handles generateName via labels (`crossplane.io/composition-resource-name`)
- Nested XR support with configurable depth limit
