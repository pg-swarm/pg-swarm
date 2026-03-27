# Contributing to pg-swarm

Thank you for your interest in contributing to pg-swarm! This document provides guidelines and information for contributors.

## Getting Started

1. Fork the repository on GitHub
2. Clone your fork locally
3. Create a branch for your work (`git checkout -b my-feature`)
4. Make your changes
5. Run tests and linting
6. Commit and push to your fork
7. Open a pull request

## Development Setup

### Prerequisites

- Go 1.26+
- [buf](https://buf.build/) (protobuf generation)
- Node.js 18+ (dashboard build)
- Docker and Docker Compose (local dev)
- minikube (integration testing)

### Build and Test

```bash
make build             # Compile all binaries (runs proto + dashboard first)
make test              # Unit tests only
make lint              # golangci-lint
make manifests         # Regenerate operator testdata YAMLs
make proto             # Regenerate protobuf Go code
make dashboard         # Build React dashboard
```

### Running Locally

```bash
# Docker Compose — starts PostgreSQL, central, and a satellite
cd deploy/docker
docker-compose up -d

# Dashboard dev server with mock data
cd dashboard
MOCK=true npm run dev
```

## What to Contribute

### Good First Issues

Look for issues labeled [`good first issue`](https://github.com/pg-swarm/pg-swarm/labels/good%20first%20issue) — these are scoped, well-defined tasks suitable for newcomers.

### Areas Where Help is Welcome

- **Tests** — Unit and integration test coverage, especially for the operator and sentinel sidecar
- **Documentation** — Tutorials, deployment guides, API examples
- **Bug fixes** — Check the issue tracker
- **Dashboard** — UI improvements, new visualizations
- **Observability** — Prometheus metrics, OpenTelemetry integration

## Pull Request Process

1. **Keep PRs focused.** One logical change per PR. If you find an unrelated issue while working, open a separate PR for it.

2. **Write tests.** New features should include tests. Bug fixes should include a regression test where practical.

3. **Run checks locally** before pushing:
   ```bash
   make test
   make lint
   ```

4. **Write clear commit messages.** Use present tense ("Add feature" not "Added feature"). Include context on *why*, not just *what*.

5. **Update documentation** if your change affects user-facing behavior, configuration, or APIs.

6. **Don't break the build.** All CI checks must pass before merge.

7. **Be patient with reviews.** Maintainers review PRs as time allows. Complex changes may require multiple review rounds.

## Code Style

### Go

- Follow standard Go conventions ([Effective Go](https://go.dev/doc/effective_go), [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments))
- Run `golangci-lint` before committing
- Use `context.Context` for cancellation and timeouts
- Prefer returning errors over panicking
- Keep functions focused — if a function does too many things, split it

### JavaScript (Dashboard)

- React with JSX (not TypeScript)
- Functional components with hooks
- CSS in `index.css` using CSS variables for theming
- No external UI framework — vanilla CSS with the existing design system

### Protobuf

- Follow the [Buf style guide](https://buf.build/docs/best-practices/style-guide)
- Run `make proto` after changing `.proto` files
- Never edit files in `api/gen/` — they are generated

## Kubernetes Resources

When modifying the operator's manifest builders:

- All K8s resources must include `TypeMeta` (`apiVersion` and `kind`)
- Secrets use create-only semantics — never overwrite existing secrets
- VolumeClaimTemplates are immutable on existing StatefulSets
- Run `make manifests` to regenerate golden-file test data after changes
- Run `make test` to verify the golden files match

## Reporting Bugs

Open a [GitHub issue](https://github.com/pg-swarm/pg-swarm/issues/new) with:

- What you expected to happen
- What actually happened
- Steps to reproduce
- Environment details (OS, Go version, K8s version, pg-swarm version)
- Relevant logs or error messages

## Suggesting Features

Open a [GitHub issue](https://github.com/pg-swarm/pg-swarm/issues/new) with:

- The problem you're trying to solve
- Your proposed solution (if you have one)
- Any alternatives you considered

## Code of Conduct

This project follows the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md). By participating, you agree to abide by its terms.

## License

By contributing to pg-swarm, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
