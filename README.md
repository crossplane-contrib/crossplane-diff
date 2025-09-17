# crossplane-diff

A standalone tool for visualizing the differences between Crossplane resources in YAML files and their resulting state
when applied to a live Kubernetes cluster. Similar to `kubectl diff` but with specific enhancements for Crossplane
resources, particularly Composite Resources (XRs) and their composed resources.

## Overview

The `crossplane-diff` tool helps you:

- **Preview changes** before applying Crossplane resources to a cluster
- **Visualize differences** at multiple levels: both the XR itself and all downstream composed resources
- **Support complex compositions** including functions, requirements, and environment configurations
- **Handle both Crossplane v1 and v2** resource definitions, including namespaced composite resources
- **Detect resources that would be removed** by your changes

## Installation

### Download Pre-built Binary

Download the latest release from the [releases page](https://github.com/crossplane-contrib/crossplane-diff/releases).

### Build from Source

```bash
git clone https://github.com/crossplane-contrib/crossplane-diff.git
cd crossplane-diff
go build -o crossplane-diff ./cmd/diff
```

## Usage

### Basic Usage

```bash
# Show changes that would result from applying an XR from a file
crossplane-diff diff xr.yaml

# Show changes from stdin
cat xr.yaml | crossplane-diff diff -

# Process multiple files
crossplane-diff diff xr1.yaml xr2.yaml xr3.yaml

# Show changes in a compact format with minimal context
crossplane-diff diff --compact xr.yaml

# Disable color output
crossplane-diff diff --no-color xr.yaml
```

### Command Options

```
crossplane-diff diff [<files> ...] [flags]

Arguments:
  [<files> ...]    YAML files containing Crossplane resources to diff.

Flags:
  --no-color      Disable colorized output.
  --compact       Show compact diffs with minimal context.
  --timeout=1m    How long to run before timing out.
  --qps=0         Maximum QPS to the API server.
  --burst=0       Maximum burst for throttle.
  --verbose       Print verbose logging statements.
```

### Prerequisites

- A running Kubernetes cluster with Crossplane installed
- `kubectl` configured to access your cluster
- Appropriate RBAC permissions (see [Required Permissions](#required-permissions))

## How It Works

The tool performs the following steps:

1. **Load resources** from YAML files or stdin
2. **Find matching compositions** for each Composite Resource
3. **Render resources** using the same composition pipeline as Crossplane
4. **Resolve requirements** iteratively (environment configs, external resources)
5. **Propagate namespaces** from XRs to managed resources (for Crossplane v2)
6. **Validate resources** against their schemas and enforce scope constraints
7. **Calculate diffs** by comparing rendered resources against current cluster state
8. **Display formatted output** showing what would change

## Crossplane v2 Support

This tool fully supports both Crossplane v1 and v2, including:

- **Cluster-scoped XRDs**: Traditional cluster-wide resources
- **Namespaced XRDs**: Resources confined to specific namespaces for better isolation
- **Automatic scope detection**: Determines whether XRDs are cluster or namespace scoped
- **Namespace propagation**: Ensures managed resources inherit appropriate namespaces
- **Scope validation**: Enforces rules like "namespaced XRs cannot own cluster-scoped managed resources"

**Note**: Namespace information is read directly from the XR definitions in your YAML files, not from command-line
flags.

## Required Permissions

The tool requires read access to:

- **Crossplane definitions**: XRDs, Compositions, Functions
- **Crossplane runtime resources**: XRs, Claims, Managed Resources
- **Crossplane configuration**: EnvironmentConfigs
- **Kubernetes resources**: CRDs, referenced resources
- **Resource hierarchies**: Owner references and relationships

## Output Format

The output follows familiar diff conventions with colorized output (unless disabled):

```diff
+++ Resource/new-resource-(generated)
+ apiVersion: nop.crossplane.io/v1alpha1
+ kind: NopResource
+ metadata:
+   name: new-resource
+ spec:
+   forProvider:
+     field: value

---
--- XNopResource/removed-resource
- apiVersion: diff.example.org/v1alpha1
- kind: XNopResource
- spec:
-   coolField: goodbye!

~~~
~~~ Resource/modified-resource
  metadata:
    name: modified-resource
- spec:
-   oldValue: something
+ spec:
+   newValue: something-else
---

Summary: 1 added, 1 modified, 1 removed
```

## Guiding Principles

The tool prioritizes **accuracy above all else**:

- Never silently continues in the face of failures
- Avoids making best-guesses that could compromise accuracy
- Fails completely rather than emit potentially incorrect partial results
- Reaches extensively into the cluster for all information needed to produce accurate diffs
- Caches resources only to avoid API throttling

### Caveats

The tool does not take a snapshot of cluster state before processing, so changes made to the cluster during execution
may affect results. 

## Documentation

- **[Design Document](design/design-doc-cli-diff.md)**: Comprehensive technical design and architecture
- **[CLAUDE.md](CLAUDE.md)**: Instructions for Claude.  Contains development principles and guidelines for LLMs.

## Development

### Local Development Setup

**Prerequisites:**
- Go 1.24+
- Docker
- Earthly ([install instructions](https://earthly.dev/get-earthly))
- kubectl

**Setup Steps:**

1. **Install required tools:**
   ```bash
   go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20
   setup-envtest use 1.30.3 # or whatever cluster version we're using now
   ```

2. **Generate test manifests:**
   ```bash
   earthly +go-generate --CROSSPLANE_IMAGE_TAG=main
   ```

3. **Build the project:**
   ```bash
   earthly +build
   ```

### Running Tests

**Unit and Integration Tests (fast):**
```bash
cd cmd/diff
go test ./...
```

**Development Checks (linting, tests, generation):**
```bash
# Run before opening any PR
earthly +reviewable
```

**End-to-End Tests:**
```bash
# Full test matrix against multiple Crossplane versions (slower)
earthly -P +e2e-matrix

# Single E2E test against specific version
earthly +e2e --CROSSPLANE_IMAGE_TAG=main
```

### Build and Development Commands

```bash
# Build binary
earthly +build

# Run linting
earthly +lint

# Generate code/manifests
earthly +generate

# All available targets
earthly --help
```

### Architecture

The tool follows a clean layered architecture:

- **Command Layer**: CLI argument parsing and coordination
- **Application Layer**: Process orchestration and resource loading
- **Domain Layer**: Core diff logic, rendering, and validation
- **Client Layer**: Kubernetes and Crossplane API interactions

### Testing

The project includes comprehensive testing:

- **Unit tests**: Fast, isolated component testing
- **Integration tests**: Using `envtest` for realistic cluster interactions
- **E2E tests**: Full end-to-end scenarios across v1, v2-cluster, and v2-namespaced configurations

## Contributing

Contributions are welcome! Please see our [contributing guidelines](CONTRIBUTING.md) for detailed setup instructions and development workflow.

**Quick Start for Contributors:**

1. Fork and clone the repository
2. Run setup: `earthly +go-generate --CROSSPLANE_IMAGE_TAG=main`
3. Make your changes
4. Test your changes: `earthly +reviewable && earthly -P +e2e-matrix`
5. Open a pull request

See [CONTRIBUTING.md](CONTRIBUTING.md) for complete guidelines and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for community standards.

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

