# Contributing to crossplane-diff

Thank you for your interest in contributing to crossplane-diff! This document provides guidelines and instructions for contributing to the project.

## Getting Started

### Prerequisites

Before you begin, ensure you have the following tools installed:

- **Go 1.24+**: [Download and install Go](https://golang.org/doc/install)
- **Docker**: Required for running tests and builds
- **kubectl**: For interacting with Kubernetes clusters
- **Earthly**: [Install Earthly](https://earthly.dev/get-earthly) for building and testing
- **setup-envtest**: For integration tests
  ```bash
  go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20
  setup-envtest use 1.30.3 # or latest supported version
  ```

### Development Setup

1. **Fork and clone the repository**:
   ```bash
   git clone https://github.com/your-username/crossplane-diff.git
   cd crossplane-diff
   ```

2. **Generate required manifests**:
   ```bash
   earthly +go-generate --CROSSPLANE_IMAGE_TAG=main
   ```

3. **Build the project**:
   ```bash
   earthly +build
   ```

4. **Run unit and integration tests**:
   ```bash
   cd cmd/diff
   go test ./...
   ```

## Development Workflow

### Making Changes

1. **Create a feature branch**:
   ```bash
   git checkout -b feature/your-feature-name
   ```

2. **Make your changes** following the [coding guidelines](#coding-guidelines)

3. **Write tests** for your changes:
    - Unit tests for individual components
    - Integration tests for component interactions
    - E2E tests for full workflow scenarios when appropriate

4. **Run the development checks**:
   ```bash
   # Run all pre-commit checks (linting, unit tests, etc.)
   earthly +reviewable

   # Run the full end-to-end test matrix (takes longer)
   earthly -P +e2e-matrix
   ```

### Before Opening a Pull Request

**Always run these commands before submitting your PR:**

1. **Check your code is ready for review**:
   ```bash
   earthly +reviewable
   ```
   This command runs:
    - Code generation verification
    - Linting checks
    - Unit and integration tests

2. **Run the full test suite**:
   ```bash
   earthly -P +e2e-matrix
   ```
   This runs end-to-end tests against multiple Crossplane versions (release-1.20 and main).

3. **Ensure your commits are clean**:
    - Use descriptive commit messages
    - Squash related commits into logical units
    - Follow [conventional commit](https://www.conventionalcommits.org/) format when possible

## Coding Guidelines

### General Principles

The project follows the principles outlined in [CLAUDE.md](CLAUDE.md):

- **Accuracy over convenience**: Never silently continue in the face of failures
- **Fail fast and clearly**: Provide useful error messages with context
- **No partial results**: If we can't diff an XR completely, fail the entire diff for that XR
- **Comprehensive logging**: Include contextual objects in log messages

### Code Style

- Follow standard Go conventions and use `gofmt`
- Use meaningful variable and function names
- Add comments for complex logic, especially around namespace handling and resource resolution
- Prefer explicit error handling over silent failures

### Testing

- **Unit tests**: Test individual functions and methods in isolation
- **Integration tests**: Test component interactions using `envtest`
- **E2E tests**: Test full scenarios with real Crossplane clusters
- Aim for high test coverage, especially for critical paths
- Test both success and failure scenarios

### Architecture Guidelines

When adding new features:

1. **Follow the layered architecture**:
    - Command Layer: CLI parsing and coordination
    - Application Layer: Process orchestration
    - Domain Layer: Core business logic
    - Client Layer: External system interactions

2. **Use dependency injection**: Components should depend on interfaces, not concrete implementations

3. **Maintain separation of concerns**: Each component should have a single, well-defined responsibility

## Testing

### Running Tests Locally

```bash
# Unit and integration tests (fast)
cd cmd/diff
go test ./...

# All development checks (linting, tests, etc.)
earthly +reviewable

# Full end-to-end test matrix (slower, comprehensive)
earthly -P +e2e-matrix

# Individual E2E test against specific Crossplane version
earthly +e2e --CROSSPLANE_IMAGE_TAG=main
```

### Test Categories

- **Unit Tests** (`*_test.go`): Fast, isolated tests for individual components
- **Integration Tests** (`*_integration_test.go`): Tests using `envtest` for realistic Kubernetes interactions
- **E2E Tests** (`test/e2e/`): Full end-to-end scenarios with real Crossplane clusters

## Submitting Changes

### Pull Request Process

1. **Ensure your branch is up to date**:
   ```bash
   git fetch upstream
   git rebase upstream/main
   ```

2. **Run all checks**:
   ```bash
   earthly +reviewable
   earthly -P +e2e-matrix
   ```

3. **Open a pull request** with:
    - Clear description of the changes
    - Reference to any related issues
    - Screenshots/examples for UI/output changes
    - Notes about testing performed

4. **Address review feedback** promptly and thoroughly

### Commit Message Format

Use clear, descriptive commit messages:

```
<type>: <short description>

<longer description if needed>

Fixes #issue-number
```

Types: `feat`, `fix`, `docs`, `test`, `refactor`, `chore`

## Documentation

- Update relevant documentation when making changes
- Add or update code comments for complex logic
- Update the design document for architectural changes
- Include examples in the README for new features

## Getting Help

- **Issues**: Check existing [GitHub issues](https://github.com/crossplane-contrib/crossplane-diff/issues) or create a new one
- **Discussions**: Use [GitHub Discussions](https://github.com/crossplane-contrib/crossplane-diff/discussions) for questions and ideas
- **Code of Conduct**: Please follow our [Code of Conduct](CODE_OF_CONDUCT.md)

## Release Process

Releases are handled by maintainers. Contributors should:

- Ensure changes are well-tested and documented
- Update version-related documentation if needed
- Note any breaking changes in the PR description

Thank you for contributing to crossplane-diff!
