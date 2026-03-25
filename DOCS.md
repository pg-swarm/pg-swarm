# Documentation Index

All project documentation lives in the [`docs/`](docs/) directory.

## Architecture & Design

- [Design Document](docs/DESIGN.md) — System architecture, components, and data flows
- [Boot & Failover Sequence](docs/BOOT-FAILOVER.md) — Step-by-step boot, replica, and failover operations

## Features & Specifications

- [Configuration Change Management](docs/CONFIG.md) — Safe config updates to active clusters
- [Backup & Restore](docs/BACKUP.md) — Backup requirements and restore procedures
- [Recovery Logic](docs/RECOVERY.md) — Cluster failure conditions and three-layer recovery
- [Log-Based Recovery Agent](docs/LOG-AGENT.md) — Real-time PG log watcher and automated corrective actions
- [Web Pod Shell](docs/SHELL.md) — Browser-based terminal into pod containers

## Operations & Planning

- [Gap Analysis](docs/GAP-ANALYSIS.md) — Dashboard mock data vs real backend comparison
- [Hardening Roadmap](docs/HARDENING.md) — Security and enterprise readiness work
- [TODO](docs/TODO.md) — Roadmap and planned work

## Project

- [Changelog](docs/CHANGELOG.md) — Release history
- [Contributing](docs/CONTRIBUTING.md) — Contribution guidelines
- [Maintainers](docs/MAINTAINERS.md) — Project maintainers

## Other Documentation

- [deploy/docker/README.md](deploy/docker/README.md) — Docker deployment
- [deploy/k8s/README.md](deploy/k8s/README.md) — Kubernetes deployment
- [design/DESIGN.md](design/DESIGN.md) — Extended design notes
