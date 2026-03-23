# Enterprise Hardening Roadmap

This document tracks the security and enterprise readiness work needed before production deployment. Implementation should begin after core functionality is complete.

## Current State (as of 2026-03-15)

### What's Already Solid
- gRPC satellite auth (SHA-256 hashed tokens, timing-safe compare)
- Secret generation (24-byte random, create-only preserve policy)
- Backup architecture (multi-destination, incremental, lease-based locking, PITR scaffold)
- Failover sidecar (leader election via K8s Leases)
- Structured logging (zerolog)
- Kustomize deployment with base/overlay structure
- Golden-file manifest tests (12 scenarios)

---

## Phase 1 — Critical (must-have before any enterprise pilot)

### REST API Authentication
- **Gap:** All 59 REST endpoints in `rest.go` have zero auth middleware. Anyone with network access is admin.
- **Plan:** Add OIDC/JWT bearer token validation middleware to GoFiber. Support API keys for service-to-service calls.
- **Files:** `internal/central/server/rest.go`, new `internal/central/auth/middleware.go`

### TLS / mTLS
- **Gap:** gRPC (`:9090`) and REST (`:8080`) both plaintext. Tokens sent unencrypted in gRPC metadata.
- **Plan:** TLS on REST server (Let's Encrypt or provided certs). mTLS on gRPC between central and satellites with CA-signed client certificates.
- **Files:** `cmd/central/main.go`, `internal/central/server/grpc.go`, `cmd/satellite/main.go`

### Audit Logging
- **Gap:** No record of who approved satellites, changed profiles, triggered switchovers, or when.
- **Plan:** Audit log table in PostgreSQL. Middleware logs actor, action, target resource, timestamp. Immutable (append-only).
- **Files:** New `internal/central/audit/` package, new migration, REST middleware integration

### Prometheus Metrics
- **Gap:** No metrics endpoints. No request latency, error rate, satellite count, or replication lag export.
- **Plan:** `/metrics` endpoint on central. Key metrics: request latency/error rate, connected satellite count, cluster states, replication lag, backup status/age.
- **Files:** `internal/central/server/rest.go` (metrics endpoint), new `internal/central/metrics/` package

---

## Phase 2 — High Priority (before production deployment)

### User RBAC
- Admin / operator / viewer roles scoped to namespaces or clusters
- Role assignments stored in PostgreSQL, checked in REST middleware

### Rate Limiting
- GoFiber rate limit middleware on REST API
- Per-IP and per-token limits to prevent abuse

### Backup Restore Drills
- Periodic automated restore-to-temp-cluster
- Verify backup integrity with `pg_checksums`
- Measure and report RTO

### Secrets Encryption at Rest
- Encrypt token hashes and sensitive config in PostgreSQL
- Optional HashiCorp Vault integration for secret storage

### CI Pipeline Hardening
- Add `make test` + `make lint` to GitHub Actions
- Trivy container image vulnerability scanning
- Fail pipeline on critical/high CVEs

### API Documentation
- OpenAPI/Swagger spec auto-generated from GoFiber routes
- Versioned API contract

---

## Phase 3 — Enterprise Features (for enterprise sales / compliance)

### Multi-tenancy
- Tenant model with `tenant_id` scoping on all resources
- Tenant-scoped API access and dashboard views
- Required if offering as SaaS or shared platform

### Helm Chart
- Standard Kubernetes packaging for central and satellite
- Values-based configuration for all deployment knobs
- Published to a Helm repository

### E2E and Chaos Testing
- End-to-end scenarios: register satellite -> create cluster -> failover -> restore
- Chaos tests: network partition, pod kill, split-brain
- Load test: 500 satellites, high health check frequency

### Container Image Signing
- Cosign signatures on all published images
- SBOM generation (Syft) for supply chain security
- Required for SOC 2 / FedRAMP

### Key Rotation
- Zero-downtime satellite token rotation
- PostgreSQL password rotation without cluster restart
- Rotation audit trail

### Operator Runbooks
- On-call manual for common incidents (satellite offline, cluster degraded, backup failure)
- Troubleshooting decision trees
- SLA/SLO documentation with RPO/RTO targets



